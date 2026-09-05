package claude

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func TestFoldTransitions(t *testing.T) {
	cases := []struct {
		name     string
		prev     State
		event    string
		permAsk  bool
		want     State
		wantTool bool
		wantEnd  bool
	}{
		{"start->idle", "", "SessionStart", false, stateIdle, false, false},
		{"prompt->working", stateIdle, "UserPromptSubmit", false, stateWorking, false, false},
		{"pretool->working+tool", stateWorking, "PreToolUse", false, stateWorking, true, false},
		{"posttool->working", stateWorking, "PostToolUse", false, stateWorking, false, false},
		{"permission->input", stateWorking, "Notification", true, stateInput, false, false},
		{"nag->idle", stateInput, "Notification", false, stateIdle, false, false},
		{"stop->idle", stateWorking, "Stop", false, stateIdle, false, false},
		{"end->ended", stateWorking, "SessionEnd", false, stateEnded, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, tool, end := fold(c.prev, c.event, c.permAsk)
			if got != c.want || tool != c.wantTool || end != c.wantEnd {
				t.Fatalf("fold(%q,%q,%v) = %q,%v,%v; want %q,%v,%v",
					c.prev, c.event, c.permAsk, got, tool, end, c.want, c.wantTool, c.wantEnd)
			}
		})
	}
}

func TestIsNag(t *testing.T) {
	if !isNag("Claude is waiting for your input") {
		t.Error("expected the idle nag to be classified as a nag")
	}
	if isNag("Allow Claude to run Bash?") {
		t.Error("a permission ask must not be classified as a nag")
	}
}

func TestIssueFromBranch(t *testing.T) {
	cases := map[string]int{"issue42/spike": 42, "issue-7/foo": 7, "plugin/claude": 0, "": 0, "issue1/spike-core": 1}
	for b, want := range cases {
		if got := issueFromBranch(b); got != want {
			t.Errorf("issueFromBranch(%q) = %d, want %d", b, got, want)
		}
	}
}

func TestPRNumberFromURL(t *testing.T) {
	cases := map[string]int{"https://github.com/o/r/pull/12": 12, "https://github.com/o/r/pull/8?x=1": 8, "no-pull": 0}
	for u, want := range cases {
		if got := prNumberFromURL(u); got != want {
			t.Errorf("prNumberFromURL(%q) = %d, want %d", u, got, want)
		}
	}
}

func newTestStore(t *testing.T) *store {
	t.Helper()
	s, err := core.OpenStore(context.Background(), filepath.Join(t.TempDir(), "claude.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	st := &store{core: s}
	if err := st.migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestApplyFoldDedup(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	clock := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	fi := foldInput{sid: "S1", event: "PreToolUse", ts: "T1", tool: "Bash"}

	applied, err := st.applyFold(ctx, fi, "h", clock)
	if err != nil || !applied {
		t.Fatalf("first fold applied=%v err=%v", applied, err)
	}
	// Same (sid,event,ts) is a duplicate: it must not apply or double-count.
	applied, err = st.applyFold(ctx, fi, "h", clock)
	if err != nil || applied {
		t.Fatalf("duplicate fold applied=%v err=%v (want false)", applied, err)
	}
	r, _ := st.get(ctx, "S1")
	if r == nil || r.ToolCalls != 1 || r.State != string(stateWorking) || r.LastTool != "Bash" {
		t.Fatalf("row after dedup = %+v; want toolCalls 1, working, Bash", r)
	}
}

func TestApplyEnrichmentCoalesce(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	clock := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	// Hook-only fold: state working, no branch/model/PR.
	if _, err := st.applyFold(ctx, foldInput{sid: "S1", event: "UserPromptSubmit", ts: "T1"}, "h", clock); err != nil {
		t.Fatal(err)
	}
	// Tail enrichment adds branch/model/PR without touching state.
	if err := st.applyEnrichment(ctx, "S1", "h", enrichment{gitBranch: "plugin/claude", model: "m", prNumber: 8, issueNumber: 0}, clock); err != nil {
		t.Fatal(err)
	}
	r, _ := st.get(ctx, "S1")
	if r.State != string(stateWorking) || r.GitBranch != "plugin/claude" || r.Model != "m" || r.PrNumber != 8 {
		t.Fatalf("row = %+v; want working + enriched fields preserved", r)
	}
}

func TestStaleScanInjectedLiveness(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	clock := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	if _, err := st.applyFold(ctx, foldInput{sid: "dead", event: "SessionStart", ts: "T1", pid: 4242}, "h", clock); err != nil {
		t.Fatal(err)
	}
	dead, err := st.staleScan(ctx, clock, func(pid int) bool { return false }) // injected: all dead
	if err != nil || len(dead) != 1 || dead[0] != "dead" {
		t.Fatalf("staleScan = %v err=%v; want [dead]", dead, err)
	}
	r, _ := st.get(ctx, "dead")
	if r.StaleSince == nil {
		t.Fatal("dead session should carry a staleSince marker, never be deleted")
	}
}
