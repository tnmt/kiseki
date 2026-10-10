package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeTokens(t *testing.T, entries ...Entry) string {
	t.Helper()
	b, _ := json.Marshal(entries)
	path := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLookup(t *testing.T) {
	tokA, entA, _ := New("laptop", "laptop", []string{"browser-history"})
	tokB, entB, _ := New("desktop", "desktop", []string{"claude-code"})
	tokens, err := Load(writeTokens(t, entA, entB))
	if err != nil {
		t.Fatal(err)
	}

	got, ok := tokens.Lookup(tokA)
	if !ok || got.Host != "laptop" || !got.Allows("browser-history") || got.Allows("claude-code") {
		t.Errorf("laptop token: %+v ok=%v", got, ok)
	}
	got, ok = tokens.Lookup(tokB)
	if !ok || got.Host != "desktop" {
		t.Errorf("desktop token: %+v ok=%v", got, ok)
	}
	if _, ok := tokens.Lookup("wrong"); ok {
		t.Error("wrong token accepted")
	}
	if _, ok := tokens.Lookup(""); ok {
		t.Error("empty token accepted")
	}
}

func TestLoadRejectsBadEntries(t *testing.T) {
	_, good, _ := New("a", "a", []string{"x"})
	cases := map[string]Entry{
		"no host":    {Name: "a", Sources: []string{"x"}, TokenSHA256: good.TokenSHA256},
		"no sources": {Name: "a", Host: "a", TokenSHA256: good.TokenSHA256},
		"bad hash":   {Name: "a", Host: "a", Sources: []string{"x"}, TokenSHA256: "zz"},
	}
	for name, e := range cases {
		if _, err := Load(writeTokens(t, e)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestNewTokensDiffer(t *testing.T) {
	a, _, _ := New("a", "a", []string{"x"})
	b, _, _ := New("a", "a", []string{"x"})
	if a == b || len(a) < 43 {
		t.Errorf("tokens %q %q", a, b)
	}
}
