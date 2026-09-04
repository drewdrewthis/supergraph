package core

import (
	"context"
	"log"
	"path/filepath"
	"sync"
)

// defaultRingCapacity is the per-plugin event-ring size when the supervisor opens a
// Store. Kept modest: the ring is a recent-events buffer, not the system of record.
const defaultRingCapacity = 10000

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

// Plugins returns a snapshot copy of the currently running plugins keyed by name, so
// the server can discover optional interfaces (e.g. HTTPRoutes) without core knowing
// any concrete plugin type. A plugin appears only after its Start goroutine launches.
func (sv *Supervisor) Plugins() map[string]Plugin {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	out := make(map[string]Plugin, len(sv.plugins))
	for n, p := range sv.plugins {
		out[n] = p
	}
	return out
}

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
	for _, name := range SortedFactoryNames(sv.factories) {
		sv.health.MarkStarting(name)

		plugin, err := sv.factories[name](sv.cfg.PluginConfigFor(name))
		if err != nil {
			log.Printf("core: build plugin %q failed: %v", name, err)
			sv.health.MarkStale(name)
			continue
		}

		// An optional CursorReporter surfaces the plugin's resume cursor in the
		// health snapshot without core knowing the plugin's concrete type.
		if cr, ok := plugin.(CursorReporter); ok {
			sv.health.SetCursorFunc(name, func() string { return cr.Cursor(ctx) })
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
			_ = store.Close()
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
			sv.stopDead(p.Name())
		}
	}()
	if err := p.Start(ctx, emit); err != nil {
		log.Printf("core: plugin %q Start returned error: %v", p.Name(), err)
		sv.stopDead(p.Name())
	}
}

// stopDead handles a plugin that has stopped running (panic or Start error): it is
// marked stale AND its canary is suppressed. Continuing to fire synthetic canary
// events for a dead plugin would advance its lastEventAt and mask the failure as ok
// — the exact false-green the PRD §6 canary rule warns against.
func (sv *Supervisor) stopDead(name string) {
	sv.health.MarkStale(name)
	sv.StopCanary(name)
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

// StopCanary suppresses the canary for one plugin. It is the PRODUCTION path for a
// dead plugin: stopDead calls it when a plugin panics or its Start returns, so core
// stops firing synthetic canary events that would otherwise advance the dead
// plugin's lastEventAt and mask the failure as ok. It is not a test-only hook.
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
