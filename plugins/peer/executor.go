package peer

import (
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// executor.go is the peer plugin's pull-through HTTP executor (/plugins/peer/op,
// github HTTPRoutes pattern): a warm local read or a refresh proxy, returning the
// mirrored nodes tagged host + freshness.

// servedNode is one mirrored node on the wire, tagged with its origin host and
// mirror freshness.
type servedNode struct {
	Key        string          `json:"key"`
	Host       string          `json:"host"`
	LastSeenAt time.Time       `json:"lastSeenAt"`
	Node       json.RawMessage `json:"node"`
}

// servedResult is the JSON /plugins/peer/op returns: the host, its current
// staleSince (non-null ⇒ the peer is flagged stale but its rows are still served),
// and the mirrored nodes.
type servedResult struct {
	Host       string       `json:"host"`
	StaleSince *time.Time   `json:"staleSince"`
	Nodes      []servedNode `json:"nodes"`
}

// handleOp serves POST {op, host, variables, refresh}. refresh=true proxies to
// the remote and refreshes the mirror; refresh=false is the warm path — a pure local
// read from peer_nodes with zero upstream call (S2). A stopped peer still serves its
// mirrored rows, flagged by staleSince (F8).
func (p *Plugin) handleOp(w http.ResponseWriter, r *http.Request) {
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

	// Both reads discard their error on purpose: a proxy failure (401, transport) or
	// a warm-read error degrades to serving whatever is cached (possibly nothing)
	// under the current staleSince, never a 500 — a stale mirror is still useful (F8).
	var nodes []mirroredNode
	if req.Refresh {
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
