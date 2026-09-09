package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
)

// handleWebhook receives a GitHub webhook (direct, forwarded, or redelivered),
// verifies its HMAC, and invalidates the cache for the object it touches. A bad or
// absent signature is 401 with no emit (AC-GH-HMAC); a good one purges the touched
// key (evicting the node and every list result whose scope covers it) and emits one
// github.node.purged envelope (AC-GH-PURGE-TAG).
// maxWebhookBody caps an inbound webhook body (S2).
const maxWebhookBody = 1 << 20

func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	// S2: cap the body before any read/HMAC. GitHub caps payloads at 25 MiB but we
	// only read a handful of ids, so 1 MiB is ample and bounds pre-auth memory.
	r.Body = http.MaxBytesReader(w, r.Body, maxWebhookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", pluginconfig.ReadErrStatus(err))
		return
	}
	if !p.verifySignature(body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ctx := r.Context()
	// Redelivery replays a delivery id; dedup so a healed drop lands exactly once.
	if id := r.Header.Get("X-GitHub-Delivery"); id != "" {
		if p.store.deliverySeen(ctx, id) {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = p.store.markDelivery(ctx, id, p.now())
	}

	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	event := r.Header.Get("X-GitHub-Event")

	key, ok := eventKey(event, payload)
	if !ok {
		w.WriteHeader(http.StatusOK) // signed but no addressable object
		return
	}
	if _, err := p.store.purge(ctx, key); err != nil {
		http.Error(w, "purge failed", http.StatusInternalServerError)
		return
	}
	p.emitPurged(ctx, key)
	// A check_run fans out to the pr keys it ran against, so the sidebar's
	// statusCheckRollup refreshes on the next read (#26). The primary purge already
	// succeeded; a related-key failure logs and continues rather than 500-ing.
	for _, rk := range relatedKeys(event, payload) {
		if rk == key {
			continue
		}
		if _, err := p.store.purge(ctx, rk); err != nil {
			// %q escapes control chars (incl. newlines), so an attacker-shaped event
			// or key cannot inject log lines — the gosec taint pass cannot see that.
			log.Printf("github: webhook %q related purge failed for %q: %v", event, rk, err) //nolint:gosec // G706: %q escapes control chars
			continue
		}
		p.emitPurged(ctx, rk)
	}
	w.WriteHeader(http.StatusOK)
}

// verifySignature checks X-Hub-Signature-256 ("sha256="+hex HMAC-SHA256) with a
// constant-time compare. An empty configured secret rejects everything.
func (p *Plugin) verifySignature(body []byte, sig string) bool {
	if p.cfg.webhookSecret == "" || sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(p.cfg.webhookSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(sig))
}

// emitPurged emits github.node.purged for an evicted key.
func (p *Plugin) emitPurged(ctx context.Context, key string) {
	payload, _ := json.Marshal(map[string]any{"key": key})
	p.doEmit(ctx, core.Envelope{
		TS: p.now(), Source: "github", Type: "github.node.purged", V: 1,
		Key: key, Payload: payload,
	})
}
