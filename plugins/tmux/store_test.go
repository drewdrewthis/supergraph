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

func TestMigrateIdempotentPreservesRows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t) // newStore already ran migrate once
	now := time.Now().UTC()

	_ = s.upsertSession(ctx, SessionRow{Key: sessionKey("s", "h"), Name: "s", Worktree: "/w", Branch: "b",
		Attached: true, CreatedAt: &now}, now)
	_ = s.upsertPane(ctx, PaneRow{Key: paneKey("s", 1, 0, "h"), Session: "s", Cmd: "zsh",
		PaneID: "%3", WindowName: "editor", WindowActive: true}, now)

	// A second migrate over the already-migrated DB must be a no-op (duplicate-column
	// error tolerated) and must not drop or alter the pre-existing rows.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate must be a no-op: %v", err)
	}
	sess, _ := s.scanSessions(ctx, "h")
	if len(sess) != 1 || sess[0].Name != "s" || !sess[0].Attached || sess[0].CreatedAt == nil {
		t.Fatalf("session row not preserved through re-migrate: %+v", sess)
	}
	panes, _ := s.scanPanes(ctx, "h")
	if len(panes) != 1 || panes[0].PaneID != "%3" || panes[0].WindowName != "editor" || !panes[0].WindowActive {
		t.Fatalf("pane row / #27 columns not preserved: %+v", panes)
	}
}

// TestMigrateUpgradesPreExistingDB exercises the ACTUAL upgrade path
// (AC-TMUX-MIGRATE-INPLACE) that TestMigrateIdempotentPreservesRows does not: it
// starts from a genuinely pre-#27 database (only the base `schema`, none of the
// five addColumns applied), inserts rows with the OLD column set, then runs the
// current migrate and checks the pre-existing rows survive untouched, the five new
// columns exist and read as zero/NULL for those old rows, a subsequent write using
// the new fields works, and a second migrate is still a no-op.
func TestMigrateUpgradesPreExistingDB(t *testing.T) {
	ctx := context.Background()
	cs, err := core.OpenStore(ctx, filepath.Join(t.TempDir(), "tmux.db"), 100)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	s := &store{core: cs}

	// Run only the base `schema` — the pre-#27 column set — never s.migrate, so none
	// of the five #27 columns exist yet.
	if _, err := s.db().ExecContext(ctx, schema); err != nil {
		t.Fatalf("pre-#27 schema: %v", err)
	}
	now := time.Now().UTC().Format(rfc)
	if _, err := s.db().ExecContext(ctx,
		`INSERT INTO tmux_sessions (key, name, worktree, branch, last_seen_at, stale_since) VALUES (?,?,?,?,?,NULL)`,
		sessionKey("s", "h"), "s", "/w", "b", now); err != nil {
		t.Fatalf("insert pre-#27 session: %v", err)
	}
	if _, err := s.db().ExecContext(ctx,
		`INSERT INTO tmux_panes (key, session, window, pane, pid, cmd, path, active, free, last_seen_at, stale_since) VALUES (?,?,?,?,?,?,?,?,?,?,NULL)`,
		paneKey("s", 1, 0, "h"), "s", 1, 0, 42, "zsh", "/tmp", 1, 1, now); err != nil {
		t.Fatalf("insert pre-#27 pane: %v", err)
	}

	// Sanity: the #27 columns genuinely do not exist yet. If this ever passes, the
	// test below isn't exercising the upgrade path at all.
	if _, err := s.db().ExecContext(ctx, `SELECT attached FROM tmux_sessions LIMIT 1`); err == nil {
		t.Fatal("attached column already exists before migrate — test setup is not pre-#27")
	}

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate over a pre-#27 DB must succeed: %v", err)
	}

	sess, err := s.scanSessions(ctx, "h")
	if err != nil || len(sess) != 1 {
		t.Fatalf("scanSessions after migrate: got %d, err %v", len(sess), err)
	}
	if sess[0].Name != "s" || sess[0].Worktree != "/w" || sess[0].Branch != "b" {
		t.Fatalf("pre-existing session row values changed across migrate: %+v", sess[0])
	}
	if sess[0].Attached || sess[0].CreatedAt != nil {
		t.Fatalf("new session columns on a pre-#27 row must read as zero/NULL: %+v", sess[0])
	}

	panes, err := s.scanPanes(ctx, "h")
	if err != nil || len(panes) != 1 {
		t.Fatalf("scanPanes after migrate: got %d, err %v", len(panes), err)
	}
	if panes[0].Session != "s" || panes[0].Cmd != "zsh" || panes[0].Path != "/tmp" || !panes[0].Active || !panes[0].Free {
		t.Fatalf("pre-existing pane row values changed across migrate: %+v", panes[0])
	}
	if panes[0].PaneID != "" || panes[0].WindowName != "" || panes[0].WindowActive {
		t.Fatalf("new pane columns on a pre-#27 row must read as zero/NULL: %+v", panes[0])
	}

	// A subsequent write using the new #27 fields must succeed post-migration.
	now2 := time.Now().UTC()
	if err := s.upsertSession(ctx, SessionRow{Key: sessionKey("s", "h"), Name: "s", Worktree: "/w", Branch: "b",
		Attached: true, CreatedAt: &now2}, now2); err != nil {
		t.Fatalf("upsertSession with new fields after migrate: %v", err)
	}
	if err := s.upsertPane(ctx, PaneRow{Key: paneKey("s", 1, 0, "h"), Session: "s", Cmd: "zsh",
		PaneID: "%3", WindowName: "editor", WindowActive: true}, now2); err != nil {
		t.Fatalf("upsertPane with new fields after migrate: %v", err)
	}
	sess, _ = s.scanSessions(ctx, "h")
	if len(sess) != 1 || !sess[0].Attached || sess[0].CreatedAt == nil {
		t.Fatalf("session new-field write not visible after migrate: %+v", sess)
	}
	panes, _ = s.scanPanes(ctx, "h")
	if len(panes) != 1 || panes[0].PaneID != "%3" || panes[0].WindowName != "editor" || !panes[0].WindowActive {
		t.Fatalf("pane new-field write not visible after migrate: %+v", panes)
	}

	// A second migrate over the now-upgraded DB is still a no-op.
	if err := s.migrate(ctx); err != nil {
		t.Fatalf("second migrate after upgrade must be a no-op: %v", err)
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
