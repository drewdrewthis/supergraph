package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; registers driver name "sqlite" (PRD §5 D1)
)

// Store is core's per-plugin SQLite base. It owns event ingest (a fixed-capacity
// ring) and cursor persistence; a plugin owns any additional STATE tables it
// creates via Migrate. One Store maps to one .db file.
type Store struct {
	db           *sql.DB
	ringCapacity int
}

// OpenStore opens (creating as needed) the SQLite database at path, configures it
// for concurrent access, and runs core's own migrations. ringCapacity is the max
// number of events retained; older events are pruned on each append.
//
// WAL + a 5s busy_timeout is what lets a reader (Events) run concurrently with the
// writer (AppendEvent) without a "database is locked" error, so core does not need
// to serialize all access to a single connection (PRD §5 D1, risk 3).
func OpenStore(ctx context.Context, path string, ringCapacity int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("core: create data dir: %w", err)
	}

	// Pragmas go in the DSN, not a one-off Exec, because busy_timeout is
	// per-connection: with a multi-connection pool, a concurrent reader on a
	// different connection than the one we Exec'd would get SQLITE_BUSY ("database
	// is locked") immediately. DSN pragmas run on every connection the pool opens,
	// which is what makes WAL reads concurrent with the writer safe (AC-CORE-17).
	dsn := "file:" + path + "?_pragma=journal_mode%28WAL%29&_pragma=busy_timeout%285000%29"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("core: open sqlite %s: %w", path, err)
	}

	s := &Store{db: db, ringCapacity: ringCapacity}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// migrate creates core's tables idempotently. It runs on every open, so reopening
// an existing db is a no-op that never errors or loses data.
func (s *Store) migrate(ctx context.Context) error {
	const schema = `
CREATE TABLE IF NOT EXISTS events (
	id      INTEGER PRIMARY KEY AUTOINCREMENT,
	ts      TEXT,
	source  TEXT,
	type    TEXT,
	v       INTEGER,
	key     TEXT,
	payload BLOB
);
CREATE TABLE IF NOT EXISTS cursors (
	name  TEXT PRIMARY KEY,
	value TEXT
);`
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("core: migrate: %w", err)
	}
	return nil
}

// DB exposes the underlying handle so a plugin can create and query its own STATE
// tables against the same database.
func (s *Store) DB() *sql.DB { return s.db }

// AppendEvent inserts one event and prunes the oldest rows so the total never
// exceeds ringCapacity — both in a single transaction so a concurrent reader never
// observes an over-capacity or partially-pruned ring.
func (s *Store) AppendEvent(ctx context.Context, e Envelope) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("core: begin append tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events (ts, source, type, v, key, payload) VALUES (?,?,?,?,?,?)`,
		e.TS.UTC().Format(timeLayout), e.Source, e.Type, e.V, e.Key, []byte(e.Payload),
	); err != nil {
		return fmt.Errorf("core: insert event: %w", err)
	}

	// Keep only the newest ringCapacity rows by insert order (id).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT ?)`,
		s.ringCapacity,
	); err != nil {
		return fmt.Errorf("core: prune ring: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("core: commit append: %w", err)
	}
	return nil
}

// SetCursor upserts a named cursor value (e.g. a poll high-water mark).
func (s *Store) SetCursor(ctx context.Context, name, value string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO cursors (name, value) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET value=excluded.value`,
		name, value,
	); err != nil {
		return fmt.Errorf("core: set cursor %q: %w", name, err)
	}
	return nil
}

// Cursor returns the named cursor's value, or "" with a nil error when it has
// never been set — callers treat absent and empty identically.
func (s *Store) Cursor(ctx context.Context, name string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM cursors WHERE name = ?`, name).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("core: read cursor %q: %w", name, err)
	}
	return value, nil
}

// Events returns up to limit events, newest first.
func (s *Store) Events(ctx context.Context, limit int) ([]Envelope, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, source, type, v, key, payload FROM events ORDER BY id DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("core: query events: %w", err)
	}
	defer rows.Close()

	var out []Envelope
	for rows.Next() {
		var (
			e     Envelope
			tsStr string
			pay   []byte
		)
		if err := rows.Scan(&tsStr, &e.Source, &e.Type, &e.V, &e.Key, &pay); err != nil {
			return nil, fmt.Errorf("core: scan event: %w", err)
		}
		ts, perr := parseTime(tsStr)
		if perr != nil {
			return nil, fmt.Errorf("core: parse event ts %q: %w", tsStr, perr)
		}
		e.TS = ts
		e.Payload = append([]byte(nil), pay...)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("core: iterate events: %w", err)
	}
	return out, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }
