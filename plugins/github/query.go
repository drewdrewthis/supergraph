package github

import (
	"context"
	"encoding/json"
	"strconv"
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
	Labels    struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
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
	}
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
	return n
}

// Issue returns the cached issue at key, or nil on a miss (never an upstream hop —
// D1, AC-GHQ-HIT/MISS). A wrong-kind key returns nil.
func Issue(ctx context.Context, key string) *IssueNode {
	p := current.Load()
	if p == nil || p.store == nil || kindOf(key) != "issue" {
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
	p := current.Load()
	if p == nil || p.store == nil || kindOf(key) != "pr" {
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
	p := current.Load()
	if p == nil || p.store == nil {
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
	p := current.Load()
	if p == nil || p.store == nil {
		return nil
	}
	nodes, _ := p.store.nodesByKind(ctx, "pr", owner+"/"+repo)
	out := make([]PRNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, *mapPR(n.Key, n.JSON))
	}
	return out
}
