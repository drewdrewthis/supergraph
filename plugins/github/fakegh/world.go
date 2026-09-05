package fakegh

import (
	"sort"
	"strconv"
	"time"
)

// SetNode inserts or replaces a node under key, bumping its ETag. typename is the
// GraphQL __typename the node reports; body is the JSON served on a REST fetch.
// It returns the stored node's current ETag.
func (s *Server) SetNode(key, typename string, body map[string]any) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setNodeLocked(key, typename, body)
}

func (s *Server) setNodeLocked(key, typename string, body map[string]any) string {
	n, ok := s.world.nodes[key]
	if !ok {
		n = &Node{Key: key, Typename: typename, version: 1, Body: body}
		s.world.nodes[key] = n
	} else {
		n.version++
		n.Typename = typename
		n.Body = body
	}
	return n.etag()
}

// Mutate applies fn to the body of an existing node, bumping its ETag. It returns
// the new ETag and whether the node existed.
func (s *Server) Mutate(key string, fn func(body map[string]any)) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.world.nodes[key]
	if !ok {
		return "", false
	}
	fn(n.Body)
	n.version++
	return n.etag(), true
}

// Node returns a copy-free view of a stored node and whether it exists. Callers
// must not mutate the returned body outside Mutate.
func (s *Server) Node(key string) (*Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.world.nodes[key]
	return n, ok
}

// ETag returns the current strong ETag for key, or "" if absent.
func (s *Server) ETag(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.world.nodes[key]; ok {
		return n.etag()
	}
	return ""
}

// AddRepo registers a repo for /user/repos pagination and stores its repo: node.
func (s *Server) AddRepo(owner, repo string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	full := owner + "/" + repo
	for _, r := range s.world.repos {
		if r == full {
			return
		}
	}
	s.world.repos = append(s.world.repos, full)
	sort.Strings(s.world.repos)
	s.setNodeLocked("repo:"+full, "Repository", map[string]any{
		"id":        "R_" + full,
		"name":      repo,
		"full_name": full,
		"owner":     map[string]any{"login": owner},
	})
}

// AddIssue stores an issue node keyed issue:owner/repo#number.
func (s *Server) AddIssue(owner, repo string, number int, title, state string) string {
	key := "issue:" + owner + "/" + repo + "#" + strconv.Itoa(number)
	return s.SetNode(key, "Issue", map[string]any{
		"number": number,
		"title":  title,
		"state":  state,
		"id":     key,
	})
}

// AddPR stores a pull-request node keyed pr:owner/repo#number. mergedAt is encoded
// so the plugin's pin-grace logic can classify recently-merged PRs.
func (s *Server) AddPR(owner, repo string, number int, title, state string, mergedAt time.Time) string {
	key := "pr:" + owner + "/" + repo + "#" + strconv.Itoa(number)
	body := map[string]any{
		"number": number,
		"title":  title,
		"state":  state,
		"id":     key,
		"merged": !mergedAt.IsZero(),
	}
	if !mergedAt.IsZero() {
		body["merged_at"] = mergedAt.UTC().Format(time.RFC3339)
	}
	return s.SetNode(key, "PullRequest", body)
}
