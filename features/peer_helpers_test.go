package features

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// peerWorld is the peer plugin scenarios' per-scenario state. It drives TWO
// supergraph subprocesses: a CONSUMER (cw, a reused *world booted on loopback with
// the peer plugin) and a REMOTE (a self-managed serveProc running the harness-only
// fakeremote source executor). Steps are black-box: HTTP against the running
// processes and direct reads of the consumer's peer.db, never an import of the
// plugins/peer package. A fresh one is created per @peer scenario in sc.Before,
// matching the single-worker sequential pattern the core harness already uses.
type peerWorld struct {
	cw *world // consumer (loopback, peer plugin)

	remoteDir  string
	remoteHost string // 127.0.0.1:<port> used to PROBE and to point the consumer at
	remoteBind string // config listen: "127.0.0.1:<port>" (loopback) or ":<port>" (mesh)
	remoteTok  string // token the remote requires (non-loopback), "" for loopback
	remote     *serveProc

	consumerTok    string // token the consumer's peer config carries for boxB
	backoffMax     int    // consumer backoffMaxSeconds (0 ⇒ default)
	seeds          []seedNode
	remotePeerBoxC string // when set, the REMOTE also runs a peer plugin pointed at
	// this (dead) boxC url, so a recursive hop is STRUCTURALLY possible — the
	// AC-PEER-LOOP negative control ("remote's peer executor never invoked") is then
	// meaningful rather than vacuous (M1).

	served         servedWire // last servedResult decoded from the consumer executor
	samples        []time.Duration
	directRC       map[string]json.RawMessage // last direct-remote node bodies by key
	remoteBaseline int                        // remote served-count captured before a zero-call assertion
	locOut         string                     // captured `make loc-peer` output
}

// seedNode is one remote-seeded node (its @host key + a JSON body string).
type seedNode struct{ key, body string }

// servedWire mirrors plugins/peer's servedResult JSON — a black-box copy of the
// contract, not an import.
type servedWire struct {
	Host       string           `json:"host"`
	StaleSince *time.Time       `json:"staleSince"`
	Nodes      []servedNodeWire `json:"nodes"`
}

type servedNodeWire struct {
	Key        string          `json:"key"`
	Host       string          `json:"host"`
	LastSeenAt time.Time       `json:"lastSeenAt"`
	Node       json.RawMessage `json:"node"`
}

// peerRow mirrors the peers GraphQL query result.
type peerRow struct {
	HostID       string     `json:"hostId"`
	URL          string     `json:"url"`
	LastSeenAt   *time.Time `json:"lastSeenAt"`
	StaleSince   *time.Time `json:"staleSince"`
	LagSeconds   float64    `json:"remoteMaxPluginLagSeconds"`
	MirroredKeys int        `json:"mirroredKeys"`
}

func (pw *peerWorld) init() error {
	pw.cw = &world{}
	if err := pw.cw.init(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "sg-peer-remote-*")
	if err != nil {
		return err
	}
	pw.remoteDir = dir
	pw.remoteHost = freePort() // 127.0.0.1:<free>
	pw.remoteBind = pw.remoteHost
	return nil
}

func (pw *peerWorld) cleanup() {
	pw.stopRemote()
	if pw.cw != nil {
		pw.cw.cleanup()
	}
	if pw.remoteDir != "" {
		_ = os.RemoveAll(pw.remoteDir)
	}
}

// ---------- consumer ----------

func (pw *peerWorld) writeConsumerConfig() error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostId = %q\n", "consumerA")
	fmt.Fprintf(&b, "listen = %q\n", pw.cw.listen)
	fmt.Fprintf(&b, "dataDir = %q\n", pw.cw.dataDir)
	b.WriteString("lagThresholdSeconds = 30.000000\n")
	b.WriteString("[plugins.peer]\n")
	b.WriteString("remotePlugin = \"fakeremote\"\n")
	b.WriteString("staleThresholdSeconds = 30.000000\n")
	if pw.backoffMax > 0 {
		fmt.Fprintf(&b, "backoffMaxSeconds = %d\n", pw.backoffMax)
	}
	b.WriteString("[[plugins.peer.peers]]\n")
	b.WriteString("hostId = \"boxB\"\n")
	fmt.Fprintf(&b, "url = %q\n", "http://"+pw.remoteHost)
	fmt.Fprintf(&b, "token = %q\n", pw.consumerTok)
	return os.WriteFile(pw.cw.cfgPath, []byte(b.String()), 0o644)
}

func (pw *peerWorld) startConsumer() error {
	if err := pw.writeConsumerConfig(); err != nil {
		return err
	}
	return pw.cw.startServe()
}

func (pw *peerWorld) restartConsumer(token string) error {
	pw.cw.stopServe()
	pw.consumerTok = token
	return pw.startConsumer()
}

// ---------- remote (self-managed serveProc running fakeremote) ----------

func (pw *peerWorld) writeRemoteConfig() error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostId = %q\n", "remoteB")
	fmt.Fprintf(&b, "listen = %q\n", pw.remoteBind)
	fmt.Fprintf(&b, "dataDir = %q\n", pw.remoteDir)
	b.WriteString("lagThresholdSeconds = 30.000000\n")
	if pw.remoteTok != "" {
		b.WriteString("[tokens]\n")
		fmt.Fprintf(&b, "boxB = %q\n", pw.remoteTok)
	}
	b.WriteString("[plugins.fakeremote]\n")
	for _, s := range pw.seeds {
		b.WriteString("[[plugins.fakeremote.nodes]]\n")
		fmt.Fprintf(&b, "key = %q\n", s.key)
		fmt.Fprintf(&b, "body = %q\n", s.body)
	}
	if pw.remotePeerBoxC != "" {
		// Remote runs its OWN peer plugin (executor mounted at /plugins/peer/op),
		// pointed at a dead boxC — the consumer must NOT proxy into it (M1).
		b.WriteString("[plugins.peer]\n")
		b.WriteString("[[plugins.peer.peers]]\n")
		b.WriteString("hostId = \"boxC\"\n")
		fmt.Fprintf(&b, "url = %q\n", pw.remotePeerBoxC)
	}
	return os.WriteFile(filepath.Join(pw.remoteDir, "config.toml"), []byte(b.String()), 0o644)
}

func (pw *peerWorld) startRemote() error {
	if err := pw.writeRemoteConfig(); err != nil {
		return err
	}
	sp := &serveProc{stdout: &safeBuf{}, stderr: &safeBuf{}, done: make(chan struct{})}
	cmd := exec.Command(binPath, "--config", filepath.Join(pw.remoteDir, "config.toml"), "serve")
	cmd.Stdout = sp.stdout
	cmd.Stderr = sp.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sp.cmd = cmd
	go func() { sp.waitErr = cmd.Wait(); close(sp.done) }()
	pw.remote = sp
	return pw.waitRemote(10 * time.Second)
}

// waitRemote polls the remote's /health (with the mesh bearer when the bind is
// non-loopback) until it answers 200.
func (pw *peerWorld) waitRemote(d time.Duration) error {
	deadline := time.Now().Add(d)
	client := http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		if pw.remote.exited() {
			return fmt.Errorf("remote exited early: %s", pw.remote.stderr.String())
		}
		req, _ := http.NewRequest(http.MethodGet, "http://"+pw.remoteHost+"/health", nil)
		if pw.remoteTok != "" {
			req.Header.Set("Authorization", "Bearer "+pw.remoteTok)
		}
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("remote /health not ready within %s: %s", d, pw.remote.stderr.String())
}

func (pw *peerWorld) stopRemote() {
	if pw.remote == nil {
		return
	}
	_ = pw.remote.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-pw.remote.done:
	case <-time.After(5 * time.Second):
		_ = pw.remote.cmd.Process.Kill()
		<-pw.remote.done
	}
	pw.remote = nil
}

func (pw *peerWorld) remoteLog() string {
	if pw.remote == nil {
		return ""
	}
	return pw.remote.stdout.String() + pw.remote.stderr.String()
}

func (pw *peerWorld) consumerLog() string {
	if pw.cw.serve == nil {
		return ""
	}
	return pw.cw.serve.stdout.String() + pw.cw.serve.stderr.String()
}

// ---------- HTTP against the consumer executor / GraphQL ----------

func (pw *peerWorld) postExecutor(op, host string, refresh bool) (servedWire, error) {
	body, _ := json.Marshal(map[string]any{"op": op, "host": host, "refresh": refresh})
	resp, err := http.Post("http://"+pw.cw.listen+"/plugins/peer/op", "application/json", bytes.NewReader(body))
	if err != nil {
		return servedWire{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var sw servedWire
	if err := json.Unmarshal(raw, &sw); err != nil {
		return servedWire{}, fmt.Errorf("decode served %q: %w", raw, err)
	}
	return sw, nil
}

// directRemote queries the remote's fakeremote executor directly (bypassing the
// peer), returning the node bodies keyed by their @host key — proof of what the
// remote OFFERED, for AC-PEER-LOOP.
func (pw *peerWorld) directRemote(op string) (map[string]json.RawMessage, error) {
	body, _ := json.Marshal(map[string]any{"op": op})
	req, _ := http.NewRequest(http.MethodPost, "http://"+pw.remoteHost+"/plugins/fakeremote/graphql", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if pw.remoteTok != "" {
		req.Header.Set("Authorization", "Bearer "+pw.remoteTok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Nodes []struct {
			Key  string          `json:"key"`
			Node json.RawMessage `json:"node"`
		} `json:"nodes"`
	}
	_ = json.Unmarshal(raw, &out)
	m := map[string]json.RawMessage{}
	for _, n := range out.Nodes {
		m[n.Key] = n.Node
	}
	return m, nil
}

func (pw *peerWorld) consumerPeers() ([]peerRow, error) {
	q := `{ peers { hostId url lastSeenAt staleSince remoteMaxPluginLagSeconds mirroredKeys } }`
	body, _ := json.Marshal(map[string]string{"query": q})
	resp, err := http.Post("http://"+pw.cw.listen+"/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out struct {
		Data struct {
			Peers []peerRow `json:"peers"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode peers %q: %w", raw, err)
	}
	if len(out.Errors) > 0 {
		return nil, fmt.Errorf("peers graphql errors: %v", out.Errors)
	}
	return out.Data.Peers, nil
}

func (pw *peerWorld) peerRowFor(host string) (peerRow, error) {
	rows, err := pw.consumerPeers()
	if err != nil {
		return peerRow{}, err
	}
	for _, r := range rows {
		if r.HostID == host {
			return r, nil
		}
	}
	return peerRow{}, fmt.Errorf("no peers row for %q in %v", host, rows)
}

// waitPeerStale polls until host's peers row has staleSince set (or times out).
func (pw *peerWorld) waitPeerStale(host string, d time.Duration, want bool) error {
	deadline := time.Now().Add(d)
	var last peerRow
	for time.Now().Before(deadline) {
		r, err := pw.peerRowFor(host)
		if err == nil {
			last = r
			if (r.StaleSince != nil) == want {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("peer %q staleSince set=%v not reached within %s (last=%+v)", host, want, d, last)
}

// ---------- direct consumer peer.db reads (same pattern as latestTemplatePayload) ----------

func (pw *peerWorld) peerDB() (*sql.DB, error) {
	path := filepath.Join(pw.cw.dataDir, "peer.db")
	return sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout%285000%29")
}

func (pw *peerWorld) peerNodeKeys() ([]string, error) {
	db, err := pw.peerDB()
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT key FROM peer_nodes ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (pw *peerWorld) peerNodeBody(key string) ([]byte, bool, error) {
	db, err := pw.peerDB()
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = db.Close() }()
	var body []byte
	err = db.QueryRow(`SELECT node_json FROM peer_nodes WHERE key=?`, key).Scan(&body)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	return body, err == nil, err
}

func (pw *peerWorld) peerEventCount(typ, key string) (int, error) {
	db, err := pw.peerDB()
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var n int
	err = db.QueryRow(`SELECT COUNT(*) FROM events WHERE source='peer' AND type=? AND key=?`, typ, key).Scan(&n)
	return n, err
}
