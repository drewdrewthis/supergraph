package core

import (
	"context"
	"log"
	"path/filepath"
	"sort"
	"sync"
)

// defaultRingCapacity is the per-plugin event-ring size when the supervisor opens a
// Store. Kept modest: the ring is a recent-events buffer, not the system of record.
const defaultRingCapacity = 10000

// subBuffer is the per-subscriber channel depth on the Bus. A subscriber that falls
// this far behind starts losing the oldest events rather than stalling the emit path.
const subBuffer = 256

// Bus fans out emitted envelopes to live subscribers. Each subscriber gets its own
// buffered channel; a full channel drops the event for THAT subscriber only (select
// with a default) so one slow consumer can never block AppendEvent, the canary, or a
// sibling subscriber. Dropping is acceptable here because subscriptions are a live
// tail, not a durable log — the Store is the durable record.
type Bus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Envelope
}

// NewBus builds an empty Bus.
func NewBus() *Bus {
	return &Bus{subs: map[int]chan Envelope{}}
}

// Subscribe returns a channel of future envelopes. The subscription lives until ctx
// is cancelled, at which point the channel is removed and closed so ranging callers
// terminate cleanly.
func (b *Bus) Subscribe(ctx context.Context) <-chan Envelope {
	ch := make(chan Envelope, subBuffer)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, id)
		close(ch)
		b.mu.Unlock()
	}()
	return ch
}

// publish delivers e to every subscriber, dropping for any whose buffer is full.
func (b *Bus) publish(e Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop rather than block the shared emit path
		}
	}
}

// Supervisor builds every registered plugin, gives each its own SQLite Store and a
// recover-guarded goroutine, and owns the shared emit path (Store append -> health
// Record -> Bus fan-out). A panic in one plugin's Start marks only that plugin stale
// and never touches the process or its siblings (AC-CORE-4).
type Supervisor struct {
	cfg          Config
	factories    map[string]Factory
	health       *HealthAggregator
	bus          *Bus
	ringCapacity int

	mu      sync.Mutex
	stores  map[string]*Store
	plugins map[string]Plugin
	stopped map[string]bool // plugins whose canary is suppressed
}

// NewSupervisor wires a supervisor over an explicit factory map (production passes
// core.Factories(); tests pass their own), a health aggregator, and a bus. Taking the
// factories as an argument rather than reading the global registry keeps the
// supervisor testable without polluting the process-wide registry.
func NewSupervisor(cfg Config, factories map[string]Factory, health *HealthAggregator, bus *Bus) *Supervisor {
	return &Supervisor{
		cfg:          cfg,
		factories:    factories,
		health:       health,
		bus:          bus,
		ringCapacity: defaultRingCapacity,
		stores:       map[string]*Store{},
		plugins:      map[string]Plugin{},
		stopped:      map[string]bool{},
	}
}

// Health exposes the aggregator so callers can mount /health.
func (sv *Supervisor) Health() *HealthAggregator { return sv.health }

// Bus exposes the fan-out bus so callers can wire subscriptions.
func (sv *Supervisor) Bus() *Bus { return sv.bus }

// emitFor builds the emit closure a plugin (and the canary) uses: persist to the
// plugin's ring, stamp health freshness, then fan out to subscribers. It is bound to
// one plugin's name and Store so the write always lands in the right db.
func (sv *Supervisor) emitFor(name string, store *Store) Emit {
	return func(ctx context.Context, e Envelope) error {
		if err := store.AppendEvent(ctx, e); err != nil {
			return err
		}
		sv.health.Record(name, e.TS)
		sv.bus.publish(e)
		return nil
	}
}

// startAll builds every plugin and launches its Start goroutine. It runs Migrate
// synchronously BEFORE launching Start (AC-CORE-13): because the goroutine is
// spawned only after Migrate returns, Start can never observe an unmigrated store.
// A build or Migrate failure marks that plugin stale and skips it without aborting
// the others. Exposed (unexported) for tests that need plugins running without the
// blocking Run loop.
func (sv *Supervisor) startAll(ctx context.Context) {
	for _, name := range sortedKeys(sv.factories) {
		sv.health.MarkStarting(name)

		plugin, err := sv.factories[name](sv.cfg.PluginConfigFor(name))
		if err != nil {
			log.Printf("core: build plugin %q failed: %v", name, err)
			sv.health.MarkStale(name)
			continue
		}

		store, err := OpenStore(ctx, filepath.Join(sv.cfg.DataDir, name+".db"), sv.ringCapacity)
		if err != nil {
			log.Printf("core: open store for %q failed: %v", name, err)
			sv.health.MarkStale(name)
			continue
		}

		if err := plugin.Migrate(ctx, store); err != nil {
			log.Printf("core: migrate %q failed: %v", name, err)
			sv.health.MarkStale(name)
			store.Close()
			continue
		}

		sv.mu.Lock()
		sv.stores[name] = store
		sv.plugins[name] = plugin
		sv.mu.Unlock()

		emit := sv.emitFor(name, store)
		go sv.runPlugin(ctx, plugin, emit)
	}
}

// runPlugin runs one plugin's Start under recover(). A panic is contained here: it
// marks the plugin stale and is logged, but never propagates to crash the process
// or affect a sibling goroutine.
func (sv *Supervisor) runPlugin(ctx context.Context, p Plugin, emit Emit) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("core: plugin %q panicked in Start: %v", p.Name(), r)
			sv.health.MarkStale(p.Name())
		}
	}()
	if err := p.Start(ctx, emit); err != nil {
		log.Printf("core: plugin %q Start returned error: %v", p.Name(), err)
	}
}

// Run starts every plugin, blocks until ctx is cancelled, then closes all stores.
func (sv *Supervisor) Run(ctx context.Context) {
	sv.startAll(ctx)
	<-ctx.Done()
	sv.Stop()
}

// Stop closes every open Store. Safe to call once after Run's context is done.
func (sv *Supervisor) Stop() {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	for name, s := range sv.stores {
		if err := s.Close(); err != nil {
			log.Printf("core: close store %q: %v", name, err)
		}
	}
}

// StopCanary suppresses the canary for one plugin. Used by the panic-inject / test
// path to simulate a plugin that has stopped emitting, so it can be observed
// crossing the lag threshold into stale.
func (sv *Supervisor) StopCanary(name string) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.stopped[name] = true
}

// canaryTargets is the set of running plugins whose canary is not suppressed.
func (sv *Supervisor) canaryTargets() []string {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := make([]string, 0, len(sv.plugins))
	for name := range sv.plugins {
		if !sv.stopped[name] {
			out = append(out, name)
		}
	}
	return out
}

// fireCanary emits one synthetic envelope for name through the real emit path, so it
// lands in the ring, updates health, and reaches subscribers exactly like a real
// event. A suppressed or not-yet-running plugin is a no-op.
func (sv *Supervisor) fireCanary(ctx context.Context, name string) error {
	sv.mu.Lock()
	store := sv.stores[name]
	suppressed := sv.stopped[name]
	sv.mu.Unlock()
	if store == nil || suppressed {
		return nil
	}
	e := Envelope{
		TS:     sv.health.now(),
		Source: name,
		Type:   "canary",
		V:      1,
		Key:    "canary:" + name,
	}
	return sv.emitFor(name, store)(ctx, e)
}

// sortedKeys returns a factory map's names in stable order for deterministic startup.
func sortedKeys(m map[string]Factory) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
