package claude

import (
	"regexp"
	"strconv"
	"strings"
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

// branchIssueRe extracts the issue number a git branch encodes: an "issueN/..." (or
// "issue-N/...") prefix. A branch with no such prefix (e.g. "plugin/claude") yields 0.
var branchIssueRe = regexp.MustCompile(`^issue-?(\d+)\b`)

// issueFromBranch derives the S4 join key (issue number) from a branch name, or 0
// when the branch encodes none (PRD §6 "issue number ↔ branch/worktree").
func issueFromBranch(branch string) int {
	m := branchIssueRe.FindStringSubmatch(strings.TrimSpace(branch))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
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
