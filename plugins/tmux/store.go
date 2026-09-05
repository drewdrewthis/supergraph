package tmux

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// store is the tmux plugin's SQLite state, layered over the core.Store (which owns
// the events ring and the cursors table). It holds the session/window/pane read
// model the reconcile poll and control-mode client keep current.
type store struct{ core *core.Store }

const rfc = time.RFC3339Nano

// PaneRow is one cached pane. HostID and indices are parsed from the key on read
// (github convention) but the session/window/pane columns are denormalised so
// free-slot and branch queries need no key parsing in SQL.
type PaneRow struct {
	Key, HostID, Session string
	Window, Pane, Pid    int
	Cmd, Path            string
	Active, Free         bool
	StaleSince           *time.Time
}

// SessionRow is one cached session, carrying the worktree/branch join github
// reserves as paneForBranch.
type SessionRow struct {
	Key, HostID, Name, Worktree, Branch string
	LastSeenAt, StaleSince              *time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS tmux_sessions (
	key          TEXT PRIMARY KEY,
	name         TEXT,
	worktree     TEXT,
	branch       TEXT,
	last_seen_at TEXT,
	stale_since  TEXT
);
CREATE TABLE IF NOT EXISTS tmux_panes (
	key          TEXT PRIMARY KEY,
	session      TEXT,
	window       INTEGER,
	pane         INTEGER,
	pid          INTEGER,
	cmd          TEXT,
	path         TEXT,
	active       INTEGER,
	free         INTEGER,
	last_seen_at TEXT,
	stale_since  TEXT
);`

func (s *store) migrate(ctx context.Context) error {
	if _, err := s.db().ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("tmux: migrate: %w", err)
	}
	return nil
}

func (s *store) db() *sql.DB { return s.core.DB() }

// upsertSession writes one session, clearing stale_since (a seen entity is live).
func (s *store) upsertSession(ctx context.Context, r SessionRow, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO tmux_sessions (key, name, worktree, branch, last_seen_at, stale_since)
		 VALUES (?,?,?,?,?,NULL)
		 ON CONFLICT(key) DO UPDATE SET name=excluded.name, worktree=excluded.worktree,
		   branch=excluded.branch, last_seen_at=excluded.last_seen_at, stale_since=NULL`,
		r.Key, r.Name, r.Worktree, r.Branch, now.UTC().Format(rfc))
	if err != nil {
		return fmt.Errorf("tmux: upsert session: %w", err)
	}
	return nil
}

// upsertPane writes one pane, clearing stale_since.
func (s *store) upsertPane(ctx context.Context, r PaneRow, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO tmux_panes (key, session, window, pane, pid, cmd, path, active, free, last_seen_at, stale_since)
		 VALUES (?,?,?,?,?,?,?,?,?,?,NULL)
		 ON CONFLICT(key) DO UPDATE SET session=excluded.session, window=excluded.window,
		   pane=excluded.pane, pid=excluded.pid, cmd=excluded.cmd, path=excluded.path,
		   active=excluded.active, free=excluded.free, last_seen_at=excluded.last_seen_at, stale_since=NULL`,
		r.Key, r.Session, r.Window, r.Pane, r.Pid, r.Cmd, r.Path, b2i(r.Active), b2i(r.Free), now.UTC().Format(rfc))
	if err != nil {
		return fmt.Errorf("tmux: upsert pane: %w", err)
	}
	return nil
}

// markStale stamps stale_since on one pane by key (a pane that vanished).
func (s *store) markPaneStale(ctx context.Context, key string, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`UPDATE tmux_panes SET stale_since=? WHERE key=? AND stale_since IS NULL`,
		now.UTC().Format(rfc), key)
	return err
}

// markAllStale stamps stale_since on every live session and pane (server down).
func (s *store) markAllStale(ctx context.Context, now time.Time) error {
	ts := now.UTC().Format(rfc)
	if _, err := s.db().ExecContext(ctx, `UPDATE tmux_panes SET stale_since=? WHERE stale_since IS NULL`, ts); err != nil {
		return err
	}
	_, err := s.db().ExecContext(ctx, `UPDATE tmux_sessions SET stale_since=? WHERE stale_since IS NULL`, ts)
	return err
}

// livePaneKeys returns the keys of every pane not currently marked stale — the set
// the reconcile diff compares against to detect vanished panes.
func (s *store) livePaneKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT key FROM tmux_panes WHERE stale_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// scanPanes returns panes, optionally filtered by host (parsed from the key).
func (s *store) scanPanes(ctx context.Context, host string) ([]PaneRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, session, window, pane, pid, cmd, path, active, free, stale_since FROM tmux_panes ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PaneRow
	for rows.Next() {
		r, err := scanPane(rows)
		if err != nil {
			return nil, err
		}
		if host == "" || r.HostID == host {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// scanSessions returns sessions, optionally filtered by host.
func (s *store) scanSessions(ctx context.Context, host string) ([]SessionRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, name, worktree, branch, last_seen_at, stale_since FROM tmux_sessions ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRow
	for rows.Next() {
		var (
			r           SessionRow
			last, stale sql.NullString
			wt, br      sql.NullString
		)
		if err := rows.Scan(&r.Key, &r.Name, &wt, &br, &last, &stale); err != nil {
			return nil, err
		}
		r.Worktree, r.Branch = wt.String, br.String
		_, r.HostID, _ = splitHost(r.Key)
		r.LastSeenAt, r.StaleSince = nsTime(last), nsTime(stale)
		if host == "" || r.HostID == host {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// panesForBranch returns panes whose session worktree is on branch (the
// issue↔branch join). It joins pane.session to session.name.
func (s *store) panesForBranch(ctx context.Context, branch string) ([]PaneRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT p.key, p.session, p.window, p.pane, p.pid, p.cmd, p.path, p.active, p.free, p.stale_since
		 FROM tmux_panes p JOIN tmux_sessions s ON p.session = s.name
		 WHERE s.branch = ? ORDER BY p.key`, branch)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PaneRow
	for rows.Next() {
		r, err := scanPane(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanPane reads one pane row and parses its host from the key.
func scanPane(rows *sql.Rows) (PaneRow, error) {
	var (
		r            PaneRow
		active, free int
		path         sql.NullString
		stale        sql.NullString
	)
	if err := rows.Scan(&r.Key, &r.Session, &r.Window, &r.Pane, &r.Pid, &r.Cmd, &path, &active, &free, &stale); err != nil {
		return r, err
	}
	r.Path, r.Active, r.Free = path.String, active == 1, free == 1
	_, r.HostID, _ = splitHost(r.Key)
	r.StaleSince = nsTime(stale)
	return r, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nsTime parses a nullable RFC3339 column into an optional time.
func nsTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t, err := time.Parse(rfc, ns.String)
	if err != nil {
		return nil
	}
	return &t
}
