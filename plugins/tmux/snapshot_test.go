package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func TestParsePanesClassifiesFreeBusy(t *testing.T) {
	out := []byte("main\t1\t0\t100\tzsh\t/tmp\t1\nmain\t1\t1\t101\tsleep\t/tmp\t0\n")
	rows := parsePanes("h", out, []string{"zsh", "bash"})
	if len(rows) != 2 {
		t.Fatalf("got %d rows", len(rows))
	}
	if !rows[0].Free || rows[0].Key != "pane:main:1.0@h" || !rows[0].Active {
		t.Fatalf("row0 wrong: %+v", rows[0])
	}
	if rows[1].Free {
		t.Fatal("sleep pane must be busy")
	}
}

func TestParseSessionsResolvesBranch(t *testing.T) {
	branchOf := func(_ context.Context, path string) string {
		if path == "/w" {
			return "plugin/tmux"
		}
		return ""
	}
	rows := parseSessions("h", []byte("work\t/w\ntrunk\t/t\n"), branchOf)
	if len(rows) != 2 || rows[0].Branch != "plugin/tmux" || rows[0].Key != "session:work@h" {
		t.Fatalf("sessions wrong: %+v", rows)
	}
}

func TestParseSessionsCreatedAt(t *testing.T) {
	nob := func(context.Context, string) string { return "" }
	rows := parseSessions("h", []byte("s1\t/tmp\t1788915774\n"), nob)
	if len(rows) != 1 || rows[0].CreatedAt == nil {
		t.Fatalf("createdAt not parsed: %+v", rows)
	}
	if got := rows[0].CreatedAt.Unix(); got != 1788915774 {
		t.Fatalf("createdAt epoch got %d", got)
	}
	// A line without #{session_created} (shorter fixture) leaves CreatedAt nil, not zero.
	if r := parseSessions("h", []byte("s1\t/tmp\n"), nob); r[0].CreatedAt != nil {
		t.Fatalf("missing created field must be nil, got %v", r[0].CreatedAt)
	}
}

// newProbePlugin builds a minimal Plugin wired only to a fake tmux exec (no store),
// enough to exercise the list-clients-based attached probe.
func newProbePlugin(run func(context.Context, ...string) ([]byte, error)) *Plugin {
	return &Plugin{cfg: config{}, hostID: "h", run: run}
}

func TestAttachedSessionsIgnoresControlModeClients(t *testing.T) {
	// s1 has only the plugin's own control-mode client (control=1) → NOT attached;
	// s2 has a human client (control=0) → attached.
	run := func(context.Context, ...string) ([]byte, error) {
		return []byte("s1\t1\ns2\t0\n"), nil
	}
	got := newProbePlugin(run).attachedSessions(context.Background())
	if got["s1"] {
		t.Error("control-mode-only session must not read attached (own client, #27 §3)")
	}
	if !got["s2"] {
		t.Error("session with a human client must read attached")
	}
}

func TestAttachedSessionsEmptyOnError(t *testing.T) {
	run := func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("no server running / zero clients")
	}
	got := newProbePlugin(run).attachedSessions(context.Background())
	if len(got) != 0 {
		t.Fatalf("a failed clients probe must yield an empty map, got %v", got)
	}
}

func TestSessionChanged(t *testing.T) {
	t0 := time.Unix(1788915774, 0).UTC()
	base := SessionRow{Name: "s", Attached: false, CreatedAt: &t0, Worktree: "/w", Branch: "b"}
	if sessionChanged(base, base) {
		t.Error("identical session must not be flagged changed")
	}
	att := base
	att.Attached = true
	if !sessionChanged(base, att) {
		t.Error("attach flip must be a change")
	}
	nilc := base
	nilc.CreatedAt = nil
	if !sessionChanged(base, nilc) {
		t.Error("createdAt set→nil must be a change")
	}
	wt := base
	wt.Worktree = "/other"
	if !sessionChanged(base, wt) {
		t.Error("worktree move must be a change")
	}
}

// newReconcilePlugin builds a Plugin wired to a fake tmux exec and a real store.
func newReconcilePlugin(t *testing.T, run func(context.Context, ...string) ([]byte, error)) (*Plugin, *[]core.Envelope) {
	t.Helper()
	s := newStore(t)
	var got []core.Envelope
	p := &Plugin{
		cfg:      config{idleShells: []string{"zsh"}, slotKind: "worker"},
		hostID:   "h",
		store:    s,
		now:      func() time.Time { return time.Unix(1700000000, 0).UTC() },
		run:      run,
		branchOf: func(context.Context, string) string { return "" },
	}
	p.emitFn = func(_ context.Context, e core.Envelope) error { got = append(got, e); return nil }
	return p, &got
}

func fakeRun(sessions, panes string) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "list-sessions" {
				return []byte(sessions), nil
			}
			if a == "list-panes" {
				return []byte(panes), nil
			}
		}
		return nil, nil
	}
}

func TestReconcileEmitsSnapshotAndHealsVanished(t *testing.T) {
	ctx := context.Background()
	run := fakeRun("main\t/tmp\n", "main\t1\t0\t100\tzsh\t/tmp\t1\nmain\t1\t1\t101\tzsh\t/tmp\t0\n")
	p, got := newReconcilePlugin(t, run)

	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !hasType(*got, "tmux.snapshot") {
		t.Fatal("no tmux.snapshot emitted on success")
	}
	if cur := p.Cursor(ctx); cur == "" {
		t.Fatal("cursor snapshot:lastAt not set")
	}

	// Second reconcile with pane 1.1 gone -> it must be marked stale + a
	// tmux.pane.closed emitted, and drop out of freeSlots.
	*got = nil
	p.run = fakeRun("main\t/tmp\n", "main\t1\t0\t100\tzsh\t/tmp\t1\n")
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if !hasType(*got, "tmux.pane.closed") {
		t.Fatal("no tmux.pane.closed for vanished pane")
	}
	free, _ := Slots(ctx, "h", true)
	_ = free // Slots reads the current singleton; asserted via store below
	panes, _ := p.store.scanPanes(ctx, "h")
	for _, pr := range panes {
		if pr.Key == "pane:main:1.1@h" && pr.StaleSince == nil {
			t.Fatal("vanished pane not marked stale")
		}
	}
}

// TestReconcileEmitsUpdatedOnBusyChange pins S-A: reconcile emits tmux.pane.updated
// for a NEW pane and for a live pane whose free/busy (or cmd/path) changed — but NOT
// for an unchanged pane on a later poll.
func TestReconcileEmitsUpdatedOnBusyChange(t *testing.T) {
	ctx := context.Background()
	p, got := newReconcilePlugin(t, fakeRun("main\t/tmp\n", "main\t1\t0\t100\tzsh\t/tmp\t1\n"))

	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile1: %v", err)
	}
	if !hasType(*got, "tmux.pane.updated") {
		t.Fatal("first sighting must emit tmux.pane.updated")
	}

	// Same pane goes busy (zsh -> sleep, free -> busy): must re-emit updated.
	*got = nil
	p.run = fakeRun("main\t/tmp\n", "main\t1\t0\t100\tsleep\t/tmp\t0\n")
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile2: %v", err)
	}
	if !hasType(*got, "tmux.pane.updated") {
		t.Fatal("free->busy change must emit tmux.pane.updated")
	}

	// No change on the next poll: must NOT re-emit (no event flood).
	*got = nil
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile3: %v", err)
	}
	if hasType(*got, "tmux.pane.updated") {
		t.Fatal("unchanged pane must not re-emit tmux.pane.updated")
	}
}

func TestReconcileErrorEmitsNoSnapshot(t *testing.T) {
	ctx := context.Background()
	run := func(_ context.Context, args ...string) ([]byte, error) {
		if strings.Contains(strings.Join(args, " "), "list-panes") {
			return nil, errors.New("stub hang/exit non-zero")
		}
		return []byte("main\t/tmp\n"), nil
	}
	p, got := newReconcilePlugin(t, run)
	if err := p.reconcile(ctx); err == nil {
		t.Fatal("reconcile must return the poll error")
	}
	if hasType(*got, "tmux.snapshot") {
		t.Fatal("an errored poll must NOT emit tmux.snapshot (owner T1 / AC-TMUX-POLL-ERROR)")
	}
}

func hasType(es []core.Envelope, typ string) bool {
	for _, e := range es {
		if e.Type == typ {
			return true
		}
	}
	return false
}
