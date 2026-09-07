package core

import (
	"context"
	"testing"
	"time"
)

// shutdownPlugin is a test double whose Start signals once it is blocked on
// ctx.Done and then, before returning, sleeps for a configured settle duration to
// model a plugin reaping its children during shutdown. Run must not return until
// this Start returns.
type shutdownPlugin struct {
	name    string
	settle  time.Duration
	started chan struct{}
}

func (p *shutdownPlugin) Name() string                          { return p.name }
func (p *shutdownPlugin) Migrate(context.Context, *Store) error { return nil }

func (p *shutdownPlugin) Start(ctx context.Context, _ Emit) error {
	close(p.started)
	<-ctx.Done()
	time.Sleep(p.settle)
	return nil
}

func (p *shutdownPlugin) Health(context.Context) HealthStatus {
	return HealthStatus{Plugin: p.name}
}

// runSupervisor launches Run in a goroutine and blocks until the plugin's Start is
// actually running, so a test's cancel cannot race store-open (which would skip the
// plugin and never enter the WaitGroup, defeating the join being tested).
func runSupervisor(t *testing.T, p *shutdownPlugin) (cancel context.CancelFunc, done chan struct{}) {
	t.Helper()
	cfg := Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300}
	sv := NewSupervisor(cfg, map[string]Factory{p.Name(): factoryFor(p)},
		NewHealthAggregator(cfg.LagThresholdSeconds), NewBus())

	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() {
		defer close(done)
		sv.Run(ctx)
	}()
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("plugin Start never ran")
	}
	return cancel, done
}

func newShutdownPlugin(name string, settle time.Duration) *shutdownPlugin {
	return &shutdownPlugin{name: name, settle: settle, started: make(chan struct{})}
}

// Run must join a plugin's Start on shutdown: a plugin whose Start settles ~1s
// after ctx cancel forces Run to block for at least that long before returning,
// otherwise the process could exit before the plugin reaped its children
// (docs/plugin-contract.md, owner decision A).
func TestRunJoinsSlowPluginStart(t *testing.T) {
	cancel, done := runSupervisor(t, newShutdownPlugin("slow", time.Second))

	start := time.Now()
	cancel()
	<-done
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond {
		t.Fatalf("Run returned after %v, expected it to join the ~1s plugin Start", elapsed)
	}
}

// A plugin whose Start returns promptly on ctx cancel lets Run return promptly:
// the join adds no latency of its own.
func TestRunReturnsPromptlyOnFastPlugin(t *testing.T) {
	cancel, done := runSupervisor(t, newShutdownPlugin("fast", 0))

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return promptly for a fast-exiting plugin")
	}
}
