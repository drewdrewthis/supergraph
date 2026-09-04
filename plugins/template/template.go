// Package template is the reference plugin (PRD §6 Step 12): it proves the
// zero-core-edit contract (AC-CORE-10) that every future plugin (tmux, claude,
// github, peer, telegram) follows. It has no real data source — it just emits a
// hello event then a tick on an interval — so it exercises the full Plugin
// lifecycle (Register, Migrate, Start, Health) without any external dependency.
package template

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// defaultIntervalSeconds is the tick cadence when cfg.Raw omits intervalSeconds.
const defaultIntervalSeconds = 30

func init() {
	core.Register("template", New)
}

// Plugin is the template plugin's Plugin implementation.
type Plugin struct {
	hostID   string
	interval time.Duration
	panic    bool

	mu       sync.Mutex
	count    int
	lastEmit time.Time
}

// New builds the template plugin from its resolved config. It is registered as
// the "template" factory in init().
func New(cfg core.PluginConfig) (core.Plugin, error) {
	interval := defaultIntervalSeconds * time.Second
	if v, ok := cfg.Raw["intervalSeconds"]; ok {
		switch n := v.(type) {
		case int:
			interval = time.Duration(n) * time.Second
		case int64:
			interval = time.Duration(n) * time.Second
		case float64:
			interval = time.Duration(n) * time.Second
		}
	}

	panicInject := os.Getenv("SUPERGRAPH_TEMPLATE_PANIC") == "1"
	if v, ok := cfg.Raw["panic"]; ok {
		if b, ok := v.(bool); ok && b {
			panicInject = true
		}
	}

	return &Plugin{
		hostID:   cfg.HostID,
		interval: interval,
		panic:    panicInject,
	}, nil
}

// Name returns the plugin's stable id.
func (p *Plugin) Name() string { return "template" }

// Migrate creates the plugin's own state table, idempotently.
func (p *Plugin) Migrate(ctx context.Context, s *Store) error {
	return migrate(ctx, s.DB())
}

// migrate is the storage-agnostic half of Migrate, taking a *sql.DB directly so
// the unit test can exercise it without depending on core.Store's constructor.
func migrate(ctx context.Context, db *sql.DB) error {
	const schema = `CREATE TABLE IF NOT EXISTS template_state (
	key   TEXT PRIMARY KEY,
	value TEXT
);`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("template: migrate: %w", err)
	}
	return nil
}

// Store is a type alias for core.Store, kept local so Migrate's signature reads
// naturally without importing core.Store at every call site in this file.
type Store = core.Store

// Start emits one "template.hello" event immediately, then a "template.tick"
// event every interval with an incrementing n, until ctx is done. When the
// panic-inject path is armed (AC-CORE-4), it panics right after the first emit
// so the supervisor's recover-guard can be exercised end to end.
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	if err := p.emit(ctx, emit, "template.hello", "hello"); err != nil {
		return err
	}

	if p.panic {
		panic("template: induced panic")
	}

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.emit(ctx, emit, "template.tick", "tick"); err != nil {
				return err
			}
		}
	}
}

// emit builds and sends one envelope, bumping the plugin's own emit count/cursor
// used by Health.
func (p *Plugin) emit(ctx context.Context, emit core.Emit, eventType, msg string) error {
	p.mu.Lock()
	p.count++
	n := p.count
	now := time.Now().UTC()
	p.lastEmit = now
	p.mu.Unlock()

	payload, err := json.Marshal(map[string]any{"msg": msg, "n": n})
	if err != nil {
		return fmt.Errorf("template: marshal payload: %w", err)
	}

	return emit(ctx, core.Envelope{
		TS:      now,
		Source:  "template",
		Type:    eventType,
		V:       2,
		Key:     "template:hello@" + p.hostID,
		Payload: payload,
	})
}

// Health returns a freshness snapshot: the last emit time and a cursor naming the
// running emit count.
func (p *Plugin) Health(_ context.Context) core.HealthStatus {
	p.mu.Lock()
	defer p.mu.Unlock()

	status := core.HealthStatus{
		Plugin: "template",
		Cursor: fmt.Sprintf("n=%d", p.count),
		State:  core.HealthStarting,
	}
	if !p.lastEmit.IsZero() {
		t := p.lastEmit
		status.LastEventAt = &t
		status.LagSeconds = time.Since(p.lastEmit).Seconds()
		status.State = core.HealthOK
	}
	return status
}
