// Package github is the event-invalidated caching proxy in front of GitHub's REST
// and GraphQL APIs (EDR docs/edr/github.md). It is not a model of GitHub: it fetches
// on miss, caches parsed JSON nodes keyed by object id, honours ETag/304, pins
// immutable nodes, and lets webhook events purge exactly the entries that touch a
// changed object. All state lives in the plugin's own SQLite db; core is untouched.
package github

import (
	"context"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
	"github.com/drewdrewthis/supergraph/plugins/internal/single"
)

// current is the running plugin instance, published in Migrate so the graph query
// resolvers (graph/github_map.go) can read cached nodes through the exported
// package funcs in query.go — via the shared single.Ptr seam claude's `live` and
// tmux use. No plugin imports another: the cross-plugin join lives in graph/ (D2).
var current single.Ptr[Plugin]

func init() { core.Register("github", New) }

// pinConfig is the immutable-node policy (EDR §"Immutable pin list").
type pinConfig struct {
	commits, releases     bool
	mergedPRsAfterDays    int
	closedIssuesAfterDays int
}

// config is the resolved [plugins.github] section.
type config struct {
	token             string
	baseURL           string // REST base; test-only override points at fakegh
	graphqlURL        string // GraphQL endpoint; test-only override points at fakegh
	ingress           string // "forward" (default) | "tunnel"
	webhookSecret     string
	ghPath            string   // gh binary; tests point at the stub
	selfURL           string   // this box's own base URL, for hook creation
	forwardRepo       string   // `gh webhook forward --repo owner/repo` target
	forwardOrg        string   // `gh webhook forward --org name` target (used if forwardRepo unset)
	forwardEvents     []string // event names passed to `gh webhook forward --events`
	hookRepos         []string // owner/repo allowlist for hook creation; empty ⇒ create none
	reconcileInterval time.Duration
	notifications     bool
	pin               pinConfig
	ttl               map[string]time.Duration // per-kind expiry; empty ⇒ off
}

// Plugin is the github plugin. One instance serves both the HTTP surface
// (read/executor/webhook) and the background loops (ingest, reconcile), so it holds
// every shared dependency: the store, the HTTP client bits, and the captured emit.
type Plugin struct {
	cfg   config
	store *store
	hc    *http.Client
	ops   map[string]namedOp // loaded queries/*.graphql

	flightMu sync.Mutex
	flights  map[string]*flight // singleflight in-flight fetches

	emitMu sync.RWMutex
	emit   core.Emit

	now   func() time.Time
	sleep func(context.Context, time.Duration)

	hookSkipOnce sync.Once // gates the one-time "hookRepos empty" reconcile log
}

// New builds the plugin from its resolved config (EDR §"Config keys").
func New(cfg core.PluginConfig) (core.Plugin, error) {
	c := config{
		baseURL:           "https://api.github.com",
		graphqlURL:        "https://api.github.com/graphql",
		ingress:           "forward",
		ghPath:            "gh",
		reconcileInterval: time.Hour,
		selfURL:           "http://127.0.0.1:7788",
		forwardEvents:     defaultForwardEvents,
		ttl:               map[string]time.Duration{},
		pin: pinConfig{
			commits: true, releases: true,
			mergedPRsAfterDays: 7, closedIssuesAfterDays: 30,
		},
	}
	raw := cfg.Raw
	c.token = pluginconfig.Str(raw, "token", os.Getenv("GITHUB_TOKEN"))
	c.baseURL = pluginconfig.Str(raw, "baseURL", c.baseURL)
	c.graphqlURL = pluginconfig.Str(raw, "graphqlURL", c.graphqlURL)
	c.ingress = pluginconfig.Str(raw, "ingress", c.ingress)
	c.webhookSecret = pluginconfig.Str(raw, "webhookSecret", c.webhookSecret)
	c.ghPath = pluginconfig.Str(raw, "ghPath", c.ghPath)
	// EDR names this key tunnelURL (the box's externally reachable base under
	// ingress=tunnel); selfURL stays accepted as an alias.
	c.selfURL = pluginconfig.Str(raw, "tunnelURL", pluginconfig.Str(raw, "selfURL", c.selfURL))
	c.forwardRepo = pluginconfig.Str(raw, "forwardRepo", c.forwardRepo)
	c.forwardOrg = pluginconfig.Str(raw, "forwardOrg", c.forwardOrg)
	if ev := pluginconfig.Strs(raw, "forwardEvents"); len(ev) > 0 {
		c.forwardEvents = ev
	}
	c.hookRepos = pluginconfig.Strs(raw, "hookRepos")
	c.notifications = pluginconfig.Bool(raw, "notifications", c.notifications)
	if n := pluginconfig.Int(raw, "reconcileIntervalSeconds", 0); n > 0 {
		c.reconcileInterval = time.Duration(n) * time.Second
	}
	if pin, ok := subMap(raw, "pin"); ok {
		c.pin.commits = pluginconfig.Bool(pin, "commits", c.pin.commits)
		c.pin.releases = pluginconfig.Bool(pin, "releases", c.pin.releases)
		c.pin.mergedPRsAfterDays = pluginconfig.Int(pin, "mergedPRsAfterDays", c.pin.mergedPRsAfterDays)
		c.pin.closedIssuesAfterDays = pluginconfig.Int(pin, "closedIssuesAfterDays", c.pin.closedIssuesAfterDays)
	}
	if ttl, ok := subMap(raw, "ttl"); ok {
		for kind, v := range ttl {
			if n := pluginconfig.ToInt(v); n > 0 {
				c.ttl[kind] = time.Duration(n) * time.Second
			}
		}
	}

	return &Plugin{
		cfg:     c,
		hc:      &http.Client{Timeout: 15 * time.Second},
		ops:     loadOps(),
		flights: map[string]*flight{},
		now:     func() time.Time { return time.Now().UTC() },
		sleep:   sleepCtx,
	}, nil
}

// Name is the stable plugin id (SQLite filename + health key).
func (p *Plugin) Name() string { return "github" }

// Migrate creates the plugin's four state tables and stashes the store for the HTTP
// handlers and loops (which never receive a Store directly).
func (p *Plugin) Migrate(ctx context.Context, s *core.Store) error {
	p.store = &store{core: s}
	if err := p.store.migrate(ctx); err != nil {
		return err
	}
	current.Set(p)
	return nil
}

// Start captures the emit closure (the only source of it — HTTP handlers read it
// back under lock) then runs the ingest and reconcile loops until ctx is done. With
// no resolved token it stays dormant: log once, spawn nothing (no forward
// supervisor spamming "exited: exit status 1" every restart).
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	if p.cfg.token == "" {
		log.Printf("github: not configured (no [plugins.github] token); dormant")
		return nil
	}

	p.emitMu.Lock()
	p.emit = emit
	p.emitMu.Unlock()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); p.runIngest(ctx) }()
	go func() { defer wg.Done(); p.runReconcile(ctx) }()
	wg.Wait()
	return nil
}

// Routes mounts the JSON GraphQL executor and the webhook receiver under
// /plugins/github/ (EDR §"Ingest", §"CLI"). Core dispatches per-request.
func (p *Plugin) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"graphql": http.HandlerFunc(p.handleGraphQL),
		"webhook": http.HandlerFunc(p.handleWebhook),
	}
}

// Cursor surfaces the reconcile since-cursor in the health snapshot (optional
// core.CursorReporter). It is an in-memory-cheap read of the persisted cursor.
func (p *Plugin) Cursor(ctx context.Context) string {
	if p.store == nil {
		return ""
	}
	return p.store.cursor(ctx, "reconcile:cursor")
}

// doEmit sends one envelope if Start has captured emit, else drops it (a webhook
// arriving before Start is captured is degenerate; the store write still happens).
func (p *Plugin) doEmit(ctx context.Context, e core.Envelope) {
	p.emitMu.RLock()
	emit := p.emit
	p.emitMu.RUnlock()
	if emit != nil {
		_ = emit(ctx, e)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func subMap(raw map[string]any, k string) (map[string]any, bool) {
	if raw == nil {
		return nil, false
	}
	m, ok := raw[k].(map[string]any)
	return m, ok
}
