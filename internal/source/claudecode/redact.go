package claudecode

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	redacted = "[REDACTED]"
	// Strings longer than this are cut. Tool output such as a whole file
	// listing can be megabytes, and the server rejects records over 1 MiB.
	maxStringBytes = 256 << 10
)

// Each pattern matches a secret in a known format. Patterns with a capture
// group keep the group (the key or header name) and replace only the rest.
var secretPatterns = []struct {
	re   *regexp.Regexp
	keep bool
}{
	{regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`), false},
	{regexp.MustCompile(`sk-ant-[A-Za-z0-9_-]{20,}`), false},
	{regexp.MustCompile(`\bsk-[A-Za-z0-9]{32,}`), false},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,}`), false},
	{regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{50,}`), false},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), false},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), false},
	{regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`), false},
	{regexp.MustCompile(`\b[A-Z0-9]{5}(?:-[A-Z0-9]{5}){4}\b`), false},
	{regexp.MustCompile(`(\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)*_(?:TOKEN|KEY|SECRET|PASSWORD|PASSWD)\s*=\s*["']?)[^\s"',;]{8,}`), true},
	{regexp.MustCompile(`(?i)(authorization:\s*bearer\s+)[A-Za-z0-9._~+/=-]{16,}`), true},
	{regexp.MustCompile(`(?i)([A-Za-z0-9_]*(?:password|passwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token)[A-Za-z0-9_]*["']?\s*[=:]\s*["']?)[^\s"',;]{8,}`), true},
}

var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "secret": true, "client_secret": true,
	"api_key": true, "apikey": true, "access_token": true, "refresh_token": true,
	"id_token": true, "authorization": true, "private_key": true,
}

func scrubString(s string) string {
	if len(s) > maxStringBytes {
		s = fmt.Sprintf("%s…[truncated %d bytes]", strings.ToValidUTF8(s[:maxStringBytes], ""), len(s)-maxStringBytes)
	}
	for _, p := range secretPatterns {
		if p.keep {
			s = p.re.ReplaceAllString(s, "${1}"+redacted)
		} else {
			s = p.re.ReplaceAllString(s, redacted)
		}
	}
	return s
}

// scrub walks a decoded JSON value, replacing secrets, dropping inline image
// data and truncating oversized strings.
func scrub(v any) any {
	switch x := v.(type) {
	case string:
		return scrubString(x)
	case []any:
		for i := range x {
			x[i] = scrub(x[i])
		}
		return x
	case map[string]any:
		if x["type"] == "base64" {
			if data, ok := x["data"].(string); ok {
				x["data"] = fmt.Sprintf("[image data omitted: %d bytes]", len(data))
			}
		}
		for k, val := range x {
			if s, ok := val.(string); ok && sensitiveKeys[strings.ToLower(k)] && s != "" {
				x[k] = redacted
				continue
			}
			x[k] = scrub(val)
		}
		return x
	default:
		return v
	}
}

func marshal(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(b.String(), "\n")), nil
}
