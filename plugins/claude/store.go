package claude

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// store is the claude plugin's SQLite state over core.Store (which owns the events
// ring and cursors). It holds one table of session rows plus a fold-dedup set. There
// is deliberately NO tool_input or Notification-body column. `mission`/`last_response`
// DO hold rune-truncated prompt/response text, but only via the opt-in state-file
// channel — they stay NULL unless `stateDir` is set (EDR §"SQLite state", §Privacy).
type store struct{ core *core.Store }

// SessionRow is one ClaudeSession. ClaudeInstance is the projection of rows whose
// Pane is non-empty (no second table). Empty/zero fields render as GraphQL null.
type SessionRow struct {
	HostID, SessionID, Cwd, GitBranch, Model, State, LastTool, Pane, PrURL string
	// Mission, LastResponse and PaneTitle are set only by the state-file channel
	// (statefile.go, issue #28); they are empty on rows built from the hook/transcript
	// channels and render as GraphQL null. Mission/LastResponse are rune-truncated
	// prompt text (the plugin's only stored content, opt-in via stateDir).
	Mission, LastResponse, PaneTitle      string
	IssueNumber, ToolCalls, Pid, PrNumber int
	StartedAt, LastEventAt                time.Time
	StaleSince                            *time.Time
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
	// mission/last_response/pane_title are add-only nullable columns for the state-file
	// channel (issue #28). ADD COLUMN errors when the column already exists, which is
	// the idempotent-success case on a re-migrated db — tolerated, never fatal.
	for _, col := range []string{"mission", "last_response", "pane_title"} {
		if _, err := s.core.DB().ExecContext(ctx, "ALTER TABLE claude_sessions ADD COLUMN "+col+" TEXT"); err != nil &&
			!strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("claude: migrate add %s: %w", col, err)
		}
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
// NOTE: deliberately omits mission/last_response/pane_title — the state-file channel owns
// those, and naming them here would let a hook fold clobber file-set text (issue #28).
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

// stateFileInsert / stateFileUpdate write ONLY the state-file channel's columns. They
// never name tool_calls, so a folded row's counters survive; the update never lowers
// state for a folded session because applyStateFile keeps the stored state in that
// case (a fold is authoritative over the file).
// Every column is named explicitly with an empty/zero default. Omitting a column would
// leave it SQL NULL, and scanRow scans git_branch/last_tool/pr_url into plain strings —
// a NULL there makes the whole row unscannable and therefore invisible to every query.
const stateFileInsert = `INSERT INTO claude_sessions
	(sid,host_id,cwd,git_branch,issue_number,model,state,last_tool,tool_calls,pane,pid,pr_number,pr_url,started_at,last_event_at,stale_since,mission,last_response,pane_title)
	VALUES (?,?,?,'',0,'',?,'',0,?,?,0,'',?,?,'',?,?,?)`
const stateFileUpdate = `UPDATE claude_sessions
	SET cwd=?, state=?, pane=?, pid=?, last_event_at=?, mission=?, last_response=?, pane_title=? WHERE sid=?`

// applyStateFile reconciles one state file by sid. It COALESCEs the structural fields
// (an empty file field never clears a stored value) and applies the file's `state`
// ONLY for a session the fold path never touched (AC-CLAUDE-STATEFILE-RECONCILE): a
// hook/transcript-folded row keeps its state and counters. mission/lastResponse/
// paneTitle are set here and, because the fold SQL does not name those columns, a
// later fold never clears them. It returns whether the row actually changed, so a
// no-op re-scan emits nothing (AC-CLAUDE-STATEFILE-EMIT).
func (s *store) applyStateFile(ctx context.Context, sf stateFile, paneTitle, host string, now time.Time) (bool, error) {
	cur, err := s.get(ctx, sf.SID)
	if err != nil {
		return false, err
	}
	cwd, pane, state := "", "", string(stateIdle)
	mission, lastResp, title, pid := "", "", "", 0
	if cur != nil {
		cwd, pane, state = cur.Cwd, cur.Pane, cur.State
		mission, lastResp, title, pid = cur.Mission, cur.LastResponse, cur.PaneTitle, cur.Pid
	}
	setIf(&cwd, sf.Cwd)
	setIf(&pane, sf.Pane)
	setIf(&mission, truncRunes(sf.FirstPrompt, missionMaxRunes))
	setIf(&lastResp, truncRunes(sf.LastResponse, lastResponseMaxRunes))
	setIf(&title, paneTitle)
	if sf.Pid != 0 {
		pid = sf.Pid
	}
	if !s.hasFold(ctx, sf.SID) {
		setIf(&state, sf.State)
	}
	if cur != nil && cwd == cur.Cwd && pane == cur.Pane && state == cur.State &&
		mission == cur.Mission && lastResp == cur.LastResponse && title == cur.PaneTitle && pid == cur.Pid {
		return false, nil
	}
	ts := now.Format(rfc)
	if cur == nil {
		_, err = s.db().ExecContext(ctx, stateFileInsert, sf.SID, host, cwd, state, pane, pid, ts, ts, mission, lastResp, title)
	} else {
		_, err = s.db().ExecContext(ctx, stateFileUpdate, cwd, state, pane, pid, ts, mission, lastResp, title, sf.SID)
	}
	if err != nil {
		return false, fmt.Errorf("claude: statefile %s: %w", sf.SID, err)
	}
	return true, nil
}

// setIf overwrites *dst with v only when v is non-empty, the COALESCE rule the
// state-file upsert shares with enrichSet.
func setIf(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}

// hasFold reports whether the fold path (hook or transcript tool_use) has ever
// recorded a transition for sid — the signal that the session's state is
// hook-authoritative and the state file must not overwrite it.
func (s *store) hasFold(ctx context.Context, sid string) bool {
	var one int
	_ = s.db().QueryRowContext(ctx, `SELECT 1 FROM claude_folds WHERE sid=? LIMIT 1`, sid).Scan(&one)
	return one == 1
}

func (s *store) stateOf(ctx context.Context, sid string) State {
	var st string
	_ = s.db().QueryRowContext(ctx, `SELECT state FROM claude_sessions WHERE sid=?`, sid).Scan(&st)
	return State(st)
}

// selectCols COALESCEs every non-nullable column so a NULL left by any INSERT that
// omits a column can never make a row unscannable — and therefore invisible to every
// read. Only mission/last_response/pane_title are intentionally nullable (NullString).
const selectCols = `SELECT
	COALESCE(sid,'') AS sid, COALESCE(host_id,'') AS host_id, COALESCE(cwd,'') AS cwd,
	COALESCE(git_branch,'') AS git_branch, COALESCE(issue_number,0) AS issue_number,
	COALESCE(model,'') AS model, COALESCE(state,'') AS state, COALESCE(last_tool,'') AS last_tool,
	COALESCE(tool_calls,0) AS tool_calls, COALESCE(pane,'') AS pane, COALESCE(pid,0) AS pid,
	COALESCE(pr_number,0) AS pr_number, COALESCE(pr_url,'') AS pr_url,
	COALESCE(started_at,'') AS started_at, COALESCE(last_event_at,'') AS last_event_at,
	COALESCE(stale_since,'') AS stale_since, mission, last_response, pane_title
	FROM claude_sessions`

func scanRow(sc interface{ Scan(...any) error }) (SessionRow, error) {
	var (
		r                            SessionRow
		started, last, st            string
		mission, lastResp, paneTitle sql.NullString // NULL on rows never touched by the state-file channel
	)
	if err := sc.Scan(&r.SessionID, &r.HostID, &r.Cwd, &r.GitBranch, &r.IssueNumber, &r.Model,
		&r.State, &r.LastTool, &r.ToolCalls, &r.Pane, &r.Pid, &r.PrNumber, &r.PrURL,
		&started, &last, &st, &mission, &lastResp, &paneTitle); err != nil {
		return r, err
	}
	r.Mission, r.LastResponse, r.PaneTitle = mission.String, lastResp.String, paneTitle.String
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

// prune deletes sessions whose last event is older than cutoff and their folds, so an
// unbounded fleet history does not grow the db forever (config retentionDays). It
// parses each stored timestamp in Go rather than comparing RFC3339Nano strings in SQL,
// whose trimmed fractional seconds make lexical order unreliable.
func (s *store) prune(ctx context.Context, cutoff time.Time) error {
	rows, err := s.db().QueryContext(ctx, `SELECT sid,last_event_at FROM claude_sessions`)
	if err != nil {
		return fmt.Errorf("claude: prune scan: %w", err)
	}
	var old []string
	for rows.Next() {
		var sid, last string
		if err := rows.Scan(&sid, &last); err != nil {
			_ = rows.Close()
			return err
		}
		if t, err := time.Parse(rfc, last); err == nil && t.Before(cutoff) {
			old = append(old, sid)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, sid := range old {
		if _, err := s.db().ExecContext(ctx, `DELETE FROM claude_sessions WHERE sid=?`, sid); err != nil {
			return fmt.Errorf("claude: prune session %s: %w", sid, err)
		}
		if _, err := s.db().ExecContext(ctx, `DELETE FROM claude_folds WHERE sid=?`, sid); err != nil {
			return fmt.Errorf("claude: prune folds %s: %w", sid, err)
		}
	}
	return nil
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
