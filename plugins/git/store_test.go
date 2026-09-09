package git

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
	cs, err := core.OpenStore(ctx, filepath.Join(t.TempDir(), "git.db"), 100)
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

func intp(v int) *int { return &v }

// TestUpsertScanRoundTrip covers the repo/worktree round-trip and the load-bearing
// ahead/behind nil-vs-0 distinction (AC-GIT-AHEAD-BEHIND): a nil pointer must read
// back nil, and a genuine 0 must read back 0 — never coerced into each other.
func TestUpsertScanRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()

	repoKey := "h:/repo/a"
	if err := s.upsertRepo(ctx, RepoNode{Key: repoKey, HostID: "h", Slug: "owner/a", Root: "/repo/a"}, now); err != nil {
		t.Fatalf("upsertRepo: %v", err)
	}
	// Worktree with a known upstream position: ahead 0, behind 3.
	wtLevel := WorktreeNode{Key: "h:/repo/a", HostID: "h", RepoKey: repoKey, Path: "/repo/a", Branch: "main", Head: "abc", Ahead: intp(0), Behind: intp(3)}
	// Worktree with no upstream: both nil.
	wtNoUp := WorktreeNode{Key: "h:/repo/a/wt", HostID: "h", RepoKey: repoKey, Path: "/repo/a/wt", Branch: "feature", Head: "def"}
	if err := s.upsertWorktree(ctx, wtLevel, now); err != nil {
		t.Fatalf("upsertWorktree level: %v", err)
	}
	if err := s.upsertWorktree(ctx, wtNoUp, now); err != nil {
		t.Fatalf("upsertWorktree noup: %v", err)
	}

	got, err := s.scanWorktrees(ctx, repoKey)
	if err != nil || len(got) != 2 {
		t.Fatalf("scanWorktrees got %d %v", len(got), err)
	}
	byKey := map[string]WorktreeNode{}
	for _, w := range got {
		byKey[w.Key] = w
	}

	lvl := byKey["h:/repo/a"]
	if lvl.RepoSlug != "owner/a" {
		t.Fatalf("repo slug not joined: %q", lvl.RepoSlug)
	}
	if lvl.Ahead == nil || *lvl.Ahead != 0 {
		t.Fatalf("ahead 0 did not round-trip as 0: %v", lvl.Ahead)
	}
	if lvl.Behind == nil || *lvl.Behind != 3 {
		t.Fatalf("behind 3 did not round-trip: %v", lvl.Behind)
	}

	noup := byKey["h:/repo/a/wt"]
	if noup.Ahead != nil || noup.Behind != nil {
		t.Fatalf("nil ahead/behind did not round-trip as nil: %v %v", noup.Ahead, noup.Behind)
	}
}

func TestScanReposHostFilter(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()
	_ = s.upsertRepo(ctx, RepoNode{Key: "h:/r", HostID: "h", Slug: "o/r", Root: "/r"}, now)
	_ = s.upsertRepo(ctx, RepoNode{Key: "g:/r", HostID: "g", Slug: "o/r", Root: "/r"}, now)

	if got := mustRepos(t, s, "h"); len(got) != 1 || got[0].HostID != "h" {
		t.Fatalf("host filter wrong: %+v", got)
	}
	if got := mustRepos(t, s, ""); len(got) != 2 {
		t.Fatalf("empty host should return all, got %d", len(got))
	}
}

func mustRepos(t *testing.T, s *store, host string) []RepoNode {
	t.Helper()
	got, err := s.scanRepos(context.Background(), host)
	if err != nil {
		t.Fatalf("scanRepos: %v", err)
	}
	return got
}

// TestMarkStaleScopedToRepo asserts markWorktreesStaleForRepo stale-marks only the
// absent keys AND only within its own repoKey: a second repo's rows must stay live
// even though they are absent from the first repo's keepKeys (AC-GIT-RECONCILE).
func TestMarkStaleScopedToRepo(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	now := time.Now().UTC()

	repoA, repoB := "h:/repo/a", "h:/repo/b"
	_ = s.upsertRepo(ctx, RepoNode{Key: repoA, HostID: "h", Slug: "o/a", Root: "/repo/a"}, now)
	_ = s.upsertRepo(ctx, RepoNode{Key: repoB, HostID: "h", Slug: "o/b", Root: "/repo/b"}, now)
	_ = s.upsertWorktree(ctx, WorktreeNode{Key: "h:/repo/a/keep", RepoKey: repoA, HostID: "h", Path: "/repo/a/keep"}, now)
	_ = s.upsertWorktree(ctx, WorktreeNode{Key: "h:/repo/a/gone", RepoKey: repoA, HostID: "h", Path: "/repo/a/gone"}, now)
	_ = s.upsertWorktree(ctx, WorktreeNode{Key: "h:/repo/b/keep", RepoKey: repoB, HostID: "h", Path: "/repo/b/keep"}, now)

	gone, err := s.markWorktreesStaleForRepo(ctx, repoA, []string{"h:/repo/a/keep"}, now)
	if err != nil {
		t.Fatalf("markWorktreesStaleForRepo: %v", err)
	}
	if len(gone) != 1 || gone[0] != "h:/repo/a/gone" {
		t.Fatalf("newly-staled set wrong: %v", gone)
	}

	a := worktreesByKey(t, s, repoA)
	if a["h:/repo/a/gone"].StaleSince == nil {
		t.Fatal("absent worktree not stale-marked")
	}
	if a["h:/repo/a/keep"].StaleSince != nil {
		t.Fatal("kept worktree wrongly stale-marked")
	}
	// The other repo is untouched — a stale-mark scoped to repoA never reaches repoB.
	b := worktreesByKey(t, s, repoB)
	if b["h:/repo/b/keep"].StaleSince != nil {
		t.Fatal("other repo's worktree was stale-marked across repoKey boundary")
	}

	// Re-running with the same keepKeys reports nothing new (already stale).
	gone2, _ := s.markWorktreesStaleForRepo(ctx, repoA, []string{"h:/repo/a/keep"}, now)
	if len(gone2) != 0 {
		t.Fatalf("re-mark reported already-stale rows: %v", gone2)
	}
}

func worktreesByKey(t *testing.T, s *store, repoKey string) map[string]WorktreeNode {
	t.Helper()
	got, err := s.scanWorktrees(context.Background(), repoKey)
	if err != nil {
		t.Fatalf("scanWorktrees: %v", err)
	}
	m := map[string]WorktreeNode{}
	for _, w := range got {
		m[w.Key] = w
	}
	return m
}
