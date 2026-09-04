package github

import (
	"strings"
)

// Cache key grammar (EDR §"Cache key = object id"):
//
//	key   := <kind>":"<scope>[ "#"<number> | "/"<id> ] [ "@"<hostId> ]
//	scope := <owner>"/"<repo>   (except user/org kinds)
//
// Every kind is ONE ROW in kindSpecs below — its key format, REST path, GraphQL
// typename, and the webhook events that address it. Adding a kind is a new row, not
// new code: parseKey/restPath/typenameFor/eventKey all read the table.
type kindSpec struct {
	kind     string   // leading token, e.g. "issue"
	typename string   // GraphQL typename stamped on stored nodes
	keyfmt   string   // key template, e.g. "issue:{owner}/{repo}#{disc}"
	rest     string   // REST GET path template, e.g. "/repos/{owner}/{repo}/issues/{disc}"
	events   []string // webhook X-GitHub-Event names that map to this kind ("" = none)
	obj      string   // webhook payload object carrying the discriminator
	field    string   // field in obj giving the discriminator (number/id/name/tag_name)
}

// kindSpecs is the single source of truth for the key grammar and the webhook
// event→key map. {disc} is the primary discriminator (issue/pr number, check-run
// id, label name, ...); {subid} the second (review/comment id); {sub} a slash-
// bearing tail (a ref name); {login} the user kind's sole segment.
var kindSpecs = []kindSpec{
	{kind: "issue", typename: "Issue", keyfmt: "issue:{owner}/{repo}#{disc}", rest: "/repos/{owner}/{repo}/issues/{disc}", events: []string{"issues", "issue_comment"}, obj: "issue", field: "number"},
	{kind: "pr", typename: "PullRequest", keyfmt: "pr:{owner}/{repo}#{disc}", rest: "/repos/{owner}/{repo}/pulls/{disc}", events: []string{"pull_request", "pull_request_review", "pull_request_review_comment"}, obj: "pull_request", field: "number"},
	{kind: "checkRun", typename: "CheckRun", keyfmt: "checkRun:{owner}/{repo}/{disc}", rest: "/repos/{owner}/{repo}/check-runs/{disc}", events: []string{"check_run"}, obj: "check_run", field: "id"},
	{kind: "review", typename: "PullRequestReview", keyfmt: "review:{owner}/{repo}#{disc}/{subid}", rest: "/repos/{owner}/{repo}/pulls/{disc}/reviews/{subid}"},
	{kind: "comment", typename: "IssueComment", keyfmt: "comment:{owner}/{repo}#{disc}/{subid}", rest: "/repos/{owner}/{repo}/issues/comments/{subid}"},
	{kind: "label", typename: "Label", keyfmt: "label:{owner}/{repo}/{disc}", rest: "/repos/{owner}/{repo}/labels/{disc}", events: []string{"label"}, obj: "label", field: "name"},
	{kind: "release", typename: "Release", keyfmt: "release:{owner}/{repo}/{disc}", rest: "/repos/{owner}/{repo}/releases/tags/{disc}", events: []string{"release"}, obj: "release", field: "tag_name"},
	{kind: "commit", typename: "Commit", keyfmt: "commit:{owner}/{repo}/{disc}", rest: "/repos/{owner}/{repo}/commits/{disc}"},
	{kind: "ref", typename: "Ref", keyfmt: "ref:{owner}/{repo}/{sub}", rest: "/repos/{owner}/{repo}/git/refs/{sub}"},
	{kind: "repo", typename: "Repository", keyfmt: "repo:{owner}/{repo}", rest: "/repos/{owner}/{repo}"},
	{kind: "user", typename: "User", keyfmt: "user:{login}", rest: "/users/{login}"},
}

var (
	specByKind  = map[string]kindSpec{}
	specByEvent = map[string]kindSpec{}
)

func init() {
	for _, s := range kindSpecs {
		specByKind[s.kind] = s
		for _, e := range s.events {
			specByEvent[e] = s
		}
	}
}

// kindOf returns the leading kind token ("issue", "pr", ...).
func kindOf(key string) string {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[:i]
	}
	return ""
}

// stripHost drops an optional "@hostId" suffix, leaving the host-agnostic body.
func stripHost(key string) string {
	if at := strings.LastIndexByte(key, '@'); at >= 0 {
		return key[:at]
	}
	return key
}

// parseKey decomposes a key into the named parts the templates reference
// (owner, repo, disc, subid, sub, login), driven only by the grammar's separators.
func parseKey(key string) map[string]string {
	kind := kindOf(key)
	rest := stripHost(key)
	if i := strings.IndexByte(rest, ':'); i >= 0 {
		rest = rest[i+1:]
	}
	p := map[string]string{}
	if kind == "user" {
		p["login"] = rest
		return p
	}
	owner, tail, _ := strings.Cut(rest, "/")
	p["owner"] = owner
	ri := strings.IndexAny(tail, "#/")
	if ri < 0 {
		p["repo"] = tail
		return p
	}
	p["repo"] = tail[:ri]
	rem := tail[ri:]
	if rem[0] == '#' {
		rem = rem[1:]
		if si := strings.IndexByte(rem, '/'); si >= 0 {
			p["disc"], p["subid"] = rem[:si], rem[si+1:]
		} else {
			p["disc"] = rem
		}
		return p
	}
	rem = rem[1:] // leading '/'
	p["sub"] = rem
	if si := strings.IndexByte(rem, '/'); si >= 0 {
		p["disc"] = rem[:si]
	} else {
		p["disc"] = rem
	}
	return p
}

// fill substitutes {token} placeholders present in parts, leaving others intact.
func fill(template string, parts map[string]string) string {
	return placeholderRe.ReplaceAllStringFunc(template, func(m string) string {
		if v, ok := parts[m[1:len(m)-1]]; ok {
			return v
		}
		return m
	})
}

// restPath maps a canonical key to its REST GET path via the table.
func restPath(key string) string { return fill(specByKind[kindOf(key)].rest, parseKey(key)) }

// typenameFor maps a key kind to its GraphQL typename via the table.
func typenameFor(key string) string {
	if s, ok := specByKind[kindOf(key)]; ok {
		return s.typename
	}
	return "Node"
}

// scopePrefix returns the "repo:<owner>/<repo>" prefix a key lives under, or "" for
// user/org kinds that have no repo scope.
func scopePrefix(key string) string {
	p := parseKey(key)
	if p["owner"] == "" || p["repo"] == "" {
		return ""
	}
	return "repo:" + p["owner"] + "/" + p["repo"]
}

// composite is the tag a LIST result is stored under: its scope prefix joined with
// the node kind it lists. A purge of any contained node keys on this exact string,
// so evicting one member evicts the list — without over-purging a sibling kind or a
// sibling repo. Single nodes never carry it, so a purge of #5 leaves #6 cached.
func composite(key string) string {
	sp := scopePrefix(key)
	if sp == "" {
		return ""
	}
	return sp + "|" + kindOf(key)
}

// listKey names a stored list result for op over scope prefix (e.g.
// "list:openIssues:repo:o/r"). It is distinct from any object key.
func listKey(op, scope string) string { return "list:" + op + ":" + scope }

// eventKey maps a webhook X-GitHub-Event + payload to the object key it touches,
// via the table. Every mapped event is an invalidation: the key is purged, forcing
// the next read to refetch (event-invalidated proxy).
func eventKey(event string, payload map[string]any) (string, bool) {
	s, ok := specByEvent[event]
	if !ok {
		return "", false
	}
	owner, repo := repoFullName(payload)
	if owner == "" || repo == "" {
		return "", false
	}
	disc, ok := discField(payload, s.obj, s.field)
	if !ok {
		return "", false
	}
	return fill(s.keyfmt, map[string]string{"owner": owner, "repo": repo, "disc": disc}), true
}

// repoFullName pulls owner/repo from the standard webhook "repository" block.
func repoFullName(payload map[string]any) (string, string) {
	repo, ok := payload["repository"].(map[string]any)
	if !ok {
		return "", ""
	}
	if full, ok := repo["full_name"].(string); ok {
		if o, r, found := strings.Cut(full, "/"); found {
			return o, r
		}
	}
	name, _ := repo["name"].(string)
	if owner, ok := repo["owner"].(map[string]any); ok {
		if login, ok := owner["login"].(string); ok {
			return login, name
		}
	}
	return "", name
}

// discField reads payload[obj][field] as a string discriminator (JSON numbers
// coerced to their integer form).
func discField(payload map[string]any, obj, field string) (string, bool) {
	m, ok := payload[obj].(map[string]any)
	if !ok {
		return "", false
	}
	v, ok := m[field]
	if !ok || v == nil {
		return "", false
	}
	s := coerce(v)
	return s, s != ""
}
