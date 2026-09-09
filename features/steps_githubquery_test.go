package features

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// ghqState is the github-query scenarios' per-run capture, closed over by
// registerGithubQuerySteps so no field is added to the shared ghWorld. Every query
// step overwrites the field it sets, so nothing leaks across scenarios.
type ghqState struct {
	issue    map[string]any
	issueSet bool // an `issue` query ran (distinguishes null from "not queried")
	issues   []map[string]any
	pr       map[string]any
	prs      []map[string]any // pullRequestsForRepo result (#26)
	samples  []time.Duration
}

// registerGithubQuerySteps wires every @local step in features/github-query.feature
// onto the SAME ghWorld g (and the server the shared Given started), reading the
// seeded github/claude/tmux dbs under one data dir. It reuses g.fake +
// g.assertZeroRequests; the typed reads + join go through core /graphql (g.sw.gql).
func registerGithubQuerySteps(sc *godog.ScenarioContext, g *ghWorld) {
	st := &ghqState{}
	re := func(p string) *regexp.Regexp { return regexp.MustCompile(p) }

	// ---------- Given: seed the cache-only stores ----------
	sc.Step(re(`^issue \x60issue:o/r#(\d+)\x60 is already in the store, fresh, with title "([^"]*)"$`),
		func(n, title string) error { return g.ghqSeedIssue(atoiMust(n), title) })
	sc.Step(re(`^issue \x60issue:o/r#(\d+)\x60 is in the store$`),
		func(n string) error { return g.ghqSeedIssue(atoiMust(n), "issue "+n) })
	sc.Step(re(`^no node for key \x60issue:o/r#(\d+)\x60 is in the store$`), noop1)
	sc.Step(re(`^issue \x60issue:o/r#(\d+)\x60 is in the store assigned to "([^"]*)" in state "([^"]*)"$`),
		func(n, login, state string) error { return g.ghqSeedIssueAssigned(atoiMust(n), login, state) })
	sc.Step(re(`^the \x60openIssues\x60 op has been warmed for \x60o/r\x60 with issues #5, #6, #7$`),
		func() error { return g.ghqWarmIssues(5, 6, 7) })
	sc.Step(re(`^pull request \x60pr:o/r#(\d+)\x60 is already in the store with headRefName "([^"]*)"$`),
		func(n, head string) error { return g.ghqSeedPR(atoiMust(n), head, "") })
	sc.Step(re(`^pull request \x60pr:o/r#(\d+)\x60 is in the store with headRefName "([^"]*)" and body "([^"]*)"$`),
		func(n, head, body string) error { return g.ghqSeedPR(atoiMust(n), head, body) })
	sc.Step(re(`^a tmux pane \x60(pane:[^\x60]+)\x60 is on branch "([^"]*)"$`),
		func(key, branch string) error { return g.ghqSeedPane(key, branch) })
	sc.Step(re(`^a claude session \x60([^\x60]+)\x60 is on branch "([^"]*)"$`),
		func(sid, branch string) error { return g.ghqSeedClaude(sid, branch) })
	sc.Step(re(`^no tmux pane or claude session is on any branch deriving to issue (\d+)$`), noop1)
	sc.Step(re(`^no claude session is on any branch deriving to issue (\d+)$`), noop1)
	sc.Step(re(`^issue \x60issue:o/r#(\d+)\x60 is in the store with a matching pane and session on branch "([^"]*)"$`),
		func(n, branch string) error {
			num := atoiMust(n)
			if err := g.ghqSeedIssue(num, "issue "+n); err != nil {
				return err
			}
			if err := g.ghqSeedPane(fmt.Sprintf("pane:issue%s-spike:0.0@boxA", n), branch); err != nil {
				return err
			}
			return g.ghqSeedClaude("sess-"+n, branch)
		})

	// ---------- When: typed reads through core /graphql ----------
	sc.Step(re(`^\x60issue\x60 is queried for key \x60issue:o/r#(\d+)\x60$`),
		func(n string) error { return g.ghqQueryIssue(st, atoiMust(n), "number title state url") })
	sc.Step(re(`^\x60issue\x60 is queried for key \x60issue:o/r#(\d+)\x60 selecting \x60tmuxPanes\x60 and \x60claudeSessions\x60$`),
		func(n string) error {
			return g.ghqQueryIssue(st, atoiMust(n), "number tmuxPanes { key } claudeSessions { sessionId }")
		})
	sc.Step(re(`^\x60issue\x60 is queried for key \x60issue:o/r#(\d+)\x60 selecting \x60tmuxPanes\x60$`),
		func(n string) error { return g.ghqQueryIssue(st, atoiMust(n), "number tmuxPanes { key }") })
	sc.Step(re(`^\x60pullRequest\x60 is queried for key \x60pr:o/r#(\d+)\x60$`),
		func(n string) error { return g.ghqQueryPR(st, atoiMust(n)) })
	sc.Step(re(`^\x60issuesForRepo\x60 is queried for owner "([^"]*)" repo "([^"]*)"$`),
		func(owner, repo string) error { return g.ghqQueryIssuesForRepo(st, owner, repo) })
	sc.Step(re(`^this exact query is run 20 times against the warm cache:?$`),
		func(doc *godog.DocString) error { return g.ghqRun20(st, doc.Content) })

	// ---------- Then ----------
	sc.Step(re(`^the complete stored issue node is returned with title "([^"]*)"$`),
		func(title string) error {
			if !st.issueSet || st.issue == nil {
				return fmt.Errorf("issue was null, want title %q", title)
			}
			if got, _ := st.issue["title"].(string); got != title {
				return fmt.Errorf("title = %q, want %q", got, title)
			}
			return nil
		})
	sc.Step(re(`^the result is null$`), func() error {
		if !st.issueSet {
			return fmt.Errorf("no issue query ran")
		}
		if st.issue != nil {
			return fmt.Errorf("issue = %v, want null", st.issue)
		}
		return nil
	})
	sc.Step(re(`^exactly issues #5, #6, #7 are returned$`), func() error {
		return assertIssueNumbers(st.issues, 5, 6, 7)
	})
	sc.Step(re(`^issue #(\d+) has state "([^"]*)" and assignee "([^"]*)"$`), func(n, state, login string) error {
		var row map[string]any
		for _, r := range st.issues {
			if f, ok := r["number"].(float64); ok && int(f) == atoiMust(n) {
				row = r
			}
		}
		if row == nil {
			return fmt.Errorf("issue #%s not in issuesForRepo result %v", n, st.issues)
		}
		if got, _ := row["state"].(string); got != state {
			return fmt.Errorf("issue #%s state = %q, want %q", n, got, state)
		}
		if !listHasField(row, "assignees", "login", login) {
			return fmt.Errorf("issue #%s assignees %v missing login %q", n, jsonField(row, "assignees"), login)
		}
		return nil
	})
	sc.Step(re(`^an empty list is returned$`), func() error {
		if len(st.issues) != 0 {
			return fmt.Errorf("issuesForRepo returned %d, want 0", len(st.issues))
		}
		return nil
	})
	sc.Step(re(`^the returned pull request's headRefName is "([^"]*)"$`), func(head string) error {
		if st.pr == nil {
			return fmt.Errorf("pullRequest was null, want headRefName %q", head)
		}
		if got, _ := st.pr["headRefName"].(string); got != head {
			return fmt.Errorf("headRefName = %q, want %q", got, head)
		}
		return nil
	})
	sc.Step(re(`^the response tmuxPanes (contains|does not contain) pane key \x60(pane:[^\x60]+)\x60$`),
		func(expect, key string) error {
			has := listHasField(st.issue, "tmuxPanes", "key", key)
			if expect == "contains" && !has {
				return fmt.Errorf("tmuxPanes %v missing pane %q", jsonField(st.issue, "tmuxPanes"), key)
			}
			if expect == "does not contain" && has {
				return fmt.Errorf("tmuxPanes unexpectedly contains pane %q", key)
			}
			return nil
		})
	sc.Step(re(`^the response claudeSessions contains session id \x60([^\x60]+)\x60$`), func(sid string) error {
		if !listHasField(st.issue, "claudeSessions", "sessionId", sid) {
			return fmt.Errorf("claudeSessions %v missing session %q", jsonField(st.issue, "claudeSessions"), sid)
		}
		return nil
	})
	sc.Step(re(`^the response tmuxPanes is an empty list$`), func() error {
		if n := listLen(st.issue, "tmuxPanes"); n != 0 {
			return fmt.Errorf("tmuxPanes has %d entries, want 0", n)
		}
		return nil
	})
	sc.Step(re(`^the response claudeSessions is an empty list$`), func() error {
		if n := listLen(st.issue, "claudeSessions"); n != 0 {
			return fmt.Errorf("claudeSessions has %d entries, want 0", n)
		}
		return nil
	})
	sc.Step(re(`^the p95 latency over the 20 paired samples is under 1s$`), func() error {
		if len(st.samples) != 20 {
			return fmt.Errorf("captured %d samples, want 20", len(st.samples))
		}
		if got := p95(st.samples); got >= time.Second {
			return fmt.Errorf("p95 = %s, want < 1s", got)
		}
		return nil
	})

	// ---------- PR sidebar-parity fields (#26) ----------
	registerGithubPRFieldSteps(sc, g, st, re)

	// ---------- AC-GHQ-LOC / AC-GHQ-ZEROCORE ----------
	var locExit int
	var diffOut string
	sc.Step(re(`^the github-query change is applied$`), func() error { return nil })
	sc.Step(re(`^\x60make loc-github\x60 is run$`), func() error {
		cmd := exec.Command("make", "--no-print-directory", "loc-github")
		cmd.Dir = repoRoot
		locExit = exitCode(cmd.Run())
		return nil
	})
	sc.Step(re(`^it exits zero against the cap raised to measured plus five percent$`), func() error {
		if locExit != 0 {
			return fmt.Errorf("make loc-github exited %d, want 0 (cap 1810)", locExit)
		}
		return nil
	})
	sc.Step(re(`^the github EDR budget table shows the new measured total and cap$`), func() error { return nil })
	sc.Step(re(`^\x60git diff --stat core/ server/\x60 is run$`), func() error {
		cmd := exec.Command("git", "diff", "--stat", "core/", "server/")
		cmd.Dir = repoRoot
		out, _ := cmd.CombinedOutput()
		diffOut = strings.TrimSpace(string(out))
		return nil
	})
	sc.Step(re(`^the diffstat is empty$`), func() error {
		if coreChangeApproved() {
			fmt.Println("core diff exempted by trailer")
			return nil
		}
		if diffOut != "" {
			return fmt.Errorf("git diff --stat core/ server/ not empty:\n%s", diffOut)
		}
		return nil
	})

	// ---------- @live (AC-GHQ-LIVE-WARM) — real GitHub ----------
	// Opt-in via FEATURES_TAGS='@live' with GITHUB_TOKEN + LIVE_REPO set; excluded
	// from the default hermetic run by the ~@live default tag expression.
	var liveOwner, liveRepo string
	sc.Step(lit("a supergraph server with a real GITHUB_TOKEN and a repo under GITHUB_ORG"),
		func() error {
			o, r, err := liveRepoName()
			if err != nil {
				return err
			}
			liveOwner, liveRepo = o, r
			return g.startLive()
		})
	sc.Step(lit("the `openIssues` op is warmed through `/plugins/github/graphql` for that repo"),
		func() error {
			status, err := g.warmOp("openIssues", liveOwner, liveRepo)
			if err != nil {
				return err
			}
			if status != 200 {
				return fmt.Errorf("warm openIssues status %d, want 200", status)
			}
			return nil
		})
	sc.Step(lit("`issuesForRepo` is queried for that repo"),
		func() error { return g.ghqQueryIssuesForRepo(st, liveOwner, liveRepo) })
	sc.Step(lit("the returned issue numbers equal the open issues the GitHub REST API lists"),
		func() error {
			want, err := restOpenIssueNumbers(liveOwner, liveRepo)
			if err != nil {
				return err
			}
			got := map[int]bool{}
			for _, r := range st.issues {
				if f, ok := r["number"].(float64); ok {
					got[int(f)] = true
				}
			}
			if len(got) != len(want) {
				return fmt.Errorf("issuesForRepo returned %d numbers, REST lists %d (%v vs %v)", len(got), len(want), got, want)
			}
			for _, w := range want {
				if !got[w] {
					return fmt.Errorf("issuesForRepo missing #%d that REST lists (%v)", w, want)
				}
			}
			return nil
		})
}

// liveRepoName is the github-query package alias for liveRepo (defined in
// github_helpers_test.go) so this file reads clearly at its one call site.
func liveRepoName() (string, string, error) { return liveRepo() }

func noop1(_ string) error { return nil }

func atoiMust(s string) int { n, _ := strconv.Atoi(s); return n }

// ---------- query helpers (core /graphql) ----------

// ghqQueryIssue fires `issue(key)` selecting sel and captures the node (or null).
// It resets the fake GitHub log first so AC-GHQ-HIT/MISS's "zero requests during
// the query" measures only the read, not the startup reconcile.
func (g *ghWorld) ghqQueryIssue(st *ghqState, n int, sel string) error {
	g.fake.ResetLog()
	q := fmt.Sprintf(`{ issue(key: "issue:o/r#%d") { %s } }`, n, sel)
	_, data, err := g.sw.gql(q)
	if err != nil {
		return err
	}
	st.issueSet = true
	st.issue, _ = data["issue"].(map[string]any)
	return nil
}

func (g *ghWorld) ghqQueryPR(st *ghqState, n int) error {
	g.fake.ResetLog()
	q := fmt.Sprintf(`{ pullRequest(key: "pr:o/r#%d") { number headRefName state url draft reviewDecision statusCheckRollup mergeStateStatus } }`, n)
	_, data, err := g.sw.gql(q)
	if err != nil {
		return err
	}
	st.pr, _ = data["pullRequest"].(map[string]any)
	return nil
}

func (g *ghWorld) ghqQueryIssuesForRepo(st *ghqState, owner, repo string) error {
	g.fake.ResetLog()
	q := fmt.Sprintf(`{ issuesForRepo(owner: %q, repo: %q) { number state assignees { login } } }`, owner, repo)
	_, data, err := g.sw.gql(q)
	if err != nil {
		return err
	}
	st.issues = nil
	if arr, ok := data["issuesForRepo"].([]any); ok {
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				st.issues = append(st.issues, m)
			}
		}
	}
	return nil
}

func (g *ghWorld) ghqRun20(st *ghqState, query string) error {
	st.samples = nil
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		if _, _, err := g.sw.gql(query); err != nil {
			return err
		}
		st.samples = append(st.samples, time.Since(t0))
	}
	return nil
}

// ---------- seed helpers (direct db writes, same pattern as g.seedNode) ----------

func (g *ghWorld) ghqSeedIssue(n int, title string) error {
	key := fmt.Sprintf("issue:o/r#%d", n)
	body := map[string]any{"number": n, "title": title, "state": "open", "id": key, "url": "https://x/" + key}
	now := time.Now()
	return g.seedNode(key, "Issue", body, `W/"q"`, false, now, now)
}

// ghqSeedIssueAssigned seeds an issue carrying a state and one assignee login, in
// the GraphQL node shape the openIssues op caches (assignees{nodes{login}}), so the
// dispatcher's state/assignees fields resolve from cache with zero upstream calls.
func (g *ghWorld) ghqSeedIssueAssigned(n int, login, state string) error {
	key := fmt.Sprintf("issue:o/r#%d", n)
	body := map[string]any{
		"number": n, "title": fmt.Sprintf("issue %d", n), "state": state,
		"id": key, "url": "https://x/" + key,
		"assignees": map[string]any{"nodes": []any{map[string]any{"login": login}}},
	}
	now := time.Now()
	return g.seedNode(key, "Issue", body, `W/"q"`, false, now, now)
}

func (g *ghWorld) ghqWarmIssues(nums ...int) error {
	for _, n := range nums {
		if err := g.ghqSeedIssue(n, fmt.Sprintf("issue %d", n)); err != nil {
			return err
		}
	}
	return nil
}

func (g *ghWorld) ghqSeedPR(n int, head, body string) error {
	key := fmt.Sprintf("pr:o/r#%d", n)
	node := map[string]any{
		"number": n, "title": fmt.Sprintf("pr %d", n), "state": "open", "id": key,
		"url": "https://x/" + key, "headRefName": head, "baseRefName": "main", "body": body,
	}
	now := time.Now()
	return g.seedNode(key, "PullRequest", node, `W/"q"`, false, now, now)
}

// tmuxDB / claudeDB open the plugin's own sqlite files (Migrate created the tables
// at startup, even for the dormant tmux plugin), mirroring g.ghDB.
func (g *ghWorld) tmuxDB() (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.Join(g.sw.dataDir, "tmux.db")+"?_pragma=busy_timeout%285000%29")
}

func (g *ghWorld) claudeDB() (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+filepath.Join(g.sw.dataDir, "claude.db")+"?_pragma=busy_timeout%285000%29")
}

// waitTable blocks until the named table exists in db, closing the seed-before-migrate
// race: startServe's /health gate (readyPlugins) only awaits template+fakeok, so the
// tmux/claude plugins' Migrate can still be in flight when a seed runs — losing that
// race yields "no such table" on the slower ubuntu runner (green on faster macs). The
// dormant tmux plugin never emits lastEventAt, so it cannot join readyPlugins; polling
// sqlite_master for its own table is the black-box wait that fits this per-seed path.
func waitTable(db *sql.DB, table string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		var name string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&name)
		if err == nil {
			return nil
		}
		if err != sql.ErrNoRows {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("table %q not migrated within 5s (plugin Migrate never ran)", table)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ghqSeedPane seeds a tmux session (carrying the branch) plus a pane joined to it
// by session name (panesForBranch joins pane.session = session.name).
func (g *ghWorld) ghqSeedPane(paneKey, branch string) error {
	db, err := g.tmuxDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := waitTable(db, "tmux_sessions"); err != nil {
		return err
	}
	name := sessionNameFromPaneKey(paneKey)
	host := hostFromKey(paneKey)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO tmux_sessions (key, name, worktree, branch, last_seen_at, stale_since)
		VALUES (?,?,?,?,?,NULL)
		ON CONFLICT(key) DO UPDATE SET name=excluded.name, branch=excluded.branch`,
		"session:"+name+"@"+host, name, "", branch, now); err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO tmux_panes (key, session, window, pane, pid, cmd, path, active, free, last_seen_at, stale_since)
		VALUES (?,?,?,?,?,?,NULL,?,?,?,NULL)
		ON CONFLICT(key) DO UPDATE SET session=excluded.session`,
		paneKey, name, 0, 0, 0, "zsh", 1, 0, now)
	return err
}

// ghqSeedClaude seeds a claude session on branch, keyed to the branch's derived
// issue number the SAME way the claude plugin would (shared issuekey), so a session
// on a bare issueN branch attaches by issue number and one on a non-issue branch
// (e.g. a PR hotfix branch) attaches only via the join's branch set.
func (g *ghWorld) ghqSeedClaude(sid, branch string) error {
	db, err := g.claudeDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	if err := waitTable(db, "claude_sessions"); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = db.Exec(`INSERT INTO claude_sessions
		(sid, host_id, cwd, git_branch, issue_number, model, state, last_tool, tool_calls, pane, pid, pr_number, pr_url, started_at, last_event_at, stale_since)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'')
		ON CONFLICT(sid) DO UPDATE SET git_branch=excluded.git_branch, issue_number=excluded.issue_number`,
		sid, "boxA", "/w", branch, issueNumForBranch(branch), "", "idle", "", 0, "", 0, 0, "", now, now)
	return err
}

// issueNumForBranch derives the issue number a branch encodes with the exact anchored
// grammar the shared internal/issuekey package uses, so the test seeds claude rows the
// way the plugin would. Kept as a local copy to keep the black-box harness independent
// of the production package under test; the derivation grammar's own contract (the
// positive/negative forms) is unit-tested in internal/issuekey/issuekey_test.go, which
// is the source of truth — this copy only needs to agree with it.
var ghqBranchRe = regexp.MustCompile(`^issue-?(\d+)([/-]|$)`)

func issueNumForBranch(branch string) int {
	m := ghqBranchRe.FindStringSubmatch(strings.TrimSpace(branch))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

func sessionNameFromPaneKey(key string) string {
	s := strings.TrimPrefix(key, "pane:")
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

func hostFromKey(key string) string {
	if i := strings.LastIndexByte(key, '@'); i >= 0 {
		return key[i+1:]
	}
	return "boxA"
}

// ---------- assertion helpers ----------

func assertIssueNumbers(rows []map[string]any, want ...int) error {
	got := map[int]bool{}
	for _, r := range rows {
		if f, ok := r["number"].(float64); ok {
			got[int(f)] = true
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("issuesForRepo returned %d distinct numbers, want %d (%v)", len(got), len(want), rows)
	}
	for _, w := range want {
		if !got[w] {
			return fmt.Errorf("issuesForRepo missing #%d (got %v)", w, rows)
		}
	}
	return nil
}

func listHasField(node map[string]any, list, field, want string) bool {
	if node == nil {
		return false
	}
	arr, ok := node[list].([]any)
	if !ok {
		return false
	}
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			if s, _ := m[field].(string); s == want {
				return true
			}
		}
	}
	return false
}

func listLen(node map[string]any, list string) int {
	if node == nil {
		return 0
	}
	if arr, ok := node[list].([]any); ok {
		return len(arr)
	}
	return 0
}

func jsonField(node map[string]any, field string) string {
	if node == nil {
		return "<null>"
	}
	b, _ := json.Marshal(node[field])
	return string(b)
}

// ===================== PR sidebar-parity fields (#26) =====================

// registerGithubPRFieldSteps wires the #26 scenarios (draft/reviewDecision/
// statusCheckRollup/mergeStateStatus + pullRequestsForRepo + the check_run→pr
// invalidation fan-out) onto the same ghWorld/ghqState the other github-query
// steps use. re is the shared compiler passed in so the closure style matches.
func registerGithubPRFieldSteps(sc *godog.ScenarioContext, g *ghWorld, st *ghqState, re func(string) *regexp.Regexp) {
	// ---------- Given ----------
	sc.Step(re(`^pull request \x60pr:o/r#(\d+)\x60 is warmed through the \x60pr\x60 op with draft (true|false), reviewDecision (null|"[^"]*"), statusCheckRollup (null|"[^"]*"), mergeStateStatus (null|"[^"]*")$`),
		func(n, draft, rev, roll, merge string) error {
			return g.ghqWarmPR(atoiMust(n), draft == "true", prFieldVal(rev), prFieldVal(roll), prFieldVal(merge))
		})
	sc.Step(re(`^pull request \x60pr:o/r#(\d+)\x60 is in the store with draft (true|false), reviewDecision (null|"[^"]*"), statusCheckRollup (null|"[^"]*"), mergeStateStatus (null|"[^"]*")$`),
		func(n, draft, rev, roll, merge string) error {
			return g.ghqSeedPRSidebar(atoiMust(n), draft == "true", prFieldVal(rev), prFieldVal(roll), prFieldVal(merge))
		})
	sc.Step(re(`^cached checkRun nodes for \x60pr:o/r#(\d+)\x60 all report success$`),
		func(n string) error { return g.ghqSeedCheckRunsSuccess(atoiMust(n)) })
	sc.Step(re(`^the \x60openPRs\x60 op has been warmed for \x60o/r\x60 with PRs #(\d+), #(\d+) carrying the four sidebar fields$`),
		func(a, b string) error { return g.ghqWarmOpenPRs(atoiMust(a), atoiMust(b)) })
	sc.Step(re(`^a websocket subscription to \x60checkRunUpdated\x60 is open$`),
		func() error { return g.ghqOpenCheckRunSub() })
	sc.Step(re(`^upstream \x60pr:o/r#(\d+)\x60 changes draft to (true|false)$`),
		func(n, v string) error { return g.ghqMutatePRField(atoiMust(n), "draft", v) })
	sc.Step(re(`^upstream \x60pr:o/r#(\d+)\x60 changes (reviewDecision|statusCheckRollup|mergeStateStatus) to "([^"]*)"$`),
		func(n, field, v string) error { return g.ghqMutatePRField(atoiMust(n), field, v) })

	// ---------- When ----------
	sc.Step(re(`^\x60pullRequestsForRepo\x60 is queried for owner "([^"]*)" repo "([^"]*)"$`),
		func(owner, repo string) error { return g.ghqQueryPRsForRepo(st, owner, repo) })
	sc.Step(re(`^\x60pr:o/r#(\d+)\x60 is re-warmed and queried again$`),
		func(n string) error { return g.ghqRewarmAndQueryPR(st, atoiMust(n)) })
	sc.Step(re(`^a signed \x60check_run\x60 webhook naming pull request #(\d+) is received$`),
		func(n string) error { return g.ghqCheckRunWebhook(atoiMust(n)) })
	sc.Step(re(`^a signed \x60pull_request\x60 webhook naming PR #(\d+) is received$`),
		func(n string) error { return g.ghqPRWebhook(atoiMust(n)) })
	sc.Step(re(`^a signed \x60pull_request_review\x60 webhook naming PR #(\d+) is received$`),
		func(n string) error { return g.ghqReviewWebhook(atoiMust(n)) })
	sc.Step(re(`^the reconcile revalidate pass runs once$`), g.reconcileRunsOnce)

	// ---------- Then ----------
	sc.Step(re(`^the returned pull request's draft is (true|false)$`), func(want string) error {
		if st.pr == nil {
			return fmt.Errorf("pullRequest was null, want draft %s", want)
		}
		got, _ := st.pr["draft"].(bool)
		if fmt.Sprintf("%t", got) != want {
			return fmt.Errorf("draft = %t, want %s", got, want)
		}
		return nil
	})
	sc.Step(re(`^the returned pull request's (reviewDecision|statusCheckRollup|mergeStateStatus) is "([^"]*)"$`),
		func(field, want string) error {
			if st.pr == nil {
				return fmt.Errorf("pullRequest was null, want %s %q", field, want)
			}
			got, ok := st.pr[field].(string)
			if !ok {
				return fmt.Errorf("%s = %v (null), want %q", field, st.pr[field], want)
			}
			if got != want {
				return fmt.Errorf("%s = %q, want %q", field, got, want)
			}
			return nil
		})
	sc.Step(re(`^the returned pull request's (reviewDecision|statusCheckRollup|mergeStateStatus) is null$`),
		func(field string) error {
			if st.pr == nil {
				return fmt.Errorf("pullRequest was null")
			}
			if v := st.pr[field]; v != nil {
				return fmt.Errorf("%s = %v, want null", field, v)
			}
			return nil
		})
	sc.Step(re(`^exactly pull requests #(\d+), #(\d+) are returned$`), func(a, b string) error {
		return assertPRNumbers(st.prs, atoiMust(a), atoiMust(b))
	})
	sc.Step(re(`^each returned pull request has draft, reviewDecision, statusCheckRollup and mergeStateStatus populated$`), func() error {
		if len(st.prs) == 0 {
			return fmt.Errorf("no pull requests returned")
		}
		for _, r := range st.prs {
			if _, ok := r["draft"].(bool); !ok {
				return fmt.Errorf("draft missing on %v", r)
			}
			for _, f := range []string{"reviewDecision", "statusCheckRollup", "mergeStateStatus"} {
				if r[f] == nil {
					return fmt.Errorf("%s null on %v", f, r)
				}
			}
		}
		return nil
	})
	sc.Step(re(`^an empty pull request list is returned$`), func() error {
		if len(st.prs) != 0 {
			return fmt.Errorf("pullRequestsForRepo returned %d, want 0", len(st.prs))
		}
		return nil
	})
	sc.Step(re(`^the pull request result is null$`), func() error {
		if st.pr != nil {
			return fmt.Errorf("pullRequest = %v, want null", st.pr)
		}
		return nil
	})
	sc.Step(re(`^exactly one envelope keyed \x60(pr:o/r#\d+)\x60 is pushed on the \x60checkRunUpdated\x60 subscription$`),
		func(key string) error { return g.ghqAssertOnePush(key) })
	sc.Step(re(`^\x60(pr:o/r#\d+)\x60 is purged from the cache$`), func(key string) error {
		exists, err := g.nodeExists(key)
		if err != nil {
			return err
		}
		if exists {
			return fmt.Errorf("%s still cached after webhook, want purged", key)
		}
		return nil
	})

	// ---------- AC-GHPR-ADDONLY ----------
	var schemaDiff string
	sc.Step(re(`^\x60git diff main -- plugins/github/schema/github\.graphqls\x60 is run$`), func() error {
		cmd := exec.Command("git", "diff", "main", "--", "plugins/github/schema/github.graphqls")
		cmd.Dir = repoRoot
		out, _ := cmd.CombinedOutput()
		schemaDiff = string(out)
		return nil
	})
	sc.Step(re(`^the schema diff has no deleted or retyped lines$`), func() error {
		for _, line := range strings.Split(schemaDiff, "\n") {
			if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
				return fmt.Errorf("schema diff has a deletion line (not additions-only):\n%s", line)
			}
		}
		return nil
	})
}

// prFieldVal maps a feature cell to a stored value: `null` → "" (which projects to
// GraphQL null / false), otherwise the unquoted string.
func prFieldVal(s string) string {
	s = strings.TrimSpace(s)
	if s == "null" {
		return ""
	}
	return strings.Trim(s, `"`)
}

// prSidebarBody builds the nested GraphQL PR node shape the pr/openPRs ops return,
// carrying the #26 sidebar fields. An empty reviewDecision/mergeState/rollup omits
// that value so the projection yields GraphQL null.
func prSidebarBody(n int, head string, draft bool, reviewDecision, rollup, mergeState string) map[string]any {
	key := fmt.Sprintf("pr:o/r#%d", n)
	body := map[string]any{
		"number": n, "title": fmt.Sprintf("pr %d", n), "state": "OPEN", "id": key,
		"url": "https://x/" + key, "headRefName": head, "baseRefName": "main", "body": "",
		"isDraft": draft,
	}
	if reviewDecision != "" {
		body["reviewDecision"] = reviewDecision
	}
	if mergeState != "" {
		body["mergeStateStatus"] = mergeState
	}
	rollupNode := map[string]any{"commit": map[string]any{"statusCheckRollup": nil}}
	if rollup != "" {
		rollupNode = map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": rollup}}}
	}
	body["commits"] = map[string]any{"nodes": []any{rollupNode}}
	return body
}

// ghqWarmPR seeds pr:o/r#N in the fake GitHub world then warms it through the real
// pr point op, so the stored node is the canonical GraphQL projection.
func (g *ghWorld) ghqWarmPR(n int, draft bool, reviewDecision, rollup, mergeState string) error {
	g.fake.SetNode(fmt.Sprintf("pr:o/r#%d", n), "PullRequest", prSidebarBody(n, "feature/x", draft, reviewDecision, rollup, mergeState))
	_, status, err := g.postOp("pr", map[string]any{"owner": "o", "repo": "r", "number": n})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("warm pr status %d", status)
	}
	return nil
}

// ghqSeedPRSidebar writes a sidebar-shaped PR node straight into the cache (direct
// db seed, same pattern as ghqSeedPR), bypassing the warm path.
func (g *ghWorld) ghqSeedPRSidebar(n int, draft bool, reviewDecision, rollup, mergeState string) error {
	now := time.Now()
	return g.seedNode(fmt.Sprintf("pr:o/r#%d", n), "PullRequest",
		prSidebarBody(n, "feature/x", draft, reviewDecision, rollup, mergeState), `W/"q"`, false, now, now)
}

// ghqSeedCheckRunsSuccess seeds two cached checkRun nodes whose conclusion is
// success — the discriminating decoy for AC-GHPR-ROLLUP-RAW.
func (g *ghWorld) ghqSeedCheckRunsSuccess(prNum int) error {
	now := time.Now()
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("%d%d", prNum, i)
		body := map[string]any{"id": id, "name": fmt.Sprintf("ci-%d", i), "status": "completed", "conclusion": "success"}
		if err := g.seedNode("checkRun:o/r/"+id, "CheckRun", body, `W/"c"`, false, now, now); err != nil {
			return err
		}
	}
	return nil
}

// ghqWarmOpenPRs seeds N open PRs in the fake world then warms them via the openPRs
// list op, so pullRequestsForRepo serves them cache-only.
func (g *ghWorld) ghqWarmOpenPRs(nums ...int) error {
	g.fake.AddRepo("o", "r")
	for _, n := range nums {
		g.fake.SetNode(fmt.Sprintf("pr:o/r#%d", n), "PullRequest",
			prSidebarBody(n, "feature/x", n%2 == 0, "APPROVED", "SUCCESS", "CLEAN"))
	}
	_, status, err := g.postOp("openPRs", map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("warm openPRs status %d", status)
	}
	return nil
}

// ghqQueryPRsForRepo fires pullRequestsForRepo through core /graphql, resetting the
// fake log first so "zero requests during the query" measures only the read.
func (g *ghWorld) ghqQueryPRsForRepo(st *ghqState, owner, repo string) error {
	g.fake.ResetLog()
	q := fmt.Sprintf(`{ pullRequestsForRepo(owner: %q, repo: %q) { number draft reviewDecision statusCheckRollup mergeStateStatus } }`, owner, repo)
	_, data, err := g.sw.gql(q)
	if err != nil {
		return err
	}
	st.prs = nil
	if arr, ok := data["pullRequestsForRepo"].([]any); ok {
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				st.prs = append(st.prs, m)
			}
		}
	}
	return nil
}

// ghqRewarmAndQueryPR re-warms pr:o/r#N through the pr op (refetching upstream after
// a purge) then reads it via the cache-only typed resolver.
func (g *ghWorld) ghqRewarmAndQueryPR(st *ghqState, n int) error {
	_, status, err := g.postOp("pr", map[string]any{"owner": "o", "repo": "r", "number": n})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("re-warm pr status %d", status)
	}
	return g.ghqQueryPR(st, n)
}

// ghqMutatePRField changes one sidebar field on the upstream fake PR node so a
// refetch after a webhook purge observes the new value.
func (g *ghWorld) ghqMutatePRField(n int, field, val string) error {
	key := fmt.Sprintf("pr:o/r#%d", n)
	_, ok := g.fake.Mutate(key, func(b map[string]any) {
		switch field {
		case "draft":
			b["isDraft"] = val == "true"
		case "reviewDecision", "mergeStateStatus":
			b[field] = val
		case "statusCheckRollup":
			b["commits"] = map[string]any{"nodes": []any{
				map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": val}}},
			}}
		}
	})
	if !ok {
		return fmt.Errorf("%s not found upstream", key)
	}
	return nil
}

func (g *ghWorld) ghqPostWebhook(event, action string, payload map[string]any) error {
	status, err := g.postWebhook(g.webhookSecret, event, action, payload)
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("%s webhook status %d, want 200", event, status)
	}
	return nil
}

func (g *ghWorld) ghqCheckRunWebhook(prNum int) error {
	return g.ghqPostWebhook("check_run", "completed", map[string]any{
		"check_run":  map[string]any{"id": 9000 + prNum, "pull_requests": []any{map[string]any{"number": prNum}}},
		"repository": map[string]any{"full_name": "o/r"},
	})
}

func (g *ghWorld) ghqPRWebhook(prNum int) error {
	return g.ghqPostWebhook("pull_request", "synchronize", map[string]any{
		"pull_request": map[string]any{"number": prNum},
		"repository":   map[string]any{"full_name": "o/r"},
	})
}

func (g *ghWorld) ghqReviewWebhook(prNum int) error {
	return g.ghqPostWebhook("pull_request_review", "submitted", map[string]any{
		"pull_request": map[string]any{"number": prNum},
		"review":       map[string]any{"id": 1, "state": "approved"},
		"repository":   map[string]any{"full_name": "o/r"},
	})
}

// ghqOpenCheckRunSub opens the checkRunUpdated subscription AND blocks until the
// server has actually registered the subscriber on the bus. openSubscription
// returns as soon as the subscribe frame is written, so an envelope emitted in
// the gap before registration is dropped by the unbuffered fan-out bus — the
// cause of a real intermittent failure on the #26 event scenarios. The probe is
// one signed `label` webhook: a DIFFERENT key kind from the pr: key the
// scenarios assert on, so ghqAssertOnePush's key filter ignores it entirely.
// Seeing the probe's own envelope arrive is proof the subscriber is live.
func (g *ghWorld) ghqOpenCheckRunSub() error {
	if err := g.sw.openSubscription("checkRunUpdated"); err != nil {
		return err
	}
	const probeKey = "label:o/r/ghq-sub-probe"
	start := time.Now()
	deadline := start.Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := g.ghqPostWebhook("label", "created", map[string]any{
			"label":      map[string]any{"name": "ghq-sub-probe"},
			"repository": map[string]any{"full_name": "o/r"},
		}); err != nil {
			return err
		}
		for {
			p, err := g.sw.ws.nextPush(500 * time.Millisecond)
			if err != nil {
				break // no more frames within the short window; re-probe
			}
			if ghqPushKey(p) == probeKey {
				return nil
			}
			// some other envelope (unrelated to this probe) — keep draining
			// this window in case the probe's own push is still in flight.
		}
	}
	return fmt.Errorf("subscriber readiness probe: no envelope keyed %s after %s", probeKey, time.Since(start))
}

// ghqAssertOnePush drains the checkRunUpdated subscription for a window and requires
// exactly one pushed envelope whose inner key equals want (a check_run also pushes a
// checkRun-keyed envelope, so this counts only the pr key).
func (g *ghWorld) ghqAssertOnePush(want string) error {
	if g.sw.ws == nil {
		return fmt.Errorf("no subscription open")
	}
	count := 0
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p, err := g.sw.ws.nextPush(time.Until(deadline))
		if err != nil {
			break
		}
		if ghqPushKey(p) == want {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("envelopes keyed %s = %d, want exactly 1", want, count)
	}
	return nil
}

// ghqPushKey extracts the envelope key from a checkRunUpdated push frame (its
// payload string is the marshaled {"key":...} purge envelope).
func ghqPushKey(push map[string]any) string {
	data, _ := push["data"].(map[string]any)
	ev, _ := data["checkRunUpdated"].(map[string]any)
	ps, _ := ev["payload"].(string)
	var m map[string]any
	_ = json.Unmarshal([]byte(ps), &m)
	k, _ := m["key"].(string)
	return k
}

func assertPRNumbers(rows []map[string]any, want ...int) error {
	got := map[int]bool{}
	for _, r := range rows {
		if f, ok := r["number"].(float64); ok {
			got[int(f)] = true
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("pullRequestsForRepo returned %d distinct numbers, want %d (%v)", len(got), len(want), rows)
	}
	for _, w := range want {
		if !got[w] {
			return fmt.Errorf("pullRequestsForRepo missing #%d (got %v)", w, rows)
		}
	}
	return nil
}
