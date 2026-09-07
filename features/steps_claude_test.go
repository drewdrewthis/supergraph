package features

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// registerClaudeSteps wires every @claude scenario step (features/claude.feature).
// All @local scenarios drive the plugin black-box: synthetic hook POSTs to
// /plugins/claude/hook, synthetic JSONL under a temp projectsDir, and GraphQL/db
// readback — no live Claude session.
func registerClaudeSteps(sc *godog.ScenarioContext, w *world) {
	sc.Step(`^a \x60(SessionStart|UserPromptSubmit|PreToolUse|PostToolUse|Notification|Stop|SessionEnd)\x60 hook payload for session \x60([A-Za-z0-9]+)\x60 is POSTed to \x60/plugins/claude/hook\x60$`, w.postHookEvent)
	sc.Step(`^a \x60(SessionStart|UserPromptSubmit|PreToolUse|PostToolUse|Notification|Stop|SessionEnd)\x60 hook payload for \x60([A-Za-z0-9]+)\x60 is POSTed$`, w.postHookEvent)
	sc.Step(`^a \x60PreToolUse\x60 hook payload for \x60([A-Za-z0-9]+)\x60 with \x60tool_name\x60 "([A-Za-z0-9]+)" is POSTed$`, w.postHookTool)
	sc.Step(`^a \x60Notification\x60 hook payload for \x60([A-Za-z0-9]+)\x60 with a permission message is POSTed$`, w.postHookPermission)
	sc.Step(`^a \x60Notification\x60 hook payload for \x60([A-Za-z0-9]+)\x60 with message "([^"]*)" is POSTed$`, w.postHookNag)
	sc.Step(`^a \x60PreToolUse\x60 hook payload for session \x60([A-Za-z0-9]+)\x60 is POSTed with \x60TMUX_PANE\x60 "([^"]*)" in its environment$`, w.postHookPane)
	sc.Step(`^\x60claudeSession\(sessionId: "([A-Za-z0-9]+)"\)\x60 has state "([a-z]+)"$`, w.assertState)
	sc.Step(`^\x60claudeSession\(sessionId: "([A-Za-z0-9]+)"\)\x60 is not in state "([a-z]+)"$`, w.assertNotState)
	sc.Step(`^\x60claudeSession\(sessionId: "([A-Za-z0-9]+)"\)\x60 has state "([a-z]+)" and \x60lastTool\x60 "([A-Za-z0-9]+)" and \x60toolCalls\x60 ([0-9]+)$`, w.assertStateToolCalls)
	sc.Step(lit("a supergraph server started with the claude plugin and data dir <tmp>"), w.startDefaultServer)
	sc.Step(lit("a supergraph server started with the claude plugin, a second \"fakeok\" plugin that never panics, and data dir <tmp>"), w.startDefaultServer)
	sc.Step(lit("a supergraph server on a non-loopback listen with a bearer token configured and the claude plugin, data dir <tmp>"), w.startClaudeAuthServer)
	sc.Step(lit("I induce a panic in the claude plugin's Start via its panic-inject path"), w.claudeInduce)
	sc.Step(lit("the \"claude\" entry in \x60/health\x60 has state \"stale\""), w.claudeStale)
	sc.Step(lit("session \x60S2\x60 is in state \"working\""), w.s2Working)
	sc.Step(lit("the stored row carries no copy of the notification message body"), w.noNotificationBody)
	sc.Step(lit("a session on git branch \x60issue42/spike\x60 has been folded from hook events"), w.s4IssueFolded)
	sc.Step(lit("I query \x60claudeSessions\x60 filtered by issue number 42"), w.queryIssue42)
	sc.Step(lit("at least one \x60ClaudeSession\x60 row is returned, source-tagged \"claude\", with \x60issueNumber\x60 42"), w.assertIssue42Row)
	sc.Step(lit("I query \x60claudeSessions\x60 filtered by issue number 99 which has no session"), w.queryIssue99)
	sc.Step(lit("no claude row is returned for issue 99, so the umbrella \x60touching\x60 query supplies the absent-source \x60staleSince\x60 marker (prd.feature @S4, still pending)"), w.assertNoIssue99Row)
	sc.Step(lit("a \x60ClaudeInstance\x60 exists with \x60pane\x60 \"%23\" and a non-zero \x60pid\x60 whose \x60session\x60 is \x60S3\x60"), w.assertInstanceS3)
	sc.Step(lit("a \x60claude.instance.updated\x60 envelope is emitted with pane \"%23\""), w.assertInstanceEnvelope)
	sc.Step(lit("the instance's \x60pane\x60 \"%23\" is the join key a \x60TmuxPane\x60 row keys on"), w.assertPaneJoinKey)
	sc.Step(lit("a synthetic transcript for session \x60S4\x60 on git branch \x60issue7/foo\x60 with a \x60pr-link\x60 record for PR 12"), w.transcriptS4)
	sc.Step(lit("the transcript tail folds session \x60S4\x60"), w.waitFoldS4)
	sc.Step(lit("\x60claudeSession(sessionId: \"S4\")\x60 has \x60issueNumber\x60 7 and \x60prNumber\x60 12 and \x60prUrl\x60 ending \"/pull/12\""), w.assertS4Join)
	sc.Step(lit("a \x60SessionStart\x60 hook payload for session \x60A1\x60 is POSTed to \x60/plugins/claude/hook\x60 with no Authorization header"), w.postA1NoAuth)
	sc.Step(lit("a \x60SessionStart\x60 hook payload for session \x60A1\x60 is POSTed to \x60/plugins/claude/hook\x60 with a wrong Bearer token"), w.postA1WrongAuth)
	sc.Step(lit("a \x60SessionStart\x60 hook payload for session \x60A1\x60 is POSTed to \x60/plugins/claude/hook\x60 with the correct Bearer token"), w.postA1GoodAuth)
	sc.Step(lit("the hook response status is 401"), w.status401)
	sc.Step(lit("no \x60claudeSession(sessionId: \"A1\")\x60 row exists and no claude envelope was emitted for \x60A1\x60"), w.noA1RowNoEnvelope)
	sc.Step(lit("the response status is 200 and \x60claudeSession(sessionId: \"A1\")\x60 has state \"idle\""), w.a1Idle200)
	sc.Step(lit("a hook payload with an unknown \x60hook_event_name\x60 \"Bogus\" for session \x60A2\x60 is POSTed to \x60/plugins/claude/hook\x60"), w.postA2Bogus)
	sc.Step(lit("a hook payload with a non-numeric \x60issue_number\x60 \"not-a-number\" for session \x60A2\x60 is POSTed to \x60/plugins/claude/hook\x60"), w.postA2BadIssue)
	sc.Step(lit("the response status is 400"), w.status400)
	sc.Step(lit("no \x60claudeSession(sessionId: \"A2\")\x60 row exists"), w.noA2Row)
	sc.Step(lit("a synthetic transcript for session \x60S5\x60 exists under the projects dir with no hook ever POSTed"), w.transcriptS5)
	sc.Step(lit("the transcript tail scans the projects dir once"), w.waitFoldS5)
	sc.Step(lit("\x60claudeSession(sessionId: \"S5\")\x60 exists, folded from the transcript alone"), w.assertS5Exists)
	sc.Step(lit("session \x60S6\x60 was folded from a hook payload that carried no gitBranch, model, or PR"), w.s6HookOnly)
	sc.Step(lit("the transcript tail folds \x60S6\x60 from a transcript carrying gitBranch \x60plugin/claude\x60, model \x60claude-fable-5-1\x60, and a \x60pr-link\x60 for PR 8"), w.transcriptS6Enrich)
	sc.Step(lit("\x60claudeSession(sessionId: \"S6\")\x60 now has \x60gitBranch\x60 \"plugin/claude\", \x60model\x60 \"claude-fable-5-1\", and \x60prNumber\x60 8"), w.assertS6Enriched)
	sc.Step(lit("a \x60PreToolUse\x60 transition for session \x60S7\x60 has been folded from a hook payload"), w.s7HookPreTool)
	sc.Step(lit("the transcript tail later observes the same \x60PreToolUse\x60 transition for \x60S7\x60"), w.s7TailSameTransition)
	sc.Step(lit("exactly one \x60claudeSession(sessionId: \"S7\")\x60 row exists"), w.assertS7One)
	sc.Step(lit("its \x60toolCalls\x60 count is 1, not 2"), w.assertS7ToolCalls1)
	sc.Step(lit("the tail has read session \x60S8\x60's transcript up to byte offset T"), w.s8Read)
	sc.Step(lit("the claude server is stopped and restarted on the same data dir <tmp>"), w.restartServe)
	sc.Step(lit("the \x60tail:\x60 cursor for \x60S8\x60's transcript is still T after the restart"), w.assertS8Cursor)
	sc.Step(lit("the next scan resumes from offset T and does not re-fold records before T"), w.assertS8NoRefold)
	sc.Step(lit("session \x60S9\x60 is folded with a \x60pid\x60 for a process that is not running, and no \x60SessionEnd\x60 arrived"), w.s9DeadPid)
	sc.Step(lit("the pid-liveness sweep runs once"), w.waitSweepS9)
	sc.Step(lit("\x60claudeSession(sessionId: \"S9\")\x60 has a non-null \x60staleSince\x60"), w.assertS9Stale)
	sc.Step(lit("the row is still present, marked stale, never deleted"), w.assertS9Present)
	sc.Step(lit("an open \x60claudeSessionUpdated\x60 subscription capturing every claude envelope"), w.openClaudeSub)
	sc.Step(lit("session \x60S10\x60 is folded from a \x60UserPromptSubmit\x60 with prompt \"SECRET-PROMPT-STRING\", a \x60PreToolUse\x60 with \x60tool_input\x60 containing \"SECRET-CMD-STRING\", and a transcript assistant text \"SECRET-RESPONSE-STRING\""), w.foldS10Secrets)
	sc.Step(lit("\x60claudeSession(sessionId: \"S10\")\x60 has \x60lastTool\x60 and \x60toolCalls\x60 set"), w.assertS10Set)
	sc.Step(lit("a raw dump of \x60<tmp>/claude.db\x60 contains none of \"SECRET-PROMPT-STRING\", \"SECRET-CMD-STRING\", or \"SECRET-RESPONSE-STRING\""), w.assertDBNoSecrets)
	sc.Step(lit("every envelope pushed to the subscription contains none of \"SECRET-PROMPT-STRING\", \"SECRET-CMD-STRING\", or \"SECRET-RESPONSE-STRING\""), w.assertSubNoSecrets)
	sc.Step(lit("the \x60claudeSession(sessionId: \"S10\")\x60 GraphQL result contains none of \"SECRET-PROMPT-STRING\", \"SECRET-CMD-STRING\", or \"SECRET-RESPONSE-STRING\""), w.assertGQLNoSecrets)
	sc.Step(lit("a temp settings file at <settingsPath> already containing an unrelated \x60PreToolUse\x60 hook"), w.settingsWithUnrelated)
	sc.Step(lit("\x60supergraph install\x60 runs with no \x60--install-hook\x60 flag"), w.installPrint)
	sc.Step(lit("it prints a hook JSON block naming a \x60supergraph claude-hook\x60 command for each of the 7 lifecycle events"), w.assertPrintedBlock)
	sc.Step(lit("the temp settings file at <settingsPath> is left unchanged"), w.assertSettingsUnchanged)
	sc.Step(lit("\x60supergraph install --install-hook --settings <settingsPath>\x60 merges the claude hook block"), w.installMerge)
	sc.Step(lit("each of the 7 lifecycle events in <settingsPath> wires a \x60supergraph claude-hook\x60 command"), w.assertAllEventsWired)
	sc.Step(lit("the pre-existing unrelated \x60PreToolUse\x60 hook is still present"), w.assertUnrelatedPresent)
	sc.Step(lit("\x60supergraph install --install-hook --settings <settingsPath>\x60 runs a second time"), w.installMerge)
	sc.Step(lit("no duplicate \x60supergraph claude-hook\x60 entry is added to any event array"), w.assertNoDuplicate)
	sc.Step(lit("the claude plugin source under \x60plugins/claude\x60"), noop)
	sc.Step(lit("\x60make loc-claude\x60 counts non-comment, non-blank prod lines excluding tests"), w.runLocClaude)
	sc.Step(lit("the count is 750 or fewer"), w.assertLocOK)
	sc.Step(lit("the claude plugin package and its blank import in graph/plugins_import.go"), noop)
	sc.Step(lit("\x60git diff --stat core/\x60 is run after the claude plugin compiles in"), w.runGitDiffCore)
	sc.Step(lit("it reports 0 core files changed"), w.assertNoCoreDiff)
	sc.Step(lit("20 synthetic \x60PreToolUse\x60 hook payloads for distinct sessions are POSTed to \x60/plugins/claude/hook\x60"), w.f1Post20)
	sc.Step(lit("each session is queryable and the p95 of POST-to-queryable over the 20 samples is under 1s"), w.f1AssertP95)

	// ---------- @live @pending (AC-CLAUDE-PANE) ----------
	// Honest @pending, matching the github/peer live convention: strict mode
	// requires every step TEXT to resolve even though the scenario is excluded from
	// the default run. Driving a real Claude Code tool-call inside a managed tmux
	// pane end-to-end (hook install + pane->pid->session + TmuxPane join) needs a
	// live-session harness that is not built; see README > Live scenarios.
	for _, s := range []string{
		"the claude plugin is running with its hook installed on a box running tmux",
		"a real Claude Code session runs a tool call inside a tmux pane",
		"a `ClaudeInstance` appears with that session's real `pane` and live `pid`",
		"it joins to the tmux plugin's `TmuxPane` row for the same pane",
	} {
		sc.Step(lit(s), pendingStep)
	}
}

// ---- HTTP + GraphQL helpers ----

func (w *world) hookURL() string { return "http://" + w.listen + "/plugins/claude/hook" }

func (w *world) postHookAuth(payload map[string]any, auth string) int {
	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest(http.MethodPost, w.hookURL(), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.lastStatus = -1
		return -1
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	w.lastStatus = resp.StatusCode
	return resp.StatusCode
}

// postHook POSTs a payload with the box's own token (when configured) as the caller.
func (w *world) postHook(sid, event string, extra map[string]any) int {
	p := map[string]any{"session_id": sid, "hook_event_name": event}
	for k, v := range extra {
		p[k] = v
	}
	auth := ""
	if w.token != "" {
		auth = "Bearer " + w.token
	}
	return w.postHookAuth(p, auth)
}

func (w *world) gql(query string) (string, map[string]any, error) {
	body, _ := json.Marshal(map[string]any{"query": query})
	req, _ := http.NewRequest(http.MethodPost, "http://"+w.listen+"/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data   map[string]any `json:"data"`
		Errors []any          `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return string(raw), nil, fmt.Errorf("gql decode: %s", raw)
	}
	if len(out.Errors) > 0 {
		return string(raw), nil, fmt.Errorf("gql errors: %v", out.Errors)
	}
	return string(raw), out.Data, nil
}

const sessionFields = `{ sessionId state lastTool toolCalls issueNumber prNumber prUrl gitBranch model staleSince }`

func (w *world) session(sid string) map[string]any {
	_, d, err := w.gql(fmt.Sprintf(`{ claudeSession(sessionId:%q)%s }`, sid, sessionFields))
	if err != nil || d == nil {
		return nil
	}
	s, _ := d["claudeSession"].(map[string]any)
	return s
}

func (w *world) waitSession(sid string, pred func(map[string]any) bool, timeout time.Duration) map[string]any {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := w.session(sid); s != nil && pred(s) {
			return s
		}
		time.Sleep(100 * time.Millisecond)
	}
	return w.session(sid)
}

func numField(m map[string]any, k string) (float64, bool) {
	if m == nil {
		return 0, false
	}
	f, ok := m[k].(float64)
	return f, ok
}

// ---- claude.db direct readers ----

func (w *world) openClaudeDB() (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.Join(w.dataDir, "claude.db")+"?_pragma=busy_timeout%285000%29")
}

func (w *world) claudeEventsMatching(sid string) int {
	db, err := w.openClaudeDB()
	if err != nil {
		return -1
	}
	defer func() { _ = db.Close() }()
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM events WHERE source='claude' AND (key LIKE ? OR payload LIKE ?)`,
		"%"+sid+"%", "%"+sid+"%").Scan(&n)
	return n
}

func (w *world) claudeEventPayloads(typ string) []string {
	db, err := w.openClaudeDB()
	if err != nil {
		return nil
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT payload FROM events WHERE type=?`, typ)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			out = append(out, p)
		}
	}
	return out
}

// ---- transcript writer ----

func (w *world) writeTranscript(sid string, records []map[string]any) string {
	dir := filepath.Join(w.claudeProjectsDir, "proj")
	_ = os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, sid+".jsonl")
	var b strings.Builder
	for _, r := range records {
		j, _ := json.Marshal(r)
		b.Write(j)
		b.WriteByte('\n')
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
	return path
}

// ---- server start variants ----

func (w *world) startClaudeAuthServer() error {
	if err := w.writeConfig(cfgOpts{lag: 30, templateInterval: 1, nonLoopback: true}); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) claudeInduce() error {
	w.stopServe()
	if err := w.writeConfig(cfgOpts{lag: 30, templateInterval: 1, claudePanic: true}); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) claudeStale() error {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, err := w.getHealth()
		if err == nil {
			if r := findRow(rows, "claude"); r != nil && r["state"] == "stale" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("claude did not reach stale after induced panic")
}

func (w *world) restartServe() error {
	w.stopServe()
	return w.startServe()
}

// ---- hook post handlers ----

func (w *world) postHookEvent(event, sid string) error {
	w.postHook(sid, event, nil)
	return nil
}

func (w *world) postHookTool(sid, tool string) error {
	w.postHook(sid, "PreToolUse", map[string]any{"tool_name": tool})
	return nil
}

func (w *world) postHookPermission(sid string) error {
	w.postHook(sid, "Notification", map[string]any{"message": "Claude needs your permission to run Bash"})
	return nil
}

func (w *world) postHookNag(sid, msg string) error {
	w.postHook(sid, "Notification", map[string]any{"message": msg})
	return nil
}

func (w *world) postHookPane(sid, pane string) error {
	w.postHook(sid, "PreToolUse", map[string]any{"tmux_pane": pane, "pid": os.Getpid(), "tool_name": "Bash"})
	return nil
}

// ---- state assertions ----

func (w *world) assertState(sid, state string) error {
	s := w.waitSession(sid, func(m map[string]any) bool { return m["state"] == state }, 3*time.Second)
	if s == nil || s["state"] != state {
		return fmt.Errorf("session %s state=%v, want %q", sid, s, state)
	}
	return nil
}

func (w *world) assertNotState(sid, state string) error {
	s := w.session(sid)
	if s != nil && s["state"] == state {
		return fmt.Errorf("session %s unexpectedly in state %q", sid, state)
	}
	return nil
}

func (w *world) assertStateToolCalls(sid, state, tool string, calls int) error {
	s := w.waitSession(sid, func(m map[string]any) bool {
		tc, _ := numField(m, "toolCalls")
		return m["state"] == state && m["lastTool"] == tool && int(tc) == calls
	}, 3*time.Second)
	if s == nil {
		return fmt.Errorf("session %s missing", sid)
	}
	tc, _ := numField(s, "toolCalls")
	if s["state"] != state || s["lastTool"] != tool || int(tc) != calls {
		return fmt.Errorf("session %s = state=%v lastTool=%v toolCalls=%v; want %q/%q/%d", sid, s["state"], s["lastTool"], s["toolCalls"], state, tool, calls)
	}
	return nil
}

// ---- STATE-MACHINE ----

func (w *world) s2Working() error {
	w.postHook("S2", "SessionStart", nil)
	w.postHook("S2", "UserPromptSubmit", nil)
	return w.assertState("S2", "working")
}

func (w *world) noNotificationBody() error {
	data, err := os.ReadFile(filepath.Join(w.dataDir, "claude.db"))
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte("permission to run Bash")) {
		return fmt.Errorf("notification message body leaked into claude.db")
	}
	return nil
}

// ---- S4 half ----

func (w *world) s4IssueFolded() error {
	w.postHook("S4ISSUE", "SessionStart", map[string]any{"gitBranch": "issue42/spike"})
	return nil
}

func (w *world) queryIssue42() error { return w.queryIssue(42) }
func (w *world) queryIssue99() error { return w.queryIssue(99) }

func (w *world) queryIssue(n int) error {
	_, d, err := w.gql(fmt.Sprintf(`{ claudeSessions(issueNumber:%d){ sessionId issueNumber } }`, n))
	if err != nil {
		return err
	}
	rows, _ := d["claudeSessions"].([]any)
	w.lastSessions = nil
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok {
			w.lastSessions = append(w.lastSessions, m)
		}
	}
	return nil
}

func (w *world) assertIssue42Row() error {
	if len(w.lastSessions) == 0 {
		return fmt.Errorf("no claude row for issue 42")
	}
	n, _ := numField(w.lastSessions[0], "issueNumber")
	if int(n) != 42 {
		return fmt.Errorf("issueNumber=%v, want 42", w.lastSessions[0]["issueNumber"])
	}
	return nil
}

func (w *world) assertNoIssue99Row() error {
	if len(w.lastSessions) != 0 {
		return fmt.Errorf("unexpected claude rows for issue 99: %v", w.lastSessions)
	}
	return nil
}

// ---- PANE ----

func (w *world) instances() []map[string]any {
	_, d, err := w.gql(`{ claudeInstances{ pane pid session{ sessionId } } }`)
	if err != nil || d == nil {
		return nil
	}
	raw, _ := d["claudeInstances"].([]any)
	var out []map[string]any
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func (w *world) assertInstanceS3() error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, in := range w.instances() {
			if in["pane"] != "%23" {
				continue
			}
			pid, _ := numField(in, "pid")
			sess, _ := in["session"].(map[string]any)
			if pid != 0 && sess != nil && sess["sessionId"] == "S3" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no ClaudeInstance pane=%%23 pid!=0 session=S3; got %v", w.instances())
}

func (w *world) assertInstanceEnvelope() error {
	for _, p := range w.claudeEventPayloads("claude.instance.updated") {
		if strings.Contains(p, `"pane":"%23"`) {
			return nil
		}
	}
	return fmt.Errorf("no claude.instance.updated envelope with pane %%23")
}

func (w *world) assertPaneJoinKey() error {
	// The instance's pane is the exact join key a TmuxPane row keys on: assert the
	// stored instance still exposes pane "%23" verbatim (tmux plugin keys on pane).
	for _, in := range w.instances() {
		if in["pane"] == "%23" {
			return nil
		}
	}
	return fmt.Errorf("pane %%23 not present as a join key")
}

// ---- ISSUE-JOIN ----

func (w *world) transcriptS4() error {
	w.writeTranscript("S4", []map[string]any{
		{"sessionId": "S4", "gitBranch": "issue7/foo", "cwd": "/w", "timestamp": "2026-09-05T01:00:00Z",
			"prNumber": 12, "prUrl": "https://github.com/o/r/pull/12"},
	})
	return nil
}

func (w *world) waitFoldS4() error {
	s := w.waitSession("S4", func(m map[string]any) bool { n, _ := numField(m, "issueNumber"); return int(n) == 7 }, 5*time.Second)
	if s == nil {
		return fmt.Errorf("S4 not folded by tail")
	}
	return nil
}

func (w *world) assertS4Join() error {
	s := w.session("S4")
	if s == nil {
		return fmt.Errorf("S4 missing")
	}
	iss, _ := numField(s, "issueNumber")
	pr, _ := numField(s, "prNumber")
	url, _ := s["prUrl"].(string)
	if int(iss) != 7 || int(pr) != 12 || !strings.HasSuffix(url, "/pull/12") {
		return fmt.Errorf("S4 join = issue %v pr %v url %q", s["issueNumber"], s["prNumber"], url)
	}
	return nil
}

// ---- HOOK-AUTH ----

func (w *world) postA1NoAuth() error {
	w.postHookAuth(map[string]any{"session_id": "A1", "hook_event_name": "SessionStart"}, "")
	return nil
}

func (w *world) postA1WrongAuth() error {
	w.postHookAuth(map[string]any{"session_id": "A1", "hook_event_name": "SessionStart"}, "Bearer wrong-token")
	return nil
}

func (w *world) postA1GoodAuth() error {
	w.postHookAuth(map[string]any{"session_id": "A1", "hook_event_name": "SessionStart"}, "Bearer "+w.token)
	return nil
}

func (w *world) status401() error { return w.assertStatus(401) }
func (w *world) status400() error { return w.assertStatus(400) }

func (w *world) assertStatus(code int) error {
	if w.lastStatus != code {
		return fmt.Errorf("status=%d, want %d", w.lastStatus, code)
	}
	return nil
}

func (w *world) noA1RowNoEnvelope() error {
	if s := w.session("A1"); s != nil {
		return fmt.Errorf("A1 row unexpectedly exists: %v", s)
	}
	if n := w.claudeEventsMatching("A1"); n != 0 {
		return fmt.Errorf("A1 envelopes unexpectedly emitted: %d", n)
	}
	return nil
}

func (w *world) a1Idle200() error {
	if err := w.assertStatus(200); err != nil {
		return err
	}
	return w.assertState("A1", "idle")
}

// ---- HOOK-SCHEMA ----

func (w *world) postA2Bogus() error {
	auth := ""
	if w.token != "" {
		auth = "Bearer " + w.token
	}
	w.postHookAuth(map[string]any{"session_id": "A2", "hook_event_name": "Bogus"}, auth)
	return nil
}

func (w *world) postA2BadIssue() error {
	auth := ""
	if w.token != "" {
		auth = "Bearer " + w.token
	}
	w.postHookAuth(map[string]any{"session_id": "A2", "hook_event_name": "SessionStart", "issue_number": "not-a-number"}, auth)
	return nil
}

func (w *world) noA2Row() error {
	if s := w.session("A2"); s != nil {
		return fmt.Errorf("A2 row unexpectedly exists: %v", s)
	}
	return nil
}

// ---- BACKFILL ----

func (w *world) transcriptS5() error {
	w.writeTranscript("S5", []map[string]any{
		{"sessionId": "S5", "gitBranch": "issue3/x", "cwd": "/w", "timestamp": "2026-09-05T02:00:00Z"},
	})
	return nil
}

func (w *world) waitFoldS5() error {
	if w.waitSession("S5", func(m map[string]any) bool { return m["sessionId"] == "S5" }, 5*time.Second) == nil {
		return fmt.Errorf("S5 not discovered by tail")
	}
	return nil
}

func (w *world) assertS5Exists() error {
	if w.session("S5") == nil {
		return fmt.Errorf("S5 not present")
	}
	return nil
}

// ---- ENRICH ----

func (w *world) s6HookOnly() error {
	w.postHook("S6", "SessionStart", nil)
	return nil
}

func (w *world) transcriptS6Enrich() error {
	w.writeTranscript("S6", []map[string]any{
		{"sessionId": "S6", "gitBranch": "plugin/claude", "cwd": "/w", "timestamp": "2026-09-05T03:00:00Z",
			"message": map[string]any{"model": "claude-fable-5-1"}},
		{"sessionId": "S6", "timestamp": "2026-09-05T03:00:01Z", "prNumber": 8, "prUrl": "https://github.com/o/r/pull/8"},
	})
	return nil
}

func (w *world) assertS6Enriched() error {
	s := w.waitSession("S6", func(m map[string]any) bool {
		pr, _ := numField(m, "prNumber")
		return m["gitBranch"] == "plugin/claude" && int(pr) == 8
	}, 5*time.Second)
	if s == nil {
		return fmt.Errorf("S6 not enriched")
	}
	pr, _ := numField(s, "prNumber")
	if s["gitBranch"] != "plugin/claude" || s["model"] != "claude-fable-5-1" || int(pr) != 8 {
		return fmt.Errorf("S6 = branch %v model %v pr %v", s["gitBranch"], s["model"], s["prNumber"])
	}
	return nil
}

// ---- DEDUP ----

func (w *world) s7HookPreTool() error {
	w.dedupTS = "2026-09-05T04:00:00Z"
	w.postHook("S7", "PreToolUse", map[string]any{"tool_name": "Bash", "ts": w.dedupTS})
	return w.assertState("S7", "working")
}

func (w *world) s7TailSameTransition() error {
	w.writeTranscript("S7", []map[string]any{
		{"sessionId": "S7", "timestamp": w.dedupTS, "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Bash"},
		}}},
	})
	time.Sleep(2500 * time.Millisecond) // allow >=2 tail scans to observe the duplicate
	return nil
}

func (w *world) assertS7One() error {
	if w.session("S7") == nil {
		return fmt.Errorf("S7 missing")
	}
	return nil
}

func (w *world) assertS7ToolCalls1() error {
	s := w.session("S7")
	tc, _ := numField(s, "toolCalls")
	if int(tc) != 1 {
		return fmt.Errorf("S7 toolCalls=%v, want 1 (dedup failed)", s["toolCalls"])
	}
	return nil
}

// ---- CURSOR ----

func (w *world) s8Read() error {
	w.s8Path = w.writeTranscript("S8", []map[string]any{
		{"sessionId": "S8", "timestamp": "2026-09-05T05:00:00Z", "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "name": "Bash"},
		}}},
	})
	if w.waitSession("S8", func(m map[string]any) bool { tc, _ := numField(m, "toolCalls"); return int(tc) == 1 }, 5*time.Second) == nil {
		return fmt.Errorf("S8 not folded")
	}
	fi, err := os.Stat(w.s8Path)
	if err != nil {
		return err
	}
	w.s8Offset = fi.Size()
	// wait until the cursor has been written to the persisted offset
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.readTailCursor() == w.s8Offset {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("tail cursor never reached offset %d (got %d)", w.s8Offset, w.readTailCursor())
}

func (w *world) readTailCursor() int64 {
	db, err := w.openClaudeDB()
	if err != nil {
		return -1
	}
	defer func() { _ = db.Close() }()
	var v string
	_ = db.QueryRow(`SELECT value FROM cursors WHERE name=?`, "tail:"+w.s8Path).Scan(&v)
	var n int64
	_, _ = fmt.Sscan(v, &n)
	return n
}

func (w *world) assertS8Cursor() error {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if w.readTailCursor() == w.s8Offset {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("tail cursor after restart = %d, want %d", w.readTailCursor(), w.s8Offset)
}

func (w *world) assertS8NoRefold() error {
	time.Sleep(1500 * time.Millisecond) // let at least one post-restart scan run
	s := w.session("S8")
	tc, _ := numField(s, "toolCalls")
	if int(tc) != 1 {
		return fmt.Errorf("S8 toolCalls=%v after restart, want 1 (records re-folded before offset T)", s["toolCalls"])
	}
	return nil
}

// ---- STALE ----

func (w *world) s9DeadPid() error {
	w.postHook("S9", "SessionStart", map[string]any{"pid": 2000000000}) // a pid no process holds
	return nil
}

func (w *world) waitSweepS9() error {
	if w.waitSession("S9", func(m map[string]any) bool { return m["staleSince"] != nil }, 5*time.Second) == nil {
		return fmt.Errorf("S9 never marked stale by the sweep")
	}
	return nil
}

func (w *world) assertS9Stale() error {
	s := w.session("S9")
	if s == nil || s["staleSince"] == nil {
		return fmt.Errorf("S9 staleSince not set: %v", s)
	}
	return nil
}

func (w *world) assertS9Present() error {
	s := w.session("S9")
	if s == nil {
		return fmt.Errorf("S9 was deleted, should be kept marked stale")
	}
	return nil
}

// ---- PRIVACY ----

func (w *world) openClaudeSub() error { return w.openSubscription("claudeSessionUpdated") }

func (w *world) foldS10Secrets() error {
	w.postHook("S10", "UserPromptSubmit", map[string]any{"prompt": "SECRET-PROMPT-STRING"})
	w.postHook("S10", "PreToolUse", map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "SECRET-CMD-STRING"}})
	w.writeTranscript("S10", []map[string]any{
		{"sessionId": "S10", "timestamp": "2026-09-05T06:00:00Z", "message": map[string]any{"content": "assistant said SECRET-RESPONSE-STRING"}},
	})
	if w.waitSession("S10", func(m map[string]any) bool { tc, _ := numField(m, "toolCalls"); return int(tc) >= 1 }, 5*time.Second) == nil {
		return fmt.Errorf("S10 not folded")
	}
	w.claudeFrames = w.drainClaudeFrames(2 * time.Second)
	return nil
}

func (w *world) drainClaudeFrames(total time.Duration) []string {
	deadline := time.Now().Add(total)
	var frames []string
	for time.Now().Before(deadline) {
		m, err := w.ws.nextPush(400 * time.Millisecond)
		if err != nil {
			continue
		}
		b, _ := json.Marshal(m)
		frames = append(frames, string(b))
	}
	return frames
}

var secrets = []string{"SECRET-PROMPT-STRING", "SECRET-CMD-STRING", "SECRET-RESPONSE-STRING"}

func (w *world) assertS10Set() error {
	s := w.session("S10")
	tc, _ := numField(s, "toolCalls")
	if s == nil || s["lastTool"] == nil || int(tc) < 1 {
		return fmt.Errorf("S10 lastTool/toolCalls not set: %v", s)
	}
	return nil
}

func (w *world) assertDBNoSecrets() error {
	data, err := os.ReadFile(filepath.Join(w.dataDir, "claude.db"))
	if err != nil {
		return err
	}
	for _, sec := range secrets {
		if bytes.Contains(data, []byte(sec)) {
			return fmt.Errorf("claude.db leaked %q", sec)
		}
	}
	return nil
}

func (w *world) assertSubNoSecrets() error {
	if len(w.claudeFrames) == 0 {
		return fmt.Errorf("no envelopes pushed to the subscription to verify")
	}
	for _, f := range w.claudeFrames {
		for _, sec := range secrets {
			if strings.Contains(f, sec) {
				return fmt.Errorf("subscription envelope leaked %q", sec)
			}
		}
	}
	return nil
}

func (w *world) assertGQLNoSecrets() error {
	raw, _, err := w.gql(fmt.Sprintf(`{ claudeSession(sessionId:%q)%s }`, "S10", sessionFields))
	if err != nil {
		return err
	}
	for _, sec := range secrets {
		if strings.Contains(raw, sec) {
			return fmt.Errorf("GraphQL result leaked %q", sec)
		}
	}
	return nil
}

// ---- INSTALL ----

func (w *world) settingsWithUnrelated() error {
	w.settingsOrig = []byte(`{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo unrelated"}]}]}}`)
	return os.WriteFile(w.claudeSettingsPath, w.settingsOrig, 0o644)
}

func (w *world) installPrint() error {
	w.runCLI("install")
	return nil
}

func (w *world) assertPrintedBlock() error {
	if strings.Count(w.lastStdout, "supergraph claude-hook") < 7 {
		return fmt.Errorf("printed block missing claude-hook for all 7 events: %s", w.lastStdout)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Notification", "Stop", "SessionEnd"} {
		if !strings.Contains(w.lastStdout, ev) {
			return fmt.Errorf("printed block missing event %q", ev)
		}
	}
	return nil
}

func (w *world) assertSettingsUnchanged() error {
	data, err := os.ReadFile(w.claudeSettingsPath)
	if err != nil {
		return err
	}
	if !bytes.Equal(data, w.settingsOrig) {
		return fmt.Errorf("settings file changed by a print-only install")
	}
	return nil
}

func (w *world) installMerge() error {
	w.runCLI("install", "--install-hook", "--settings", w.claudeSettingsPath)
	if w.lastExit != 0 {
		return fmt.Errorf("install --install-hook exit=%d stderr=%s", w.lastExit, w.lastStderr)
	}
	return nil
}

func (w *world) mergedSettings() (map[string]any, error) {
	data, err := os.ReadFile(w.claudeSettingsPath)
	if err != nil {
		return nil, err
	}
	var s map[string]any
	return s, json.Unmarshal(data, &s)
}

func (w *world) assertAllEventsWired() error {
	s, err := w.mergedSettings()
	if err != nil {
		return err
	}
	hooks, _ := s["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Notification", "Stop", "SessionEnd"} {
		if !eventHasCommand(hooks[ev], "supergraph claude-hook") {
			return fmt.Errorf("event %q not wired to claude-hook", ev)
		}
	}
	return nil
}

func (w *world) assertUnrelatedPresent() error {
	s, err := w.mergedSettings()
	if err != nil {
		return err
	}
	hooks, _ := s["hooks"].(map[string]any)
	if !eventHasCommand(hooks["PreToolUse"], "echo unrelated") {
		return fmt.Errorf("pre-existing unrelated PreToolUse hook was lost")
	}
	return nil
}

func (w *world) assertNoDuplicate() error {
	data, err := os.ReadFile(w.claudeSettingsPath)
	if err != nil {
		return err
	}
	if n := strings.Count(string(data), "supergraph claude-hook"); n != 7 {
		return fmt.Errorf("claude-hook appears %d times after a second merge, want 7 (duplicated)", n)
	}
	return nil
}

func eventHasCommand(arr any, cmd string) bool {
	entries, _ := arr.([]any)
	for _, e := range entries {
		em, _ := e.(map[string]any)
		inner, _ := em["hooks"].([]any)
		for _, h := range inner {
			hm, _ := h.(map[string]any)
			if hm["command"] == cmd {
				return true
			}
		}
	}
	return false
}

// ---- LOC + ZEROCORE ----

func (w *world) runLocClaude() error {
	cmd := exec.Command("make", "loc-claude")
	cmd.Dir = repoRoot
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	w.makeStdout = so.String()
	w.makeExit = exitCode(err)
	return nil
}

func (w *world) assertLocOK() error {
	if w.makeExit != 0 {
		return fmt.Errorf("loc-claude over budget: %s", w.makeStdout)
	}
	return nil
}

func (w *world) runGitDiffCore() error {
	cmd := exec.Command("git", "diff", "--stat", "--", "core/", "server/")
	cmd.Dir = repoRoot
	var so bytes.Buffer
	cmd.Stdout = &so
	_ = cmd.Run()
	w.diffOut = strings.TrimSpace(so.String())
	return nil
}

func (w *world) assertNoCoreDiff() error {
	if w.diffOut != "" {
		return fmt.Errorf("core/ or server/ changed:\n%s", w.diffOut)
	}
	return nil
}

// ---- F1 ----

func (w *world) f1Post20() error {
	var durations []time.Duration
	for i := 0; i < 20; i++ {
		sid := fmt.Sprintf("F1-%02d", i)
		start := time.Now()
		if code := w.postHook(sid, "PreToolUse", map[string]any{"tool_name": "Bash"}); code != http.StatusOK {
			return fmt.Errorf("hook %s status %d", sid, code)
		}
		if w.session(sid) == nil {
			return fmt.Errorf("session %s not queryable after POST", sid)
		}
		durations = append(durations, time.Since(start))
	}
	sort.Slice(durations, func(a, b int) bool { return durations[a] < durations[b] })
	w.p95Store = durations[18] // ceil(0.95*20)-1
	return nil
}

func (w *world) f1AssertP95() error {
	if w.p95Store <= 0 {
		return fmt.Errorf("no p95 measured")
	}
	if w.p95Store >= time.Second {
		return fmt.Errorf("p95 ingest-to-queryable = %s, want < 1s", w.p95Store)
	}
	return nil
}
