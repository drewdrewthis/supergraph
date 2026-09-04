// Package fakegh is a test-only in-memory fake of the GitHub REST + GraphQL API
// (plus webhook delivery/redelivery) backed by httptest. It exists so the github
// plugin — a lazy, event-invalidated caching proxy — can be exercised end to end
// with no network: a programmable world of repos/issues/PRs/etc keyed by object
// id, strong ETags from a per-node version counter (If-None-Match -> 304 with no
// quota decrement), programmable REST + GraphQL rate-limit accounting, per-repo
// hooks with a deliveries/attempts redelivery API, an If-Modified-Since /notifications
// endpoint, an inspectable request log, and helpers to mutate nodes and emit
// signed webhook POSTs. It is imported only by tests and is excluded from the
// plugin's production LOC budget by path (plugins/github/fakegh/**).
package fakegh

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"time"
)

// Server is a running fake GitHub. Its embedded *httptest.Server exposes URL (the
// REST/GraphQL base), Close, and Client. All state is guarded by mu so the fake is
// safe under the concurrent misses the singleflight scenario drives.
type Server struct {
	*httptest.Server

	mu    sync.Mutex
	world *World
	log   []Request
	rate  rateState

	hooks          []*Hook
	deliveries     []*Delivery
	nextHookID     int64
	nextDeliveryID int64

	webhookSecret string

	notifLastModified time.Time
	notifThreads      []map[string]any
	pollInterval      int
}

// World is the mutable object graph the fake serves. repos is an ordered set of
// "owner/repo" strings for /user/repos pagination; nodes maps a canonical cache
// key (issue:o/r#N, pr:o/r#N, repo:o/r, ...) to its stored node.
type World struct {
	repos []string
	nodes map[string]*Node
}

// Node is one cached object. version is the source of the strong ETag; every
// mutation bumps it so a stale If-None-Match no longer matches.
type Node struct {
	Key      string
	Typename string
	Body     map[string]any

	version int
}

func (n *Node) etag() string { return fmt.Sprintf("%q", "v"+strconv.Itoa(n.version)) }

// Request is one recorded inbound HTTP request. The fields called out by the EDR
// (conditional headers, the reconcile since cursor, auth, and the GraphQL op name)
// are lifted out so tests can assert on them directly.
type Request struct {
	Method          string
	Path            string
	RawQuery        string
	Since           string
	IfNoneMatch     string
	IfModifiedSince string
	Authorization   string
	Op              string
}

type rateState struct {
	restRemaining int
	restReset     time.Time
	restFloor     int

	gqlRemaining int
	gqlReset     time.Time
	gqlFloor     int
	gqlCost      int
}

// New starts a fake GitHub with sane defaults (5000 REST + 5000 GraphQL points,
// resets one hour out, GraphQL cost 1, poll interval 60s). Call Close when done.
func New() *Server {
	s := &Server{
		world:        &World{nodes: map[string]*Node{}},
		pollInterval: 60,
		rate: rateState{
			restRemaining: 5000,
			restReset:     time.Now().Add(time.Hour),
			gqlRemaining:  5000,
			gqlReset:      time.Now().Add(time.Hour),
			gqlCost:       1,
		},
	}
	s.Server = httptest.NewServer(s.routes())
	return s
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /user/repos", s.handleUserRepos)
	mux.HandleFunc("GET /users/{login}", s.handleUser)
	mux.HandleFunc("GET /notifications", s.handleNotifications)
	mux.HandleFunc("POST /graphql", s.handleGraphQL)

	mux.HandleFunc("GET /repos/{owner}/{repo}", s.handleRepo)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/{number}", s.handleIssue)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", s.handlePull)
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews/{id}", s.handleReview)
	mux.HandleFunc("GET /repos/{owner}/{repo}/issues/comments/{id}", s.handleComment)
	mux.HandleFunc("GET /repos/{owner}/{repo}/check-runs/{id}", s.handleCheckRun)
	mux.HandleFunc("GET /repos/{owner}/{repo}/labels/{name}", s.handleLabel)
	mux.HandleFunc("GET /repos/{owner}/{repo}/releases/tags/{tag}", s.handleRelease)
	mux.HandleFunc("GET /repos/{owner}/{repo}/commits/{sha}", s.handleCommit)
	mux.HandleFunc("GET /repos/{owner}/{repo}/git/refs/{ref...}", s.handleRef)

	mux.HandleFunc("POST /repos/{owner}/{repo}/hooks", s.handleCreateHook)
	mux.HandleFunc("GET /repos/{owner}/{repo}/hooks", s.handleListHooks)
	mux.HandleFunc("GET /repos/{owner}/{repo}/hooks/{id}/deliveries", s.handleListDeliveries)
	mux.HandleFunc("POST /repos/{owner}/{repo}/hooks/{id}/deliveries/{delivery}/attempts", s.handleRedeliver)

	return s.record(mux)
}

// record logs every request. For /graphql it peeks (and restores) the body to
// capture the operation name, so the request log carries op counts too.
func (s *Server) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var op string
		if r.URL.Path == "/graphql" && r.Body != nil {
			buf, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(buf))
			var q struct {
				OperationName string `json:"operationName"`
				Query         string `json:"query"`
			}
			_ = json.Unmarshal(buf, &q)
			op = q.OperationName
			if op == "" {
				op = parseOpName(q.Query)
			}
		}
		s.mu.Lock()
		s.log = append(s.log, Request{
			Method:          r.Method,
			Path:            r.URL.Path,
			RawQuery:        r.URL.RawQuery,
			Since:           r.URL.Query().Get("since"),
			IfNoneMatch:     r.Header.Get("If-None-Match"),
			IfModifiedSince: r.Header.Get("If-Modified-Since"),
			Authorization:   r.Header.Get("Authorization"),
			Op:              op,
		})
		s.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// --- rate-limit helpers (caller holds mu) ---

func (s *Server) setRESTHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Remaining", strconv.Itoa(s.rate.restRemaining))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(s.rate.restReset.Unix(), 10))
}

func (s *Server) decREST() {
	if s.rate.restRemaining > s.rate.restFloor {
		s.rate.restRemaining--
	}
}

func (s *Server) chargeGQL() {
	s.rate.gqlRemaining -= s.rate.gqlCost
	if s.rate.gqlRemaining < s.rate.gqlFloor {
		s.rate.gqlRemaining = s.rate.gqlFloor
	}
}

// --- programmable knobs ---

// SetRESTRate overrides the REST remaining count and reset time.
func (s *Server) SetRESTRate(remaining int, reset time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate.restRemaining, s.rate.restReset = remaining, reset
}

// SetRESTFloor sets the REST remaining floor; decrements never go below it.
func (s *Server) SetRESTFloor(floor int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate.restFloor = floor
}

// SetGraphQLRate sets the GraphQL points remaining, reset time, and per-op cost
// reported in every rateLimit block (used to drive the near-floor pause).
func (s *Server) SetGraphQLRate(remaining int, resetAt time.Time, cost int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate.gqlRemaining, s.rate.gqlReset, s.rate.gqlCost = remaining, resetAt, cost
}

// SetGraphQLFloor sets the GraphQL points floor; charges never go below it.
func (s *Server) SetGraphQLFloor(floor int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rate.gqlFloor = floor
}

// SetWebhookSecret sets the HMAC secret used by EmitWebhook and SignedPOST.
func (s *Server) SetWebhookSecret(secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.webhookSecret = secret
}

// WebhookSecret returns the current server-level webhook secret.
func (s *Server) WebhookSecret() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.webhookSecret
}

// SetPollInterval sets the X-Poll-Interval (seconds) served by /notifications.
func (s *Server) SetPollInterval(seconds int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pollInterval = seconds
}

// --- request log inspection ---

// Requests returns a copy of the recorded request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.log))
	copy(out, s.log)
	return out
}

// CountPath counts recorded requests whose method matches and whose path contains
// substr. An empty method matches any method.
func (s *Server) CountPath(method, substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.log {
		if (method == "" || r.Method == method) && contains(r.Path, substr) {
			n++
		}
	}
	return n
}

// CountOp counts recorded GraphQL requests carrying the given operation name.
func (s *Server) CountOp(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.log {
		if r.Op == op {
			n++
		}
	}
	return n
}

// ResetLog clears the recorded request log.
func (s *Server) ResetLog() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = nil
}

func contains(s, sub string) bool { return sub == "" || bytes.Contains([]byte(s), []byte(sub)) }

func newGUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
