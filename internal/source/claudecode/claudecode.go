package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tnmt/kiseki/internal/store"
)

const Source = "claude-code"

// maxPayloadBytes is kept below the server's 1 MiB limit.
const maxPayloadBytes = 900 << 10

var recordTypes = map[string]bool{"user": true, "assistant": true, "system": true}

// FileState is where reading of a session file resumes. Session files only
// grow, so a byte offset is enough.
type FileState struct {
	Offset int64  `json:"offset"`
	LastTS int64  `json:"last_ts"`
	CWD    string `json:"cwd"`
}

type Result struct {
	Records []store.Record
	FileState
}

type Options struct {
	ExcludeCWDPrefixes []string
}

// Files lists session files under projectsDir, including sub-agent transcripts.
func Files(projectsDir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(projectsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".jsonl") {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

// ReadFile converts the complete lines after st.Offset into records. A final
// line without a newline is left for the next run because the session may
// still be writing it. A later line with the same uuid replaces an earlier
// one: Claude Code appends a rewritten copy to update metadata.
func ReadFile(path string, st FileState, tz string, opts Options) (Result, error) {
	f, err := os.Open(path)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if _, err := f.Seek(st.Offset, io.SeekStart); err != nil {
		return Result{}, err
	}

	res := Result{FileState: st}
	index := map[string]int{}
	lastTS, cwd := st.LastTS, st.CWD
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := r.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, err
		}
		res.Offset += int64(len(line))

		rec, ok := convert(bytes.TrimRight(line, "\r\n"), tz, &lastTS, &cwd, opts)
		if !ok {
			continue
		}
		if i, dup := index[rec.RecordID]; dup {
			res.Records[i] = rec
			continue
		}
		index[rec.RecordID] = len(res.Records)
		res.Records = append(res.Records, rec)
	}
	res.CWD = cwd
	res.LastTS = lastTS
	for _, rec := range res.Records {
		res.LastTS = max(res.LastTS, rec.OccurredAt)
	}
	return res, nil
}

type header struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	CWD       string `json:"cwd"`
}

// convert returns the record for one line, or false for lines that are not
// archived. lastTS and cwd carry context to lines that have neither, such as
// the session title.
func convert(line []byte, tz string, lastTS *int64, cwd *string, opts Options) (store.Record, bool) {
	var h header
	if err := json.Unmarshal(line, &h); err != nil || h.SessionID == "" {
		return store.Record{}, false
	}
	if h.CWD != "" {
		*cwd = h.CWD
	}
	if excluded(*cwd, opts.ExcludeCWDPrefixes) {
		return store.Record{}, false
	}

	var recordID string
	var at int64
	switch {
	case recordTypes[h.Type] && h.UUID != "":
		t, err := time.Parse(time.RFC3339Nano, h.Timestamp)
		if err != nil {
			return store.Record{}, false
		}
		recordID, at = h.SessionID+":"+h.UUID, t.UnixMilli()
		*lastTS = max(*lastTS, at)
	case h.Type == "ai-title":
		if *lastTS == 0 {
			return store.Record{}, false
		}
		recordID, at = h.SessionID+":ai-title", *lastTS
	default:
		return store.Record{}, false
	}

	payload, ok := sanitize(line)
	if !ok {
		return store.Record{}, false
	}
	return store.Record{RecordID: recordID, OccurredAt: at, TZ: tz, Payload: payload}, true
}

func excluded(cwd string, prefixes []string) bool {
	if cwd == "" {
		return false
	}
	for _, p := range prefixes {
		p = strings.TrimRight(p, "/")
		if cwd == p || strings.HasPrefix(cwd, p+"/") {
			return true
		}
	}
	return false
}

// sanitize redacts secrets and shrinks the line. A line that is still too
// large becomes a stub that keeps the envelope, so the record exists and the
// batch is not rejected.
func sanitize(line []byte) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	out, err := marshal(scrub(v))
	if err != nil {
		return nil, false
	}
	if len(out) <= maxPayloadBytes {
		return out, true
	}
	stub := map[string]any{"truncated": true, "original_bytes": len(line)}
	for _, k := range []string{"type", "uuid", "sessionId", "timestamp", "cwd", "parentUuid", "isSidechain"} {
		if val, ok := v[k]; ok {
			stub[k] = val
		}
	}
	out, err = marshal(stub)
	return out, err == nil
}
