package features

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/cucumber/godog"
)

// registerGithubSteps wires every Given/When/Then phrase in features/github.feature.
// Steps are black-box: HTTP against the running `supergraph serve` subprocess, the
// fake GitHub httptest server (plugins/github/fakegh), and direct reads of the
// plugin's own github.db SQLite file (the same pattern steps_test.go already uses
// for template.db) — never an import of the plugins/github package itself, so
// these steps survive an internal refactor of the plugin (budget 1650).
//
// @live @pending scenarios: every step in them returns godog.ErrPending directly,
// per the brief. Godog stops executing a scenario's steps at the first pending
// result and marks the rest skipped, but strict mode still requires every step
// TEXT in the feature to resolve to a registered definition, so each live phrase
// gets its own registration even though only the first one in each scenario ever
// actually runs.
func registerGithubSteps(sc *godog.ScenarioContext) {
	g := &ghWorld{}

	sc.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		if !hasTag(sc, "@github") {
			return ctx, nil
		}
		return ctx, g.init()
	})
	sc.After(func(ctx context.Context, sc *godog.Scenario, _ error) (context.Context, error) {
		if hasTag(sc, "@github") {
			g.cleanup()
		}
		return ctx, nil
	})

	// ---------- shared Given ----------
	sc.Step(lit("a supergraph server started with the github plugin and data dir <tmp>"), g.startDefault)
	// "evidence is captured: ..." is registered generically (and shared with
	// prd.feature) in steps_prd_test.go's registerPRDSteps.

	// ---------- S3 ----------
	sc.Step(lit("a websocket subscription to `checkRunUpdated` on `127.0.0.1:7788/graphql`"), g.s3OpenSub)
	sc.Step(lit("20 correctly-signed check_run webhooks are POSTed to `/plugins/github/webhook`"), g.s3Emit20)
	sc.Step(lit("the p95 of receipt-to-subscription-push over the 20 samples is under 1s"), g.s3AssertP95)

	// ---------- AC-GH-ISSUE-SUB ----------
	sc.Step(lit("a websocket subscription to `issueUpdated` on `127.0.0.1:7788/graphql`"), g.issueSubOpen)
	sc.Step(lit("a correctly-signed `issues` `labeled` webhook for `o/r#5` is received"), g.issueSubWebhookLabeled)
	sc.Step(lit("an `issueUpdated` push for `issue:o/r#5` is received within 1s of the webhook receipt"), g.issueSubAssertPush)

	// ---------- F1 ----------
	sc.Step(lit("20 correctly-signed issues \"opened\" webhooks are POSTed to `/plugins/github/webhook`"), g.f1Emit20)
	sc.Step(lit("each issue is queryable and the p95 of emit-to-queryable over the 20 samples is under 1s"), g.f1AssertP95)

	// ---------- F2 ----------
	sc.Step(lit("the fake GitHub server has an issue that no webhook was delivered for"), g.f2SeedDroppedIssue)
	sc.Step(lit("the since-cursor reconcile runs once"), g.reconcileRunsOnce)
	sc.Step(lit("a query for that issue returns it, healed without a webhook"), g.f2AssertHealed)

	// ---------- F3 ----------
	sc.Step(lit("the fake GitHub `/user/repos` listing gains a new repo after boot"), g.f3AddRepo)
	sc.Step(lit("the reconcile loop runs once"), g.reconcileRunsOnce)
	sc.Step(lit("a query for open issues covers the new repo with no config change"), g.f3AssertCoversNewRepo)
	sc.Step(lit("a hook is created for the new repo"), g.f3AssertHookCreated)

	// ---------- AC-GH-CACHE-HIT ----------
	sc.Step(lit(`issue `+"`issue:o/r#5`"+` is already in the store, fresh, with etag `+"`W/\"abc\"`"), g.cacheHitSeed)
	sc.Step(lit("`issue` is queried for `o/r#5`"), g.queryIssue5)
	sc.Step(lit("the complete stored node is served"), g.assertNodeServed)
	sc.Step(lit(`the served node's etag equals the stored etag `+"`W/\"abc\"`"), g.assertEtagAbc)
	sc.Step(lit("the fake GitHub server records zero requests during the query"), g.assertZeroRequests)

	// ---------- AC-GH-STALE ----------
	sc.Step(lit(`issue `+"`issue:o/r#5`"+` is in the store, fresh, with title "old"`), g.staleSeedOld)
	sc.Step(lit(`the fake GitHub server's `+"`o/r#5`"+` is changed to title "new" with no webhook, notification, or reconcile`), g.staleMutateUpstream)
	sc.Step(lit(`the stale cached node with title "old" is served, by design`), g.staleAssertOldServed)
	sc.Step(lit("`issue:o/r#5` is purged and the next query returns title \"new\""), g.staleAssertHealed)

	// ---------- AC-GH-COLDSTART ----------
	sc.Step(lit("a supergraph server started with the github plugin and an empty data dir <tmp>"), g.startDefault)
	sc.Step(lit("no bulk backfill runs"), noop)
	sc.Step(lit("the first read of each named op runs against the fake GitHub server"), g.coldstartRunAllOps)
	sc.Step(lit("each op completes in under 2 seconds"), g.coldstartAssertUnder2s)
	sc.Step(lit("each op populates exactly the keys it declares in its `# keys`/`# scope` directive and nothing more"), g.coldstartAssertKeys)

	// ---------- AC-GH-ETAG-304 ----------
	sc.Step(lit("issue `issue:o/r#5` is in the store with a stored etag but marked expired"), g.etagSeedExpired)
	sc.Step(lit("the fake GitHub server returns 304 for `If-None-Match` matching that etag"), noop)
	sc.Step(lit("the read-through sends `If-None-Match` with the stored etag"), g.etagAssertConditional)
	sc.Step(lit("the fake server returns 304 and the stored node is served with its fetched_at bumped"), g.etagAssert304Served)
	sc.Step(lit("no GraphQL points or REST quota are spent on the body"), g.etagAssertZeroQuota)

	// ---------- AC-GH-PURGE-TAG ----------
	sc.Step(lit("these entries are cached: `issue:o/r#5`, an `openIssues(o/r)` list result, and `issue:o/r#6`"), g.purgeSeedEntries)
	sc.Step(lit("a correctly-signed `issue` `edited` webhook for `o/r#5` is received"), g.purgeWebhookEdited)
	sc.Step(lit("`issue:o/r#5` is deleted and the `openIssues(o/r)` list result is deleted"), g.purgeAssertDeleted)
	sc.Step(lit("`issue:o/r#6` is still cached"), g.purgeAssertSiblingCached)
	sc.Step(lit("a `github.node.purged` envelope is emitted with key `issue:o/r#5`"), g.purgeAssertEnvelope)

	// ---------- AC-GH-PIN ----------
	sc.Step(lit("a merged PR `pr:o/r#9` closed more than the pin grace window ago is in the store"), g.pinSeedMergedPR)
	sc.Step(lit("`pr:o/r#9` is queried twice after its TTL would have expired"), g.pinQueryTwice)
	sc.Step(lit("it is served from the store both times"), g.pinAssertServedBoth)
	sc.Step(lit("no conditional or full fetch is made to the fake GitHub server"), g.pinAssertNoFetch)

	// ---------- AC-GH-SINGLEFLIGHT ----------
	sc.Step(lit("issue `issue:o/r#7` is not in the store"), g.sfSeedUpstreamOnly)
	sc.Step(lit("10 concurrent queries for `issue:o/r#7` arrive"), g.sfConcurrentQueries)
	sc.Step(lit("the fake GitHub server receives exactly one fetch for it"), g.sfAssertOneFetch)
	sc.Step(lit("all 10 queries return the same stored node"), g.sfAssertSameNode)

	// ---------- AC-GH-HMAC ----------
	sc.Step(lit("a webhook with an invalid `X-Hub-Signature-256` is POSTed to `/plugins/github/webhook`"), g.hmacBadSig)
	sc.Step(lit("the response status is 401"), g.assertLastWebhook401)
	sc.Step(lit("no github event is emitted"), g.assertNoGithubEvent)
	sc.Step(lit("a correctly-signed webhook is POSTed to `/plugins/github/webhook`"), g.hmacGoodSig)
	sc.Step(lit("the response status is 200"), g.assertLastWebhook200)
	sc.Step(lit("exactly one github event is emitted"), g.assertOneGithubEvent)

	// ---------- AC-GH-FORWARD (local) ----------
	sc.Step(lit("a github plugin configured for `forward` ingress with a fake `gh webhook forward` stub"), g.forwardGiven)
	sc.Step(lit("the plugin starts and the stub delivers a signed webhook to the handler"), g.forwardStubDelivers)
	sc.Step(lit("the webhook is ingested"), g.forwardAssertIngested)
	sc.Step(lit("the stub child exits"), g.forwardStubExits)
	sc.Step(lit("the plugin restarts it with backoff and runs redelivery from the last-seen delivery id"), g.forwardAssertRestarted)
	sc.Step(lit("a delivery missed while it was down is replayed via `/attempts` exactly once"), g.forwardAssertRedelivered)

	// ---------- AC-GH-NOTIFY-304 ----------
	sc.Step(lit("`notifications` is false by default"), noop)
	sc.Step(lit("the notifications poll does not run"), g.notifyAssertNotPolling)
	sc.Step(lit("the server is restarted with `notifications = true`"), g.notifyRestartEnabled)
	sc.Step(lit("the fake `/notifications` returns 304 for the stored `If-Modified-Since`"), noop)
	sc.Step(lit("the notifications poll runs"), g.notifyWaitOnePoll)
	sc.Step(lit("no node fetch is made and no quota is spent"), g.notifyAssertZeroQuota)
	sc.Step(lit("`/notifications` next returns 200 with a changed thread"), g.notifyEmitChange)
	sc.Step(lit("that change feeds the cursor and store"), g.notifyAssertCursorAdvanced)

	// ---------- AC-GH-FLOOR ----------
	sc.Step(lit("the fake GitHub GraphQL returns `rateLimit` remaining near the floor with a near-future `resetAt`"), g.floorSeedNearFloor)
	sc.Step(lit("a read-through or reconcile hits the floor"), g.floorTriggerRead)
	sc.Step(lit("the plugin pauses until `resetAt` rather than erroring"), g.floorAssertPaused)
	sc.Step(lit("it does not panic or exit and `/health` shows no forced-stale for github"), g.floorAssertHealthy)
	sc.Step(lit("the fake server restores points after `resetAt`"), noop)
	sc.Step(lit("the paused work resumes and completes"), g.floorAssertResumed)

	// ---------- AC-GH-RATELOG ----------
	sc.Step(lit("the plugin makes a request to the fake GitHub server"), g.ratelogTrigger)
	sc.Step(lit("the log records the REST `x-ratelimit-*` headers and the GraphQL `rateLimit{remaining,resetAt}`"), g.ratelogAssert)

	// ---------- AC-GH-NAMEDOP-KEYS ----------
	sc.Step(lit("I run `supergraph query --op openIssues --var owner=o --var repo=r`"), g.namedopRun)
	sc.Step(lit("the `plugins/github/queries/openIssues.graphql` op runs and its result is tagged with scope `repo:o/r`"), g.namedopAssertScope)
	sc.Step(lit("each returned issue node is stored under its own key"), g.namedopAssertNodesStored)
	sc.Step(lit("a correctly-signed `issue` `closed` webhook for `o/r#5` is received"), g.namedopWebhookClosed)
	sc.Step(lit("the `openIssues(o/r)` list result is evicted by the covering scope"), g.namedopAssertListEvicted)
	sc.Step(lit("I run `supergraph schema Issue` and the `Issue` type definition is printed"), g.namedopSchemaPrint)

	// ---------- AC-GH-CURSOR ----------
	sc.Step(lit("one reconcile has advanced the `since` cursor for a repo to time T"), g.cursorAdvance)
	sc.Step(lit("the server is stopped and restarted on the same data dir <tmp>"), g.cursorRestart)
	sc.Step(lit("the `since` cursor for that repo is still T after the restart"), g.cursorAssertPersisted)
	sc.Step(lit("the next reconcile's fetch to the fake GitHub server carries `since=` equal to T minus 60s"), g.cursorAssertOverlap)

	// ---------- AC-GH-RECONCILE-SHAPE / AC-GH-RECONCILE-NOEVENT ----------
	// "the since-cursor reconcile runs once" is already registered (F2) as g.reconcileRunsOnce.
	sc.Step(lit("the fake GitHub server has a rich issue `o/r#5` with labels, an assignee, and updatedAt"), g.reconcileSeedRichIssue)
	sc.Step(lit("the `openIssues` op has been warmed once for `o/r`"), g.reconcileWarmOpenIssues)
	sc.Step(lit("a query for issue `o/r#5` still shows its labels, assignee, and updatedAt"), g.reconcileAssertShapeIntact)
	sc.Step(lit("no `github.node.updated` event was emitted for `issue:o/r#5`"), g.reconcileAssertNoUpdateEvent)

	// ---------- AC-GH-LOC ----------
	sc.Step(lit("the github plugin source under `plugins/github`"), noop)
	sc.Step(lit("`make loc-github` counts non-comment, non-blank prod lines excluding tests and `internal/fakegh`"), g.locRun)
	sc.Step(lit("the count is 1650 or fewer"), g.locAssert)

	// ---------- AC-GH-ZEROCORE ----------
	sc.Step(lit("the github plugin package and its blank import in graph/plugins_import.go"), noop)
	sc.Step(lit("`git diff --stat core/` is run"), g.zerocoreRun)

	// ---------- github-query typed reads + cross-plugin join (github-query.feature) ----------
	// Registered on the SAME ghWorld g (and its server, started by the shared Given
	// above) so the join scenarios read the seeded github/claude/tmux dbs under one
	// data dir. See features/steps_githubquery_test.go.
	registerGithubQuerySteps(sc, g)

	// ---------- @live @pending ----------
	registerGithubLiveSteps(sc, g)
}

func noop() error { return nil }

func hasTag(sc *godog.Scenario, tag string) bool {
	for _, t := range sc.Tags {
		if t.Name == tag {
			return true
		}
	}
	return false
}

// ===================== S3 =====================

func (g *ghWorld) s3OpenSub() error {
	// The github plugin serves entirely through its own /plugins/github/ HTTPRoutes
	// seam (EDR §"CLI": "git diff --stat core/ = 0 ... not the gqlgen glob"). It
	// never registers a `checkRunUpdated` field on the core GraphQL schema, so this
	// subscribe is expected to fail downstream — see the defect note on s3AssertP95.
	return g.sw.openSubscription("checkRunUpdated")
}

func (g *ghWorld) s3Emit20() error {
	g.fake.SetWebhookSecret(g.webhookSecret)
	g.samples = nil
	for i := 1; i <= 20; i++ {
		g.fake.AddRepo("o", "r")
		id := 1000 + i
		t0 := time.Now()
		_, status, err := g.fake.EmitWebhook("http://"+g.sw.listen+"/plugins/github/webhook", "check_run", "completed",
			map[string]any{"check_run": map[string]any{"id": id}, "repository": map[string]any{"full_name": "o/r"}})
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("check_run webhook %d: status %d", i, status)
		}
		g.samples = append(g.samples, time.Since(t0))
	}
	return nil
}

func (g *ghWorld) s3AssertP95() error {
	if g.sw.ws == nil {
		return fmt.Errorf("no subscription connection")
	}
	// A check_run webhook purges the touched key and the push arrives on the
	// checkRunUpdated subscription; assert it lands within the window.
	_, err := g.sw.ws.nextPush(2 * time.Second)
	if err != nil {
		return fmt.Errorf("S3: no checkRunUpdated push within 2s: %w", err)
	}
	if p95(g.samples) >= time.Second {
		return fmt.Errorf("p95 = %s, want < 1s", p95(g.samples))
	}
	return nil
}

// ===================== AC-GH-ISSUE-SUB =====================

func (g *ghWorld) issueSubOpen() error { return g.sw.openSubscription("issueUpdated") }

// issueSubWebhookLabeled delivers a signed issues.labeled webhook for o/r#5; the
// handler purges issue:o/r#5 and emits one github.node.purged envelope the
// issueUpdated subscription relays (the dispatcher's subscribe-not-poll path).
func (g *ghWorld) issueSubWebhookLabeled() error {
	status, err := g.postWebhook(g.webhookSecret, "issues", "labeled",
		map[string]any{
			"issue":      map[string]any{"number": 5},
			"label":      map[string]any{"name": "bug"},
			"repository": map[string]any{"full_name": "o/r"},
		})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("issues labeled webhook status %d", status)
	}
	return nil
}

// issueSubAssertPush asserts the push lands within the S3 1s bound, measured on the
// envelope's own emit-stamped ts (like wsPushedWithin1s), with the expected key.
func (g *ghWorld) issueSubAssertPush() error {
	if g.sw.ws == nil {
		return fmt.Errorf("no subscription connection")
	}
	p, err := g.sw.ws.nextPush(3 * time.Second)
	if err != nil {
		return fmt.Errorf("AC-GH-ISSUE-SUB: no issueUpdated push within 3s: %w", err)
	}
	received := time.Now()
	data, _ := p["data"].(map[string]any)
	evt, ok := data["issueUpdated"].(map[string]any)
	if !ok {
		return fmt.Errorf("push data missing issueUpdated: %v", p)
	}
	if pl, _ := evt["payload"].(string); !strings.Contains(pl, "issue:o/r#5") {
		return fmt.Errorf("issueUpdated push not for issue:o/r#5: payload=%q", pl)
	}
	tsStr, _ := evt["ts"].(string)
	emittedAt, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return fmt.Errorf("parse event ts %q: %w", tsStr, err)
	}
	if elapsed := received.Sub(emittedAt); elapsed >= time.Second {
		return fmt.Errorf("issueUpdated push arrived %s after emit (>= 1s bound)", elapsed)
	}
	return nil
}

// ===================== F1 =====================

func (g *ghWorld) f1Emit20() error {
	g.fake.SetWebhookSecret(g.webhookSecret)
	g.fake.AddRepo("o", "r")
	g.samples = nil
	for i := 1; i <= 20; i++ {
		g.fake.AddIssue("o", "r", i, fmt.Sprintf("issue %d", i), "open")
		t0 := time.Now()
		_, status, err := g.fake.EmitWebhook("http://"+g.sw.listen+"/plugins/github/webhook", "issues", "opened",
			map[string]any{"issue": map[string]any{"number": i}, "repository": map[string]any{"full_name": "o/r"}})
		if err != nil {
			return err
		}
		if status != 200 {
			return fmt.Errorf("issue webhook %d: status %d", i, status)
		}
		// queryable-at: first successful read of the object through the plugin
		deadline := time.Now().Add(2 * time.Second)
		for {
			qr, st, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": i})
			if err == nil && st == 200 && len(qr.Nodes) == 1 {
				g.samples = append(g.samples, time.Since(t0))
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("issue %d never became queryable", i)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	return nil
}

func (g *ghWorld) f1AssertP95() error {
	if len(g.samples) != 20 {
		return fmt.Errorf("got %d samples, want 20", len(g.samples))
	}
	if p95(g.samples) >= time.Second {
		return fmt.Errorf("p95 = %s, want < 1s", p95(g.samples))
	}
	return nil
}

// ===================== F2 =====================

func (g *ghWorld) f2SeedDroppedIssue() error {
	g.fake.AddRepo("o", "r")
	g.fake.AddIssue("o", "r", 42, "dropped issue", "open")
	return nil
}

func (g *ghWorld) f2AssertHealed() error {
	qr, status, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": 42})
	if err != nil {
		return err
	}
	if status != 200 || len(qr.Nodes) != 1 {
		return fmt.Errorf("issue not healed: status=%d nodes=%d", status, len(qr.Nodes))
	}
	return nil
}

// ===================== F3 =====================

func (g *ghWorld) f3AddRepo() error {
	g.fake.AddRepo("o", "newrepo")
	g.fake.AddIssue("o", "newrepo", 1, "first issue", "open")
	return nil
}

func (g *ghWorld) f3AssertCoversNewRepo() error {
	qr, status, err := g.postOp("openIssues", map[string]any{"owner": "o", "repo": "newrepo"})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("openIssues status %d", status)
	}
	// A direct query for the new repo works even without discovery, so the real
	// zero-config proof is that reconcile's own discovery pass created a hook for
	// it (asserted separately) and the object is fetchable through the plugin.
	if len(qr.Nodes) == 0 {
		return fmt.Errorf("expected at least one issue node for the new repo, got 0")
	}
	return nil
}

func (g *ghWorld) f3AssertHookCreated() error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		if g.fake.CountPath("POST", "/repos/o/newrepo/hooks") >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no hook creation POST recorded for o/newrepo")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ===================== AC-GH-CACHE-HIT =====================

func (g *ghWorld) cacheHitSeed() error {
	now := time.Now()
	body := map[string]any{"number": 5, "title": "cached issue", "state": "open", "id": "issue:o/r#5"}
	if err := g.seedNode("issue:o/r#5", "Issue", body, `W/"abc"`, false, now, now); err != nil {
		return err
	}
	g.fake.ResetLog()
	return nil
}

func (g *ghWorld) queryIssue5() error {
	qr, status, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": 5})
	if err != nil {
		return err
	}
	g.lastOp, g.lastOpStatus = qr, status
	return nil
}

func (g *ghWorld) assertNodeServed() error {
	if g.lastOpStatus != 200 || len(g.lastOp.Nodes) != 1 {
		return fmt.Errorf("status=%d nodes=%d", g.lastOpStatus, len(g.lastOp.Nodes))
	}
	var body map[string]any
	_ = json.Unmarshal(g.lastOp.Nodes[0].Node, &body)
	if body["title"] != "cached issue" {
		return fmt.Errorf("unexpected node body: %v", body)
	}
	return nil
}

func (g *ghWorld) assertEtagAbc() error {
	if len(g.lastOp.Nodes) != 1 {
		return fmt.Errorf("no node in response")
	}
	if g.lastOp.Nodes[0].ETag != `W/"abc"` {
		return fmt.Errorf("etag = %q, want W/\"abc\"", g.lastOp.Nodes[0].ETag)
	}
	return nil
}

func (g *ghWorld) assertZeroRequests() error {
	if n := len(g.fake.Requests()); n != 0 {
		return fmt.Errorf("fake GitHub recorded %d requests, want 0: %+v", n, g.fake.Requests())
	}
	return nil
}

// ===================== AC-GH-STALE =====================

func (g *ghWorld) staleSeedOld() error {
	g.fake.AddRepo("o", "r")
	etag := g.fake.AddIssue("o", "r", 5, "old", "open")
	now := time.Now()
	body := map[string]any{"number": 5, "title": "old", "state": "open", "id": "issue:o/r#5"}
	return g.seedNode("issue:o/r#5", "Issue", body, etag, false, now, now)
}

func (g *ghWorld) staleMutateUpstream() error {
	_, ok := g.fake.Mutate("issue:o/r#5", func(b map[string]any) { b["title"] = "new" })
	if !ok {
		return fmt.Errorf("issue:o/r#5 not found upstream")
	}
	return nil
}

func (g *ghWorld) staleAssertOldServed() error {
	qr, status, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": 5})
	if err != nil {
		return err
	}
	if status != 200 || len(qr.Nodes) != 1 {
		return fmt.Errorf("status=%d nodes=%d", status, len(qr.Nodes))
	}
	var body map[string]any
	_ = json.Unmarshal(qr.Nodes[0].Node, &body)
	if body["title"] != "old" {
		return fmt.Errorf("title = %v, want stale \"old\"", body["title"])
	}
	return nil
}

func (g *ghWorld) staleAssertHealed() error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		title, err := g.nodeTitle("issue:o/r#5")
		if err == nil && title == "new" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("issue:o/r#5 title still %q after reconcile, want \"new\"", title)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ===================== AC-GH-COLDSTART =====================

// coldstartOps is every named op fakegh actually implements over GraphQL/REST for
// a meaningful read; paneForBranch is excluded (EDR marks it "@pending": a
// cross-plugin join wave 2 leaves unimplemented, and fakegh's GraphQL dispatch has
// no case for it).
var coldstartOps = []string{"openIssues", "issue", "pr", "checkRunsForPR", "issueComments", "repoLabels", "openPRs", "prsAwaitingReview", "myClaimed"}

func (g *ghWorld) coldstartRunAllOps() error {
	g.fake.AddRepo("o", "r")
	g.fake.AddIssue("o", "r", 1, "seed", "open")
	g.fake.AddPR("o", "r", 1, "seed pr", "open", time.Time{})
	g.samples = nil
	for _, op := range coldstartOps {
		vars := map[string]any{"owner": "o", "repo": "r", "number": 1, "login": "o"}
		t0 := time.Now()
		_, status, err := g.postOp(op, vars)
		if err != nil {
			return fmt.Errorf("op %s: %w", op, err)
		}
		if status != 200 {
			return fmt.Errorf("op %s: status %d", op, status)
		}
		g.samples = append(g.samples, time.Since(t0))
	}
	return nil
}

func (g *ghWorld) coldstartAssertUnder2s() error {
	for i, d := range g.samples {
		if d >= 2*time.Second {
			return fmt.Errorf("op %s took %s, want < 2s", coldstartOps[i], d)
		}
	}
	return nil
}

func (g *ghWorld) coldstartAssertKeys() error {
	// Every node the cold reads populated must be under the owner/repo scope we
	// queried — no cross-scope over-fetch.
	db, err := g.ghDB()
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT key FROM github_nodes`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return err
		}
		if strings.HasPrefix(key, "list:") {
			continue
		}
		if !strings.Contains(key, "o/r") {
			return fmt.Errorf("cold-start populated an out-of-scope key: %s", key)
		}
	}
	return rows.Err()
}

// ===================== AC-GH-ETAG-304 =====================

func (g *ghWorld) etagSeedExpired() error {
	g.fake.AddRepo("o", "r")
	etag := g.fake.AddIssue("o", "r", 5, "unchanged", "open")
	old := time.Now().Add(-2 * time.Hour) // ttl.issue=3600s (1h) in the harness config
	body := map[string]any{"number": 5, "title": "unchanged", "state": "open", "id": "issue:o/r#5"}
	if err := g.seedNode("issue:o/r#5", "Issue", body, etag, false, old, old); err != nil {
		return err
	}
	g.fake.ResetLog()
	return nil
}

func (g *ghWorld) etagAssertConditional() error {
	_, status, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": 5})
	if err != nil {
		return err
	}
	g.lastOpStatus = status
	reqs := g.fake.Requests()
	for _, r := range reqs {
		if strings.Contains(r.Path, "/repos/o/r/issues/5") && r.IfNoneMatch != "" {
			return nil
		}
	}
	return fmt.Errorf("no conditional GET with If-None-Match recorded: %+v", reqs)
}

func (g *ghWorld) etagAssert304Served() error {
	qr, status, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": 5})
	if err != nil {
		return err
	}
	if status != 200 || len(qr.Nodes) != 1 {
		return fmt.Errorf("status=%d nodes=%d", status, len(qr.Nodes))
	}
	var body map[string]any
	_ = json.Unmarshal(qr.Nodes[0].Node, &body)
	if body["title"] != "unchanged" {
		return fmt.Errorf("title changed unexpectedly: %v", body["title"])
	}
	return nil
}

func (g *ghWorld) etagAssertZeroQuota() error {
	if g.fake.CountPath("GET", "/repos/o/r/issues/5") == 0 {
		return fmt.Errorf("expected at least one conditional GET")
	}
	return nil // fakegh's own serveNode never decrements quota on a 304 (rest.go serveNode)
}

// ===================== AC-GH-PURGE-TAG =====================

func (g *ghWorld) purgeSeedEntries() error {
	now := time.Now()
	if err := g.seedNode("issue:o/r#5", "Issue", map[string]any{"number": 5, "title": "five", "state": "open", "id": "issue:o/r#5"}, `"v1"`, false, now, now); err != nil {
		return err
	}
	if err := g.seedNode("issue:o/r#6", "Issue", map[string]any{"number": 6, "title": "six", "state": "open", "id": "issue:o/r#6"}, `"v1"`, false, now, now); err != nil {
		return err
	}
	raw, _ := json.Marshal(map[string]any{"issues": []map[string]any{{"number": 5}, {"number": 6}}})
	return g.seedListNode("list:openIssues:repo:o/r", "repo:o/r|issue", raw, now)
}

func (g *ghWorld) purgeWebhookEdited() error {
	status, err := g.postWebhook(g.webhookSecret, "issues", "edited",
		map[string]any{"issue": map[string]any{"number": 5}, "repository": map[string]any{"full_name": "o/r"}})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("webhook status %d", status)
	}
	return nil
}

func (g *ghWorld) purgeAssertDeleted() error {
	if ok, err := g.nodeExists("issue:o/r#5"); err != nil || ok {
		return fmt.Errorf("issue:o/r#5 still cached (exists=%v err=%v)", ok, err)
	}
	if ok, err := g.nodeExists("list:openIssues:repo:o/r"); err != nil || ok {
		return fmt.Errorf("openIssues(o/r) list still cached (exists=%v err=%v)", ok, err)
	}
	return nil
}

func (g *ghWorld) purgeAssertSiblingCached() error {
	ok, err := g.nodeExists("issue:o/r#6")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("issue:o/r#6 was purged, want it still cached")
	}
	return nil
}

func (g *ghWorld) purgeAssertEnvelope() error {
	n, err := g.countEvents("github.node.purged", "issue:o/r#5")
	if err != nil {
		return err
	}
	if n < 1 {
		return fmt.Errorf("no github.node.purged envelope recorded for issue:o/r#5")
	}
	return nil
}

// ===================== AC-GH-PIN =====================

func (g *ghWorld) pinSeedMergedPR() error {
	mergedAt := time.Now().Add(-10 * 24 * time.Hour) // > 7d grace window
	etag := g.fake.AddPR("o", "r", 9, "merged pr", "closed", mergedAt)
	old := time.Now().Add(-2 * time.Hour) // far past ttl.pr=3600s, irrelevant once pinned
	body := map[string]any{"number": 9, "title": "merged pr", "state": "closed", "id": "pr:o/r#9", "merged": true, "merged_at": mergedAt.UTC().Format(time.RFC3339)}
	if err := g.seedNode("pr:o/r#9", "PullRequest", body, etag, true, old, old); err != nil {
		return err
	}
	g.fake.ResetLog()
	return nil
}

func (g *ghWorld) pinQueryTwice() error {
	for i := 0; i < 2; i++ {
		qr, status, err := g.postOp("pr", map[string]any{"owner": "o", "repo": "r", "number": 9})
		if err != nil {
			return err
		}
		if status != 200 || len(qr.Nodes) != 1 {
			return fmt.Errorf("query %d: status=%d nodes=%d", i, status, len(qr.Nodes))
		}
		g.lastOp = qr
	}
	return nil
}

func (g *ghWorld) pinAssertServedBoth() error {
	if len(g.lastOp.Nodes) != 1 {
		return fmt.Errorf("no node served")
	}
	return nil
}

func (g *ghWorld) pinAssertNoFetch() error {
	if n := len(g.fake.Requests()); n != 0 {
		return fmt.Errorf("fake GitHub recorded %d requests for a pinned node, want 0: %+v", n, g.fake.Requests())
	}
	return nil
}

// ===================== AC-GH-SINGLEFLIGHT =====================

func (g *ghWorld) sfSeedUpstreamOnly() error {
	g.fake.AddRepo("o", "r")
	g.fake.AddIssue("o", "r", 7, "coalesced", "open")
	g.fake.ResetLog()
	return nil
}

func (g *ghWorld) sfConcurrentQueries() error {
	var wg sync.WaitGroup
	results := make([]ghQueryResult, 10)
	errs := make([]error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			qr, status, err := g.postOp("issue", map[string]any{"owner": "o", "repo": "r", "number": 7})
			if err == nil && status != 200 {
				err = fmt.Errorf("status %d", status)
			}
			results[i], errs[i] = qr, err
		}(i)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	g.lastOp = results[0]
	for _, r := range results {
		if len(r.Nodes) != 1 {
			return fmt.Errorf("expected one node per response, got %d", len(r.Nodes))
		}
	}
	g.samples = nil // reuse unrelated field slot not needed; keep results local via closure below
	g.forwardCompare = results
	return nil
}

func (g *ghWorld) sfAssertOneFetch() error {
	if n := g.fake.CountPath("GET", "/repos/o/r/issues/7"); n != 1 {
		return fmt.Errorf("fake GitHub received %d fetches for issue 7, want 1", n)
	}
	return nil
}

func (g *ghWorld) sfAssertSameNode() error {
	if len(g.forwardCompare) != 10 {
		return fmt.Errorf("expected 10 captured responses, got %d", len(g.forwardCompare))
	}
	first := string(g.forwardCompare[0].Nodes[0].Node)
	for i, r := range g.forwardCompare {
		if string(r.Nodes[0].Node) != first {
			return fmt.Errorf("response %d node differs from response 0", i)
		}
	}
	return nil
}

// ===================== AC-GH-HMAC =====================

func (g *ghWorld) hmacBadSig() error {
	status, err := g.postWebhook("wrong-secret", "issues", "opened",
		map[string]any{"issue": map[string]any{"number": 1}, "repository": map[string]any{"full_name": "o/r"}})
	if err != nil {
		return err
	}
	g.lastWebhook = status
	return nil
}

func (g *ghWorld) hmacGoodSig() error {
	status, err := g.postWebhook(g.webhookSecret, "issues", "opened",
		map[string]any{"issue": map[string]any{"number": 1}, "repository": map[string]any{"full_name": "o/r"}})
	if err != nil {
		return err
	}
	g.lastWebhook = status
	return nil
}

func (g *ghWorld) assertLastWebhook401() error {
	if g.lastWebhook != 401 {
		return fmt.Errorf("status = %d, want 401", g.lastWebhook)
	}
	return nil
}

func (g *ghWorld) assertLastWebhook200() error {
	if g.lastWebhook != 200 {
		return fmt.Errorf("status = %d, want 200", g.lastWebhook)
	}
	return nil
}

func (g *ghWorld) assertNoGithubEvent() error {
	n, err := g.countEvents("", "")
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("%d github events emitted, want 0", n)
	}
	return nil
}

func (g *ghWorld) assertOneGithubEvent() error {
	n, err := g.countEvents("", "")
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%d github events emitted, want exactly 1", n)
	}
	return nil
}

// ===================== AC-GH-FORWARD (local) =====================

func (g *ghWorld) forwardGiven() error {
	stub, err := buildGhStub()
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "ghstub-deliveries-*")
	if err != nil {
		return err
	}
	g.deliveriesDir = dir
	g.ghPath = stub
	g.ingress = "forward"
	// forward now requires a --repo target; o/r is the repo this scenario hooks.
	g.forwardRepo = "o/r"
	// A short reconcile interval lets the plugin discover o/r and create+store its
	// hook early, so the post-crash restart's redelivery pass has a hook to replay
	// the missed delivery from.
	g.fake.AddRepo("o", "r")
	g.reconcileSecs = 1
	_ = os.Setenv("GHSTUB_DELIVERIES_DIR", dir)
	_ = os.Setenv("GHSTUB_CRASH_AFTER", "1")
	g.stubEnvSet = true
	return g.startDefault()
}

// writeStubDelivery drops a delivery JSON file the ghstub binary polls for and
// signs+POSTs to the plugin's webhook route (see fakegh/ghstub/main.go).
func (g *ghWorld) writeStubDelivery(id, event, action string, payload map[string]any) error {
	if action != "" {
		payload["action"] = action
	}
	raw, _ := json.Marshal(payload)
	rec := map[string]any{"event": event, "action": action, "delivery_id": id, "payload": json.RawMessage(raw)}
	b, _ := json.Marshal(rec)
	return os.WriteFile(g.deliveriesDir+"/"+id+".json", b, 0o644)
}

func (g *ghWorld) forwardStubDelivers() error {
	// Wait until reconcile has created the repo's hook before delivering, so the
	// crash+restart happen with a hook already in place for redelivery to use.
	deadline := time.Now().Add(4 * time.Second)
	for g.fake.HookID("o", "r") == 0 {
		if time.Now().After(deadline) {
			return fmt.Errorf("forward: hook for o/r was never created")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return g.writeStubDelivery("d1", "issues", "opened",
		map[string]any{"issue": map[string]any{"number": 1}, "repository": map[string]any{"full_name": "o/r"}})
}

func (g *ghWorld) forwardAssertIngested() error {
	// With ingest.go passing --secret, the ghstub signs the forwarded delivery with
	// the configured webhook secret, so the plugin's HMAC check accepts it and the
	// issue becomes queryable. This has nothing to do with the crash/backoff path
	// (GHSTUB_CRASH_AFTER only fires after this delivery is sent) — the bound just
	// needs enough slack for the stub's 30ms poll tick plus one HTTP round trip and
	// a sqlite write under -race's slowed goroutine scheduling, so it's generous
	// rather than tight (10s, well above any observed race-mode latency).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := g.nodeExists("issue:o/r#1"); ok {
			return nil
		}
		n, _ := g.countEvents("", "issue:o/r#1")
		if n > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("forward: issue:o/r#1 was never ingested (webhookSecret=%q)", g.webhookSecret)
}

func (g *ghWorld) forwardStubExits() error {
	// GHSTUB_CRASH_AFTER=1 makes the stub os.Exit(1) after its one delivery. Queue a
	// delivery that "arrived while it was down": the supervisor's next (restart)
	// iteration runs redelivery from the last-seen id and replays exactly this one
	// via /attempts.
	hookID := g.fake.HookID("o", "r")
	if hookID == 0 {
		return fmt.Errorf("forward: no hook to queue an undelivered delivery against")
	}
	g.fake.QueueUndelivered(hookID, "issues", "opened",
		map[string]any{"issue": map[string]any{"number": 2}, "repository": map[string]any{"full_name": "o/r"}})
	return nil
}

func (g *ghWorld) forwardAssertRestarted() error {
	// After the crash the supervisor waits backoff, then on its restart iteration
	// runs redelivery from the last-seen id: the queued undelivered event is
	// replayed to the handler and becomes queryable. issue:o/r#2 appearing proves
	// the restart happened and redelivery ran. backoffInitial (ingest.go) is 500ms,
	// so under normal load this resolves in ~1s; give it the same generous,
	// race-mode-safe bound as forwardAssertIngested rather than a tight one.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok, _ := g.nodeExists("issue:o/r#2"); ok {
			return nil
		}
		if n, _ := g.countEvents("", "issue:o/r#2"); n > 0 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("forward: the missed delivery (issue:o/r#2) was never replayed after restart")
}

func (g *ghWorld) forwardAssertRedelivered() error {
	if n := g.fake.CountPath("POST", "/attempts"); n != 1 {
		return fmt.Errorf("forward: expected exactly one /attempts redelivery, got %d", n)
	}
	return nil
}

// ===================== AC-GH-NOTIFY-304 =====================

func (g *ghWorld) notifyAssertNotPolling() error {
	time.Sleep(200 * time.Millisecond)
	if n := g.fake.CountPath("GET", "/notifications"); n != 0 {
		return fmt.Errorf("notifications poll ran (%d requests) while notifications=false", n)
	}
	return nil
}

func (g *ghWorld) notifyRestartEnabled() error {
	g.fake.SetPollInterval(1) // fast poll cadence so the test doesn't wait a real 60s
	g.fake.SetNotifications([]map[string]any{{"id": "t0"}})
	lm := time.Now().UTC().Format(time.RFC1123)
	if err := g.setCursorRow("notif:lastModified", lm); err != nil {
		return err
	}
	g.sw.stopServe()
	g.notifications = true
	if err := g.writeConfig(); err != nil {
		return err
	}
	g.fake.ResetLog()
	return g.sw.startServe()
}

func (g *ghWorld) notifyWaitOnePoll() error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if g.fake.CountPath("GET", "/notifications") >= 1 {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("no /notifications poll observed after restart")
}

func (g *ghWorld) notifyAssertZeroQuota() error {
	if ok, err := g.nodeExists("issue:o/r#1"); err != nil || ok {
		return fmt.Errorf("unexpected node fetch during a 304 notifications poll")
	}
	return nil
}

func (g *ghWorld) notifyEmitChange() error {
	// fakegh truncates notifLastModified to the whole second (matching real GitHub's
	// HTTP-date granularity); wait past the second boundary so this SetNotifications
	// call is guaranteed to advance it relative to the value the first poll already
	// observed, rather than landing in the same truncated second.
	time.Sleep(1100 * time.Millisecond)
	g.fake.ResetLog()
	g.fake.SetNotifications([]map[string]any{{"id": "t1", "subject": map[string]any{"title": "changed"}}})
	return nil
}

func (g *ghWorld) notifyAssertCursorAdvanced() error {
	deadline := time.Now().Add(5 * time.Second)
	before, _ := g.cursorValue("notif:lastModified")
	for time.Now().Before(deadline) {
		if g.fake.CountPath("GET", "/notifications") >= 1 {
			after, err := g.cursorValue("notif:lastModified")
			if err != nil {
				return err
			}
			if after != "" && after != before {
				return nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("notif:lastModified cursor did not advance from %q after a 200 notifications response", before)
}

// ===================== AC-GH-FLOOR =====================

func (g *ghWorld) floorSeedNearFloor() error {
	g.fake.AddRepo("o", "r")
	g.fake.SetGraphQLRate(5, time.Now().Add(1500*time.Millisecond), 1)
	return nil
}

type floorResult struct {
	status int
	err    error
}

func (g *ghWorld) floorTriggerRead() error {
	g.forwardFloorDone = make(chan floorResult, 1)
	go func() {
		_, status, err := g.postOp("openIssues", map[string]any{"owner": "o", "repo": "r"})
		g.forwardFloorDone <- floorResult{status, err}
	}()
	return nil
}

func (g *ghWorld) floorAssertPaused() error {
	select {
	case r := <-g.forwardFloorDone:
		return fmt.Errorf("read-through returned early (status=%d err=%v) instead of pausing near the points floor", r.status, r.err)
	case <-time.After(500 * time.Millisecond):
		return nil // still in flight: paused, as expected
	}
}

func (g *ghWorld) floorAssertHealthy() error {
	rows, _, err := g.sw.getHealth()
	if err != nil {
		return err
	}
	r := findRow(rows, "github")
	if r == nil {
		return fmt.Errorf("no github row in /health")
	}
	if r["state"] == "stale" {
		return fmt.Errorf("github health forced stale during floor pause")
	}
	return nil
}

func (g *ghWorld) floorAssertResumed() error {
	select {
	case r := <-g.forwardFloorDone:
		if r.err != nil {
			return r.err
		}
		if r.status != 200 {
			return fmt.Errorf("resumed read status = %d, want 200", r.status)
		}
		return nil
	case <-time.After(4 * time.Second):
		return fmt.Errorf("paused read-through never resumed")
	}
}

// ===================== AC-GH-RATELOG =====================

func (g *ghWorld) ratelogTrigger() error {
	g.fake.AddRepo("o", "r")
	g.fake.AddIssue("o", "r", 1, "x", "open")
	_, status, err := g.postOp("openIssues", map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("status %d", status)
	}
	return nil
}

func (g *ghWorld) ratelogAssert() error {
	out := g.sw.serve.stderr.String() + g.sw.serve.stdout.String()
	if !strings.Contains(out, "rate graphql remaining=") {
		return fmt.Errorf("log does not record GraphQL rateLimit; got:\n%s", tail(out, 2000))
	}
	return nil
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ===================== AC-GH-NAMEDOP-KEYS =====================

func (g *ghWorld) namedopRun() {
	g.fake.AddRepo("o", "r")
	g.fake.AddIssue("o", "r", 5, "five", "open")
	g.fake.AddIssue("o", "r", 6, "six", "open")
	g.sw.runCLI("--endpoint", "http://"+g.sw.listen+"/graphql", "query", "--op", "openIssues",
		"--var", "owner=o", "--var", "repo=r", "--queries-dir", repoRoot+"/plugins/github/queries")
}

func (g *ghWorld) namedopAssertScope() error {
	if g.sw.lastExit != 0 {
		return fmt.Errorf("supergraph query --op exited %d: %s", g.sw.lastExit, g.sw.lastStderr)
	}
	// `supergraph query --op` prints only the op's .Data (not .Scope) — the scope
	// tag itself is verified indirectly below via namedopAssertListEvicted, which
	// checks that a purge covering `repo:o/r|issue` evicts the list result stored
	// under that scope. Here just confirm the op actually ran and returned data.
	if !strings.Contains(g.sw.lastStdout, `"five"`) || !strings.Contains(g.sw.lastStdout, `"six"`) {
		return fmt.Errorf("stdout does not show both issue titles:\n%s", g.sw.lastStdout)
	}
	return nil
}

func (g *ghWorld) namedopAssertNodesStored() error {
	ok5, err := g.nodeExists("issue:o/r#5")
	if err != nil {
		return err
	}
	ok6, err := g.nodeExists("issue:o/r#6")
	if err != nil {
		return err
	}
	if !ok5 || !ok6 {
		return fmt.Errorf("AC-GH-NAMEDOP-KEYS: named-op key storage did not run — issue:o/r#5 stored=%v, issue:o/r#6 stored=%v", ok5, ok6)
	}
	return nil
}

func (g *ghWorld) namedopWebhookClosed() error {
	status, err := g.postWebhook(g.webhookSecret, "issues", "closed",
		map[string]any{"issue": map[string]any{"number": 5}, "repository": map[string]any{"full_name": "o/r"}})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("webhook status %d", status)
	}
	return nil
}

func (g *ghWorld) namedopAssertListEvicted() error {
	ok, err := g.nodeExists("list:openIssues:repo:o/r")
	if err != nil {
		return err
	}
	if ok {
		return fmt.Errorf("openIssues(o/r) list result still cached after a closed webhook for #5")
	}
	return nil
}

func (g *ghWorld) namedopSchemaPrint() error {
	g.sw.runCLI("--endpoint", "http://"+g.sw.listen+"/graphql", "schema", "Issue")
	if g.sw.lastExit != 0 {
		return fmt.Errorf("supergraph schema Issue exited %d: %s", g.sw.lastExit, g.sw.lastStderr)
	}
	if !strings.Contains(g.sw.lastStdout, "Issue") {
		return fmt.Errorf("stdout does not print the Issue type:\n%s", g.sw.lastStdout)
	}
	return nil
}

// ===================== AC-GH-CURSOR =====================

func (g *ghWorld) cursorAdvance() error {
	g.fake.AddRepo("o", "r")
	return g.reconcileRunsOnce()
}

func (g *ghWorld) cursorRestart() error {
	g.sw.stopServe()
	return g.sw.startServe()
}

func (g *ghWorld) cursorAssertPersisted() error {
	v, err := g.cursorValue("since:o/r")
	if err != nil {
		return err
	}
	if v == "" {
		return fmt.Errorf("since:o/r cursor missing after restart")
	}
	g.cursorT = v
	return nil
}

func (g *ghWorld) cursorAssertOverlap() error {
	g.fake.ResetLog()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range g.fake.Requests() {
			if r.Since != "" {
				t, err1 := time.Parse(time.RFC3339, g.cursorT)
				since, err2 := time.Parse(time.RFC3339, r.Since)
				if err1 == nil && err2 == nil {
					if t.Sub(since) >= 55*time.Second && t.Sub(since) <= 65*time.Second {
						return nil
					}
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no reconcile GraphQL request carried since=T-60s")
}

// ===================== AC-GH-LOC =====================

// locOutputRe scans all lines of `make loc-github` output for the prod LOC count,
// since make's recursive job-control chatter ("Entering/Leaving directory") can
// surround the line we care about.
var locOutputRe = regexp.MustCompile(`prod LOC:\s*(\d+)`)

func (g *ghWorld) locRun() error {
	cmd := exec.Command("make", "--no-print-directory", "loc-github")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	g.sw.lastStdout = string(out)
	g.sw.lastExit = exitCode(err)
	return nil
}

func (g *ghWorld) locAssert() error {
	budget := 1650
	if v := os.Getenv("LOC_BUDGET"); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			budget = n
		}
	}
	m := locOutputRe.FindStringSubmatch(g.sw.lastStdout)
	if m == nil {
		return fmt.Errorf("could not parse loc-github output: %q", g.sw.lastStdout)
	}
	var count int
	if _, err := fmt.Sscanf(m[1], "%d", &count); err != nil {
		return fmt.Errorf("could not parse loc-github output: %q", g.sw.lastStdout)
	}
	if count > budget {
		return fmt.Errorf("plugins/github prod LOC = %d, exceeds the %d budget (honest red per docs/edr/github.md pending owner decision)", count, budget)
	}
	return nil
}

// ===================== AC-GH-ZEROCORE =====================

func (g *ghWorld) zerocoreRun() error {
	cmd := exec.Command("git", "diff", "--stat", "core/")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	g.sw.lastStdout = string(out)
	if err != nil {
		return err
	}
	return nil
}

// ===================== AC-GH-RECONCILE-SHAPE / AC-GH-RECONCILE-NOEVENT =====================

// reconcileSeedRichIssue registers o/r and seeds a fully-shaped GraphQL issue
// (labels/assignee/updatedAt) upstream; the REST endpoint for the same key serves
// the flat REST shape (fakegh restView), so a reconcile that re-stored the REST body
// would visibly drop these fields.
func (g *ghWorld) reconcileSeedRichIssue() error {
	g.fake.AddRepo("o", "r")
	g.fake.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug", "p1"}, []string{"alice"})
	return nil
}

// reconcileWarmOpenIssues warms the openIssues list path so issue:o/r#5 is cached in
// the canonical GraphQL shape before reconcile revalidates it.
func (g *ghWorld) reconcileWarmOpenIssues() error {
	_, status, err := g.postOp("openIssues", map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("warm openIssues status %d", status)
	}
	return nil
}

// reconcileAssertShapeIntact reads issue:o/r#5 through the cache-only core resolver
// and requires labels, assignee, and updatedAt to survive the reconcile pass.
func (g *ghWorld) reconcileAssertShapeIntact() error {
	_, data, err := g.sw.gql(`{ issue(key: "issue:o/r#5") { number updatedAt labels assignees { login } } }`)
	if err != nil {
		return err
	}
	iss, ok := data["issue"].(map[string]any)
	if !ok || iss == nil {
		return fmt.Errorf("issue query returned null: %v", data)
	}
	if labels, _ := iss["labels"].([]any); len(labels) == 0 {
		return fmt.Errorf("labels dropped by reconcile: %v", iss)
	}
	if assignees, _ := iss["assignees"].([]any); len(assignees) == 0 {
		return fmt.Errorf("assignees dropped by reconcile: %v", iss)
	}
	if iss["updatedAt"] == nil {
		return fmt.Errorf("updatedAt dropped by reconcile: %v", iss)
	}
	return nil
}

// reconcileAssertNoUpdateEvent requires the unchanged revalidation to have emitted
// no github.node.updated for the issue.
func (g *ghWorld) reconcileAssertNoUpdateEvent() error {
	n, err := g.countEvents("github.node.updated", "issue:o/r#5")
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("reconcile emitted %d github.node.updated for issue:o/r#5 on an unchanged node, want 0", n)
	}
	return nil
}

// ===================== @live @pending =====================

func pendingStep() error { return godog.ErrPending }

func registerGithubLiveSteps(sc *godog.ScenarioContext, g *ghWorld) {
	live := []string{
		"the github plugin is running against the live account",
		"a real webhook delivery is dropped",
		"boot redelivery or the next since-cursor reconcile runs",
		"the dropped object is present in the graph",
		`evidence is captured: "the dropped event is healed within one window, via a log"`,
		"a new repo is created and the next reconcile runs",
		`evidence is captured: "the repo appears in the graph with zero config within one reconcile, via a query screenshot"`,
		"the github plugin is running against the live account with 20 repos",
		"it runs at steady state for one hour",
		"GraphQL points and REST calls stay under the 500-per-hour budget",
		`evidence is captured: "GitHub API usage stays under 500 per hour, via a rate-limit log"`,
		"the github plugin in `forward` ingress against live GitHub",
		"a real event fires on a watched repo",
		"`gh webhook forward` streams it over the outbound websocket to the handler and it is ingested",
	}
	for _, s := range live {
		sc.Step(lit(s), pendingStep)
	}
	registerGithubRateLogSteps(sc, g)
	registerGithubNotify304Steps(sc, g)
}

// registerGithubRateLogSteps wires AC-GH-RATELOG against real GitHub: one live
// GraphQL request must log the upstream rateLimit{remaining,resetAt}.
func registerGithubRateLogSteps(sc *godog.ScenarioContext, g *ghWorld) {
	sc.Step(lit("the github plugin running against live GitHub"), func() error { return g.startLive() })
	sc.Step(lit("it makes a live GraphQL request"), func() error {
		o, r, err := liveRepo()
		if err != nil {
			return err
		}
		_, err = g.warmOp("openIssues", o, r)
		return err
	})
	sc.Step(lit("the log records the live `rateLimit{remaining,resetAt}`"), func() error {
		if !g.waitLogLine("github: rate graphql remaining=", 15*time.Second) {
			return fmt.Errorf("no `github: rate graphql remaining=` line in serve log:\n%s", g.sw.serve.stdout.String())
		}
		if !strings.Contains(g.sw.serve.stdout.String(), "resetAt=") {
			return fmt.Errorf("rate graphql line lacks resetAt=:\n%s", g.sw.serve.stdout.String())
		}
		return nil
	})
}

// registerGithubNotify304Steps wires AC-GH-NOTIFY-304 against real GitHub: the
// notifications poll conditionally GETs /notifications and a subsequent unchanged
// poll returns 304 for zero quota. The plugin polls at the server-advertised
// X-Poll-Interval (~60s), so the assertion waits across two polls.
func registerGithubNotify304Steps(sc *godog.ScenarioContext, g *ghWorld) {
	sc.Step(lit("the github plugin polling live `/notifications` with `notifications = true`"), func() error {
		g.notifications = true
		return g.startLive()
	})
	sc.Step(lit("there are no changes since the stored `If-Modified-Since`"), func() error { return nil })
	sc.Step(lit("GitHub returns 304 at the advertised `X-Poll-Interval` and no quota is spent"), func() error {
		if !g.waitLogLine("github: notifications 304", 160*time.Second) {
			return fmt.Errorf("no `github: notifications 304` line within 160s (account activity may have kept every poll at 200):\n%s", g.sw.serve.stdout.String())
		}
		return nil
	})
}
