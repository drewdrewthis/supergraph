// Command spikeharness is the no-creds seeding + timing helper for
// scripts/spike-measure.sh (docs/plans/post-tier.md §B, docs/edr/spike-measure.md).
//
// It is an in-module helper (not a _test.go) so a spawned `supergraph serve`
// subprocess can be driven end to end with the same fakes the features suite uses:
// it hosts plugins/github/fakegh as the wired GitHub upstream, seeds the github and
// claude SQLite caches by the SAME direct-write shape features/steps_githubquery_test.go
// uses for the AC-GHQ-P95 query, times the cross-plugin query over HTTP, and polls the
// peers query for the two-box staleness transition. tmux is NOT seeded here — the
// script drives a real `tmux -L sgmeasure` socket so the tmux plugin ingests panes
// through its real reconcile path (EDR D4).
//
// Subcommands: freeport, fakegh, seed-github, seed-claude, measure, measure-cli,
// pollpeer, peercheck.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
	_ "modernc.org/sqlite" // sqlite driver for the direct cache seeds
)

func main() {
	if len(os.Args) < 2 {
		fail("usage: spikeharness <freeport|fakegh|seed-github|seed-claude|measure|measure-cli|pollpeer|peercheck> [flags]")
	}
	var err error
	switch os.Args[1] {
	case "freeport":
		err = cmdFreeport()
	case "fakegh":
		err = cmdFakegh(os.Args[2:])
	case "seed-github":
		err = cmdSeedGithub(os.Args[2:])
	case "seed-claude":
		err = cmdSeedClaude(os.Args[2:])
	case "measure":
		err = cmdMeasure(os.Args[2:])
	case "measure-cli":
		err = cmdMeasureCLI(os.Args[2:])
	case "pollpeer":
		err = cmdPollpeer(os.Args[2:])
	case "peercheck":
		err = cmdPeercheck(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "spikeharness: "+msg)
	os.Exit(1)
}

// flags is a tiny positional-free --key value parser (kept local so the helper has
// no flag-package quirks around repeated subcommands).
func flags(args []string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(args); i += 2 {
		m[args[i]] = args[i+1]
	}
	return m
}

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// ---------- freeport ----------

// cmdFreeport prints one currently-free loopback host:port, the same trick the
// features harness's freePort uses.
func cmdFreeport() error {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer func() { _ = l.Close() }()
	fmt.Println(l.Addr().String())
	return nil
}

// ---------- fakegh ----------

// cmdFakegh starts a fake GitHub server, seeds a realistic world (repos/issues/PRs),
// prints `URL=<httptest url>` on stdout, then blocks until SIGTERM/SIGINT so the
// serve subprocess can point [plugins.github] at it as a live upstream.
func cmdFakegh(args []string) error {
	f := flags(args)
	repos := atoiOr(f["--repos"], 10)
	issues := atoiOr(f["--issues"], 200)
	prs := atoiOr(f["--prs"], 100)

	srv := fakegh.New()
	defer srv.Close()
	for r := 0; r < repos; r++ {
		srv.AddRepo("o", fmt.Sprintf("r%d", r))
	}
	srv.AddRepo("o", "r") // the queried repo
	for n := 1; n <= issues; n++ {
		srv.AddIssue("o", "r", n, fmt.Sprintf("issue %d", n), "open")
	}
	for n := 1; n <= prs; n++ {
		srv.AddPR("o", "r", n, fmt.Sprintf("pr %d", n), "open", time.Time{})
	}
	fmt.Printf("URL=%s\n", srv.URL)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	return nil
}

// ---------- direct cache seeds ----------

func openDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout%285000%29")
}

// cmdSeedGithub writes issue/PR nodes straight into the running plugin's github.db,
// the exact on-disk shape features/github_helpers_test.go seedNode uses (a warm
// cache hit reads these with zero upstream call). Concentrated in the queried repo
// o/r so the join's per-repo PR scan sees the full PR set (a conservative upper
// bound — see the EDR).
func cmdSeedGithub(args []string) error {
	f := flags(args)
	path := f["--db"]
	if path == "" {
		return fmt.Errorf("seed-github: --db required")
	}
	issues := atoiOr(f["--issues"], 200)
	prs := atoiOr(f["--prs"], 100)

	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO github_nodes (key, typename, node_json, etag, pinned, fetched_at, updated_at)
		VALUES (?,?,?,?,0,?,?)
		ON CONFLICT(key) DO UPDATE SET node_json=excluded.node_json, fetched_at=excluded.fetched_at`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for n := 1; n <= issues; n++ {
		key := fmt.Sprintf("issue:o/r#%d", n)
		body, _ := json.Marshal(map[string]any{
			"number": n, "title": fmt.Sprintf("issue %d", n), "state": "open",
			"id": key, "url": "https://x/" + key,
		})
		if _, err := stmt.Exec(key, "Issue", body, `W/"i`+strconv.Itoa(n)+`"`, now, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	for n := 1; n <= prs; n++ {
		key := fmt.Sprintf("pr:o/r#%d", n)
		body, _ := json.Marshal(map[string]any{
			"number": n, "title": fmt.Sprintf("pr %d", n), "state": "open", "id": key,
			"url": "https://x/" + key, "headRefName": fmt.Sprintf("pr%d/branch", n),
			"baseRefName": "main", "body": "",
		})
		if _, err := stmt.Exec(key, "PullRequest", body, `W/"p`+strconv.Itoa(n)+`"`, now, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("seeded github: %d issues, %d prs\n", issues, prs)
	return nil
}

// cmdSeedClaude writes claude_sessions straight into the running plugin's claude.db,
// the exact shape features/steps_githubquery_test.go ghqSeedClaude uses. Every 5th
// session lands on issue5/spike-core so claudeSessions(issueNumber:5) returns a
// realistic handful; the rest spread across other issue branches.
func cmdSeedClaude(args []string) error {
	f := flags(args)
	path := f["--db"]
	if path == "" {
		return fmt.Errorf("seed-claude: --db required")
	}
	sessions := atoiOr(f["--sessions"], 30)

	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO claude_sessions
		(sid, host_id, cwd, git_branch, issue_number, model, state, last_tool, tool_calls, pane, pid, pr_number, pr_url, started_at, last_event_at, stale_since)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'')
		ON CONFLICT(sid) DO UPDATE SET git_branch=excluded.git_branch, issue_number=excluded.issue_number`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	for i := 0; i < sessions; i++ {
		var branch string
		if i%5 == 0 {
			branch = "issue5/spike-core"
		} else {
			branch = fmt.Sprintf("issue%d/work", (i%40)+1)
		}
		sid := fmt.Sprintf("spk-sess-%d", i)
		if _, err := stmt.Exec(sid, "boxA", "/w", branch, issueNumForBranch(branch),
			"opus", "idle", "", 0, "", 0, 0, "", now, now); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	fmt.Printf("seeded claude: %d sessions\n", sessions)
	return nil
}

// branchRe is the shared internal/issuekey grammar, copied verbatim from
// features/steps_githubquery_test.go so this black-box helper agrees with the harness
// without importing the package under measurement.
var branchRe = regexp.MustCompile(`^issue-?(\d+)([/-]|$)`)

// issueNumForBranch derives the issue number a branch encodes via branchRe (0 when the
// branch does not encode one).
func issueNumForBranch(branch string) int {
	m := branchRe.FindStringSubmatch(strings.TrimSpace(branch))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ---------- measure ----------

type summary struct {
	Label     string  `json:"label"`
	N         int     `json:"n"`
	Warmup    int     `json:"warmup"`
	P50ms     float64 `json:"p50_ms"`
	P95ms     float64 `json:"p95_ms"`
	P99ms     float64 `json:"p99_ms"`
	Minms     float64 `json:"min_ms"`
	Maxms     float64 `json:"max_ms"`
	Threshold string  `json:"threshold"`
	Verdict   string  `json:"verdict"`
}

// cmdMeasure POSTs the query file's content to url N+warmup times, discards the
// first `warmup` samples, and prints one machine-readable JSON summary line plus a
// human line. p95 < 1s is the S2/S4 warm bar.
func cmdMeasure(args []string) error {
	f := flags(args)
	url := f["--url"]
	qfile := f["--query"]
	if url == "" || qfile == "" {
		return fmt.Errorf("measure: --url and --query required")
	}
	n := atoiOr(f["--n"], 120)
	warmup := atoiOr(f["--warmup"], 20)
	label := f["--label"]
	if label == "" {
		label = "query"
	}

	q, err := os.ReadFile(qfile) //nolint:gosec // qfile is a script-written query path, not attacker input
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{"query": string(q)})

	client := &http.Client{Timeout: 30 * time.Second}
	samples := make([]time.Duration, 0, n)
	total := n + warmup
	for i := 0; i < total; i++ {
		t0 := time.Now()
		if err := postOnce(client, url, body); err != nil {
			return fmt.Errorf("measure request %d: %w", i, err)
		}
		d := time.Since(t0)
		if i >= warmup {
			samples = append(samples, d)
		}
	}

	emitSummary(summarize(label, warmup, samples))
	return nil
}

// summarize turns warm samples into a percentile summary with the S2/S4 verdict.
func summarize(label string, warmup int, samples []time.Duration) summary {
	s := summary{
		Label: label, N: len(samples), Warmup: warmup,
		P50ms: ms(pct(samples, 0.50)), P95ms: ms(pct(samples, 0.95)),
		P99ms: ms(pct(samples, 0.99)), Minms: ms(minD(samples)), Maxms: ms(maxD(samples)),
		Threshold: "p95 < 1000ms (PRD S2/S4 warm)",
	}
	if pct(samples, 0.95) < time.Second {
		s.Verdict = "PASS"
	} else {
		s.Verdict = "FAIL"
	}
	return s
}

// emitSummary prints the machine-readable MEASURE JSON line plus a human line.
func emitSummary(s summary) {
	j, _ := json.Marshal(s)
	fmt.Printf("MEASURE %s\n", j)
	fmt.Printf("%s: n=%d p50=%.2fms p95=%.2fms p99=%.2fms verdict=%s\n",
		s.Label, s.N, s.P50ms, s.P95ms, s.P99ms, s.Verdict)
}

// ---------- measure-cli ----------

// cmdMeasureCLI times the SAME query through the `supergraph query` CLI binary
// (endpoint-only, no config) instead of a raw HTTP POST. Each sample forks the binary,
// so p50/p95/p99 here include process spawn + cobra startup on top of the query — the
// numbers are reported separately from the direct-POST path (AC-SPIKE-LATENCY). It
// reuses the same N/warmup/percentile machinery as `measure`.
func cmdMeasureCLI(args []string) error {
	f := flags(args)
	bin := f["--bin"]
	url := f["--url"]
	qfile := f["--query"]
	if bin == "" || url == "" || qfile == "" {
		return fmt.Errorf("measure-cli: --bin, --url and --query required")
	}
	n := atoiOr(f["--n"], 200)
	warmup := atoiOr(f["--warmup"], 20)
	label := f["--label"]
	if label == "" {
		label = "query-cli"
	}
	q, err := os.ReadFile(qfile) //nolint:gosec // qfile is a script-written query path, not attacker input
	if err != nil {
		return err
	}
	query := string(q)

	samples := make([]time.Duration, 0, n)
	total := n + warmup
	for i := 0; i < total; i++ {
		t0 := time.Now()
		cmd := exec.Command(bin, "query", query, "--endpoint", url) //nolint:gosec // bin/url are script-provided, not attacker input
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("measure-cli request %d: %w\n%s", i, err, out)
		}
		d := time.Since(t0)
		if i >= warmup {
			samples = append(samples, d)
		}
	}
	emitSummary(summarize(label, warmup, samples))
	return nil
}

func postOnce(client *http.Client, url string, body []byte) error {
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, raw)
	}
	var r struct {
		Errors []any `json:"errors"`
	}
	_ = json.Unmarshal(raw, &r)
	if len(r.Errors) > 0 {
		return fmt.Errorf("graphql errors: %s", raw)
	}
	return nil
}

// ---------- pollpeer ----------

// cmdPollpeer polls the peers query on url every interval until the named host's
// staleSince is set (--until stale) or cleared with a live lastSeenAt (--until
// live), printing the elapsed milliseconds. It is how the script times the two-box
// stale-marking and recovery transitions (F8).
func cmdPollpeer(args []string) error {
	f := flags(args)
	url := f["--url"]
	host := f["--host"]
	until := f["--until"]
	if url == "" || host == "" || (until != "stale" && until != "live") {
		return fmt.Errorf("pollpeer: --url, --host, --until stale|live required")
	}
	interval := time.Duration(atoiOr(f["--interval-ms"], 200)) * time.Millisecond
	timeout := time.Duration(atoiOr(f["--timeout-s"], 30)) * time.Second

	// Optional raw poll log: one timestamped line per sample, so the results doc can
	// embed the actual kill->stale / restart->recovered trace (AC-SPIKE-PEER-STALE).
	var logw *os.File
	if lf := f["--log"]; lf != "" {
		var err error
		logw, err = os.OpenFile(lf, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // script-provided path
		if err != nil {
			return err
		}
		defer func() { _ = logw.Close() }()
	}

	client := &http.Client{Timeout: 3 * time.Second}
	q, _ := json.Marshal(map[string]any{"query": `{ peers { hostId lastSeenAt staleSince remoteMaxPluginLagSeconds } }`})
	start := time.Now()
	deadline := start.Add(timeout)
	for {
		st, seen, lag, ok := peerState(client, url, q, host)
		if logw != nil {
			_, _ = fmt.Fprintf(logw, "%s host=%s until=%s elapsed_ms=%d staleSince=%q lastSeenAt=%q lag=%g\n",
				time.Now().UTC().Format(time.RFC3339Nano), host, until,
				time.Since(start).Milliseconds(), st, seen, lag)
		}
		if ok {
			switch until {
			case "stale":
				if st != "" {
					fmt.Printf("POLLPEER host=%s until=stale elapsed_ms=%d staleSince=%s\n",
						host, time.Since(start).Milliseconds(), st)
					return nil
				}
			case "live":
				if st == "" && seen != "" {
					fmt.Printf("POLLPEER host=%s until=live elapsed_ms=%d lastSeenAt=%s lag=%g\n",
						host, time.Since(start).Milliseconds(), seen, lag)
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pollpeer: host %s did not reach %q within %s (staleSince=%q lastSeenAt=%q)",
				host, until, timeout, st, seen)
		}
		time.Sleep(interval)
	}
}

// ---------- peercheck ----------

// cmdPeercheck asserts F8's "no peer-of-peer rows" invariant on consumer A: A's own
// `peers` list must contain only its directly-configured peers (never a peer's peer,
// which would appear if A ingested B's peer set), and A's mirror table (peer_nodes)
// must hold no row whose origin `@host` key or peer_host is a third host. It prints a
// single machine line ending in verdict=PASS|FAIL and always exits 0 so the script's
// SUMMARY can record the result regardless.
func cmdPeercheck(args []string) error {
	f := flags(args)
	url := f["--url"]
	allow := f["--allow"] // the single directly-configured peer hostId (e.g. boxB)
	dbPath := f["--db"]   // A's peer.db, to grep peer_nodes for third-host rows
	if url == "" || allow == "" {
		return fmt.Errorf("peercheck: --url and --allow required")
	}

	client := &http.Client{Timeout: 3 * time.Second}
	q, _ := json.Marshal(map[string]any{"query": `{ peers { hostId mirroredKeys } }`})
	resp, err := client.Post(url, "application/json", bytes.NewReader(q))
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var r struct {
		Data struct {
			Peers []struct {
				HostID       string `json:"hostId"`
				MirroredKeys int    `json:"mirroredKeys"`
			} `json:"peers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("peercheck: decode peers: %w: %s", err, raw)
	}

	hosts := make([]string, 0, len(r.Data.Peers))
	mirrored := 0
	foreignPeer := 0
	for _, p := range r.Data.Peers {
		hosts = append(hosts, p.HostID)
		mirrored += p.MirroredKeys
		if p.HostID != allow {
			foreignPeer++
		}
	}

	thirdHostRows, err := peerNodesThirdHost(dbPath, allow)
	if err != nil {
		return fmt.Errorf("peercheck: %w", err)
	}

	verdict := "PASS"
	if foreignPeer > 0 || thirdHostRows > 0 {
		verdict = "FAIL"
	}
	fmt.Printf("PEERCHECK allow=%s peer_hosts=%s mirrored_rows=%d third_host_rows=%d verdict=%s\n",
		allow, strings.Join(hosts, ","), mirrored, thirdHostRows, verdict)
	return nil
}

// peerNodesThirdHost counts rows in A's peer_nodes mirror whose peer_host or origin
// `@host` key suffix is not the single allowed direct peer. A missing db (nothing
// mirrored yet) counts as zero — the invariant holds vacuously.
func peerNodesThirdHost(dbPath, allow string) (int, error) {
	if dbPath == "" {
		return 0, nil
	}
	if _, err := os.Stat(dbPath); err != nil {
		return 0, nil //nolint:nilerr // no db => nothing mirrored => zero foreign rows
	}
	db, err := openDB(dbPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT key, peer_host FROM peer_nodes`)
	if err != nil {
		return 0, nil //nolint:nilerr // table absent => nothing mirrored
	}
	defer func() { _ = rows.Close() }()
	third := 0
	for rows.Next() {
		var key, peerHost string
		if err := rows.Scan(&key, &peerHost); err != nil {
			return 0, err
		}
		origin := peerHost
		if i := strings.LastIndexByte(key, '@'); i >= 0 {
			origin = key[i+1:]
		}
		if peerHost != allow || origin != allow {
			third++
		}
	}
	return third, rows.Err()
}

func peerState(client *http.Client, url string, q []byte, host string) (stale, seen string, lag float64, ok bool) {
	resp, err := client.Post(url, "application/json", bytes.NewReader(q))
	if err != nil {
		return "", "", 0, false
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var r struct {
		Data struct {
			Peers []struct {
				HostID     string  `json:"hostId"`
				LastSeenAt *string `json:"lastSeenAt"`
				StaleSince *string `json:"staleSince"`
				Lag        float64 `json:"remoteMaxPluginLagSeconds"`
			} `json:"peers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", "", 0, false
	}
	for _, p := range r.Data.Peers {
		if p.HostID != host {
			continue
		}
		s, ls := "", ""
		if p.StaleSince != nil {
			s = *p.StaleSince
		}
		if p.LastSeenAt != nil {
			ls = *p.LastSeenAt
		}
		return s, ls, p.Lag, true
	}
	return "", "", 0, false
}

// ---------- percentile helpers ----------

func pct(durs []time.Duration, p float64) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	s := append([]time.Duration(nil), durs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(float64(len(s))*p+0.9999999) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func minD(durs []time.Duration) time.Duration {
	if len(durs) == 0 {
		return 0
	}
	m := durs[0]
	for _, d := range durs {
		if d < m {
			m = d
		}
	}
	return m
}

func maxD(durs []time.Duration) time.Duration {
	m := time.Duration(0)
	for _, d := range durs {
		if d > m {
			m = d
		}
	}
	return m
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }
