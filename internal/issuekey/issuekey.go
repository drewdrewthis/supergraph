// Package issuekey is the single source of truth for the branch↔issue join key
// shared across the claude, tmux, and github plugins (docs/edr/github-query.md D3).
// One regex here keeps the three plugins from drifting: each derives the same
// issue number from the same branch, so a pane and a session on one branch resolve
// to the same issue by construction.
package issuekey

import (
	"regexp"
	"strconv"
	"strings"
)

// branchRe anchors the "issueN" prefix a git branch encodes: an optional dash then
// the digits, terminated by a '/', a '-', or end of string. It matches
// "issue1/spike-core", "issue-12", "issue12-foo" and rejects "fix/issue3",
// "myissue4" (owner 2026-09-05; the "<N>-slug" form is deferred).
var branchRe = regexp.MustCompile(`^issue-?(\d+)([/-]|$)`)

// FromBranch derives the issue number a branch name encodes. ok is false (and n is
// 0) when the branch encodes none.
func FromBranch(branch string) (n int, ok bool) {
	m := branchRe.FindStringSubmatch(strings.TrimSpace(branch))
	if m == nil {
		return 0, false
	}
	n, _ = strconv.Atoi(m[1])
	return n, true
}

// closingRe is GitHub's own issue-closing keyword grammar: a close/fix/resolve verb
// then a "#N" (or a cross-repo "owner/repo#N"). A bare "#N" mention with no verb is
// NOT a link (AC-GHQ-MENTION-NOLINK).
var closingRe = regexp.MustCompile(`(?i)(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+(#|[\w.-]+/[\w.-]+#)(\d+)`)

// ClosingRefs returns the issue numbers text closes via a GitHub closing keyword.
// A cross-repo "owner/repo#N" reference is DROPPED in v1: only a bare "#N" in the
// same repo links (owner 2026-09-05), so the resulting numbers are always local.
func ClosingRefs(text string) []int {
	var out []int
	for _, m := range closingRe.FindAllStringSubmatch(text, -1) {
		if m[3] != "#" { // owner/repo#N cross-repo form — dropped in v1
			continue
		}
		if n, err := strconv.Atoi(m[4]); err == nil {
			out = append(out, n)
		}
	}
	return out
}
