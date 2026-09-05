package claude

import (
	"bufio"
	"context"
	"path/filepath"
	"strings"
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

func TestValidSessionID(t *testing.T) {
	for _, s := range []string{"abc", "a-b_c.d", "0f8c46811-bf1a", strings.Repeat("a", 128)} {
		if !validSessionID(s) {
			t.Errorf("validSessionID(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"", "has space", "semi;colon", "slash/x", "at@host", strings.Repeat("a", 129)} {
		if validSessionID(s) {
			t.Errorf("validSessionID(%q) = true, want false", s)
		}
	}
}

func TestReadLineCapsOversized(t *testing.T) {
	big := strings.Repeat("x", 2000)
	rd := bufio.NewReader(strings.NewReader("short\n" + big + "\nafter\n"))

	line, oversized, consumed, complete := readLine(rd, 100)
	if !complete || oversized || string(line) != "short\n" || consumed != 6 {
		t.Fatalf("line1 = %q ov=%v c=%d ok=%v; want short\\n,false,6,true", line, oversized, consumed, complete)
	}
	line, oversized, consumed, complete = readLine(rd, 100)
	if !complete || !oversized || line != nil || consumed != int64(len(big)+1) {
		t.Fatalf("line2 = %q ov=%v c=%d ok=%v; want nil,true,%d,true", line, oversized, consumed, complete, len(big)+1)
	}
	line, oversized, _, complete = readLine(rd, 100)
	if !complete || oversized || string(line) != "after\n" {
		t.Fatalf("line3 = %q ov=%v ok=%v; want after\\n,false,true", line, oversized, complete)
	}
	// A trailing line with no newline is a partial write: consumed 0, not complete.
	rd2 := bufio.NewReader(strings.NewReader("partial-no-newline"))
	if _, _, c, ok := readLine(rd2, 100); ok || c != 0 {
		t.Fatalf("partial line: consumed=%d complete=%v; want 0,false", c, ok)
	}
}

func TestPruneRemovesOldSessionsAndFolds(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recent := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	if _, err := st.applyFold(ctx, foldInput{sid: "old", event: "SessionStart", ts: "T1"}, "h", old); err != nil {
		t.Fatal(err)
	}
	if _, err := st.applyFold(ctx, foldInput{sid: "new", event: "SessionStart", ts: "T2"}, "h", recent); err != nil {
		t.Fatal(err)
	}
	if err := st.prune(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if r, _ := st.get(ctx, "old"); r != nil {
		t.Fatal("session older than cutoff should be pruned")
	}
	if r, _ := st.get(ctx, "new"); r == nil {
		t.Fatal("session newer than cutoff must survive")
	}
	// The pruned session's fold row is gone too, so a later replay is not deduped away.
	var folds int
	_ = st.db().QueryRowContext(ctx, `SELECT COUNT(*) FROM claude_folds WHERE sid='old'`).Scan(&folds)
	if folds != 0 {
		t.Fatalf("pruned session folds = %d, want 0", folds)
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
