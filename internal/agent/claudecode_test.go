package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tnmt/kiseki/internal/store"
)

func sessionLine(uuid string, at time.Time, text string) string {
	return fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"s1","timestamp":%q,"cwd":"/work/proj","message":{"role":"user","content":%q}}`+"\n",
		uuid, at.UTC().Format(time.RFC3339Nano), text)
}

func (e *env) claudeSetup(t *testing.T, exclude ...string) (projects, file string) {
	t.Helper()
	projects = filepath.Join(t.TempDir(), "projects")
	file = filepath.Join(projects, "-work-proj", "s1.jsonl")
	os.MkdirAll(filepath.Dir(file), 0o700)
	e.cfg.BrowserHistory = nil
	e.cfg.ClaudeCode = &ClaudeCodeConfig{ProjectsDir: projects, ExcludeCWDPrefixes: exclude}
	return projects, file
}

func (e *env) countSource(t *testing.T, source string) int {
	t.Helper()
	var n int
	if err := e.peek.QueryRow(`SELECT COUNT(*) FROM records WHERE source = ? AND host = 'laptop'`, source).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCodeIncrementalAndPartialLine(t *testing.T) {
	e := newEnv(t)
	_, file := e.claudeSetup(t)
	base := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)

	appendTo(t, file, sessionLine("u1", base, "one"))
	second := sessionLine("u2", base.Add(time.Minute), "two")
	appendTo(t, file, second[:len(second)/2])

	run := func() {
		t.Helper()
		if err := Run(context.Background(), e.cfg, quiet); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if n := e.countSource(t, "claude-code"); n != 1 {
		t.Fatalf("%d records; the half-written line must wait", n)
	}

	appendTo(t, file, second[len(second)/2:])
	appendTo(t, file, sessionLine("u3", base.Add(2*time.Minute), "three"))
	run()
	if n := e.countSource(t, "claude-code"); n != 3 {
		t.Fatalf("%d records after completing the line, want 3", n)
	}

	run()
	if n := e.countSource(t, "claude-code"); n != 3 {
		t.Errorf("%d records after an idle run, want 3", n)
	}
}

func TestClaudeCodeRewindsAfterServerRestore(t *testing.T) {
	e := newEnv(t)
	_, file := e.claudeSetup(t)
	base := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	appendTo(t, file, sessionLine("u1", base, "one"))
	appendTo(t, file, sessionLine("u2", base.Add(10*time.Hour), "two"))
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}

	if _, err := e.peek.Exec(`DELETE FROM records WHERE record_id = 's1:u2'`); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	if n := e.countSource(t, "claude-code"); n != 2 {
		t.Errorf("%d records after restore, want 2", n)
	}
}

func TestClaudeCodeStartsOverWhenFileShrinks(t *testing.T) {
	e := newEnv(t)
	_, file := e.claudeSetup(t)
	base := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	appendTo(t, file, sessionLine("u1", base, strings.Repeat("long ", 50)))
	Run(context.Background(), e.cfg, quiet)

	os.WriteFile(file, []byte(sessionLine("u9", base.Add(time.Hour), "x")), 0o600)
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	var n int
	e.peek.QueryRow(`SELECT COUNT(*) FROM records WHERE record_id = 's1:u9'`).Scan(&n)
	if n != 1 {
		t.Errorf("replaced file was not read from the start")
	}
}

func TestClaudeCodeExcludedProjectIsNotSent(t *testing.T) {
	e := newEnv(t)
	_, file := e.claudeSetup(t, "/work/proj")
	appendTo(t, file, sessionLine("u1", time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC), "private"))
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Fatal(err)
	}
	if n := e.countSource(t, "claude-code"); n != 0 {
		t.Errorf("%d records from an excluded project", n)
	}
}

func TestClaudeCodeMissingProjectsDirIsNotAnError(t *testing.T) {
	e := newEnv(t)
	e.claudeSetup(t)
	e.cfg.ClaudeCode.ProjectsDir = filepath.Join(t.TempDir(), "absent")
	if err := Run(context.Background(), e.cfg, quiet); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestSplitBatchesByCountAndBytes(t *testing.T) {
	mk := func(n, payloadBytes int) []store.Record {
		recs := make([]store.Record, n)
		for i := range recs {
			recs[i] = store.Record{RecordID: fmt.Sprint(i), Payload: []byte(`"` + strings.Repeat("x", payloadBytes) + `"`)}
		}
		return recs
	}
	if got := splitBatches("x", mk(1200, 10)); len(got) != 3 || len(got[0].Records) != batchMaxRecords {
		t.Errorf("by count: %d batches", len(got))
	}
	big := splitBatches("x", mk(10, 900<<10))
	total := 0
	for _, b := range big {
		sum := 0
		for _, r := range b.Records {
			sum += len(r.Payload)
		}
		if sum > batchMaxBytes {
			t.Errorf("batch of %d bytes exceeds the limit", sum)
		}
		total += len(b.Records)
	}
	if len(big) < 2 || total != 10 {
		t.Errorf("by bytes: %d batches, %d records", len(big), total)
	}
	if got := splitBatches("x", nil); len(got) != 0 {
		t.Errorf("empty input gave %d batches", len(got))
	}
}
