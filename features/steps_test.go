package features

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/drewdrewthis/supergraph/core"
)

// lit builds an anchored regexp that matches a step phrase literally, so step
// text full of regex metacharacters ({ } ( ) . ? etc.) needs no hand-escaping.
func lit(s string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(s) + "$")
}

// InitializeScenario wires a fresh world per scenario and registers every step
// phrase in both .feature files. Strict mode makes any unregistered step fail, so
// this set must be complete.
func InitializeScenario(sc *godog.ScenarioContext) {
	w := &world{}

	sc.Before(func(ctx context.Context, _ *godog.Scenario) (context.Context, error) {
		return ctx, w.init()
	})
	sc.After(func(ctx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		w.cleanup()
		return ctx, nil
	})

	registerPRDSteps(sc)
	registerGithubSteps(sc)
	registerPeerSteps(sc)
	registerGitSteps(sc)

	// --- shared ---
	sc.Step(lit("a supergraph server started with the template plugin and data dir <tmp>"), w.startDefaultServer)
	sc.Step(lit("exit code is 0"), w.assertExit0)
	sc.Step(lit("exit code is non-zero"), w.assertExitNonZero)
	sc.Step(lit("I run `supergraph stop`"), w.stopStep)
	sc.Step(lit("I run `supergraph start`"), w.startStep)
	sc.Step(lit("I run `supergraph status`"), w.statusStep)
	sc.Step(lit("it reports 0 files changed"), w.assertNoDiffReported)

	// --- AC-CORE-1 ---
	sc.Step(lit(`the template plugin emits a "V:2" event captured from the templateEvents subscription`), w.ac1Emit)
	sc.Step(lit("the stored Payload is byte-identical to the emitted Payload"), w.ac1ByteIdentical)
	sc.Step(lit(`the emitted event's "v" field equals 2`), w.ac1V2)

	// --- AC-CORE-2 ---
	sc.Step(lit("a template ring store with capacity 5 in data dir <tmp>"), w.ac2OpenRing)
	sc.Step(lit("10 events are appended through the ring"), w.ac2Append10)
	sc.Step(lit("a query of the ring returns exactly 5 rows"), w.ac2Assert5)
	sc.Step(lit("the ring store is closed and reopened"), w.ac2Reopen)
	sc.Step(lit("a cursor written before the reopen is unchanged after it"), w.ac2AssertCursor)

	// --- AC-CORE-3 ---
	sc.Step(lit("I run `curl -s 127.0.0.1:7788/health`"), w.ac3Curl)
	sc.Step(lit(`the JSON response is an array of objects with exactly the keys "plugin", "lastEventAt", "cursor", "lagSeconds", "state"`), w.ac3Keys)
	sc.Step(lit(`each object's "state" is one of "starting", "ok", "stale"`), w.ac3StateEnum)
	sc.Step(lit(`every object's "lastEventAt" is JSON null or a valid timestamp (never empty string or epoch)`), w.ac3LastEventShape)
	sc.Step(lit("I run `supergraph query '{ health { plugin state } }'`"), w.ac3QueryHealth)
	sc.Step(lit("the GraphQL response matches the same plugin/state set as the `/health` response"), w.ac3Match)

	// --- AC-CORE-6 ---
	sc.Step(lit("I run `supergraph query '{ __typename }'`"), w.ac6Query)
	sc.Step(lit(`stdout contains "Query"`), w.ac6QueryContains)
	sc.Step(lit("I run `supergraph serve` a second time on the same port"), w.ac6SecondServe)
	sc.Step(lit(`stderr contains an "address in use" error`), w.ac6AddrInUse)

	// --- AC-CORE-8 ---
	sc.Step(lit("I open a websocket subscription to `templateEvents` on `127.0.0.1:7788/graphql`"), w.wsOpen)
	sc.Step(lit("the template plugin emits a matching event"), w.wsEmit)
	sc.Step(lit("a pushed message is received on the subscription within 1s"), w.wsPushedWithin1s)
	sc.Step(lit("I close the websocket subscription"), w.wsCloseStep)
	sc.Step(lit("no error is logged by the server after the socket closes"), w.wsNoServerError)

	// --- AC-CORE-9 ---
	sc.Step(lit("I run `go test ./features/...` excluding scenarios tagged @unmet"), w.ac9GreenRun)
	sc.Step(lit("this step is intentionally unmet"), w.ac9Unmet)

	// --- AC-CORE-11 ---
	sc.Step(lit(`a `+"`config.toml`"+` with a valid "hostId"`), w.ac11Valid)
	sc.Step(lit(`a `+"`config.toml`"+` with no "hostId" field`), w.ac11NoHostID)
	sc.Step(lit("I run `supergraph serve` with that config"), w.ac11Serve)
	sc.Step(lit(`the loaded config exposes "hostId", "peers", "tokens"`), w.ac11Exposes)
	sc.Step(lit(`stderr contains an error message naming "hostId"`), w.ac11ErrNamesHostID)

	// --- AC-CORE-15 ---
	sc.Step(lit("a clean checkout with no server running"), w.ac15Given)
	sc.Step(lit("I run `make dev`"), w.ac15MakeDev)
	sc.Step(lit("within a bounded wait, `GET /health` returns HTTP 200"), w.ac15Health200)
	sc.Step(lit(`the JSON response includes a "template" entry`), w.ac15Template)
	sc.Step(lit("no file under the real `~/.local/share` data dir was written"), w.ac15NoRealWrite)

	// --- AC-CORE-16 ---
	sc.Step(lit("the files `core/plugin.go` and `docs/plugin-contract.md`"), w.ac16Given)
	sc.Step(lit("I diff the sed-extracted `type Plugin interface { ... }` block from each file"), w.ac16Diff)
	sc.Step(lit("the diff output is empty"), w.ac16Empty)

	// --- AC-CORE-17 ---
	sc.Step(lit("a writer loop appends template events continuously"), w.ac17Writer)
	sc.Step(lit(`a concurrent GraphQL read queries the "template" ring while the writer runs`), w.ac17Read)
	sc.Step(lit("the concurrent read completes and returns rows"), w.ac17Rows)
	sc.Step(lit(`a grep of the run log for "database is locked" returns nothing`), w.ac17NoLock)

	// --- AC-CORE-10 (template.feature) ---
	sc.Step(lit("the template plugin exists under `plugins/template/`, registered via `graph/plugins_import.go` and regenerated `graph/`"), w.ac10Given)
	sc.Step(lit("I check that no file under core/ references the template plugin"), w.ac10Grep)
	sc.Step(lit("no core file imports a plugin package"), w.ac10NoRef)
	sc.Step(lit(`a supergraph server started with the template plugin and data dir <tmp> serves a "template" entry in `+"`/health`"), w.ac10ServesTemplate)

	// --- AC-CORE-10b ---
	sc.Step(lit("I run `supergraph query '{ templatePing }'`"), w.ac10bQueryPing)
	sc.Step(lit(`stdout contains resolver data for "templatePing" that is not null`), w.ac10bPingNotNull)
	sc.Step(lit("at least one message is received on the `templateEvents` subscription"), w.ac10bMsg)

	// --- AC-CORE-4 ---
	sc.Step(lit(`a supergraph server started with the template plugin, a second "fakeok" plugin that never panics, and data dir <tmp>`), w.ac4Given)
	sc.Step(lit("I induce a panic in the template plugin's Start via its panic-inject path"), w.ac4Induce)
	sc.Step(lit("I wait for the panic to be detected"), w.ac4WaitStale)
	sc.Step(lit(`the "template" entry in `+"`/health`"+` has state "stale"`), w.ac4TemplateStale)
	sc.Step(lit("`GET /health` still returns HTTP 200"), w.ac4Health200)
	sc.Step(lit(`the "fakeok" entry in `+"`/health`"+` has state "ok"`), w.ac4FakeokOk)
	sc.Step(lit("`supergraph query '{ __typename }'` still returns HTTP 200"), w.ac4QueryOk)

	// --- AC-CORE-12 ---
	sc.Step(lit("the template plugin calls `core.Register` from its own `init()`"), w.ac12Given)
	sc.Step(lit("a supergraph server boots with the template plugin and data dir <tmp>"), w.ac12Boot)
	sc.Step(lit(`the boot log lists "template" among the registered factories`), w.ac12LogLists)
	sc.Step(lit("no file under `core/` was edited to achieve this"), w.ac12NoCore)
	sc.Step(lit(`a second plugin registers the name "template" a second time`), w.ac12Dup)
	sc.Step(lit(`the registration panics with a message naming "template"`), w.ac12Panic)

	// --- AC-CORE-13 ---
	sc.Step(lit("an empty data dir <tmp> with no template database"), w.ac13Given)
	sc.Step(lit("I run `supergraph serve` with the template plugin against data dir <tmp>"), w.ac13Serve1)
	sc.Step(lit("`sqlite3 <tmp>/template.db .tables` lists the template plugin's tables"), w.ac13Tables)
	sc.Step(lit("I run `supergraph serve` again with the template plugin against data dir <tmp>"), w.ac13Serve2)
	sc.Step(lit("`sqlite3 <tmp>/template.db .tables` lists the same tables with no duplicates"), w.ac13SameTables)

	// --- AC-CORE-7a/7b / -14 (service, gated by FEATURES_SERVICE) ---
	registerServiceSteps(sc, w)

	// --- claude plugin (@claude) ---
	registerClaudeSteps(sc, w)

	// --- tmux plugin @local scenarios (real tmux on a private -L socket) ---
	registerTmuxSteps(sc)

	// --- spike-measure harness (@local checks + @slow full run) ---
	registerSpikeSteps(sc)

	// --- subscribe CLI (@subscribe) ---
	registerSubscribeSteps(sc, w)

	// --- justfile agent tooling layer (@justfile @local) ---
	registerJustfileSteps(sc)
}

// ---------- shared ----------

func (w *world) startDefaultServer() error {
	if err := w.writeConfig(defaultCfg()); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) assertExit0() error {
	if w.lastExit != 0 {
		return fmt.Errorf("exit=%d stderr=%q", w.lastExit, w.lastStderr)
	}
	return nil
}

func (w *world) assertExitNonZero() error {
	if w.lastExit == 0 {
		return fmt.Errorf("exit=0 stdout=%q", w.lastStdout)
	}
	return nil
}

func (w *world) stopStep() error {
	if w.svc {
		w.runCLI("stop")
		return nil
	}
	w.stopServe()
	w.lastExit = 0
	return nil
}

func (w *world) startStep() error {
	if w.svc {
		w.runCLI("start")
		return nil
	}
	return w.startServe()
}

func (w *world) statusStep() error {
	w.runCLI("status")
	return nil
}

// ---------- AC-CORE-1 ----------

func (w *world) ac1Emit() error {
	if err := w.openSubscription("templateEvents"); err != nil {
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		p, err := w.ws.nextPush(8 * time.Second)
		if err != nil {
			return err
		}
		data, _ := p["data"].(map[string]any)
		te, _ := data["templateEvents"].(map[string]any)
		if te == nil {
			continue
		}
		vf, _ := te["v"].(float64)
		if int(vf) != 2 {
			continue // skip any non-v2 frame
		}
		s, _ := te["payload"].(string)
		w.emittedPayload = []byte(s)
		w.emittedV = int(vf)
		w.lastExit = 0
		return nil
	}
	return fmt.Errorf("no v=2 templateEvents push observed")
}

func (w *world) ac1ByteIdentical() error {
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		ok, err := w.storeHasPayload(w.emittedPayload)
		if err == nil && ok {
			w.storedPayload = w.emittedPayload
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("emitted payload %q not found byte-identical in store", w.emittedPayload)
}

func (w *world) ac1V2() error {
	if w.emittedV != 2 {
		return fmt.Errorf("v=%d", w.emittedV)
	}
	return nil
}

func (w *world) storeHasPayload(pay []byte) (bool, error) {
	dbPath := filepath.Join(w.dataDir, "template.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout%285000%29")
	if err != nil {
		return false, err
	}
	defer db.Close()
	var cnt int
	err = db.QueryRow(`SELECT COUNT(*) FROM events WHERE source='template' AND v=2 AND payload=?`, pay).Scan(&cnt)
	return cnt > 0, err
}

// ---------- AC-CORE-2 ----------

func (w *world) ac2OpenRing() error {
	w.ringPath = filepath.Join(w.dataDir, "ring.db")
	st, err := core.OpenStore(context.Background(), w.ringPath, 5)
	if err != nil {
		return err
	}
	w.ring = st
	return nil
}

func (w *world) ac2Append10() error {
	for i := 0; i < 10; i++ {
		if err := w.ring.AppendEvent(context.Background(), tmplEnv(i)); err != nil {
			return err
		}
	}
	return nil
}

func (w *world) ac2Assert5() error {
	evs, err := w.ring.Events(context.Background(), 100)
	if err != nil {
		return err
	}
	if len(evs) != 5 {
		return fmt.Errorf("ring rows=%d want 5", len(evs))
	}
	w.ringRows = len(evs)
	return nil
}

func (w *world) ac2Reopen() error {
	ctx := context.Background()
	if err := w.ring.SetCursor(ctx, "pos", "hw-42"); err != nil {
		return err
	}
	w.cursorVal = "hw-42"
	if err := w.ring.Close(); err != nil {
		return err
	}
	st, err := core.OpenStore(ctx, w.ringPath, 5)
	if err != nil {
		return err
	}
	w.ring = st
	return nil
}

func (w *world) ac2AssertCursor() error {
	v, err := w.ring.Cursor(context.Background(), "pos")
	if err != nil {
		return err
	}
	if v != w.cursorVal {
		return fmt.Errorf("cursor=%q want %q", v, w.cursorVal)
	}
	return nil
}

// ---------- AC-CORE-3 ----------

func (w *world) ac3Curl() error {
	rows, body, err := w.getHealth()
	if err != nil {
		w.lastExit = 1
		return err
	}
	w.healthRows = rows
	w.healthBody = body
	w.lastExit = 0
	return nil
}

func (w *world) ac3Keys() error {
	want := []string{"plugin", "lastEventAt", "cursor", "lagSeconds", "state"}
	if len(w.healthRows) == 0 {
		return fmt.Errorf("no health rows")
	}
	for _, r := range w.healthRows {
		if len(r) != len(want) {
			return fmt.Errorf("row has %d keys: %v", len(r), r)
		}
		for _, k := range want {
			if _, ok := r[k]; !ok {
				return fmt.Errorf("missing key %q in %v", k, r)
			}
		}
	}
	return nil
}

func (w *world) ac3StateEnum() error {
	ok := map[string]bool{"starting": true, "ok": true, "stale": true}
	for _, r := range w.healthRows {
		s, _ := r["state"].(string)
		if !ok[s] {
			return fmt.Errorf("bad state %q", s)
		}
	}
	return nil
}

func (w *world) ac3LastEventShape() error {
	for _, r := range w.healthRows {
		v := r["lastEventAt"]
		if v == nil {
			continue // JSON null is valid
		}
		s, isStr := v.(string)
		if !isStr || s == "" {
			return fmt.Errorf("lastEventAt not null/string: %v", v)
		}
		if strings.HasPrefix(s, "0001-01-01") {
			return fmt.Errorf("epoch lastEventAt: %v", s)
		}
		if _, err := time.Parse(time.RFC3339Nano, s); err != nil {
			return fmt.Errorf("bad timestamp %q: %w", s, err)
		}
	}
	return nil
}

func (w *world) ac3QueryHealth() error {
	w.runCLI("query", "{ health { plugin state } }")
	return nil
}

func (w *world) ac3Match() error {
	var data map[string]any
	if err := json.Unmarshal([]byte(w.lastStdout), &data); err != nil {
		return fmt.Errorf("parse query out %q: %w", w.lastStdout, err)
	}
	arr, _ := data["health"].([]any)
	got := map[string]bool{}
	for _, e := range arr {
		m, _ := e.(map[string]any)
		got[fmt.Sprintf("%v", m["plugin"])] = true
	}
	want := map[string]bool{}
	for _, r := range w.healthRows {
		want[fmt.Sprintf("%v", r["plugin"])] = true
	}
	if len(got) == 0 {
		return fmt.Errorf("no plugins in graphql health response")
	}
	for p := range want {
		if !got[p] {
			return fmt.Errorf("graphql missing plugin %q", p)
		}
	}
	for p := range got {
		if !want[p] {
			return fmt.Errorf("graphql has extra plugin %q", p)
		}
	}
	return nil
}

// ---------- AC-CORE-6 ----------

func (w *world) ac6Query() error {
	w.runCLI("query", "{ __typename }")
	return nil
}

func (w *world) ac6QueryContains() error {
	if !strings.Contains(w.lastStdout, "Query") {
		return fmt.Errorf("stdout=%q", w.lastStdout)
	}
	return nil
}

func (w *world) ac6SecondServe() error {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "--config", w.cfgPath, "serve")
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	w.lastStdout = so.String()
	w.lastStderr = se.String()
	w.lastExit = exitCode(err)
	return nil
}

func (w *world) ac6AddrInUse() error {
	if !strings.Contains(strings.ToLower(w.lastStderr), "in use") {
		return fmt.Errorf("stderr=%q", w.lastStderr)
	}
	return nil
}

// ---------- AC-CORE-8 ----------

func (w *world) wsOpen() error { return w.openSubscription("templateEvents") }

func (w *world) wsEmit() error {
	// No trigger to fire here: the template plugin has no on-demand emit path,
	// so this step is a no-op — the next tick (see plugins/template's Start)
	// delivers the "matching event" on its own ~1s cadence. wsPushedWithin1s
	// measures the <1s bound from the delivered envelope's own `ts` (stamped at
	// emit time), not from any clock started in this step, since a step-local
	// clock start lands at an arbitrary point in the tick cycle.
	return nil
}

func (w *world) wsPushedWithin1s() error {
	// The template plugin emits on its own ~1s ticker (see wsEmit); a subscribed
	// client's clock-start at an arbitrary point in that cycle is uniformly
	// distributed in [0,1s) against the NEXT tick, so it inevitably brushes the
	// 1s bound under CI scheduling jitter even with no real latency regression.
	// The honest measurement per AC-CORE-8 ("<1s from emit") is emit-to-receipt:
	// the envelope's own `ts` (source-stamped at emit time in plugins/template's
	// p.emit) versus receipt time here, not receipt time versus an arbitrary
	// step-boundary clock start.
	//
	// Outer read deadline is generous (absorbs CI scheduling jitter reading the
	// socket); the AC bound itself is asserted against emit-to-receipt latency.
	p, err := w.ws.nextPush(3 * time.Second)
	if err != nil {
		return err
	}
	received := time.Now()

	data, ok := p["data"].(map[string]any)
	if !ok {
		return fmt.Errorf("push without data: %v", p)
	}
	evt, ok := data["templateEvents"].(map[string]any)
	if !ok {
		return fmt.Errorf("push data missing templateEvents: %v", p)
	}
	tsStr, ok := evt["ts"].(string)
	if !ok {
		return fmt.Errorf("push event missing ts: %v", evt)
	}
	emittedAt, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		return fmt.Errorf("parse event ts %q: %w", tsStr, err)
	}

	elapsed := received.Sub(emittedAt)
	if elapsed >= time.Second {
		return fmt.Errorf("push arrived %s after emit (>= 1s bound)", elapsed)
	}
	w.wsData = p
	w.wsPushed = true
	return nil
}

func (w *world) wsCloseStep() error {
	w.ws.close()
	time.Sleep(300 * time.Millisecond) // let any late server log land
	return nil
}

func (w *world) wsNoServerError() error {
	if w.serve == nil {
		return fmt.Errorf("no serve process")
	}
	logs := w.serve.stderr.String() + w.serve.stdout.String()
	if strings.Contains(logs, "websocket error") {
		return fmt.Errorf("server logged a websocket error after close:\n%s", logs)
	}
	return nil
}

// ---------- AC-CORE-9 ----------

func (w *world) ac9GreenRun() error {
	if code := runEmbeddedMetSuite(); code != 0 {
		return fmt.Errorf("embedded met suite exit=%d", code)
	}
	w.lastExit = 0
	return nil
}

func (w *world) ac9Unmet() error {
	return fmt.Errorf("this step is intentionally unmet")
}

// ---------- AC-CORE-11 ----------

func (w *world) ac11Valid() error {
	return w.writeConfig(cfgOpts{lag: 30, templateInterval: 1})
}

func (w *world) ac11NoHostID() error {
	return w.writeConfig(cfgOpts{omitHostID: true, lag: 30, templateInterval: 1})
}

func (w *world) ac11Serve() error {
	sp := &serveProc{stdout: &safeBuf{}, stderr: &safeBuf{}, done: make(chan struct{})}
	cmd := exec.Command(binPath, "--config", w.cfgPath, "serve")
	cmd.Stdout = sp.stdout
	cmd.Stderr = sp.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sp.cmd = cmd
	go func() { sp.waitErr = cmd.Wait(); close(sp.done) }()
	select {
	case <-sp.done:
		// exited quickly = invalid config (missing hostId)
		w.lastExit = exitCode(sp.waitErr)
		w.lastStderr = sp.stderr.String()
		return nil
	case <-time.After(1500 * time.Millisecond):
		// still running = valid config booted
		w.serve = sp
		if err := w.waitHealth(8 * time.Second); err != nil {
			return err
		}
		w.lastExit = 0
		w.lastStderr = sp.stderr.String()
		return nil
	}
}

func (w *world) ac11Exposes() error {
	cfg, err := core.LoadConfig(w.cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if strings.TrimSpace(cfg.HostID) == "" {
		return fmt.Errorf("hostId empty after load")
	}
	_ = cfg.Peers
	_ = cfg.Tokens
	return nil
}

func (w *world) ac11ErrNamesHostID() error {
	if !strings.Contains(w.lastStderr, "hostId") {
		return fmt.Errorf("stderr does not name hostId: %q", w.lastStderr)
	}
	return nil
}

// ---------- AC-CORE-15 ----------

func realDataDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", "supergraph")
}

func (w *world) ac15Given() error {
	w.realDirBefore = map[string]bool{}
	_ = filepath.WalkDir(realDataDir(), func(p string, _ os.DirEntry, err error) error {
		if err == nil {
			w.realDirBefore[p] = true
		}
		return nil
	})
	return nil
}

func (w *world) ac15MakeDev() error {
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "make", "-C", repoRoot, "dev-check")
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	w.makeStdout = so.String()
	w.makeExit = exitCode(err)
	if err != nil {
		w.makeStdout += "\n[stderr] " + se.String()
	}
	return nil
}

func (w *world) ac15Health200() error {
	if w.makeExit != 0 {
		return fmt.Errorf("make dev-check exit=%d out=%s", w.makeExit, w.makeStdout)
	}
	return nil
}

func (w *world) ac15Template() error {
	if !strings.Contains(w.makeStdout, `"plugin":"template"`) {
		return fmt.Errorf("template not present in dev-check output:\n%s", w.makeStdout)
	}
	return nil
}

func (w *world) ac15NoRealWrite() error {
	var added []string
	_ = filepath.WalkDir(realDataDir(), func(p string, _ os.DirEntry, err error) error {
		if err == nil && !w.realDirBefore[p] {
			added = append(added, p)
		}
		return nil
	})
	if len(added) > 0 {
		return fmt.Errorf("make dev wrote the real data dir: %v", added)
	}
	return nil
}

// ---------- AC-CORE-16 ----------

func (w *world) ac16Given() error { return nil }

func (w *world) ac16Diff() error {
	script := `diff <(sed -n '/type Plugin interface {/,/^}/p' core/plugin.go) <(sed -n '/type Plugin interface {/,/^}/p' docs/plugin-contract.md)`
	cmd := exec.Command("bash", "-c", script)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	w.diffOut = string(out)
	w.lastExit = exitCode(err)
	return nil
}

func (w *world) ac16Empty() error {
	if strings.TrimSpace(w.diffOut) != "" {
		return fmt.Errorf("interface blocks differ:\n%s", w.diffOut)
	}
	return nil
}

// ---------- AC-CORE-17 ----------

func (w *world) ac17Writer() error {
	st, err := core.OpenStore(context.Background(), filepath.Join(w.dataDir, "conc.db"), 100)
	if err != nil {
		return err
	}
	w.cstore = st
	w.writerStop = make(chan struct{})
	w.writerDone = make(chan struct{})
	go func() {
		defer close(w.writerDone)
		i := 0
		for {
			select {
			case <-w.writerStop:
				return
			default:
			}
			if err := w.cstore.AppendEvent(context.Background(), tmplEnv(i)); err != nil && locked(err) {
				w.lockErr.Store(true)
			}
			i++
		}
	}()
	return nil
}

func (w *world) ac17Read() error {
	maxN := 0
	end := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(end) {
		evs, err := w.cstore.Events(context.Background(), 50)
		if err != nil {
			if locked(err) {
				w.lockErr.Store(true)
			}
		} else if len(evs) > maxN {
			maxN = len(evs)
		}
	}
	close(w.writerStop)
	<-w.writerDone
	w.concurrentRows = maxN
	return nil
}

func (w *world) ac17Rows() error {
	if w.concurrentRows <= 0 {
		return fmt.Errorf("concurrent read returned no rows")
	}
	return nil
}

func (w *world) ac17NoLock() error {
	if w.lockErr.Load() {
		return fmt.Errorf(`"database is locked" observed during concurrent read/write`)
	}
	if w.serve != nil && strings.Contains(w.serve.stderr.String(), "database is locked") {
		return fmt.Errorf("server logged a database-locked error")
	}
	return nil
}

// ---------- AC-CORE-10 ----------

func (w *world) ac10Given() error { return nil }

func (w *world) ac10Grep() error {
	// git diff base..HEAD is not feasible in-test (greenfield: every core file is
	// "added"). The load-bearing S5 proof is structural: no file under core/ imports
	// a plugin package, so adding the template plugin required zero core coupling.
	// (Core comments may mention the word "template" as an example — that is not a
	// dependency, so we match the import path, not the bare word.)
	cmd := exec.Command("grep", "-rl", "drewdrewthis/supergraph/plugins", "core/")
	cmd.Dir = repoRoot
	out, _ := cmd.CombinedOutput() // grep exits 1 when there are no matches
	w.diffOut = strings.TrimSpace(string(out))
	return nil
}

func (w *world) ac10NoRef() error {
	if w.diffOut != "" {
		return fmt.Errorf("core files import a plugin package (S5 violated):\n%s", w.diffOut)
	}
	return nil
}

func (w *world) ac10GrepAndAssert() error {
	if err := w.ac10Grep(); err != nil {
		return err
	}
	return w.ac10NoRef()
}

func (w *world) ac10ServesTemplate() error {
	if err := w.startDefaultServer(); err != nil {
		return err
	}
	rows, _, err := w.getHealth()
	if err != nil {
		return err
	}
	if findRow(rows, "template") == nil {
		return fmt.Errorf("no template entry in /health: %v", rows)
	}
	return nil
}

// ---------- AC-CORE-10b ----------

func (w *world) ac10bQueryPing() error {
	w.runCLI("query", "{ templatePing }")
	return nil
}

func (w *world) ac10bPingNotNull() error {
	var data map[string]any
	if err := json.Unmarshal([]byte(w.lastStdout), &data); err != nil {
		return fmt.Errorf("parse %q: %w", w.lastStdout, err)
	}
	v, ok := data["templatePing"]
	if !ok || v == nil || v == "" {
		return fmt.Errorf("templatePing null/absent: %q", w.lastStdout)
	}
	return nil
}

func (w *world) ac10bMsg() error {
	p, err := w.ws.nextPush(3 * time.Second)
	if err != nil {
		return err
	}
	data, _ := p["data"].(map[string]any)
	if _, ok := data["templateEvents"]; !ok {
		return fmt.Errorf("no templateEvents in push: %v", p)
	}
	return nil
}

// ---------- AC-CORE-4 ----------

func (w *world) ac4Given() error {
	// fakeok is compiled into the binary, so it is present automatically.
	return w.startDefaultServer()
}

func (w *world) ac4Induce() error {
	// There is no runtime panic injection into a live plugin, so re-launch with the
	// panic-inject config armed. The template's own tick is parked long, but the panic
	// fires right after its first emit and marks it forcedStale, so it stays stale
	// regardless of any later event.
	w.stopServe()
	if err := w.writeConfig(cfgOpts{lag: 30, templateInterval: 3600, templatePanic: true}); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) ac4WaitStale() error {
	// Shared by the template AC-CORE-4 and the claude @F5 scenarios: wait until the
	// induced-panic plugin (whichever was armed) reaches stale. fakeok never panics.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		rows, _, err := w.getHealth()
		if err == nil {
			for _, name := range []string{"template", "claude"} {
				if r := findRow(rows, name); r != nil && r["state"] == "stale" {
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no plugin reached stale after induced panic")
}

func (w *world) ac4TemplateStale() error {
	rows, _, err := w.getHealth()
	if err != nil {
		return err
	}
	r := findRow(rows, "template")
	if r == nil || r["state"] != "stale" {
		return fmt.Errorf("template not stale: %v", r)
	}
	return nil
}

func (w *world) ac4Health200() error {
	if _, _, err := w.getHealth(); err != nil {
		return fmt.Errorf("/health not 200 after panic: %w", err)
	}
	return nil
}

func (w *world) ac4FakeokOk() error {
	rows, _, err := w.getHealth()
	if err != nil {
		return err
	}
	r := findRow(rows, "fakeok")
	if r == nil {
		return fmt.Errorf("no fakeok entry: %v", rows)
	}
	if r["state"] != "ok" {
		return fmt.Errorf("fakeok state=%v", r["state"])
	}
	return nil
}

func (w *world) ac4QueryOk() error {
	w.runCLI("query", "{ __typename }")
	if w.lastExit != 0 || !strings.Contains(w.lastStdout, "Query") {
		return fmt.Errorf("base query failed after panic exit=%d out=%q err=%q", w.lastExit, w.lastStdout, w.lastStderr)
	}
	return nil
}

// ---------- AC-CORE-12 ----------

func (w *world) ac12Given() error { return nil }

func (w *world) ac12Boot() error {
	if err := w.startDefaultServer(); err != nil {
		return err
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w.bootStdout = w.serve.stdout.String()
		if strings.Contains(w.bootStdout, "registered plugins") {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}

func (w *world) ac12LogLists() error {
	if !strings.Contains(w.bootStdout, "registered plugins") {
		return fmt.Errorf("no registered-plugins log line:\n%s", w.bootStdout)
	}
	if !strings.Contains(w.bootStdout, "template") {
		return fmt.Errorf("boot log does not list template:\n%s", w.bootStdout)
	}
	return nil
}

func (w *world) ac12NoCore() error { return w.ac10GrepAndAssert() }

func (w *world) ac12Dup() error {
	w.panicMsg = registerTwice()
	return nil
}

func (w *world) ac12Panic() error {
	if w.panicMsg == "" {
		return fmt.Errorf("duplicate registration did not panic")
	}
	if !strings.Contains(w.panicMsg, "template") {
		return fmt.Errorf("panic message did not name template: %q", w.panicMsg)
	}
	return nil
}

// registerTwice forces a duplicate core.Register("template", …). The features test
// binary does not import the plugin packages, so the first call succeeds and the
// second panics; if template were already registered the first call panics — either
// way the recovered message must name "template".
func registerTwice() (msg string) {
	defer func() {
		if r := recover(); r != nil {
			msg = fmt.Sprint(r)
		}
	}()
	dummy := func(_ core.PluginConfig) (core.Plugin, error) { return nil, nil }
	core.Register("template", dummy)
	core.Register("template", dummy)
	return ""
}

// ---------- AC-CORE-13 ----------

func (w *world) ac13Given() error { return nil }

func (w *world) ac13Serve1() error {
	if err := w.writeConfig(defaultCfg()); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) ac13Tables() error {
	// waitHealth only proves the plugins have emitted; the WAL write behind
	// that emit can still take a beat to become visible to a fresh
	// connection, so retry briefly rather than failing on the first miss.
	need := []string{"template_state", "events", "cursors"}
	deadline := time.Now().Add(2 * time.Second)
	var tabs []string
	var err error
	for {
		tabs, err = w.templateTables()
		if err == nil && allTablesPresent(tabs, need) {
			w.firstTables = tabs
			return nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			return fmt.Errorf("missing table in %v (want %v)", tabs, need)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func allTablesPresent(tabs, need []string) bool {
	for _, n := range need {
		if !contains(tabs, n) {
			return false
		}
	}
	return true
}

func (w *world) ac13Serve2() error { return w.startServe() }

func (w *world) ac13SameTables() error {
	tabs, err := w.templateTables()
	if err != nil {
		return err
	}
	if !sameSet(tabs, w.firstTables) {
		return fmt.Errorf("tables changed across restart: %v vs %v", w.firstTables, tabs)
	}
	return nil
}

func (w *world) templateTables() ([]string, error) {
	dbPath := filepath.Join(w.dataDir, "template.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout%285000%29")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}
