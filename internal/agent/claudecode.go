package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tnmt/kiseki/internal/source/claudecode"
	"github.com/tnmt/kiseki/internal/store"
)

const claudeStateFile = "claude-code-state.json"

func collectClaudeCode(ctx context.Context, cfg *Config, client *Client, spool *Spool, log *slog.Logger) error {
	cc := cfg.ClaudeCode
	dir := cc.ProjectsDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		dir = filepath.Join(home, ".claude", "projects")
	}
	files, err := claudecode.Files(dir)
	if errors.Is(err, os.ErrNotExist) {
		log.Info("no Claude Code projects directory; skipping", "dir", dir)
		return nil
	}
	if err != nil {
		return err
	}

	state := map[string]claudecode.FileState{}
	if err := loadJSON(cfg.SpoolDir, claudeStateFile, &state); err != nil {
		return fmt.Errorf("state: %w", err)
	}
	serverCursor, haveServer, cursorErr := client.Cursor(ctx, claudecode.Source)
	if cursorErr != nil {
		log.Warn("could not read the server cursor; using local state only", "err", cursorErr)
		haveServer = false
	}

	opts := claudecode.Options{ExcludeCWDPrefixes: cc.ExcludeCWDPrefixes}
	var errs []error
	var totalSent, totalSpooled, filesRead int
	for _, file := range files {
		key, err := filepath.Rel(dir, file)
		if err != nil {
			key = file
		}
		info, err := os.Stat(file)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}

		st, known := state[key]
		var notBefore int64
		switch {
		case !known:
		case info.Size() < st.Offset:
			st = claudecode.FileState{}
		case haveServer && st.LastTS > serverCursor:
			// The server holds less than this machine has already sent, as
			// after a restore from backup. Read again from the start and
			// resend what the server may be missing.
			st = claudecode.FileState{}
			notBefore = max(serverCursor-cfg.overlap().Milliseconds(), 0)
		case info.Size() == st.Offset:
			continue
		}

		res, err := claudecode.ReadFile(file, st, cfg.Timezone, opts)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}
		records := res.Records
		if notBefore > 0 {
			records = filterSince(records, notBefore)
		}
		sent, spooled, err := deliver(ctx, client, spool, claudecode.Source, records)
		totalSent += sent
		totalSpooled += spooled
		filesRead++
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			continue
		}

		state[key] = res.FileState
		if err := saveJSON(cfg.SpoolDir, claudeStateFile, state); err != nil {
			errs = append(errs, err)
		}
	}
	log.Info("claude code", "files", len(files), "read", filesRead, "sent", totalSent, "spooled", totalSpooled)
	return errors.Join(errs...)
}

func filterSince(records []store.Record, since int64) []store.Record {
	kept := records[:0]
	for _, r := range records {
		if r.OccurredAt >= since {
			kept = append(kept, r)
		}
	}
	return kept
}
