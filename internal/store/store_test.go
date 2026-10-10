package store

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

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "kiseki.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func rec(id string, at time.Time, tz, payload string) Record {
	return Record{RecordID: id, OccurredAt: at.UnixMilli(), TZ: tz, Payload: json.RawMessage(payload)}
}

func TestUpsertIsIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)

	res, err := s.Upsert(ctx, "browser-history", "laptop", []Record{rec("a", at, "Asia/Tokyo", `{"v":1}`)})
	if err != nil || res != (UpsertResult{Inserted: 1}) {
		t.Fatalf("first upsert = %+v, %v", res, err)
	}
	res, err = s.Upsert(ctx, "browser-history", "laptop", []Record{rec("a", at, "Asia/Tokyo", `{"v":2}`)})
	if err != nil || res != (UpsertResult{Updated: 1}) {
		t.Fatalf("second upsert = %+v, %v", res, err)
	}

	var n int
	var payload string
	if err := s.db.QueryRow(`SELECT COUNT(*), MAX(payload) FROM records`).Scan(&n, &payload); err != nil {
		t.Fatal(err)
	}
	if n != 1 || payload != `{"v":2}` {
		t.Errorf("rows = %d, payload = %s", n, payload)
	}
}

func TestSameRecordIDOnDifferentHostsIsDistinct(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	for _, host := range []string{"laptop", "desktop"} {
		if _, err := s.Upsert(ctx, "browser-history", host, []Record{rec("a", at, "Asia/Tokyo", `{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM records`).Scan(&n)
	if n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}

func TestLocalDateFollowsTimeZone(t *testing.T) {
	s := openTest(t)
	// 2026-10-09 16:30 UTC is already 2026-10-10 in Tokyo.
	at := time.Date(2026, 10, 9, 16, 30, 0, 0, time.UTC)
	if _, err := s.Upsert(context.Background(), "x", "h", []Record{rec("a", at, "Asia/Tokyo", `{}`)}); err != nil {
		t.Fatal(err)
	}
	var d string
	s.db.QueryRow(`SELECT local_date FROM records`).Scan(&d)
	if d != "2026-10-10" {
		t.Errorf("local_date = %s", d)
	}
}

func TestUpsertRollsBackOnBadTimeZone(t *testing.T) {
	s := openTest(t)
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	_, err := s.Upsert(context.Background(), "x", "h", []Record{
		rec("good", at, "Asia/Tokyo", `{}`),
		rec("bad", at, "Not/AZone", `{}`),
	})
	if err == nil {
		t.Fatal("expected error")
	}
	var n int
	s.db.QueryRow(`SELECT COUNT(*) FROM records`).Scan(&n)
	if n != 0 {
		t.Errorf("rows = %d, want 0", n)
	}
}

func TestLatestOccurredAt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.LatestOccurredAt(ctx, "x", "h"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}
	t1 := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	s.Upsert(ctx, "x", "h", []Record{rec("a", t1, "UTC", `{}`), rec("b", t2, "UTC", `{}`)})
	s.Upsert(ctx, "x", "other", []Record{rec("c", t2.Add(time.Hour), "UTC", `{}`)})
	got, ok, err := s.LatestOccurredAt(ctx, "x", "h")
	if err != nil || !ok || got != t2.UnixMilli() {
		t.Errorf("got %d ok=%v err=%v, want %d", got, ok, err, t2.UnixMilli())
	}
}

func TestReopenKeepsDataAndDoesNotRemigrate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kiseki.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	s.Upsert(context.Background(), "x", "h", []Record{rec("a", at, "UTC", `{}`)})
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, ok, _ := s.LatestOccurredAt(context.Background(), "x", "h"); !ok {
		t.Error("data lost after reopen")
	}
}

func TestSearch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	s.Upsert(ctx, "browser-history", "h", []Record{
		rec("v1", at, "UTC", `{"url":"https://example.com/docs","title":"Go 設計メモ"}`),
		rec("v2", at.Add(time.Hour), "UTC", `{"url":"https://example.org/","title":"Other_page"}`),
	})
	s.Upsert(ctx, "claude-code", "h", []Record{
		rec("s1:m1", at.Add(2*time.Hour), "UTC", `{"type":"user","message":{"content":"保存形式を検討する"}}`),
		rec("s1:ai-title", at.Add(2*time.Hour), "UTC", `{"type":"ai-title","aiTitle":"Storage format"}`),
	})

	cases := []struct {
		name, q, source string
		want            []string
	}{
		{"trigram japanese", "保存形式", "", []string{"s1:m1"}},
		{"trigram ascii, case-insensitive", "STORAGE", "", []string{"s1:ai-title"}},
		{"url text", "example.org", "", []string{"v2"}},
		{"short query falls back to scan", "設計", "", []string{"v1"}},
		{"source filter", "example", "claude-code", nil},
		{"newest first", "example", "", []string{"v2", "v1"}},
		{"like wildcard is literal", "_", "", []string{"v2"}},
		{"quote does not break the query", `"x" OR`, "", nil},
		{"no match", "nothing here", "", nil},
		{"blank", "  ", "", nil},
	}
	for _, c := range cases {
		hits, err := s.Search(ctx, c.q, c.source, 50)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var got []string
		for _, h := range hits {
			got = append(got, h.RecordID)
		}
		if strings.Join(got, ",") != strings.Join(c.want, ",") {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSearchFollowsUpdates(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	s.Upsert(ctx, "browser-history", "h", []Record{rec("v1", at, "UTC", `{"url":"https://a.example/","title":"before title"}`)})
	s.Upsert(ctx, "browser-history", "h", []Record{rec("v1", at, "UTC", `{"url":"https://a.example/","title":"after title"}`)})

	if hits, _ := s.Search(ctx, "before", "", 10); len(hits) != 0 {
		t.Errorf("stale text still searchable: %d hits", len(hits))
	}
	if hits, _ := s.Search(ctx, "after", "", 10); len(hits) != 1 {
		t.Errorf("updated text not searchable: %d hits", len(hits))
	}
}

func TestMigrationBackfillsSearch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kiseki.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// A database written before the search table existed.
	if _, err := db.Exec(migrations[0].sql); err != nil {
		t.Fatal(err)
	}
	db.Exec(`PRAGMA user_version = 1`)
	db.Exec(`INSERT INTO records (source, host, record_id, occurred_at, tz, local_date, ingested_at, payload)
		VALUES ('browser-history', 'h', 'v1', 1760000000000, 'UTC', '2025-10-09', 1, '{"url":"https://old.example/","title":"legacy page"}')`)
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if hits, err := s.Search(context.Background(), "legacy", "", 10); err != nil || len(hits) != 1 {
		t.Errorf("backfill: %d hits, err %v", len(hits), err)
	}
}

func TestQueries(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	d1 := time.Date(2026, 10, 9, 3, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 10, 10, 3, 0, 0, 0, time.UTC)
	s.Upsert(ctx, "browser-history", "laptop", []Record{rec("a", d1, "UTC", `{}`), rec("b", d2, "UTC", `{}`)})
	s.Upsert(ctx, "claude-code", "laptop", []Record{
		rec("sess1:m1", d2.Add(time.Minute), "UTC", `{}`),
		rec("sess10:m1", d2.Add(2*time.Minute), "UTC", `{}`),
	})

	rows, err := s.RecordsForDate(ctx, "2026-10-10", "")
	if err != nil || len(rows) != 3 || rows[0].RecordID != "b" {
		t.Errorf("RecordsForDate: %v %v", rows, err)
	}
	if rows, _ := s.RecordsForDate(ctx, "2026-10-10", "claude-code"); len(rows) != 2 {
		t.Errorf("source filter: %d rows", len(rows))
	}

	sess, err := s.SessionRecords(ctx, "sess1")
	if err != nil || len(sess) != 1 || sess[0].RecordID != "sess1:m1" {
		t.Errorf("SessionRecords must not match sess10: %v %v", sess, err)
	}

	days, err := s.RecentDays(ctx, 1)
	if err != nil || len(days) != 1 || days[0].Date != "2026-10-10" || days[0].BySource["claude-code"] != 2 || days[0].BySource["browser-history"] != 1 {
		t.Errorf("RecentDays: %+v %v", days, err)
	}

	st, err := s.Status(ctx)
	if err != nil || len(st) != 2 || st[0].Source != "browser-history" || st[0].Count != 2 || st[1].Count != 2 {
		t.Errorf("Status: %+v %v", st, err)
	}
}

func TestSnapshotIsAConsistentCopyWhileServing(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kiseki.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	at := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	s.Upsert(ctx, "browser-history", "h", []Record{rec("v1", at, "UTC", `{"url":"https://a.example/","title":"snapshot page"}`)})

	out := filepath.Join(dir, "snapshots", "kiseki.db")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Snapshot(ctx, dbPath, out); err != nil {
		t.Fatal(err)
	}
	// The source keeps accepting writes after the snapshot.
	s.Upsert(ctx, "browser-history", "h", []Record{rec("v2", at.Add(time.Hour), "UTC", `{"url":"https://b.example/","title":"later page"}`)})

	snap, err := Open(out)
	if err != nil {
		t.Fatalf("snapshot does not open: %v", err)
	}
	defer snap.Close()
	if hits, _ := snap.Search(ctx, "snapshot", "", 10); len(hits) != 1 {
		t.Errorf("snapshot lost data or its search index: %d hits", len(hits))
	}
	if hits, _ := snap.Search(ctx, "later", "", 10); len(hits) != 0 {
		t.Errorf("snapshot contains a write made after it")
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temporary file left behind: %v", err)
	}

	// Taking another snapshot replaces the first.
	if err := Snapshot(ctx, dbPath, out); err != nil {
		t.Fatal(err)
	}
	again, err := Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if hits, _ := again.Search(ctx, "later", "", 10); len(hits) != 1 {
		t.Errorf("second snapshot is stale: %d hits", len(hits))
	}
}

func TestSnapshotOfMissingDatabaseFails(t *testing.T) {
	dir := t.TempDir()
	if err := Snapshot(context.Background(), filepath.Join(dir, "absent.db"), filepath.Join(dir, "out.db")); err == nil {
		t.Error("expected an error")
	}
	if _, err := os.Stat(filepath.Join(dir, "absent.db")); !os.IsNotExist(err) {
		t.Error("snapshot must not create the source database")
	}
}
