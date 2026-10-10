package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"time"
	_ "time/tzdata"

	"github.com/tnmt/kiseki/internal/index"
	_ "modernc.org/sqlite"
)

type migration struct {
	sql string
	// after runs in the same transaction, for changes SQL cannot express.
	after func(tx *sql.Tx) error
}

// migrations are applied in order; PRAGMA user_version holds the number applied.
var migrations = []migration{
	{sql: `
CREATE TABLE records (
	id          INTEGER PRIMARY KEY,
	source      TEXT    NOT NULL,
	host        TEXT    NOT NULL,
	record_id   TEXT    NOT NULL,
	occurred_at INTEGER NOT NULL, -- Unix milliseconds, UTC
	tz          TEXT    NOT NULL, -- IANA name
	local_date  TEXT    NOT NULL, -- YYYY-MM-DD in tz
	ingested_at INTEGER NOT NULL, -- Unix milliseconds, UTC
	payload     TEXT    NOT NULL, -- raw JSON
	UNIQUE (source, host, record_id)
);
CREATE INDEX records_local_date ON records (local_date);
CREATE INDEX records_host_source_occurred ON records (host, source, occurred_at);
`},
	{
		sql: `
-- rowid equals records.id. trigram matches substrings of three or more
-- characters, which suits Japanese text without a word segmenter.
CREATE VIRTUAL TABLE search USING fts5(title, body, tokenize = 'trigram');
`,
		after: backfillSearch,
	},
}

type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Record is one item as sent by an agent or a collector.
type Record struct {
	RecordID   string          `json:"record_id"`
	OccurredAt int64           `json:"occurred_at"`
	TZ         string          `json:"tz"`
	Payload    json.RawMessage `json:"payload"`
}

type UpsertResult struct {
	Inserted int `json:"inserted"`
	Updated  int `json:"updated"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// A single connection keeps writers serialized and PRAGMAs applied.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = NORMAL",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i].sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if migrations[i].after != nil {
			if err := migrations[i].after(tx); err != nil {
				tx.Rollback()
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Upsert stores all records in one transaction. A record with an existing
// (source, host, record_id) replaces the stored one.
func (s *Store) Upsert(ctx context.Context, source, host string, records []Record) (UpsertResult, error) {
	var res UpsertResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback()

	ingestedAt := s.now().UnixMilli()
	for _, r := range records {
		loc, err := time.LoadLocation(r.TZ)
		if err != nil {
			return res, fmt.Errorf("record %q: timezone: %w", r.RecordID, err)
		}
		localDate := time.UnixMilli(r.OccurredAt).In(loc).Format(time.DateOnly)

		var id int64
		err = tx.QueryRowContext(ctx,
			`SELECT id FROM records WHERE source = ? AND host = ? AND record_id = ?`,
			source, host, r.RecordID).Scan(&id)
		switch err {
		case nil:
			res.Updated++
		case sql.ErrNoRows:
			res.Inserted++
		default:
			return res, err
		}

		if _, err := tx.ExecContext(ctx, `
INSERT INTO records (source, host, record_id, occurred_at, tz, local_date, ingested_at, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (source, host, record_id) DO UPDATE SET
	occurred_at = excluded.occurred_at,
	tz          = excluded.tz,
	local_date  = excluded.local_date,
	ingested_at = excluded.ingested_at,
	payload     = excluded.payload`,
			source, host, r.RecordID, r.OccurredAt, r.TZ, localDate, ingestedAt, string(r.Payload)); err != nil {
			return res, err
		}
		if id == 0 {
			if err := tx.QueryRowContext(ctx,
				`SELECT id FROM records WHERE source = ? AND host = ? AND record_id = ?`,
				source, host, r.RecordID).Scan(&id); err != nil {
				return res, err
			}
		}
		if err := reindex(ctx, tx, id, source, r.Payload); err != nil {
			return res, err
		}
	}
	return res, tx.Commit()
}

// LatestOccurredAt returns the newest occurred_at for the host and source, or
// false when there are no records.
func (s *Store) LatestOccurredAt(ctx context.Context, source, host string) (int64, bool, error) {
	var latest sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(occurred_at) FROM records WHERE source = ? AND host = ?`,
		source, host).Scan(&latest)
	if err != nil {
		return 0, false, err
	}
	return latest.Int64, latest.Valid, nil
}

// reindex replaces the search entry of one record.
func reindex(ctx context.Context, tx *sql.Tx, id int64, source string, payload []byte) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM search WHERE rowid = ?`, id); err != nil {
		return err
	}
	title, body := index.Text(source, payload)
	if title == "" && body == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO search (rowid, title, body) VALUES (?, ?, ?)`, id, title, body)
	return err
}

func backfillSearch(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, source, payload FROM records`)
	if err != nil {
		return err
	}
	type entry struct {
		id          int64
		title, body string
	}
	var entries []entry
	for rows.Next() {
		var id int64
		var source, payload string
		if err := rows.Scan(&id, &source, &payload); err != nil {
			rows.Close()
			return err
		}
		if title, body := index.Text(source, []byte(payload)); title != "" || body != "" {
			entries = append(entries, entry{id, title, body})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	for _, e := range entries {
		if _, err := tx.Exec(`INSERT INTO search (rowid, title, body) VALUES (?, ?, ?)`, e.id, e.title, e.body); err != nil {
			return err
		}
	}
	return nil
}

// Snapshot writes a consistent copy of the database at dbPath to outPath.
// It is safe while the server is running: VACUUM INTO reads in one
// transaction, which WAL mode allows beside a writer. The copy is made under
// a temporary name and renamed, so a reader never sees a partial file.
func Snapshot(ctx context.Context, dbPath, outPath string) error {
	if _, err := os.Stat(dbPath); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout = 5000"); err != nil {
		return err
	}

	tmp := outPath + ".tmp"
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", tmp); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("vacuum into: %w", err)
	}
	if err := os.Rename(tmp, outPath); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
