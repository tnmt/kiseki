// Package index extracts the text of a record that full-text search covers.
package index

import (
	"encoding/json"
	"strings"
)

// maxBodyBytes bounds the indexed text of one record.
const maxBodyBytes = 16 << 10

// Text returns the searchable title and body of a payload. Both are empty for
// records that are not indexed.
func Text(source string, payload []byte) (title, body string) {
	switch source {
	case "browser-history":
		var p struct {
			URL   string `json:"url"`
			Title string `json:"title"`
		}
		if json.Unmarshal(payload, &p) != nil {
			return "", ""
		}
		return p.Title, p.URL
	case "claude-code":
		return claudeText(payload)
	}
	return "", ""
}

type claudeLine struct {
	Type    string `json:"type"`
	AITitle string `json:"aiTitle"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type block struct {
	Type  string         `json:"type"`
	Text  string         `json:"text"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

func claudeText(payload []byte) (string, string) {
	var l claudeLine
	if json.Unmarshal(payload, &l) != nil {
		return "", ""
	}
	if l.Type == "ai-title" {
		return l.AITitle, ""
	}
	if l.Type != "user" && l.Type != "assistant" {
		return "", ""
	}

	var text string
	if json.Unmarshal(l.Message.Content, &text) == nil {
		return "", clip(text)
	}
	var blocks []block
	if json.Unmarshal(l.Message.Content, &blocks) != nil {
		return "", ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "tool_use":
			parts = append(parts, b.Name)
			for _, key := range []string{"description", "command", "file_path", "pattern", "query", "url"} {
				if s, ok := b.Input[key].(string); ok {
					parts = append(parts, s)
				}
			}
		}
	}
	return "", clip(strings.Join(parts, "\n"))
}

func clip(s string) string {
	if len(s) <= maxBodyBytes {
		return s
	}
	return strings.ToValidUTF8(s[:maxBodyBytes], "")
}
