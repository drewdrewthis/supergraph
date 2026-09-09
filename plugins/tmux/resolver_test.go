package tmux

import (
	"context"
	"testing"
	"time"
)

// withActive publishes p as the current plugin for the duration of the test, so
// the exported accessors (which read getCurrent()) see it, then restores nil.
func withActive(t *testing.T, p *Plugin) {
	t.Helper()
	active.Set(p)
	t.Cleanup(func() { active.Set(nil) })
}

// TestWindowsBySessionAgreesWithWindows seeds two sessions — one with two windows
// (one holding two live panes plus a stale pane that must be excluded), one with a
// single window — and checks WindowsBySession's one-scan grouping matches what the
// per-session Windows accessor returns (#27 N+1 fix must not change semantics).
func TestWindowsBySessionAgreesWithWindows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()
	stale := now.Add(-time.Minute)

	panes := []PaneRow{
		{Key: paneKey("alpha", 0, 0, "h"), HostID: "h", Session: "alpha", Window: 0, Pane: 0, Cmd: "zsh", WindowName: "w0", WindowActive: true, Active: true},
		{Key: paneKey("alpha", 1, 0, "h"), HostID: "h", Session: "alpha", Window: 1, Pane: 0, Cmd: "zsh", WindowName: "w1", Active: true},
		{Key: paneKey("alpha", 1, 1, "h"), HostID: "h", Session: "alpha", Window: 1, Pane: 1, Cmd: "vim", WindowName: "w1"},
		{Key: paneKey("alpha", 1, 2, "h"), HostID: "h", Session: "alpha", Window: 1, Pane: 2, Cmd: "zsh", WindowName: "w1", StaleSince: &stale},
		{Key: paneKey("beta", 0, 0, "h"), HostID: "h", Session: "beta", Window: 0, Pane: 0, Cmd: "zsh", WindowName: "only", Active: true},
	}
	for _, pr := range panes {
		if err := s.upsertPane(ctx, pr, now); err != nil {
			t.Fatalf("upsertPane: %v", err)
		}
	}
	// Mark the stale pane stale in the store too — upsertPane alone doesn't set
	// StaleSince; markPaneStale is the store's own path to it.
	if err := s.markPaneStale(ctx, paneKey("alpha", 1, 2, "h"), stale); err != nil {
		t.Fatalf("markPaneStale: %v", err)
	}

	withActive(t, &Plugin{store: s})

	bySession, err := WindowsBySession(ctx, "h")
	if err != nil {
		t.Fatalf("WindowsBySession: %v", err)
	}
	if len(bySession) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %+v", len(bySession), bySession)
	}

	for _, session := range []string{"alpha", "beta"} {
		want, err := Windows(ctx, "h", session)
		if err != nil {
			t.Fatalf("Windows(%s): %v", session, err)
		}
		got := bySession[session]
		if len(got) != len(want) {
			t.Fatalf("session %s: len mismatch got %d want %d", session, len(got), len(want))
		}
		for i := range want {
			if got[i].Key != want[i].Key || got[i].Index != want[i].Index || got[i].Name != want[i].Name ||
				got[i].Active != want[i].Active || len(got[i].Panes) != len(want[i].Panes) {
				t.Fatalf("session %s window %d mismatch: got %+v want %+v", session, i, got[i], want[i])
			}
		}
	}

	alpha := bySession["alpha"]
	if len(alpha) != 2 {
		t.Fatalf("alpha: expected 2 windows, got %d", len(alpha))
	}
	if len(alpha[1].Panes) != 2 {
		t.Fatalf("alpha window 1: expected 2 live panes (stale excluded), got %d", len(alpha[1].Panes))
	}
}
