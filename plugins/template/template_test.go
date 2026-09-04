package template

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// newTestStore opens a core.Store backed by a temp file, closed automatically at
// test cleanup.
func newTestStore(t *testing.T) *core.Store {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "template.db")
	s, err := core.OpenStore(ctx, path, 100)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestMigrateCreatesTemplateStateTable(t *testing.T) {
	ctx := context.Background()
	store := newTestStore(t)

	p, err := New(core.PluginConfig{HostID: "h1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Migrate(ctx, store); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var name string
	err = store.DB().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='template_state'`,
	).Scan(&name)
	if err != nil {
		t.Fatalf("expected template_state table to exist: %v", err)
	}
	if name != "template_state" {
		t.Fatalf("got table %q, want template_state", name)
	}
}

func TestStartEmitsHelloWithinOneSecond(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := New(core.PluginConfig{HostID: "h1", Raw: map[string]any{"intervalSeconds": 60}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	captured := make(chan core.Envelope, 4)
	emit := func(ctx context.Context, e core.Envelope) error {
		captured <- e
		return nil
	}

	go p.Start(ctx, emit)

	select {
	case e := <-captured:
		if e.Type != "template.hello" {
			t.Fatalf("got type %q, want template.hello", e.Type)
		}
		if e.V != 2 {
			t.Fatalf("got V=%d, want 2", e.V)
		}
		if e.Key != "template:hello@h1" {
			t.Fatalf("got key %q, want template:hello@h1", e.Key)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("timed out waiting for hello event")
	}
}

func TestStartPanicsOnPanicInjectConfig(t *testing.T) {
	p, err := New(core.PluginConfig{HostID: "h1", Raw: map[string]any{"panic": true}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	emit := func(ctx context.Context, e core.Envelope) error { return nil }

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected Start to panic when panic-inject config is set")
			}
		}()
		_ = p.Start(context.Background(), emit)
	}()
}

func TestStartPanicsOnPanicInjectEnvVar(t *testing.T) {
	t.Setenv("SUPERGRAPH_TEMPLATE_PANIC", "1")

	p, err := New(core.PluginConfig{HostID: "h1"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	emit := func(ctx context.Context, e core.Envelope) error { return nil }

	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("expected Start to panic when SUPERGRAPH_TEMPLATE_PANIC=1")
			}
		}()
		_ = p.Start(context.Background(), emit)
	}()
}
