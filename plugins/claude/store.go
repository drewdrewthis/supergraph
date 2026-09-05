package claude

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// store is the claude plugin's SQLite state over core.Store (which owns the events
// ring and cursors). It holds one table of session rows plus a fold-dedup set. There
// is deliberately NO prompt/response/tool_input column — privacy is enforced by the
// schema, not only by code (EDR §"SQLite state").
type store struct{ core *core.Store }

// SessionRow is one ClaudeSession. ClaudeInstance is the projection of rows whose
// Pane is non-empty (no second table). Empty/zero fields render as GraphQL null.
type SessionRow struct {
	HostID, SessionID, Cwd, GitBranch, Model, State, LastTool, Pane, PrURL string
	IssueNumber, ToolCalls, Pid, PrNumber                                  int
	StartedAt, LastEventAt                                                 time.Time
	StaleSince                                                             *time.Time
}

const rfc = time.RFC3339Nano

const claudeSchema = `
CREATE TABLE IF NOT EXISTS claude_sessions (
	sid           TEXT PRIMARY KEY,
	host_id       TEXT,
	cwd           TEXT,
	git_branch    TEXT,
	issue_number  INTEGER,
	model         TEXT,
	state         TEXT,
	last_tool     TEXT,
	tool_calls    INTEGER NOT NULL DEFAULT 0,
	pane          TEXT,
	pid           INTEGER,
	pr_number     INTEGER,
	pr_url        TEXT,
	started_at    TEXT,
	last_event_at TEXT,
	stale_since   TEXT
);
CREATE TABLE IF NOT EXISTS claude_folds (
	sid   TEXT,
	event TEXT,
	ts    TEXT,
	PRIMARY KEY (sid, event, ts)
);`

func (s *store) migrate(ctx context.Context) error {
	if _, err := s.core.DB().ExecContext(ctx, claudeSchema); err != nil {
		return fmt.Errorf("claude: migrate: %w", err)
	}
	return nil
}

func (s *store) db() *sql.DB { return s.core.DB() }

// cursor / setCursor delegate to core's cursors table: the tail's per-transcript
// byte offset lives there so it survives a restart (AC-CLAUDE-CURSOR).
func (s *store) cursor(ctx context.Context, name string) string {
	v, _ := s.core.Cursor(ctx, name)
	return v
}

func (s *store) setCursor(ctx context.Context, name, value string) error {
	return s.core.SetCursor(ctx, name, value)
}

const insertCols = `INSERT INTO claude_sessions
	(sid,host_id,cwd,git_branch,issue_number,model,state,last_tool,tool_calls,pane,pid,pr_number,pr_url,started_at,last_event_at,stale_since)
	VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// enrichSet is the COALESCE tail shared by both upserts: a zero/empty incoming field
// never clobbers a stored value, so enrichment adds fields without erasing state.
const enrichSet = `
	last_event_at = excluded.last_event_at,
	cwd          = CASE WHEN excluded.cwd <> ''         THEN excluded.cwd         ELSE claude_sessions.cwd END,
	git_branch   = CASE WHEN excluded.git_branch <> ''  THEN excluded.git_branch  ELSE claude_sessions.git_branch END,
	issue_number = CASE WHEN excluded.issue_number <> 0 THEN excluded.issue_number ELSE claude_sessions.issue_number END,
	model        = CASE WHEN excluded.model <> ''       THEN excluded.model       ELSE claude_sessions.model END,
	pane         = CASE WHEN excluded.pane <> ''        THEN excluded.pane        ELSE claude_sessions.pane END,
	pid          = CASE WHEN excluded.pid <> 0          THEN excluded.pid         ELSE claude_sessions.pid END,
	pr_number    = CASE WHEN excluded.pr_number <> 0    THEN excluded.pr_number   ELSE claude_sessions.pr_number END,
	pr_url       = CASE WHEN excluded.pr_url <> ''      THEN excluded.pr_url      ELSE claude_sessions.pr_url END`

// foldSQL overwrites state, adds tool_calls, and sets last_tool/stale_since when the
// event carries them; enrichSQL leaves state and tool_calls untouched (tail
// enrichment). Both create the row if absent.
const foldSQL = insertCols + ` ON CONFLICT(sid) DO UPDATE SET
	state       = excluded.state,
	tool_calls  = claude_sessions.tool_calls + excluded.tool_calls,
	last_tool   = CASE WHEN excluded.last_tool <> ''   THEN excluded.last_tool   ELSE claude_sessions.last_tool END,
	stale_since = CASE WHEN excluded.stale_since <> '' THEN excluded.stale_since ELSE claude_sessions.stale_since END,` + enrichSet

const enrichSQL = insertCols + ` ON CONFLICT(sid) DO UPDATE SET` + enrichSet

// applyFold dedups on (sid,event,ts) then folds the transition. It returns whether it
// applied (false = a duplicate already seen via the other channel, AC-CLAUDE-DEDUP).
func (s *store) applyFold(ctx context.Context, fi foldInput, host string, now time.Time) (bool, error) {
	res, err := s.db().ExecContext(ctx,
		`INSERT OR IGNORE INTO claude_folds (sid,event,ts) VALUES (?,?,?)`, fi.sid, fi.event, fi.ts)
	if err != nil {
		return false, fmt.Errorf("claude: dedup %s: %w", fi.sid, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	prev := s.stateOf(ctx, fi.sid)
	next, toolCall, ended := fold(prev, fi.event, fi.permissionAsk)
	toolInc, lastTool := 0, ""
	if toolCall {
		toolInc, lastTool = 1, fi.tool
	}
	stale := ""
	if ended {
		stale = now.Format(rfc)
	}
	e := fi.enrich
	_, err = s.db().ExecContext(ctx, foldSQL,
		fi.sid, host, e.cwd, e.gitBranch, e.issueNumber, e.model, string(next), lastTool, toolInc,
		fi.pane, fi.pid, e.prNumber, e.prURL, now.Format(rfc), now.Format(rfc), stale)
	if err != nil {
		return false, fmt.Errorf("claude: fold %s: %w", fi.sid, err)
	}
	return true, nil
}

// applyEnrichment adds structural fields (branch, model, PR, cwd) without touching
// state or tool_calls, creating a row (state "idle") if the session is new — this is
// how a hook-less transcript session is backfilled (AC-CLAUDE-TAIL-BACKFILL).
func (s *store) applyEnrichment(ctx context.Context, sid, host string, e enrichment, now time.Time) error {
	_, err := s.db().ExecContext(ctx, enrichSQL,
		sid, host, e.cwd, e.gitBranch, e.issueNumber, e.model, string(stateIdle), "", 0,
		"", 0, e.prNumber, e.prURL, now.Format(rfc), now.Format(rfc), "")
	if err != nil {
		return fmt.Errorf("claude: enrich %s: %w", sid, err)
	}
	return nil
}

func (s *store) stateOf(ctx context.Context, sid string) State {
	var st string
	_ = s.db().QueryRowContext(ctx, `SELECT state FROM claude_sessions WHERE sid=?`, sid).Scan(&st)
	return State(st)
}

const selectCols = `SELECT sid,host_id,cwd,git_branch,issue_number,model,state,last_tool,tool_calls,pane,pid,pr_number,pr_url,started_at,last_event_at,stale_since FROM claude_sessions`

func scanRow(sc interface{ Scan(...any) error }) (SessionRow, error) {
	var (
		r                 SessionRow
		started, last, st string
	)
	if err := sc.Scan(&r.SessionID, &r.HostID, &r.Cwd, &r.GitBranch, &r.IssueNumber, &r.Model,
		&r.State, &r.LastTool, &r.ToolCalls, &r.Pane, &r.Pid, &r.PrNumber, &r.PrURL,
		&started, &last, &st); err != nil {
		return r, err
	}
	r.StartedAt, _ = time.Parse(rfc, started)
	r.LastEventAt, _ = time.Parse(rfc, last)
	if st != "" {
		if t, err := time.Parse(rfc, st); err == nil {
			r.StaleSince = &t
		}
	}
	return r, nil
}

func (s *store) get(ctx context.Context, sid string) (*SessionRow, error) {
	row := s.db().QueryRowContext(ctx, selectCols+` WHERE sid=?`, sid)
	r, err := scanRow(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claude: get %s: %w", sid, err)
	}
	return &r, nil
}

// list returns sessions optionally filtered by host and/or issue number (the S4-half
// filter, AC-CLAUDE-S4-HALF).
func (s *store) list(ctx context.Context, host *string, issue *int) ([]SessionRow, error) {
	q, args := selectCols, []any{}
	where := ""
	if host != nil {
		where, args = " WHERE host_id=?", append(args, *host)
	}
	if issue != nil {
		if where == "" {
			where = " WHERE"
		} else {
			where += " AND"
		}
		where += " issue_number=?"
		args = append(args, *issue)
	}
	return s.queryRows(ctx, q+where+` ORDER BY sid`, args...)
}

// instances returns the rows with a non-empty pane (the ClaudeInstance projection).
func (s *store) instances(ctx context.Context, host *string) ([]SessionRow, error) {
	q := selectCols + ` WHERE pane <> ''`
	args := []any{}
	if host != nil {
		q += ` AND host_id=?`
		args = append(args, *host)
	}
	return s.queryRows(ctx, q+` ORDER BY pane`, args...)
}

func (s *store) queryRows(ctx context.Context, q string, args ...any) ([]SessionRow, error) {
	rows, err := s.db().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("claude: query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := []SessionRow{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// staleScan marks every live-pid session whose process is gone as stale (without a
// SessionEnd), returning the newly-stale sids so the caller can emit ended envelopes.
// It is the negative-control liveness path (AC-CLAUDE-STALE).
func (s *store) staleScan(ctx context.Context, now time.Time, alive func(pid int) bool) ([]string, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT sid,pid FROM claude_sessions WHERE pid <> 0 AND state <> 'ended' AND (stale_since IS NULL OR stale_since='')`)
	if err != nil {
		return nil, err
	}
	var dead []struct {
		sid string
		pid int
	}
	for rows.Next() {
		var sid string
		var pid int
		if err := rows.Scan(&sid, &pid); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !alive(pid) {
			dead = append(dead, struct {
				sid string
				pid int
			}{sid, pid})
		}
	}
	_ = rows.Close()
	var out []string
	for _, d := range dead {
		if _, err := s.db().ExecContext(ctx,
			`UPDATE claude_sessions SET stale_since=? WHERE sid=?`, now.Format(rfc), d.sid); err != nil {
			return nil, err
		}
		out = append(out, d.sid)
	}
	return out, nil
}
