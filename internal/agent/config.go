package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
)

type Config struct {
	Server       string `json:"server"`
	ServerFile   string `json:"server_file"`
	TokenFile    string `json:"token_file"`
	SpoolDir     string `json:"spool_dir"`
	Timezone     string `json:"timezone"`
	OverlapHours int    `json:"overlap_hours"`

	BrowserHistory *BrowserHistoryConfig `json:"browser_history"`
	ClaudeCode     *ClaudeCodeConfig     `json:"claude_code"`

	retryBackoff time.Duration
}

type BrowserHistoryConfig struct {
	// Browsers are scanned for profiles in their default data directories:
	// "brave-origin" and "chrome".
	Browsers []string `json:"browsers"`
	// HistoryPaths are History files outside the default locations.
	HistoryPaths []string `json:"history_paths"`
	// ExcludeProfiles are "<browser>/<profile directory>" entries, such as
	// "brave-origin/Profile 1". The directory name is used rather than the
	// display name because renaming a profile must not start sending it.
	ExcludeProfiles []string `json:"exclude_profiles"`
	ExcludeDomains  []string `json:"exclude_domains"`
}

type ClaudeCodeConfig struct {
	// ProjectsDir defaults to ~/.claude/projects.
	ProjectsDir string `json:"projects_dir"`
	// ExcludeCWDPrefixes skips sessions whose working directory is at or under
	// one of these paths.
	ExcludeCWDPrefixes []string `json:"exclude_cwd_prefixes"`
}

func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if (c.Server == "") == (c.ServerFile == "") {
		return nil, errors.New("config: exactly one of server and server_file is required")
	}
	if c.ServerFile != "" {
		b, err := os.ReadFile(c.ServerFile)
		if err != nil {
			return nil, fmt.Errorf("config: server_file: %w", err)
		}
		c.Server = strings.TrimSpace(string(b))
		if c.Server == "" {
			return nil, fmt.Errorf("config: server_file %s is empty", c.ServerFile)
		}
	}
	if c.TokenFile == "" || c.SpoolDir == "" {
		return nil, errors.New("config: token_file and spool_dir are required")
	}
	c.Server = strings.TrimRight(c.Server, "/")
	if c.OverlapHours == 0 {
		c.OverlapHours = 48
	}
	if c.OverlapHours < 0 {
		return nil, errors.New("config: overlap_hours must not be negative")
	}
	if c.Timezone == "" {
		name := time.Local.String()
		if name == "Local" {
			return nil, errors.New("config: timezone is required because the system zone has no IANA name")
		}
		c.Timezone = name
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return nil, fmt.Errorf("config: timezone: %w", err)
	}
	if bh := c.BrowserHistory; bh != nil {
		for _, e := range bh.ExcludeProfiles {
			browser, name, ok := strings.Cut(e, "/")
			if !ok || name == "" || !slices.Contains(bh.Browsers, browser) {
				return nil, fmt.Errorf("config: exclude_profiles: %q must be <browser>/<profile directory> with a browser listed in browsers", e)
			}
		}
	}
	return &c, nil
}

func (c *Config) overlap() time.Duration { return time.Duration(c.OverlapHours) * time.Hour }
