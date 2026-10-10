package browserhistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tnmt/kiseki/internal/store"
	_ "modernc.org/sqlite"
)

const Source = "browser-history"

// Chromium stores visit_time as microseconds since 1601-01-01 UTC.
const chromiumEpochOffsetMicros = 11_644_473_600_000_000

type Profile struct {
	Browser     string
	Name        string
	HistoryPath string
}

// Key identifies the profile in agent state and in record IDs.
func (p Profile) Key() string { return p.Browser + "/" + p.Name }

type Visit struct {
	ID               int64
	URL              string
	Title            string
	OccurredAtMillis int64
	DurationMicros   int64
	Transition       int64
	Synced           bool
}

// UserDataDir returns the browser's user data directory, or "" for an unknown
// browser or platform.
func UserDataDir(browser, home string) string {
	var rel string
	switch runtime.GOOS + "/" + browser {
	case "linux/brave-origin":
		rel = ".config/BraveSoftware/Brave-Origin"
	case "linux/chrome":
		rel = ".config/google-chrome"
	case "darwin/brave-origin":
		rel = "Library/Application Support/BraveSoftware/Brave-Origin"
	case "darwin/chrome":
		rel = "Library/Application Support/Google/Chrome"
	default:
		return ""
	}
	return filepath.Join(home, rel)
}

// DetectProfiles lists the profiles named in the browser's Local State.
func DetectProfiles(browser, userDataDir string) ([]Profile, error) {
	b, err := os.ReadFile(filepath.Join(userDataDir, "Local State"))
	if err != nil {
		return nil, err
	}
	var state struct {
		Profile struct {
			InfoCache map[string]json.RawMessage `json:"info_cache"`
		} `json:"profile"`
	}
	if err := json.Unmarshal(b, &state); err != nil {
		return nil, fmt.Errorf("Local State: %w", err)
	}
	names := make([]string, 0, len(state.Profile.InfoCache))
	for name := range state.Profile.InfoCache {
		names = append(names, name)
	}
	sort.Strings(names)
	var profiles []Profile
	for _, name := range names {
		path := filepath.Join(userDataDir, name, "History")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		profiles = append(profiles, Profile{Browser: browser, Name: name, HistoryPath: path})
	}
	return profiles, nil
}

// ReadVisits returns visits at or after sinceMillis in time order. It copies
// History and its WAL files first because the browser keeps the live file
// locked; SQLite then replays the copied WAL on open.
func ReadVisits(ctx context.Context, historyPath string, sinceMillis int64) ([]Visit, error) {
	tmp, err := os.MkdirTemp("", "kiseki-history-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	snapshot := filepath.Join(tmp, "History")
	if err := copyFile(historyPath, snapshot); err != nil {
		return nil, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := copyFile(historyPath+suffix, snapshot+suffix); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", snapshot)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	sinceMicros := sinceMillis*1000 + chromiumEpochOffsetMicros
	rows, err := db.QueryContext(ctx, `
SELECT visits.id, urls.url, urls.title, visits.visit_time,
       visits.visit_duration, visits.transition, visits.originator_cache_guid
FROM visits JOIN urls ON urls.id = visits.url
WHERE visits.visit_time >= ?
ORDER BY visits.visit_time ASC, visits.id ASC`, sinceMicros)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var visits []Visit
	for rows.Next() {
		var v Visit
		var visitTime int64
		var guid sql.NullString
		if err := rows.Scan(&v.ID, &v.URL, &v.Title, &visitTime, &v.DurationMicros, &v.Transition, &guid); err != nil {
			return nil, err
		}
		v.OccurredAtMillis = (visitTime - chromiumEpochOffsetMicros) / 1000
		v.Synced = guid.Valid && guid.String != ""
		visits = append(visits, v)
	}
	return visits, rows.Err()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

type payload struct {
	URL            string `json:"url"`
	Title          string `json:"title"`
	Browser        string `json:"browser"`
	Profile        string `json:"profile"`
	DurationMicros int64  `json:"visit_duration_us"`
	Transition     int64  `json:"transition"`
	Synced         bool   `json:"synced"`
}

// Records converts visits to ingest records. Non-web URLs and excluded domains
// are dropped, and secrets in URLs are redacted, before anything leaves the
// machine.
func Records(p Profile, visits []Visit, tz string, excludeDomains []string) ([]store.Record, error) {
	records := make([]store.Record, 0, len(visits))
	for _, v := range visits {
		clean, host, ok := sanitizeURL(v.URL)
		if !ok || excluded(host, excludeDomains) {
			continue
		}
		body, err := json.Marshal(payload{
			URL: clean, Title: v.Title, Browser: p.Browser, Profile: p.Name,
			DurationMicros: v.DurationMicros, Transition: v.Transition, Synced: v.Synced,
		})
		if err != nil {
			return nil, err
		}
		records = append(records, store.Record{
			RecordID:   p.Key() + ":" + strconv.FormatInt(v.ID, 10),
			OccurredAt: v.OccurredAtMillis,
			TZ:         tz,
			Payload:    body,
		})
	}
	return records, nil
}

func excluded(host string, domains []string) bool {
	host = strings.ToLower(host)
	for _, d := range domains {
		d = strings.ToLower(strings.TrimPrefix(d, "."))
		if host == d || strings.HasSuffix(host, "."+d) {
			return true
		}
	}
	return false
}

var sensitiveParams = []string{
	"token", "access_token", "id_token", "refresh_token", "key", "api_key", "apikey",
	"secret", "client_secret", "password", "passwd", "code", "session", "sessionid",
	"sid", "auth", "signature", "sig",
}

func sensitive(name string) bool {
	return slices.Contains(sensitiveParams, strings.ToLower(name))
}

// sanitizeURL returns the URL without credentials and with sensitive query
// values replaced. ok is false for anything but http and https.
func sanitizeURL(raw string) (clean, host string, ok bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", "", false
	}
	u.User = nil
	if u.RawQuery != "" {
		q, err := url.ParseQuery(u.RawQuery)
		if err != nil {
			u.RawQuery = ""
		} else {
			for name := range q {
				if sensitive(name) {
					q.Set(name, "REDACTED")
				}
			}
			u.RawQuery = q.Encode()
		}
	}
	if u.Fragment != "" {
		if q, err := url.ParseQuery(u.Fragment); err == nil {
			for name := range q {
				if sensitive(name) {
					u.Fragment = ""
					break
				}
			}
		}
	}
	return u.String(), u.Hostname(), true
}
