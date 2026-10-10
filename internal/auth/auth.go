package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
)

// Entry grants one agent write access to a set of sources. Host is fixed by
// the token so an agent cannot write as another host.
type Entry struct {
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	Sources     []string `json:"sources"`
	TokenSHA256 string   `json:"token_sha256"`
}

func (e Entry) Allows(source string) bool { return slices.Contains(e.Sources, source) }

type Tokens struct {
	entries []Entry
}

func Load(path string) (*Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var entries []Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, e := range entries {
		if e.Name == "" || e.Host == "" || len(e.Sources) == 0 {
			return nil, fmt.Errorf("%s: entry %q needs name, host and sources", path, e.Name)
		}
		if raw, err := hex.DecodeString(e.TokenSHA256); err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("%s: entry %q has an invalid token_sha256", path, e.Name)
		}
	}
	return &Tokens{entries: entries}, nil
}

// Lookup returns the entry whose hash matches token. Every entry is compared
// so the time taken does not reveal which one matched.
func (t *Tokens) Lookup(token string) (Entry, bool) {
	sum := sha256.Sum256([]byte(token))
	var found Entry
	var ok bool
	for _, e := range t.entries {
		want, _ := hex.DecodeString(e.TokenSHA256)
		if subtle.ConstantTimeCompare(sum[:], want) == 1 {
			found, ok = e, true
		}
	}
	return found, ok
}

// New generates a token from 32 random bytes and the entry that authorizes it.
func New(name, host string, sources []string) (token string, entry Entry, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", Entry{}, err
	}
	token = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(token))
	return token, Entry{Name: name, Host: host, Sources: sources, TokenSHA256: hex.EncodeToString(sum[:])}, nil
}
