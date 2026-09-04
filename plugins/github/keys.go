package github

import (
	"fmt"
	"strings"
)

// Cache key grammar (EDR §"Cache key = object id"):
//
//	key   := <kind>":"<scope>[ "#"<number> | "/"<id> ] [ "@"<hostId> ]
//	scope := <owner>"/"<repo>   (except user/org kinds)
//
// Every key is one canonical string. keys.go parses it into its parts, derives
// the tags a purge keys on, and maps a webhook event to the key it touches.

// kindOf returns the leading kind token ("issue", "pr", "checkRun", ...).
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

// scopePrefix returns the "repo:<owner>/<repo>" prefix a key lives under, or "" for
// user/org kinds that have no repo scope. It is the coarse half of a list result's
// covering tag.
func scopePrefix(key string) string {
	body := stripHost(key)
	i := strings.IndexByte(body, ':')
	if i < 0 {
		return ""
	}
	rest := body[i+1:]
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 {
		return "" // user:<login> — no repo scope
	}
	repo := parts[1]
	if h := strings.IndexByte(repo, '#'); h >= 0 {
		repo = repo[:h] // owner/repo#number → strip the number
	}
	return "repo:" + parts[0] + "/" + repo
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

// purgeTags are the tags a purge of key deletes on: the exact key (the node itself)
// and its composite (every list result whose scope covers it).
func purgeTags(key string) []string {
	tags := []string{key}
	if c := composite(key); c != "" {
		tags = append(tags, c)
	}
	return tags
}

// listKey names a stored list result for op over scope prefix (e.g.
// "list:openIssues:repo:o/r"). It is distinct from any object key.
func listKey(op, scope string) string { return "list:" + op + ":" + scope }

// eventKey maps a webhook X-GitHub-Event + action + payload to the object key it
// touches. Returns ok=false when the event carries no addressable object. Every
// mapped event is treated as an invalidation: the key is purged, forcing the next
// read to refetch (event-invalidated proxy).
func eventKey(event string, payload map[string]any) (string, bool) {
	owner, repo := repoFullName(payload)
	if owner == "" || repo == "" {
		return "", false
	}
	scope := owner + "/" + repo
	switch event {
	case "issues", "issue_comment":
		if n, ok := numberField(payload, "issue"); ok {
			return fmt.Sprintf("issue:%s#%d", scope, n), true
		}
	case "pull_request", "pull_request_review", "pull_request_review_comment":
		if n, ok := numberField(payload, "pull_request"); ok {
			return fmt.Sprintf("pr:%s#%d", scope, n), true
		}
	case "check_run":
		if id, ok := idField(payload, "check_run"); ok {
			return fmt.Sprintf("checkRun:%s/%s", scope, id), true
		}
	case "label":
		if lbl, ok := payload["label"].(map[string]any); ok {
			if name, ok := lbl["name"].(string); ok {
				return fmt.Sprintf("label:%s/%s", scope, name), true
			}
		}
	case "release":
		if rel, ok := payload["release"].(map[string]any); ok {
			if tag, ok := rel["tag_name"].(string); ok {
				return fmt.Sprintf("release:%s/%s", scope, tag), true
			}
		}
	}
	return "", false
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

// numberField reads payload[obj]["number"] as an int (GitHub numbers arrive as JSON
// floats).
func numberField(payload map[string]any, obj string) (int, bool) {
	m, ok := payload[obj].(map[string]any)
	if !ok {
		return 0, false
	}
	switch n := m["number"].(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	}
	return 0, false
}

// idField reads payload[obj]["id"] as a string (numbers coerced).
func idField(payload map[string]any, obj string) (string, bool) {
	m, ok := payload[obj].(map[string]any)
	if !ok {
		return "", false
	}
	switch id := m["id"].(type) {
	case float64:
		return fmt.Sprintf("%d", int64(id)), true
	case string:
		return id, id != ""
	}
	return "", false
}
