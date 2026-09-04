package github

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// newPlugin builds a *Plugin from raw config, migrates a throwaway SQLite store, and
// installs a deterministic clock + no-op sleep so tests never touch real time.
func newPlugin(t *testing.T, raw map[string]any) *Plugin {
	t.Helper()
	pl, err := New(core.PluginConfig{Raw: raw})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p := pl.(*Plugin)
	st, err := core.OpenStore(context.Background(), filepath.Join(t.TempDir(), "gh.db"), 128)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := p.Migrate(context.Background(), st); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	fixed := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return fixed }
	p.sleep = func(context.Context, time.Duration) {}
	return p
}

// recorder captures emitted envelopes for assertions.
type recorder struct {
	mu sync.Mutex
	ev []core.Envelope
}

func (r *recorder) emit(_ context.Context, e core.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ev = append(r.ev, e)
	return nil
}

func (r *recorder) events() []core.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]core.Envelope, len(r.ev))
	copy(out, r.ev)
	return out
}

// install wires the recorder as the plugin's emit sink.
func (r *recorder) install(p *Plugin) {
	p.emitMu.Lock()
	p.emit = r.emit
	p.emitMu.Unlock()
}
