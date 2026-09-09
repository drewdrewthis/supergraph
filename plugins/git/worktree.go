// Package git is the local git read-model plugin: it polls `git worktree list`
// and `git rev-list --left-right --count` per configured repo root, joins each
// worktree to a tmux session by path and to an issue/PR by branch (via
// internal/issuekey), and republishes the result as the git-owned slice of the
// cross-plugin read model. Zero core edits — this plugin exists entirely behind
// the plugins/*/schema glob seam the other read-model plugins (tmux, github)
// already use.
//
// This file holds the pure parsers: turning `git worktree list --porcelain`
// and `git rev-list --left-right --count` stdout into typed rows, with no
// process execution or I/O of their own, so they're exhaustively unit-testable
// against captured command output.
package git

import (
	"path/filepath"
	"strconv"
	"strings"
)

// Worktree is one entry from `git worktree list --porcelain`, joined view
// consumers key on by Path (tmux session cwd) and Branch (issue/PR lookup).
type Worktree struct {
	Path     string
	Branch   string // "" when Detached or Bare
	Head     string // commit sha
	Detached bool
	Bare     bool
	Ahead    *int // nil = no upstream / unknown, distinct from a genuine 0
	Behind   *int
}

// parseWorktreeList parses `git worktree list --porcelain` stdout into rows.
// The format is stanzas separated by a blank line, each stanza starting with
// a `worktree <path>` line; per-stanza keys (`HEAD`, `branch`, `detached`,
// `bare`) follow in any order and unknown keys are ignored, not an error,
// since porcelain is a stable-but-extensible contract we only read a subset
// of.
func parseWorktreeList(porcelain string) []Worktree {
	var out []Worktree
	var cur *Worktree

	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}

	for _, raw := range strings.Split(porcelain, "\n") {
		line := strings.TrimRight(raw, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			flush()
			cur = &Worktree{Path: val}
		case "HEAD":
			if cur != nil {
				cur.Head = val
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "detached":
			if cur != nil {
				cur.Detached = true
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		}
	}
	flush()

	return out
}

// parseAheadBehind parses `git rev-list --left-right --count <branch>...<upstream>`
// stdout ("<ahead>\t<behind>\n"). With the <branch>...<upstream> triple-dot form
// the LEFT count is ahead and the RIGHT is behind. Any malformed input (empty,
// wrong field count, non-numeric) yields (nil, nil) — nil means "no upstream /
// unknown", never 0, since 0 is a genuine "level with upstream" reading a
// caller must be able to distinguish from "we don't know" (AC).
func parseAheadBehind(out string) (ahead, behind *int) {
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return nil, nil
	}
	a, aerr := strconv.Atoi(fields[0])
	b, berr := strconv.Atoi(fields[1])
	if aerr != nil || berr != nil {
		return nil, nil
	}
	return &a, &b
}

// slugFromRemoteURL derives an "owner/name" slug from an origin remote URL,
// covering the scp-like ssh form (git@host:owner/name.git), explicit ssh://,
// and https:// — taking the LAST TWO path segments so a nested group path
// (e.g. GitLab's group/sub/name) still yields a two-segment slug. A local
// path or anything yielding fewer than two segments returns ("", false) so
// the caller can fall back to the directory basename (RepoSlug).
func slugFromRemoteURL(raw string) (string, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", false
	}

	// scp-like form: git@host:owner/name(.git) — no "://" scheme, colon
	// separates host from path.
	if !strings.Contains(s, "://") {
		if _, path, ok := strings.Cut(s, ":"); ok {
			return segmentsToSlug(path)
		}
		return "", false
	}

	// URL form: scheme://[user@]host/path... — "file://" is excluded even
	// when its path happens to have two segments, since a local path is
	// never a hosted owner/repo (caller falls back to the dir basename).
	scheme, rest, _ := strings.Cut(s, "://")
	if strings.EqualFold(scheme, "file") {
		return "", false
	}
	if _, path, ok := strings.Cut(rest, "/"); ok {
		return segmentsToSlug(path)
	}
	return "", false
}

// segmentsToSlug strips a trailing ".git" and trailing slashes from a path,
// then returns its last two "/"-separated segments joined as "owner/name".
func segmentsToSlug(path string) (string, bool) {
	p := strings.TrimSuffix(strings.TrimRight(path, "/"), ".git")
	p = strings.Trim(p, "/")
	if p == "" {
		return "", false
	}
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return "", false
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1], true
}

// RepoSlug returns the "owner/name" slug derived from remoteURL, falling
// back to the repo root's directory basename when the remote yields nothing
// usable (no remote configured, or a local/file path) — exported because
// both the reconcile path and tests need the same fallback behavior.
func RepoSlug(remoteURL, root string) string {
	if slug, ok := slugFromRemoteURL(remoteURL); ok {
		return slug
	}
	return filepath.Base(filepath.Clean(root))
}
