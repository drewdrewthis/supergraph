package peer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	coderws "github.com/coder/websocket"
)

// client.go is the peer plugin's outbound mesh layer: every call to a remote box
// goes through here, so bearer auth lives in exactly one place. A remote's mesh
// bind is non-loopback, so core mandates a token there and enforces Bearer on every
// route including the base /graphql and its pluginLag websocket (D6). A loopback
// remote (the seeded @local fixtures) needs no token.

// remoteNode is one node in a remote executor's response. It mirrors the github
// executor wire shape (key + raw node body), the source-agnostic contract the peer
// mirrors against — the peer never decodes a node's body, only its @host key.
type remoteNode struct {
	Key  string          `json:"key"`
	Node json.RawMessage `json:"node"`
}

// remoteResult is the {nodes:[...]} envelope a remote /plugins/<name>/graphql
// executor returns. errAuth is set true by postOp when the remote answered 401.
type remoteResult struct {
	Nodes []remoteNode `json:"nodes"`
}

// errUnauthorized flags a 401 from the remote so the caller marks the peer
// unreachable and mirrors nothing (AC-PEER-AUTH).
type errUnauthorized struct{ host string }

func (e errUnauthorized) Error() string { return "peer: 401 from " + e.host }

// postOp proxies one named op to a remote plugin executor with bearer auth and
// returns the decoded nodes. A 401 yields errUnauthorized; any other non-200 is a
// transport error.
func (p *Plugin) postOp(ctx context.Context, pr peerCfg, targetPlugin, op string, vars map[string]any) (remoteResult, error) {
	reqBody, _ := json.Marshal(map[string]any{"op": op, "variables": vars})
	url := strings.TrimRight(pr.URL, "/") + "/plugins/" + targetPlugin + "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return remoteResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	authorize(req, pr.Token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return remoteResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return remoteResult{}, errUnauthorized{host: pr.HostID}
	}
	if resp.StatusCode != http.StatusOK {
		return remoteResult{}, fmt.Errorf("peer: %s op %q: status %d", pr.HostID, op, resp.StatusCode)
	}
	var out remoteResult
	if err := json.Unmarshal(body, &out); err != nil {
		return remoteResult{}, fmt.Errorf("peer: decode %s op %q: %w", pr.HostID, op, err)
	}
	return out, nil
}

// ping probes a remote's base /graphql liveness with a trivial `{ ping }` query and
// bearer auth. It returns errUnauthorized on 401, an error on any other failure, and
// nil when the remote answered — the poll-fallback liveness signal (D7).
func (p *Plugin) ping(ctx context.Context, pr peerCfg) error {
	reqBody, _ := json.Marshal(map[string]any{"query": "{ ping }"})
	url := strings.TrimRight(pr.URL, "/") + "/graphql"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	authorize(req, pr.Token)
	resp, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return errUnauthorized{host: pr.HostID}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer: ping %s: status %d", pr.HostID, resp.StatusCode)
	}
	return nil
}

// subscribeLag dials the remote's base /graphql pluginLag subscription over
// graphql-transport-ws with bearer auth. It calls onConnect once the subscription
// is established (the liveness signal — a healthy remote pushes nothing) and onLag
// for every pushed Health frame's worst lag, until ctx is done or the socket drops.
// A 401 on the handshake returns errUnauthorized (AC-PEER-AUTH via the subscription
// path); any dial or read failure returns an error the liveness loop treats as a
// drop (backoff reconnect).
func (p *Plugin) subscribeLag(ctx context.Context, pr peerCfg, threshold float64, onConnect func(), onLag func(float64)) error {
	wsURL := "ws" + strings.TrimPrefix(strings.TrimRight(pr.URL, "/"), "http") + "/graphql"
	opts := &coderws.DialOptions{Subprotocols: []string{"graphql-transport-ws"}}
	if pr.Token != "" {
		opts.HTTPHeader = http.Header{"Authorization": {"Bearer " + pr.Token}}
	}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	conn, resp, err := coderws.Dial(dctx, wsURL, opts)
	dcancel()
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return errUnauthorized{host: pr.HostID}
		}
		return err
	}
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	if err := wsWrite(ctx, conn, map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		return err
	}
	q := fmt.Sprintf("subscription { pluginLag(thresholdSeconds: %g) { plugin lagSeconds } }", threshold)
	acked := false
	for {
		m, err := wsRead(ctx, conn)
		if err != nil {
			return err
		}
		switch m["type"] {
		case "connection_ack":
			if !acked {
				acked = true
				if err := wsWrite(ctx, conn, map[string]any{"id": "1", "type": "subscribe",
					"payload": map[string]any{"query": q}}); err != nil {
					return err
				}
				onConnect()
			}
		case "next":
			onLag(worstLag(m))
		case "error":
			return fmt.Errorf("peer: pluginLag error from %s: %v", pr.HostID, m["payload"])
		}
	}
}

// worstLag pulls the largest lagSeconds out of a pluginLag `next` frame.
func worstLag(m map[string]any) float64 {
	payload, _ := m["payload"].(map[string]any)
	data, _ := payload["data"].(map[string]any)
	h, _ := data["pluginLag"].(map[string]any)
	lag, _ := h["lagSeconds"].(float64)
	return lag
}

func authorize(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func wsWrite(ctx context.Context, c *coderws.Conn, v map[string]any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Write(wctx, coderws.MessageText, b)
}

func wsRead(ctx context.Context, c *coderws.Conn) (map[string]any, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}
