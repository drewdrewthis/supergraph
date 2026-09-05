package features

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// registerPeerSteps wires every Given/When/Then in features/peer.feature. Steps are
// black-box: HTTP against the running consumer and remote `supergraph serve`
// subprocesses (the remote runs the harness-only fakeremote source executor) plus
// direct reads of the consumer's peer.db — never an import of plugins/peer, so these
// steps survive an internal refactor of the plugin.
//
// @live @pending scenarios return godog.ErrPending from their first step; godog
// then skips the rest, but strict mode still requires every step TEXT to resolve to
// a registered definition, so each live phrase is registered too.
func registerPeerSteps(sc *godog.ScenarioContext) {
	pw := &peerWorld{}

	sc.Before(func(ctx context.Context, s *godog.Scenario) (context.Context, error) {
		if !hasTag(s, "@peer") || hasTag(s, "@pending") {
			return ctx, nil
		}
		return ctx, pw.init()
	})
	sc.After(func(ctx context.Context, s *godog.Scenario, _ error) (context.Context, error) {
		if hasTag(s, "@peer") && !hasTag(s, "@pending") {
			pw.cleanup()
		}
		return ctx, nil
	})

	// ---------- Given: remotes ----------
	sc.Step(lit("a remote box seeded with free-slot rows for `boxB` is booted on loopback"), pw.givenRemoteFreeSlots)
	sc.Step(lit("a remote box seeded with `issue:o/r#5@boxB` is booted on loopback"), pw.givenRemoteIssue)
	sc.Step(lit("a remote box seeded with tmux and claude rows for `boxB`, plus a foreign `tmux:s1@boxC` row, is booted on loopback"), pw.givenRemoteTmuxClaudeForeign)
	sc.Step(lit("a remote box seeded with a `tmux:s1@boxB` row and a foreign `tmux:s1@boxC` row is booted on loopback"), pw.givenRemoteTmuxForeign)
	sc.Step(lit("a remote box seeded with `issue:o/r#5@boxB` is booted on a non-loopback mesh bind requiring token `s3cret` for `boxB`"), pw.givenRemoteMeshAuth)

	// ---------- Given: consumers ----------
	sc.Step(lit("a consumer box with a peer plugin pointed at `boxB` is booted"), pw.givenConsumer)
	sc.Step(lit("a consumer box with a peer plugin pointed at `boxB` and a 2s max backoff is booted"), pw.givenConsumerBackoff2)
	sc.Step(lit("a consumer box with a peer plugin pointed at `boxB` with no bearer token is booted"), pw.givenConsumerNoToken)
	sc.Step(lit("a consumer box with a peer plugin pointed at `boxB`, whose remote is not running, is booted"), pw.givenConsumerNoRemote)

	// ---------- Given: warm / direct ----------
	sc.Step(lit("the consumer has warmed its mirror of `boxB` once"), pw.givenWarmed)
	sc.Step(lit("a direct query to the remote returns both the `boxB` row and the foreign `boxC` row"), pw.givenDirectOffersBoth)

	// ---------- When ----------
	sc.Step(lit("the consumer runs the warm `freeSlots` read against `boxB` 20 times"), pw.whenWarm20)
	sc.Step(lit("the consumer proxies the `issue` op to `boxB`"), pw.whenProxyIssue)
	sc.Step(lit("the consumer mirrors the remote's tmux data for `boxB`"), pw.whenMirrorTmux)
	sc.Step(lit("the consumer proxies the `issue` op to an unconfigured host `boxZ`"), pw.whenProxyUnknown)
	sc.Step(lit("the consumer proxies the `issue` op to `boxB` with no Authorization"), pw.whenProxyIssue)
	sc.Step(lit("the consumer is reconfigured with a wrong bearer token and proxies again"), pw.whenReconfigWrong)
	sc.Step(lit("the consumer is reconfigured with the correct token `s3cret` and proxies again"), pw.whenReconfigCorrect)
	sc.Step(lit("the remote box is stopped"), pw.whenStopRemote)
	sc.Step(lit("the remote box is restarted on the same port"), pw.whenRestartRemote)
	sc.Step(lit("I check the tree"), noop)
	sc.Step(lit("`make loc-peer` counts prod lines excluding tests"), pw.whenLocPeer)

	// ---------- Then ----------
	sc.Step(lit("every warm read is served from the local mirror with zero new calls to the remote"), pw.thenWarmZeroRemote)
	sc.Step(lit("the p95 of the 20 warm-read latencies is under 1s"), pw.thenP95Under1s)
	sc.Step(lit("within 30s the consumer's `peers` shows `boxB` with a non-null `staleSince`"), pw.thenStaleWithin30s)
	sc.Step(lit("the consumer's warm read of `boxB` still returns its tmux and claude rows, flagged stale, never omitted"), pw.thenStaleRowsStillServed)
	sc.Step(lit("no mirrored row for `boxB` carries a hostId other than `boxB`"), pw.thenNoForeignRows)
	sc.Step(lit("the served node is tagged host `boxB` and carries a `lastSeenAt`"), pw.thenServedTagged)
	sc.Step(lit("a row keyed `issue:o/r#5@boxB` is present in the consumer's `peer_nodes` store"), pw.thenIssueRowPresent)
	sc.Step(lit("the served node body equals the remote's seeded node body"), pw.thenServedBodyEquals)
	sc.Step(lit("a `peer.node.mirrored` envelope keyed `issue:o/r#5@boxB` is recorded by the consumer"), pw.thenMirroredEnvelope)
	sc.Step(lit(`the "peer" entry in the consumer's `+"`/health`"+` shows a non-null `+"`lastEventAt`"), pw.thenPeerHealthAdvanced)
	sc.Step(lit("the consumer stores the `boxB` row and drops the foreign `boxC` row"), pw.thenKeepsBoxBDropsBoxC)
	sc.Step(lit("no row keyed with hostId `boxC` is present in the consumer's `peer_nodes`"), pw.thenNoBoxC)
	sc.Step(lit("the remote box's own peer executor was never invoked"), pw.thenRemotePeerNeverInvoked)
	sc.Step(lit("the served result carries no nodes"), pw.thenServedEmpty)
	sc.Step(lit("the remote received no request for the unknown host"), pw.thenNoRemoteRequest)
	sc.Step(lit("the remote responds 401 and the consumer marks `boxB` unreachable with a non-null `staleSince`"), pw.thenAuthFailedStale)
	sc.Step(lit("no row for `boxB` is stored in the consumer's `peer_nodes`"), pw.thenNoBoxBRows)
	sc.Step(lit("the remote responds 401 and still nothing is mirrored for `boxB`"), pw.thenNoBoxBRows)
	sc.Step(lit("the proxy succeeds and at least one row for `boxB` is cached"), pw.thenBoxBCached)
	sc.Step(lit("within 4s the consumer's `peers` shows `boxB` with a null `staleSince` again"), pw.thenStaleClearedWithin4s)
	sc.Step(lit("a fresh proxy to `boxB` refreshes the mirror"), pw.thenFreshProxyRefreshes)
	sc.Step(lit("`GET /health` on the consumer returns HTTP 200 with a \"peer\" entry present"), pw.thenHealthHasPeer)
	sc.Step(lit(`the consumer's "peer" entry has a null `+"`lastEventAt`"+`, because nothing has been mirrored and no synthetic heartbeat is emitted`), pw.thenPeerLastEventNull)
	sc.Step(lit("within a short wait the consumer's `peers` shows `boxB` with a non-null `staleSince`, derived only from the failed connection attempt"), pw.thenStaleWithin30s)

	// ---------- @integration ----------
	sc.Step(lit("the peer plugin exists under `plugins/peer/`, registered via `graph/plugins_import.go` and regenerated `graph/`"), noop)
	sc.Step(lit("`git diff --stat origin/main -- core server` reports no files changed"), pw.thenZeroCoreDiff)
	sc.Step(lit("no file under core/ imports a plugin package"), pw.thenNoCoreImports)
	sc.Step(lit("the peer plugin source under `plugins/peer/`"), noop)
	sc.Step(lit("the count is at most 680"), pw.thenLocUnderCap)

	registerPeerLiveSteps(sc)
}

// ---------- Given handlers ----------

const issueBody = `{"number":5,"title":"issue 5"}`

func (pw *peerWorld) givenRemoteFreeSlots() error {
	pw.seeds = []seedNode{
		{"slot:1@boxB", `{"pane":"1","free":true}`},
		{"slot:2@boxB", `{"pane":"2","free":true}`},
		{"slot:3@boxB", `{"pane":"3","free":false}`},
	}
	return pw.startRemote()
}

func (pw *peerWorld) givenRemoteIssue() error {
	pw.seeds = []seedNode{{"issue:o/r#5@boxB", issueBody}}
	return pw.startRemote()
}

func (pw *peerWorld) givenRemoteTmuxClaudeForeign() error {
	pw.seeds = []seedNode{
		{"tmux:s1@boxB", `{"session":"s1"}`},
		{"claude:c1@boxB", `{"agent":"c1"}`},
		{"tmux:s1@boxC", `{"session":"s1","foreign":true}`},
	}
	return pw.startRemote()
}

func (pw *peerWorld) givenRemoteTmuxForeign() error {
	pw.seeds = []seedNode{
		{"tmux:s1@boxB", `{"session":"s1"}`},
		{"tmux:s1@boxC", `{"session":"s1","foreign":true}`},
	}
	// Give the remote its OWN peer plugin pointed at a dead boxC so a recursive hop
	// is structurally possible: the AC-PEER-LOOP assertion "the remote's peer executor
	// was never invoked" is now a real negative control, not a vacuous truth (M1).
	pw.remotePeerBoxC = "http://" + freePort() // free loopback port, nothing listening
	return pw.startRemote()
}

func (pw *peerWorld) givenRemoteMeshAuth() error {
	pw.seeds = []seedNode{{"issue:o/r#5@boxB", issueBody}}
	// Rebind on all interfaces (non-loopback) so core mandates + enforces a token.
	_, port, _ := strings.Cut(pw.remoteHost, ":")
	pw.remoteBind = ":" + port
	pw.remoteTok = "s3cret"
	return pw.startRemote()
}

func (pw *peerWorld) givenConsumer() error         { return pw.startConsumer() }
func (pw *peerWorld) givenConsumerBackoff2() error { pw.backoffMax = 2; return pw.startConsumer() }
func (pw *peerWorld) givenConsumerNoToken() error  { pw.consumerTok = ""; return pw.startConsumer() }

// givenConsumerNoRemote boots the consumer pointed at a remote address nothing is
// listening on (startRemote was never called), so the liveness dial is refused.
func (pw *peerWorld) givenConsumerNoRemote() error { return pw.startConsumer() }

func (pw *peerWorld) givenWarmed() error {
	// The op name ("warm") is irrelevant here: the mirror cache is HOST-scoped, so a
	// refresh of any op for boxB fills the same peer_nodes rows a later freeSlots warm
	// read serves (S5). With remotePlugin=fakeremote the op is not routed per-op anyway.
	sw, err := pw.postExecutor("warm", "boxB", true)
	if err != nil {
		return err
	}
	if len(sw.Nodes) == 0 {
		return fmt.Errorf("warm proxy cached no rows: %+v", sw)
	}
	return nil
}

func (pw *peerWorld) givenDirectOffersBoth() error {
	m, err := pw.directRemote("tmux")
	if err != nil {
		return err
	}
	pw.directRC = m
	if _, ok := m["tmux:s1@boxB"]; !ok {
		return fmt.Errorf("remote did not offer tmux:s1@boxB: %v keys", keysOf(m))
	}
	if _, ok := m["tmux:s1@boxC"]; !ok {
		return fmt.Errorf("remote did not offer the foreign tmux:s1@boxC: %v keys", keysOf(m))
	}
	return nil
}

// ---------- When handlers ----------

func (pw *peerWorld) whenWarm20() error {
	pw.remoteBaseline = strings.Count(pw.remoteLog(), "fakeremote: served")
	pw.samples = nil
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		sw, err := pw.postExecutor("freeSlots", "boxB", false)
		if err != nil {
			return err
		}
		pw.samples = append(pw.samples, time.Since(t0))
		if len(sw.Nodes) == 0 {
			return fmt.Errorf("warm read %d served no rows", i)
		}
	}
	return nil
}

func (pw *peerWorld) whenProxyIssue() error {
	sw, err := pw.postExecutor("issue", "boxB", true)
	pw.served = sw
	return err
}

func (pw *peerWorld) whenMirrorTmux() error {
	sw, err := pw.postExecutor("tmux", "boxB", true)
	pw.served = sw
	return err
}

func (pw *peerWorld) whenProxyUnknown() error {
	pw.remoteBaseline = strings.Count(pw.remoteLog(), "fakeremote: served")
	sw, err := pw.postExecutor("issue", "boxZ", true)
	pw.served = sw
	return err
}

func (pw *peerWorld) whenReconfigWrong() error {
	if err := pw.restartConsumer("wrong-token"); err != nil {
		return err
	}
	sw, err := pw.postExecutor("issue", "boxB", true)
	pw.served = sw
	return err
}

func (pw *peerWorld) whenReconfigCorrect() error {
	if err := pw.restartConsumer("s3cret"); err != nil {
		return err
	}
	sw, err := pw.postExecutor("issue", "boxB", true)
	pw.served = sw
	return err
}

func (pw *peerWorld) whenStopRemote() error    { pw.stopRemote(); return nil }
func (pw *peerWorld) whenRestartRemote() error { return pw.startRemote() }

func (pw *peerWorld) whenLocPeer() error {
	cmd := exec.Command("make", "-C", repoRoot, "loc-peer")
	var so bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &so
	err := cmd.Run()
	pw.locOut = so.String()
	if err != nil {
		return fmt.Errorf("make loc-peer failed: %v\n%s", err, pw.locOut)
	}
	return nil
}

// ---------- Then handlers ----------

func (pw *peerWorld) thenWarmZeroRemote() error {
	after := strings.Count(pw.remoteLog(), "fakeremote: served")
	if after != pw.remoteBaseline {
		return fmt.Errorf("warm reads hit the remote: served-count %d → %d", pw.remoteBaseline, after)
	}
	return nil
}

func (pw *peerWorld) thenP95Under1s() error {
	if p := p95(pw.samples); p >= time.Second {
		return fmt.Errorf("p95 = %s, want < 1s", p)
	}
	return nil
}

func (pw *peerWorld) thenStaleWithin30s() error {
	return pw.waitPeerStale("boxB", 30*time.Second, true)
}

func (pw *peerWorld) thenStaleRowsStillServed() error {
	sw, err := pw.postExecutor("tmux", "boxB", false)
	if err != nil {
		return err
	}
	if sw.StaleSince == nil {
		return fmt.Errorf("served rows not flagged stale: %+v", sw)
	}
	var hasTmux, hasClaude bool
	for _, n := range sw.Nodes {
		if strings.HasPrefix(n.Key, "tmux:") {
			hasTmux = true
		}
		if strings.HasPrefix(n.Key, "claude:") {
			hasClaude = true
		}
	}
	if !hasTmux || !hasClaude {
		return fmt.Errorf("stale read omitted rows (tmux=%v claude=%v): %+v", hasTmux, hasClaude, sw)
	}
	return nil
}

func (pw *peerWorld) thenNoForeignRows() error {
	keys, err := pw.peerNodeKeys()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, "@boxB") {
			return fmt.Errorf("mirrored row with non-boxB host: %q (all=%v)", k, keys)
		}
	}
	return nil
}

func (pw *peerWorld) thenServedTagged() error {
	if len(pw.served.Nodes) == 0 {
		return fmt.Errorf("no served nodes")
	}
	n := pw.served.Nodes[0]
	if n.Host != "boxB" {
		return fmt.Errorf("served host = %q want boxB", n.Host)
	}
	if n.LastSeenAt.IsZero() {
		return fmt.Errorf("served node has no lastSeenAt")
	}
	return nil
}

func (pw *peerWorld) thenIssueRowPresent() error {
	_, ok, err := pw.peerNodeBody("issue:o/r#5@boxB")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("issue:o/r#5@boxB not present in peer_nodes")
	}
	return nil
}

func (pw *peerWorld) thenServedBodyEquals() error {
	body, ok, err := pw.peerNodeBody("issue:o/r#5@boxB")
	if err != nil || !ok {
		return fmt.Errorf("stored body missing: ok=%v err=%v", ok, err)
	}
	if !jsonEqual(body, []byte(issueBody)) {
		return fmt.Errorf("stored body %s != seeded %s", body, issueBody)
	}
	if len(pw.served.Nodes) == 0 || !jsonEqual(pw.served.Nodes[0].Node, []byte(issueBody)) {
		return fmt.Errorf("served body != seeded: %+v", pw.served.Nodes)
	}
	return nil
}

func (pw *peerWorld) thenMirroredEnvelope() error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		n, err := pw.peerEventCount("peer.node.mirrored", "issue:o/r#5@boxB")
		if err == nil && n > 0 {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no peer.node.mirrored envelope for issue:o/r#5@boxB")
}

func (pw *peerWorld) thenPeerHealthAdvanced() error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, err := pw.cw.getHealth()
		if err == nil {
			if r := findRow(rows, "peer"); r != nil && r["lastEventAt"] != nil {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("peer /health lastEventAt did not advance")
}

func (pw *peerWorld) thenKeepsBoxBDropsBoxC() error {
	keys, err := pw.peerNodeKeys()
	if err != nil {
		return err
	}
	if !contains(keys, "tmux:s1@boxB") {
		return fmt.Errorf("boxB row not stored: %v", keys)
	}
	if contains(keys, "tmux:s1@boxC") {
		return fmt.Errorf("foreign boxC row was stored: %v", keys)
	}
	return nil
}

func (pw *peerWorld) thenNoBoxC() error {
	keys, err := pw.peerNodeKeys()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if strings.HasSuffix(k, "@boxC") {
			return fmt.Errorf("peer-of-peer row stored: %q", k)
		}
	}
	return nil
}

func (pw *peerWorld) thenRemotePeerNeverInvoked() error {
	if strings.Contains(pw.remoteLog(), "peer: executor") {
		return fmt.Errorf("the remote's own peer executor was invoked:\n%s", pw.remoteLog())
	}
	// The consumer DID run its drop path — proof the foreign row reached it and was
	// dropped rather than never offered.
	if !strings.Contains(pw.consumerLog(), "dropped foreign-host row") {
		return fmt.Errorf("consumer never logged a foreign-row drop:\n%s", pw.consumerLog())
	}
	return nil
}

func (pw *peerWorld) thenServedEmpty() error {
	if len(pw.served.Nodes) != 0 {
		return fmt.Errorf("unknown-host proxy returned nodes: %+v", pw.served)
	}
	return nil
}

func (pw *peerWorld) thenNoRemoteRequest() error {
	after := strings.Count(pw.remoteLog(), "fakeremote: served")
	if after != pw.remoteBaseline {
		return fmt.Errorf("unknown-host proxy made an outbound hop: served-count %d → %d", pw.remoteBaseline, after)
	}
	return nil
}

func (pw *peerWorld) thenAuthFailedStale() error {
	if err := pw.waitPeerStale("boxB", 10*time.Second, true); err != nil {
		return err
	}
	return pw.thenNoBoxBRows()
}

func (pw *peerWorld) thenNoBoxBRows() error {
	keys, err := pw.peerNodeKeys()
	if err != nil {
		return err
	}
	for _, k := range keys {
		if strings.HasSuffix(k, "@boxB") {
			return fmt.Errorf("a boxB row was mirrored despite auth failure: %q", k)
		}
	}
	return nil
}

func (pw *peerWorld) thenBoxBCached() error {
	if len(pw.served.Nodes) == 0 {
		return fmt.Errorf("proxy returned no nodes after correct token")
	}
	keys, err := pw.peerNodeKeys()
	if err != nil {
		return err
	}
	if !contains(keys, "issue:o/r#5@boxB") {
		return fmt.Errorf("no boxB row cached after correct token: %v", keys)
	}
	return nil
}

func (pw *peerWorld) thenStaleClearedWithin4s() error {
	return pw.waitPeerStale("boxB", 4*time.Second, false)
}

func (pw *peerWorld) thenFreshProxyRefreshes() error {
	sw, err := pw.postExecutor("issue", "boxB", true)
	if err != nil {
		return err
	}
	if len(sw.Nodes) == 0 {
		return fmt.Errorf("fresh proxy did not refresh the mirror: %+v", sw)
	}
	return nil
}

func (pw *peerWorld) thenHealthHasPeer() error {
	rows, _, err := pw.cw.getHealth()
	if err != nil {
		return err
	}
	if findRow(rows, "peer") == nil {
		return fmt.Errorf("no peer entry in /health: %v", rows)
	}
	return nil
}

func (pw *peerWorld) thenPeerLastEventNull() error {
	rows, _, err := pw.cw.getHealth()
	if err != nil {
		return err
	}
	r := findRow(rows, "peer")
	if r == nil {
		return fmt.Errorf("no peer entry in /health")
	}
	if r["lastEventAt"] != nil {
		return fmt.Errorf("peer lastEventAt is not null (synthetic heartbeat?): %v", r["lastEventAt"])
	}
	return nil
}

// ---------- @integration ----------

func (pw *peerWorld) thenZeroCoreDiff() error {
	cmd := exec.Command("git", "diff", "--stat", "origin/main", "--", "core", "server")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git diff failed (is origin/main fetched?): %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("core/server changed vs origin/main:\n%s", out)
	}
	return nil
}

func (pw *peerWorld) thenNoCoreImports() error {
	cmd := exec.Command("grep", "-rl", "drewdrewthis/supergraph/plugins", "core/")
	cmd.Dir = repoRoot
	out, _ := cmd.CombinedOutput() // grep exits 1 with no matches
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("core files import a plugin package:\n%s", out)
	}
	return nil
}

var locCountRe = regexp.MustCompile(`prod LOC:\s*(\d+)`)

func (pw *peerWorld) thenLocUnderCap() error {
	m := locCountRe.FindStringSubmatch(pw.locOut)
	if m == nil {
		return fmt.Errorf("could not parse loc-peer output:\n%s", pw.locOut)
	}
	n, _ := strconv.Atoi(m[1])
	if n > 680 {
		return fmt.Errorf("peer prod LOC %d exceeds 680", n)
	}
	return nil
}

// ---------- @live @pending ----------

func registerPeerLiveSteps(sc *godog.ScenarioContext) {
	pending := func() error { return godog.ErrPending }
	sc.Step(lit("two real boxes are running on the mesh with a shared bearer token"), pending)
	sc.Step(lit("two real boxes are running on the mesh, each with its own bearer token"), pending)
	sc.Step(lit("a warmed cross-box `freeSlots` query runs 20 times against the peer box"), pending)
	sc.Step(lit("one box is stopped"), pending)
	sc.Step(lit("the consumer proxies with a wrong bearer token"), pending)
}

// ---------- misc ----------

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ca, _ := json.Marshal(x)
	cb, _ := json.Marshal(y)
	return bytes.Equal(ca, cb)
}

func keysOf(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
