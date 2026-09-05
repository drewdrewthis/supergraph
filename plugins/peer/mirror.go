package peer

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// mirror.go is the judgment-heavy core: it pulls a named op through to a remote
// source executor, applies the two loop guards (D5), caches every kept node under
// its original @host key, and re-emits one local envelope per node so cross-plugin
// joins key on the same grammar the origin used (D4).

// hostOf returns the @hostId suffix of a key (the segment after the last '@'), or
// "" when the key carries none. Peer routing and the loop guards read only this.
func hostOf(key string) string {
	if at := strings.LastIndexByte(key, '@'); at >= 0 {
		return key[at+1:]
	}
	return ""
}

// mirror proxies op to the source plugin behind host, drops any node that would
// form a peer-of-peer row (D5), caches the survivors, and returns them. An unknown
// host returns empty with no outbound hop; a 401 marks the peer unreachable and
// mirrors nothing.
func (p *Plugin) mirror(ctx context.Context, op, host string, vars map[string]any) ([]mirroredNode, error) {
	pr, ok := p.peerByHost(host)
	if !ok {
		// Unknown host: never invent a hop (D3 key-grammar routing).
		log.Printf("peer: op %q for unknown host %q dropped", op, host)
		return nil, nil
	}
	target := p.targetPlugin(op)
	// Loop guard 1: the fan-out only targets a remote's SOURCE plugins, never its
	// own peer plugin, so a mesh can never recurse through peers (D5-1).
	if target == "peer" {
		return nil, nil
	}
	res, err := p.postOp(ctx, pr, target, op, vars)
	if err != nil {
		var ua errUnauthorized
		if errors.As(err, &ua) {
			_, _ = p.store.markStale(ctx, host, p.now())
		}
		return nil, err
	}

	now := p.now()
	var kept []mirroredNode
	for _, rn := range res.Nodes {
		kh := hostOf(rn.Key)
		// Loop guard 2: keep only first-hop rows. A row whose @host is not this
		// peer's own id is the remote's mirror of a THIRD box (drop, D5-2); a row
		// tagged with the local host is our own echo (drop). Together: no
		// peer-of-peer rows, no cycle (F8).
		if kh != pr.HostID || kh == p.cfg.hostID {
			log.Printf("peer: dropped foreign-host row %q from %s", rn.Key, host)
			continue
		}
		mn := mirroredNode{Key: rn.Key, Host: pr.HostID, NodeJSON: []byte(rn.Node), LastSeenAt: now}
		if err := p.store.upsertNode(ctx, mn); err != nil {
			return nil, err
		}
		p.emitMirrored(ctx, mn)
		kept = append(kept, mn)
	}
	// A successful proxy is itself a liveness signal.
	_ = p.store.markSeen(ctx, host, now, 0)
	return kept, nil
}

// emitMirrored emits one peer.node.mirrored envelope preserving the origin @host
// key, so core's /health "peer" lastEventAt advances and joins stay key-compatible.
func (p *Plugin) emitMirrored(ctx context.Context, n mirroredNode) {
	payload, _ := json.Marshal(map[string]any{
		"peer":       n.Host,
		"node":       json.RawMessage(n.NodeJSON),
		"lastSeenAt": n.LastSeenAt.UTC().Format(time.RFC3339Nano),
	})
	p.doEmit(ctx, core.Envelope{
		TS: n.LastSeenAt, Source: "peer", Type: "peer.node.mirrored", V: 1,
		Key: n.Key, Payload: payload,
	})
}

// targetPlugin maps a named op to the remote SOURCE plugin that serves it. A
// test/plugin-level override (cfg.remotePlugin) forces every op at one executor so
// the @local harness can point the whole fan-out at its seeded fakeremote; in
// production the static map routes each op to its owning source plugin.
func (p *Plugin) targetPlugin(op string) string {
	if p.cfg.remotePlugin != "" {
		return p.cfg.remotePlugin
	}
	if plugin, ok := opPlugins[op]; ok {
		return plugin
	}
	return "github"
}

// opPlugins is the production op→source-plugin routing table. It is intentionally
// small: the mesh mirrors whatever a source plugin already exposes at its executor.
var opPlugins = map[string]string{
	"issue":         "github",
	"pr":            "github",
	"issueComments": "github",
	"freeSlots":     "tmux",
	"paneForBranch": "tmux",
	"claude":        "claude",
}
