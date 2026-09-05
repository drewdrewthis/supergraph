package claude

import (
	"strconv"
	"strings"

	"github.com/drewdrewthis/supergraph/internal/issuekey"
)

// Key grammar (EDR §"Key grammar"):
//
//	session  := "session:" <sessionId> "@" <hostId>
//	instance := "instance:" <pane> "@" <hostId>
//
// sessionId is the Claude Code UUID; hostId is the box. Every hook payload and every
// transcript record folds to its session key; a pane (hook env) adds the instance key.
func sessionKey(sid, host string) string   { return "session:" + sid + "@" + host }
func instanceKey(pane, host string) string { return "instance:" + pane + "@" + host }

// issueFromBranch derives the S4 join key (issue number) from a branch name, or 0
// when the branch encodes none (PRD §6 "issue number ↔ branch/worktree"). The
// derivation is the shared internal/issuekey regex — one source of truth
// across claude, tmux, and github so the cross-plugin join cannot drift.
func issueFromBranch(branch string) int {
	n, _ := issuekey.FromBranch(branch)
	return n
}

// prNumberFromURL pulls the PR number out of a "/pull/N" tail so a pr-link record's
// prUrl alone confirms the number even when the record omits an explicit prNumber.
func prNumberFromURL(url string) int {
	i := strings.LastIndex(url, "/pull/")
	if i < 0 {
		return 0
	}
	tail := url[i+len("/pull/"):]
	if s := strings.IndexAny(tail, "/?#"); s >= 0 {
		tail = tail[:s]
	}
	n, _ := strconv.Atoi(tail)
	return n
}
