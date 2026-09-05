package graph

// The github plugin's cross-plugin join (docs/edr/github-query.md D2/D3) lives here,
// outside github.resolvers.go, so gqlgen's follow-schema regeneration (which
// preserves only resolver METHOD bodies) never rewrites it. graph/ is the one place
// that imports every plugin, so it calls each plugin's exported accessor directly —
// no plugin imports another. The join key is ONE shared derivation
// (internal/issuekey) plus the cached PR closing-keyword links.

import (
	"context"

	"github.com/drewdrewthis/supergraph/graph/model"
	"github.com/drewdrewthis/supergraph/internal/issuekey"
	"github.com/drewdrewthis/supergraph/plugins/claude"
	"github.com/drewdrewthis/supergraph/plugins/github"
	"github.com/drewdrewthis/supergraph/plugins/tmux"
)

// issueBranches is the set of git branches that link to issue N in a repo — the one
// join key set both tmuxPanes and claudeSessions read, so the two sides can only
// agree because they share issuekey.FromBranch (AC-GHQ-JOIN-CONSISTENT):
//   - any tmux session branch that derives to N (the bare issueN branch path),
//   - any claude session branch keyed to N by the same derivation,
//   - the head branch of any cached PR whose head derives to N OR whose body/title
//     closes #N via a GitHub closing keyword (AC-GHQ-PR-CLOSES; a bare #N mention
//     does NOT link, AC-GHQ-MENTION-NOLINK).
func issueBranches(ctx context.Context, owner, repo string, n int) map[string]struct{} {
	b := map[string]struct{}{}
	if sess, err := tmux.Sessions(ctx, ""); err == nil {
		for _, s := range sess {
			if k, ok := issuekey.FromBranch(s.Branch); ok && k == n {
				b[s.Branch] = struct{}{}
			}
		}
	}
	for _, cs := range claude.QuerySessions(ctx, nil, &n) {
		if cs.GitBranch != "" {
			b[cs.GitBranch] = struct{}{}
		}
	}
	for _, pr := range github.CachedPRs(ctx, owner, repo) {
		if pr.HeadRefName == "" {
			continue
		}
		if k, ok := issuekey.FromBranch(pr.HeadRefName); ok && k == n {
			b[pr.HeadRefName] = struct{}{}
			continue
		}
		if containsInt(issuekey.ClosingRefs(pr.Body+"\n"+pr.Title), n) {
			b[pr.HeadRefName] = struct{}{}
		}
	}
	return b
}

// tmuxPanesForIssue returns every tmux pane on a branch linked to issue N, deduped
// by pane key. An empty result renders as [] (never null — AC-GHQ-JOIN-EMPTY).
func tmuxPanesForIssue(ctx context.Context, owner, repo string, n int) []model.TmuxPane {
	seen := map[string]bool{}
	var rows []tmux.PaneRow
	for br := range issueBranches(ctx, owner, repo, n) {
		prs, err := tmux.PaneForBranch(ctx, br)
		if err != nil {
			continue
		}
		for _, p := range prs {
			if seen[p.Key] {
				continue
			}
			seen[p.Key] = true
			rows = append(rows, p)
		}
	}
	return panesToModel(rows)
}

// claudeSessionsForIssue returns every claude session keyed to issue N or on a
// branch linked to N (the PR-closes path attaches a session whose own branch
// derives to no issue — AC-GHQ-PR-CLOSES). Deduped by session id, [] when none.
func claudeSessionsForIssue(ctx context.Context, owner, repo string, n int) []model.ClaudeSession {
	branches := issueBranches(ctx, owner, repo, n)
	seen := map[string]bool{}
	out := make([]model.ClaudeSession, 0)
	for _, cs := range claude.QuerySessions(ctx, nil, nil) {
		_, inBranch := branches[cs.GitBranch]
		if cs.IssueNumber != n && (cs.GitBranch == "" || !inBranch) {
			continue
		}
		if seen[cs.SessionID] {
			continue
		}
		seen[cs.SessionID] = true
		out = append(out, claudeSessionModel(cs))
	}
	return out
}

// panesForBranchModel is the PullRequest.tmuxPanes join: panes on the exact head
// branch (D3). [] when none.
func panesForBranchModel(ctx context.Context, branch string) []model.TmuxPane {
	rows, err := tmux.PaneForBranch(ctx, branch)
	if err != nil {
		return panesToModel(nil)
	}
	return panesToModel(rows)
}

// claudeSessionsOnBranchModel is the PullRequest.claudeSessions join: sessions
// keyed to the head branch's derived issue, plus any session on the exact branch
// (so a PR whose head derives to no issue still attaches its sessions). [] when none.
func claudeSessionsOnBranchModel(ctx context.Context, branch string) []model.ClaudeSession {
	seen := map[string]bool{}
	out := make([]model.ClaudeSession, 0)
	add := func(rows []claude.SessionRow) {
		for _, cs := range rows {
			if seen[cs.SessionID] {
				continue
			}
			seen[cs.SessionID] = true
			out = append(out, claudeSessionModel(cs))
		}
	}
	if n, ok := issuekey.FromBranch(branch); ok {
		add(claude.QuerySessions(ctx, nil, &n))
	}
	var onBranch []claude.SessionRow
	for _, cs := range claude.QuerySessions(ctx, nil, nil) {
		if cs.GitBranch == branch {
			onBranch = append(onBranch, cs)
		}
	}
	add(onBranch)
	return out
}

func containsInt(xs []int, n int) bool {
	for _, x := range xs {
		if x == n {
			return true
		}
	}
	return false
}
