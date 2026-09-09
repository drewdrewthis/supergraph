package git

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// fakeGit is the injected runner (AC-GIT-RECONCILE's seam): it answers `git worktree
// list` per root from a fixture map, can fail a chosen root's list, and errors
// rev-list by default so ahead/behind stay nil unless a fixture supplies a count.
type fakeGit struct {
	list    map[string]string
	listErr map[string]bool
	revlist map[string]string
}

func (f *fakeGit) run(ctx context.Context, dir string, args ...string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	switch args[0] {
	case "worktree":
		if f.listErr[dir] {
			return "", errors.New("fatal: not a git repository")
		}
		return f.list[dir], nil
	case "remote":
		return "", nil // no origin → RepoSlug falls back to the dir basename
	case "rev-list":
		if v, ok := f.revlist[dir]; ok {
			return v, nil
		}
		return "", errors.New("fatal: no upstream configured")
	}
	return "", nil
}

func newPlugin(t *testing.T, roots []string) (*Plugin, *fakeGit) {
	t.Helper()
	fg := &fakeGit{list: map[string]string{}, listErr: map[string]bool{}, revlist: map[string]string{}}
	p := &Plugin{
		cfg:    config{roots: roots, reconcileInterval: time.Second, configured: true},
		hostID: "h",
		store:  newStore(t),
		run:    fg.run,
		now:    func() time.Time { return time.Unix(1, 0).UTC() },
		hashes: map[string]string{},
	}
	return p, fg
}

func wt(path, head, branch string) string {
	return "worktree " + path + "\nHEAD " + head + "\nbranch refs/heads/" + branch + "\n\n"
}

func countType(es []core.Envelope, typ string) int {
	n := 0
	for _, e := range es {
		if e.Type == typ {
			n++
		}
	}
	return n
}

// TestReconcileVanishedWorktreeStaled: a worktree present in the first reconcile but
// absent from the second is stale-marked, yet still returned by scanWorktrees (a
// vanished row is retained-and-flagged, not deleted — AC-GIT-RECONCILE).
func TestReconcileVanishedWorktreeStaled(t *testing.T) {
	ctx := context.Background()
	p, fg := newPlugin(t, []string{"/repo/a"})

	fg.list["/repo/a"] = wt("/repo/a", "aaa", "main") + wt("/repo/a/feat", "bbb", "feat")
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	fg.list["/repo/a"] = wt("/repo/a", "aaa", "main")
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	got := worktreesByKey(t, p.store, "h:/repo/a")
	if len(got) != 2 {
		t.Fatalf("vanished worktree should be retained: %d rows", len(got))
	}
	if got["h:/repo/a/feat"].StaleSince == nil {
		t.Fatal("vanished worktree not stale-marked")
	}
	if got["h:/repo/a"].StaleSince != nil {
		t.Fatal("surviving worktree wrongly stale-marked")
	}
}

// TestReconcileRootErrorIsolated: root A's `worktree list` failing leaves A's cached
// rows unchanged and un-staled, while root B still reconciles normally — one root's
// git error is contained to that root (AC-GIT-RECONCILE).
func TestReconcileRootErrorIsolated(t *testing.T) {
	ctx := context.Background()
	p, fg := newPlugin(t, []string{"/repo/a", "/repo/b"})

	fg.list["/repo/a"] = wt("/repo/a", "aaa", "main")
	fg.list["/repo/b"] = wt("/repo/b", "b111", "main")
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	// A now fails; B advances to a new head.
	fg.listErr["/repo/a"] = true
	fg.list["/repo/b"] = wt("/repo/b", "b222", "main")
	if err := p.reconcile(ctx); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	a := worktreesByKey(t, p.store, "h:/repo/a")
	if a["h:/repo/a"].StaleSince != nil {
		t.Fatal("errored root A's rows were stale-marked")
	}
	if a["h:/repo/a"].Head != "aaa" {
		t.Fatalf("errored root A's row was mutated: head=%q", a["h:/repo/a"].Head)
	}
	b := worktreesByKey(t, p.store, "h:/repo/b")
	if b["h:/repo/b"].Head != "b222" {
		t.Fatalf("healthy root B did not advance: head=%q", b["h:/repo/b"].Head)
	}
}

// TestReconcileEmitHashGated: an unchanged reconcile emits nothing while a new or
// changed worktree emits exactly one git.worktree.updated (AC-GIT-SUBSCRIBE).
func TestReconcileEmitHashGated(t *testing.T) {
	ctx := context.Background()
	p, fg := newPlugin(t, []string{"/repo/a"})

	var emitted []core.Envelope
	p.emitFn = func(ctx context.Context, e core.Envelope) error {
		emitted = append(emitted, e)
		return nil
	}

	fg.list["/repo/a"] = wt("/repo/a", "aaa", "main")
	_ = p.reconcile(ctx)
	if got := countType(emitted, "git.worktree.updated"); got != 1 {
		t.Fatalf("first reconcile: want 1 updated, got %d", got)
	}
	if e := emitted[0]; e.V != 2 || e.Source != "git" {
		t.Fatalf("envelope shape wrong: %+v", e)
	}

	emitted = nil
	_ = p.reconcile(ctx) // identical → hash gate suppresses
	if len(emitted) != 0 {
		t.Fatalf("unchanged reconcile emitted %d envelopes", len(emitted))
	}

	emitted = nil
	fg.list["/repo/a"] = wt("/repo/a", "ccc", "main") // head moved
	_ = p.reconcile(ctx)
	if got := countType(emitted, "git.worktree.updated"); got != 1 {
		t.Fatalf("changed reconcile: want 1 updated, got %d", got)
	}
}
