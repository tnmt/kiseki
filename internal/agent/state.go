package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// State records, per collection unit (for example one browser profile), the
// newest occurred_at already handed to the server or the spool.
type State map[string]int64

func LoadState(dir string) (State, error) {
	s := State{}
	return s, loadJSON(dir, "state.json", &s)
}

func (s State) Save(dir string) error { return saveJSON(dir, "state.json", s) }

func loadJSON(dir, name string, v any) error {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func saveJSON(dir, name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}
