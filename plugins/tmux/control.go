package tmux

import (
	"bufio"
	"context"
	"log"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// baseStructural are the control-mode notifications that mean the server's
// structure changed and a reconcile is due. tmux 3.6a has no dedicated
// %pane-exited — pane death arrives as %window-pane-changed + %layout-change whose
// layout string no longer lists the dead pane (EDR §Measurements) — so any of these
// triggers a fresh list-panes rather than parsing each delta.
var baseStructural = []string{
	"%window-add", "%window-close", "%unlinked-window-add", "%unlinked-window-close",
	"%window-pane-changed", "%layout-change", "%sessions-changed",
	"%session-changed", "%session-window-changed",
}

// supportsPaneExited reports whether this tmux emits the dedicated %pane-exited
// notification (added after 3.6; 3.6a and earlier signal death via %layout-change).
// The CI matrix runs different tmux versions, so the trigger set is version-gated.
func supportsPaneExited(version string) bool { return versionAtLeast(version, 3, 7) }

// structuralPrefixes is the reconcile-trigger notification set for a tmux version.
func structuralPrefixes(version string) []string {
	if supportsPaneExited(version) {
		return append(append([]string{}, baseStructural...), "%pane-exited")
	}
	return baseStructural
}

// isStructural reports whether a control-mode line is a reconcile trigger for the
// given tmux version.
func isStructural(line, version string) bool {
	for _, p := range structuralPrefixes(version) {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// versionAtLeast parses a "tmux 3.6a"/"3.7" version string and compares major.minor.
func versionAtLeast(version string, major, minor int) bool {
	f := strings.Fields(version)
	v := version
	if len(f) > 0 {
		v = f[len(f)-1] // drop a leading "tmux "
	}
	maj, min := parseVer(v)
	if maj != major {
		return maj > major
	}
	return min >= minor
}

// parseVer extracts the leading major and minor integers from "3.6a" etc.
func parseVer(v string) (int, int) {
	dot := strings.IndexByte(v, '.')
	if dot < 0 {
		return leadingInt(v), 0
	}
	return leadingInt(v[:dot]), leadingInt(v[dot+1:])
}

func leadingInt(s string) int {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	n, _ := strconv.Atoi(s[:i])
	return n
}

// watch is the supervised control-mode loop (D1, D7). Each cycle it probes server
// liveness WITHOUT attaching (a bare `tmux -C attach` against a dead socket makes
// tmux fork a brand-new server — B2), attaches only when the server is up, forwards
// every structural notification to the reconcile trigger, and reconnects with
// exponential backoff until ctx is done.
//
// tmux.server.down is emitted exactly ONCE per up→down transition (B1): `up` starts
// optimistic so the first real down announces once, then stays silent while down so
// core's lag crosses the stale threshold honestly (owner T1). An attach that falls
// out in under minStableAttach is a failed/immediate exit, not a held session, and
// never resets the backoff (B2).
func (p *Plugin) watch(ctx context.Context) {
	backoff := time.Second
	up := true
	for ctx.Err() == nil {
		genuine := false
		if p.serverAlive(ctx) {
			start := p.now()
			att := p.attach(ctx)
			if ctx.Err() != nil {
				return
			}
			genuine = att && p.now().Sub(start) >= p.cfg.minStableAttach
		}
		if genuine {
			up = true
			backoff = time.Second
			continue // the held session ended; retry promptly without a false down
		}
		if up { // up→down transition: announce exactly once
			p.serverDown(ctx)
			up = false
		}
		p.sleep(ctx, backoff)
		backoff = nextBackoff(backoff, p.cfg.reconnectBackoffMax)
	}
}

// nextBackoff doubles cur toward max (inclusive), never overshooting.
func nextBackoff(cur, max time.Duration) time.Duration {
	if cur >= max {
		return max
	}
	if n := cur * 2; n < max {
		return n
	}
	return max
}

// probeAlive reports whether the watched tmux server is up WITHOUT attaching to it.
// `tmux -C attach` against a dead socket makes tmux fork a fresh server (B2), so
// liveness is gated on a read-only list-sessions first; its non-zero exit ("no
// server running") means down and the caller never attaches.
func (p *Plugin) probeAlive(ctx context.Context) bool {
	_, err := p.run(ctx, p.tmuxArgs("list-sessions")...)
	return err == nil
}

// attachOnce runs one `tmux -C attach` client until it exits (the default `attach`
// seam; unit tests swap in a fake). It returns true if the client actually attached
// (produced output before exiting), false if the attach failed outright. The
// CommandContext child is killed when ctx is cancelled and reaped in the defer, so
// no attach process leaks on shutdown. stdin is held open for the client's lifetime
// — closing it makes tmux emit %exit immediately (EDR §Measurements).
func (p *Plugin) attachOnce(ctx context.Context) bool {
	// tmuxPath/socket are operator config (not attacker input) and control mode is a
	// passive read-only client (D2), so launching it here is intended.
	cmd := exec.CommandContext(ctx, p.cfg.tmuxPath, p.tmuxArgs("-C", "attach")...) //nolint:gosec // G204: operator-configured tmux binary/socket
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return false
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return false
	}
	if err := cmd.Start(); err != nil {
		return false
	}
	defer func() { _ = stdin.Close(); _ = cmd.Wait() }()

	// Suppress %output pane content so the stream stays purely structural.
	_, _ = stdin.Write([]byte("refresh-client -f no-output\n"))
	// A fresh attach means state may have changed while we were disconnected.
	p.triggerReconcile()

	attached := false
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		attached = true
		line := sc.Text()
		if strings.HasPrefix(line, "%exit") {
			return true
		}
		// %begin/%end/%error bracket the reply block of any command we send
		// (refresh-client above); they are not server notifications, so skip them
		// explicitly rather than letting %error slip past as a non-structural line.
		if strings.HasPrefix(line, "%begin") || strings.HasPrefix(line, "%end") || strings.HasPrefix(line, "%error") {
			continue
		}
		if isStructural(line, p.version) {
			p.triggerReconcile()
		}
	}
	// A Scan loop ends on EOF or error; surface the error (ErrTooLong on an
	// oversized frame, a read failure) so a truncated stream is not read as a clean
	// server exit.
	if err := sc.Err(); err != nil {
		log.Printf("tmux: control-mode scan ended with error: %v", err)
	}
	return attached
}

// triggerReconcile asks the reconcile loop to run now, coalescing bursts (the
// buffered channel drops a second signal while one is pending).
func (p *Plugin) triggerReconcile() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

// serverDown marks every local entity stale and emits tmux.server.down (F8's local
// shape, before the peer plugin).
func (p *Plugin) serverDown(ctx context.Context) {
	_ = p.store.markAllStale(ctx, p.now())
	p.emit(ctx, "tmux.server.down", serverKey(p.hostID),
		map[string]any{"hostId": p.hostID}, p.now())
}
