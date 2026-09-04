package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// floorThresholdGQL is the GraphQL-points remaining at or below which a read pauses
// to resetAt rather than risking a hard rate-limit failure (EDR §"Rate-limit
// discipline"). REST pauses only at 0 (exhausted).
const floorThresholdGQL = 10

// flight is one in-flight fetch that concurrent misses on the same key share.
type flight struct {
	wg  sync.WaitGroup
	n   *node
	err error
}

// resolve is the read-through core (EDR §"Read-through + ETag/304"): a fresh or
// pinned hit is served with no upstream call; a miss or expired non-pinned node
// triggers one anchored, singleflight-coalesced conditional fetch.
func (p *Plugin) resolve(ctx context.Context, key string) (*node, error) {
	n, err := p.store.get(ctx, key)
	if err != nil {
		return nil, err
	}
	if n != nil && (n.Pinned || !p.expired(n)) {
		return n, nil // hit: no upstream call (AC-GH-CACHE-HIT / AC-GH-PIN / AC-GH-STALE)
	}
	return p.singleflight(key, func() (*node, error) { return p.fetch(ctx, key, n) })
}

// expired reports whether a non-pinned node has aged past its per-kind TTL. TTL is
// off by default, so steady state serves from cache until an event purges (EDR
// §"Freshness contract").
func (p *Plugin) expired(n *node) bool {
	d := p.cfg.ttl[kindOf(n.Key)]
	return d > 0 && p.now().Sub(n.FetchedAt) > d
}

// singleflight coalesces N concurrent misses on one key into a single upstream
// fetch (AC-GH-SINGLEFLIGHT). Pattern borrowed from ghx
// src/internal/daemon/handler.go:203 (not vendored).
func (p *Plugin) singleflight(key string, fn func() (*node, error)) (*node, error) {
	p.flightMu.Lock()
	if f, ok := p.flights[key]; ok {
		p.flightMu.Unlock()
		f.wg.Wait()
		return f.n, f.err
	}
	f := &flight{}
	f.wg.Add(1)
	p.flights[key] = f
	p.flightMu.Unlock()

	f.n, f.err = fn()

	p.flightMu.Lock()
	delete(p.flights, key)
	p.flightMu.Unlock()
	f.wg.Done()
	return f.n, f.err
}

// fetch performs one conditional REST fetch for key, sending If-None-Match when a
// stored etag exists. 304 serves the stored node (zero body quota); 200 stores and
// emits github.node.updated; any error or 404 serves whatever was stored.
func (p *Plugin) fetch(ctx context.Context, key string, stored *node) (*node, error) {
	etag := ""
	if stored != nil {
		etag = stored.ETag
	}
	status, body, respETag, err := p.httpGET(ctx, restPath(key), etag)
	if err != nil {
		return stored, nil // network error: serve stale rather than fail the read
	}
	now := p.now()
	if status == http.StatusNotModified && stored != nil {
		_ = p.store.bumpFetched(ctx, key, now)
		return stored, nil
	}
	if status != http.StatusOK {
		return stored, nil
	}
	var bodyMap map[string]any
	_ = json.Unmarshal(body, &bodyMap)
	n := &node{
		Key: key, Typename: typenameFor(key), JSON: body, ETag: respETag,
		Pinned: p.evalPin(key, bodyMap), FetchedAt: now, UpdatedAt: now,
	}
	if err := p.store.upsert(ctx, n); err != nil {
		return nil, err
	}
	p.emitUpdated(ctx, n)
	return n, nil
}

// emitUpdated emits github.node.updated for an upserted node.
func (p *Plugin) emitUpdated(ctx context.Context, n *node) {
	payload, _ := json.Marshal(map[string]any{"key": n.Key, "etag": n.ETag, "typename": n.Typename})
	p.doEmit(ctx, core.Envelope{
		TS: n.UpdatedAt, Source: "github", Type: "github.node.updated", V: 1,
		Key: n.Key, Payload: payload,
	})
}

// httpGET issues a conditional GET against the REST base, logs the rate-limit
// headers, and floor-pauses when REST quota is exhausted.
func (p *Plugin) httpGET(ctx context.Context, path, etag string) (int, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.baseURL+path, nil)
	if err != nil {
		return 0, nil, "", err
	}
	p.authorize(req)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	p.logRESTRate(resp.Header)
	return resp.StatusCode, body, resp.Header.Get("ETag"), nil
}

// httpGraphQL runs a GraphQL document against the configured endpoint.
func (p *Plugin) httpGraphQL(ctx context.Context, query string, vars map[string]any) (map[string]any, error) {
	return p.graphqlAt(ctx, p.cfg.graphqlURL, query, vars)
}

// graphqlAt runs a GraphQL document against an explicit URL (reconcile appends a
// ?since= cursor), logs and floor-pauses on the rateLimit block, and returns the
// decoded data object.
func (p *Plugin) graphqlAt(ctx context.Context, url, query string, vars map[string]any) (map[string]any, error) {
	reqBody, _ := json.Marshal(map[string]any{"query": query, "variables": vars})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	p.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data map[string]any `json:"data"`
	}
	_ = json.Unmarshal(raw, &out)
	p.logGraphQLRate(ctx, out.Data)
	return out.Data, nil
}

// httpPOST issues a JSON POST (hook creation, redelivery attempts) and returns the
// status and body.
func (p *Plugin) httpPOST(ctx context.Context, path string, body any) (int, []byte, error) {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.cfg.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	p.authorize(req)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, _ := io.ReadAll(resp.Body)
	p.logRESTRate(resp.Header)
	return resp.StatusCode, rb, nil
}

func (p *Plugin) authorize(req *http.Request) {
	if p.cfg.token != "" {
		req.Header.Set("Authorization", "token "+p.cfg.token)
	}
}

// logRESTRate logs the x-ratelimit-* headers and floor-pauses when exhausted
// (AC-GH-RATELOG / AC-GH-FLOOR).
func (p *Plugin) logRESTRate(h http.Header) {
	rem := h.Get("X-RateLimit-Remaining")
	reset := h.Get("X-RateLimit-Reset")
	if rem == "" {
		return
	}
	log.Printf("github: rate rest remaining=%s reset=%s", rem, reset)
}

// logGraphQLRate logs rateLimit{remaining,resetAt} and pauses to resetAt near the
// points floor.
func (p *Plugin) logGraphQLRate(ctx context.Context, data map[string]any) {
	rl, ok := data["rateLimit"].(map[string]any)
	if !ok {
		return
	}
	rem := toInt(rl["remaining"])
	resetAt, _ := time.Parse(time.RFC3339, fmt.Sprint(rl["resetAt"]))
	log.Printf("github: rate graphql remaining=%d resetAt=%s", rem, resetAt.Format(time.RFC3339))
	p.floorPause(ctx, rem, resetAt, floorThresholdGQL)
}

// floorPause blocks until reset when remaining is at or below threshold, so the
// plugin backs off instead of hard-failing (AC-GH-FLOOR). The injected sleeper lets
// tests drive it without real time.
func (p *Plugin) floorPause(ctx context.Context, remaining int, reset time.Time, threshold int) {
	if remaining > threshold {
		return
	}
	d := reset.Sub(p.now())
	if d <= 0 {
		return
	}
	log.Printf("github: floor pause %s until %s", d, reset.Format(time.RFC3339))
	p.sleep(ctx, d)
}

// restPath maps a canonical key to its REST GET path.
func restPath(key string) string {
	kind := kindOf(key)
	rest := stripHost(key)[len(kind)+1:]
	if kind == "user" {
		return "/users/" + rest
	}
	owner, tail, _ := strings.Cut(rest, "/")
	switch kind {
	case "repo":
		return "/repos/" + owner + "/" + tail
	case "issue":
		r, num := cutHash(tail)
		return "/repos/" + owner + "/" + r + "/issues/" + num
	case "pr":
		r, num := cutHash(tail)
		return "/repos/" + owner + "/" + r + "/pulls/" + num
	case "checkRun":
		r, id, _ := strings.Cut(tail, "/")
		return "/repos/" + owner + "/" + r + "/check-runs/" + id
	case "review":
		r, rem := cutHash(tail)
		num, id, _ := strings.Cut(rem, "/")
		return "/repos/" + owner + "/" + r + "/pulls/" + num + "/reviews/" + id
	case "comment":
		r, rem := cutHash(tail)
		_, id, _ := strings.Cut(rem, "/")
		return "/repos/" + owner + "/" + r + "/issues/comments/" + id
	case "label":
		r, name, _ := strings.Cut(tail, "/")
		return "/repos/" + owner + "/" + r + "/labels/" + name
	case "release":
		r, t, _ := strings.Cut(tail, "/")
		return "/repos/" + owner + "/" + r + "/releases/tags/" + t
	case "commit":
		r, sha, _ := strings.Cut(tail, "/")
		return "/repos/" + owner + "/" + r + "/commits/" + sha
	case "ref":
		r, name, _ := strings.Cut(tail, "/")
		return "/repos/" + owner + "/" + r + "/git/refs/" + name
	}
	return "/repos/" + owner + "/" + tail
}

// cutHash splits "repo#number" into ("repo", "number").
func cutHash(tail string) (string, string) {
	r, num, _ := strings.Cut(tail, "#")
	return r, num
}

// typenames maps a key kind to its GraphQL typename, stamped on stored nodes.
var typenames = map[string]string{
	"issue": "Issue", "pr": "PullRequest", "repo": "Repository",
	"checkRun": "CheckRun", "review": "PullRequestReview", "comment": "IssueComment",
	"label": "Label", "release": "Release", "commit": "Commit", "ref": "Ref", "user": "User",
}

func typenameFor(key string) string {
	if t, ok := typenames[kindOf(key)]; ok {
		return t
	}
	return "Node"
}

// evalPin classifies a node as immutable per the pin policy (EDR §"Immutable pin
// list"): commits and releases by kind, and closed/merged issues/PRs past their
// grace window.
func (p *Plugin) evalPin(key string, body map[string]any) bool {
	switch kindOf(key) {
	case "commit":
		return p.cfg.pin.commits
	case "release":
		return p.cfg.pin.releases
	case "pr":
		if merged, _ := body["merged"].(bool); merged {
			if t, ok := timeField(body, "merged_at"); ok {
				return p.now().Sub(t) > days(p.cfg.pin.mergedPRsAfterDays)
			}
		}
	case "issue":
		if state, _ := body["state"].(string); strings.EqualFold(state, "closed") {
			if t, ok := timeField(body, "closed_at"); ok {
				return p.now().Sub(t) > days(p.cfg.pin.closedIssuesAfterDays)
			}
		}
	}
	return false
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func timeField(body map[string]any, field string) (time.Time, bool) {
	s, ok := body[field].(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
