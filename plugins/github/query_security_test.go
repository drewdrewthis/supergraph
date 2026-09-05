package github

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// seedIssue writes one cached issue node under o/<repo> so the nodesByKind LIKE
// pattern can be exercised.
func seedIssue(t *testing.T, p *Plugin, repo string, n int) {
	t.Helper()
	now := time.Now().UTC()
	key := "issue:o/" + repo + "#" + strconv.Itoa(n)
	if err := p.store.upsert(context.Background(), &node{
		Key: key, Typename: "Issue", JSON: []byte(`{"number":` + strconv.Itoa(n) + `}`),
		FetchedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed %q: %v", key, err)
	}
}

// TestIssuesForRepoRejectsWildcardScope guards S1: an owner/repo carrying a SQL LIKE
// metacharacter must be rejected up front (safeName), never interpolated into the
// scan pattern where "%" would match every repo.
func TestIssuesForRepoRejectsWildcardScope(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	seedIssue(t, p, "r", 5)
	if got := IssuesForRepo(ctx, "o", "%"); len(got) != 0 {
		t.Errorf(`IssuesForRepo(o, "%%") = %v, want empty (wildcard scope rejected)`, got)
	}
	if got := CachedPRs(ctx, "%", "r"); len(got) != 0 {
		t.Errorf(`CachedPRs("%%", r) = %v, want empty`, got)
	}
}

// TestNodesByKindEscapesUnderscore guards S1: a repo literally named "a_b" (valid
// under safeName, which admits '_') must match only "a_b" — the LIKE '_' wildcard is
// escaped, so it does NOT match a sibling repo "axb".
func TestNodesByKindEscapesUnderscore(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	seedIssue(t, p, "a_b", 6)
	seedIssue(t, p, "axb", 7)

	got := IssuesForRepo(ctx, "o", "a_b")
	if len(got) != 1 || got[0].Number != 6 {
		t.Fatalf(`IssuesForRepo(o, "a_b") = %+v, want exactly issue #6 (no wildcard match on "axb")`, got)
	}
}

// TestIssueRejectsUnsafeKey guards S1 on the single-node reads: a key whose scope is
// not allowlist-safe returns nil rather than driving a lookup.
func TestIssueRejectsUnsafeKey(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	seedIssue(t, p, "r", 5)
	if got := Issue(ctx, "issue:o/%#5"); got != nil {
		t.Errorf("Issue with unsafe scope = %+v, want nil", got)
	}
	if got := PullRequest(ctx, "pr:o/..%#5"); got != nil {
		t.Errorf("PullRequest with unsafe scope = %+v, want nil", got)
	}
}
