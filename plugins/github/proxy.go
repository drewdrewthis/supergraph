package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

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

// fetchOutcome names what a conditional fetch did, so reconcile can tally the pass
// (checked/notModified/fetched/changed) without re-deriving it from the node.
type fetchOutcome int

const (
	outcomeOther       fetchOutcome = iota // network error, 404, or served-stale
	outcomeNotModified                     // 304: body unchanged, freshness bumped
	outcomeUnchanged                       // 200 but the canonical body was identical
	outcomeChanged                         // 200 with a changed canonical body; emitted
)

// fetch performs one conditional REST fetch for key, sending If-None-Match when a
// stored etag exists. 304 serves the stored node (zero body quota); 200 stores the
// canonical body and emits github.node.updated only when it changed; any error or
// 404 serves whatever was stored.
func (p *Plugin) fetch(ctx context.Context, key string, stored *node) (*node, error) {
	n, _, err := p.fetchNode(ctx, key, stored)
	return n, err
}

// fetchNode is fetch's core, additionally reporting the outcome for reconcile's
// per-pass counters. The REST conditional GET stays the cheap change detector
// (If-None-Match / 304), but on a 200 the body that LANDS for an Issue or
// PullRequest is re-read through its GraphQL point op so the store always holds the
// one canonical (GraphQL) shape the cache-only resolvers read — never the flat REST
// body, whose snake_case labels/assignees the resolvers cannot see. Kinds with no
// point op keep the REST body unchanged.
func (p *Plugin) fetchNode(ctx context.Context, key string, stored *node) (*node, fetchOutcome, error) {
	etag := ""
	if stored != nil {
		etag = stored.ETag
	}
	status, body, respETag, err := p.httpGET(ctx, restPath(key), etag)
	if err != nil {
		return stored, outcomeOther, nil // network error: serve stale rather than fail the read
	}
	now := p.now()
	if status == http.StatusNotModified && stored != nil {
		_ = p.store.bumpFetched(ctx, key, now)
		return stored, outcomeNotModified, nil
	}
	if status != http.StatusOK {
		return stored, outcomeOther, nil
	}
	canonical := body
	if cb, ok := p.canonicalBody(ctx, key); ok {
		canonical = cb
	} else if _, needsCanonical := graphqlPointOps[kindOf(key)]; stored != nil && needsCanonical && isGraphQLShape(stored.JSON) {
		return stored, outcomeOther, nil // canonicalization failed; keep the existing GraphQL-shaped node
	}
	var bodyMap map[string]any
	_ = json.Unmarshal(canonical, &bodyMap)
	hash := contentHash(canonical)
	n := &node{
		Key: key, Typename: typenameFor(key), JSON: canonical, ETag: respETag, ContentHash: hash,
		Pinned: p.evalPin(key, bodyMap), FetchedAt: now, UpdatedAt: now,
	}
	if err := p.store.upsert(ctx, n); err != nil {
		return nil, outcomeOther, err
	}
	// Emit only on a real content change: a fresh miss (no prior node) or a
	// differing hash. An etag change with an identical body — or the first
	// conditional GET of an etag-less row, which always 200s — stays silent.
	if stored == nil || stored.ContentHash != hash {
		p.emitUpdated(ctx, n)
		return n, outcomeChanged, nil
	}
	return n, outcomeUnchanged, nil
}

// graphqlPointOps maps a node kind to its GraphQL point-op name and the repository
// sub-field that carries the node, for the two kinds whose canonical shape is the
// GraphQL shape. Kinds absent here have no point op and keep their REST body.
var graphqlPointOps = map[string]struct{ op, field string }{
	"issue": {"issue", "issue"},
	"pr":    {"pr", "pullRequest"},
}

// isGraphQLShape reports whether raw is already the camelCase GraphQL node shape
// (updatedAt) rather than the flat REST projection (updated_at) — the thing worth
// protecting from a failed canonicalization.
func isGraphQLShape(raw []byte) bool { return strings.Contains(string(raw), `"updatedAt"`) }

// canonicalBody re-reads key's body through its GraphQL point op and returns the
// marshaled node object, so a landed Issue/PullRequest body is stored in the same
// shape the openIssues list path stores. It returns ok=false for a kind with no
// point op, an unparseable key, or an empty upstream result, leaving the REST body
// in place.
func (p *Plugin) canonicalBody(ctx context.Context, key string) ([]byte, bool) {
	spec, ok := graphqlPointOps[kindOf(key)]
	if !ok {
		return nil, false
	}
	op, ok := p.ops[spec.op]
	if !ok {
		return nil, false
	}
	parts := parseKey(key)
	number, err := strconv.Atoi(parts["disc"])
	if err != nil {
		return nil, false
	}
	vars := map[string]any{"owner": parts["owner"], "repo": parts["repo"], "number": number}
	data, err := p.graphqlAt(ctx, p.cfg.graphqlURL, op.query, vars)
	if err != nil {
		return nil, false
	}
	repo, _ := data["repository"].(map[string]any)
	if repo == nil {
		return nil, false
	}
	obj, _ := repo[spec.field].(map[string]any)
	if obj == nil {
		return nil, false
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return nil, false
	}
	return b, true
}

// contentHash is the sha256 over a node's canonical JSON, driving no-change
// suppression in fetchNode.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// emitUpdated emits github.node.updated for an upserted node.
func (p *Plugin) emitUpdated(ctx context.Context, n *node) {
	payload, _ := json.Marshal(map[string]any{"key": n.Key, "etag": n.ETag, "typename": n.Typename})
	p.doEmit(ctx, core.Envelope{
		TS: n.UpdatedAt, Source: "github", Type: "github.node.updated", V: 1,
		Key: n.Key, Payload: payload,
	})
}

// evalPin classifies a node as immutable per the pin policy (EDR §"Immutable pin
// list"): commits and releases by kind, and closed/merged issues/PRs past their
// grace window. It reads BOTH the canonical GraphQL shape now landed by the
// point/revalidate paths (mergedAt/closedAt camelCase, state MERGED/CLOSED) and the
// flat REST shape (merged_at/closed_at snake_case, state lowercase) that webhook
// ingest can still carry, so a merged/closed node pins regardless of which fetch
// path stored it.
func (p *Plugin) evalPin(key string, body map[string]any) bool {
	switch kindOf(key) {
	case "commit":
		return p.cfg.pin.commits
	case "release":
		return p.cfg.pin.releases
	case "pr":
		if t, ok := firstTime(body, "mergedAt", "merged_at"); ok {
			return p.now().Sub(t) > days(p.cfg.pin.mergedPRsAfterDays)
		}
		// A closed-unmerged PR is terminal too, so pin it past the same grace (P3).
		if s, _ := body["state"].(string); strings.EqualFold(s, "closed") {
			if t, ok := firstTime(body, "closedAt", "closed_at"); ok {
				return p.now().Sub(t) > days(p.cfg.pin.mergedPRsAfterDays)
			}
		}
	case "issue":
		if s, _ := body["state"].(string); strings.EqualFold(s, "closed") {
			if t, ok := firstTime(body, "closedAt", "closed_at"); ok {
				return p.now().Sub(t) > days(p.cfg.pin.closedIssuesAfterDays)
			}
		}
	}
	return false
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// firstTime returns the first of fields present as an RFC3339 time, letting a caller
// read the same instant under its GraphQL (camelCase) or REST (snake_case) name.
func firstTime(body map[string]any, fields ...string) (time.Time, bool) {
	for _, f := range fields {
		if t, ok := timeField(body, f); ok {
			return t, true
		}
	}
	return time.Time{}, false
}

func timeField(body map[string]any, field string) (time.Time, bool) {
	s, ok := body[field].(string)
	if !ok || s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}
