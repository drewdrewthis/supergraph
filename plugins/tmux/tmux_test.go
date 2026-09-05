package tmux

import (
	"context"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func TestNewDefaultsAndOverrides(t *testing.T) {
	p, err := New(core.PluginConfig{HostID: "h"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tp := p.(*Plugin)
	if tp.Name() != "tmux" {
		t.Fatalf("Name %q", tp.Name())
	}
	if tp.cfg.configured {
		t.Error("empty Raw must be dormant (not configured)")
	}
	if tp.cfg.eventSource != "control" || tp.cfg.reconcileInterval != 15*time.Second || tp.cfg.slotKind != "worker" {
		t.Errorf("defaults wrong: %+v", tp.cfg)
	}

	p2, _ := New(core.PluginConfig{HostID: "h", Raw: map[string]any{
		"socket": "sg-test", "eventSource": "poll", "reconcileIntervalSeconds": 2,
		"slotKind": "reviewer", "tmuxPath": "/stub/tmux", "idleShells": []any{"nu"},
	}})
	c := p2.(*Plugin).cfg
	if !c.configured || c.socket != "sg-test" || c.eventSource != "poll" || c.reconcileInterval != 2*time.Second ||
		c.slotKind != "reviewer" || c.tmuxPath != "/stub/tmux" || len(c.idleShells) != 1 || c.idleShells[0] != "nu" {
		t.Errorf("overrides wrong: %+v", c)
	}
}

func TestDormantStartReturnsOnCancel(t *testing.T) {
	p, _ := New(core.PluginConfig{HostID: "h"}) // no Raw -> dormant
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var emitted int
	go func() {
		done <- p.Start(ctx, func(context.Context, core.Envelope) error { emitted++; return nil })
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("dormant Start err: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dormant Start did not return on cancel")
	}
	if emitted != 0 {
		t.Fatalf("dormant plugin emitted %d events (must never touch the operator's server)", emitted)
	}
}

func TestTmuxArgsSocketSelector(t *testing.T) {
	name := &Plugin{cfg: config{socket: "sg-test"}}
	if got := name.tmuxArgs("list-panes"); got[0] != "-L" || got[1] != "sg-test" {
		t.Fatalf("name socket -> -L, got %v", got)
	}
	path := &Plugin{cfg: config{socket: "/tmp/sg.sock"}}
	if got := path.tmuxArgs("list-panes"); got[0] != "-S" || got[1] != "/tmp/sg.sock" {
		t.Fatalf("path socket -> -S, got %v", got)
	}
	none := &Plugin{cfg: config{}}
	if got := none.tmuxArgs("list-panes"); got[0] != "list-panes" {
		t.Fatalf("no socket -> no selector, got %v", got)
	}
}
