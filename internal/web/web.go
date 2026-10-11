// Package web serves the read-only browser interface.
package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/tnmt/kiseki/internal/store"
)

//go:embed templates static
var assets embed.FS

const (
	cookieName    = "kiseki_session"
	sessionMaxAge = 30 * 24 * 3600
	pageTemplate  = "layout.html"
)

type Config struct {
	Store     *store.Store
	ReadToken string
	// Location decides which day is "today".
	Location *time.Location
	Now      func() time.Time
	// FailDelay slows down wrong-token attempts.
	FailDelay time.Duration
}

type Web struct {
	cfg       Config
	sessionID string
	pages     map[string]*template.Template
}

func New(cfg Config) (*Web, error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	mac := hmac.New(sha256.New, []byte(cfg.ReadToken))
	mac.Write([]byte("kiseki-web-session-v1"))
	w := &Web{cfg: cfg, sessionID: hex.EncodeToString(mac.Sum(nil)), pages: map[string]*template.Template{}}

	funcs := template.FuncMap{"sourceLabel": sourceLabel, "urlq": url.QueryEscape, "count": count}
	for _, name := range []string{"index.html", "day.html", "session.html", "search.html", "status.html", "login.html"} {
		t, err := template.New(name).Funcs(funcs).ParseFS(assets, "templates/"+pageTemplate, "templates/"+name, "templates/fragments.html")
		if err != nil {
			return nil, err
		}
		w.pages[name] = t
	}
	frag, err := template.New("fragments").Funcs(funcs).ParseFS(assets, "templates/fragments.html")
	if err != nil {
		return nil, err
	}
	w.pages["fragments"] = frag
	return w, nil
}

func (w *Web) Handler() http.Handler {
	static, _ := fs.Sub(assets, "static")
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", cacheFor(5*time.Minute, http.FileServerFS(static))))
	mux.HandleFunc("GET /login", w.handleLoginForm)
	mux.HandleFunc("POST /login", w.handleLogin)
	mux.HandleFunc("POST /logout", w.handleLogout)

	mux.HandleFunc("GET /{$}", w.protect(w.handleIndex))
	mux.HandleFunc("GET /day/{date}", w.protect(w.handleDay))
	mux.HandleFunc("GET /day/{date}/browser", w.protect(w.handleBrowserDomain))
	mux.HandleFunc("GET /session/{id}", w.protect(w.handleSession))
	mux.HandleFunc("GET /search", w.protect(w.handleSearch))
	mux.HandleFunc("GET /status", w.protect(w.handleStatus))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		h := rw.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		next.ServeHTTP(rw, r)
	})
}

func cacheFor(d time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Cache-Control", "public, max-age="+itoa(int(d.Seconds())))
		next.ServeHTTP(rw, r)
	})
}

func (w *Web) authenticated(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.sessionID)) == 1
}

func (w *Web) protect(next http.HandlerFunc) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if w.authenticated(r) {
			next(rw, r)
			return
		}
		login := "/login?next=" + url.QueryEscape(r.URL.RequestURI())
		if r.Header.Get("HX-Request") == "true" {
			rw.Header().Set("HX-Redirect", login)
			rw.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(rw, r, login, http.StatusSeeOther)
	}
}

func (w *Web) handleLoginForm(rw http.ResponseWriter, r *http.Request) {
	w.render(rw, r, "login.html", http.StatusOK, map[string]any{"Next": safeNext(r.URL.Query().Get("next"))})
}

func (w *Web) handleLogin(rw http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(rw, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	if subtle.ConstantTimeCompare([]byte(r.PostFormValue("token")), []byte(w.cfg.ReadToken)) != 1 {
		time.Sleep(w.cfg.FailDelay)
		w.render(rw, r, "login.html", http.StatusUnauthorized, map[string]any{"Next": next, "Error": "Wrong token"})
		return
	}
	http.SetCookie(rw, &http.Cookie{
		Name: cookieName, Value: w.sessionID, Path: "/", MaxAge: sessionMaxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r),
	})
	http.Redirect(rw, r, next, http.StatusSeeOther)
}

func (w *Web) handleLogout(rw http.ResponseWriter, r *http.Request) {
	http.SetCookie(rw, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: isHTTPS(r),
	})
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

func isHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// safeNext only lets a login redirect stay on this site.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") || strings.HasPrefix(next, "/login") {
		return "/"
	}
	return next
}

func (w *Web) render(rw http.ResponseWriter, r *http.Request, page string, status int, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	data["Authenticated"] = w.authenticated(r)
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.WriteHeader(status)
	if err := w.pages[page].ExecuteTemplate(rw, pageTemplate, data); err != nil {
		slog.Error("render", "page", page, "err", err)
	}
}

func (w *Web) renderFragment(rw http.ResponseWriter, name string, data any) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := w.pages["fragments"].ExecuteTemplate(rw, name, data); err != nil {
		slog.Error("render fragment", "name", name, "err", err)
	}
}

func (w *Web) fail(rw http.ResponseWriter, r *http.Request, err error) {
	slog.Error("web", "path", r.URL.Path, "err", err)
	http.Error(rw, "internal error", http.StatusInternalServerError)
}

func (w *Web) notFound(rw http.ResponseWriter, r *http.Request) {
	w.render(rw, r, "index.html", http.StatusNotFound, map[string]any{"NotFound": true})
}
