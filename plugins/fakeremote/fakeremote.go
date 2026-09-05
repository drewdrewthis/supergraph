//go:build harness

// Package fakeremote is a harness-only source plugin that stands in for a remote
// box's per-plugin executor in the peer plugin's @local scenarios. It serves
// /plugins/fakeremote/graphql, returning a config-seeded set of nodes in the same
// {nodes:[{key,node}]} wire shape a real source executor (github) returns, keyed by
// their @host grammar — including a plantable foreign-host row so the peer's loop
// guard (AC-PEER-LOOP) can be exercised. It logs every served request so a black-box
// step can assert the "zero calls to the remote" warm path. Built only under the
// `harness` tag, so it never ships in a production binary.
package fakeremote

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

func init() { core.Register("fakeremote", New) }

// seedNode is one config-seeded node: its @host key and its raw JSON body.
type seedNode struct {
	key  string
	body json.RawMessage
}

// Plugin serves the seeded nodes at its executor route.
type Plugin struct {
	hostID string
	nodes  []seedNode
}

// New builds the plugin, reading its seed set from [plugins.fakeremote].nodes.
func New(cfg core.PluginConfig) (core.Plugin, error) {
	p := &Plugin{hostID: cfg.HostID}
	if list, ok := cfg.Raw["nodes"].([]any); ok {
		for _, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			key, _ := m["key"].(string)
			body, _ := m["body"].(string)
			if key != "" {
				p.nodes = append(p.nodes, seedNode{key: key, body: json.RawMessage(body)})
			}
		}
	}
	return p, nil
}

// Name returns the plugin's stable id.
func (p *Plugin) Name() string { return "fakeremote" }

// Migrate is a no-op: the seed set lives in config, not a state table.
func (p *Plugin) Migrate(_ context.Context, _ *core.Store) error { return nil }

// Start emits one hello so the process's /health shows the plugin ok, then idles
// until ctx is done (the peer's liveness only needs the base pluginLag stream, which
// core serves regardless).
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	_ = emit(ctx, core.Envelope{
		TS:     time.Now().UTC(), // core stamps /health lastEventAt from TS; a zero TS reads as epoch
		Source: "fakeremote", Type: "fakeremote.hello", V: 1,
		Key: "fakeremote:hello@" + p.hostID, Payload: json.RawMessage(`{}`),
	})
	<-ctx.Done()
	return nil
}

// Routes mounts the seeded executor under /plugins/fakeremote/.
func (p *Plugin) Routes() map[string]http.Handler {
	return map[string]http.Handler{"graphql": http.HandlerFunc(p.handle)}
}

// handle returns every seeded node, ignoring the op/variables (the peer's loop guard
// does the host filtering). It logs each request so a step can count remote calls.
func (p *Plugin) handle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Op string `json:"op"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	log.Printf("fakeremote: served op=%q count=%d", req.Op, len(p.nodes))
	nodes := make([]map[string]any, 0, len(p.nodes))
	for _, n := range p.nodes {
		nodes = append(nodes, map[string]any{"key": n.key, "node": n.body})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"nodes": nodes})
}
