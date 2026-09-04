package core

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recorder captures ordered call events from fake plugins for ordering assertions.
type recorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.seen = append(r.seen, s)
	r.mu.Unlock()
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// fakePlugin is a test double: it records Migrate/Start ordering, optionally panics
// in Start, and (when it does not panic) emits one seed event then blocks on ctx.
type fakePlugin struct {
	name     string
	doPanic  bool
	rec      *recorder
	emitted  chan struct{}
	emitOnce sync.Once
}

func (f *fakePlugin) Name() string { return f.name }

func (f *fakePlugin) Migrate(_ context.Context, _ *Store) error {
	f.rec.add(f.name + ":migrate")
	return nil
}

func (f *fakePlugin) Start(ctx context.Context, emit Emit) error {
	f.rec.add(f.name + ":start")
	if f.doPanic {
		panic("boom in " + f.name)
	}
	_ = emit(ctx, Envelope{TS: time.Now(), Source: f.name, Type: "seed", V: 1, Key: "k"})
	f.emitOnce.Do(func() { close(f.emitted) })
	<-ctx.Done()
	return nil
}

func (f *fakePlugin) Health(_ context.Context) HealthStatus {
	return HealthStatus{Plugin: f.name}
}

func newFake(name string, doPanic bool, rec *recorder) *fakePlugin {
	return &fakePlugin{name: name, doPanic: doPanic, rec: rec, emitted: make(chan struct{})}
}

func factoryFor(p Plugin) Factory {
	return func(PluginConfig) (Plugin, error) { return p, nil }
}

// AC-CORE-4/AC-CORE-13: a plugin that panics in Start goes stale while its sibling
// stays ok, Snapshot still works, the process does not exit, and Migrate ran before
// Start for each plugin.
func TestSupervisorPanicIsolationAndMigrateOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &recorder{}
	okPlug := newFake("fakeok", false, rec)
	badPlug := newFake("template", true, rec)
	cfg := Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300}

	h := NewHealthAggregator(cfg.LagThresholdSeconds)
	sv := NewSupervisor(cfg, map[string]Factory{
		"fakeok":   factoryFor(okPlug),
		"template": factoryFor(badPlug),
	}, h, NewBus())

	sv.startAll(ctx)
	defer sv.Stop()

	select {
	case <-okPlug.emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("fakeok never emitted its seed event")
	}
	waitForState(t, h, "template", HealthStale)

	byName := snapByName(t, h)
	if byName["template"].State != HealthStale {
		t.Errorf("panicking plugin state = %s, want stale", byName["template"].State)
	}
	if byName["fakeok"].State != HealthOK {
		t.Errorf("sibling plugin state = %s, want ok", byName["fakeok"].State)
	}

	order := rec.list()
	assertOrder(t, order, "fakeok:migrate", "fakeok:start")
	assertOrder(t, order, "template:migrate", "template:start")
}

func waitForState(t *testing.T, h *HealthAggregator, name string, want HealthState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if snapByName(t, h)[name].State == want {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("plugin %q never reached %s (last: %+v)", name, want, snapByName(t, h)[name])
}

func indexOf(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}

func assertOrder(t *testing.T, seq []string, before, after string) {
	t.Helper()
	bi, ai := indexOf(seq, before), indexOf(seq, after)
	if bi < 0 || ai < 0 {
		t.Fatalf("missing %q(%d) or %q(%d) in %v", before, bi, after, ai, seq)
	}
	if bi > ai {
		t.Fatalf("%q came after %q: %v", before, after, seq)
	}
}
