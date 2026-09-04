package github

import (
	"context"
	"encoding/json"
	"net/http"
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
