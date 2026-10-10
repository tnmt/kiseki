package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func line(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

func msg(typ, uuid, ts, text string) map[string]any {
	return map[string]any{
		"type": typ, "uuid": uuid, "sessionId": "s1", "timestamp": ts, "cwd": "/home/u/proj",
		"message": map[string]any{"role": typ, "content": text},
	}
}

func write(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadFileKeepsConversationAndSkipsNoise(t *testing.T) {
	p := write(t,
		line(t, map[string]any{"type": "permission-mode", "sessionId": "s1"})+
			line(t, msg("user", "u1", "2026-10-10T01:00:00.000Z", "hello"))+
			line(t, map[string]any{"type": "attachment", "uuid": "a1", "sessionId": "s1", "timestamp": "2026-10-10T01:00:01.000Z", "attachment": map[string]any{"type": "total_tokens_reminder"}})+
			line(t, msg("assistant", "u2", "2026-10-10T01:00:02.000Z", "hi"))+
			line(t, map[string]any{"type": "ai-title", "sessionId": "s1", "aiTitle": "Greeting"}))
	res, err := ReadFile(p, FileState{}, "Asia/Tokyo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, r := range res.Records {
		ids = append(ids, r.RecordID)
		if r.TZ != "Asia/Tokyo" {
			t.Errorf("tz = %s", r.TZ)
		}
	}
	if strings.Join(ids, ",") != "s1:u1,s1:u2,s1:ai-title" {
		t.Fatalf("records = %v", ids)
	}
	if res.Records[0].OccurredAt != 1_791_594_000_000 {
		t.Errorf("occurred_at = %d", res.Records[0].OccurredAt)
	}
	title := res.Records[2]
	if title.OccurredAt != res.Records[1].OccurredAt {
		t.Errorf("title takes the time of the preceding message, got %d", title.OccurredAt)
	}
	if res.LastTS != res.Records[1].OccurredAt || res.CWD != "/home/u/proj" {
		t.Errorf("state: %+v", res.FileState)
	}
}

func TestReadFileResumesAndLeavesPartialLine(t *testing.T) {
	first := line(t, msg("user", "u1", "2026-10-10T01:00:00.000Z", "one"))
	second := line(t, msg("user", "u2", "2026-10-10T01:00:05.000Z", "two"))
	partial := second[:len(second)/2]
	p := write(t, first+partial)

	res, err := ReadFile(p, FileState{}, "UTC", Options{})
	if err != nil || len(res.Records) != 1 || res.Offset != int64(len(first)) {
		t.Fatalf("first pass: %d records, offset %d (want %d), err %v", len(res.Records), res.Offset, len(first), err)
	}

	if err := os.WriteFile(p, []byte(first+second), 0o600); err != nil {
		t.Fatal(err)
	}
	res2, err := ReadFile(p, res.FileState, "UTC", Options{})
	if err != nil || len(res2.Records) != 1 || res2.Records[0].RecordID != "s1:u2" {
		t.Fatalf("second pass: %+v err %v", res2.Records, err)
	}
	if res2.Offset != int64(len(first)+len(second)) {
		t.Errorf("offset = %d", res2.Offset)
	}

	res3, _ := ReadFile(p, res2.FileState, "UTC", Options{})
	if len(res3.Records) != 0 {
		t.Errorf("third pass re-read %d records", len(res3.Records))
	}
}

func TestLaterCopyOfSameUUIDWins(t *testing.T) {
	a := msg("user", "u1", "2026-10-10T01:00:00.000Z", "same")
	a["promptId"] = "old"
	b := msg("user", "u1", "2026-10-10T01:00:00.000Z", "same")
	b["promptId"] = "new"
	res, err := ReadFile(write(t, line(t, a)+line(t, b)), FileState{}, "UTC", Options{})
	if err != nil || len(res.Records) != 1 {
		t.Fatalf("records = %d err %v", len(res.Records), err)
	}
	if !strings.Contains(string(res.Records[0].Payload), `"promptId":"new"`) {
		t.Errorf("payload = %s", res.Records[0].Payload)
	}
}

func TestExcludeByCWDAlsoCoversTitle(t *testing.T) {
	m := msg("user", "u1", "2026-10-10T01:00:00.000Z", "private")
	m["cwd"] = "/home/u/secret/sub"
	p := write(t, line(t, m)+line(t, map[string]any{"type": "ai-title", "sessionId": "s1", "aiTitle": "t"}))
	res, _ := ReadFile(p, FileState{}, "UTC", Options{ExcludeCWDPrefixes: []string{"/home/u/secret"}})
	if len(res.Records) != 0 {
		t.Errorf("excluded session produced %d records", len(res.Records))
	}
	res, _ = ReadFile(p, FileState{}, "UTC", Options{ExcludeCWDPrefixes: []string{"/home/u/sec"}})
	if len(res.Records) != 2 {
		t.Errorf("a prefix that is not a path component must not match, got %d records", len(res.Records))
	}
}

func TestRedactionAndShrinking(t *testing.T) {
	big := strings.Repeat("x", 300<<10)
	m := msg("user", "u1", "2026-10-10T01:00:00.000Z", "key sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123 and ghp_"+strings.Repeat("a", 36))
	m["systemReport"] = "Windows Version: Windows 10 Home\nProduct  Key: ABCDE-12345-FGHIJ-67890-KLMNO\nCLAUDE_CODE_MESSAGING_TOKEN=tok_0123456789abcdef"
	m["toolUseResult"] = map[string]any{
		"password": "hunter2hunter2",
		"stdout":   "DB_PASSWORD=supersecretvalue\nAuthorization: Bearer abcdefghijklmnopqrstuvwx\n-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
		"big":      big,
		"image":    map[string]any{"type": "base64", "media_type": "image/jpeg", "data": strings.Repeat("A", 5000)},
	}
	res, err := ReadFile(write(t, line(t, m)), FileState{}, "UTC", Options{})
	if err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v records %d", err, len(res.Records))
	}
	got := string(res.Records[0].Payload)
	for _, leak := range []string{"sk-ant-api03", "ghp_aaaa", "hunter2hunter2", "supersecretvalue", "ABCDE-12345", "tok_0123456789abcdef", "abcdefghijklmnopqrstuvwx", "MIIE", strings.Repeat("A", 100)} {
		if strings.Contains(got, leak) {
			t.Errorf("payload still contains %q", leak)
		}
	}
	for _, want := range []string{"DB_PASSWORD=[REDACTED]", "CLAUDE_CODE_MESSAGING_TOKEN=[REDACTED]", "Product  Key: [REDACTED]", "Authorization: Bearer [REDACTED]", "[image data omitted: 5000 bytes]", "[truncated "} {
		if !strings.Contains(got, want) {
			t.Errorf("payload lacks %q", want)
		}
	}
	if !json.Valid(res.Records[0].Payload) {
		t.Error("payload is not valid JSON")
	}
}

func TestOversizedLineBecomesStub(t *testing.T) {
	m := msg("user", "u1", "2026-10-10T01:00:00.000Z", "x")
	parts := make([]any, 8)
	for i := range parts {
		parts[i] = strings.Repeat("y", 200<<10)
	}
	m["many"] = parts
	res, err := ReadFile(write(t, line(t, m)), FileState{}, "UTC", Options{})
	if err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v records %d", err, len(res.Records))
	}
	var stub map[string]any
	json.Unmarshal(res.Records[0].Payload, &stub)
	if stub["truncated"] != true || stub["uuid"] != "u1" || len(res.Records[0].Payload) > maxPayloadBytes {
		t.Errorf("stub = %v (%d bytes)", stub, len(res.Records[0].Payload))
	}
}

func TestNumbersAndUnicodeSurvive(t *testing.T) {
	m := msg("assistant", "u1", "2026-10-10T01:00:00.000Z", "日本語 <b>&</b>")
	m["usage"] = map[string]any{"input_tokens": 12345678901234}
	res, _ := ReadFile(write(t, line(t, m)), FileState{}, "UTC", Options{})
	got := string(res.Records[0].Payload)
	if !strings.Contains(got, "日本語 <b>&</b>") || !strings.Contains(got, `"input_tokens":12345678901234`) {
		t.Errorf("payload = %s", got)
	}
}

func TestFilesIncludesSubagents(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"proj/s1.jsonl", "proj/s1/subagents/agent-a.jsonl", "proj/s1/tool-results/x.txt", "proj/memory/m.md"} {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o700)
		os.WriteFile(p, nil, 0o600)
	}
	files, err := Files(root)
	if err != nil || len(files) != 2 {
		t.Errorf("files = %v err %v", files, err)
	}
}

func TestRedactionLeavesOrdinaryTextAlone(t *testing.T) {
	for _, s := range []string{
		"key: string",
		"const tokenFile = path",
		"MAX_TOKEN_COUNT=5",
		"session 4686bb19-0ef6-4e9b-b2e7-d5ddc36893f5 started",
		"total_tokens: 12345",
		"PRIMARY_KEY=id",
	} {
		if got := scrubString(s); got != s {
			t.Errorf("scrubString(%q) = %q", s, got)
		}
	}
}
