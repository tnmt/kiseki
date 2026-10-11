package withings

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/tnmt/kiseki/internal/store"
	_ "modernc.org/sqlite"
)

var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

type measCall struct {
	auth       string
	lastUpdate string
	offset     string
}

// fakeWithings serves groups in pages of pageSize. Each refresh rotates the
// tokens to access-N / refresh-N.
type fakeWithings struct {
	t        *testing.T
	mu       sync.Mutex
	groups   []map[string]any
	pageSize int
	// meas returns a status to answer the n-th getmeas call (0-based) with
	// instead of data, or 0.
	meas      func(n int) int
	refreshes int
	refreshOK bool
	calls     []measCall
	refreshed []string
	codes     []string
}

func (f *fakeWithings) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/oauth2", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		if r.Form.Get("action") != "requesttoken" || r.Form.Get("client_secret") != "secret" {
			reply(w, 503, nil)
			return
		}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			f.refreshed = append(f.refreshed, r.Form.Get("refresh_token"))
			if !f.refreshOK {
				reply(w, 503, nil)
				return
			}
		case "authorization_code":
			f.codes = append(f.codes, r.Form.Get("code"))
		}
		f.refreshes++
		reply(w, 0, map[string]any{
			"userid":        12345,
			"access_token":  fmt.Sprintf("access-%d", f.refreshes),
			"refresh_token": fmt.Sprintf("refresh-%d", f.refreshes),
			"expires_in":    10800,
			"scope":         "user.metrics",
		})
	})
	mux.HandleFunc("POST /measure", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		r.ParseForm()
		n := len(f.calls)
		f.calls = append(f.calls, measCall{r.Header.Get("Authorization"), r.Form.Get("lastupdate"), r.Form.Get("offset")})
		if f.meas != nil {
			if status := f.meas(n); status != 0 {
				reply(w, status, nil)
				return
			}
		}
		offset, _ := strconv.Atoi(r.Form.Get("offset"))
		end := min(offset+f.pageSize, len(f.groups))
		more := 0
		if end < len(f.groups) {
			more = 1
		}
		reply(w, 0, map[string]any{
			"updatetime":  now.Unix(),
			"timezone":    "Asia/Tokyo",
			"measuregrps": f.groups[offset:end],
			"more":        more,
			"offset":      end,
		})
	})
	return mux
}

func reply(w http.ResponseWriter, status int, body any) {
	json.NewEncoder(w).Encode(map[string]any{"status": status, "body": body})
}

func group(id int64, date time.Time) map[string]any {
	return map[string]any{
		"grpid": id, "attrib": 0, "date": date.Unix(), "created": date.Unix(), "modified": date.Unix(),
		"category": 1, "deviceid": "dev", "comment": nil,
		"measures": []map[string]any{{"value": 55700, "type": 1, "unit": -3, "algo": 0, "fm": 3}},
	}
}

type env struct {
	fake *fakeWithings
	c    *Collector
	peek *sql.DB
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "kiseki.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	peek, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { peek.Close() })

	fake := &fakeWithings{t: t, pageSize: 1000, refreshOK: true}
	srv := httptest.NewServer(fake.handler())
	t.Cleanup(srv.Close)

	stateDir := filepath.Join(dir, "withings")
	os.Mkdir(stateDir, 0o700)
	c := &Collector{
		App:     &App{ClientID: "id", ClientSecret: "secret", APIBase: srv.URL},
		Dir:     stateDir,
		Store:   st,
		Host:    "server",
		TZ:      "UTC",
		Overlap: 48 * time.Hour,
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:     func() time.Time { return now },
	}
	return &env{fake: fake, c: c, peek: peek}
}

func (e *env) writeToken(t *testing.T, access, refresh string, expiresAt time.Time) {
	t.Helper()
	if err := SaveToken(TokenPath(e.c.Dir), &Token{UserID: "12345", AccessToken: access, RefreshToken: refresh, ExpiresAt: expiresAt.Unix()}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) count(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.peek.QueryRow(`SELECT COUNT(*) FROM records WHERE source = 'withings' AND host = 'server'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) state(t *testing.T) state {
	t.Helper()
	var st state
	if err := loadJSON(filepath.Join(e.c.Dir, stateFile), &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestNotAuthorized(t *testing.T) {
	e := newEnv(t)
	if _, err := e.c.RunOnce(context.Background()); !errors.Is(err, ErrNotAuthorized) {
		t.Fatalf("err = %v", err)
	}
}

func TestFullThenIncrementalSync(t *testing.T) {
	e := newEnv(t)
	e.writeToken(t, "access-0", "refresh-0", now.Add(time.Hour))
	day := time.Date(2026, 10, 9, 22, 30, 0, 0, time.UTC) // 07:30 on the 10th in Tokyo
	e.fake.groups = []map[string]any{group(1, day.Add(-48*time.Hour)), group(2, day.Add(-24*time.Hour)), group(3, day)}
	e.fake.pageSize = 2

	res, err := e.c.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Inserted != 3 || e.count(t) != 3 {
		t.Fatalf("res = %+v, count = %d", res, e.count(t))
	}
	if got := e.fake.calls; len(got) != 2 || got[0].lastUpdate != "" || got[1].offset != "2" || got[0].auth != "Bearer access-0" {
		t.Fatalf("calls = %+v", got)
	}

	var payload, tz, localDate string
	e.peek.QueryRow(`SELECT payload, tz, local_date FROM records WHERE record_id = '3'`).Scan(&payload, &tz, &localDate)
	if tz != "Asia/Tokyo" || localDate != "2026-10-10" {
		t.Fatalf("tz = %q, local_date = %q", tz, localDate)
	}
	var stored map[string]any
	json.Unmarshal([]byte(payload), &stored)
	if stored["deviceid"] != "dev" || stored["measures"] == nil {
		t.Fatalf("payload = %s", payload)
	}

	st := e.state(t)
	if st.SyncedUntil != now.Unix() || st.Resume != nil || st.UserID != "12345" {
		t.Fatalf("state = %+v", st)
	}

	e.fake.calls = nil
	if _, err := e.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := strconv.FormatInt(day.Add(-48*time.Hour).Unix(), 10)
	if got := e.fake.calls[0].lastUpdate; got != want {
		t.Fatalf("lastupdate = %s, want %s (latest stored minus overlap)", got, want)
	}
}

func TestRateLimitResumesNextRun(t *testing.T) {
	e := newEnv(t)
	e.writeToken(t, "access-0", "refresh-0", now.Add(time.Hour))
	for i := range 5 {
		e.fake.groups = append(e.fake.groups, group(int64(i+1), now.Add(-time.Duration(i)*time.Hour)))
	}
	e.fake.pageSize = 2
	e.fake.meas = func(n int) int {
		if n == 1 {
			return statusRateLimited
		}
		return 0
	}

	res, err := e.c.RunOnce(context.Background())
	if !errors.Is(err, ErrRateLimited) || res.Inserted != 2 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	st := e.state(t)
	if st.Resume == nil || st.Resume.Offset != 2 || st.Resume.LastUpdate != 0 || st.SyncedUntil != 0 {
		t.Fatalf("state = %+v", st)
	}

	e.fake.meas = nil
	e.fake.calls = nil
	later := now.Add(time.Hour)
	e.c.Now = func() time.Time { return later }
	if _, err := e.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.calls[0]; got.offset != "2" || got.lastUpdate != "" {
		t.Fatalf("resumed call = %+v", got)
	}
	if e.count(t) != 5 {
		t.Fatalf("count = %d", e.count(t))
	}
	// The interrupted sync began before the rate limit; its start time marks
	// what is complete, not the time it finished.
	if st := e.state(t); st.SyncedUntil != now.Unix() || st.Resume != nil {
		t.Fatalf("state = %+v", st)
	}
}

func TestExpiringTokenIsRefreshedAndStoredFirst(t *testing.T) {
	e := newEnv(t)
	e.writeToken(t, "access-0", "refresh-0", now.Add(time.Minute))
	if _, err := e.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(e.fake.refreshed) != 1 || e.fake.refreshed[0] != "refresh-0" {
		t.Fatalf("refreshed = %v", e.fake.refreshed)
	}
	if e.fake.calls[0].auth != "Bearer access-1" {
		t.Fatalf("auth = %q", e.fake.calls[0].auth)
	}
	tok, err := LoadToken(TokenPath(e.c.Dir))
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != "refresh-1" || tok.ExpiresAt != now.Unix()+10800 {
		t.Fatalf("token = %+v", tok)
	}
}

func TestRejectedAccessTokenRefreshesOnce(t *testing.T) {
	for _, status := range invalidTokenStatuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			e := newEnv(t)
			e.writeToken(t, "access-0", "refresh-0", now.Add(time.Hour))
			e.fake.meas = func(n int) int {
				if n == 0 {
					return status
				}
				return 0
			}
			if _, err := e.c.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(e.fake.calls) != 2 || e.fake.calls[1].auth != "Bearer access-1" {
				t.Fatalf("calls = %+v", e.fake.calls)
			}

			e.fake.calls = nil
			e.fake.meas = func(int) int { return status }
			if _, err := e.c.RunOnce(context.Background()); !errors.Is(err, errAccessTokenRejected) {
				t.Fatalf("err = %v", err)
			}
			if len(e.fake.calls) != 2 {
				t.Fatalf("calls = %d, want one retry", len(e.fake.calls))
			}
		})
	}
}

func TestRejectedRefreshTokenKeepsFile(t *testing.T) {
	e := newEnv(t)
	e.fake.refreshOK = false
	e.writeToken(t, "access-0", "refresh-0", now.Add(-time.Hour))
	if _, err := e.c.RunOnce(context.Background()); !errors.Is(err, ErrAuthRequired) {
		t.Fatalf("err = %v", err)
	}
	tok, err := LoadToken(TokenPath(e.c.Dir))
	if err != nil || tok.RefreshToken != "refresh-0" {
		t.Fatalf("token = %+v, err = %v", tok, err)
	}
	if len(e.fake.calls) != 0 {
		t.Fatalf("getmeas called with an expired token")
	}
}

func TestStateOfAnotherUserIsDiscarded(t *testing.T) {
	e := newEnv(t)
	e.writeToken(t, "access-0", "refresh-0", now.Add(time.Hour))
	saveJSON(filepath.Join(e.c.Dir, stateFile), state{UserID: "999", Resume: &resume{LastUpdate: 5, Offset: 7}})
	if _, err := e.c.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := e.fake.calls[0]; got.offset != "" || got.lastUpdate != "" {
		t.Fatalf("call = %+v", got)
	}
}

func TestRecordZone(t *testing.T) {
	c := &Collector{TZ: "UTC"}
	page := &MeasurePage{Timezone: "Asia/Tokyo", Groups: []MeasureGroup{
		{GrpID: 1, Date: 1, Timezone: "Europe/Paris"},
		{GrpID: 2, Date: 1, Timezone: "Not/AZone"},
	}}
	recs := c.records(page)
	if recs[0].TZ != "Europe/Paris" || recs[1].TZ != "Asia/Tokyo" {
		t.Fatalf("zones = %s, %s", recs[0].TZ, recs[1].TZ)
	}
	if c.zone("", "") != "UTC" {
		t.Fatal("fallback zone not used")
	}
}

func TestAuthorize(t *testing.T) {
	fake := &fakeWithings{t: t, refreshOK: true}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	redirect := fmt.Sprintf("http://127.0.0.1:%d%s", port, CallbackPath)
	app := &App{ClientID: "id", ClientSecret: "secret", RedirectURI: redirect, APIBase: srv.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tok, err := app.Authorize(ctx, func(authURL string) {
		u, err := url.Parse(authURL)
		if err != nil {
			t.Error(err)
			return
		}
		q := u.Query()
		if q.Get("redirect_uri") != redirect || q.Get("scope") != "user.metrics" || q.Get("client_id") != "id" {
			t.Errorf("authorize URL = %s", authURL)
		}
		go func() {
			bad, err := http.Get(redirect + "?code=evil&state=wrong")
			if err == nil {
				bad.Body.Close()
				if bad.StatusCode != http.StatusBadRequest {
					t.Errorf("wrong state answered %d", bad.StatusCode)
				}
			}
			resp, err := http.Get(redirect + "?code=the-code&state=" + url.QueryEscape(q.Get("state")))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	})
	if err != nil {
		t.Fatal(err)
	}
	if tok.UserID != "12345" || tok.RefreshToken != "refresh-1" {
		t.Fatalf("token = %+v", tok)
	}
	if len(fake.codes) != 1 || fake.codes[0] != "the-code" {
		t.Fatalf("codes = %v", fake.codes)
	}
}

func TestAuthorizeRejectsNonLoopbackRedirect(t *testing.T) {
	app := &App{RedirectURI: "https://example.com/callback"}
	if _, err := app.Authorize(context.Background(), func(string) {}); err == nil {
		t.Fatal("expected an error")
	}
}
