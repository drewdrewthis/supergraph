// Package claude turns running Claude Code sessions into graph rows. It ingests
// session state from two channels (EDR docs/edr/claude.md): lifecycle HOOKS POSTed to
// /plugins/claude/hook (sub-ms live state, the only source of TMUX_PANE and
// permission-input state) and a poll-scan TAIL of ~/.claude/projects/*/<sid>.jsonl
// (the correctness floor that backfills pre-install sessions and enriches with
// gitBranch/model/PR the hook lacks). Only structural METADATA is ever STORED or
// EMITTED — prompt, response, and tool_input bodies are dropped at the decode boundary
// (redact.go whitelist), so nothing that persists or leaves the box carries them. A raw
// hook body does transit the local forwarder's POST request body before that decode;
// note too that a stored `cwd` is a filesystem path and can embed the OS username. All
// state lives in the plugin's own SQLite db; core is untouched.
package claude

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
	"github.com/drewdrewthis/supergraph/plugins/internal/single"
)

func init() { core.Register("claude", New) }

// live is the running plugin instance, published for graph/ resolvers; see plugins/internal/single.Ptr for the one-instance-per-process convention.
var live single.Ptr[Plugin]

// Plugin is the claude plugin: one instance serves the hook HTTP route and the tail
// loop, so it holds every shared dependency (store, captured emit, injected clock/fs).
type Plugin struct {
	hostID        string
	projectsDir   string
	settingsPath  string
	scanInterval  time.Duration
	retentionDays int
	pidLiveness   bool
	panicInject   bool

	store *store
	alive func(pid int) bool
	now   func() time.Time

	emitMu sync.RWMutex
	emitFn core.Emit
}

// New builds the plugin from its resolved [plugins.claude] config.
func New(cfg core.PluginConfig) (core.Plugin, error) {
	home, _ := os.UserHomeDir()
	p := &Plugin{
		hostID:        cfg.HostID,
		projectsDir:   filepath.Join(home, ".claude", "projects"),
		settingsPath:  filepath.Join(home, ".claude", "settings.json"),
		scanInterval:  5 * time.Second,
		retentionDays: 30,
		pidLiveness:   true,
		alive:         realAlive,
		now:           func() time.Time { return time.Now().UTC() },
	}
	raw := cfg.Raw
	p.projectsDir = pluginconfig.Str(raw, "projectsDir", p.projectsDir)
	p.settingsPath = pluginconfig.Str(raw, "settingsPath", p.settingsPath)
	p.pidLiveness = pluginconfig.Bool(raw, "pidLiveness", p.pidLiveness)
	p.retentionDays = pluginconfig.Int(raw, "retentionDays", p.retentionDays)
	if n := pluginconfig.Int(raw, "scanIntervalSeconds", 0); n > 0 {
		p.scanInterval = time.Duration(n) * time.Second
	}
	p.panicInject = pluginconfig.Bool(raw, "panic", os.Getenv("SUPERGRAPH_CLAUDE_PANIC") == "1")
	return p, nil
}

// Name is the stable plugin id (SQLite filename + health key).
func (p *Plugin) Name() string { return "claude" }

// Migrate creates the session table and publishes this instance for the resolvers.
func (p *Plugin) Migrate(ctx context.Context, s *core.Store) error {
	p.store = &store{core: s}
	if err := p.store.migrate(ctx); err != nil {
		return err
	}
	live.Set(p)
	return nil
}

// Start captures emit then runs the tail loop until ctx is done. The panic-inject
// path (F5) fires here so core's recover-guard can mark claude stale end to end.
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	p.emitMu.Lock()
	p.emitFn = emit
	p.emitMu.Unlock()
	if p.panicInject {
		panic("claude: induced panic")
	}
	p.runTail(ctx)
	return nil
}

// Routes mounts the hook receiver under /plugins/claude/hook (core dispatches
// per-request). Auth for a non-loopback bind is core's bearer middleware, upstream.
func (p *Plugin) Routes() map[string]http.Handler {
	return map[string]http.Handler{"hook": http.HandlerFunc(p.handleHook)}
}

// Cursor surfaces the number of tracked sessions in the health snapshot (optional
// core.CursorReporter); it is an in-memory-cheap count.
func (p *Plugin) Cursor(ctx context.Context) string {
	if p.store == nil {
		return ""
	}
	rows, _ := p.store.list(ctx, nil, nil)
	return "sessions=" + strconv.Itoa(len(rows))
}

// emit builds and sends one metadata-only envelope keyed to the session/instance.
func (p *Plugin) emit(ctx context.Context, typ, key string, payload map[string]any) {
	b, _ := json.Marshal(payload)
	p.doEmit(ctx, core.Envelope{TS: p.now(), Source: "claude", Type: typ, V: 1, Key: key, Payload: b})
}

// emitSessionUpdated emits claude.session.updated with the row's whitelisted fields.
// It is the join surface F1/S4 and tmux/github read; it carries NO body text.
func (p *Plugin) emitSessionUpdated(ctx context.Context, sid string) {
	r, err := p.store.get(ctx, sid)
	if err != nil || r == nil {
		return
	}
	p.emit(ctx, "claude.session.updated", sessionKey(sid, p.hostID), map[string]any{
		"sid": sid, "state": r.State, "cwd": r.Cwd, "gitBranch": r.GitBranch,
		"issueNumber": r.IssueNumber, "model": r.Model, "lastTool": r.LastTool, "toolCalls": r.ToolCalls,
	})
}

func (p *Plugin) doEmit(ctx context.Context, e core.Envelope) {
	p.emitMu.RLock()
	emit := p.emitFn
	p.emitMu.RUnlock()
	if emit != nil {
		_ = emit(ctx, e)
	}
}

// --- package query funcs (called by graph/claude.resolvers.go through `live`) ---

// QuerySessions returns sessions optionally filtered by host and issue number.
func QuerySessions(ctx context.Context, host *string, issue *int) []SessionRow {
	p := live.Get()
	if p == nil || p.store == nil {
		return nil
	}
	rows, _ := p.store.list(ctx, host, issue)
	return rows
}

// QuerySession returns one session by id, or nil.
func QuerySession(ctx context.Context, sid string) *SessionRow {
	p := live.Get()
	if p == nil || p.store == nil {
		return nil
	}
	r, _ := p.store.get(ctx, sid)
	return r
}

// QueryInstances returns the ClaudeInstance projection (rows with a pane).
func QueryInstances(ctx context.Context, host *string) []SessionRow {
	p := live.Get()
	if p == nil || p.store == nil {
		return nil
	}
	rows, _ := p.store.instances(ctx, host)
	return rows
}
