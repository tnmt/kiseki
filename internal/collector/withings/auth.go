package withings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"time"
)

// CallbackPath is the path of the redirect URI registered with Withings.
const CallbackPath = "/callback"

// Authorize runs the authorization-code flow. It listens on the redirect URI's
// loopback address only until one redirect arrives, passes the authorization
// URL to show, and exchanges the code at once because it expires 30 seconds
// after the redirect.
func (a *App) Authorize(ctx context.Context, show func(authURL string)) (*Token, error) {
	redirect, err := url.Parse(a.RedirectURI)
	if err != nil {
		return nil, fmt.Errorf("redirect URI: %w", err)
	}
	if redirect.Scheme != "http" || !isLoopback(redirect.Hostname()) || redirect.Port() == "" {
		return nil, fmt.Errorf("redirect URI %q must be http on a loopback address with a port", a.RedirectURI)
	}
	ln, err := net.Listen("tcp", redirect.Host)
	if err != nil {
		return nil, err
	}

	stateBytes := make([]byte, 16)
	rand.Read(stateBytes)
	state := hex.EncodeToString(stateBytes)

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(redirect.Path, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "invalid state", http.StatusBadRequest)
			return
		}
		code := q.Get("code")
		var res result
		if code == "" {
			res.err = errors.New("withings: authorization was not granted")
			writePage(w, http.StatusBadRequest, "Authorization was not granted.")
		} else {
			res.code = code
			writePage(w, http.StatusOK, "kiseki received the authorization. You can close this tab.")
		}
		select {
		case done <- res:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	show(a.AuthorizeURL(state))

	var res result
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res = <-done:
	}
	if res.err != nil {
		return nil, res.err
	}
	return a.Exchange(ctx, res.code, time.Now())
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writePage(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, "<!doctype html><meta charset=\"utf-8\"><title>kiseki</title><p>%s</p>", html.EscapeString(message))
}
