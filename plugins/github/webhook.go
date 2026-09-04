package github

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"

	"github.com/drewdrewthis/supergraph/core"
)

// handleWebhook receives a GitHub webhook (direct, forwarded, or redelivered),
// verifies its HMAC, and invalidates the cache for the object it touches. A bad or
// absent signature is 401 with no emit (AC-GH-HMAC); a good one purges the touched
// key (evicting the node and every list result whose scope covers it) and emits one
// github.node.purged envelope (AC-GH-PURGE-TAG).
func (p *Plugin) handleWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", http.StatusBadRequest)
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
