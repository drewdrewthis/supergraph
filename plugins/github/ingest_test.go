package github

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

// TestForwardSupervisorBackoff: when the `gh webhook forward` child exits, the
// supervisor restarts it with exponential backoff (500ms, 1s, 2s, ...) via the
// injected sleeper, and reruns redelivery before each (re)start.
func TestForwardSupervisorBackoff(t *testing.T) {
	p := newPlugin(t, map[string]any{"ingress": "forward"})
	p.cfg.ghPath = "/nonexistent/gh-binary-xyz" // exec fails immediately → restart loop

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var sleeps []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) {
		mu.Lock()
		sleeps = append(sleeps, d)
		n := len(sleeps)
		mu.Unlock()
		if n >= 3 {
			cancel()
		}
	}

	p.forwardSupervisor(ctx)

	mu.Lock()
	defer mu.Unlock()
	if len(sleeps) < 3 {
		t.Fatalf("supervisor restarted %d times, want >=3", len(sleeps))
	}
	want := []time.Duration{backoffInitial, 2 * backoffInitial, 4 * backoffInitial}
	for i, w := range want {
		if sleeps[i] != w {
			t.Errorf("backoff[%d] = %v, want %v", i, sleeps[i], w)
		}
	}
}

// TestForwardSupervisorBackoffCap: backoff saturates at backoffMax.
func TestForwardSupervisorBackoffCap(t *testing.T) {
	p := newPlugin(t, map[string]any{"ingress": "forward"})
	p.cfg.ghPath = "/nonexistent/gh-binary-xyz"
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var last time.Duration
	count := 0
	p.sleep = func(_ context.Context, d time.Duration) {
		mu.Lock()
		last = d
		count++
		done := count >= 12
		mu.Unlock()
		if done {
			cancel()
		}
	}
	p.forwardSupervisor(ctx)
	mu.Lock()
	defer mu.Unlock()
	if last != backoffMax {
		t.Errorf("saturated backoff = %v, want %v", last, backoffMax)
	}
}

// TestRedeliver: an undelivered delivery with id past the cursor triggers a
// redelivery attempt POST and advances the per-repo lastDeliveryId cursor; a rerun
// with nothing new issues no further attempts (AC-GH-FORWARD).
func TestRedeliver(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newPlugin(t, map[string]any{"baseURL": srv.URL, "token": "t"})
	ctx := context.Background()

	// register a hook (id 1) in both fakegh and the plugin store.
	srv.AddRepo("o", "r")
	p.cfg.webhookSecret = "s"
	p.ensureHook(ctx, "o", "r")
	hookID, ok := p.store.hook(ctx, "o", "r")
	if !ok {
		t.Fatal("hook not recorded")
	}
	// queue an undelivered event on that hook.
	srv.QueueUndelivered(hookID, "issues", "opened", map[string]any{
		"repository": map[string]any{"full_name": "o/r"},
		"issue":      map[string]any{"number": float64(5)},
	})

	p.redeliver(ctx)
	if c := srv.CountPath("POST", "/attempts"); c != 1 {
		t.Fatalf("attempts POSTs = %d, want 1", c)
	}
	if cur := p.store.cursor(ctx, "hook:o/r:lastDeliveryId"); cur == "" {
		t.Errorf("cursor not advanced")
	}

	srv.ResetLog()
	p.redeliver(ctx) // nothing new past the cursor
	if c := srv.CountPath("POST", "/attempts"); c != 0 {
		t.Errorf("re-attempted an already-delivered event: %d", c)
	}
}

// TestNotificationsPoll304: the first poll (200) advances the Last-Modified cursor;
// the second sends If-Modified-Since and takes the zero-quota 304 path. Both honour
// X-Poll-Interval (AC-GH-NOTIFY-304).
func TestNotificationsPoll304(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newPlugin(t, map[string]any{"baseURL": srv.URL, "token": "t"})
	srv.SetPollInterval(90)
	srv.SetNotifications([]map[string]any{{"id": "1"}})
	ctx := context.Background()

	if got := p.pollOnce(ctx); got != 90 {
		t.Errorf("first poll interval = %d, want 90", got)
	}
	if p.store.cursor(ctx, "notif:lastModified") == "" {
		t.Fatal("200 did not advance the Last-Modified cursor")
	}
	srv.ResetLog()
	if got := p.pollOnce(ctx); got != 90 {
		t.Errorf("second poll interval = %d, want 90", got)
	}
	// the second poll must have sent the conditional header (→ 304 on the fake).
	sent := false
	for _, r := range srv.Requests() {
		if r.IfModifiedSince != "" {
			sent = true
		}
	}
	if !sent {
		t.Errorf("second poll did not send If-Modified-Since")
	}
}
