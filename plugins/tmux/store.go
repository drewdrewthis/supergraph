package tmux

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
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
	// PaneID is tmux's native #{pane_id}; WindowName/WindowActive denormalise the
	// window a pane belongs to so windows are a projection over this table (#27).
	PaneID, WindowName string
	WindowActive       bool
	StaleSince         *time.Time
}

// SessionRow is one cached session, carrying the worktree/branch join github
// reserves as paneForBranch, plus the #27 attach/createdAt read model.
type SessionRow struct {
	Key, HostID, Name, Worktree, Branch string
	Attached                            bool
	CreatedAt                           *time.Time
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

// addColumns are the additive #27 columns. They are applied with ALTER TABLE ADD
// COLUMN so a DB written by the pre-#27 plugin (rows already present) upgrades in
// place; a column that already exists errors with "duplicate column name", which is
// the no-op signal on a second start (AC-TMUX-MIGRATE-INPLACE), not fatal.
var addColumns = []string{
	`ALTER TABLE tmux_sessions ADD COLUMN attached INTEGER`,
	`ALTER TABLE tmux_sessions ADD COLUMN created_at TEXT`,
	`ALTER TABLE tmux_panes ADD COLUMN pane_id TEXT`,
	`ALTER TABLE tmux_panes ADD COLUMN window_name TEXT`,
	`ALTER TABLE tmux_panes ADD COLUMN window_active INTEGER`,
}

func (s *store) migrate(ctx context.Context) error {
	if _, err := s.db().ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("tmux: migrate: %w", err)
	}
	for _, stmt := range addColumns {
		if _, err := s.db().ExecContext(ctx, stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("tmux: migrate add column: %w", err)
		}
	}
	return nil
}

// isDuplicateColumn reports whether err is SQLite's benign "column already exists"
// — the signal that a second start over an already-migrated DB is a no-op.
func isDuplicateColumn(err error) bool {
	return strings.Contains(err.Error(), "duplicate column name")
}

func (s *store) db() *sql.DB { return s.core.DB() }

// upsertSession writes one session, clearing stale_since (a seen entity is live).
func (s *store) upsertSession(ctx context.Context, r SessionRow, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO tmux_sessions (key, name, worktree, branch, attached, created_at, last_seen_at, stale_since)
		 VALUES (?,?,?,?,?,?,?,NULL)
		 ON CONFLICT(key) DO UPDATE SET name=excluded.name, worktree=excluded.worktree,
		   branch=excluded.branch, attached=excluded.attached, created_at=excluded.created_at,
		   last_seen_at=excluded.last_seen_at, stale_since=NULL`,
		r.Key, r.Name, r.Worktree, r.Branch, b2i(r.Attached), nsFmt(r.CreatedAt), now.UTC().Format(rfc))
	if err != nil {
		return fmt.Errorf("tmux: upsert session: %w", err)
	}
	return nil
}

// upsertPane writes one pane, clearing stale_since.
func (s *store) upsertPane(ctx context.Context, r PaneRow, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO tmux_panes (key, session, window, pane, pid, cmd, path, active, free, pane_id, window_name, window_active, last_seen_at, stale_since)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)
		 ON CONFLICT(key) DO UPDATE SET session=excluded.session, window=excluded.window,
		   pane=excluded.pane, pid=excluded.pid, cmd=excluded.cmd, path=excluded.path,
		   active=excluded.active, free=excluded.free, pane_id=excluded.pane_id,
		   window_name=excluded.window_name, window_active=excluded.window_active,
		   last_seen_at=excluded.last_seen_at, stale_since=NULL`,
		r.Key, r.Session, r.Window, r.Pane, r.Pid, r.Cmd, r.Path, b2i(r.Active), b2i(r.Free),
		r.PaneID, r.WindowName, b2i(r.WindowActive), now.UTC().Format(rfc))
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

// livePaneRows returns every non-stale pane keyed by pane key — the prior read
// model the reconcile diff compares against to detect NEW panes, CHANGED panes
// (free/busy, cmd, path), and vanished panes in one pass.
func (s *store) livePaneRows(ctx context.Context) (map[string]PaneRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, session, window, pane, pid, cmd, path, active, free, pane_id, window_name, window_active, stale_since
		 FROM tmux_panes WHERE stale_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]PaneRow{}
	for rows.Next() {
		r, err := scanPane(rows)
		if err != nil {
			return nil, err
		}
		out[r.Key] = r
	}
	return out, rows.Err()
}

// scanPanes returns panes, optionally filtered by host (parsed from the key).
func (s *store) scanPanes(ctx context.Context, host string) ([]PaneRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, session, window, pane, pid, cmd, path, active, free, pane_id, window_name, window_active, stale_since FROM tmux_panes ORDER BY key`)
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
		`SELECT key, name, worktree, branch, attached, created_at, last_seen_at, stale_since FROM tmux_sessions ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SessionRow
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		if host == "" || r.HostID == host {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// liveSessionRows returns every non-stale session keyed by session key — the prior
// read model sessionChanged compares against to detect NEW and CHANGED sessions
// (attached/createdAt/worktree/branch) on a reconcile diff, mirroring livePaneRows.
func (s *store) liveSessionRows(ctx context.Context) (map[string]SessionRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, name, worktree, branch, attached, created_at, last_seen_at, stale_since
		 FROM tmux_sessions WHERE stale_since IS NULL`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]SessionRow{}
	for rows.Next() {
		r, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out[r.Key] = r
	}
	return out, rows.Err()
}

// scanSession reads one session row: attached/created_at are the #27 columns, NULL
// for a row written by the pre-#27 plugin (a NULL attached reads false).
func scanSession(rows *sql.Rows) (SessionRow, error) {
	var (
		r           SessionRow
		wt, br      sql.NullString
		created     sql.NullString
		attached    sql.NullInt64
		last, stale sql.NullString
	)
	if err := rows.Scan(&r.Key, &r.Name, &wt, &br, &attached, &created, &last, &stale); err != nil {
		return r, err
	}
	r.Worktree, r.Branch = wt.String, br.String
	r.Attached = attached.Int64 == 1
	r.CreatedAt = nsTime(created)
	_, r.HostID, _ = splitHost(r.Key)
	r.LastSeenAt, r.StaleSince = nsTime(last), nsTime(stale)
	return r, nil
}

// panesForBranch returns panes whose session worktree is on branch (the
// issue↔branch join). It joins pane.session to session.name.
func (s *store) panesForBranch(ctx context.Context, branch string) ([]PaneRow, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT p.key, p.session, p.window, p.pane, p.pid, p.cmd, p.path, p.active, p.free, p.pane_id, p.window_name, p.window_active, p.stale_since
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
		r               PaneRow
		active, free    int
		path            sql.NullString
		paneID, winName sql.NullString
		winActive       sql.NullInt64
		stale           sql.NullString
	)
	if err := rows.Scan(&r.Key, &r.Session, &r.Window, &r.Pane, &r.Pid, &r.Cmd, &path, &active, &free,
		&paneID, &winName, &winActive, &stale); err != nil {
		return r, err
	}
	r.Path, r.Active, r.Free = path.String, active == 1, free == 1
	r.PaneID, r.WindowName, r.WindowActive = paneID.String, winName.String, winActive.Int64 == 1
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

// nsFmt formats an optional time for a nullable column: nil ⇒ SQL NULL.
func nsFmt(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(rfc)
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
