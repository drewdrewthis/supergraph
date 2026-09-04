package core

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"
)

// HealthAggregator holds the latest freshness signal for every plugin and derives
// the coarse HealthState from event lag. It is the single source behind both
// GET /health and the health GraphQL query, so their shapes cannot drift.
//
// now is injectable so tests can drive lag deterministically without sleeping.
type HealthAggregator struct {
	mu        sync.Mutex
	threshold time.Duration
	plugins   map[string]*pluginHealth
	now       func() time.Time
}

// pluginHealth is one plugin's mutable freshness state. State is not stored; it is
// derived at Snapshot time from lastEventAt, the forcedStale flag, and the current
// clock, so a plugin can go stale purely by the passage of time with no event to
// trigger it. cursorFn, when set, is the plugin's own CursorReporter.
type pluginHealth struct {
	lastEventAt *time.Time
	cursorFn    func() string
	forcedStale bool
}

// NewHealthAggregator builds an aggregator whose stale threshold is thresholdSeconds.
func NewHealthAggregator(thresholdSeconds float64) *HealthAggregator {
	return &HealthAggregator{
		threshold: time.Duration(thresholdSeconds * float64(time.Second)),
		plugins:   map[string]*pluginHealth{},
		now:       time.Now,
	}
}

// entry returns (creating if needed) the record for name. Caller holds mu.
func (h *HealthAggregator) entry(name string) *pluginHealth {
	p, ok := h.plugins[name]
	if !ok {
		p = &pluginHealth{}
		h.plugins[name] = p
	}
	return p
}

// MarkStarting registers a plugin before its first event so it shows as "starting"
// from the moment the supervisor builds it. It is also the only path that CLEARS a
// forced-stale mark: a plugin becomes healthy again only by being (re)started, never
// by a stray event, which is what keeps a crashed plugin from being laundered to ok.
func (h *HealthAggregator) MarkStarting(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entry(name).forcedStale = false
}

// Record stamps a plugin's latest event time. It deliberately does NOT clear a
// forced-stale mark: once a plugin's Start has crashed it is not running, so any
// event arriving afterwards (e.g. a stale canary in flight) must not resurrect it to
// ok. Only an explicit restart via MarkStarting clears the flag.
func (h *HealthAggregator) Record(name string, at time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.entry(name)
	t := at
	p.lastEventAt = &t
}

// MarkStale flags a plugin as unhealthy independent of lag. It is used for every
// forced failure — a panic in Start (AC-CORE-4) as well as a build, migrate, or
// store-open failure — so the field is named forcedStale, not "panicked". It stays
// stale until the next MarkStarting.
func (h *HealthAggregator) MarkStale(name string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entry(name).forcedStale = true
}

// SetCursorFunc registers a plugin's cursor accessor (its CursorReporter) for the
// health snapshot. fn is called while the aggregator lock is held during Snapshot,
// so it MUST be cheap and non-blocking — an in-memory read, never I/O.
func (h *HealthAggregator) SetCursorFunc(name string, fn func() string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entry(name).cursorFn = fn
}

// Snapshot returns every plugin's current HealthStatus, sorted by plugin name for
// a stable endpoint ordering. LagSeconds is now - lastEventAt; State precedence is
// forced-stale > no-event-yet (starting) > lag-over-threshold (stale) > ok. Cursor
// is pulled from the plugin's CursorReporter, if it registered one.
func (h *HealthAggregator) Snapshot() []HealthStatus {
	h.mu.Lock()
	defer h.mu.Unlock()

	now := h.now()
	out := make([]HealthStatus, 0, len(h.plugins))
	for name, p := range h.plugins {
		cursor := ""
		if p.cursorFn != nil {
			cursor = p.cursorFn()
		}
		hs := HealthStatus{Plugin: name, Cursor: cursor}
		switch {
		case p.forcedStale:
			hs.State = HealthStale
		case p.lastEventAt == nil:
			hs.State = HealthStarting
		default:
			hs.LagSeconds = now.Sub(*p.lastEventAt).Seconds()
			if hs.LagSeconds > h.threshold.Seconds() {
				hs.State = HealthStale
			} else {
				hs.State = HealthOK
			}
		}
		if p.lastEventAt != nil {
			t := *p.lastEventAt
			hs.LastEventAt = &t
		}
		out = append(out, hs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Plugin < out[j].Plugin })
	return out
}

// ServeHTTP answers GET /health with the snapshot JSON array (AC-CORE-3). The array
// is never null even with no plugins, and lastEventAt serializes as JSON null before
// a plugin's first event.
func (h *HealthAggregator) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(h.Snapshot())
}
