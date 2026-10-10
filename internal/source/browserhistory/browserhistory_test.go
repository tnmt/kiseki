package browserhistory

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func unixMillisToChromium(ms int64) int64 { return ms*1000 + chromiumEpochOffsetMicros }

type visitRow struct {
	url, title string
	atMillis   int64
	guid       any
}

// newHistory builds a History file in WAL mode with checkpointing disabled and
// the connection left open, so recent rows exist only in the -wal file.
func newHistory(t *testing.T, dir string, rows []visitRow) (path string, db *sql.DB) {
	t.Helper()
	path = filepath.Join(dir, "History")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	for _, stmt := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA wal_autocheckpoint = 0`,
		`CREATE TABLE urls (id INTEGER PRIMARY KEY, url TEXT, title TEXT)`,
		`CREATE TABLE visits (id INTEGER PRIMARY KEY, url INTEGER, visit_time INTEGER,
			visit_duration INTEGER, transition INTEGER, originator_cache_guid TEXT)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i, r := range rows {
		id := int64(i + 1)
		if _, err := db.Exec(`INSERT INTO urls VALUES (?, ?, ?)`, id, r.url, r.title); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO visits VALUES (?, ?, ?, 5000000, 805306368, ?)`,
			id, id, unixMillisToChromium(r.atMillis), r.guid); err != nil {
			t.Fatal(err)
		}
	}
	return path, db
}

func TestReadVisitsReplaysWALFromLiveDatabase(t *testing.T) {
	base := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC).UnixMilli()
	path, _ := newHistory(t, t.TempDir(), []visitRow{
		{"https://example.com/old", "Old", base - 3_600_000, nil},
		{"https://example.com/a", "A", base, nil},
		{"https://example.com/b", "B", base + 1000, "guid-1"},
	})
	if fi, err := os.Stat(path + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("expected non-empty WAL, got %v %v", fi, err)
	}

	visits, err := ReadVisits(context.Background(), path, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(visits) != 2 || visits[0].URL != "https://example.com/a" || visits[1].URL != "https://example.com/b" {
		t.Fatalf("visits = %+v", visits)
	}
	if visits[0].OccurredAtMillis != base || visits[0].Synced || !visits[1].Synced {
		t.Errorf("visit fields: %+v", visits)
	}
	if visits[0].ID != 2 || visits[0].DurationMicros != 5000000 {
		t.Errorf("id/duration: %+v", visits[0])
	}
}

func TestReadVisitsLeavesNoTempFiles(t *testing.T) {
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)
	path, _ := newHistory(t, t.TempDir(), []visitRow{{"https://example.com/", "E", 1_760_000_000_000, nil}})
	if _, err := ReadVisits(context.Background(), path, 0); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(tmpRoot)
	if len(entries) != 0 {
		t.Errorf("leftover temp entries: %v", entries)
	}
}

func TestReadVisitsMissingFile(t *testing.T) {
	if _, err := ReadVisits(context.Background(), filepath.Join(t.TempDir(), "History"), 0); err == nil {
		t.Error("expected error")
	}
}

func TestRecords(t *testing.T) {
	p := Profile{Browser: "brave-origin", Name: "Default"}
	visits := []Visit{
		{ID: 7, URL: "https://example.com/a?x=1", Title: "A", OccurredAtMillis: 1_760_000_000_000},
		{ID: 8, URL: "https://www.secret.example.org/p", Title: "excluded"},
		{ID: 9, URL: "https://notsecret.example.org/p", Title: "not excluded by suffix"},
		{ID: 10, URL: "chrome://settings", Title: "internal"},
		{ID: 11, URL: "file:///etc/passwd", Title: "file"},
	}
	got, err := Records(p, visits, "Asia/Tokyo", []string{"secret.example.org"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("records = %+v", got)
	}
	if got[0].RecordID != "brave-origin/Default:7" || got[0].TZ != "Asia/Tokyo" || got[0].OccurredAt != 1_760_000_000_000 {
		t.Errorf("record 0: %+v", got[0])
	}
	if got[1].RecordID != "brave-origin/Default:9" {
		t.Errorf("record 1: %+v", got[1])
	}
	var pl map[string]any
	if err := json.Unmarshal(got[0].Payload, &pl); err != nil || pl["url"] != "https://example.com/a?x=1" || pl["browser"] != "brave-origin" {
		t.Errorf("payload: %v %v", pl, err)
	}
}

func TestSanitizeURL(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"https://example.com/p?q=go&page=2", "https://example.com/p?page=2&q=go", true},
		{"https://example.com/cb?code=abc&state=1", "https://example.com/cb?code=REDACTED&state=1", true},
		{"https://example.com/?Access_Token=zzz", "https://example.com/?Access_Token=REDACTED", true},
		{"https://user:pw@example.com/p", "https://example.com/p", true},
		{"https://example.com/cb#access_token=zzz&type=bearer", "https://example.com/cb", true},
		{"https://example.com/docs#section-2", "https://example.com/docs#section-2", true},
		{"chrome://settings", "", false},
		{"data:text/html,hi", "", false},
		{"not a url", "", false},
	}
	for _, c := range cases {
		got, _, ok := sanitizeURL(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("sanitizeURL(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestExcluded(t *testing.T) {
	for host, want := range map[string]bool{
		"example.com": true, "www.example.com": true, "WWW.Example.COM": true,
		"notexample.com": false, "example.com.evil.net": false,
	} {
		if got := excluded(host, []string{"example.com"}); got != want {
			t.Errorf("excluded(%q) = %v", host, got)
		}
	}
}

func TestDetectProfiles(t *testing.T) {
	dir := t.TempDir()
	state := `{"profile":{"info_cache":{"Default":{},"Profile 1":{},"Profile 2":{}}}}`
	os.WriteFile(filepath.Join(dir, "Local State"), []byte(state), 0o600)
	for _, name := range []string{"Default", "Profile 1"} {
		os.MkdirAll(filepath.Join(dir, name), 0o700)
		os.WriteFile(filepath.Join(dir, name, "History"), nil, 0o600)
	}
	got, err := DetectProfiles("chrome", dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range got {
		names = append(names, p.Name)
	}
	if strings.Join(names, ",") != "Default,Profile 1" {
		t.Errorf("profiles = %v (Profile 2 has no History and must be skipped)", names)
	}
}
