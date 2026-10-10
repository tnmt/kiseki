package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Spool keeps batches that could not be delivered. Files in pending/ are
// retried in order; files the server rejected move to rejected/ for a person
// to look at.
type Spool struct {
	dir string
}

func NewSpool(dir string) (*Spool, error) {
	for _, sub := range []string{"pending", "rejected"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) Add(b Batch) error {
	body, err := json.Marshal(b)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d.json", time.Now().UnixNano())
	tmp := filepath.Join(s.dir, "pending", name+".tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, "pending", name))
}

// Flush sends pending batches oldest first. It stops at the first delivery
// failure so ordering is kept; the remaining files stay for the next run.
func (s *Spool) Flush(ctx context.Context, c *Client, log *slog.Logger) error {
	files, err := filepath.Glob(filepath.Join(s.dir, "pending", "*.json"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var b Batch
		if err := json.Unmarshal(body, &b); err != nil {
			log.Error("spool file is unreadable; moving to rejected", "file", f, "err", err)
			if err := os.Rename(f, filepath.Join(s.dir, "rejected", filepath.Base(f))); err != nil {
				return err
			}
			continue
		}
		_, err = c.Post(ctx, b)
		var rejected *RejectedError
		switch {
		case err == nil:
			if err := os.Remove(f); err != nil {
				return err
			}
		case errors.As(err, &rejected):
			log.Error("server rejected a spooled batch; moving to rejected", "file", f, "err", err)
			if err := os.Rename(f, filepath.Join(s.dir, "rejected", filepath.Base(f))); err != nil {
				return err
			}
		default:
			return err
		}
	}
	return nil
}
