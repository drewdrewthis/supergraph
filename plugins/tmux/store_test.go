package tmux

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func newStore(t *testing.T) *store {
	t.Helper()
	ctx := context.Background()
	cs, err := core.OpenStore(ctx, filepath.Join(t.TempDir(), "tmux.db"), 100)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	s := &store{core: cs}
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func TestStoreUpsertScanAndStale(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()

	pr := PaneRow{Key: paneKey("main", 1, 0, "h"), Session: "main", Window: 1, Pane: 0, Pid: 42, Cmd: "zsh", Path: "/tmp", Active: true, Free: true}
	if err := s.upsertPane(ctx, pr, now); err != nil {
		t.Fatalf("upsertPane: %v", err)
	}
	panes, err := s.scanPanes(ctx, "h")
	if err != nil || len(panes) != 1 {
		t.Fatalf("scanPanes got %d %v", len(panes), err)
	}
	if panes[0].HostID != "h" || !panes[0].Free || panes[0].StaleSince != nil {
		t.Fatalf("pane fields wrong: %+v", panes[0])
	}
	if got, _ := s.scanPanes(ctx, "other"); len(got) != 0 {
		t.Fatalf("host filter leaked %d rows", len(got))
	}

	live, _ := s.livePaneKeys(ctx)
	if len(live) != 1 {
		t.Fatalf("livePaneKeys got %d", len(live))
	}
	if err := s.markPaneStale(ctx, pr.Key, now); err != nil {
		t.Fatalf("markPaneStale: %v", err)
	}
	if live, _ := s.livePaneKeys(ctx); len(live) != 0 {
		t.Fatalf("stale pane still live")
	}
	panes, _ = s.scanPanes(ctx, "h")
	if panes[0].StaleSince == nil {
		t.Fatal("StaleSince not set after markPaneStale")
	}
}

func TestPanesForBranch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()
	_ = s.upsertSession(ctx, SessionRow{Key: sessionKey("work", "h"), Name: "work", Worktree: "/w", Branch: "plugin/tmux"}, now)
	_ = s.upsertSession(ctx, SessionRow{Key: sessionKey("trunk", "h"), Name: "trunk", Worktree: "/t", Branch: "main"}, now)
	_ = s.upsertPane(ctx, PaneRow{Key: paneKey("work", 0, 0, "h"), Session: "work", Cmd: "zsh"}, now)
	_ = s.upsertPane(ctx, PaneRow{Key: paneKey("trunk", 0, 0, "h"), Session: "trunk", Cmd: "zsh"}, now)

	got, err := s.panesForBranch(ctx, "plugin/tmux")
	if err != nil {
		t.Fatalf("panesForBranch: %v", err)
	}
	if len(got) != 1 || got[0].Session != "work" {
		t.Fatalf("branch join wrong: %+v", got)
	}
}

func TestMarkAllStale(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()
	_ = s.upsertSession(ctx, SessionRow{Key: sessionKey("s", "h"), Name: "s"}, now)
	_ = s.upsertPane(ctx, PaneRow{Key: paneKey("s", 0, 0, "h"), Session: "s", Cmd: "zsh"}, now)
	if err := s.markAllStale(ctx, now); err != nil {
		t.Fatalf("markAllStale: %v", err)
	}
	sess, _ := s.scanSessions(ctx, "h")
	panes, _ := s.scanPanes(ctx, "h")
	if sess[0].StaleSince == nil || panes[0].StaleSince == nil {
		t.Fatal("markAllStale left an entity live")
	}
}
