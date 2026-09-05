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
	"strings"
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

	lastFree    map[string]bool
	branchDir   string
	mainDir     string
	branchPanes []paneJSON
	locOut      string
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
	if g.sw != nil {
		g.sw.stopServe()
	}
	if g.socket != "" {
		_ = exec.Command("tmux", "-L", g.socket, "kill-server").Run()
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
// race. (Re-sending an idempotent-enough command like `sleep`/`C-c` is harmless.)
func (g *tmuxWorld) sendUntil(target string, want func(string) bool, keys ...string) error {
	deadline := time.Now().Add(15 * time.Second)
	var lastCmd string
	for time.Now().Before(deadline) {
		args := append([]string{"send-keys", "-t", target}, keys...)
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
