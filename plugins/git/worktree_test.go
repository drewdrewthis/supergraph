package git

import (
	"fmt"
	"testing"
)

func TestParseWorktreeList(t *testing.T) {
	t.Run("multi-stanza fixture", func(t *testing.T) {
		in := `worktree /repo/supergraph
HEAD 0f3fc9babc
branch refs/heads/main

worktree /repo/supergraph/.worktrees/issue29-git-plugin
HEAD abc123def
branch refs/heads/issue29/git-plugin

worktree /some/detached
HEAD def456
detached

worktree /some/bare
bare
`
		got := parseWorktreeList(in)
		if len(got) != 4 {
			t.Fatalf("len = %d, want 4: %+v", len(got), got)
		}

		w := got[0]
		if w.Path != "/repo/supergraph" || w.Branch != "main" || w.Head != "0f3fc9babc" || w.Detached || w.Bare {
			t.Errorf("stanza 0 = %+v", w)
		}

		w = got[1]
		if w.Path != "/repo/supergraph/.worktrees/issue29-git-plugin" {
			t.Errorf("stanza 1 path = %q", w.Path)
		}
		if w.Branch != "issue29/git-plugin" {
			t.Errorf("stanza 1 branch = %q, want slash-containing branch preserved whole", w.Branch)
		}
		if w.Head != "abc123def" || w.Detached || w.Bare {
			t.Errorf("stanza 1 = %+v", w)
		}

		w = got[2]
		if w.Path != "/some/detached" || w.Head != "def456" || !w.Detached || w.Branch != "" || w.Bare {
			t.Errorf("stanza 2 = %+v", w)
		}

		w = got[3]
		if w.Path != "/some/bare" || !w.Bare || w.Detached || w.Branch != "" || w.Head != "" {
			t.Errorf("stanza 3 = %+v", w)
		}
	})

	t.Run("branch name containing slashes", func(t *testing.T) {
		in := "worktree /x\nHEAD abc\nbranch refs/heads/feature/nested/deep\n"
		got := parseWorktreeList(in)
		if len(got) != 1 || got[0].Branch != "feature/nested/deep" {
			t.Fatalf("got %+v, want branch feature/nested/deep", got)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		got := parseWorktreeList("")
		if len(got) != 0 {
			t.Fatalf("got %+v, want empty", got)
		}
	})

	t.Run("no trailing blank line before EOF", func(t *testing.T) {
		in := "worktree /x\nHEAD abc\nbranch refs/heads/main"
		got := parseWorktreeList(in)
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1 (last stanza must flush without trailing blank line): %+v", len(got), got)
		}
		if got[0].Path != "/x" || got[0].Branch != "main" || got[0].Head != "abc" {
			t.Errorf("got %+v", got[0])
		}
	})

	t.Run("unknown keys are ignored", func(t *testing.T) {
		in := "worktree /x\nHEAD abc\nbranch refs/heads/main\nlocked\nprunable some reason\n"
		got := parseWorktreeList(in)
		if len(got) != 1 || got[0].Path != "/x" || got[0].Branch != "main" {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("stanza with no worktree line is skipped", func(t *testing.T) {
		in := "HEAD abc\nbranch refs/heads/main\n\nworktree /x\nHEAD def\n"
		got := parseWorktreeList(in)
		if len(got) != 1 || got[0].Path != "/x" {
			t.Fatalf("got %+v, want only the stanza with a worktree line", got)
		}
	})
}

func TestParseAheadBehind(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantAhead  *int
		wantBehind *int
	}{
		{"valid tab-separated", "2\t5\n", wtIntp(2), wtIntp(5)},
		{"genuine zero-zero is not nil", "0\t0\n", wtIntp(0), wtIntp(0)},
		{"empty output", "", nil, nil},
		{"garbage", "garbage", nil, nil},
		{"wrong field count", "1\t2\t3", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ahead, behind := parseAheadBehind(tc.in)
			assertIntPtrEqual(t, "ahead", ahead, tc.wantAhead)
			assertIntPtrEqual(t, "behind", behind, tc.wantBehind)
		})
	}
}

func TestSlugFromRemoteURL(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantSlug string
		wantOK   bool
	}{
		{"scp-like with .git", "git@github.com:owner/name.git", "owner/name", true},
		{"scp-like without .git", "git@github.com:owner/name", "owner/name", true},
		{"explicit ssh scheme", "ssh://git@github.com/owner/name.git", "owner/name", true},
		{"https with .git", "https://github.com/owner/name.git", "owner/name", true},
		{"https with trailing slash", "https://github.com/owner/name/", "owner/name", true},
		{"nested group path takes last two segments", "http://gitlab.example.com/group/sub/name.git", "sub/name", true},
		{"local path", "/local/path/repo", "", false},
		{"file scheme", "file:///x/y", "", false},
		{"empty string", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			slug, ok := slugFromRemoteURL(tc.in)
			if slug != tc.wantSlug || ok != tc.wantOK {
				t.Errorf("slugFromRemoteURL(%q) = (%q, %v), want (%q, %v)", tc.in, slug, ok, tc.wantSlug, tc.wantOK)
			}
		})
	}
}

func TestRepoSlug(t *testing.T) {
	t.Run("remote present", func(t *testing.T) {
		got := RepoSlug("https://github.com/owner/name.git", "/anything")
		if got != "owner/name" {
			t.Errorf("got %q, want owner/name", got)
		}
	})

	t.Run("falls back to directory basename", func(t *testing.T) {
		got := RepoSlug("", "/repo/supergraph")
		if got != "supergraph" {
			t.Errorf("got %q, want supergraph", got)
		}
	})
}

func wtIntp(n int) *int { return &n }

func assertIntPtrEqual(t *testing.T, label string, got, want *int) {
	t.Helper()
	if (got == nil) != (want == nil) {
		t.Fatalf("%s = %v, want %v (nil-ness mismatch)", label, wtPtrStr(got), wtPtrStr(want))
	}
	if got != nil && *got != *want {
		t.Fatalf("%s = %d, want %d", label, *got, *want)
	}
}

func wtPtrStr(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("&%d", *p)
}
