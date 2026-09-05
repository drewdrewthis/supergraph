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
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func init() { core.Register("peer", New) }

// active is the running plugin instance, published for the graph `peers` resolver
// to delegate through (the plugin never imports graph; graph reads this accessor).
var active atomic.Pointer[Plugin]

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
}

// New builds the plugin from its resolved config (EDR §"Config keys").
func New(cfg core.PluginConfig) (core.Plugin, error) {
	c := config{
		hostID:         cfg.HostID,
		staleThreshold: 30 * time.Second,
		backoffMax:     60 * time.Second,
		remotePlugin:   strOr(cfg.Raw, "remotePlugin", ""),
	}
	if n := numOr(cfg.Raw, "staleThresholdSeconds", 0); n > 0 {
		c.staleThreshold = time.Duration(n * float64(time.Second))
	}
	if n := numOr(cfg.Raw, "backoffMaxSeconds", 0); n > 0 {
		c.backoffMax = time.Duration(n * float64(time.Second))
	}
	c.peers = parsePeers(cfg.Raw)
	p := &Plugin{
		cfg: c,
		hc:  &http.Client{Timeout: 15 * time.Second},
		now: func() time.Time { return time.Now().UTC() },
	}
	active.Store(p)
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

// Routes mounts the pull-through executor under /plugins/peer/ (HTTPRoutes seam).
func (p *Plugin) Routes() map[string]http.Handler {
	return map[string]http.Handler{"graphql": http.HandlerFunc(p.handleGraphQL)}
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

// --- HTTP executor ---

// servedNode is one mirrored node on the wire, tagged with its origin host and
// mirror freshness.
type servedNode struct {
	Key        string          `json:"key"`
	Host       string          `json:"host"`
	LastSeenAt time.Time       `json:"lastSeenAt"`
	Node       json.RawMessage `json:"node"`
}

// servedResult is the JSON /plugins/peer/graphql returns: the host, its current
// staleSince (non-null ⇒ the peer is flagged stale but its rows are still served),
// and the mirrored nodes.
type servedResult struct {
	Host       string       `json:"host"`
	StaleSince *time.Time   `json:"staleSince"`
	Nodes      []servedNode `json:"nodes"`
}

// handleGraphQL serves POST {op, host, variables, refresh}. refresh=true proxies to
// the remote and refreshes the mirror; refresh=false is the warm path — a pure local
// read from peer_nodes with zero upstream call (S2). A stopped peer still serves its
// mirrored rows, flagged by staleSince (F8).
func (p *Plugin) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	var req struct {
		Op        string         `json:"op"`
		Host      string         `json:"host"`
		Variables map[string]any `json:"variables"`
		Refresh   bool           `json:"refresh"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")
	// Logged so a black-box step can prove a remote box's OWN peer executor is never
	// invoked by a mirroring consumer (AC-PEER-LOOP: no proxy to a remote's peer).
	log.Printf("peer: executor op=%q host=%q refresh=%t", req.Op, req.Host, req.Refresh)

	var nodes []mirroredNode
	if req.Refresh {
		// A proxy error (401, transport) degrades to stale rather than 500-ing the
		// read: the peer is marked stale and whatever was already cached is served.
		nodes, _ = p.mirror(ctx, req.Op, req.Host, req.Variables)
	} else {
		nodes, _ = p.store.nodesForHost(ctx, req.Host)
	}

	st, _ := p.store.state(ctx, req.Host)
	out := servedResult{Host: req.Host, StaleSince: st.StaleSince, Nodes: make([]servedNode, 0, len(nodes))}
	for _, n := range nodes {
		out.Nodes = append(out.Nodes, servedNode{Key: n.Key, Host: n.Host, LastSeenAt: n.LastSeenAt, Node: json.RawMessage(n.NodeJSON)})
	}
	_ = json.NewEncoder(w).Encode(out)
}

// maxBody caps the executor POST body: an op name, a host, and a few variables are
// all it needs, so 64 KiB is generous and bounds pre-decode memory.
const maxBody = 64 << 10

// --- graph resolver accessor ---

// PeerView is one peer's liveness for the graph `peers` resolver (a black-box view,
// so graph never imports the plugin's internal types).
type PeerView struct {
	HostID       string
	URL          string
	LastSeenAt   *time.Time
	StaleSince   *time.Time
	LagSeconds   float64
	MirroredKeys int
}

// Peers returns every configured peer's liveness snapshot. The graph `peers`
// resolver delegates here; a nil/unstarted plugin yields no peers rather than a
// panic.
func Peers(ctx context.Context) ([]PeerView, error) {
	p := active.Load()
	if p == nil || p.store == nil {
		return nil, nil
	}
	states, err := p.store.states(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PeerView, 0, len(states))
	for _, s := range states {
		n, _ := p.store.countForHost(ctx, s.Host)
		out = append(out, PeerView{
			HostID: s.Host, URL: s.URL, LastSeenAt: s.LastSeenAt,
			StaleSince: s.StaleSince, LagSeconds: s.LagSeconds, MirroredKeys: n,
		})
	}
	return out, nil
}

// --- config coercion (toml decodes to string/int64/float64/bool/[]any/map) ---

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
			HostID: strOr(m, "hostId", ""),
			URL:    strOr(m, "url", ""),
			Token:  strOr(m, "token", ""),
		}
		if pr.HostID != "" && pr.URL != "" {
			out = append(out, pr)
		}
	}
	return out
}

func strOr(raw map[string]any, k, def string) string {
	if raw != nil {
		if v, ok := raw[k].(string); ok && v != "" {
			return v
		}
	}
	return def
}

func numOr(raw map[string]any, k string, def float64) float64 {
	if raw == nil {
		return def
	}
	switch n := raw[k].(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	case int:
		return float64(n)
	}
	return def
}
