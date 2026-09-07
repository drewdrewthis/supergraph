package github

import (
	"context"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

func newReconcilePlugin(t *testing.T, srv *fakegh.Server) *Plugin {
	t.Helper()
	return newPlugin(t, map[string]any{
		"baseURL":       srv.URL,
		"graphqlURL":    srv.URL + "/graphql",
		"token":         "t",
		"webhookSecret": "s",
	})
}

// TestDiscoverRepos: discovery pages /user/repos until a short page ends it.
func TestDiscoverRepos(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	srv.AddRepo("o", "a")
	srv.AddRepo("o", "b")

	repos := p.discoverRepos(context.Background())
	if len(repos) != 2 {
		t.Fatalf("discovered %v, want 2", repos)
	}
	// a single short page → exactly one list request, no runaway pagination.
	if c := srv.CountPath("GET", "/user/repos"); c != 1 {
		t.Errorf("repo-list requests = %d, want 1", c)
	}
}

// TestReconcileOnce: a reconcile pass discovers the repo, creates one hook, pulls
// open issues, and advances both the repo since-cursor and the health cursor.
func TestReconcileOnce(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	p.cfg.hookRepos = []string{"o/r"}
	srv.AddRepo("o", "r")
	srv.AddIssue("o", "r", 5, "Five", "open")
	ctx := context.Background()

	p.reconcileOnce(ctx)

	if _, ok := p.store.hook(ctx, "o", "r"); !ok {
		t.Error("hook not created")
	}
	if c := srv.CountPath("POST", "/hooks"); c != 1 {
		t.Errorf("hook POSTs = %d, want 1", c)
	}
	if n, _ := p.store.get(ctx, "issue:o/r#5"); n == nil {
		t.Error("open issue not stored by since-reconcile")
	}
	if p.store.cursor(ctx, "since:o/r") == "" {
		t.Error("since cursor not advanced")
	}
	if p.Cursor(ctx) == "" {
		t.Error("reconcile health cursor not advanced")
	}

	// a second pass must not create a duplicate hook.
	srv.ResetLog()
	p.reconcileOnce(ctx)
	if c := srv.CountPath("POST", "/hooks"); c != 0 {
		t.Errorf("duplicate hook created: %d POSTs", c)
	}
}

// TestSinceReconcileOverlap: once a since-cursor exists, the next pull sends
// since=<cursor-60s> (a clock-skew overlap window), not the bare cursor.
func TestSinceReconcileOverlap(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	ctx := context.Background()
	// seed a prior cursor at a fixed instant.
	cursorT := time.Date(2026, 9, 5, 10, 0, 0, 0, time.UTC)
	_ = p.store.setCursor(ctx, "since:o/r", cursorT.Format(time.RFC3339))

	srv.ResetLog()
	p.sinceReconcile(ctx, "o", "r")

	want := cursorT.Add(-60 * time.Second).UTC().Format(time.RFC3339)
	found := false
	for _, r := range srv.Requests() {
		if r.Since == want {
			found = true
		}
	}
	if !found {
		t.Errorf("since=%q not sent; requests=%+v", want, srv.Requests())
	}
}

// TestFloorPause: at or below the points floor the plugin pauses to resetAt via the
// injected sleeper; above it, it does not (AC-GH-FLOOR).
func TestFloorPause(t *testing.T) {
	p := newPlugin(t, nil)
	now := p.now()
	reset := now.Add(30 * time.Minute)

	cases := []struct {
		name      string
		remaining int
		wantSleep time.Duration
	}{
		{"below floor pauses", floorThresholdGQL - 1, 30 * time.Minute},
		{"at floor pauses", floorThresholdGQL, 30 * time.Minute},
		{"above floor no pause", floorThresholdGQL + 1, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var slept time.Duration
			p.sleep = func(_ context.Context, d time.Duration) { slept = d }
			p.floorPause(context.Background(), c.remaining, reset, floorThresholdGQL)
			if slept != c.wantSleep {
				t.Errorf("slept %v, want %v", slept, c.wantSleep)
			}
		})
	}
}

// TestEnsureHookAllowlistEmpty: with an empty hookRepos allowlist (the owner
// default) reconcile creates no webhooks — zero POST /hooks (finding B).
func TestEnsureHookAllowlistEmpty(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv) // no hookRepos set ⇒ empty
	srv.AddRepo("o", "r")
	ctx := context.Background()

	p.reconcileOnce(ctx)

	if c := srv.CountPath("POST", "/hooks"); c != 0 {
		t.Errorf("empty allowlist created %d hooks, want 0", c)
	}
	if _, ok := p.store.hook(ctx, "o", "r"); ok {
		t.Error("hook recorded despite empty allowlist")
	}
}

// TestEnsureHookAllowlistScoped: only repos in hookRepos get a hook; others are
// skipped even when discovered (finding B).
func TestEnsureHookAllowlistScoped(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	p.cfg.hookRepos = []string{"o/a"}
	srv.AddRepo("o", "a")
	srv.AddRepo("o", "b")
	ctx := context.Background()

	p.reconcileOnce(ctx)

	if _, ok := p.store.hook(ctx, "o", "a"); !ok {
		t.Error("allowlisted repo o/a got no hook")
	}
	if _, ok := p.store.hook(ctx, "o", "b"); ok {
		t.Error("non-allowlisted repo o/b got a hook")
	}
	if c := srv.CountPath("POST", "/repos/o/b/hooks"); c != 0 {
		t.Errorf("non-allowlisted repo o/b: %d hook POSTs, want 0", c)
	}
}
