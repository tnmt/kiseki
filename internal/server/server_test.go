package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tnmt/kiseki/internal/auth"
	"github.com/tnmt/kiseki/internal/store"
)

type fixture struct {
	srv       *httptest.Server
	store     *store.Store
	laptopTok string
	otherTok  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kiseki.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	laptopTok, laptop, _ := auth.New("laptop", "laptop", []string{"browser-history"})
	otherTok, other, _ := auth.New("desktop", "desktop", []string{"browser-history", "claude-code"})
	b, _ := json.Marshal([]auth.Entry{laptop, other})
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := writeFile(path, b); err != nil {
		t.Fatal(err)
	}
	tokens, err := auth.Load(path)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(New(st, tokens, []string{"browser-history", "claude-code"}).Handler())
	t.Cleanup(srv.Close)
	return &fixture{srv: srv, store: st, laptopTok: laptopTok, otherTok: otherTok}
}

func (f *fixture) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

const oneRecord = `{"source":"browser-history","records":[{"record_id":"a","occurred_at":1760000000000,"tz":"Asia/Tokyo","payload":{"url":"https://example.com/"}}]}`

func TestIngestAuth(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, token, body string
		want              int
	}{
		{"no token", "", oneRecord, http.StatusUnauthorized},
		{"wrong token", "nope", oneRecord, http.StatusUnauthorized},
		{"ok", f.laptopTok, oneRecord, http.StatusOK},
		{"source not allowed", f.laptopTok, strings.Replace(oneRecord, "browser-history", "claude-code", 1), http.StatusForbidden},
		{"unknown source", f.laptopTok, strings.Replace(oneRecord, "browser-history", "nothing", 1), http.StatusBadRequest},
	}
	for _, c := range cases {
		if got, _ := f.do(t, "POST", "/api/v1/ingest", c.token, c.body); got != c.want {
			t.Errorf("%s: status %d, want %d", c.name, got, c.want)
		}
	}
}

func TestIngestIsIdempotentAndHostComesFromToken(t *testing.T) {
	f := newFixture(t)
	for i, want := range []map[string]any{{"inserted": 1.0, "updated": 0.0}, {"inserted": 0.0, "updated": 1.0}} {
		status, out := f.do(t, "POST", "/api/v1/ingest", f.laptopTok, oneRecord)
		if status != 200 || out["inserted"] != want["inserted"] || out["updated"] != want["updated"] {
			t.Errorf("attempt %d: %d %v, want %v", i, status, out, want)
		}
	}
	// The same record_id from another host is a distinct record.
	if status, out := f.do(t, "POST", "/api/v1/ingest", f.otherTok, oneRecord); status != 200 || out["inserted"] != 1.0 {
		t.Errorf("other host: %d %v", status, out)
	}
}

func TestIngestValidation(t *testing.T) {
	f := newFixture(t)
	rec := func(fields string) string {
		return `{"source":"browser-history","records":[{` + fields + `}]}`
	}
	cases := map[string]string{
		"not json":        `{`,
		"unknown field":   `{"source":"browser-history","records":[],"extra":1}`,
		"empty record_id": rec(`"record_id":"","occurred_at":1,"tz":"UTC","payload":{}`),
		"long record_id":  rec(`"record_id":"` + strings.Repeat("x", 257) + `","occurred_at":1,"tz":"UTC","payload":{}`),
		"zero time":       rec(`"record_id":"a","occurred_at":0,"tz":"UTC","payload":{}`),
		"bad tz":          rec(`"record_id":"a","occurred_at":1,"tz":"Not/AZone","payload":{}`),
		"empty tz":        rec(`"record_id":"a","occurred_at":1,"tz":"","payload":{}`),
		"missing payload": rec(`"record_id":"a","occurred_at":1,"tz":"UTC"`),
		"null payload":    rec(`"record_id":"a","occurred_at":1,"tz":"UTC","payload":null`),
	}
	for name, body := range cases {
		if got, _ := f.do(t, "POST", "/api/v1/ingest", f.laptopTok, body); got != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, got)
		}
	}
}

func TestIngestRejectsWholeRequestOnOneBadRecord(t *testing.T) {
	f := newFixture(t)
	body := `{"source":"browser-history","records":[
		{"record_id":"good","occurred_at":1760000000000,"tz":"UTC","payload":{}},
		{"record_id":"bad","occurred_at":1760000000000,"tz":"Not/AZone","payload":{}}]}`
	if got, _ := f.do(t, "POST", "/api/v1/ingest", f.laptopTok, body); got != http.StatusBadRequest {
		t.Fatalf("status %d", got)
	}
	_, out := f.do(t, "GET", "/api/v1/cursors?source=browser-history", f.laptopTok, "")
	if out["latest_occurred_at"] != nil {
		t.Errorf("good record was stored: %v", out)
	}
}

func TestIngestLimits(t *testing.T) {
	f := newFixture(t)

	var many strings.Builder
	many.WriteString(`{"source":"browser-history","records":[`)
	for i := 0; i <= maxRecords; i++ {
		if i > 0 {
			many.WriteString(",")
		}
		many.WriteString(`{"record_id":"r","occurred_at":1,"tz":"UTC","payload":{}}`)
	}
	many.WriteString(`]}`)
	if got, _ := f.do(t, "POST", "/api/v1/ingest", f.laptopTok, many.String()); got != http.StatusRequestEntityTooLarge {
		t.Errorf("too many records: %d", got)
	}

	big := `{"source":"browser-history","records":[{"record_id":"a","occurred_at":1,"tz":"UTC","payload":"` + strings.Repeat("x", maxPayloadBytes) + `"}]}`
	if got, _ := f.do(t, "POST", "/api/v1/ingest", f.laptopTok, big); got != http.StatusRequestEntityTooLarge {
		t.Errorf("large payload: %d", got)
	}

	huge := `{"source":"browser-history","records":[],"pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	if got, _ := f.do(t, "POST", "/api/v1/ingest", f.laptopTok, huge); got != http.StatusRequestEntityTooLarge {
		t.Errorf("large body: %d", got)
	}
}

func TestCursors(t *testing.T) {
	f := newFixture(t)
	_, out := f.do(t, "GET", "/api/v1/cursors?source=browser-history", f.laptopTok, "")
	if out["latest_occurred_at"] != nil {
		t.Errorf("empty: %v", out)
	}
	f.do(t, "POST", "/api/v1/ingest", f.laptopTok, oneRecord)
	_, out = f.do(t, "GET", "/api/v1/cursors?source=browser-history", f.laptopTok, "")
	if out["latest_occurred_at"] != 1760000000000.0 {
		t.Errorf("after ingest: %v", out)
	}
	// Another host's records do not move this host's cursor.
	_, out = f.do(t, "GET", "/api/v1/cursors?source=browser-history", f.otherTok, "")
	if out["latest_occurred_at"] != nil {
		t.Errorf("other host: %v", out)
	}

	for name, c := range map[string]struct {
		path, token string
		want        int
	}{
		"no token":           {"/api/v1/cursors?source=browser-history", "", 401},
		"source not allowed": {"/api/v1/cursors?source=claude-code", f.laptopTok, 403},
		"unknown source":     {"/api/v1/cursors?source=nothing", f.laptopTok, 400},
		"missing source":     {"/api/v1/cursors", f.laptopTok, 400},
	} {
		if got, _ := f.do(t, "GET", c.path, c.token, ""); got != c.want {
			t.Errorf("%s: %d, want %d", name, got, c.want)
		}
	}
}

func TestHealthzNeedsNoAuth(t *testing.T) {
	f := newFixture(t)
	if got, _ := f.do(t, "GET", "/healthz", "", ""); got != 200 {
		t.Errorf("status %d", got)
	}
}

func writeFile(path string, b []byte) error { return os.WriteFile(path, b, 0o600) }
