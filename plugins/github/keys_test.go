package github

import "testing"

// TestKeyGrammar drives the kindSpecs data table: for each kind the key parses to
// the right REST path and typename, and the scope/composite tags come out as the
// purge model expects.
func TestKeyGrammar(t *testing.T) {
	cases := []struct {
		key, rest, typename, scope, comp string
	}{
		{"issue:o/r#5", "/repos/o/r/issues/5", "Issue", "repo:o/r", "repo:o/r|issue"},
		{"pr:o/r#12", "/repos/o/r/pulls/12", "PullRequest", "repo:o/r", "repo:o/r|pr"},
		{"repo:o/r", "/repos/o/r", "Repository", "repo:o/r", "repo:o/r|repo"},
		{"checkRun:o/r/99", "/repos/o/r/check-runs/99", "CheckRun", "repo:o/r", "repo:o/r|checkRun"},
		{"review:o/r#5/88", "/repos/o/r/pulls/5/reviews/88", "PullRequestReview", "repo:o/r", "repo:o/r|review"},
		{"comment:o/r#5/77", "/repos/o/r/issues/comments/77", "IssueComment", "repo:o/r", "repo:o/r|comment"},
		{"label:o/r/bug", "/repos/o/r/labels/bug", "Label", "repo:o/r", "repo:o/r|label"},
		{"release:o/r/v1.2", "/repos/o/r/releases/tags/v1.2", "Release", "repo:o/r", "repo:o/r|release"},
		{"commit:o/r/deadbeef", "/repos/o/r/commits/deadbeef", "Commit", "repo:o/r", "repo:o/r|commit"},
		{"ref:o/r/heads/main", "/repos/o/r/git/refs/heads/main", "Ref", "repo:o/r", "repo:o/r|ref"},
		{"user:octocat", "/users/octocat", "User", "", ""},
	}
	for _, c := range cases {
		t.Run(c.key, func(t *testing.T) {
			if got := restPath(c.key); got != c.rest {
				t.Errorf("restPath = %q, want %q", got, c.rest)
			}
			if got := typenameFor(c.key); got != c.typename {
				t.Errorf("typenameFor = %q, want %q", got, c.typename)
			}
			if got := scopePrefix(c.key); got != c.scope {
				t.Errorf("scopePrefix = %q, want %q", got, c.scope)
			}
			if got := composite(c.key); got != c.comp {
				t.Errorf("composite = %q, want %q", got, c.comp)
			}
		})
	}
}

// TestKeyHostSuffix confirms the optional @host suffix is stripped before parsing.
func TestKeyHostSuffix(t *testing.T) {
	if got := restPath("issue:o/r#5@box1"); got != "/repos/o/r/issues/5" {
		t.Errorf("restPath with host = %q", got)
	}
	if got := scopePrefix("issue:o/r#5@box1"); got != "repo:o/r" {
		t.Errorf("scopePrefix with host = %q", got)
	}
}

// TestEventKey drives the webhook event→key half of the table: each mapped event
// resolves to the object key it touches; unmapped or repo-less payloads do not.
func TestEventKey(t *testing.T) {
	repo := map[string]any{"full_name": "o/r"}
	cases := []struct {
		name, event string
		payload     map[string]any
		want        string
		ok          bool
	}{
		{"issues", "issues", map[string]any{"repository": repo, "issue": map[string]any{"number": float64(5)}}, "issue:o/r#5", true},
		{"issue_comment→issue", "issue_comment", map[string]any{"repository": repo, "issue": map[string]any{"number": float64(5)}}, "issue:o/r#5", true},
		{"pull_request", "pull_request", map[string]any{"repository": repo, "pull_request": map[string]any{"number": float64(12)}}, "pr:o/r#12", true},
		{"review→pr", "pull_request_review", map[string]any{"repository": repo, "pull_request": map[string]any{"number": float64(12)}}, "pr:o/r#12", true},
		{"check_run", "check_run", map[string]any{"repository": repo, "check_run": map[string]any{"id": float64(99)}}, "checkRun:o/r/99", true},
		{"label", "label", map[string]any{"repository": repo, "label": map[string]any{"name": "bug"}}, "label:o/r/bug", true},
		{"release", "release", map[string]any{"repository": repo, "release": map[string]any{"tag_name": "v1.2"}}, "release:o/r/v1.2", true},
		{"unknown event", "star", map[string]any{"repository": repo}, "", false},
		{"missing repo", "issues", map[string]any{"issue": map[string]any{"number": float64(5)}}, "", false},
		{"missing object", "issues", map[string]any{"repository": repo}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := eventKey(c.event, c.payload)
			if ok != c.ok || got != c.want {
				t.Errorf("eventKey(%q) = (%q,%v), want (%q,%v)", c.event, got, ok, c.want, c.ok)
			}
		})
	}
}

// TestRepoFullNameFromOwnerBlock covers the fallback path where the payload carries
// name+owner.login instead of full_name.
func TestRepoFullNameFromOwnerBlock(t *testing.T) {
	payload := map[string]any{
		"repository": map[string]any{"name": "r", "owner": map[string]any{"login": "o"}},
		"issue":      map[string]any{"number": float64(7)},
	}
	got, ok := eventKey("issues", payload)
	if !ok || got != "issue:o/r#7" {
		t.Errorf("eventKey = (%q,%v)", got, ok)
	}
}
