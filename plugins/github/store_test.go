package github

import (
	"context"
	"testing"
	"time"
)

func TestStoreUpsertGet(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	in := &node{Key: "issue:o/r#5", Typename: "Issue", JSON: []byte(`{"n":5}`), ETag: `"v1"`, Pinned: true, FetchedAt: now, UpdatedAt: now}
	if err := p.store.upsert(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, err := p.store.get(ctx, "issue:o/r#5")
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.ETag != `"v1"` || !got.Pinned || string(got.JSON) != `{"n":5}` {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// pin column round-trips: an unset key reads back nil.
	if miss, _ := p.store.get(ctx, "issue:o/r#404"); miss != nil {
		t.Errorf("absent key = %+v, want nil", miss)
	}
}

func TestStoreBumpFetched(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	t0 := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	_ = p.store.upsert(ctx, &node{Key: "issue:o/r#5", FetchedAt: t0, UpdatedAt: t0})
	t1 := time.Now().UTC().Truncate(time.Second)
	if err := p.store.bumpFetched(ctx, "issue:o/r#5", t1); err != nil {
		t.Fatal(err)
	}
	got, _ := p.store.get(ctx, "issue:o/r#5")
	if !got.FetchedAt.Equal(t1) {
		t.Errorf("FetchedAt = %v, want %v", got.FetchedAt, t1)
	}
}

// TestStorePurgeTagIndex is the heart of the invalidation model: purging one node
// evicts it and every list result tagged with its covering composite, but leaves a
// sibling node and a sibling-kind list untouched.
func TestStorePurgeTagIndex(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	now := time.Now().UTC()
	// two issues + the openIssues list covering them, plus an unrelated labels list.
	_ = p.store.upsert(ctx, &node{Key: "issue:o/r#5", FetchedAt: now, UpdatedAt: now})
	_ = p.store.upsert(ctx, &node{Key: "issue:o/r#6", FetchedAt: now, UpdatedAt: now})
	_ = p.store.putList(ctx, listKey("openIssues", "repo:o/r"), []byte(`[]`), composite("issue:o/r#5"), now)
	_ = p.store.putList(ctx, listKey("repoLabels", "repo:o/r"), []byte(`[]`), composite("label:o/r/bug"), now)

	deleted, err := p.store.purge(ctx, "issue:o/r#5")
	if err != nil {
		t.Fatal(err)
	}
	assertGone := func(key string) {
		if n, _ := p.store.get(ctx, key); n != nil {
			t.Errorf("%q still present", key)
		}
	}
	assertPresent := func(key string) {
		if n, _ := p.store.get(ctx, key); n == nil {
			t.Errorf("%q was evicted", key)
		}
	}
	assertGone("issue:o/r#5")
	assertGone(listKey("openIssues", "repo:o/r"))
	assertPresent("issue:o/r#6")
	assertPresent(listKey("repoLabels", "repo:o/r"))
	if len(deleted) != 2 {
		t.Errorf("deleted = %v, want the node + its list", deleted)
	}
}

func TestStoreDeliveryDedup(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	if p.store.deliverySeen(ctx, "d1") {
		t.Fatal("unseen delivery reported seen")
	}
	_ = p.store.markDelivery(ctx, "d1", time.Now())
	if !p.store.deliverySeen(ctx, "d1") {
		t.Error("marked delivery not seen")
	}
}

func TestStoreHooks(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	if _, ok := p.store.hook(ctx, "o", "r"); ok {
		t.Fatal("absent hook reported present")
	}
	_ = p.store.putHook(ctx, "o", "r", 42, "secret")
	id, ok := p.store.hook(ctx, "o", "r")
	if !ok || id != 42 {
		t.Errorf("hook = (%d,%v)", id, ok)
	}
	hooks, err := p.store.hooks(ctx)
	if err != nil || len(hooks) != 1 || hooks[0] != [3]string{"o", "r", "42"} {
		t.Errorf("hooks = %v (%v)", hooks, err)
	}
}

func TestStoreCursor(t *testing.T) {
	p := newPlugin(t, nil)
	ctx := context.Background()
	if got := p.store.cursor(ctx, "reconcile:cursor"); got != "" {
		t.Errorf("unset cursor = %q", got)
	}
	_ = p.store.setCursor(ctx, "reconcile:cursor", "2026-09-05T12:00:00Z")
	if got := p.store.cursor(ctx, "reconcile:cursor"); got != "2026-09-05T12:00:00Z" {
		t.Errorf("cursor = %q", got)
	}
}
