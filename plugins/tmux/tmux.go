// Package tmux is a read model of the LOCAL tmux server (EDR docs/edr/tmux.md). One
// long-lived control-mode client (`tmux -C attach`, stdin held open,
// refresh-client -f no-output) streams server-wide structural notifications into a
// per-plugin SQLite cache; a low-cadence reconcile poll (list-panes -a /
// list-sessions) backstops missed events, recomputes free/busy, and marks vanished
// entities staleSince. It serves TmuxSession/Pane + a Slot projection over the
// gqlgen extend-type glob seam — no HTTPRoutes, no webhook. All state lives in the
// plugin's own db; core is untouched (S5).
package tmux

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
	"github.com/drewdrewthis/supergraph/plugins/internal/single"
)

func init() { core.Register("tmux", New) }

// config is the resolved [plugins.tmux] section (EDR §"Config keys").
type config struct {
	socket              string
	eventSource         string // "control" (D1) | "poll"
	reconcileInterval   time.Duration
	idleShells          []string
	slotKind            string
	reconnectBackoffMax time.Duration
	minStableAttach     time.Duration // a control attach shorter than this is a failed/immediate exit (B2)
	tmuxPath            string
	configured          bool // false ⇒ no [plugins.tmux] section: stay dormant
}

// Plugin is the tmux plugin. It holds the store, the resolved config, the captured
// emit, and the injectable seams (clock, sleep, tmux exec, git branch) the unit
// tests replace with fakes.
type Plugin struct {
	cfg     config
	hostID  string
	version string
	store   *store
	trigger chan struct{}

	emitMu sync.RWMutex
	emitFn core.Emit

	now         func() time.Time
	sleep       func(context.Context, time.Duration)
	run         func(context.Context, ...string) ([]byte, error)
	branchOf    func(context.Context, string) string
	attach      func(context.Context) bool // one control-mode attach until exit; fake in tests (B1/B2)
	serverAlive func(context.Context) bool // liveness probe that never spawns a server (B2)
}

// active is the running plugin instance, published for graph/ resolvers; see plugins/internal/single.Ptr for the one-instance-per-process convention.
var active single.Ptr[Plugin]

func getCurrent() *Plugin { return active.Get() }

// New builds the plugin from its resolved config and registers it as the current
// instance for the graph resolvers.
func New(cfg core.PluginConfig) (core.Plugin, error) {
	c := config{
		eventSource:         "control",
		reconcileInterval:   15 * time.Second,
		idleShells:          []string{"zsh", "bash", "sh", "fish"},
		slotKind:            "worker",
		reconnectBackoffMax: 30 * time.Second,
		minStableAttach:     2 * time.Second,
		tmuxPath:            "tmux",
		configured:          len(cfg.Raw) > 0,
	}
	c.socket = pluginconfig.Str(cfg.Raw, "socket", c.socket)
	c.eventSource = pluginconfig.Str(cfg.Raw, "eventSource", c.eventSource)
	c.slotKind = pluginconfig.Str(cfg.Raw, "slotKind", c.slotKind)
	c.tmuxPath = pluginconfig.Str(cfg.Raw, "tmuxPath", c.tmuxPath)
	if n := pluginconfig.Int(cfg.Raw, "reconcileIntervalSeconds", 0); n > 0 {
		c.reconcileInterval = time.Duration(n) * time.Second
	}
	if n := pluginconfig.Int(cfg.Raw, "reconnectBackoffMaxSeconds", 0); n > 0 {
		c.reconnectBackoffMax = time.Duration(n) * time.Second
	}
	if xs := pluginconfig.Strs(cfg.Raw, "idleShells"); xs != nil {
		c.idleShells = xs
	}

	p := &Plugin{
		cfg:      c,
		hostID:   cfg.HostID,
		trigger:  make(chan struct{}, 1),
		now:      func() time.Time { return time.Now().UTC() },
		sleep:    sleepCtx,
		branchOf: gitBranch,
	}
	p.run = func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, p.cfg.tmuxPath, args...).Output() //nolint:gosec // G204: operator-configured tmux binary, not attacker input
	}
	p.attach = p.attachOnce
	p.serverAlive = p.probeAlive
	active.Set(p)
	return p, nil
}

// Name is the stable plugin id (SQLite filename + health key).
func (p *Plugin) Name() string { return "tmux" }

// Migrate creates the plugin's three state tables and stashes the store for the
// reconcile loop and the graph resolvers.
func (p *Plugin) Migrate(ctx context.Context, s *core.Store) error {
	p.store = &store{core: s}
	return p.store.migrate(ctx)
}

// Start captures emit, then runs the control-mode watcher (D1) and the reconcile
// loop until ctx is done. With no [plugins.tmux] section the plugin stays dormant —
// it never attaches to the operator's real server unless tmux is configured.
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	p.emitMu.Lock()
	p.emitFn = emit
	p.emitMu.Unlock()

	if !p.cfg.configured {
		<-ctx.Done()
		return nil
	}
	if out, err := p.run(ctx, "-V"); err == nil {
		p.version = string(out)
	}

	if p.cfg.eventSource == "control" {
		go p.watch(ctx)
	}

	_ = p.reconcile(ctx)
	ticker := time.NewTicker(p.cfg.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			_ = p.reconcile(ctx)
		case <-p.trigger:
			_ = p.reconcile(ctx)
		}
	}
}

// Cursor surfaces the reconcile high-water mark in the health snapshot (optional
// core.CursorReporter). It is a cheap single-row read of the persisted cursor.
func (p *Plugin) Cursor(ctx context.Context) string {
	if p.store == nil {
		return ""
	}
	v, _ := p.store.core.Cursor(ctx, "snapshot:lastAt")
	return v
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
