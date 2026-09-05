// Package peer is the multi-box federation plugin: a lazy pull-through mirror
// (EDR docs/edr/peer.md). It proxies named ops to a remote box's per-plugin
// executors, caches the returned nodes in its own SQLite keyed by their original
// @host key, serves them back tagged host + lastSeenAt, and tracks each remote's
// liveness with a WS-client subscription to the remote's base /graphql pluginLag
// stream. It needs ZERO core change and no peer plugin on the remote; a down box
// reads as stale-since-T, never empty (F8). All state lives in the plugin's own db.
package peer

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
)

func init() { core.Register("peer", New) }

// peerCfg is one configured mesh peer: its stable hostId, mesh URL, and bearer.
type peerCfg struct {
	HostID string
	URL    string
	Token  string
}

// config is the resolved [plugins.peer] section.
type config struct {
	hostID         string
	peers          []peerCfg
	staleThreshold time.Duration
	backoffMax     time.Duration
	// mirrorTTL evicts a host's mirrored rows not refreshed within it, on each proxy
	// call for that host (S2). 0 ⇒ never purge (serve until an explicit refresh).
	mirrorTTL time.Duration
	// remotePlugin, when set, forces every proxy at one remote executor. It is the
	// @local harness override that points the whole fan-out at the seeded fakeremote;
	// production leaves it empty and routes per op (mirror.go opPlugins).
	remotePlugin string
}

// Plugin is the peer plugin. One instance serves the HTTP executor and runs the
// per-peer liveness loops, so it holds the store, the outbound client, and emit.
type Plugin struct {
	cfg   config
	store *store
	hc    *http.Client

	emitMu sync.RWMutex
	emit   core.Emit

	now func() time.Time

	// subscribe and sleep are injectable seams so liveness backoff/transition logic
	// is unit-testable without a real socket or wall-clock (S5); New wires them to
	// the real subscribeLag and sleepCtx.
	subscribe func(ctx context.Context, pr peerCfg, threshold float64, onConnect func(), onLag func(float64)) error
	sleep     func(ctx context.Context, d time.Duration) bool
}

// New builds the plugin from its resolved config (EDR §"Config keys").
func New(cfg core.PluginConfig) (core.Plugin, error) {
	c := config{
		hostID:         cfg.HostID,
		staleThreshold: 30 * time.Second,
		backoffMax:     60 * time.Second,
		remotePlugin:   pluginconfig.Str(cfg.Raw, "remotePlugin", ""),
	}
	if n := pluginconfig.Num(cfg.Raw, "staleThresholdSeconds", 0); n > 0 {
		c.staleThreshold = time.Duration(n * float64(time.Second))
	}
	if n := pluginconfig.Num(cfg.Raw, "backoffMaxSeconds", 0); n > 0 {
		c.backoffMax = time.Duration(n * float64(time.Second))
	}
	if n := pluginconfig.Num(cfg.Raw, "mirrorTTLSeconds", 0); n > 0 {
		c.mirrorTTL = time.Duration(n * float64(time.Second))
	}
	c.peers = parsePeers(cfg.Raw)
	p := &Plugin{
		cfg:   c,
		hc:    &http.Client{Timeout: 15 * time.Second},
		now:   func() time.Time { return time.Now().UTC() },
		sleep: sleepCtx,
	}
	p.subscribe = p.subscribeLag
	active.Set(p)
	return p, nil
}

// Name is the stable plugin id (SQLite filename + health key).
func (p *Plugin) Name() string { return "peer" }

// Migrate creates the two state tables and seeds a peer_state row per configured
// peer so the peers query lists a peer before any liveness probe has run.
func (p *Plugin) Migrate(ctx context.Context, s *core.Store) error {
	p.store = &store{core: s}
	if err := p.store.migrate(ctx); err != nil {
		return err
	}
	for _, pr := range p.cfg.peers {
		if err := p.store.initState(ctx, pr.HostID, pr.URL); err != nil {
			return err
		}
	}
	return nil
}

// Start captures emit, then runs one liveness goroutine per peer until ctx is done.
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	p.emitMu.Lock()
	p.emit = emit
	p.emitMu.Unlock()

	var wg sync.WaitGroup
	for _, pr := range p.cfg.peers {
		wg.Add(1)
		go func(pr peerCfg) { defer wg.Done(); p.runLiveness(ctx, pr) }(pr)
	}
	wg.Wait()
	return nil
}

// Routes mounts the pull-through executor at /plugins/peer/op (HTTPRoutes seam). The
// route is "op", not "graphql": the body is a JSON {op,host,refresh} command, not a
// GraphQL document, so the honest path name says so.
func (p *Plugin) Routes() map[string]http.Handler {
	return map[string]http.Handler{"op": http.HandlerFunc(p.handleOp)}
}

// Cursor surfaces a peers=<n> stale=<m> summary in the health snapshot (optional
// core.CursorReporter). It is a cheap in-memory-ish count, never a network call.
func (p *Plugin) Cursor(ctx context.Context) string {
	if p.store == nil {
		return ""
	}
	states, err := p.store.states(ctx)
	if err != nil {
		return ""
	}
	stale := 0
	for _, s := range states {
		if s.StaleSince != nil {
			stale++
		}
	}
	return "peers=" + strconv.Itoa(len(states)) + " stale=" + strconv.Itoa(stale)
}

// doEmit sends one envelope if Start has captured emit, else drops it.
func (p *Plugin) doEmit(ctx context.Context, e core.Envelope) {
	p.emitMu.RLock()
	emit := p.emit
	p.emitMu.RUnlock()
	if emit != nil {
		_ = emit(ctx, e)
	}
}

// peerByHost returns the configured peer for host.
func (p *Plugin) peerByHost(host string) (peerCfg, bool) {
	for _, pr := range p.cfg.peers {
		if pr.HostID == host {
			return pr, true
		}
	}
	return peerCfg{}, false
}

func parsePeers(raw map[string]any) []peerCfg {
	if raw == nil {
		return nil
	}
	list, ok := raw["peers"].([]any)
	if !ok {
		return nil
	}
	var out []peerCfg
	for _, e := range list {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		pr := peerCfg{
			HostID: pluginconfig.Str(m, "hostId", ""),
			URL:    pluginconfig.Str(m, "url", ""),
			Token:  pluginconfig.Str(m, "token", ""),
		}
		if pr.HostID != "" && pr.URL != "" {
			out = append(out, pr)
		}
	}
	return out
}
