package features

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/cucumber/godog"
)

// tmuxWorld is the per-scenario state for the tmux plugin's @local scenarios. It
// wraps a fresh *world (reused for dataDir/cfgPath/listen and its
// startServe/stopServe/runCLI subprocess management) plus the private tmux socket
// and the config knobs a scenario needs. Steps are black-box: real `tmux` on a
// private -L socket, the running `supergraph serve` subprocess, and `supergraph
// query` — no import of plugins/tmux (except the compiled parser is covered by that
// package's own unit tests).
type tmuxWorld struct {
	sw            *world
	socket        string
	reconcileSecs int
	eventSource   string
	tmuxPath      string // plugin's tmux binary; a stub for AC-TMUX-POLL-ERROR
	stubTrigger   string // file whose presence flips the stub to failure mode
	templatePanic bool
	lag           float64

	samples     []time.Duration
	firstCursor string
	hooksBefore string

	lastFree     map[string]bool
	branchDir    string
	mainDir      string
	branchPanes  []paneJSON
	locOut       string
	lastSessions []sessionJSON

	// attachCmd/attachDone track the real (#27 AC-TMUX-ATTACHED-LIVE) `script`-wrapped
	// tmux attach client, so cleanup can reap it if the scenario fails before its own
	// detach step runs.
	attachCmd  *exec.Cmd
	attachDone chan struct{}

	// attachLatencies/detachLatencies hold per-cycle client-appeared/vanished-to-
	// subscription-envelope latencies (AC-TMUX-ATTACHED-LIVE), t0 taken from the real
	// tmux server's own list-clients output — never from cmd.Start(), which returns
	// long before tmux has actually attached and would flatter the number.
	attachLatencies []time.Duration
	detachLatencies []time.Duration
}

func (g *tmuxWorld) init() error {
	// Zero every field: tw is a single shared instance reused across all @local
	// scenarios, so any knob left set by a prior scenario (tmuxPath stub,
	// templatePanic, samples) must not leak into the next one.
	*g = tmuxWorld{}
	g.sw = &world{}
	if err := g.sw.init(); err != nil {
		return err
	}
	g.socket = fmt.Sprintf("sg-test-%d-%d", os.Getpid(), time.Now().UnixNano())
	g.reconcileSecs = 1
	g.eventSource = "control"
	g.lag = 30
	return nil
}

func (g *tmuxWorld) cleanup() {
	// Stop serve first so the plugin's control-mode `tmux -C attach` child is killed
	// with its context before the server is torn down (avoids orphaned clients
	// accumulating across the suite's many scenarios).
	if g.attachCmd != nil && g.attachCmd.Process != nil {
		_ = g.attachCmd.Process.Kill()
	}
	if g.attachDone != nil {
		select {
		case <-g.attachDone:
		case <-time.After(2 * time.Second):
		}
	}
	if g.sw != nil {
		g.sw.stopServe()
	}
	if g.socket != "" {
		killTmuxServer(g.socket)
	}
	for _, d := range []string{g.branchDir, g.mainDir} {
		if d != "" {
			_ = os.RemoveAll(d)
		}
	}
	if g.sw != nil {
		g.sw.cleanup()
	}
}

// killTmuxServer tears down the scenario's private tmux server and VERIFIES nothing
// on the socket survives (M3, ~2/7 runs leaked). kill-server can return non-zero
// (server already exiting) and, worse, `stopServe` SIGKILLs the serve subprocess so
// its control-mode `tmux -C attach` CHILD is orphaned (the ctx-kill never runs) and
// can outlive the server death. So after kill-server we poll has-session until the
// server is gone (≤2s), then ALWAYS pgrep the socket for any leftover `tmux -L
// <socket>` process — server OR orphaned attach client — and SIGKILL it by pid,
// logging when the fallback fired so a recurring leak stays visible.
func killTmuxServer(socket string) {
	_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := exec.Command("tmux", "-L", socket, "has-session").Run(); err != nil {
			break // server gone; still sweep for orphaned attach clients below
		}
		time.Sleep(50 * time.Millisecond)
	}
	out, err := exec.Command("pgrep", "-f", "tmux -L "+socket).Output()
	if err != nil {
		return // pgrep found nothing (or is absent): clean
	}
	killed := 0
	for _, line := range strings.Fields(string(out)) {
		if pid, perr := strconv.Atoi(line); perr == nil {
			if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr == nil {
				killed++
			}
		}
	}
	if killed > 0 {
		fmt.Fprintf(os.Stderr, "tmux cleanup: SIGKILLed %d leftover process(es) on socket %q (server or orphaned -C attach)\n", killed, socket)
	}
}

// tmux runs a tmux control command against the private socket.
func (g *tmuxWorld) tmux(args ...string) (string, error) {
	full := append([]string{"-L", g.socket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return string(out), err
}

// tmuxStart starts a server-creating command with a clean config (-f /dev/null) so
// base-index/pane-base-index are 0 and no operator ~/.tmux.conf hook is present —
// the scenarios must be hermetic regardless of the developer's tmux config.
func (g *tmuxWorld) tmuxStart(args ...string) (string, error) {
	full := append([]string{"-f", "/dev/null", "-L", g.socket}, args...)
	out, err := exec.Command("tmux", full...).CombinedOutput()
	return string(out), err
}

// writeConfigAndStart renders the config.toml for the current knobs and starts the
// serve subprocess (which waits until template+fakeok are ready).
func (g *tmuxWorld) writeConfigAndStart() error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostId = %q\n", "test")
	fmt.Fprintf(&b, "listen = %q\n", g.sw.listen)
	fmt.Fprintf(&b, "dataDir = %q\n", g.sw.dataDir)
	fmt.Fprintf(&b, "lagThresholdSeconds = %f\n", g.lag)
	b.WriteString("[plugins.template]\n")
	b.WriteString("intervalSeconds = 1\n")
	if g.templatePanic {
		b.WriteString("panic = true\n")
	}
	b.WriteString("[plugins.tmux]\n")
	fmt.Fprintf(&b, "socket = %q\n", g.socket)
	fmt.Fprintf(&b, "eventSource = %q\n", g.eventSource)
	fmt.Fprintf(&b, "reconcileIntervalSeconds = %d\n", g.reconcileSecs)
	if g.tmuxPath != "" {
		fmt.Fprintf(&b, "tmuxPath = %q\n", g.tmuxPath)
	}
	if err := os.WriteFile(g.sw.cfgPath, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return g.sw.startServe()
}

// ---- query helpers (black-box via `supergraph query`) ----

type slotJSON struct {
	PaneKey *string `json:"paneKey"`
	Free    bool    `json:"free"`
}

type paneJSON struct {
	Key        string  `json:"key"`
	Session    string  `json:"session"`
	Cmd        string  `json:"cmd"`
	Free       bool    `json:"free"`
	StaleSince *string `json:"staleSince"`
	// PaneID is only populated when a query selects it (#27); zero value elsewhere.
	PaneID string `json:"paneId"`
}

// windowJSON/sessionJSON are the #27 nesting projection (TmuxWindow/TmuxSession).
type windowJSON struct {
	Key     string     `json:"key"`
	Session string     `json:"session"`
	Index   int        `json:"index"`
	Name    *string    `json:"name"`
	Active  bool       `json:"active"`
	Panes   []paneJSON `json:"panes"`
}

type sessionJSON struct {
	Name      string       `json:"name"`
	Attached  bool         `json:"attached"`
	CreatedAt *string      `json:"createdAt"`
	Windows   []windowJSON `json:"windows"`
}

func (g *tmuxWorld) queryData(gql string, into any) error {
	g.sw.runCLI("query", gql)
	if g.sw.lastExit != 0 {
		return fmt.Errorf("query exit %d: %s", g.sw.lastExit, g.sw.lastStderr)
	}
	if err := json.Unmarshal([]byte(g.sw.lastStdout), into); err != nil {
		return fmt.Errorf("parse %q: %w", g.sw.lastStdout, err)
	}
	return nil
}

func (g *tmuxWorld) freeSlots() ([]slotJSON, error) {
	var d struct {
		FreeSlots []slotJSON `json:"freeSlots"`
	}
	err := g.queryData(`{ freeSlots { paneKey free } }`, &d)
	return d.FreeSlots, err
}

func (g *tmuxWorld) panes() ([]paneJSON, error) {
	var d struct {
		TmuxPanes []paneJSON `json:"tmuxPanes"`
	}
	err := g.queryData(`{ tmuxPanes { key session cmd free staleSince } }`, &d)
	return d.TmuxPanes, err
}

// sessions queries tmuxSessions with the full #27 nesting shape (attach/createdAt/
// windows/panes) so a single helper covers every window-nesting and attach scenario.
func (g *tmuxWorld) sessions() ([]sessionJSON, error) {
	var d struct {
		TmuxSessions []sessionJSON `json:"tmuxSessions"`
	}
	err := g.queryData(`{ tmuxSessions { name attached createdAt windows { key session index name active panes { key session cmd free staleSince paneId } } } }`, &d)
	return d.TmuxSessions, err
}

// sessionByName finds one session by name in an already-queried slice, or nil.
func sessionByName(ss []sessionJSON, name string) *sessionJSON {
	for i := range ss {
		if ss[i].Name == name {
			return &ss[i]
		}
	}
	return nil
}

// windowByIndex finds one window of session name by index in an already-queried
// slice, or nil (absence is the AC-TMUX-NESTING-STALE assertion itself).
func windowByIndex(ss []sessionJSON, name string, idx int) *windowJSON {
	s := sessionByName(ss, name)
	if s == nil {
		return nil
	}
	for i := range s.Windows {
		if s.Windows[i].Index == idx {
			return &s.Windows[i]
		}
	}
	return nil
}

func (g *tmuxWorld) paneForBranch(branch string) ([]paneJSON, error) {
	var d struct {
		PaneForBranch []paneJSON `json:"paneForBranch"`
	}
	err := g.queryData(fmt.Sprintf(`{ paneForBranch(branch:%q){ key session } }`, branch), &d)
	return d.PaneForBranch, err
}

// paneCountHTTP counts cached panes via a direct GraphQL POST (no `supergraph
// query` subprocess). Used in the EVENTS-CONTROL timing loop so the measured
// split-to-queryable latency reflects when the data is actually queryable over the
// API, not the fork/exec cost of the CLI (which would add ~100ms+ of harness noise
// to every sample and can tip a true sub-second push over the 1s p95 bound).
func (g *tmuxWorld) paneCountHTTP() (int, error) {
	body, _ := json.Marshal(map[string]string{"query": "{ tmuxPanes { key } }"})
	resp, err := http.Post("http://"+g.sw.listen+"/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	var env struct {
		Data struct {
			TmuxPanes []struct {
				Key string `json:"key"`
			} `json:"tmuxPanes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return 0, err
	}
	return len(env.Data.TmuxPanes), nil
}

func freeKeys(slots []slotJSON) map[string]bool {
	m := map[string]bool{}
	for _, s := range slots {
		if s.Free && s.PaneKey != nil {
			m[*s.PaneKey] = true
		}
	}
	return m
}

// tmuxHealth returns the "tmux" row and its cursor/state from /health.
func (g *tmuxWorld) tmuxHealthRow() (map[string]any, error) {
	rows, _, err := g.sw.getHealth()
	if err != nil {
		return nil, err
	}
	r := findRow(rows, "tmux")
	if r == nil {
		return nil, fmt.Errorf("no tmux row in /health")
	}
	return r, nil
}

// ---- polling ----

func eventually(timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		if last = fn(); last == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

// waitPanes blocks until at least min panes are cached (the plugin's first
// reconcile has populated the cache after startServe returns).
func (g *tmuxWorld) waitPanes(min int) error {
	return eventually(15*time.Second, func() error {
		ps, err := g.panes()
		if err != nil {
			return err
		}
		if len(ps) < min {
			return fmt.Errorf("have %d panes, want >= %d", len(ps), min)
		}
		return nil
	})
}

// idleShellCmds mirrors the plugin's default idleShells: a pane whose
// pane_current_command is one of these reads free.
var idleShellCmds = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true}

// sendUntil sends keys to a pane and confirms the real pane_current_command
// satisfies want, RESENDING until it sticks. A freshly created pane's shell may
// not yet be reading its PTY when send-keys returns, silently dropping the
// keystrokes; retrying until tmux itself reports the expected command removes that
// race. Each attempt is prefixed with `C-u` (kill-line) so a resend that follows a
// dropped `Enter` starts from a CLEAN prompt — otherwise the resent text
// concatenates onto the leftover partial line (`sleep 300sleep 300`), bash errors,
// and the command never runs (the "current command stayed bash" flake). C-u at an
// empty prompt is a no-op, so re-sending stays harmless.
func (g *tmuxWorld) sendUntil(target string, want func(string) bool, keys ...string) error {
	deadline := time.Now().Add(20 * time.Second)
	var lastCmd string
	for time.Now().Before(deadline) {
		args := append([]string{"send-keys", "-t", target, "C-u"}, keys...)
		if out, err := g.tmux(args...); err != nil {
			return fmt.Errorf("send-keys %s: %s", target, out)
		}
		// Poll for up to 2s BEFORE resending: a shell that is merely slow to start
		// the command is caught here rather than re-sent (which would queue a
		// duplicate command that runs later and corrupts a subsequent free/busy read).
		// Only genuinely-lost keystrokes (shell not yet reading its PTY) reach a resend.
		innerDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(innerDeadline) {
			out, err := g.tmux("display-message", "-p", "-t", target, "#{pane_current_command}")
			if err != nil {
				return fmt.Errorf("display-message %s: %s", target, out)
			}
			lastCmd = strings.TrimSpace(out)
			if want(lastCmd) {
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	return fmt.Errorf("pane %s current command %q never satisfied predicate", target, lastCmd)
}

// realPaneCount counts panes across all sessions on the real socket.
func (g *tmuxWorld) realPaneCount() (int, error) {
	out, err := g.tmux("list-panes", "-a", "-F", "x")
	if err != nil {
		return 0, fmt.Errorf("list-panes: %s", out)
	}
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, nil
}

// writeStubTmux writes a stub tmux binary (a shell script) that answers -V,
// list-sessions and list-panes with canned output until stubTrigger exists, then
// exits non-zero on every call — the AC-TMUX-POLL-ERROR fault injector.
func (g *tmuxWorld) writeStubTmux() error {
	g.stubTrigger = filepath.Join(g.sw.dataDir, "stub-fail")
	path := filepath.Join(g.sw.dataDir, "tmux-stub.sh")
	script := `#!/bin/sh
if [ -f "` + g.stubTrigger + `" ]; then exit 1; fi
for a in "$@"; do
  case "$a" in
    -V) echo "tmux 3.6a"; exit 0 ;;
    list-sessions) printf 'work\t/tmp\n'; exit 0 ;;
    list-panes) printf 'work\t0\t0\t111\tzsh\t/tmp\t1\n'; exit 0 ;;
  esac
done
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	g.tmuxPath = path
	return nil
}

// scriptAttachCmd builds a real (non-control-mode) `tmux attach-session` under a
// portable pty via the `script` utility (AC-TMUX-ATTACHED-LIVE): `tmux attach`
// needs a controlling terminal the test process itself has none of, and `script`
// gives the child one without a new Go module dependency. The flag syntax differs
// between macOS's BSD script (`-q /dev/null <cmd...>`) and Linux's util-linux
// script (`-q -c "<cmd>" /dev/null`), so the shape is chosen by GOOS. ok is false
// when `script` is not on PATH or the OS is neither — the caller returns
// godog.ErrPending rather than faking the attach (a stubbed list-clients would
// assert nothing about the real attach path).
func scriptAttachCmd(socket, session string) (cmd *exec.Cmd, ok bool) {
	if _, err := exec.LookPath("script"); err != nil {
		return nil, false
	}
	tmuxArgs := []string{"-L", socket, "attach-session", "-t", session}
	switch runtime.GOOS {
	case "darwin":
		args := append([]string{"-q", "/dev/null", "tmux"}, tmuxArgs...)
		return exec.Command("script", args...), true
	case "linux":
		full := append([]string{"tmux"}, tmuxArgs...)
		return exec.Command("script", "-q", "-c", strings.Join(full, " "), "/dev/null"), true
	default:
		return nil, false
	}
}

// humanClientTTY polls for the tty of the one non-control-mode client attached to
// "s1". Detaching by session ("detach-client -s s1") would also kick the plugin's
// OWN control-mode client — which, with only one session on the socket, is by
// default also attached to "s1" — knocking the very observer that's supposed to
// see the next cycle's attach offline. Targeting the human client's own tty via
// "-t" avoids that self-inflicted outage.
//
// A `list-clients` error is treated as "zero clients" and retried, not a hard
// failure: on this tmux build, `list-clients` on a momentarily clientless server
// exits non-zero with "no current target" instead of printing empty output (seen
// live between a detach and the control client's reconnect). That is exactly the
// failure mode plugins/tmux/snapshot.go's attachedSessions already treats as a
// failsafe (empty map, never an aborted reconcile) — this mirrors that here
// rather than letting a transient clientless instant abort the scenario.
func (g *tmuxWorld) humanClientTTY() (string, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, _ := g.tmux("list-clients", "-F", "#{client_session}\t#{client_control_mode}\t#{client_tty}")
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			f := strings.Split(line, "\t")
			if len(f) < 3 {
				continue
			}
			if f[0] == "s1" && f[1] != "1" {
				return f[2], nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no human client attached to s1 within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitRealHumanClient polls the real tmux server's own list-clients output (not
// the plugin's cache) until a client attached to "s1" that is NOT the plugin's own
// control-mode client is present (want=true) or absent (want=false), and returns
// the instant that first became true as t0 — the honest start of the attach/detach
// latency window (AC-TMUX-ATTACHED-LIVE), since cmd.Start() on the `script` wrapper
// returns well before tmux has actually registered the client.
func (g *tmuxWorld) waitRealHumanClient(want bool) (time.Time, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _ := g.tmux("list-clients", "-F", "#{client_session}\t#{client_control_mode}")
		human := false
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			f := strings.Split(line, "\t")
			if len(f) < 2 {
				continue
			}
			if f[0] == "s1" && f[1] != "1" {
				human = true
				break
			}
		}
		if human == want {
			return time.Now(), nil
		}
		if time.Now().After(deadline) {
			return time.Time{}, fmt.Errorf("real human client on s1 never reached present=%v within 10s", want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// openTmuxEventsSub opens a tmuxEvents subscription selecting type/key/payload —
// openSubscription's shared {ts payload v} shape can't tell a session-attached
// envelope from any other tmuxEvents push.
func (g *tmuxWorld) openTmuxEventsSub() error {
	return g.sw.openSubscriptionSelect("tmuxEvents", "type key payload")
}

// awaitSessionAttachedEnvelope drains the open tmuxEvents subscription (opened via
// openTmuxEventsSub) until it observes a tmux.session.updated envelope for
// session "s1" whose payload's attached field equals want, skipping any other
// envelope (pane/window updates, unrelated sessions) in between. It returns the
// elapsed time since t0 — the moment the real client's presence/absence was
// first observed on the real server — which is the quantity AC-TMUX-ATTACHED-LIVE
// bounds at 1s, not the poll-window floor a `tmuxSessions` query would impose.
func (g *tmuxWorld) awaitSessionAttachedEnvelope(t0 time.Time, want bool, timeout time.Duration) (time.Duration, error) {
	const wantKey = "session:s1@test"
	deadline := t0.Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, fmt.Errorf("no tmux.session.updated envelope for s1 with attached=%v within %s", want, timeout)
		}
		p, err := g.sw.ws.nextPush(remaining)
		if err != nil {
			exited := g.sw.serve != nil && g.sw.serve.exited()
			// DIAGNOSTIC (temporary): report whether the queryable cache itself
			// converged even though the subscription never delivered the envelope —
			// distinguishes a delivery-path issue from a genuine backend miss.
			queryState := "query failed"
			if ss, qerr := g.sessions(); qerr == nil {
				if s1 := sessionByName(ss, "s1"); s1 != nil {
					queryState = fmt.Sprintf("tmuxSessions query shows attached=%v", s1.Attached)
				} else {
					queryState = "tmuxSessions query: s1 not found"
				}
			}
			return 0, fmt.Errorf("subscription read: %w (serve exited=%v stderr=%s; %s)", err, exited, g.sw.serve.stderr.String(), queryState)
		}
		data, _ := p["data"].(map[string]any)
		te, _ := data["tmuxEvents"].(map[string]any)
		if te == nil {
			continue
		}
		typ, _ := te["type"].(string)
		key, _ := te["key"].(string)
		if typ != "tmux.session.updated" || key != wantKey {
			continue
		}
		payloadStr, _ := te["payload"].(string)
		var body struct {
			Attached bool `json:"attached"`
		}
		if err := json.Unmarshal([]byte(payloadStr), &body); err != nil {
			continue
		}
		if body.Attached != want {
			continue
		}
		return time.Since(t0), nil
	}
}

// tmpGitRepoOnBranch creates a throwaway git worktree checked out on branch.
func tmpGitRepoOnBranch(branch string) (string, error) {
	dir, err := os.MkdirTemp("", "sg-branch-*")
	if err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "x"},
		{"branch", "-M", branch},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("git %v: %s", args, out)
		}
	}
	return dir, nil
}

var paneKeyRe = regexp.MustCompile(`^pane:([^:@]+):(\d+)\.(\d+)@([^@]+)$`)

func tmuxHasTag(sc *godog.Scenario, tag string) bool {
	for _, t := range sc.Tags {
		if t.Name == tag {
			return true
		}
	}
	return false
}

var _ = context.Background
