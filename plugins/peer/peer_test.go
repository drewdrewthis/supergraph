package peer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func TestHostOf(t *testing.T) {
	cases := []struct{ key, want string }{
		{"issue:o/r#5@boxB", "boxB"},
		{"tmux:s1@boxC", "boxC"},
		{"issue:o/r#5", ""},
		{"", ""},
		{"a@b@c", "c"},
	}
	for _, c := range cases {
		if got := hostOf(c.key); got != c.want {
			t.Errorf("hostOf(%q) = %q, want %q", c.key, got, c.want)
		}
	}
}

func TestTargetPlugin(t *testing.T) {
	cases := []struct {
		name, override, op, want string
	}{
		{"override wins", "fakeremote", "issue", "fakeremote"},
		{"mapped op", "", "freeSlots", "tmux"},
		{"mapped github", "", "issue", "github"},
		{"unmapped is empty (no hop)", "", "mystery", ""}, // S3: unknown op makes no hop
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := &Plugin{cfg: config{remotePlugin: c.override}}
			if got := p.targetPlugin(c.op); got != c.want {
				t.Errorf("targetPlugin(%q) override=%q = %q, want %q", c.op, c.override, got, c.want)
			}
		})
	}
}

func TestParsePeers(t *testing.T) {
	raw := map[string]any{
		"peers": []any{
			map[string]any{"hostId": "boxB", "url": "http://b", "token": "t"},
			map[string]any{"hostId": "boxC", "url": "http://c"},
			map[string]any{"hostId": "", "url": "http://x"}, // dropped: no hostId
			map[string]any{"url": "http://y"},               // dropped: no hostId
		},
	}
	got := parsePeers(raw)
	if len(got) != 2 {
		t.Fatalf("parsePeers len = %d, want 2 (%+v)", len(got), got)
	}
	if got[0] != (peerCfg{HostID: "boxB", URL: "http://b", Token: "t"}) {
		t.Errorf("peer[0] = %+v", got[0])
	}
	if got[1] != (peerCfg{HostID: "boxC", URL: "http://c"}) {
		t.Errorf("peer[1] = %+v", got[1])
	}
	if parsePeers(nil) != nil {
		t.Error("parsePeers(nil) should be nil")
	}
}

// newTestPlugin builds a plugin wired to a temp store, a fixed clock, and a captured
// emit — the injected seams the mirror/liveness logic reads.
func newTestPlugin(t *testing.T, raw map[string]any) (*Plugin, *[]core.Envelope) {
	t.Helper()
	ctx := context.Background()
	st, err := core.OpenStore(ctx, filepath.Join(t.TempDir(), "peer.db"), 100)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	pl, err := New(core.PluginConfig{HostID: "local", Raw: raw})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := pl.(*Plugin)
	if err := p.Migrate(ctx, st); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	p.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

	var mu sync.Mutex
	var emitted []core.Envelope
	p.emit = func(_ context.Context, e core.Envelope) error {
		mu.Lock()
		defer mu.Unlock()
		emitted = append(emitted, e)
		return nil
	}
	return p, &emitted
}

// fakeExecutor is a remote source executor returning a fixed node set.
func fakeExecutor(t *testing.T, nodes []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
	}))
}

func TestMirrorKeepsOwnHostDropsForeignAndLocal(t *testing.T) {
	srv := fakeExecutor(t, []map[string]any{
		{"key": "tmux:s1@boxB", "node": json.RawMessage(`{"s":"1"}`)},  // kept
		{"key": "tmux:s2@boxC", "node": json.RawMessage(`{"s":"2"}`)},  // dropped: foreign (peer-of-peer)
		{"key": "tmux:s3@local", "node": json.RawMessage(`{"s":"3"}`)}, // dropped: our own echo
	})
	defer srv.Close()

	raw := map[string]any{
		"remotePlugin": "src",
		"peers":        []any{map[string]any{"hostId": "boxB", "url": srv.URL}},
	}
	p, emitted := newTestPlugin(t, raw)

	kept, err := p.mirror(context.Background(), "tmux", "boxB", nil)
	if err != nil {
		t.Fatalf("mirror: %v", err)
	}
	if len(kept) != 1 || kept[0].Key != "tmux:s1@boxB" {
		t.Fatalf("kept = %+v, want only tmux:s1@boxB", kept)
	}
	keys, _ := p.store.nodesForHost(context.Background(), "boxB")
	if len(keys) != 1 || keys[0].Key != "tmux:s1@boxB" {
		t.Fatalf("stored = %+v, want only tmux:s1@boxB", keys)
	}
	// Exactly one re-emit, preserving the origin @host key (D4).
	if n := countType(*emitted, "peer.node.mirrored"); n != 1 {
		t.Fatalf("peer.node.mirrored emits = %d, want 1", n)
	}
	if (*emitted)[0].Key != "tmux:s1@boxB" {
		t.Errorf("emit key = %q, want tmux:s1@boxB", (*emitted)[0].Key)
	}
}

func TestMirrorUnknownHostNoHop(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []any{}})
	}))
	defer srv.Close()

	raw := map[string]any{"remotePlugin": "src", "peers": []any{map[string]any{"hostId": "boxB", "url": srv.URL}}}
	p, _ := newTestPlugin(t, raw)

	kept, err := p.mirror(context.Background(), "issue", "boxZ", nil)
	if err != nil {
		t.Fatalf("mirror unknown: %v", err)
	}
	if len(kept) != 0 {
		t.Errorf("unknown host kept %d rows, want 0", len(kept))
	}
	if hit {
		t.Error("unknown host made an outbound hop")
	}
}

func TestMirror401MarksStaleNoRows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	raw := map[string]any{"remotePlugin": "src", "peers": []any{map[string]any{"hostId": "boxB", "url": srv.URL, "token": "wrong"}}}
	p, _ := newTestPlugin(t, raw)

	_, err := p.mirror(context.Background(), "issue", "boxB", nil)
	var ua errUnauthorized
	if err == nil || !asUnauthorized(err, &ua) {
		t.Fatalf("mirror err = %v, want errUnauthorized", err)
	}
	rows, _ := p.store.nodesForHost(context.Background(), "boxB")
	if len(rows) != 0 {
		t.Errorf("stored %d rows on 401, want 0", len(rows))
	}
	st, _ := p.store.state(context.Background(), "boxB")
	if st.StaleSince == nil {
		t.Error("staleSince not set after 401")
	}
}

func TestStoreMarkStaleThenSeen(t *testing.T) {
	ctx := context.Background()
	p, _ := newTestPlugin(t, map[string]any{"peers": []any{map[string]any{"hostId": "boxB", "url": "http://b"}}})

	trans, _ := p.store.markStale(ctx, "boxB", p.now())
	if !trans {
		t.Fatal("first markStale should transition")
	}
	trans2, _ := p.store.markStale(ctx, "boxB", p.now().Add(time.Second))
	if trans2 {
		t.Error("second markStale should NOT transition (first-failure time preserved)")
	}
	st, _ := p.store.state(ctx, "boxB")
	if st.StaleSince == nil {
		t.Fatal("staleSince nil after markStale")
	}
	if err := p.store.markSeen(ctx, "boxB", p.now(), 1.5); err != nil {
		t.Fatal(err)
	}
	st, _ = p.store.state(ctx, "boxB")
	if st.StaleSince != nil {
		t.Error("markSeen did not clear staleSince")
	}
	if st.LastSeenAt == nil || st.LagSeconds != 1.5 {
		t.Errorf("markSeen state = %+v", st)
	}
}

// TestMirrorUnknownOpNoHop: an op no source plugin owns makes no outbound hop and
// mirrors nothing (S3), the same rule as an unknown host.
func TestMirrorUnknownOpNoHop(t *testing.T) {
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		_ = json.NewEncoder(w).Encode(map[string]any{"nodes": []any{}})
	}))
	defer srv.Close()

	// No remotePlugin override ⇒ targetPlugin routes per opPlugins; "mystery" is unmapped.
	p, _ := newTestPlugin(t, map[string]any{"peers": []any{map[string]any{"hostId": "boxB", "url": srv.URL}}})
	kept, err := p.mirror(context.Background(), "mystery", "boxB", nil)
	if err != nil {
		t.Fatalf("mirror unmapped op: %v", err)
	}
	if len(kept) != 0 {
		t.Errorf("unmapped op kept %d rows, want 0", len(kept))
	}
	if hit {
		t.Error("unmapped op made an outbound hop")
	}
}

// TestPostOpCapsNodes: a remote returning more than maxNodes is truncated, bounding
// consumer memory against a hostile/runaway peer (S1).
func TestPostOpCapsNodes(t *testing.T) {
	many := make([]map[string]any, maxNodes+10)
	for i := range many {
		many[i] = map[string]any{"key": fmt.Sprintf("k%d@boxB", i), "node": json.RawMessage(`{}`)}
	}
	srv := fakeExecutor(t, many)
	defer srv.Close()

	p, _ := newTestPlugin(t, map[string]any{"remotePlugin": "src", "peers": []any{map[string]any{"hostId": "boxB", "url": srv.URL}}})
	res, err := p.postOp(context.Background(), peerCfg{HostID: "boxB", URL: srv.URL}, "src", "op", nil)
	if err != nil {
		t.Fatalf("postOp: %v", err)
	}
	if len(res.Nodes) != maxNodes {
		t.Fatalf("postOp returned %d nodes, want capped to %d", len(res.Nodes), maxNodes)
	}
}

// TestMirrorTTLPurge: with mirrorTTL set, a proxy call evicts this host's rows not
// refreshed within the TTL, while the freshly-fetched rows survive (S2).
func TestMirrorTTLPurge(t *testing.T) {
	srv := fakeExecutor(t, []map[string]any{
		{"key": "tmux:s1@boxB", "node": json.RawMessage(`{"s":"1"}`)},
	})
	defer srv.Close()

	raw := map[string]any{
		"remotePlugin":     "src",
		"mirrorTTLSeconds": int64(60),
		"peers":            []any{map[string]any{"hostId": "boxB", "url": srv.URL}},
	}
	p, _ := newTestPlugin(t, raw)
	ctx := context.Background()

	// A boxB row last seen an hour before the fixed clock — older than the 60s TTL.
	stale := mirroredNode{Key: "tmux:old@boxB", Host: "boxB", NodeJSON: []byte(`{}`), LastSeenAt: p.now().Add(-time.Hour)}
	if err := p.store.upsertNode(ctx, stale); err != nil {
		t.Fatal(err)
	}
	if _, err := p.mirror(ctx, "tmux", "boxB", nil); err != nil {
		t.Fatalf("mirror: %v", err)
	}
	keys, _ := p.store.nodesForHost(ctx, "boxB")
	if len(keys) != 1 || keys[0].Key != "tmux:s1@boxB" {
		t.Fatalf("TTL purge left %+v, want only the fresh tmux:s1@boxB", keys)
	}
}

// TestLivenessBackoffAndTransition drives runLiveness with injected subscribe/sleep
// seams (S5): a never-up peer backs off 500ms→1s→2s(cap) and emits NO peer.stale,
// while an up→down peer emits exactly one.
func TestLivenessBackoffAndTransition(t *testing.T) {
	t.Run("never up: backoff doubles+caps, no stale emit", func(t *testing.T) {
		p, emitted := newTestPlugin(t, map[string]any{
			"backoffMaxSeconds": int64(2),
			"peers":             []any{map[string]any{"hostId": "boxB", "url": "http://b"}},
		})
		var sleeps []time.Duration
		ctx, cancel := context.WithCancel(context.Background())
		p.subscribe = func(context.Context, peerCfg, float64, func(), func(float64)) error {
			return errors.New("dial refused") // never onConnect ⇒ never up
		}
		p.sleep = func(_ context.Context, d time.Duration) bool {
			sleeps = append(sleeps, d)
			if len(sleeps) >= 4 {
				cancel()
				return false
			}
			return true
		}
		p.runLiveness(ctx, peerCfg{HostID: "boxB", URL: "http://b"})

		want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
		for i, w := range want {
			if i >= len(sleeps) || sleeps[i] != w {
				t.Fatalf("backoff = %v, want prefix %v", sleeps, want)
			}
		}
		if n := countType(*emitted, "peer.stale"); n != 0 {
			t.Errorf("never-up peer emitted %d peer.stale, want 0", n)
		}
	})

	t.Run("up then down emits one stale", func(t *testing.T) {
		p, emitted := newTestPlugin(t, map[string]any{"peers": []any{map[string]any{"hostId": "boxB", "url": "http://b"}}})
		ctx, cancel := context.WithCancel(context.Background())
		p.subscribe = func(_ context.Context, _ peerCfg, _ float64, onConnect func(), _ func(float64)) error {
			onConnect() // reachable, then drops
			return errors.New("dropped")
		}
		p.sleep = func(context.Context, time.Duration) bool { cancel(); return false }
		p.runLiveness(ctx, peerCfg{HostID: "boxB", URL: "http://b"})

		if n := countType(*emitted, "peer.stale"); n != 1 {
			t.Errorf("up→down emitted %d peer.stale, want 1", n)
		}
	})
}

func countType(evs []core.Envelope, typ string) int {
	n := 0
	for _, e := range evs {
		if e.Type == typ {
			n++
		}
	}
	return n
}

func asUnauthorized(err error, target *errUnauthorized) bool {
	if ua, ok := err.(errUnauthorized); ok {
		*target = ua
		return true
	}
	return false
}
