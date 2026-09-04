package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
	"go.uber.org/goleak"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/server"
)

// TestMain runs goleak for the server package so a leaked http.Server or
// subscriber goroutine (one not shut down at test end) fails the suite.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// onDemandPlugin is an integration test double registered into a LOCAL factories
// map (never the global registry): it emits nothing on its own and forwards each
// envelope handed to its emit channel through core's real emit path.
type onDemandPlugin struct {
	emit  chan core.Envelope
	ready chan struct{}
}

func (p *onDemandPlugin) Name() string                                   { return "faker" }
func (p *onDemandPlugin) Migrate(_ context.Context, _ *core.Store) error { return nil }
func (p *onDemandPlugin) Start(ctx context.Context, emit core.Emit) error {
	close(p.ready)
	for {
		select {
		case <-ctx.Done():
			return nil
		case e := <-p.emit:
			_ = emit(ctx, e)
		}
	}
}

// startTestServer boots a supervisor over a local fake plugin and serves on a
// random loopback port, returning the base URL and the plugin's emit channel.
func startTestServer(t *testing.T) (base string, emit chan core.Envelope) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fake := &onDemandPlugin{emit: make(chan core.Envelope, 1), ready: make(chan struct{})}
	cfg := core.Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300}
	sv := core.NewSupervisor(cfg, map[string]core.Factory{
		"faker": func(core.PluginConfig) (core.Plugin, error) { return fake, nil },
	}, core.NewHealthAggregator(cfg.LagThresholdSeconds), core.NewBus())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.Listen = ln.Addr().String()
	srv := server.New(cfg, sv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	go sv.Run(ctx)

	select {
	case <-fake.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin never started")
	}
	return "http://" + ln.Addr().String(), fake.emit
}

// AC-CORE-3: GET /health is a 200 JSON array whose elements carry exactly the five
// contract keys, and lastEventAt is JSON null before the first event.
func TestHealthEndpointShape(t *testing.T) {
	base, _ := startTestServer(t)

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rows []map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	row := rows[0]
	want := []string{"plugin", "lastEventAt", "cursor", "lagSeconds", "state"}
	if len(row) != len(want) {
		t.Fatalf("want exactly %d keys, got %d: %v", len(want), len(row), row)
	}
	for _, k := range want {
		if _, ok := row[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if string(row["lastEventAt"]) != "null" {
		t.Errorf("lastEventAt = %s, want null before first event", row["lastEventAt"])
	}
}

// AC-CORE-3: the health GraphQL query answers 200 over POST.
func TestGraphQLHealthQuery(t *testing.T) {
	base, _ := startTestServer(t)

	body, _ := json.Marshal(map[string]string{"query": "{ health { plugin state } }"})
	resp, err := http.Post(base+"/graphql", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /graphql: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Data struct {
			Health []struct{ Plugin, State string }
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Data.Health) != 1 || out.Data.Health[0].Plugin != "faker" {
		t.Fatalf("unexpected health payload: %+v", out.Data.Health)
	}
}

// AC-CORE-8: a graphql-transport-ws subscriber to pluginLag receives a pushed
// `next` message < 1s after an event is emitted, and closing the socket logs no
// server error.
func TestSubscriptionPushOnEmit(t *testing.T) {
	var logBuf bytes.Buffer
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(nil) })

	base, emit := startTestServer(t)
	wsURL := "ws" + strings.TrimPrefix(base, "http") + "/graphql"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := coderws.Dial(ctx, wsURL, &coderws.DialOptions{
		Subprotocols: []string{"graphql-transport-ws"},
	})
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}

	writeJSON(ctx, t, conn, map[string]any{"type": "connection_init"})
	if got := readType(ctx, t, conn); got != "connection_ack" {
		t.Fatalf("want connection_ack, got %q", got)
	}
	writeJSON(ctx, t, conn, map[string]any{
		"id":      "1",
		"type":    "subscribe",
		"payload": map[string]string{"query": "subscription { pluginLag(thresholdSeconds: 0) { plugin state } }"},
	})

	time.Sleep(50 * time.Millisecond) // let the subscription register before emitting
	start := time.Now()
	emit <- core.Envelope{TS: time.Now(), Source: "faker", Type: "test", V: 1, Key: "k"}

	readCtx, readCancel := context.WithTimeout(ctx, time.Second)
	defer readCancel()
	if got := readType(readCtx, t, conn); got != "next" {
		t.Fatalf("want next message, got %q", got)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("push took %v, want < 1s", d)
	}

	_ = conn.Close(coderws.StatusNormalClosure, "done")
	time.Sleep(100 * time.Millisecond)
	if s := logBuf.String(); strings.Contains(s, "server: websocket error") {
		t.Fatalf("server logged an error on client disconnect: %s", s)
	}
}

// routedPlugin is a fake plugin implementing the optional core.HTTPRoutes: it emits
// nothing and exposes one route so the /plugins/<name>/ mount seam (S5) is provable.
type routedPlugin struct{ ready chan struct{} }

func (p *routedPlugin) Name() string                                   { return "routed" }
func (p *routedPlugin) Migrate(_ context.Context, _ *core.Store) error { return nil }
func (p *routedPlugin) Start(ctx context.Context, _ core.Emit) error {
	close(p.ready)
	<-ctx.Done()
	return nil
}
func (p *routedPlugin) Routes() map[string]http.Handler {
	return map[string]http.Handler{
		"/hook": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("hooked"))
		}),
	}
}

// A2/S5: a plugin implementing HTTPRoutes has its routes mounted under
// /plugins/<name>/ with zero core edit; GET /plugins/routed/hook returns its body.
func TestPluginHTTPRoutesMounted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rp := &routedPlugin{ready: make(chan struct{})}
	cfg := core.Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300}
	sv := core.NewSupervisor(cfg, map[string]core.Factory{
		"routed": func(core.PluginConfig) (core.Plugin, error) { return rp, nil },
	}, core.NewHealthAggregator(cfg.LagThresholdSeconds), core.NewBus())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cfg.Listen = ln.Addr().String()
	srv := server.New(cfg, sv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	go sv.Run(ctx)

	select {
	case <-rp.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin never started")
	}

	resp, err := http.Get("http://" + ln.Addr().String() + "/plugins/routed/hook")
	if err != nil {
		t.Fatalf("GET /plugins/routed/hook: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hooked" {
		t.Fatalf("body = %q, want hooked", string(body))
	}
}

// C9: a non-loopback listen wraps every route (including /health) in bearer auth —
// no/wrong token is 401, the configured token is 200. The loopback no-auth branch is
// covered by TestHealthEndpointShape (loopback listen, no token, 200).
func TestBearerAuthNonLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	fake := &onDemandPlugin{emit: make(chan core.Envelope, 1), ready: make(chan struct{})}
	// A non-loopback advertised address triggers the middleware; we still Serve on a
	// real loopback listener, which is what the http.Server.Addr would otherwise bind.
	cfg := core.Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300,
		Listen: "192.0.2.1:7788", Tokens: map[string]string{"cli": "s3cret"}}
	sv := core.NewSupervisor(cfg, map[string]core.Factory{
		"faker": func(core.PluginConfig) (core.Plugin, error) { return fake, nil },
	}, core.NewHealthAggregator(cfg.LagThresholdSeconds), core.NewBus())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := server.New(cfg, sv)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	go sv.Run(ctx)

	select {
	case <-fake.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("plugin never started")
	}
	base := "http://" + ln.Addr().String()

	resp, err := http.Get(base + "/health")
	if err != nil {
		t.Fatalf("GET /health (no token): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodGet, base+"/health", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /health (token): %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("token status = %d, want 200", resp2.StatusCode)
	}
}

func writeJSON(ctx context.Context, t *testing.T, c *coderws.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, coderws.MessageText, b); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}

func readType(ctx context.Context, t *testing.T, c *coderws.Conn) string {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var m struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("ws decode %q: %v", string(data), err)
	}
	return m.Type
}
