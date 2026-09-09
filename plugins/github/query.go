package github

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"time"
)

// IssueNode is the typed projection of a cached issue node (docs/edr/github-query.md
// D4): gqlgen binds GraphQL type Issue onto it, so its scalar fields need no
// resolver. Owner/Repo are join context the graph/ resolvers read, not GraphQL
// fields. The tmuxPanes/claudeSessions join fields are deliberately ABSENT from the
// struct so gqlgen generates field-resolver stubs, implemented in graph/github_map.go.
type IssueNode struct {
	Owner, Repo string
	Number      int
	Title       string
	State       string
	URL         string
	UpdatedAt   *time.Time
	Labels      []string
	Assignees   []Assignee
	memo        *issueMemo
}

// Assignee is one login assigned to an issue (GraphQL type Assignee binds onto it,
// so its scalar login field needs no resolver). Empty slice = unassigned.
type Assignee struct {
	Login string
}

// issueMemo caches this issue's cross-plugin branch set so the two sibling field
// resolvers (Issue.tmuxPanes / Issue.claudeSessions) compute the join once, not
// twice (docs/edr/github-query.md D3). It hangs off the node because that is the one
// per-request value both resolvers share; sync.Once makes gqlgen's concurrent field
// resolution race-free. IssueNode holds a *pointer* to it (not the struct) so a
// value copy of IssueNode in IssuesForRepo cannot trip go vet's copylocks.
type issueMemo struct {
	once sync.Once
	set  map[string]struct{}
}

// Branches returns this issue's linked-branch set, running compute (the graph/ join)
// at most once. Safe under concurrent sibling-field resolution.
func (n *IssueNode) Branches(compute func() map[string]struct{}) map[string]struct{} {
	n.memo.once.Do(func() { n.memo.set = compute() })
	return n.memo.set
}

// PRNode is the typed projection of a cached pull-request node. HeadRefName is the
// branch the cross-plugin join keys on; Body/Title feed the closing-keyword scan
// (Body is cached for that scan, not exposed as a GraphQL field).
type PRNode struct {
	Owner, Repo string
	Number      int
	Title       string
	State       string
	URL         string
	UpdatedAt   *time.Time
	Labels      []string
	HeadRefName string
	BaseRefName *string
	Body        string
	// Sidebar-parity fields (#26). Draft is non-null; the three pointers are nil
	// when upstream is empty so "no review/rollup yet" is GraphQL null, not "".
	Draft             bool
	ReviewDecision    *string
	StatusCheckRollup *string
	MergeStateStatus  *string
}

// nodeJSON is the subset of an octokit/GraphQL node body the typed reads map from.
type nodeJSON struct {
	Title     string `json:"title"`
	State     string `json:"state"`
	URL       string `json:"url"`
	UpdatedAt string `json:"updatedAt"`
	Body      string `json:"body"`
	HeadRef   string `json:"headRefName"`
	BaseRef   string `json:"baseRefName"`
	IsDraft   bool   `json:"isDraft"`
	ReviewDec string `json:"reviewDecision"`
	MergeSt   string `json:"mergeStateStatus"`
	// Commits mirrors commits(last:1){nodes{commit{statusCheckRollup{state}}}}: the
	// rollup STATE is read verbatim, never derived from the check-run list (#26).
	Commits struct {
		Nodes []struct {
			Commit struct {
				StatusCheckRollup *struct {
					State string `json:"state"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
}

// keyParts pulls owner/repo/number off a cache key (the key is authoritative for
// identity; the JSON body carries the mutable fields).
func keyParts(key string) (owner, repo string, number int) {
	p := parseKey(key)
	number, _ = strconv.Atoi(p["disc"])
	return p["owner"], p["repo"], number
}

func labelsOf(j nodeJSON) []string {
	out := make([]string, 0, len(j.Labels.Nodes))
	for _, l := range j.Labels.Nodes {
		out = append(out, l.Name)
	}
	return out
}

func assigneesOf(j nodeJSON) []Assignee {
	out := make([]Assignee, 0, len(j.Assignees.Nodes))
	for _, a := range j.Assignees.Nodes {
		out = append(out, Assignee{Login: a.Login})
	}
	return out
}

func parseUpdated(s string) *time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &t
	}
	return nil
}

// mapIssue builds an IssueNode from a cache key plus its stored JSON body.
func mapIssue(key string, raw []byte) *IssueNode {
	var j nodeJSON
	_ = json.Unmarshal(raw, &j)
	owner, repo, number := keyParts(key)
	return &IssueNode{
		Owner: owner, Repo: repo, Number: number,
		Title: j.Title, State: j.State, URL: j.URL,
		UpdatedAt: parseUpdated(j.UpdatedAt), Labels: labelsOf(j),
		Assignees: assigneesOf(j),
		memo:      &issueMemo{},
	}
}

// safeKey rejects a key whose owner/repo segments are not allowlist-safe, so a
// crafted key cannot drive a cache read with an unvalidated scope (S1). Repo-less
// kinds pass on their empty repo.
func safeKey(key string) bool {
	p := parseKey(key)
	return safeName(p["owner"]) && safeName(p["repo"])
}

// mapPR builds a PRNode from a cache key plus its stored JSON body.
func mapPR(key string, raw []byte) *PRNode {
	var j nodeJSON
	_ = json.Unmarshal(raw, &j)
	owner, repo, number := keyParts(key)
	n := &PRNode{
		Owner: owner, Repo: repo, Number: number,
		Title: j.Title, State: j.State, URL: j.URL,
		UpdatedAt: parseUpdated(j.UpdatedAt), Labels: labelsOf(j),
		HeadRefName: j.HeadRef, Body: j.Body,
	}
	if j.BaseRef != "" {
		n.BaseRefName = &j.BaseRef
	}
	n.Draft = j.IsDraft
	n.ReviewDecision = ptrIf(j.ReviewDec)
	n.MergeStateStatus = ptrIf(j.MergeSt)
	if len(j.Commits.Nodes) > 0 {
		if r := j.Commits.Nodes[0].Commit.StatusCheckRollup; r != nil {
			n.StatusCheckRollup = ptrIf(r.State)
		}
	}
	return n
}

// ptrIf returns &s, or nil when s is empty — so an absent upstream string (no
// review decision, no rollup) projects to GraphQL null rather than "" (#26).
func ptrIf(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Issue returns the cached issue at key, or nil on a miss (never an upstream hop —
// D1, AC-GHQ-HIT/MISS). A wrong-kind key returns nil.
func Issue(ctx context.Context, key string) *IssueNode {
	p := current.Get()
	if p == nil || p.store == nil || kindOf(key) != "issue" || !safeKey(key) {
		return nil
	}
	n, _ := p.store.get(ctx, key)
	if n == nil {
		return nil
	}
	return mapIssue(key, n.JSON)
}

// PullRequest returns the cached pull request at key, or nil on a miss.
func PullRequest(ctx context.Context, key string) *PRNode {
	p := current.Get()
	if p == nil || p.store == nil || kindOf(key) != "pr" || !safeKey(key) {
		return nil
	}
	n, _ := p.store.get(ctx, key)
	if n == nil {
		return nil
	}
	return mapPR(key, n.JSON)
}

// IssuesForRepo returns the cached issue nodes under a repo scope, or an empty
// slice for a cold repo (D1, AC-GHQ-LIST). It never hops upstream.
func IssuesForRepo(ctx context.Context, owner, repo string) []IssueNode {
	p := current.Get()
	if p == nil || p.store == nil || !safeName(owner) || !safeName(repo) {
		return nil
	}
	nodes, _ := p.store.nodesByKind(ctx, "issue", owner+"/"+repo)
	out := make([]IssueNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *mapIssue(n.Key, n.JSON))
	}
	return out
}

// CachedPRs returns the cached pull-request nodes under a repo scope, for the
// join's closing-keyword / head-branch link scan (D3). Cache-only, never upstream.
func CachedPRs(ctx context.Context, owner, repo string) []PRNode {
	p := current.Get()
	if p == nil || p.store == nil || !safeName(owner) || !safeName(repo) {
		return nil
	}
	nodes, _ := p.store.nodesByKind(ctx, "pr", owner+"/"+repo)
	out := make([]PRNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *mapPR(n.Key, n.JSON))
	}
	return out
}
