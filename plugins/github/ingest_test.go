package github

import (
	"context"
	"strings"
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
	p.cfg.forwardRepo = "o/r"                   // required target so forward actually starts

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
	p.cfg.forwardRepo = "o/r"
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
	p.cfg.hookRepos = []string{"o/r"}
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

// TestForwardArgs: the argv carries --url, --secret, --events (the exact ingested
// event set) and --repo; real `gh webhook forward` rejects a missing --events, so
// its presence is load-bearing (finding A).
func TestForwardArgs(t *testing.T) {
	p := newPlugin(t, map[string]any{"ingress": "forward"})
	p.cfg.selfURL = "http://127.0.0.1:7788"
	p.cfg.webhookSecret = "s"
	p.cfg.forwardRepo = "o/r"

	args, ok := p.forwardArgs()
	if !ok {
		t.Fatal("forwardArgs returned ok=false with forwardRepo set")
	}
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"webhook forward",
		"--url http://127.0.0.1:7788/plugins/github/webhook",
		"--secret s",
		"--repo o/r",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
	ev := argValue(args, "--events")
	if ev == "" {
		t.Fatal("--events not passed")
	}
	// --events must be exactly the webhook events the plugin ingests.
	if got, want := ev, strings.Join(defaultForwardEvents, ","); got != want {
		t.Errorf("--events = %q, want %q", got, want)
	}
	for _, e := range []string{"issues", "pull_request", "check_run"} {
		if !strings.Contains(ev, e) {
			t.Errorf("--events %q omits %q", ev, e)
		}
	}
}

// TestForwardArgsOrg: with only forwardOrg set the argv targets --org, not --repo.
func TestForwardArgsOrg(t *testing.T) {
	p := newPlugin(t, map[string]any{"ingress": "forward"})
	p.cfg.forwardOrg = "acme"
	args, ok := p.forwardArgs()
	if !ok {
		t.Fatal("ok=false with forwardOrg set")
	}
	if v := argValue(args, "--org"); v != "acme" {
		t.Errorf("--org = %q, want acme", v)
	}
	if argValue(args, "--repo") != "" {
		t.Error("--repo passed alongside forwardOrg")
	}
}

// TestForwardArgsUnconfigured: with neither forwardRepo nor forwardOrg, forwardArgs
// reports ok=false so the supervisor logs once and does not start `gh` (finding A).
func TestForwardArgsUnconfigured(t *testing.T) {
	p := newPlugin(t, map[string]any{"ingress": "forward"})
	if _, ok := p.forwardArgs(); ok {
		t.Error("ok=true with no forwardRepo/forwardOrg")
	}
}

// argValue returns the value following flag in args, or "".
func argValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
