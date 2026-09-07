package fakegh

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// serveNode is the read-through core: it serves the node at key with a strong
// ETag, answers a matching If-None-Match with 304 (no quota decrement), and spends
// one REST unit only on a 200 body. Missing keys are 404 (one unit).
func (s *Server) serveNode(w http.ResponseWriter, r *http.Request, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.world.nodes[key]
	if !ok {
		s.decREST()
		s.setRESTHeaders(w)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	etag := n.etag()
	w.Header().Set("ETag", etag)
	if match := r.Header.Get("If-None-Match"); match != "" && match == etag {
		s.setRESTHeaders(w) // conditional hit: zero quota
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.decREST()
	s.setRESTHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(restView(key, n.Body))
}

// restView projects a stored (GraphQL-shaped) issue/pull body into the flat REST
// JSON real GitHub serves: labels/assignees become flat arrays and updatedAt/url
// become updated_at/html_url. This deliberate divergence from the GraphQL shape is
// what lets a test prove the reconcile revalidate re-reads the canonical GraphQL
// body rather than clobbering the store with this REST body. Non-issue/pr keys and
// bodies without the GraphQL connection fields pass through unchanged.
func restView(key string, body map[string]any) map[string]any {
	if !strings.HasPrefix(key, "issue:") && !strings.HasPrefix(key, "pr:") {
		return body
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		switch k {
		case "labels", "assignees":
			out[k] = flattenNodes(v)
		case "updatedAt":
			out["updated_at"] = v
		case "url":
			out["html_url"] = v
		default:
			out[k] = v
		}
	}
	return out
}

// flattenNodes turns a GraphQL connection {nodes:[...]} into the flat array REST
// returns; a value that is not a connection passes through unchanged.
func flattenNodes(v any) any {
	conn, ok := v.(map[string]any)
	if !ok {
		return v
	}
	nodes, ok := conn["nodes"].([]any)
	if !ok {
		return v
	}
	return nodes
}

func (s *Server) handleRepo(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "repo:"+r.PathValue("owner")+"/"+r.PathValue("repo"))
}

func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "issue:"+scope(r)+"#"+r.PathValue("number"))
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "pr:"+scope(r)+"#"+r.PathValue("number"))
}

func (s *Server) handleReview(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "review:"+scope(r)+"#"+r.PathValue("number")+"/"+r.PathValue("id"))
}

func (s *Server) handleComment(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "comment:"+scope(r)+"/"+r.PathValue("id"))
}

func (s *Server) handleCheckRun(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "checkRun:"+scope(r)+"/"+r.PathValue("id"))
}

func (s *Server) handleLabel(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "label:"+scope(r)+"/"+r.PathValue("name"))
}

func (s *Server) handleRelease(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "release:"+scope(r)+"/"+r.PathValue("tag"))
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "commit:"+scope(r)+"/"+r.PathValue("sha"))
}

func (s *Server) handleRef(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "ref:"+scope(r)+"/"+r.PathValue("ref"))
}

func (s *Server) handleUser(w http.ResponseWriter, r *http.Request) {
	s.serveNode(w, r, "user:"+r.PathValue("login"))
}

func scope(r *http.Request) string { return r.PathValue("owner") + "/" + r.PathValue("repo") }

// handleUserRepos serves the affiliation=owner repo list, paginated with RFC 5988
// Link headers (rel first/prev/next/last).
func (s *Server) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	perPage := clampInt(atoiDefault(r.URL.Query().Get("per_page"), 30), 1, 100)
	page := maxInt(atoiDefault(r.URL.Query().Get("page"), 1), 1)
	total := len(s.world.repos)
	last := int(math.Max(1, math.Ceil(float64(total)/float64(perPage))))

	start := (page - 1) * perPage
	end := minInt(start+perPage, total)
	out := []map[string]any{}
	if start < total {
		for _, full := range s.world.repos[start:end] {
			owner, repo, _ := strings.Cut(full, "/")
			out = append(out, map[string]any{
				"id":        "R_" + full,
				"name":      repo,
				"full_name": full,
				"owner":     map[string]any{"login": owner},
			})
		}
	}

	if link := buildLink(r, page, last, perPage); link != "" {
		w.Header().Set("Link", link)
	}
	s.decREST()
	s.setRESTHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func buildLink(r *http.Request, page, last, perPage int) string {
	base := func(p int) string {
		return fmt.Sprintf(`<%s://%s%s?affiliation=owner&per_page=%d&page=%d>`, scheme(r), r.Host, r.URL.Path, perPage, p)
	}
	var parts []string
	if page < last {
		parts = append(parts, base(page+1)+`; rel="next"`)
		parts = append(parts, base(last)+`; rel="last"`)
	}
	if page > 1 {
		parts = append(parts, base(page-1)+`; rel="prev"`)
		parts = append(parts, base(1)+`; rel="first"`)
	}
	return strings.Join(parts, ", ")
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// handleNotifications answers If-Modified-Since with 304 (+X-Poll-Interval, zero
// quota) when nothing changed, otherwise 200 with the thread list.
func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("X-Poll-Interval", strconv.Itoa(s.pollInterval))
	ims := r.Header.Get("If-Modified-Since")
	if ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !s.notifLastModified.After(t) {
			s.setRESTHeaders(w) // conditional hit: headers, zero quota
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	if !s.notifLastModified.IsZero() {
		w.Header().Set("Last-Modified", s.notifLastModified.UTC().Format(http.TimeFormat))
	}
	s.decREST()
	s.setRESTHeaders(w)
	w.Header().Set("Content-Type", "application/json")
	threads := s.notifThreads
	if threads == nil {
		threads = []map[string]any{}
	}
	_ = json.NewEncoder(w).Encode(threads)
}

// SetNotifications replaces the /notifications thread list and advances its
// Last-Modified to now, so the next conditional poll sees a change.
func (s *Server) SetNotifications(threads []map[string]any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notifThreads = threads
	s.notifLastModified = time.Now().UTC().Truncate(time.Second)
}

func atoiDefault(s string, def int) int {
	if v, err := strconv.Atoi(s); err == nil {
		return v
	}
	return def
}

func clampInt(v, lo, hi int) int { return maxInt(lo, minInt(v, hi)) }
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
