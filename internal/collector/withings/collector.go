package withings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tnmt/kiseki/internal/store"
)

const (
	Source = "withings"

	tokenFile = "token.json"
	stateFile = "state.json"

	// The access token lasts three hours; refresh a little before it expires
	// rather than waiting for a rejection.
	refreshMargin = 5 * time.Minute
)

// ErrNotAuthorized means the token file does not exist yet.
var ErrNotAuthorized = errors.New("withings: not authorized; run `kiseki withings auth`")

// TokenPath is where the collector keeps its OAuth tokens inside dir.
func TokenPath(dir string) string { return filepath.Join(dir, tokenFile) }

type Collector struct {
	App *App
	// Dir holds the token and the sync state. It must be writable because the
	// refresh token rotates on every refresh.
	Dir   string
	Store *store.Store
	// Host is recorded with every record; the server's name.
	Host string
	// TZ is used for groups whose response carries no resolvable time zone.
	TZ string
	// Overlap is how far before the previous sync the next one starts.
	Overlap time.Duration
	Log     *slog.Logger
	Now     func() time.Time
}

// state survives between runs. Resume is set while a sync spans several runs:
// Withings allows one call per 10 minutes, so a full history that needs many
// pages is fetched one rate-limit window at a time.
type state struct {
	UserID string `json:"userid"`
	// SyncedUntil is the start time (Unix seconds) of the last completed sync.
	// Everything modified before it is stored.
	SyncedUntil int64   `json:"synced_until"`
	Resume      *resume `json:"resume,omitempty"`
}

type resume struct {
	LastUpdate int64 `json:"lastupdate"`
	Offset     int   `json:"offset"`
	StartedAt  int64 `json:"started_at"`
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run collects once immediately and then every interval until ctx ends.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		res, err := c.RunOnce(ctx)
		switch {
		case err == nil:
			c.Log.Info("withings collected", "inserted", res.Inserted, "updated", res.Updated)
		case errors.Is(err, ErrRateLimited):
			c.Log.Warn("withings rate limited; continuing next run", "inserted", res.Inserted, "updated", res.Updated)
		case ctx.Err() != nil:
			return
		default:
			c.Log.Error("withings collection failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// RunOnce fetches the measure groups modified since the last sync and stores
// them. It returns ErrRateLimited when a sync must continue in the next run;
// the records fetched before that are stored.
func (c *Collector) RunOnce(ctx context.Context) (store.UpsertResult, error) {
	var total store.UpsertResult
	tok, err := LoadToken(TokenPath(c.Dir))
	if errors.Is(err, os.ErrNotExist) {
		return total, ErrNotAuthorized
	}
	if err != nil {
		return total, err
	}
	var st state
	if err := loadJSON(filepath.Join(c.Dir, stateFile), &st); err != nil {
		return total, err
	}
	if st.UserID != tok.UserID {
		st = state{UserID: tok.UserID}
	}

	if time.Unix(tok.ExpiresAt, 0).Sub(c.now()) < refreshMargin {
		if tok, err = c.refresh(ctx, tok); err != nil {
			return total, err
		}
	}

	r := st.Resume
	if r == nil {
		lastUpdate, err := c.lastUpdate(ctx, st)
		if err != nil {
			return total, err
		}
		r = &resume{LastUpdate: lastUpdate, StartedAt: c.now().Unix()}
	}

	refreshed := false
	for {
		page, err := c.App.GetMeas(ctx, tok.AccessToken, r.LastUpdate, r.Offset)
		if errors.Is(err, errAccessTokenRejected) && !refreshed {
			refreshed = true
			if tok, err = c.refresh(ctx, tok); err != nil {
				return total, err
			}
			continue
		}
		if err != nil {
			return total, err
		}
		res, err := c.Store.Upsert(ctx, Source, c.Host, c.records(page))
		if err != nil {
			return total, err
		}
		total.Inserted += res.Inserted
		total.Updated += res.Updated

		if page.More == 0 {
			st.SyncedUntil = r.StartedAt
			st.Resume = nil
		} else {
			r.Offset = page.Offset
			st.Resume = r
		}
		if err := saveJSON(filepath.Join(c.Dir, stateFile), st); err != nil {
			return total, err
		}
		if st.Resume == nil {
			return total, nil
		}
	}
}

// lastUpdate decides where an incremental sync starts. The newest stored
// measurement bounds it as well as the sync state, because the database may
// have been restored from a backup older than the state file. A measurement
// is always modified at or after it was taken, so starting from the earlier
// of the two cannot skip a stored gap.
func (c *Collector) lastUpdate(ctx context.Context, st state) (int64, error) {
	latestMs, ok, err := c.Store.LatestOccurredAt(ctx, Source, c.Host)
	if err != nil || !ok {
		return 0, err
	}
	from := latestMs / 1000
	if st.SyncedUntil > 0 && st.SyncedUntil < from {
		from = st.SyncedUntil
	}
	from -= int64(c.Overlap / time.Second)
	return max(from, 1), nil
}

// refresh stores the rotated tokens before returning them; losing them would
// require a new authorization.
func (c *Collector) refresh(ctx context.Context, old *Token) (*Token, error) {
	tok, err := c.App.Refresh(ctx, old.RefreshToken, c.now())
	if err != nil {
		return nil, err
	}
	if err := SaveToken(TokenPath(c.Dir), tok); err != nil {
		return nil, fmt.Errorf("save refreshed token: %w", err)
	}
	return tok, nil
}

func (c *Collector) records(page *MeasurePage) []store.Record {
	records := make([]store.Record, 0, len(page.Groups))
	for _, g := range page.Groups {
		records = append(records, store.Record{
			RecordID:   strconv.FormatInt(g.GrpID, 10),
			OccurredAt: g.Date * 1000,
			TZ:         c.zone(g.Timezone, page.Timezone),
			Payload:    g.Raw,
		})
	}
	return records
}

func (c *Collector) zone(candidates ...string) string {
	for _, tz := range candidates {
		if tz == "" {
			continue
		}
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	return c.TZ
}

func loadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func saveJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
