package features

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	coderws "github.com/coder/websocket"

	"github.com/drewdrewthis/supergraph/core"
	_ "modernc.org/sqlite" // registers the "sqlite" driver for direct db assertions
)

// binPath is the supergraph binary built once in TestMain; repoRoot is the module
// root (the parent of this features/ dir). Both are process-wide and read-only
// after TestMain.
var (
	binPath  string
	repoRoot string
)

// safeBuf is a mutex-guarded buffer so a serve subprocess's stdout/stderr can be
// written by exec's copier goroutine while a step reads it concurrently.
type safeBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// serveProc is a running `supergraph serve` subprocess plus captured output and a
// done channel closed when it exits.
type serveProc struct {
	cmd     *exec.Cmd
	stdout  *safeBuf
	stderr  *safeBuf
	done    chan struct{}
	waitErr error
}

func (s *serveProc) exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// cfgOpts describes the config.toml a scenario needs. Floats are always rendered
// with a decimal point so go-toml parses them into the float64 config fields
// rather than rejecting an integer literal.
type cfgOpts struct {
	omitHostID       bool
	lag              float64
	templateInterval int
	templatePanic    bool
}

// world is one scenario's mutable state. A fresh world is created per scenario in
// InitializeScenario, so godog's default single-worker run keeps steps isolated.
type world struct {
	dataDir string
	cfgPath string
	listen  string
	serve   *serveProc

	// last CLI / HTTP invocation
	lastStdout string
	lastStderr string
	lastExit   int

	// AC-CORE-1
	emittedPayload []byte
	emittedV       int
	storedPayload  []byte

	// AC-CORE-2 in-process ring store
	ring      *core.Store
	ringPath  string
	ringRows  int
	cursorVal string

	// AC-CORE-3 health captures
	healthBody []byte
	healthRows []map[string]any

	// AC-CORE-8 / AC-CORE-10b websocket
	ws       *wsClient
	wsData   map[string]any
	wsPushed bool
	wsEmitAt time.Time // AC-CORE-8: clock start, set after connection setup/ack so the <1s bound measures only emit-to-receipt

	// AC-CORE-12
	bootStdout string
	panicMsg   string

	// AC-CORE-15
	realDirBefore map[string]bool
	makeStdout    string
	makeExit      int

	// AC-CORE-16
	diffOut string

	// AC-CORE-13
	firstTables []string

	// AC-CORE-17 concurrency
	cstore         *core.Store
	writerStop     chan struct{}
	writerDone     chan struct{}
	concurrentRows int
	lockErr        atomic.Bool

	// service scenarios (AC-CORE-7a/7b / -14, gated by FEATURES_SERVICE)
	svc bool
}

func (w *world) init() error {
	dir, err := os.MkdirTemp("", "sg-feature-*")
	if err != nil {
		return err
	}
	w.dataDir = dir
	w.cfgPath = filepath.Join(dir, "config.toml")
	w.listen = freePort()
	return nil
}

func (w *world) cleanup() {
	w.stopServe()
	if w.ring != nil {
		_ = w.ring.Close()
	}
	if w.cstore != nil {
		_ = w.cstore.Close()
	}
	if w.ws != nil {
		w.ws.close()
	}
	if w.svc {
		if m, err := serviceManager(); err == nil {
			_ = m.Uninstall()
		}
	}
	if w.dataDir != "" {
		_ = os.RemoveAll(w.dataDir)
	}
}

// freePort returns a currently-free loopback address. A tiny race window exists
// between close and the server's bind, acceptable for a test harness.
func freePort() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "127.0.0.1:7788"
	}
	defer l.Close()
	return l.Addr().String()
}

func (w *world) writeConfig(o cfgOpts) error {
	var b strings.Builder
	if !o.omitHostID {
		fmt.Fprintf(&b, "hostId = %q\n", "test")
	}
	fmt.Fprintf(&b, "listen = %q\n", w.listen)
	fmt.Fprintf(&b, "dataDir = %q\n", w.dataDir)
	if o.lag > 0 {
		fmt.Fprintf(&b, "lagThresholdSeconds = %f\n", o.lag)
	}
	b.WriteString("[plugins.template]\n")
	if o.templateInterval > 0 {
		fmt.Fprintf(&b, "intervalSeconds = %d\n", o.templateInterval)
	}
	if o.templatePanic {
		b.WriteString("panic = true\n")
	}
	return os.WriteFile(w.cfgPath, []byte(b.String()), 0o644)
}

// defaultCfg is the config the generic "server started with template" Given uses:
// a fast template tick (sub-second pushes for the subscription ACs) and a generous
// lag threshold so every plugin reads ok.
func defaultCfg() cfgOpts {
	return cfgOpts{lag: 30, templateInterval: 1}
}

func (w *world) startServe() error {
	sp := &serveProc{stdout: &safeBuf{}, stderr: &safeBuf{}, done: make(chan struct{})}
	cmd := exec.Command(binPath, "--config", w.cfgPath, "serve")
	cmd.Stdout = sp.stdout
	cmd.Stderr = sp.stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	sp.cmd = cmd
	go func() { sp.waitErr = cmd.Wait(); close(sp.done) }()
	w.serve = sp
	return w.waitHealth(10 * time.Second)
}

// readyPlugins are the plugins every serve subprocess in this suite starts
// (the binary is always built with -tags harness, see TestMain), so a serve
// process is only "ready" once both have run their startup emit.
var readyPlugins = []string{"template", "fakeok"}

// waitHealth blocks until /health reports 200 AND every plugin in
// readyPlugins has a non-null "lastEventAt". A 200 alone only means the HTTP
// server is up; the Supervisor may still be mid-Migrate/Start (its plugins
// report "starting" with no lastEventAt yet), which raced scenarios that read
// the DB or lastEventAt straight after startServe returned.
func (w *world) waitHealth(d time.Duration) error {
	deadline := time.Now().Add(d)
	client := http.Client{Timeout: time.Second}
	var lastBody []byte
	for time.Now().Before(deadline) {
		if w.serve != nil && w.serve.exited() {
			return fmt.Errorf("serve exited early: %s", w.serve.stderr.String())
		}
		resp, err := client.Get("http://" + w.listen + "/health")
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				lastBody = body
				var rows []map[string]any
				if err := json.Unmarshal(body, &rows); err == nil && allPluginsReady(rows) {
					return nil
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	stderr := ""
	if w.serve != nil {
		stderr = w.serve.stderr.String()
	}
	return fmt.Errorf("/health not ready within %s: %s (last body: %s)", d, stderr, lastBody)
}

// allPluginsReady reports whether every readyPlugins entry is present in rows
// with a non-null, non-empty "lastEventAt" (set only after Migrate has run and
// the plugin's Start has emitted its first event).
func allPluginsReady(rows []map[string]any) bool {
	for _, name := range readyPlugins {
		r := findRow(rows, name)
		if r == nil {
			return false
		}
		s, _ := r["lastEventAt"].(string)
		if s == "" {
			return false
		}
	}
	return true
}

func (w *world) stopServe() {
	if w.serve == nil {
		return
	}
	_ = w.serve.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-w.serve.done:
	case <-time.After(5 * time.Second):
		_ = w.serve.cmd.Process.Kill()
		<-w.serve.done
	}
	w.serve = nil
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// runCLI runs the built binary with --config prepended, capturing stdout/stderr
// and the exit code into the world.
func (w *world) runCLI(args ...string) {
	full := append([]string{"--config", w.cfgPath}, args...)
	cmd := exec.Command(binPath, full...)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	w.lastStdout = so.String()
	w.lastStderr = se.String()
	w.lastExit = exitCode(err)
}

func (w *world) getHealth() ([]map[string]any, []byte, error) {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + w.listen + "/health")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, body, fmt.Errorf("health status %d", resp.StatusCode)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, body, err
	}
	return rows, body, nil
}

func findRow(rows []map[string]any, plugin string) map[string]any {
	for _, r := range rows {
		if r["plugin"] == plugin {
			return r
		}
	}
	return nil
}

// latestTemplatePayload reads the newest v=2 template event's payload straight
// from the plugin's SQLite file, the readback half of the byte-identical proof.
func (w *world) latestTemplatePayload() ([]byte, error) {
	dbPath := filepath.Join(w.dataDir, "template.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout%285000%29")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	var pay []byte
	err = db.QueryRow(`SELECT payload FROM events WHERE source='template' AND v=2 ORDER BY id DESC LIMIT 1`).Scan(&pay)
	return pay, err
}

func tmplEnv(i int) core.Envelope {
	p, _ := json.Marshal(map[string]any{"msg": "x", "n": i})
	return core.Envelope{
		TS:      time.Now().UTC(),
		Source:  "template",
		Type:    "template.tick",
		V:       2,
		Key:     fmt.Sprintf("k-%d", i),
		Payload: p,
	}
}

func locked(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "database is locked")
}

// ---- minimal graphql-transport-ws client ----

type wsClient struct {
	conn   *coderws.Conn
	ctx    context.Context
	cancel context.CancelFunc
}

func (w *world) openSubscription(field string) error {
	ctx, cancel := context.WithCancel(context.Background())
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	conn, _, err := coderws.Dial(dctx, "ws://"+w.listen+"/graphql", &coderws.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err != nil {
		cancel()
		return err
	}
	c := &wsClient{conn: conn, ctx: ctx, cancel: cancel}
	if err := c.write(map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		c.close()
		return err
	}
	// Await the ack before subscribing (graphql-transport-ws handshake).
	ackCtx, ackCancel := context.WithTimeout(ctx, 3*time.Second)
	defer ackCancel()
	for {
		m, err := c.read(ackCtx)
		if err != nil {
			c.close()
			return err
		}
		if m["type"] == "connection_ack" {
			break
		}
	}
	q := fmt.Sprintf("subscription { %s { payload v } }", field)
	if err := c.write(map[string]any{"id": "1", "type": "subscribe", "payload": map[string]any{"query": q}}); err != nil {
		c.close()
		return err
	}
	w.ws = c
	return nil
}

func (c *wsClient) write(v map[string]any) error {
	b, _ := json.Marshal(v)
	wctx, cancel := context.WithTimeout(c.ctx, 3*time.Second)
	defer cancel()
	return c.conn.Write(wctx, coderws.MessageText, b)
}

func (c *wsClient) read(ctx context.Context) (map[string]any, error) {
	_, b, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m, nil
}

// nextPush blocks until a "next" (data) message arrives or timeout, returning the
// payload object. Non-data frames (ping, ka) are skipped.
func (c *wsClient) nextPush(timeout time.Duration) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()
	for {
		m, err := c.read(ctx)
		if err != nil {
			return nil, err
		}
		switch m["type"] {
		case "next":
			if p, ok := m["payload"].(map[string]any); ok {
				return p, nil
			}
			return nil, fmt.Errorf("next without payload: %v", m)
		case "error":
			return nil, fmt.Errorf("subscription error: %v", m["payload"])
		case "complete":
			return nil, fmt.Errorf("subscription completed before a push")
		}
	}
}

func (c *wsClient) close() {
	if c.conn != nil {
		_ = c.conn.Close(coderws.StatusNormalClosure, "")
	}
	if c.cancel != nil {
		c.cancel()
	}
}
