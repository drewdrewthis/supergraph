package tmux

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// TestServerDownEmitsOncePerTransition pins B1: while the server stays down across
// many reconnect cycles, tmux.server.down is emitted exactly once (the up→down
// transition), never on every failed attach — so core's lag crosses the stale
// threshold honestly.
func TestServerDownEmitsOncePerTransition(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newStore(t)
	var got []core.Envelope
	cycles := 0
	p := &Plugin{
		cfg:         config{reconnectBackoffMax: 10 * time.Millisecond, minStableAttach: time.Second},
		hostID:      "h",
		store:       s,
		now:         func() time.Time { return time.Now().UTC() },
		serverAlive: func(context.Context) bool { return true },
		attach:      func(context.Context) bool { return false }, // never a held session
	}
	p.emitFn = func(_ context.Context, e core.Envelope) error { got = append(got, e); return nil }
	p.sleep = func(context.Context, time.Duration) {
		if cycles++; cycles >= 5 {
			cancel()
		}
	}
	p.watch(ctx)

	n := 0
	for _, e := range got {
		if e.Type == "tmux.server.down" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly 1 tmux.server.down over 5 down cycles, got %d", n)
	}
}

// TestDeadSocketNoAttachBackoffToCap pins B2: a dead socket (list-sessions probe
// fails) is never attached — attaching would make tmux auto-spawn a server and busy-
// spin — and the reconnect backoff grows to its configured cap.
func TestDeadSocketNoAttachBackoffToCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := newStore(t)
	attachCalls := 0
	var slept []time.Duration
	p := &Plugin{
		cfg:    config{reconnectBackoffMax: 4 * time.Second, minStableAttach: time.Second},
		hostID: "h",
		store:  s,
		now:    func() time.Time { return time.Now().UTC() },
		run:    func(context.Context, ...string) ([]byte, error) { return nil, errors.New("no server running") },
		attach: func(context.Context) bool { attachCalls++; return true },
	}
	p.serverAlive = p.probeAlive
	p.emitFn = func(context.Context, core.Envelope) error { return nil }
	cycles := 0
	p.sleep = func(_ context.Context, d time.Duration) {
		slept = append(slept, d)
		if cycles++; cycles >= 5 {
			cancel()
		}
	}
	p.watch(ctx)

	if attachCalls != 0 {
		t.Fatalf("dead socket must not attach (auto-spawns a server); got %d attach calls", attachCalls)
	}
	var max time.Duration
	for _, d := range slept {
		if d > max {
			max = d
		}
	}
	if max != p.cfg.reconnectBackoffMax {
		t.Fatalf("backoff never reached cap %v; max slept %v (seq %v)", p.cfg.reconnectBackoffMax, max, slept)
	}
}

func TestSupportsPaneExitedPerVersion(t *testing.T) {
	if supportsPaneExited("tmux 3.6a") {
		t.Error("3.6a must NOT be treated as supporting pane-exited (layout-change branch)")
	}
	if !supportsPaneExited("tmux 3.7") {
		t.Error("3.7 must be treated as supporting pane-exited")
	}
	if !supportsPaneExited("tmux 4.0") {
		t.Error("4.0 must support pane-exited")
	}
}

func TestIsStructuralVersionGated(t *testing.T) {
	// layout-change is structural on every version.
	if !isStructural("%layout-change ...", "tmux 3.6a") {
		t.Error("layout-change must trigger reconcile on 3.6a")
	}
	// pane-exited is only a recognised trigger where the version emits it.
	if isStructural("%pane-exited @1 @2 0", "tmux 3.6a") {
		t.Error("pane-exited must not be a 3.6a trigger")
	}
	if !isStructural("%pane-exited @1 @2 0", "tmux 3.7") {
		t.Error("pane-exited must trigger on 3.7")
	}
	// Non-structural / content lines never trigger.
	if isStructural("%output %1 hello", "tmux 3.7") {
		t.Error("output must never trigger a reconcile")
	}
}

func TestVersionAtLeast(t *testing.T) {
	if !versionAtLeast("tmux 3.6a", 3, 6) {
		t.Error("3.6a >= 3.6")
	}
	if versionAtLeast("tmux 3.5", 3, 6) {
		t.Error("3.5 < 3.6")
	}
	if !versionAtLeast("tmux 4.1", 3, 6) {
		t.Error("4.1 >= 3.6")
	}
}
