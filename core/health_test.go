package core

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fixedNow returns a now func pinned to t for deterministic lag derivation.
func fixedNow(t *time.Time) func() time.Time { return func() time.Time { return *t } }

// AC-CORE-3: state derivation and the null-lastEventAt rule, driven by a fixed clock.
func TestHealthStateDerivation(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	clock := base
	h := NewHealthAggregator(300) // 300s threshold
	h.now = fixedNow(&clock)

	// Registered, no event yet -> starting, nil lastEventAt, zero lag.
	h.MarkStarting("template")
	snap := snapByName(t, h)
	if got := snap["template"]; got.State != HealthStarting || got.LastEventAt != nil || got.LagSeconds != 0 {
		t.Fatalf("before first event: %+v", got)
	}

	// One event, now 10s later -> ok, lag 10.
	h.Record("template", base)
	clock = base.Add(10 * time.Second)
	if got := snapByName(t, h)["template"]; got.State != HealthOK || got.LagSeconds != 10 || got.LastEventAt == nil {
		t.Fatalf("within threshold: %+v", got)
	}

	// now 400s past the event -> lag over threshold -> stale.
	clock = base.Add(400 * time.Second)
	if got := snapByName(t, h)["template"]; got.State != HealthStale {
		t.Fatalf("past threshold: %+v", got)
	}

	// Panic mark with no event of its own -> stale immediately.
	h.MarkStale("crashed")
	if got := snapByName(t, h)["crashed"]; got.State != HealthStale {
		t.Fatalf("panic-marked: %+v", got)
	}

	// Snapshot is sorted by plugin name.
	all := h.Snapshot()
	if len(all) != 2 || all[0].Plugin != "crashed" || all[1].Plugin != "template" {
		t.Fatalf("snapshot order: %+v", all)
	}
}

// AC-CORE-3: /health returns 200 + a JSON array with exactly the contract keys, and
// lastEventAt serializes as JSON null before any event.
func TestHealthServeHTTPShape(t *testing.T) {
	h := NewHealthAggregator(300)
	h.MarkStarting("template")

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var raw []map[string]json.RawMessage
	if err := json.Unmarshal(rr.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode array: %v (body=%s)", err, rr.Body.String())
	}
	if len(raw) != 1 {
		t.Fatalf("want 1 element, got %d", len(raw))
	}
	want := []string{"plugin", "lastEventAt", "cursor", "lagSeconds", "state"}
	el := raw[0]
	if len(el) != len(want) {
		t.Fatalf("keys = %v", el)
	}
	for _, k := range want {
		if _, ok := el[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if string(el["lastEventAt"]) != "null" {
		t.Errorf("lastEventAt = %s, want null", el["lastEventAt"])
	}
}

// snapByName indexes a snapshot by plugin name for assertions.
func snapByName(t *testing.T, h *HealthAggregator) map[string]HealthStatus {
	t.Helper()
	out := map[string]HealthStatus{}
	for _, hs := range h.Snapshot() {
		out[hs.Plugin] = hs
	}
	return out
}
