package peer

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// store is the peer plugin's SQLite state, layered over the core.Store (which owns
// the events ring and cursors). It holds the pull-through mirror (peer_nodes) and
// per-peer liveness (peer_state). Both are add-only, created idempotently in migrate.
type store struct{ core *core.Store }

const rfc = time.RFC3339Nano

const peerSchema = `
CREATE TABLE IF NOT EXISTS peer_nodes (
	key          TEXT PRIMARY KEY,
	peer_host    TEXT,
	node_json    BLOB,
	last_seen_at TEXT
);
CREATE TABLE IF NOT EXISTS peer_state (
	host         TEXT PRIMARY KEY,
	url          TEXT,
	last_seen_at TEXT,
	stale_since  TEXT,
	lag_seconds  REAL NOT NULL DEFAULT 0
);`

// migrate creates the two state tables, add-only and idempotent.
func (s *store) migrate(ctx context.Context) error {
	if _, err := s.core.DB().ExecContext(ctx, peerSchema); err != nil {
		return fmt.Errorf("peer: migrate: %w", err)
	}
	return nil
}

func (s *store) db() *sql.DB { return s.core.DB() }

// mirroredNode is one cached remote node plus its mirror freshness.
type mirroredNode struct {
	Key        string
	Host       string
	NodeJSON   []byte
	LastSeenAt time.Time
}

// upsertNode writes (replacing) one mirrored node.
func (s *store) upsertNode(ctx context.Context, n mirroredNode) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO peer_nodes (key, peer_host, node_json, last_seen_at) VALUES (?,?,?,?)
		 ON CONFLICT(key) DO UPDATE SET
		   peer_host=excluded.peer_host, node_json=excluded.node_json, last_seen_at=excluded.last_seen_at`,
		n.Key, n.Host, n.NodeJSON, n.LastSeenAt.UTC().Format(rfc))
	if err != nil {
		return fmt.Errorf("peer: upsert %q: %w", n.Key, err)
	}
	return nil
}

// nodesForHost returns every mirrored node currently cached for host, the warm-path
// read that serves S2/F8 with zero upstream call.
func (s *store) nodesForHost(ctx context.Context, host string) ([]mirroredNode, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, peer_host, node_json, last_seen_at FROM peer_nodes WHERE peer_host=? ORDER BY key`, host)
	if err != nil {
		return nil, fmt.Errorf("peer: nodesForHost %q: %w", host, err)
	}
	defer func() { _ = rows.Close() }()
	var out []mirroredNode
	for rows.Next() {
		var n mirroredNode
		var seen string
		if err := rows.Scan(&n.Key, &n.Host, &n.NodeJSON, &seen); err != nil {
			return nil, err
		}
		n.LastSeenAt, _ = time.Parse(rfc, seen)
		out = append(out, n)
	}
	return out, rows.Err()
}

// purgeStale deletes host's mirrored rows last seen before cutoff (mirrorTTL, S2).
func (s *store) purgeStale(ctx context.Context, host string, cutoff time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`DELETE FROM peer_nodes WHERE peer_host=? AND last_seen_at < ?`, host, cutoff.UTC().Format(rfc))
	return err
}

// countForHost returns the number of mirrored rows for host (peers.mirroredKeys).
func (s *store) countForHost(ctx context.Context, host string) (int, error) {
	var n int
	err := s.db().QueryRowContext(ctx, `SELECT COUNT(*) FROM peer_nodes WHERE peer_host=?`, host).Scan(&n)
	return n, err
}

// peerState is one peer's liveness row, source of the peers query and staleness.
type peerState struct {
	Host       string
	URL        string
	LastSeenAt *time.Time
	StaleSince *time.Time
	LagSeconds float64
}

// initState seeds a peer_state row at boot so the peers query lists a configured
// peer before any liveness probe has run. It never overwrites an existing row.
func (s *store) initState(ctx context.Context, host, url string) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT OR IGNORE INTO peer_state (host, url, last_seen_at, stale_since, lag_seconds)
		 VALUES (?,?,NULL,NULL,0)`, host, url)
	return err
}

// markSeen records a live peer: last_seen_at advances and any stale mark clears.
func (s *store) markSeen(ctx context.Context, host string, at time.Time, lag float64) error {
	_, err := s.db().ExecContext(ctx,
		`UPDATE peer_state SET last_seen_at=?, stale_since=NULL, lag_seconds=? WHERE host=?`,
		at.UTC().Format(rfc), lag, host)
	return err
}

// markStale sets stale_since to at only if it is not already set (first-failure
// time is preserved across the whole outage), and reports whether it transitioned.
func (s *store) markStale(ctx context.Context, host string, at time.Time) (bool, error) {
	res, err := s.db().ExecContext(ctx,
		`UPDATE peer_state SET stale_since=? WHERE host=? AND stale_since IS NULL`,
		at.UTC().Format(rfc), host)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// state returns one peer's liveness row.
func (s *store) state(ctx context.Context, host string) (peerState, error) {
	return scanState(s.db().QueryRowContext(ctx,
		`SELECT host, url, last_seen_at, stale_since, lag_seconds FROM peer_state WHERE host=?`, host))
}

// states returns every peer's liveness row, ordered by host (the peers query).
func (s *store) states(ctx context.Context) ([]peerState, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT host, url, last_seen_at, stale_since, lag_seconds FROM peer_state ORDER BY host`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []peerState
	for rows.Next() {
		ps, err := scanState(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ps)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanState(row scanner) (peerState, error) {
	var ps peerState
	var seen, stale sql.NullString
	if err := row.Scan(&ps.Host, &ps.URL, &seen, &stale, &ps.LagSeconds); err != nil {
		return ps, err
	}
	ps.LastSeenAt = parseNullTime(seen)
	ps.StaleSince = parseNullTime(stale)
	return ps, nil
}

func parseNullTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	if t, err := time.Parse(rfc, s.String); err == nil {
		return &t
	}
	return nil
}
