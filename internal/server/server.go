package server

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"

	"github.com/tnmt/kiseki/internal/auth"
	"github.com/tnmt/kiseki/internal/store"
)

const (
	maxBodyBytes    = 8 << 20
	maxPayloadBytes = 1 << 20
	maxRecords      = 1000
	maxRecordIDLen  = 256
)

type Server struct {
	store   *store.Store
	tokens  *auth.Tokens
	sources []string
}

func New(st *store.Store, tokens *auth.Tokens, knownSources []string) *Server {
	return &Server{store: st, tokens: tokens, sources: knownSources}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/v1/ingest", s.handleIngest)
	mux.HandleFunc("GET /api/v1/cursors", s.handleCursors)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type ingestRequest struct {
	Source  string         `json:"source"`
	Records []store.Record `json:"records"`
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.authenticate(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req ingestRequest
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	if !slices.Contains(s.sources, req.Source) {
		writeError(w, http.StatusBadRequest, "unknown source "+req.Source)
		return
	}
	if !entry.Allows(req.Source) {
		writeError(w, http.StatusForbidden, "token may not write source "+req.Source)
		return
	}
	if len(req.Records) > maxRecords {
		writeError(w, http.StatusRequestEntityTooLarge, "too many records in one request")
		return
	}
	for i, rec := range req.Records {
		if status, msg := validateRecord(rec); msg != "" {
			writeError(w, status, "records["+strconv.Itoa(i)+"]: "+msg)
			return
		}
	}

	res, err := s.store.Upsert(r.Context(), req.Source, entry.Host, req.Records)
	if err != nil {
		slog.Error("ingest", "host", entry.Host, "source", req.Source, "err", err)
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func validateRecord(rec store.Record) (int, string) {
	switch {
	case rec.RecordID == "" || len(rec.RecordID) > maxRecordIDLen:
		return http.StatusBadRequest, "record_id must be 1 to 256 bytes"
	case rec.OccurredAt <= 0:
		return http.StatusBadRequest, "occurred_at must be positive Unix milliseconds"
	case len(rec.Payload) == 0 || string(rec.Payload) == "null" || !json.Valid(rec.Payload):
		return http.StatusBadRequest, "payload must be a JSON value"
	case len(rec.Payload) > maxPayloadBytes:
		return http.StatusRequestEntityTooLarge, "payload exceeds 1 MiB"
	}
	if _, err := time.LoadLocation(rec.TZ); err != nil || rec.TZ == "" {
		return http.StatusBadRequest, "tz must be an IANA time zone name"
	}
	return 0, ""
}

func (s *Server) handleCursors(w http.ResponseWriter, r *http.Request) {
	entry, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	source := r.URL.Query().Get("source")
	if !slices.Contains(s.sources, source) {
		writeError(w, http.StatusBadRequest, "unknown source "+source)
		return
	}
	if !entry.Allows(source) {
		writeError(w, http.StatusForbidden, "token may not access source "+source)
		return
	}
	latest, found, err := s.store.LatestOccurredAt(r.Context(), source, entry.Host)
	if err != nil {
		slog.Error("cursors", "host", entry.Host, "source", source, "err", err)
		writeError(w, http.StatusInternalServerError, "storage error")
		return
	}
	var out *int64
	if found {
		out = &latest
	}
	writeJSON(w, http.StatusOK, map[string]any{"source": source, "latest_occurred_at": out})
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (auth.Entry, bool) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok {
		if entry, found := s.tokens.Lookup(token); found {
			return entry, true
		}
	}
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "invalid or missing token")
	return auth.Entry{}, false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
