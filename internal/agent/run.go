package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/tnmt/kiseki/internal/source/browserhistory"
)

// Run does one collection pass: it delivers spooled batches, then collects new
// records and sends them, spooling whatever cannot be delivered.
func Run(ctx context.Context, cfg *Config, log *slog.Logger) error {
	token, err := os.ReadFile(cfg.TokenFile)
	if err != nil {
		return fmt.Errorf("token file: %w", err)
	}
	client := NewClient(cfg.Server, strings.TrimSpace(string(token)))
	if cfg.retryBackoff > 0 {
		client.backoff = cfg.retryBackoff
	}
	spool, err := NewSpool(cfg.SpoolDir)
	if err != nil {
		return err
	}
	state, err := LoadState(cfg.SpoolDir)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}

	if err := spool.Flush(ctx, client, log); err != nil {
		log.Warn("could not deliver spooled batches", "err", err)
	}

	var errs []error
	if cfg.BrowserHistory != nil {
		if err := collectBrowserHistory(ctx, cfg, client, spool, state, log); err != nil {
			errs = append(errs, fmt.Errorf("browser-history: %w", err))
		}
	}
	if cfg.ClaudeCode != nil {
		if err := collectClaudeCode(ctx, cfg, client, spool, log); err != nil {
			errs = append(errs, fmt.Errorf("claude-code: %w", err))
		}
	}
	return errors.Join(errs...)
}

func collectBrowserHistory(ctx context.Context, cfg *Config, client *Client, spool *Spool, state State, log *slog.Logger) error {
	bh := cfg.BrowserHistory
	profiles, err := browserProfiles(bh, log)
	if err != nil {
		return err
	}

	serverCursor, haveServer, cursorErr := client.Cursor(ctx, browserhistory.Source)
	if cursorErr != nil {
		log.Warn("could not read the server cursor; using local state only", "err", cursorErr)
		haveServer = false
	}

	var errs []error
	for _, p := range profiles {
		key := browserhistory.Source + ":" + p.Key()
		since := resendFrom(state, key, serverCursor, haveServer, cfg.overlap().Milliseconds())

		visits, err := browserhistory.ReadVisits(ctx, p.HistoryPath, since)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Key(), err))
			continue
		}
		records, err := browserhistory.Records(p, visits, cfg.Timezone, bh.ExcludeDomains)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Key(), err))
			continue
		}

		sent, spooled, deliverErr := deliver(ctx, client, spool, browserhistory.Source, records)
		failed := deliverErr != nil
		if failed {
			errs = append(errs, fmt.Errorf("%s: %w", p.Key(), deliverErr))
		}
		log.Info("browser history", "profile", p.Key(), "visits", len(visits), "sent", sent, "spooled", spooled)

		if len(visits) > 0 && !failed {
			state[key] = visits[len(visits)-1].OccurredAtMillis
			if err := state.Save(cfg.SpoolDir); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// resendFrom picks where to resume. A profile with no local state is read in
// full. Otherwise it is the older of the local state and the server cursor,
// less the overlap: the cursor lags behind after the server is restored from a
// backup, and the overlap covers visits that reach History late.
func resendFrom(state State, key string, serverCursor int64, haveServer bool, overlapMillis int64) int64 {
	local, ok := state[key]
	if !ok {
		return 0
	}
	from := local
	if haveServer && serverCursor < from {
		from = serverCursor
	}
	return max(from-overlapMillis, 0)
}

func browserProfiles(bh *BrowserHistoryConfig, log *slog.Logger) ([]browserhistory.Profile, error) {
	var profiles []browserhistory.Profile
	if len(bh.Browsers) > 0 {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		for _, browser := range bh.Browsers {
			dir := browserhistory.UserDataDir(browser, home)
			if dir == "" {
				return nil, fmt.Errorf("unsupported browser %q", browser)
			}
			found, err := browserhistory.DetectProfiles(browser, dir)
			if errors.Is(err, os.ErrNotExist) {
				log.Info("no Local State found; skipping browser", "browser", browser)
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", browser, err)
			}
			profiles = append(profiles, found...)
		}
	}
	for _, path := range bh.HistoryPaths {
		profiles = append(profiles, browserhistory.Profile{Browser: "custom", Name: path, HistoryPath: path})
	}
	return profiles, nil
}
