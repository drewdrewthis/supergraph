package tmux

import (
	"bufio"
	"context"
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

// watch is the supervised control-mode loop (D1, D7). It attaches one control
// client, forwards every structural notification to the reconcile trigger, and on
// %exit/EOF (server died or was killed) marks all local entities stale, emits
// tmux.server.down, and exponential-backoff reconnects until ctx is done.
func (p *Plugin) watch(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		attached := p.attachOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		// The client returned: the server is gone (or never came up). Mark local
		// state stale and, if it had actually been up, announce it.
		if attached {
			p.serverDown(ctx)
			backoff = time.Second
		}
		p.sleep(ctx, backoff)
		if backoff < p.cfg.reconnectBackoffMax {
			backoff *= 2
			if backoff > p.cfg.reconnectBackoffMax {
				backoff = p.cfg.reconnectBackoffMax
			}
		}
	}
}

// attachOnce runs one `tmux -C attach` client until it exits. It returns true if the
// client actually attached (produced output before exiting), false if the attach
// failed outright (no server yet) so the caller does not emit a spurious
// server.down. stdin is held open for the client's lifetime — closing it makes tmux
// emit %exit immediately (EDR §Measurements).
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
		if isStructural(line, p.version) {
			p.triggerReconcile()
		}
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
