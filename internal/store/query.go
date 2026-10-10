package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Row is a stored record.
type Row struct {
	ID         int64
	Source     string
	Host       string
	RecordID   string
	OccurredAt int64
	TZ         string
	LocalDate  string
	IngestedAt int64
	Payload    string
}

const rowColumns = `r.id, r.source, r.host, r.record_id, r.occurred_at, r.tz, r.local_date, r.ingested_at, r.payload`

func scanRow(sc interface{ Scan(...any) error }, extra ...any) (Row, error) {
	var r Row
	dest := append([]any{&r.ID, &r.Source, &r.Host, &r.RecordID, &r.OccurredAt, &r.TZ, &r.LocalDate, &r.IngestedAt, &r.Payload}, extra...)
	err := sc.Scan(dest...)
	return r, err
}

func (s *Store) queryRows(ctx context.Context, query string, args ...any) ([]Row, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordsForDate returns the day's records in time order. An empty source
// matches every source.
func (s *Store) RecordsForDate(ctx context.Context, date, source string) ([]Row, error) {
	return s.queryRows(ctx, `
SELECT `+rowColumns+` FROM records r
WHERE r.local_date = ? AND (? = '' OR r.source = ?)
ORDER BY r.occurred_at, r.id`, date, source, source)
}

// SessionRecords returns every Claude Code record of a session in time order.
func (s *Store) SessionRecords(ctx context.Context, sessionID string) ([]Row, error) {
	// ';' is the character after ':', so this is the prefix range "sessionID:".
	return s.queryRows(ctx, `
SELECT `+rowColumns+` FROM records r
WHERE r.source = 'claude-code' AND r.record_id >= ? AND r.record_id < ?
ORDER BY r.occurred_at, r.id`, sessionID+":", sessionID+";")
}

type DayCount struct {
	Date     string
	BySource map[string]int
}

// RecentDays returns the newest days that have records, newest first.
func (s *Store) RecentDays(ctx context.Context, limit int) ([]DayCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT local_date, source, COUNT(*) FROM records
WHERE local_date IN (SELECT DISTINCT local_date FROM records ORDER BY local_date DESC LIMIT ?)
GROUP BY local_date, source
ORDER BY local_date DESC, source`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []DayCount
	for rows.Next() {
		var date, source string
		var n int
		if err := rows.Scan(&date, &source, &n); err != nil {
			return nil, err
		}
		if len(days) == 0 || days[len(days)-1].Date != date {
			days = append(days, DayCount{Date: date, BySource: map[string]int{}})
		}
		days[len(days)-1].BySource[source] = n
	}
	return days, rows.Err()
}

type StatusRow struct {
	Host             string
	Source           string
	Count            int
	LatestOccurredAt int64
	LatestIngestedAt int64
}

// Status summarises what each host has sent for each source.
func (s *Store) Status(ctx context.Context) ([]StatusRow, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT host, source, COUNT(*), MAX(occurred_at), MAX(ingested_at)
FROM records GROUP BY host, source ORDER BY host, source`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatusRow
	for rows.Next() {
		var r StatusRow
		if err := rows.Scan(&r.Host, &r.Source, &r.Count, &r.LatestOccurredAt, &r.LatestIngestedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type SearchHit struct {
	Row
	Title string
	Body  string
}

// Search finds records whose indexed text contains q, newest first. Queries of
// three or more characters use the trigram index; shorter ones scan.
func (s *Store) Search(ctx context.Context, q, source string, limit int) ([]SearchHit, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	var where string
	var args []any
	if utf8.RuneCountInString(q) >= 3 {
		where = `search MATCH ?`
		args = append(args, `"`+strings.ReplaceAll(q, `"`, `""`)+`"`)
	} else {
		where = `(search.title LIKE ? ESCAPE '\' OR search.body LIKE ? ESCAPE '\')`
		like := "%" + likeEscaper.Replace(q) + "%"
		args = append(args, like, like)
	}
	args = append(args, source, source, limit)

	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`
SELECT %s, search.title, search.body
FROM search JOIN records r ON r.id = search.rowid
WHERE %s AND (? = '' OR r.source = ?)
ORDER BY r.occurred_at DESC, r.id DESC LIMIT ?`, rowColumns, where), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var title, body sql.NullString
		row, err := scanRow(rows, &title, &body)
		if err != nil {
			return nil, err
		}
		h.Row, h.Title, h.Body = row, title.String, body.String
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
