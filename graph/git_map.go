package graph

// The git plugin's cross-plugin join (Worktree.tmuxSession/issue/pullRequest) lives
// here, outside git.resolvers.go, so gqlgen's follow-schema regeneration (which
// preserves only resolver METHOD bodies) never rewrites it. graph/ is the one place
// that imports every plugin, so it calls each plugin's exported accessor directly —
// no plugin imports another. The join key is the shared branch derivation
// (internal/issuekey), same as github_map.go.

import (
	"path/filepath"
	"strings"

	"github.com/drewdrewthis/supergraph/graph/model"
	"github.com/drewdrewthis/supergraph/plugins/github"
	"github.com/drewdrewthis/supergraph/plugins/tmux"
)

// tmuxSessionModel maps a tmux plugin SessionRow to the generated GraphQL model,
// mirroring the inline mapping in graph/tmux.resolvers.go's TmuxSessions.
func tmuxSessionModel(s tmux.SessionRow) model.TmuxSession {
	return model.TmuxSession{
		HostID: s.HostID, Name: s.Name, Worktree: optStr(s.Worktree), Branch: optStr(s.Branch),
		LastSeenAt: s.LastSeenAt, StaleSince: s.StaleSince,
	}
}

// tmuxSessionForWorktree is the Worktree.tmuxSession join (AC-GIT-TMUX-JOIN): the
// tmux session whose Worktree path matches path after filepath.Clean. Symlink
// resolution is deliberately out of scope — paths are compared post-Clean only, so
// a session on a symlinked path that differs textually from path will not join.
// That is a documented limitation, not a silent one.
//
// tmux.Sessions ordering is not guaranteed, so when more than one session sits on
// the same worktree path the match is tie-broken by the lexicographically smallest
// Name, keeping the result deterministic across calls.
func tmuxSessionForWorktree(sessions []tmux.SessionRow, path string) *model.TmuxSession {
	want := filepath.Clean(path)
	var best *tmux.SessionRow
	for i := range sessions {
		s := &sessions[i]
		if filepath.Clean(s.Worktree) != want {
			continue
		}
		if best == nil || s.Name < best.Name {
			best = s
		}
	}
	if best == nil {
		return nil
	}
	m := tmuxSessionModel(*best)
	return &m
}

// splitRepoSlug splits a WorktreeNode.RepoSlug ("owner/name") into its two parts.
// ok is false when the slug does not split into exactly two non-empty segments.
func splitRepoSlug(slug string) (owner, repo string, ok bool) {
	parts := strings.Split(slug, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// pullRequestForWorktree is the Worktree.pullRequest join (AC-GIT-PR-JOIN). direct
// is the issue-key-derived cache lookup (cheap, exact when it hits and its head ref
// matches branch — PR #N and issue #N are different numbers in the same repo, so a
// hit on the wrong node must be rejected). cached is the fallback scan for the
// exact head branch when direct misses. Neither path filters by state — a closed or
// merged PR on the branch is still returned, since the sidebar renders PR state.
func pullRequestForWorktree(direct *github.PRNode, cached []github.PRNode, branch string) *github.PRNode {
	if direct != nil && direct.HeadRefName == branch {
		return direct
	}
	var best *github.PRNode
	for i := range cached {
		pr := &cached[i]
		if pr.HeadRefName != branch {
			continue
		}
		if best == nil || pr.Number < best.Number {
			best = pr
		}
	}
	return best
}
