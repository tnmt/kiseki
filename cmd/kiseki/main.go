package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tnmt/kiseki/internal/agent"
	"github.com/tnmt/kiseki/internal/auth"
	"github.com/tnmt/kiseki/internal/collector/withings"
	"github.com/tnmt/kiseki/internal/overland"
	"github.com/tnmt/kiseki/internal/server"
	"github.com/tnmt/kiseki/internal/source"
	"github.com/tnmt/kiseki/internal/store"
	"github.com/tnmt/kiseki/internal/web"
)

const usage = `usage: kiseki <command> [flags]

commands:
  serve       run the ingest and read API server
  agent       collect local data and push it to the server
  token new   issue an agent token
  snapshot    write a consistent copy of the database
  withings auth
              authorize the Withings collector
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kiseki:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("command required")
	}
	switch args[0] {
	case "serve":
		return serve(args[1:])
	case "agent":
		return runAgent(args[1:])
	case "snapshot":
		return snapshot(args[1:])
	case "withings":
		if len(args) < 2 || args[1] != "auth" {
			return errors.New("usage: kiseki withings auth -dir D -client-id ID -client-secret-file F")
		}
		return withingsAuth(args[2:])
	case "token":
		if len(args) < 2 || args[1] != "new" {
			return errors.New("usage: kiseki token new -name N -host H -sources a,b")
		}
		return tokenNew(args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func tokenNew(args []string) error {
	fs := flag.NewFlagSet("token new", flag.ContinueOnError)
	name := fs.String("name", "", "label for the token")
	host := fs.String("host", "", "host name recorded with every record from this token")
	sources := fs.String("sources", "", "comma-separated sources the token may write")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *name == "" || *host == "" || *sources == "" {
		return errors.New("-name, -host and -sources are required")
	}
	token, entry, err := auth.New(*name, *host, strings.Split(*sources, ","))
	if err != nil {
		return err
	}
	out, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "token (shown once): %s\n", token)
	fmt.Println(string(out))
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "127.0.0.1:8080", "HTTP listen address")
	dbPath := fs.String("db", "kiseki.db", "SQLite database path")
	tokensFile := fs.String("tokens-file", "", "JSON file listing agent tokens")
	readTokenFile := fs.String("read-token-file", "", "file containing the read-only token for the web interface; the interface is disabled when unset")
	tz := fs.String("timezone", "Asia/Tokyo", "time zone that decides which day is today in the web interface, and the fallback zone of collected records")
	hostname, _ := os.Hostname()
	host := fs.String("host", hostname, "host name recorded with records from server-side collectors")
	withingsDir := fs.String("withings-dir", "", "writable directory for the Withings collector's token and state; the collector is disabled when unset")
	withingsClientID := fs.String("withings-client-id", "", "Withings application client ID")
	withingsSecretFile := fs.String("withings-client-secret-file", "", "file containing the Withings application client secret")
	withingsInterval := fs.Duration("withings-interval", time.Hour, "how often the Withings collector runs; Withings allows one call per 10 minutes")
	withingsOverlap := fs.Duration("withings-overlap", 48*time.Hour, "how far before the previous sync the Withings collector starts")
	overlandURL := fs.String("overland-url", "", "base URL of overland-server, whose stays and moves the web interface shows on each day; disabled when unset")
	overlandTokenFile := fs.String("overland-read-token-file", "", "file containing overland-server's read token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *tokensFile == "" {
		return errors.New("-tokens-file is required")
	}
	tokens, err := auth.Load(*tokensFile)
	if err != nil {
		return fmt.Errorf("tokens: %w", err)
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer st.Close()

	var handler http.Handler = server.New(st, tokens, source.Known).Handler()
	var locations web.LocationSource
	if *overlandURL != "" {
		if *readTokenFile == "" {
			return errors.New("-overland-url requires the web interface (-read-token-file)")
		}
		token, err := readSecret(*overlandTokenFile, "overland read token")
		if err != nil {
			return err
		}
		locations = &overland.Client{BaseURL: *overlandURL, Token: token, HTTP: &http.Client{Timeout: 10 * time.Second}}
	}
	if *readTokenFile != "" {
		webHandler, err := newWebHandler(st, *readTokenFile, *tz, locations)
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.Handle("/api/", handler)
		mux.Handle("/healthz", handler)
		mux.Handle("/", webHandler)
		handler = mux
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *withingsDir != "" {
		if *host == "" {
			return errors.New("-host is required for the Withings collector")
		}
		if *withingsInterval < 10*time.Minute {
			return errors.New("-withings-interval must be at least 10m")
		}
		app, err := withingsApp(*withingsClientID, *withingsSecretFile, "")
		if err != nil {
			return err
		}
		c := &withings.Collector{
			App: app, Dir: *withingsDir, Store: st, Host: *host, TZ: *tz,
			Overlap: *withingsOverlap, Log: slog.Default().With("collector", "withings"),
		}
		go c.Run(ctx, *withingsInterval)
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		httpSrv.Shutdown(shutdownCtx)
	}()

	slog.Info("listening", "addr", *listen, "db", *dbPath)
	if err := httpSrv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func readSecret(path, what string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("a file containing the %s is required", what)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", what, err)
	}
	secret := strings.TrimSpace(string(b))
	if secret == "" {
		return "", fmt.Errorf("%s file is empty", what)
	}
	return secret, nil
}

func newWebHandler(st *store.Store, tokenFile, tz string, locations web.LocationSource) (http.Handler, error) {
	token, err := readSecret(tokenFile, "read token")
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	w, err := web.New(web.Config{Store: st, ReadToken: token, Location: loc, FailDelay: 500 * time.Millisecond, Locations: locations})
	if err != nil {
		return nil, err
	}
	return w.Handler(), nil
}

func withingsApp(clientID, secretFile, redirectURI string) (*withings.App, error) {
	if clientID == "" || secretFile == "" {
		return nil, errors.New("the Withings client ID and client secret file are required")
	}
	secret, err := readSecret(secretFile, "withings client secret")
	if err != nil {
		return nil, err
	}
	return &withings.App{
		ClientID: clientID, ClientSecret: secret, RedirectURI: redirectURI,
		HTTP: &http.Client{Timeout: time.Minute},
	}, nil
}

func withingsAuth(args []string) error {
	fs := flag.NewFlagSet("withings auth", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory to write token.json into (the server's -withings-dir, or a local one to copy from)")
	clientID := fs.String("client-id", "", "Withings application client ID")
	secretFile := fs.String("client-secret-file", "", "file containing the Withings application client secret")
	redirectURI := fs.String("redirect-uri", "http://127.0.0.1:8123/callback", "redirect URI registered with the Withings application; must be loopback")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("-dir is required")
	}
	app, err := withingsApp(*clientID, *secretFile, *redirectURI)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	tok, err := app.Authorize(ctx, func(u string) {
		fmt.Fprintf(os.Stderr, "open this URL in a browser that can reach %s:\n\n%s\n\n", *redirectURI, u)
	})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o700); err != nil {
		return err
	}
	path := withings.TokenPath(*dir)
	if err := withings.SaveToken(path, tok); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "token for Withings user %s written to %s\n", tok.UserID, path)
	return nil
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	configPath := fs.String("config", "", "agent configuration file (JSON)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *configPath == "" {
		return errors.New("-config is required")
	}
	cfg, err := agent.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return agent.Run(ctx, cfg, slog.Default())
}

func snapshot(args []string) error {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	dbPath := fs.String("db", "kiseki.db", "SQLite database path")
	out := fs.String("out", "", "where to write the copy")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("-out is required")
	}
	if err := store.Snapshot(context.Background(), *dbPath, *out); err != nil {
		return err
	}
	slog.Info("snapshot written", "out", *out)
	return nil
}
