package index

import (
	"strings"
	"testing"
)

func TestBrowserHistory(t *testing.T) {
	title, body := Text("browser-history", []byte(`{"url":"https://example.com/a","title":"Example"}`))
	if title != "Example" || body != "https://example.com/a" {
		t.Errorf("%q %q", title, body)
	}
}

func TestClaudeCode(t *testing.T) {
	cases := []struct {
		name, payload, title, body string
	}{
		{"user string", `{"type":"user","message":{"role":"user","content":"設計を見直す"}}`, "", "設計を見直す"},
		{"assistant blocks", `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"secret thoughts"},{"type":"text","text":"answer"},{"type":"tool_use","name":"Bash","input":{"command":"ls -la","description":"list","timeout":5}}]}}`, "", "answer\nBash\nlist\nls -la"},
		{"title", `{"type":"ai-title","aiTitle":"Fix the build"}`, "Fix the build", ""},
		{"tool result user", `{"type":"user","message":{"content":[{"type":"tool_result","content":"huge output"}]}}`, "", ""},
		{"system", `{"type":"system","content":"x"}`, "", ""},
		{"broken", `{`, "", ""},
	}
	for _, c := range cases {
		title, body := Text("claude-code", []byte(c.payload))
		if title != c.title || body != c.body {
			t.Errorf("%s: %q %q, want %q %q", c.name, title, body, c.title, c.body)
		}
	}
}

func TestUnknownSourceAndClipping(t *testing.T) {
	if title, body := Text("withings", []byte(`{}`)); title != "" || body != "" {
		t.Error("unknown source must not be indexed")
	}
	long := `{"type":"user","message":{"content":"` + strings.Repeat("あ", 10000) + `"}}`
	_, body := Text("claude-code", []byte(long))
	if len(body) > maxBodyBytes || !strings.HasPrefix(body, "あ") {
		t.Errorf("clipped body is %d bytes", len(body))
	}
}
