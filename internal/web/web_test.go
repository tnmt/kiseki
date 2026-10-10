package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tnmt/kiseki/internal/store"
)

const readToken = "test-read-token"

type site struct {
	srv    *httptest.Server
	client *http.Client
	store  *store.Store
}

var base = time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC) // 10:00 in Tokyo

func rec(id string, at time.Time, payload any) store.Record {
	b, _ := json.Marshal(payload)
	return store.Record{RecordID: id, OccurredAt: at.UnixMilli(), TZ: "Asia/Tokyo", Payload: b}
}

func newSite(t *testing.T) *site {
	t.Helper()
	return newSiteAt(t, base.Add(48*time.Hour))
}

func newSiteAt(t *testing.T, now time.Time) *site {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "kiseki.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	w, err := New(Config{Store: st, ReadToken: readToken, Location: tokyo, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(w.Handler())
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &site{srv: srv, store: st, client: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (s *site) login(t *testing.T) {
	t.Helper()
	resp, err := s.client.PostForm(s.srv.URL+"/login", url.Values{"token": {readToken}})
	if err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %v %v", resp, err)
	}
	resp.Body.Close()
}

func (s *site) get(t *testing.T, path string, headers ...string) (int, string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", s.srv.URL+path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := s.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func seed(t *testing.T, s *site) {
	t.Helper()
	ctx := context.Background()
	s.store.Upsert(ctx, "browser-history", "laptop", []store.Record{
		rec("brave/Default:1", base, map[string]any{"url": "https://example.com/a?x=1", "title": "Example A"}),
		rec("brave/Default:2", base.Add(time.Minute), map[string]any{"url": "https://example.com/a?x=1", "title": "Example A"}),
		rec("brave/Default:3", base.Add(2*time.Minute), map[string]any{"url": "https://example.com/b", "title": `<script>alert("xss")</script>`}),
		rec("brave/Default:4", base.Add(3*time.Minute), map[string]any{"url": "javascript:alert(1)", "title": "evil link"}),
		rec("brave/Default:5", base.Add(4*time.Minute), map[string]any{"url": "https://other.test/", "title": "Other"}),
	})
	const sid = "11111111-2222-3333-4444-555555555555"
	s.store.Upsert(ctx, "claude-code", "laptop", []store.Record{
		rec(sid+":u1", base.Add(10*time.Minute), map[string]any{"type": "user", "uuid": "u1", "sessionId": sid, "cwd": "/home/u/myproject",
			"message": map[string]any{"role": "user", "content": "保存形式を検討したい <b>bold</b>"}}),
		rec(sid+":a1", base.Add(11*time.Minute), map[string]any{"type": "assistant", "uuid": "a1", "sessionId": sid, "cwd": "/home/u/myproject",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "SQLite を使います"},
				map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "ls -la"}},
				map[string]any{"type": "thinking", "thinking": "考え中"},
			}}}),
		rec(sid+":u2", base.Add(12*time.Minute), map[string]any{"type": "user", "uuid": "u2", "sessionId": sid,
			"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "content": "total 0"}}}}),
		rec(sid+":ai-title", base.Add(12*time.Minute), map[string]any{"type": "ai-title", "sessionId": sid, "aiTitle": "Storage design"}),
	})
}

func TestPagesRequireLogin(t *testing.T) {
	s := newSite(t)
	for _, path := range []string{"/", "/day/2026-10-10", "/search", "/status", "/session/11111111-2222", "/day/2026-10-10/browser?domain=x"} {
		status, _, h := s.get(t, path)
		if status != http.StatusSeeOther || !strings.HasPrefix(h.Get("Location"), "/login?next=") {
			t.Errorf("%s: %d -> %q", path, status, h.Get("Location"))
		}
	}
	status, _, h := s.get(t, "/search?q=x", "HX-Request", "true")
	if status != http.StatusUnauthorized || !strings.HasPrefix(h.Get("HX-Redirect"), "/login") {
		t.Errorf("htmx request: %d %q", status, h.Get("HX-Redirect"))
	}
}

func TestLogin(t *testing.T) {
	s := newSite(t)
	resp, _ := s.client.PostForm(s.srv.URL+"/login", url.Values{"token": {"wrong"}})
	if resp.StatusCode != http.StatusUnauthorized || len(resp.Cookies()) != 0 {
		t.Errorf("wrong token: %d cookies %v", resp.StatusCode, resp.Cookies())
	}

	resp, _ = s.client.PostForm(s.srv.URL+"/login", url.Values{"token": {readToken}, "next": {"/status"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/status" {
		t.Errorf("login redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var c *http.Cookie
	for _, k := range resp.Cookies() {
		if k.Name == cookieName {
			c = k
		}
	}
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || strings.Contains(c.Value, readToken) {
		t.Errorf("cookie: %+v", c)
	}
	if status, _, _ := s.get(t, "/status"); status != 200 {
		t.Errorf("after login: %d", status)
	}

	s.client.PostForm(s.srv.URL+"/logout", nil)
	if status, _, _ := s.get(t, "/status"); status != http.StatusSeeOther {
		t.Errorf("after logout: %d", status)
	}
}

func TestLoginCookieIsSecureBehindTLSProxy(t *testing.T) {
	s := newSite(t)
	req, _ := http.NewRequest("POST", s.srv.URL+"/login", strings.NewReader(url.Values{"token": {readToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-Proto", "https")
	resp, _ := s.client.Do(req)
	if cs := resp.Cookies(); len(cs) != 1 || !cs[0].Secure {
		t.Errorf("cookies: %v", cs)
	}
}

func TestLoginRedirectStaysOnSite(t *testing.T) {
	for in, want := range map[string]string{
		"/day/2026-10-10": "/day/2026-10-10", "": "/", "https://evil.example/": "/", "//evil.example": "/",
		"/\\evil.example": "/", "/login": "/", "javascript:alert(1)": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	s := newSite(t)
	_, _, h := s.get(t, "/login")
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %q: %s", want, csp)
		}
	}
	if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", h)
	}
	if status, body, _ := s.get(t, "/static/app.css"); status != 200 || !strings.Contains(body, "--bg") {
		t.Errorf("static css: %d", status)
	}
	if status, body, _ := s.get(t, "/static/htmx.min.js"); status != 200 || !strings.Contains(body, "htmx") {
		t.Errorf("static htmx: %d", status)
	}
}

func TestIndexAndDay(t *testing.T) {
	s := newSite(t)
	seed(t, s)
	s.login(t)

	_, body, _ := s.get(t, "/")
	for _, want := range []string{"2026-10-10", "(Sat)", "Browser 5", "Claude Code 4", `href="/day/2026-10-12"`} {
		if !strings.Contains(body, want) {
			t.Errorf("index lacks %q", want)
		}
	}

	status, body, _ := s.get(t, "/day/2026-10-10")
	if status != 200 {
		t.Fatalf("day: %d", status)
	}
	for _, want := range []string{
		"Storage design", "myproject", "10:10–10:12", "user 1 / replies 1", "保存形式を検討したい",
		"example.com", "3 visits", "other.test", `href="/day/2026-10-09"`, `href="/day/2026-10-11"`,
		`hx-get="/day/2026-10-10/browser?domain=example.com"`, `href="/session/11111111-2222-3333-4444-555555555555"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("day page lacks %q", want)
		}
	}
	if strings.Index(body, "Claude Code") > strings.Index(body, "Browser") {
		t.Error("sections out of order")
	}

	if status, body, _ := s.get(t, "/day/2026-01-01"); status != 200 || !strings.Contains(body, "No records for this day") {
		t.Errorf("empty day: %d", status)
	}
	for _, bad := range []string{"/day/not-a-date", "/day/2026-13-45", "/day/2026-1-1"} {
		if status, _, _ := s.get(t, bad); status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", bad, status)
		}
	}
}

func TestBrowserDomainFragmentEscapesAndCollapses(t *testing.T) {
	s := newSite(t)
	seed(t, s)
	s.login(t)

	status, body, _ := s.get(t, "/day/2026-10-10/browser?domain=example.com")
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	if strings.Contains(body, "<script>") || !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("title not escaped: %s", body)
	}
	if strings.Count(body, "https://example.com/a?x=1</span>") != 1 || !strings.Contains(body, "2 visits") {
		t.Errorf("repeated visits not collapsed: %s", body)
	}
	if strings.Contains(body, "<html") {
		t.Error("fragment must not include the layout")
	}

	_, body, _ = s.get(t, "/day/2026-10-10/browser?domain=%28unknown%29")
	if strings.Contains(body, `href="javascript:`) {
		t.Errorf("javascript: link survived: %s", body)
	}
	if !strings.Contains(body, "#ZgotmplZ") {
		t.Errorf("unsafe URL was not neutralised: %s", body)
	}
}

func TestSession(t *testing.T) {
	s := newSite(t)
	seed(t, s)
	s.login(t)
	const sid = "11111111-2222-3333-4444-555555555555"

	status, body, _ := s.get(t, "/session/"+sid)
	if status != 200 {
		t.Fatalf("status %d", status)
	}
	for _, want := range []string{"Storage design", "myproject", "3 messages", "SQLite を使います", "Bash", "ls -la", "考え中", "total 0", `id="m-u1"`, "&lt;b&gt;bold&lt;/b&gt;"} {
		if !strings.Contains(body, want) {
			t.Errorf("session lacks %q", want)
		}
	}
	if strings.Contains(body, "<b>bold</b>") {
		t.Error("message HTML was not escaped")
	}

	for _, bad := range []string{"/session/zzz", "/session/ffffffff-ffff-ffff-ffff-ffffffffffff"} {
		if status, _, _ := s.get(t, bad); status != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", bad, status)
		}
	}
	// The mux cleans the path and redirects; it must never serve a session.
	if status, body, _ := s.get(t, "/session/../etc"); status == http.StatusOK && strings.Contains(body, "messages") {
		t.Errorf("path traversal reached a page: %d", status)
	}
}

func TestSessionPaging(t *testing.T) {
	s := newSite(t)
	const sid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	var recs []store.Record
	for i := 0; i < messagesPerPage*2+5; i++ {
		recs = append(recs, rec(fmt.Sprintf("%s:u%d", sid, i), base.Add(time.Duration(i)*time.Second), map[string]any{
			"type": "user", "uuid": fmt.Sprintf("u%d", i), "message": map[string]any{"content": fmt.Sprintf("message number %d", i)}}))
	}
	s.store.Upsert(context.Background(), "claude-code", "laptop", recs)
	s.login(t)

	_, body, _ := s.get(t, "/session/"+sid)
	if !strings.Contains(body, "message number 0<") || strings.Contains(body, "message number 200<") || !strings.Contains(body, "1 / 3") {
		t.Error("first page wrong")
	}
	_, body, _ = s.get(t, "/session/"+sid+"?page=3")
	if !strings.Contains(body, "message number 404<") || strings.Contains(body, "message number 399<") {
		t.Error("last page wrong")
	}
	_, body, _ = s.get(t, "/session/"+sid+"?m=u250")
	if !strings.Contains(body, "message number 250<") || !strings.Contains(body, "2 / 3") {
		t.Error("?m= must open the page that holds the message")
	}
	if status, _, _ := s.get(t, "/session/"+sid+"?page=999"); status != 200 {
		t.Errorf("out-of-range page: %d", status)
	}
}

func TestSearch(t *testing.T) {
	s := newSite(t)
	seed(t, s)
	s.login(t)

	_, body, _ := s.get(t, "/search?q=保存形式")
	for _, want := range []string{"Results: 1", "<mark>保存形式</mark>", `href="/session/11111111-2222-3333-4444-555555555555?m=u1#m-u1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("search lacks %q\n%s", want, body)
		}
	}
	if !strings.Contains(body, "<html") {
		t.Error("full page expected without HX-Request")
	}

	_, frag, _ := s.get(t, "/search?q=storage", "HX-Request", "true")
	if strings.Contains(frag, "<html") || !strings.Contains(frag, "<mark>Storage</mark>") {
		t.Errorf("fragment: %s", frag)
	}

	_, body, _ = s.get(t, "/search?q=alert")
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") && !strings.Contains(body, "<mark>alert</mark>") {
		t.Errorf("search output not escaped: %s", body)
	}
	if strings.Contains(body, `href="javascript:`) {
		t.Error("javascript: link in results")
	}

	_, body, _ = s.get(t, "/search?q=設計&source=browser-history")
	if !strings.Contains(body, "No records match") {
		t.Errorf("source filter ignored: %s", body)
	}
	_, body, _ = s.get(t, "/search?q=zzzzzz")
	if !strings.Contains(body, "No records match") {
		t.Error("no-hit message missing")
	}
	if status, _, _ := s.get(t, "/search?q=%22%20OR%20x"); status != 200 {
		t.Errorf("odd query: %d", status)
	}
	if status, _, _ := s.get(t, "/search"); status != 200 {
		t.Errorf("empty search page: %d", status)
	}
}

func TestStatusMarksStaleSources(t *testing.T) {
	// The store stamps ingestion with the wall clock, so the web clock must be
	// set relative to it rather than to base.
	s := newSiteAt(t, time.Now().Add(72*time.Hour))
	seed(t, s)
	s.login(t)
	_, body, _ := s.get(t, "/status")
	for _, want := range []string{"laptop", "Browser", "Claude Code", "Nothing received for 36+ hours"} {
		if !strings.Contains(body, want) {
			t.Errorf("status lacks %q", want)
		}
	}
}

func TestSnippet(t *testing.T) {
	long := strings.Repeat("あ", 100) + "Needle" + strings.Repeat("い", 100)
	got := string(snippet(long, "needle", 5))
	if got != "…あああああ<mark>Needle</mark>いいいいい…" {
		t.Errorf("snippet = %q", got)
	}
	if got := string(snippet("a <b>&</b> c", "&", 10)); !strings.Contains(got, "<mark>&amp;</mark>") || strings.Contains(got, "<b>") {
		t.Errorf("snippet did not escape: %q", got)
	}
	if got := string(snippet("no match here", "zzz", 10)); got != "no match here" {
		t.Errorf("fallback = %q", got)
	}
}
