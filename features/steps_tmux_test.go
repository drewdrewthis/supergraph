package features

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cucumber/godog"
)

// tw is the tmux scenario world, re-initialized (fresh socket + serve process) for
// every @tmux scenario and torn down after. Non-@tmux scenarios never touch it, so
// no tmux server is spawned for the core suite.
var tw = &tmuxWorld{}

// within is one reconcile interval plus scheduling slack — the deadline for a
// "within T" assertion. Scenarios that set a deliberately huge interval do not use
// it (they assert via the control-mode push path instead).
func (g *tmuxWorld) within() time.Duration {
	d := time.Duration(g.reconcileSecs)*3*time.Second + 3*time.Second
	if d < 10*time.Second {
		d = 10 * time.Second // floor: the suite runs many real tmux servers under load
	}
	return d
}

func (g *tmuxWorld) seedSession(name, dir string, extraPanes int) error {
	args := []string{"new-session", "-d", "-s", name}
	if dir != "" {
		args = append(args, "-c", dir)
	}
	if out, err := g.tmuxStart(args...); err != nil {
		return fmt.Errorf("new-session %s: %s", name, out)
	}
	for i := 0; i < extraPanes; i++ {
		sargs := []string{"split-window", "-t", name}
		if dir != "" {
			sargs = append(sargs, "-c", dir)
		}
		if out, err := g.tmux(sargs...); err != nil {
			return fmt.Errorf("split-window %s: %s", name, out)
		}
	}
	return nil
}

func registerTmuxSteps(sc *godog.ScenarioContext) {
	sc.Before(func(ctx context.Context, s *godog.Scenario) (context.Context, error) {
		if tmuxHasTag(s, "@local") {
			return ctx, tw.init()
		}
		return ctx, nil
	})
	sc.After(func(ctx context.Context, s *godog.Scenario, _ error) (context.Context, error) {
		if tmuxHasTag(s, "@local") {
			tw.cleanup()
		}
		return ctx, nil
	})

	// FREESLOTS-WARM
	sc.Step(lit("a tmux server on a private socket seeded with idle and busy panes"), tw.seedIdleBusy)
	sc.Step(lit("a supergraph server watching that socket with the cache warm"), tw.startWarm)
	sc.Step(lit("`freeSlots` is queried 20 times"), tw.query20)
	sc.Step(lit("every idle pane is returned and no busy pane is offered as free"), tw.assertIdleFreeBusyNot)
	sc.Step(lit("the p95 of the 20 query latencies is under 1s"), tw.assertP95Under1s)

	// EVENTS-CONTROL
	sc.Step(lit("a supergraph server watching a private tmux socket via control mode with the reconcile interval set far beyond the observation window"), tw.startControlHugeInterval)
	sc.Step(lit(`a session "s1" the control client is attached to and a second session "s3"`), tw.seedS1S3)
	sc.Step(lit(`20 panes are split in session "s3", each timed from split to queryable`), tw.split20Timed)
	sc.Step(lit("every new pane became queryable via the control-mode path and the p95 split-to-queryable latency is under 1s"), tw.assertP95Under1s)
	sc.Step(lit("the cached pane count equals the real tmux pane count, with no `%output` noise ingested"), tw.assertCacheEqualsReal)

	// NO-HOOK-CLOBBER
	sc.Step(lit("a private tmux socket with a user-set global hook `after-split-window`"), tw.seedHook)
	sc.Step(lit("a supergraph server runs the tmux plugin against that socket for a full watch-and-reconcile cycle"), tw.startAndCycle)
	sc.Step(lit("`show-hooks -g` on the socket is byte-identical before and after the run"), tw.assertHooksUnchanged)
	sc.Step(lit("`grep -R \"set-hook\" plugins/tmux` returns nothing"), tw.assertNoSetHook)

	// PANE-DEATH
	sc.Step(lit("a supergraph server watching a private tmux socket with a tracked idle-shell pane"), tw.startTrackedPane)
	sc.Step(lit("the pane appears in `freeSlots`"), tw.assertTrackedFree)
	sc.Step(lit("the pane is killed with `kill-pane`"), tw.killTrackedPane)
	sc.Step(lit("within T the pane is marked staleSince and no longer appears in `freeSlots`"), tw.assertTrackedStaleAndNotFree)

	// FREE-BUSY
	sc.Step(lit("a supergraph server watching a private tmux socket with a tracked idle-shell pane that reads free"), tw.startTrackedSolo)
	sc.Step(lit("the pane starts running `sleep 30`"), tw.runSleep)
	sc.Step(lit("within T the pane is absent from `freeSlots`"), tw.assertTrackedNotFree)
	sc.Step(lit("the command is interrupted and the pane returns to its shell prompt"), tw.interrupt)
	sc.Step(lit("within T the pane reads free again"), tw.assertTrackedFreeAgain)

	// STALE (poll-only negative control)
	sc.Step(lit("a supergraph server watching a private tmux socket in poll-only mode so only the reconcile heals"), tw.startPollLong)
	sc.Step(lit("a tracked pane in a session with a second pane"), tw.seedTrackedTwoPane)
	sc.Step(lit("the pane is killed"), tw.killTrackedPane)
	sc.Step(lit("a query before the next reconcile still shows the pane, by design"), tw.assertTrackedStillPresent)
	sc.Step(lit("the reconcile poll runs once"), tw.awaitOneReconcile)
	sc.Step(lit("the pane is marked staleSince, bounding worst-case staleness by the reconcile interval"), tw.assertTrackedStale)

	// SERVER-DOWN
	sc.Step(lit("a supergraph server watching a private tmux socket with tracked sessions and panes"), tw.startTrackedPane)
	sc.Step(lit("the tmux server is killed with `kill-server`"), tw.killServer)
	sc.Step(lit("within T every local session and pane is marked staleSince"), tw.assertAllStale)
	sc.Step(lit("the plugin does not exit or panic and `GET /health` still returns HTTP 200"), tw.assertHealth200)
	sc.Step(lit("a new tmux server is started on the same socket"), tw.restartTmuxServer)
	sc.Step(lit("the next reconcile repopulates the sessions and panes"), tw.assertRepopulated)

	// POLL-ERROR
	sc.Step(lit("a supergraph server whose tmux binary is a stub that succeeds until triggered, watching a private socket in poll-only mode"), tw.startStubPoll)
	sc.Step(lit("the `tmux` entry in `/health` reads healthy after the first reconcile"), tw.assertTmuxHealthy)
	sc.Step(lit("the stub is switched to exit non-zero on every reconcile"), tw.tripStub)
	sc.Step(lit("no further `tmux.snapshot` advances health and within the lag threshold the `tmux` entry crosses to stale"), tw.assertTmuxStale)

	// PANE-FOR-BRANCH
	sc.Step(lit("a supergraph server watching a private tmux socket"), tw.startBareControl)
	sc.Step(lit(`a session whose worktree is checked out on branch "plugin/tmux"`), tw.seedBranchSession)
	sc.Step(lit(`another session on branch "main"`), tw.seedMainSession)
	sc.Step(lit("`paneForBranch` is queried for \"plugin/tmux\""), tw.queryPaneForBranch)
	sc.Step(lit(`exactly the pane on "plugin/tmux" is returned and the "main" pane is not`), tw.assertBranchPane)

	// KEY-GRAMMAR
	sc.Step(lit("a supergraph server watching a private tmux socket with a tracked pane"), tw.startTrackedPane)
	sc.Step(lit("`tmuxPanes` is queried"), tw.queryPanes)
	sc.Step(lit("the pane key matches `pane:<session>:<window>.<pane>@<hostId>` and re-parses to those parts"), tw.assertKeyRoundTrip)

	// ISOLATION
	sc.Step(lit("a supergraph server watching a private tmux socket alongside a sibling plugin that panics in Start"), tw.startWithPanicSibling)
	sc.Step(lit("the sibling plugin's panic is induced"), tw.noop)
	sc.Step(lit(`the "tmux" entry in `+"`/health`"+` is not stale and `+"`freeSlots`"+` still answers`), tw.assertTmuxNotStaleAnswers)
	sc.Step(lit("the tmux plugin still serves `GET /health` with HTTP 200"), tw.assertHealth200)

	// CURSOR
	sc.Step(lit("a supergraph server watching a private tmux socket that has run at least one reconcile"), tw.startPollShortRecordCursor)
	sc.Step(lit("the tmux server is killed and the supergraph server is restarted against the same data dir"), tw.killAndRestart)
	sc.Step(lit("the `snapshot:lastAt` cursor reported in `/health` is resumed from disk, not reset"), tw.assertCursorResumed)

	// ZEROCORE
	sc.Step(lit("the tmux plugin exists under `plugins/tmux/`, registered via `graph/plugins_import.go` and regenerated `graph/`"), tw.noop)
	sc.Step(lit("I check `git diff --stat core/` against main"), tw.gitDiffCore)
	sc.Step(lit("it reports 0 files changed"), tw.assertNoCoreDiff)
	sc.Step(lit(`a supergraph server watching a private tmux socket serves a "tmux" entry in `+"`/health`"), tw.startBareControlAssertRow)

	// LOC
	sc.Step(lit("the tmux plugin source under `plugins/tmux/`"), tw.noop)
	sc.Step(lit("`make loc-tmux` counts non-comment non-blank lines of the non-test Go files"), tw.runLocTmux)
	sc.Step(lit("the count is at most 800"), tw.assertLocWithinCap)

	// STALE-PEER (@pending, peer-owned cross-box): register as honest pending stubs
	// so the scenario reports "pending" rather than "undefined" (the evidence step is
	// covered by the generic prd pending step).
	for _, phrase := range []string{
		"the peer plugin mirrors a second box's tmux data",
		"the second box stops",
		"within 30s the peer shows the second box's tmux sessions and panes as stale-since-T",
		"no peer-of-peer rows exist",
	} {
		sc.Step(lit(phrase), func() error { return pending() })
	}
}

func (g *tmuxWorld) noop() error { return nil }

// ---- FREESLOTS-WARM ----

const (
	idleA = "pane:s1:0.0@test"
	idleB = "pane:s1:0.1@test"
	busyC = "pane:s1:0.2@test"
)

func (g *tmuxWorld) seedIdleBusy() error {
	g.reconcileSecs = 1
	if err := g.seedSession("s1", "", 2); err != nil { // 3 panes: 0.0,0.1,0.2
		return err
	}
	// ensure the shell has actually started `sleep` before the plugin reconciles,
	// otherwise the pane is still at its (free) prompt when the cache warms.
	return g.sendUntil("s1:0.2", func(c string) bool { return c == "sleep" }, "sleep 300", "Enter")
}

func (g *tmuxWorld) startWarm() error {
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	// warm = the reconcile has classified the busy pane busy and the idle panes free.
	return eventually(8*time.Second, func() error {
		free := freeKeys(mustSlots(g))
		if free[idleA] && free[idleB] && !free[busyC] {
			return nil
		}
		return fmt.Errorf("cache not warm yet: %v", free)
	})
}

func mustSlots(g *tmuxWorld) []slotJSON {
	s, _ := g.freeSlots()
	return s
}

func (g *tmuxWorld) query20() error {
	g.samples = nil
	for i := 0; i < 20; i++ {
		t0 := time.Now()
		s, err := g.freeSlots()
		if err != nil {
			return err
		}
		g.samples = append(g.samples, time.Since(t0))
		g.lastFree = freeKeys(s)
	}
	return nil
}

func (g *tmuxWorld) assertIdleFreeBusyNot() error {
	if !g.lastFree[idleA] || !g.lastFree[idleB] {
		return fmt.Errorf("idle panes missing from freeSlots: %v", g.lastFree)
	}
	if g.lastFree[busyC] {
		return fmt.Errorf("busy pane offered as free: %v", g.lastFree)
	}
	return nil
}

func (g *tmuxWorld) assertP95Under1s() error {
	if len(g.samples) < 20 {
		return fmt.Errorf("want 20 samples, have %d", len(g.samples))
	}
	if p := p95(g.samples); p >= time.Second {
		return fmt.Errorf("p95 %s not under 1s", p)
	}
	return nil
}

// ---- EVENTS-CONTROL ----

func (g *tmuxWorld) startControlHugeInterval() error {
	g.eventSource = "control"
	g.reconcileSecs = 3600 // beyond the observation window: only the control push can satisfy the AC
	if err := g.seedSession("s1", "", 0); err != nil {
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	return g.waitPanes(1)
}

func (g *tmuxWorld) seedS1S3() error {
	if err := g.seedSession("s3", "", 0); err != nil { // s1 is the control-attach target
		return err
	}
	// wait until s3's initial pane has been observed (via the control push, since the
	// reconcile interval is huge) so the split baseline count is deterministic.
	return g.waitPanes(2)
}

func (g *tmuxWorld) split20Timed() error {
	g.samples = nil
	base, err := g.paneCountHTTP()
	if err != nil {
		return err
	}
	for i := 1; i <= 20; i++ {
		t0 := time.Now()
		// new-window (not split-window) so 20 panes never exhaust a single window's
		// space; each is still a pane created in session s3 and observed via the
		// control-mode %window-add push, which is the path under test.
		if out, err := g.tmux("new-window", "-t", "s3"); err != nil {
			return fmt.Errorf("new-window %d: %s", i, out)
		}
		want := base + i
		deadline := time.Now().Add(3 * time.Second)
		for {
			cur, err := g.paneCountHTTP()
			if err != nil {
				return err
			}
			if cur >= want {
				break
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("pane %d not queryable via control path within 3s", i)
			}
			time.Sleep(5 * time.Millisecond)
		}
		g.samples = append(g.samples, time.Since(t0))
	}
	return nil
}

func (g *tmuxWorld) assertCacheEqualsReal() error {
	// The real pane set is stable now (no more mutations), so poll until the cache
	// converges to it: the most recent control-mode push may still be in flight when
	// this step begins. Convergence (never drifting past the real count, no phantom
	// %output rows) is the property under test.
	real, err := g.realPaneCount()
	if err != nil {
		return err
	}
	return eventually(5*time.Second, func() error {
		ps, err := g.panes()
		if err != nil {
			return err
		}
		if len(ps) != real {
			return fmt.Errorf("cached pane count %d != real %d (%%output noise or drift)", len(ps), real)
		}
		return nil
	})
}

// ---- NO-HOOK-CLOBBER ----

func (g *tmuxWorld) seedHook() error {
	g.reconcileSecs = 1
	if err := g.seedSession("s1", "", 0); err != nil {
		return err
	}
	if out, err := g.tmux("set-hook", "-g", "after-split-window", "display-message hooked"); err != nil {
		return fmt.Errorf("set-hook: %s", out)
	}
	out, err := g.tmux("show-hooks", "-g")
	if err != nil {
		return fmt.Errorf("show-hooks: %s", out)
	}
	g.hooksBefore = out
	return nil
}

func (g *tmuxWorld) startAndCycle() error {
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	if err := g.waitPanes(1); err != nil {
		return err
	}
	// exercise a full cycle: a structural change (split) plus at least one reconcile.
	if out, err := g.tmux("split-window", "-t", "s1"); err != nil {
		return fmt.Errorf("split: %s", out)
	}
	return g.waitPanes(2)
}

func (g *tmuxWorld) assertHooksUnchanged() error {
	out, err := g.tmux("show-hooks", "-g")
	if err != nil {
		return fmt.Errorf("show-hooks: %s", out)
	}
	if out != g.hooksBefore {
		return fmt.Errorf("global hooks changed:\nbefore=%q\nafter=%q", g.hooksBefore, out)
	}
	return nil
}

func (g *tmuxWorld) assertNoSetHook() error {
	out, _ := exec.Command("grep", "-R", "set-hook", "../plugins/tmux").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("plugin references set-hook:\n%s", out)
	}
	return nil
}

// ---- tracked-pane scenarios (PANE-DEATH, SERVER-DOWN, KEY-GRAMMAR) ----

const trackedKey = "pane:s1:0.1@test"

func (g *tmuxWorld) startTrackedPane() error {
	g.reconcileSecs = 1
	if err := g.seedSession("s1", "", 1); err != nil { // 2 panes: 0.0, 0.1
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	return g.waitPanes(2)
}

func (g *tmuxWorld) hasFreeKey(key string) (bool, error) {
	s, err := g.freeSlots()
	if err != nil {
		return false, err
	}
	return freeKeys(s)[key], nil
}

func (g *tmuxWorld) assertTrackedFree() error {
	return eventually(g.within(), func() error {
		ok, err := g.hasFreeKey(trackedKey)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%s not free yet", trackedKey)
		}
		return nil
	})
}

func (g *tmuxWorld) killTrackedPane() error {
	if out, err := g.tmux("kill-pane", "-t", "s1:0.1"); err != nil {
		return fmt.Errorf("kill-pane: %s", out)
	}
	return nil
}

func (g *tmuxWorld) paneStale(key string) (bool, error) {
	ps, err := g.panes()
	if err != nil {
		return false, err
	}
	for _, p := range ps {
		if p.Key == key {
			return p.StaleSince != nil, nil
		}
	}
	return false, nil // gone entirely also counts as not-present; caller decides
}

func (g *tmuxWorld) assertTrackedStaleAndNotFree() error {
	return eventually(g.within(), func() error {
		stale, err := g.paneStale(trackedKey)
		if err != nil {
			return err
		}
		free, err := g.hasFreeKey(trackedKey)
		if err != nil {
			return err
		}
		if !stale || free {
			return fmt.Errorf("pane stale=%v free=%v", stale, free)
		}
		return nil
	})
}

// ---- FREE-BUSY ----

const soloKey = "pane:s1:0.0@test"

func (g *tmuxWorld) startTrackedSolo() error {
	g.reconcileSecs = 1
	if err := g.seedSession("s1", "", 0); err != nil {
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	if err := g.waitPanes(1); err != nil {
		return err
	}
	return eventually(g.within(), func() error {
		ok, err := g.hasFreeKey(soloKey)
		if err != nil || !ok {
			return fmt.Errorf("solo pane not free yet")
		}
		return nil
	})
}

func (g *tmuxWorld) runSleep() error {
	return g.sendUntil("s1:0.0", func(c string) bool { return c == "sleep" }, "sleep 30", "Enter")
}

func (g *tmuxWorld) assertTrackedNotFree() error {
	return eventually(g.within(), func() error {
		ok, err := g.hasFreeKey(soloKey)
		if err != nil {
			return err
		}
		if ok {
			return fmt.Errorf("pane still free while running sleep")
		}
		return nil
	})
}

func (g *tmuxWorld) interrupt() error {
	// wait until the real pane is back at an idle shell prompt before asserting the
	// plugin re-reads it as free (C-c returns before the shell regains the prompt).
	return g.sendUntil("s1:0.0", func(c string) bool { return idleShellCmds[c] }, "C-c")
}

func (g *tmuxWorld) assertTrackedFreeAgain() error {
	return eventually(g.within(), func() error {
		ok, err := g.hasFreeKey(soloKey)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("pane not free again")
		}
		return nil
	})
}

// ---- STALE (poll-only) ----

func (g *tmuxWorld) startPollLong() error {
	g.eventSource = "poll"
	g.reconcileSecs = 5 // long enough to observe the pre-reconcile state
	return nil
}

func (g *tmuxWorld) seedTrackedTwoPane() error {
	if err := g.seedSession("s1", "", 1); err != nil {
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	return g.waitPanes(2)
}

func (g *tmuxWorld) assertTrackedStillPresent() error {
	// Immediately after kill, before the (5s) reconcile: the row is still present
	// and not yet stale — control mode is off, so nothing heals it instantly.
	ps, err := g.panes()
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Key == trackedKey {
			if p.StaleSince != nil {
				return fmt.Errorf("pane already stale before reconcile (unexpected in poll-only)")
			}
			return nil
		}
	}
	return fmt.Errorf("pane vanished before reconcile")
}

func (g *tmuxWorld) awaitOneReconcile() error {
	return eventually(g.within(), func() error {
		stale, err := g.paneStale(trackedKey)
		if err != nil {
			return err
		}
		if !stale {
			return fmt.Errorf("reconcile has not marked stale yet")
		}
		return nil
	})
}

func (g *tmuxWorld) assertTrackedStale() error {
	stale, err := g.paneStale(trackedKey)
	if err != nil {
		return err
	}
	if !stale {
		return fmt.Errorf("pane not stale after reconcile")
	}
	return nil
}

// ---- SERVER-DOWN ----

func (g *tmuxWorld) killServer() error {
	_, _ = g.tmux("kill-server")
	return nil
}

func (g *tmuxWorld) assertAllStale() error {
	return eventually(g.within(), func() error {
		ps, err := g.panes()
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			return fmt.Errorf("no panes to check")
		}
		for _, p := range ps {
			if p.StaleSince == nil {
				return fmt.Errorf("pane %s not stale", p.Key)
			}
		}
		return nil
	})
}

func (g *tmuxWorld) assertHealth200() error {
	_, _, err := g.sw.getHealth()
	return err
}

func (g *tmuxWorld) restartTmuxServer() error {
	return g.seedSession("s1", "", 1)
}

func (g *tmuxWorld) assertRepopulated() error {
	return eventually(g.within(), func() error {
		ps, err := g.panes()
		if err != nil {
			return err
		}
		live := 0
		for _, p := range ps {
			if p.StaleSince == nil {
				live++
			}
		}
		if live < 2 {
			return fmt.Errorf("only %d live panes after restart", live)
		}
		return nil
	})
}

// ---- POLL-ERROR ----

func (g *tmuxWorld) startStubPoll() error {
	g.eventSource = "poll"
	g.reconcileSecs = 1
	g.lag = 2 // health crosses to stale ~2s after emits stop
	if err := g.writeStubTmux(); err != nil {
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	return g.waitPanes(1) // stub yields one pane on the first (successful) reconcile
}

func (g *tmuxWorld) assertTmuxHealthy() error {
	return eventually(6*time.Second, func() error {
		r, err := g.tmuxHealthRow()
		if err != nil {
			return err
		}
		if r["state"] != "ok" {
			return fmt.Errorf("tmux state %v, want ok", r["state"])
		}
		return nil
	})
}

func (g *tmuxWorld) tripStub() error {
	return os.WriteFile(g.stubTrigger, []byte("fail"), 0o644)
}

func (g *tmuxWorld) assertTmuxStale() error {
	return eventually(10*time.Second, func() error {
		r, err := g.tmuxHealthRow()
		if err != nil {
			return err
		}
		if r["state"] != "stale" {
			return fmt.Errorf("tmux state %v, want stale", r["state"])
		}
		return nil
	})
}

// ---- PANE-FOR-BRANCH ----

func (g *tmuxWorld) startBareControl() error {
	g.reconcileSecs = 1
	// a session must exist for the control client to attach; a throwaway holder
	// rooted at /tmp (NOT a git worktree) so it never matches a branch query.
	if err := g.seedSession("holder", "/tmp", 0); err != nil {
		return err
	}
	return g.writeConfigAndStart()
}

func (g *tmuxWorld) seedBranchSession() error {
	dir, err := tmpGitRepoOnBranch("plugin/tmux")
	if err != nil {
		return err
	}
	g.branchDir = dir
	return g.seedSession("feat", dir, 0)
}

func (g *tmuxWorld) seedMainSession() error {
	dir, err := tmpGitRepoOnBranch("main")
	if err != nil {
		return err
	}
	g.mainDir = dir
	if err := g.seedSession("mainwork", dir, 0); err != nil {
		return err
	}
	// wait until both branch sessions have been reconciled into the cache.
	return eventually(g.within(), func() error {
		ps, err := g.paneForBranch("plugin/tmux")
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			return fmt.Errorf("branch pane not reconciled yet")
		}
		return nil
	})
}

func (g *tmuxWorld) queryPaneForBranch() error {
	ps, err := g.paneForBranch("plugin/tmux")
	if err != nil {
		return err
	}
	g.branchPanes = ps
	return nil
}

func (g *tmuxWorld) assertBranchPane() error {
	if len(g.branchPanes) == 0 {
		return fmt.Errorf("no pane returned for branch plugin/tmux")
	}
	for _, p := range g.branchPanes {
		if p.Session != "feat" {
			return fmt.Errorf("unexpected session %q for branch plugin/tmux", p.Session)
		}
	}
	return nil
}

// ---- KEY-GRAMMAR ----

func (g *tmuxWorld) queryPanes() error {
	ps, err := g.panes()
	if err != nil {
		return err
	}
	g.branchPanes = ps
	return nil
}

func (g *tmuxWorld) assertKeyRoundTrip() error {
	if len(g.branchPanes) == 0 {
		return fmt.Errorf("no panes returned")
	}
	for _, p := range g.branchPanes {
		m := paneKeyRe.FindStringSubmatch(p.Key)
		if m == nil {
			return fmt.Errorf("key %q does not match grammar", p.Key)
		}
		if m[1] != p.Session || m[4] != "test" {
			return fmt.Errorf("key %q parts mismatch session=%q host=%q", p.Key, p.Session, m[4])
		}
	}
	return nil
}

// ---- ISOLATION ----

func (g *tmuxWorld) startWithPanicSibling() error {
	g.reconcileSecs = 1
	g.templatePanic = true
	if err := g.seedSession("s1", "", 1); err != nil {
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	return g.waitPanes(2)
}

func (g *tmuxWorld) assertTmuxNotStaleAnswers() error {
	r, err := g.tmuxHealthRow()
	if err != nil {
		return err
	}
	if r["state"] == "stale" {
		return fmt.Errorf("tmux went stale when sibling panicked")
	}
	s, err := g.freeSlots()
	if err != nil {
		return fmt.Errorf("freeSlots did not answer: %w", err)
	}
	_ = s
	return nil
}

// ---- CURSOR ----

func (g *tmuxWorld) startPollShortRecordCursor() error {
	g.eventSource = "poll"
	g.reconcileSecs = 1
	if err := g.seedSession("s1", "", 1); err != nil {
		return err
	}
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	if err := g.waitPanes(2); err != nil {
		return err
	}
	return eventually(g.within(), func() error {
		r, err := g.tmuxHealthRow()
		if err != nil {
			return err
		}
		c, _ := r["cursor"].(string)
		if c == "" {
			return fmt.Errorf("cursor not set after reconcile")
		}
		g.firstCursor = c
		return nil
	})
}

func (g *tmuxWorld) killAndRestart() error {
	_, _ = g.tmux("kill-server")
	g.sw.stopServe()
	// restart against the same dataDir/config; the tmux server stays down so no new
	// reconcile can advance the cursor — it must be resumed from disk.
	return g.sw.startServe()
}

func (g *tmuxWorld) assertCursorResumed() error {
	return eventually(6*time.Second, func() error {
		r, err := g.tmuxHealthRow()
		if err != nil {
			return err
		}
		c, _ := r["cursor"].(string)
		if c == "" {
			return fmt.Errorf("cursor empty after restart (reset, not resumed)")
		}
		if c != g.firstCursor {
			return fmt.Errorf("cursor %q != persisted %q", c, g.firstCursor)
		}
		return nil
	})
}

// ---- ZEROCORE ----

func (g *tmuxWorld) gitDiffCore() error {
	out, err := exec.Command("git", "-C", "..", "diff", "--stat", "origin/main", "--", "core/").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git diff: %s", out)
	}
	g.coreDiff = strings.TrimSpace(string(out))
	return nil
}

func (g *tmuxWorld) assertNoCoreDiff() error {
	if g.coreDiff != "" {
		return fmt.Errorf("core/ changed against main:\n%s", g.coreDiff)
	}
	return nil
}

func (g *tmuxWorld) startBareControlAssertRow() error {
	if err := g.startBareControl(); err != nil {
		return err
	}
	_, err := g.tmuxHealthRow()
	return err
}

// ---- LOC ----

func (g *tmuxWorld) runLocTmux() error {
	out, err := exec.Command("make", "-C", "..", "loc-tmux").CombinedOutput()
	g.locOut = string(out)
	if err != nil {
		return fmt.Errorf("make loc-tmux failed: %s", out)
	}
	return nil
}

func (g *tmuxWorld) assertLocWithinCap() error {
	// loc-tmux fails (non-zero) when over cap, so reaching here already implies
	// within cap; assert the reported number is present and <= 800 defensively.
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(strings.SplitN(g.locOut, ":", 2)[1]), "%d", &n); err == nil {
		if n > 800 {
			return fmt.Errorf("LOC %d exceeds cap 800", n)
		}
	}
	return nil
}

var _ = context.Background
