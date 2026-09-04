package github

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

const hookSecret = "topsecret"

func webhookRequest(t *testing.T, event, delivery, sig string, payload map[string]any) *http.Request {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/plugins/github/webhook", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	return req
}

func issuePayload() map[string]any {
	return map[string]any{
		"repository": map[string]any{"full_name": "o/r"},
		"issue":      map[string]any{"number": float64(5)},
	}
}

// TestWebhookHMAC covers the three signature outcomes: a valid signature purges and
// emits exactly one event; a bad or missing signature is 401 with no emit.
func TestWebhookHMAC(t *testing.T) {
	body, _ := json.Marshal(issuePayload())
	good := fakegh.Sign(hookSecret, body)

	cases := []struct {
		name   string
		sig    string
		status int
		emits  int
	}{
		{"valid", good, http.StatusOK, 1},
		{"bad", "sha256=deadbeef", http.StatusUnauthorized, 0},
		{"missing", "", http.StatusUnauthorized, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := newPlugin(t, map[string]any{"webhookSecret": hookSecret})
			rec := &recorder{}
			rec.install(p)
			ctx := context.Background()
			_ = p.store.upsert(ctx, &node{Key: "issue:o/r#5", FetchedAt: time.Now(), UpdatedAt: time.Now()})

			w := httptest.NewRecorder()
			p.handleWebhook(w, webhookRequest(t, "issues", "d1", c.sig, issuePayload()))

			if w.Code != c.status {
				t.Errorf("status = %d, want %d", w.Code, c.status)
			}
			ev := rec.events()
			if len(ev) != c.emits {
				t.Fatalf("emits = %d, want %d", len(ev), c.emits)
			}
			if c.emits == 1 {
				if ev[0].Type != "github.node.purged" || ev[0].Key != "issue:o/r#5" {
					t.Errorf("envelope = %+v", ev[0])
				}
				if n, _ := p.store.get(ctx, "issue:o/r#5"); n != nil {
					t.Errorf("node not purged")
				}
			}
		})
	}
}

// TestWebhookDedup: a redelivered X-GitHub-Delivery is verified but processed once.
func TestWebhookDedup(t *testing.T) {
	p := newPlugin(t, map[string]any{"webhookSecret": hookSecret})
	rec := &recorder{}
	rec.install(p)
	body, _ := json.Marshal(issuePayload())
	sig := fakegh.Sign(hookSecret, body)

	w1 := httptest.NewRecorder()
	p.handleWebhook(w1, webhookRequest(t, "issues", "dup", sig, issuePayload()))
	w2 := httptest.NewRecorder()
	p.handleWebhook(w2, webhookRequest(t, "issues", "dup", sig, issuePayload()))

	if w1.Code != http.StatusOK || w2.Code != http.StatusOK {
		t.Errorf("status = %d,%d", w1.Code, w2.Code)
	}
	if ev := rec.events(); len(ev) != 1 {
		t.Errorf("emits = %d, want 1 (deduped)", len(ev))
	}
}

// TestWebhookUnaddressable: a signed event with no addressable object is 200 no-op.
func TestWebhookUnaddressable(t *testing.T) {
	p := newPlugin(t, map[string]any{"webhookSecret": hookSecret})
	rec := &recorder{}
	rec.install(p)
	payload := map[string]any{"zen": "keep it simple"} // ping-like, no repository
	body, _ := json.Marshal(payload)
	sig := fakegh.Sign(hookSecret, body)

	w := httptest.NewRecorder()
	p.handleWebhook(w, webhookRequest(t, "ping", "p1", sig, payload))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d", w.Code)
	}
	if len(rec.events()) != 0 {
		t.Errorf("unexpected emit")
	}
}
