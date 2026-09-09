package features

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/cucumber/godog"
)

// registerClaudeStateFileSteps wires the issue #28 scenarios: the state-file ingest
// channel (statefile.go), the mission/lastResponse/paneTitle columns, the pane-title
// tmux path, tilde expansion, and the privacy carve-out. Kept in its own file so the
// pre-existing claude steps stay untouched.
func registerClaudeStateFileSteps(sc *godog.ScenarioContext, w *world) {
	// server-start variants
	sc.Step(lit("a supergraph server started with the claude plugin, a state dir, and data dir <tmp>"), w.startStateDirServer)
	sc.Step(lit("a supergraph server started with the claude plugin, a state dir, pane titles enabled, and a stub tmux reporting pane \"%7\" title \"issue28-worker\""), w.startPaneTitleOK)
	sc.Step(lit("a supergraph server started with the claude plugin, a state dir, pane titles enabled, and a stub tmux that exits non-zero"), w.startPaneTitleFail)
	sc.Step(lit("a supergraph server started with the claude plugin, a state dir, pane titles disabled, and a stub tmux on PATH"), w.startPaneTitlesOff)
	sc.Step(lit("a supergraph server started with the claude plugin, a state dir configured as \x60~/state\x60, and data dir <tmp>"), w.startTildeServer)

	// generic scan trigger — assertions poll, so this is a marker step
	sc.Step(lit("the state-file channel scans once"), noop)
	sc.Step(lit("the same unchanged state dir is scanned again"), noop)

	// state-file writers
	sc.Step(lit("a state file for session \x60M1\x60 with pane \"%7\", a live pid, cwd \"/w/x\", and state \"input\""), w.stateFileM1)
	sc.Step(lit("a state file for session \x60M3\x60 with state \"idle\""), w.stateFileM3)
	sc.Step(lit("session \x60M2\x60 folded from a \x60PreToolUse\x60 hook to state \"working\" with toolCalls 2"), w.m2Folded)
	sc.Step(lit("a state file for session \x60M2\x60 with state \"idle\" and \x60first_prompt\x60 \"MISSION-TEXT\""), w.stateFileM2Idle)
	sc.Step(lit("a state file creates session \x60M4\x60 with state \"idle\" and \x60first_prompt\x60 \"MISSION-TEXT\""), w.stateFileM4Create)
	sc.Step(lit("a state file for session \x60MT\x60 whose \x60first_prompt\x60 is a 500-rune multibyte string"), w.stateFileMT)
	sc.Step(lit("a state file for session \x60MF\x60 with an empty \x60first_prompt\x60 is scanned"), w.stateFileMF)
	sc.Step(lit("a state file for session \x60LR\x60 with \x60last_response\x60 \"R-BODY\""), w.stateFileLR)
	sc.Step(lit("a state file for session \x60LR2\x60 with a 500-rune \x60last_response\x60 is scanned"), w.stateFileLR2)
	sc.Step(lit("the state dir contains a truncated invalid-JSON file and a valid state file for \x60V1\x60"), w.stateDirMalformed)
	sc.Step(`^a state file for session \x60([A-Za-z0-9]+)\x60 with pane "([^"]*)"$`, w.stateFilePane)
	sc.Step(lit("a state file for session \x60SEC\x60 with \x60first_prompt\x60 \"SECRET-MISSION-STRING\", \x60last_response\x60 \"SECRET-RESPONSE-STRING\", and cwd \"/w/sec\""), w.stateFileSEC)
	sc.Step(lit("a state file containing \x60first_prompt\x60 \"SECRET-MISSION-STRING\" and \x60last_response\x60 \"SECRET-RESPONSE-STRING\" is present while stateDir is empty"), w.stateFileSecretsDisabled)
	sc.Step(lit("a state file for session \x60TD\x60 under the home-relative state dir"), w.stateFileTilde)

	// assertions
	sc.Step(lit("\x60claudeSession(sessionId: \"M1\")\x60 has state \"input\" and cwd \"/w/x\""), w.assertM1)
	sc.Step(lit("a \x60ClaudeInstance\x60 exists with \x60pane\x60 \"%7\" and a non-zero \x60pid\x60 whose \x60session\x60 is \x60M1\x60"), w.assertInstanceM1)
	sc.Step(lit("a \x60claude.session.updated\x60 envelope is pushed for \x60M3\x60"), w.assertM3Emitted)
	sc.Step(lit("no further \x60claude.session.updated\x60 envelope is pushed for \x60M3\x60"), w.assertNoFurtherM3)
	sc.Step(lit("\x60claudeSession(sessionId: \"MT\")\x60 \x60mission\x60 is 120 runes, valid UTF-8, and a prefix of the first prompt"), w.assertMissionMT)
	sc.Step(lit("\x60claudeSession(sessionId: \"MF\")\x60 \x60mission\x60 is null"), w.assertMissionMFNull)
	sc.Step(lit("\x60claudeSession(sessionId: \"LR\")\x60 \x60lastResponse\x60 is \"R-BODY\""), w.assertLR)
	sc.Step(lit("\x60claudeSession(sessionId: \"LR2\")\x60 \x60lastResponse\x60 is 200 runes"), w.assertLR2)
	sc.Step(lit("\x60claudeSession(sessionId: \"M2\")\x60 has state \"working\", toolCalls 2, and mission \"MISSION-TEXT\""), w.assertM2Reconcile)
	sc.Step(lit("\x60claudeSession(sessionId: \"M4\")\x60 has state \"working\" and mission \"MISSION-TEXT\""), w.assertM4Reconcile)
	sc.Step(`^\x60claudeSession\(sessionId: "([A-Za-z0-9]+)"\)\x60 exists$`, w.assertExists)
	sc.Step(lit("a state file whose \x60sid\x60 fails the session-id charset check stores nothing"), w.assertInvalidSidNothing)
	sc.Step(`^\x60claudeSession\(sessionId: "([A-Za-z0-9]+)"\)\x60 \x60paneTitle\x60 is "([^"]*)"$`, w.assertPaneTitleVal)
	sc.Step(`^\x60claudeSession\(sessionId: "([A-Za-z0-9]+)"\)\x60 \x60paneTitle\x60 is null$`, w.assertPaneTitleNull)
	sc.Step(lit("the stub tmux was invoked"), w.assertTmuxInvoked)
	sc.Step(lit("the stub tmux was not invoked"), w.assertTmuxNotInvoked)
	sc.Step(lit("a \x60claude.session.updated\x60 envelope for \x60P1\x60 with state \"input\" is pushed within 2 seconds"), w.assertInputEnvelope2s)
	sc.Step(lit("\x60claudeSession(sessionId: \"AO\")\x60 \x60mission\x60, \x60lastResponse\x60, and \x60paneTitle\x60 are all null"), w.assertAOAllNull)
	sc.Step(lit("a raw dump of \x60<tmp>/claude.db\x60 contains neither secret"), w.assertCarveoutDBNoSecrets)
	sc.Step(lit("every envelope pushed to the subscription contains neither secret"), w.assertCarveoutSubNoSecrets)
	sc.Step(lit("the \x60claudeSessions\x60 GraphQL result contains neither secret"), w.assertCarveoutGQLNoSecrets)
	sc.Step(lit("\x60claudeSession(sessionId: \"SEC\")\x60 \x60mission\x60 is \"SECRET-MISSION-STRING\" and \x60lastResponse\x60 is \"SECRET-RESPONSE-STRING\""), w.assertSECFields)
	sc.Step(lit("no other field of \x60SEC\x60 carries either secret"), w.assertSECClean)
}

var carveoutSecrets = []string{"SECRET-MISSION-STRING", "SECRET-RESPONSE-STRING"}

// ---- state-file writers ----

func (w *world) writeStateFile(dir, name string, obj map[string]any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(obj)
	return os.WriteFile(filepath.Join(dir, name), b, 0o644)
}

func (w *world) stateFileM1() error {
	return w.writeStateFile(w.claudeStateDir, "M1.json", map[string]any{
		"sid": "M1", "pane": "%7", "pid": os.Getpid(), "cwd": "/w/x", "state": "input"})
}

func (w *world) stateFileM3() error {
	return w.writeStateFile(w.claudeStateDir, "M3.json", map[string]any{"sid": "M3", "state": "idle"})
}

func (w *world) m2Folded() error {
	w.postHook("M2", "PreToolUse", map[string]any{"tool_name": "Bash", "ts": "2026-09-05T07:00:00Z"})
	w.postHook("M2", "PreToolUse", map[string]any{"tool_name": "Bash", "ts": "2026-09-05T07:00:01Z"})
	if w.waitSession("M2", func(m map[string]any) bool { tc, _ := numField(m, "toolCalls"); return int(tc) == 2 }, 3*time.Second) == nil {
		return fmt.Errorf("M2 did not reach toolCalls 2 from the hook folds")
	}
	return nil
}

func (w *world) stateFileM2Idle() error {
	return w.writeStateFile(w.claudeStateDir, "M2.json", map[string]any{
		"sid": "M2", "state": "idle", "tool_calls": 0, "first_prompt": "MISSION-TEXT"})
}

func (w *world) stateFileM4Create() error {
	if err := w.writeStateFile(w.claudeStateDir, "M4.json", map[string]any{
		"sid": "M4", "state": "idle", "first_prompt": "MISSION-TEXT"}); err != nil {
		return err
	}
	if w.waitSession("M4", func(m map[string]any) bool { return m["mission"] == "MISSION-TEXT" }, 5*time.Second) == nil {
		return fmt.Errorf("M4 not created from the state file before the hook")
	}
	return nil
}

func (w *world) stateFileMT() error {
	return w.writeStateFile(w.claudeStateDir, "MT.json", map[string]any{
		"sid": "MT", "state": "idle", "first_prompt": strings.Repeat("é", 500)})
}

func (w *world) stateFileMF() error {
	if err := w.writeStateFile(w.claudeStateDir, "MF.json", map[string]any{
		"sid": "MF", "state": "idle", "first_prompt": ""}); err != nil {
		return err
	}
	if w.waitSession("MF", func(m map[string]any) bool { return m["sessionId"] == "MF" }, 5*time.Second) == nil {
		return fmt.Errorf("MF not ingested")
	}
	return nil
}

func (w *world) stateFileLR() error {
	return w.writeStateFile(w.claudeStateDir, "LR.json", map[string]any{
		"sid": "LR", "state": "idle", "last_response": "R-BODY"})
}

func (w *world) stateFileLR2() error {
	if err := w.writeStateFile(w.claudeStateDir, "LR2.json", map[string]any{
		"sid": "LR2", "state": "idle", "last_response": strings.Repeat("ü", 500)}); err != nil {
		return err
	}
	if w.waitSession("LR2", func(m map[string]any) bool {
		s, _ := m["lastResponse"].(string)
		return utf8.RuneCountInString(s) == 200
	}, 5*time.Second) == nil {
		return fmt.Errorf("LR2 lastResponse not truncated to 200 runes")
	}
	return nil
}

func (w *world) stateDirMalformed() error {
	if err := os.WriteFile(filepath.Join(w.claudeStateDir, "bad.json"), []byte(`{"sid":"BAD","cwd":`), 0o644); err != nil {
		return err
	}
	return w.writeStateFile(w.claudeStateDir, "V1.json", map[string]any{"sid": "V1", "state": "idle"})
}

func (w *world) stateFilePane(sid, pane string) error {
	return w.writeStateFile(w.claudeStateDir, sid+".json", map[string]any{
		"sid": sid, "pane": pane, "pid": os.Getpid(), "state": "idle"})
}

func (w *world) stateFileSEC() error {
	return w.writeStateFile(w.claudeStateDir, "SEC.json", map[string]any{
		"sid": "SEC", "state": "idle", "cwd": "/w/sec",
		"first_prompt": "SECRET-MISSION-STRING", "last_response": "SECRET-RESPONSE-STRING"})
}

func (w *world) stateFileSecretsDisabled() error {
	if err := w.writeStateFile(w.claudeStateDir, "SECDIS.json", map[string]any{
		"sid": "SECDIS", "state": "idle",
		"first_prompt": "SECRET-MISSION-STRING", "last_response": "SECRET-RESPONSE-STRING"}); err != nil {
		return err
	}
	w.claudeFrames = w.drainClaudeFrames(1500 * time.Millisecond) // capture any (expected none) sub frames
	return nil
}

func (w *world) stateFileTilde() error {
	return w.writeStateFile(filepath.Join(w.claudeHomeDir, "state"), "TD.json", map[string]any{"sid": "TD", "state": "idle"})
}

// ---- server-start variants ----

func (w *world) startStateDirServer() error {
	if err := w.writeConfig(cfgOpts{lag: 30, templateInterval: 1, claudeStateDir: w.claudeStateDir}); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) startPaneTitleServer(stubOK, paneTitles bool) error {
	if err := w.installTmuxStub(stubOK); err != nil {
		return err
	}
	if err := w.writeConfig(cfgOpts{lag: 30, templateInterval: 1, claudeStateDir: w.claudeStateDir, claudePaneTitles: paneTitles}); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) startPaneTitleOK() error   { return w.startPaneTitleServer(true, true) }
func (w *world) startPaneTitleFail() error { return w.startPaneTitleServer(false, true) }
func (w *world) startPaneTitlesOff() error { return w.startPaneTitleServer(true, false) }

func (w *world) startTildeServer() error {
	w.claudeHomeDir = filepath.Join(w.dataDir, "home")
	if err := os.MkdirAll(filepath.Join(w.claudeHomeDir, "state"), 0o755); err != nil {
		return err
	}
	if err := w.writeConfig(cfgOpts{lag: 30, templateInterval: 1, claudeStateDir: "~/state"}); err != nil {
		return err
	}
	return w.startServe()
}

func (w *world) installTmuxStub(ok bool) error {
	w.claudeTmuxDir = filepath.Join(w.dataDir, "bin")
	if err := os.MkdirAll(w.claudeTmuxDir, 0o755); err != nil {
		return err
	}
	log := filepath.Join(w.claudeTmuxDir, "tmux.log")
	body := "#!/bin/sh\necho called >> " + log + "\n"
	if ok {
		body += "printf '%s\\t%s\\n' '%7' 'issue28-worker'\n"
	} else {
		body += "exit 1\n"
	}
	return os.WriteFile(filepath.Join(w.claudeTmuxDir, "tmux"), []byte(body), 0o755)
}

// ---- assertions ----

func (w *world) assertM1() error {
	s := w.waitSession("M1", func(m map[string]any) bool { return m["state"] == "input" }, 5*time.Second)
	if s == nil || s["state"] != "input" || s["cwd"] != "/w/x" {
		return fmt.Errorf("M1 = %v; want state input, cwd /w/x", s)
	}
	return nil
}

func (w *world) assertInstanceM1() error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, in := range w.instances() {
			if in["pane"] != "%7" {
				continue
			}
			pid, _ := numField(in, "pid")
			sess, _ := in["session"].(map[string]any)
			if pid != 0 && sess != nil && sess["sessionId"] == "M1" {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("no ClaudeInstance pane=%%7 pid!=0 session=M1; got %v", w.instances())
}

func (w *world) assertM3Emitted() error {
	if !w.waitClaudeFrame("M3", 4*time.Second) {
		return fmt.Errorf("no claude.session.updated envelope pushed for M3 within 4s")
	}
	return nil
}

func (w *world) assertNoFurtherM3() error {
	if !w.noClaudeFrame("M3", 2500*time.Millisecond) {
		return fmt.Errorf("an unexpected further envelope for M3 was pushed on an unchanged re-scan")
	}
	return nil
}

func (w *world) assertMissionMT() error {
	s := w.waitSession("MT", func(m map[string]any) bool { s, _ := m["mission"].(string); return utf8.RuneCountInString(s) == 120 }, 5*time.Second)
	m, _ := s["mission"].(string)
	if utf8.RuneCountInString(m) != 120 {
		return fmt.Errorf("MT mission rune count = %d, want 120", utf8.RuneCountInString(m))
	}
	if !utf8.ValidString(m) {
		return fmt.Errorf("MT mission is not valid UTF-8")
	}
	if !strings.HasPrefix(strings.Repeat("é", 500), m) {
		return fmt.Errorf("MT mission is not a prefix of the first prompt")
	}
	return nil
}

func (w *world) assertMissionMFNull() error {
	s := w.session("MF")
	if s == nil {
		return fmt.Errorf("MF missing")
	}
	if s["mission"] != nil {
		return fmt.Errorf("MF mission = %v, want null", s["mission"])
	}
	return nil
}

func (w *world) assertLR() error {
	s := w.waitSession("LR", func(m map[string]any) bool { return m["lastResponse"] == "R-BODY" }, 5*time.Second)
	if s == nil || s["lastResponse"] != "R-BODY" {
		return fmt.Errorf("LR lastResponse = %v, want R-BODY", s["lastResponse"])
	}
	return nil
}

func (w *world) assertLR2() error {
	s := w.session("LR2")
	r, _ := s["lastResponse"].(string)
	if utf8.RuneCountInString(r) != 200 {
		return fmt.Errorf("LR2 lastResponse rune count = %d, want 200", utf8.RuneCountInString(r))
	}
	if !utf8.ValidString(r) || !strings.HasPrefix(strings.Repeat("ü", 500), r) {
		return fmt.Errorf("LR2 lastResponse not a valid UTF-8 prefix of the input")
	}
	return nil
}

func (w *world) assertM2Reconcile() error {
	s := w.waitSession("M2", func(m map[string]any) bool { return m["mission"] == "MISSION-TEXT" }, 5*time.Second)
	if s == nil {
		return fmt.Errorf("M2 mission never set by the state file")
	}
	tc, _ := numField(s, "toolCalls")
	if s["state"] != "working" || int(tc) != 2 || s["mission"] != "MISSION-TEXT" {
		return fmt.Errorf("M2 = state %v toolCalls %v mission %v; want working/2/MISSION-TEXT (state file clobbered a fold)", s["state"], s["toolCalls"], s["mission"])
	}
	return nil
}

func (w *world) assertM4Reconcile() error {
	s := w.waitSession("M4", func(m map[string]any) bool { return m["state"] == "working" }, 5*time.Second)
	if s == nil || s["state"] != "working" || s["mission"] != "MISSION-TEXT" {
		return fmt.Errorf("M4 = %v; want state working, mission MISSION-TEXT (a fold cleared file-set mission)", s)
	}
	return nil
}

func (w *world) assertExists(sid string) error {
	if w.waitSession(sid, func(m map[string]any) bool { return m["sessionId"] == sid }, 5*time.Second) == nil {
		return fmt.Errorf("%s not present", sid)
	}
	return nil
}

func (w *world) assertInvalidSidNothing() error {
	// A file whose sid fails validSessionID must store nothing; its unique cwd must
	// never appear among the ingested sessions.
	if err := w.writeStateFile(w.claudeStateDir, "invsid.json", map[string]any{
		"sid": "has space", "state": "idle", "cwd": "/invalid-sid-cwd"}); err != nil {
		return err
	}
	time.Sleep(1500 * time.Millisecond) // let a scan run
	_, d, err := w.gql(`{ claudeSessions{ sessionId cwd } }`)
	if err != nil {
		return err
	}
	rows, _ := d["claudeSessions"].([]any)
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok && m["cwd"] == "/invalid-sid-cwd" {
			return fmt.Errorf("an invalid-sid state file was stored: %v", m)
		}
	}
	return nil
}

func (w *world) assertPaneTitleVal(sid, want string) error {
	s := w.waitSession(sid, func(m map[string]any) bool { return m["paneTitle"] == want }, 5*time.Second)
	if s == nil || s["paneTitle"] != want {
		return fmt.Errorf("%s paneTitle = %v, want %q", sid, s["paneTitle"], want)
	}
	return nil
}

func (w *world) assertPaneTitleNull(sid string) error {
	s := w.waitSession(sid, func(m map[string]any) bool { return m["sessionId"] == sid }, 5*time.Second)
	if s == nil {
		return fmt.Errorf("%s missing", sid)
	}
	if s["paneTitle"] != nil {
		return fmt.Errorf("%s paneTitle = %v, want null", sid, s["paneTitle"])
	}
	return nil
}

func (w *world) assertTmuxInvoked() error {
	if b, err := os.ReadFile(filepath.Join(w.claudeTmuxDir, "tmux.log")); err != nil || len(b) == 0 {
		return fmt.Errorf("stub tmux was never invoked (log missing/empty)")
	}
	return nil
}

func (w *world) assertTmuxNotInvoked() error {
	// The row must have been ingested first, proving a scan ran without spawning tmux.
	if err := w.assertExists("PT3"); err != nil {
		return err
	}
	if b, err := os.ReadFile(filepath.Join(w.claudeTmuxDir, "tmux.log")); err == nil && len(b) > 0 {
		return fmt.Errorf("stub tmux was spawned though paneTitles is disabled: %s", b)
	}
	return nil
}

func (w *world) assertInputEnvelope2s() error {
	if !w.waitClaudeFrame("input", 2*time.Second) {
		return fmt.Errorf("no state:input envelope for P1 pushed within 2s")
	}
	return nil
}

func (w *world) assertAOAllNull() error {
	s := w.waitSession("AO", func(m map[string]any) bool { return m["sessionId"] == "AO" }, 3*time.Second)
	if s == nil {
		return fmt.Errorf("AO missing")
	}
	for _, f := range []string{"mission", "lastResponse", "paneTitle"} {
		if s[f] != nil {
			return fmt.Errorf("AO %s = %v, want null (nullable add-only field)", f, s[f])
		}
	}
	return nil
}

func (w *world) assertCarveoutDBNoSecrets() error {
	data, err := os.ReadFile(filepath.Join(w.dataDir, "claude.db"))
	if err != nil {
		return err
	}
	for _, sec := range carveoutSecrets {
		if strings.Contains(string(data), sec) {
			return fmt.Errorf("claude.db leaked %q with stateDir disabled", sec)
		}
	}
	return nil
}

func (w *world) assertCarveoutSubNoSecrets() error {
	for _, f := range w.claudeFrames {
		for _, sec := range carveoutSecrets {
			if strings.Contains(f, sec) {
				return fmt.Errorf("subscription envelope leaked %q with stateDir disabled", sec)
			}
		}
	}
	return nil
}

func (w *world) assertCarveoutGQLNoSecrets() error {
	raw, _, err := w.gql(`{ claudeSessions{ sessionId cwd mission lastResponse paneTitle } }`)
	if err != nil {
		return err
	}
	for _, sec := range carveoutSecrets {
		if strings.Contains(raw, sec) {
			return fmt.Errorf("claudeSessions leaked %q with stateDir disabled", sec)
		}
	}
	return nil
}

func (w *world) assertSECFields() error {
	s := w.waitSession("SEC", func(m map[string]any) bool { return m["mission"] == "SECRET-MISSION-STRING" }, 5*time.Second)
	if s == nil || s["mission"] != "SECRET-MISSION-STRING" || s["lastResponse"] != "SECRET-RESPONSE-STRING" {
		return fmt.Errorf("SEC = %v; want mission/lastResponse carrying the secrets", s)
	}
	return nil
}

func (w *world) assertSECClean() error {
	s := w.session("SEC")
	if s == nil {
		return fmt.Errorf("SEC missing")
	}
	if s["cwd"] != "/w/sec" {
		return fmt.Errorf("SEC cwd = %v, want /w/sec", s["cwd"])
	}
	for _, f := range []string{"cwd", "lastTool", "gitBranch", "model"} {
		if sv, ok := s[f].(string); ok {
			for _, sec := range carveoutSecrets {
				if strings.Contains(sv, sec) {
					return fmt.Errorf("SEC field %s leaked secret %q", f, sec)
				}
			}
		}
	}
	return nil
}

// ---- subscription frame helpers ----

// waitClaudeFrame reports whether a pushed envelope containing needle arrives within
// timeout. Each read's budget is the REMAINING window, never a short fixed slice:
// coder/websocket closes the connection when a read's context expires, so a poll loop
// of short timeouts kills the socket on its first quiet moment and never sees a later
// frame.
func (w *world) waitClaudeFrame(needle string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		m, err := w.ws.nextPush(remaining)
		if err != nil {
			return false // the read timed out (and closed the socket): no such frame
		}
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), needle) {
			return true
		}
	}
}

// noClaudeFrame is the negative control: it reports whether NO envelope containing
// needle arrives during window. Same remaining-window read budget as waitClaudeFrame —
// a read error here means the window drained with nothing matching, which is a pass.
func (w *world) noClaudeFrame(needle string, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return true
		}
		m, err := w.ws.nextPush(remaining)
		if err != nil {
			return true
		}
		b, _ := json.Marshal(m)
		if strings.Contains(string(b), needle) {
			return false
		}
	}
}
