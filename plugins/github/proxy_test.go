package github

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

// TestResolveReadThrough: a miss fetches once and emits github.node.updated; a second
// read of the still-fresh node is served from cache with no upstream call.
func TestResolveReadThrough(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newPlugin(t, map[string]any{"baseURL": srv.URL, "token": "t"})
	rec := &recorder{}
	rec.install(p)
	srv.AddIssue("o", "r", 5, "Boom", "open")
	ctx := context.Background()

	n, err := p.resolve(ctx, "issue:o/r#5")
	if err != nil || n == nil {
		t.Fatalf("resolve: %v %v", n, err)
	}
	if n.Typename != "Issue" || n.ETag != srv.ETag("issue:o/r#5") {
		t.Errorf("node = %+v", n)
	}
	if c := srv.CountPath("GET", "/issues/5"); c != 1 {
		t.Fatalf("upstream GETs = %d, want 1", c)
	}
	if _, err := p.resolve(ctx, "issue:o/r#5"); err != nil {
		t.Fatal(err)
	}
	if c := srv.CountPath("GET", "/issues/5"); c != 1 {
		t.Errorf("cache hit still fetched: GETs = %d", c)
	}
	if ev := rec.events(); len(ev) != 1 || ev[0].Type != "github.node.updated" {
		t.Errorf("emits = %+v, want one github.node.updated", ev)
	}
}

// TestFetchETag304: a conditional refetch of an unchanged node returns 304, bumps
// freshness, keeps the body, and emits nothing (AC-GH-ETAG-304).
func TestFetchETag304(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newPlugin(t, map[string]any{"baseURL": srv.URL, "token": "t"})
	rec := &recorder{}
	rec.install(p)
	srv.AddIssue("o", "r", 5, "Boom", "open")
	ctx := context.Background()

	first, _ := p.resolve(ctx, "issue:o/r#5")
	stored, _ := p.store.get(ctx, "issue:o/r#5")
	srv.ResetLog()
	rec.ev = nil

	got, err := p.fetch(ctx, "issue:o/r#5", stored)
	if err != nil {
		t.Fatal(err)
	}
	if srv.CountPath("GET", "/issues/5") != 1 {
		t.Errorf("expected one conditional GET")
	}
	if string(got.JSON) != string(first.JSON) {
		t.Errorf("304 changed body: %s", got.JSON)
	}
	if len(rec.events()) != 0 {
		t.Errorf("304 emitted an update: %+v", rec.events())
	}
}

// TestFetch200EmitsUpdate: a changed upstream body (new ETag) is stored and emits
// github.node.updated.
func TestFetch200EmitsUpdate(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newPlugin(t, map[string]any{"baseURL": srv.URL, "token": "t"})
	rec := &recorder{}
	rec.install(p)
	srv.AddIssue("o", "r", 5, "Boom", "open")
	ctx := context.Background()
	_, _ = p.resolve(ctx, "issue:o/r#5")
	stored, _ := p.store.get(ctx, "issue:o/r#5")
	rec.ev = nil
	srv.AddIssue("o", "r", 5, "Boom!", "open") // bump version → new ETag

	got, err := p.fetch(ctx, "issue:o/r#5", stored)
	if err != nil {
		t.Fatal(err)
	}
	if got.ETag == stored.ETag {
		t.Errorf("ETag unchanged after upstream mutation")
	}
	if ev := rec.events(); len(ev) != 1 || ev[0].Type != "github.node.updated" {
		t.Errorf("emits = %+v", ev)
	}
}

// TestSingleflight: concurrent misses on one key coalesce into a single fetch.
func TestSingleflight(t *testing.T) {
	p := newPlugin(t, nil)
	var calls int32
	release := make(chan struct{})
	const n = 12
	var wg sync.WaitGroup
	results := make([]*node, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, _ := p.singleflight("issue:o/r#5", func() (*node, error) {
				atomic.AddInt32(&calls, 1)
				<-release
				return &node{Key: "issue:o/r#5"}, nil
			})
			results[i] = r
		}(i)
	}
	// let every caller queue on the in-flight fetch before it completes.
	for atomic.LoadInt32(&calls) < 1 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("fetch ran %d times, want 1 (coalesced)", got)
	}
	for i, r := range results {
		if r == nil || r.Key != "issue:o/r#5" {
			t.Errorf("caller %d got %+v", i, r)
		}
	}
}

// TestEvalPin covers the immutable-pin policy branches with an injected clock.
func TestEvalPin(t *testing.T) {
	p := newPlugin(t, nil)
	now := p.now()
	cases := []struct {
		name string
		key  string
		body map[string]any
		want bool
	}{
		{"commit always", "commit:o/r/sha", nil, true},
		{"release always", "release:o/r/v1", nil, true},
		{"merged pr past grace", "pr:o/r#1", map[string]any{"merged": true, "merged_at": now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)}, true},
		{"merged pr within grace", "pr:o/r#1", map[string]any{"merged": true, "merged_at": now.Add(-1 * 24 * time.Hour).Format(time.RFC3339)}, false},
		{"open pr", "pr:o/r#1", map[string]any{"merged": false}, false},
		{"closed issue past grace", "issue:o/r#1", map[string]any{"state": "closed", "closed_at": now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)}, true},
		{"open issue", "issue:o/r#1", map[string]any{"state": "open"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := p.evalPin(c.key, c.body); got != c.want {
				t.Errorf("evalPin = %v, want %v", got, c.want)
			}
		})
	}
}
