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
	tz := fs.String("timezone", "Asia/Tokyo", "time zone that decides which day is today in the web interface")
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
	if *readTokenFile != "" {
		webHandler, err := newWebHandler(st, *readTokenFile, *tz)
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

func newWebHandler(st *store.Store, tokenFile, tz string) (http.Handler, error) {
	b, err := os.ReadFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("read token: %w", err)
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return nil, errors.New("read token file is empty")
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("timezone: %w", err)
	}
	w, err := web.New(web.Config{Store: st, ReadToken: token, Location: loc, FailDelay: 500 * time.Millisecond})
	if err != nil {
		return nil, err
	}
	return w.Handler(), nil
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
