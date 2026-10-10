package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tnmt/kiseki/internal/auth"
	"github.com/tnmt/kiseki/internal/server"
	"github.com/tnmt/kiseki/internal/store"
	_ "modernc.org/sqlite"
)

const chromiumEpoch = 11_644_473_600_000_000

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type env struct {
	cfg     *Config
	history string
	hdb     *sql.DB
	st      *store.Store
	peek    *sql.DB
	srv     *httptest.Server
	token   string
}

func newEnv(t *testing.T, excludes ...string) *env {
	t.Helper()
	dir := t.TempDir()

	serverDB := filepath.Join(dir, "server.db")
	st, err := store.Open(serverDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	peek, err := sql.Open("sqlite", serverDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peek.Close() })
	token, entry, _ := auth.New("laptop", "laptop", []string{"browser-history", "claude-code"})
	tokensJSON, _ := json.Marshal([]auth.Entry{entry})
	tokensPath := filepath.Join(dir, "tokens.json")
	os.WriteFile(tokensPath, tokensJSON, 0o600)
	tokens, err := auth.Load(tokensPath)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st, tokens, []string{"browser-history", "claude-code"}).Handler())
	t.Cleanup(srv.Close)

	histPath := filepath.Join(dir, "History")
	hdb, err := sql.Open("sqlite", histPath)
	if err != nil {
		t.Fatal(err)
	}
	hdb.SetMaxOpenConns(1)
	t.Cleanup(func() { hdb.Close() })
	for _, stmt := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA wal_autocheckpoint = 0`,
		`CREATE TABLE urls (id INTEGER PRIMARY KEY, url TEXT, title TEXT)`,
		`CREATE TABLE visits (id INTEGER PRIMARY KEY, url INTEGER, visit_time INTEGER,
			visit_duration INTEGER, transition INTEGER, originator_cache_guid TEXT)`,
	} {
		if _, err := hdb.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}

	tokenFile := filepath.Join(dir, "token")
	os.WriteFile(tokenFile, []byte(token+"\n"), 0o600)
	return &env{
		cfg: &Config{
			Server: srv.URL, TokenFile: tokenFile, SpoolDir: filepath.Join(dir, "spool"),
			Timezone: "Asia/Tokyo", OverlapHours: 1, retryBackoff: time.Millisecond,
			BrowserHistory: &BrowserHistoryConfig{HistoryPaths: []string{histPath}, ExcludeDomains: excludes},
		},
		history: histPath, hdb: hdb, st: st, peek: peek, srv: srv, token: token,
	}
}

func (e *env) addVisit(t *testing.T, id int64, url string, at time.Time) {
	t.Helper()
	e.hdb.Exec(`INSERT INTO urls VALUES (?, ?, ?)`, id, url, "t")
	if _, err := e.hdb.Exec(`INSERT INTO visits VALUES (?, ?, ?, 0, 0, NULL)`, id, id, at.UnixMilli()*1000+chromiumEpoch); err != nil {
		t.Fatal(err)
	}
}

func (e *env) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.peek.QueryRow(`SELECT COUNT(*) FROM records WHERE source = 'browser-history' AND host = 'laptop'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRunDeliversAndIsIdempotent(t *testing.T) {
	e := newEnv(t, "blocked.example")
	base := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	e.addVisit(t, 1, "https://example.com/a", base)
	e.addVisit(t, 2, "https://blocked.example/x", base.Add(time.Minute))
	e.addVisit(t, 3, "https://example.com/b", base.Add(2*time.Minute))

	for i := 0; i < 2; i++ {
		if err := Run(context.Background(), e.cfg, quiet); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if n := e.count(t); n != 2 {
			t.Fatalf("run %d: %d records, want 2 (excluded domain dropped, no duplicates)", i, n)
		}
	}

	e.addVisit(t, 4, "https://example.com/c", base.Add(time.Hour))
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t); n != 3 {
		t.Errorf("%d records after new visit, want 3", n)
	}
}

func TestRunSpoolsWhenServerIsDownAndFlushesLater(t *testing.T) {
	e := newEnv(t)
	e.addVisit(t, 1, "https://example.com/a", time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC))

	good := e.cfg.Server
	e.cfg.Server = "http://127.0.0.1:1"
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatalf("offline run should spool, not fail: %v", err)
	}
	pending, _ := filepath.Glob(filepath.Join(e.cfg.SpoolDir, "pending", "*.json"))
	if len(pending) != 1 {
		t.Fatalf("pending files = %v", pending)
	}
	if n := e.count(t); n != 0 {
		t.Fatalf("server has %d records while offline", n)
	}

	e.cfg.Server = good
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	pending, _ = filepath.Glob(filepath.Join(e.cfg.SpoolDir, "pending", "*.json"))
	if len(pending) != 0 {
		t.Errorf("spool not drained: %v", pending)
	}
	if n := e.count(t); n != 1 {
		t.Errorf("%d records after flush, want 1", n)
	}
}

func TestRunResendsAfterServerRestore(t *testing.T) {
	e := newEnv(t)
	base := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	e.addVisit(t, 1, "https://example.com/a", base)
	e.addVisit(t, 2, "https://example.com/b", base.Add(10*time.Hour))
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}

	// Simulate restoring a backup taken before the second visit.
	if _, err := e.peek.Exec(`DELETE FROM records WHERE record_id = ?`, "custom/"+e.history+":2"); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t); n != 1 {
		t.Fatalf("setup: %d records", n)
	}

	e.cfg.OverlapHours = 1
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	if n := e.count(t); n != 2 {
		t.Errorf("%d records after restore, want 2", n)
	}
}

func TestRunMovesRejectedBatchOutOfTheWay(t *testing.T) {
	e := newEnv(t)
	e.addVisit(t, 1, "https://example.com/a", time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC))

	spool, _ := NewSpool(e.cfg.SpoolDir)
	// A source the server does not know is a permanent rejection.
	spool.Add(Batch{Source: "nothing", Records: []store.Record{{RecordID: "a", OccurredAt: 1, TZ: "UTC", Payload: json.RawMessage(`{}`)}}})

	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	rejected, _ := filepath.Glob(filepath.Join(e.cfg.SpoolDir, "rejected", "*.json"))
	pending, _ := filepath.Glob(filepath.Join(e.cfg.SpoolDir, "pending", "*.json"))
	if len(rejected) != 1 || len(pending) != 0 {
		t.Errorf("rejected=%v pending=%v", rejected, pending)
	}
	if n := e.count(t); n != 1 {
		t.Errorf("%d records; the rejected file must not block new data", n)
	}
}

func TestClientDoesNotRetryRejection(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"nope"}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	c.backoff = time.Millisecond
	_, err := c.Post(context.Background(), Batch{Source: "x"})
	var rej *RejectedError
	if !errorsAs(err, &rej) || rej.Status != 403 || rej.Message != "nope" || calls != 1 {
		t.Errorf("err=%v calls=%d", err, calls)
	}
}

func TestClientRetriesServerErrors(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"inserted":1,"updated":0}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "t")
	c.backoff = time.Millisecond
	res, err := c.Post(context.Background(), Batch{Source: "x"})
	if err != nil || res.Inserted != 1 || calls != 3 {
		t.Errorf("res=%+v err=%v calls=%d", res, err, calls)
	}
}

func TestResendFrom(t *testing.T) {
	const hour = int64(3_600_000)
	state := State{"k": 100 * hour}
	cases := []struct {
		name       string
		state      State
		server     int64
		haveServer bool
		want       int64
	}{
		{"unknown profile reads everything", State{}, 100 * hour, true, 0},
		{"local only", state, 0, false, 99 * hour},
		{"server ahead of local uses local", state, 200 * hour, true, 99 * hour},
		{"restored server behind local uses server", state, 50 * hour, true, 49 * hour},
		{"never negative", State{"k": 0}, 0, true, 0},
	}
	for _, c := range cases {
		if got := resendFrom(c.state, "k", c.server, c.haveServer, hour); got != c.want {
			t.Errorf("%s: %d, want %d", c.name, got, c.want)
		}
	}
}

func TestLoadConfig(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	c, err := LoadConfig(write(`{"server":"https://k.example/","token_file":"t","spool_dir":"s","timezone":"Asia/Tokyo"}`))
	if err != nil || c.Server != "https://k.example" || c.OverlapHours != 48 {
		t.Errorf("good config: %+v %v", c, err)
	}
	serverFile := filepath.Join(t.TempDir(), "server")
	os.WriteFile(serverFile, []byte("https://k.example/\n"), 0o600)
	c, err = LoadConfig(write(`{"server_file":"` + serverFile + `","token_file":"t","spool_dir":"s","timezone":"UTC"}`))
	if err != nil || c.Server != "https://k.example" {
		t.Errorf("server_file: %+v %v", c, err)
	}
	emptyFile := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(emptyFile, []byte("\n"), 0o600)
	for name, body := range map[string]string{
		"missing server":    `{"token_file":"t","spool_dir":"s","timezone":"UTC"}`,
		"both servers":      `{"server":"x","server_file":"` + serverFile + `","token_file":"t","spool_dir":"s","timezone":"UTC"}`,
		"absent serverfile": `{"server_file":"/nonexistent/kiseki-server","token_file":"t","spool_dir":"s","timezone":"UTC"}`,
		"empty serverfile":  `{"server_file":"` + emptyFile + `","token_file":"t","spool_dir":"s","timezone":"UTC"}`,
		"bad timezone":      `{"server":"x","token_file":"t","spool_dir":"s","timezone":"Not/AZone"}`,
		"unknown field":     `{"server":"x","token_file":"t","spool_dir":"s","timezone":"UTC","oops":1}`,
		"negative":          `{"server":"x","token_file":"t","spool_dir":"s","timezone":"UTC","overlap_hours":-1}`,
	} {
		if _, err := LoadConfig(write(body)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func errorsAs(err error, target **RejectedError) bool { return errors.As(err, target) }
