package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coderws "github.com/coder/websocket"
)

// This file pins the behaviour of the not-yet-written `supergraph subscribe`
// command (cmd/supergraph/subscribe.go). It fails to COMPILE until that file
// defines:
//
//   - subscribeOptions struct{ Field string; VarArgs []string; Once bool;
//     Endpoint string; ConfigPath string; MaxRetries int; Backoff time.Duration }
//   - func runSubscribe(ctx context.Context, opts subscribeOptions, stdout, stderr io.Writer) (exitCode int)
//   - func buildSubscription(field string, vars map[string]any) (query string, variables map[string]any, err error)
//   - func wsEndpoint(httpEndpoint string) string
//   - func subscribeAuthHeader(wsURL, cfgPath string) string
//
// See the handoff notes for exact contracts.

// ---------- fake graphql-transport-ws server ----------

// wsFrame is one decoded protocol message from the client.
type wsFrame map[string]any

// fakeSubServer runs one graphql-transport-ws connection per accept, handing each
// connection to connHandler. Non-websocket requests never occur in these tests.
func fakeSubServer(connHandler func(conn *coderws.Conn, r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := coderws.Accept(w, r, &coderws.AcceptOptions{Subprotocols: []string{"graphql-transport-ws"}})
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()
		connHandler(conn, r)
	}))
}

func srvRead(conn *coderws.Conn) (wsFrame, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m wsFrame
	_ = json.Unmarshal(b, &m)
	return m, nil
}

func srvWrite(conn *coderws.Conn, v map[string]any) error {
	b, _ := json.Marshal(v)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return conn.Write(ctx, coderws.MessageText, b)
}

// ackAndSubscribe drives the server side of the handshake: reads connection_init,
// writes connection_ack, reads subscribe and returns it (so the caller can inspect
// id/query) before pushing any next/error/complete frames itself.
func ackAndSubscribe(conn *coderws.Conn) (wsFrame, error) {
	if _, err := srvRead(conn); err != nil { // connection_init
		return nil, err
	}
	if err := srvWrite(conn, map[string]any{"type": "connection_ack"}); err != nil {
		return nil, err
	}
	return srvRead(conn) // subscribe
}

// ---------- AC1: field allowlist ----------

func TestRunSubscribe_UnknownFieldIsUsageErrorExit1(t *testing.T) {
	var stdout, stderr strings.Builder
	code := runSubscribe(context.Background(), subscribeOptions{Field: "bogusField"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want a usage error")
	}
}

func TestRunSubscribe_UnknownFieldNamesNothingWrittenToStdout(t *testing.T) {
	var stdout, stderr strings.Builder
	runSubscribe(context.Background(), subscribeOptions{Field: "issueUpdatedTypo"}, &stdout, &stderr)
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want empty on a rejected field", stdout.String())
	}
}

// ---------- AC3: query building ----------

func TestBuildSubscription_NoArgFieldTmuxEvents(t *testing.T) {
	q, vars, err := buildSubscription("tmuxEvents", map[string]any{})
	if err != nil {
		t.Fatalf("buildSubscription() error: %v", err)
	}
	want := "subscription { tmuxEvents { ts type v key payload } }"
	if q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}
	if len(vars) != 0 {
		t.Fatalf("variables = %v, want empty", vars)
	}
}

func TestBuildSubscription_NoArgFieldCheckRunUpdated(t *testing.T) {
	q, vars, err := buildSubscription("checkRunUpdated", nil)
	if err != nil {
		t.Fatalf("buildSubscription() error: %v", err)
	}
	want := "subscription { checkRunUpdated { ts type v key payload } }"
	if q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}
	if len(vars) != 0 {
		t.Fatalf("variables = %v, want empty", vars)
	}
}

func TestBuildSubscription_NoArgFieldRejectsUnknownVar(t *testing.T) {
	if _, _, err := buildSubscription("tmuxEvents", map[string]any{"owner": "acme"}); err == nil {
		t.Fatal("expected error: tmuxEvents takes no arguments")
	}
}

func TestBuildSubscription_IssueUpdatedWithOwnerAndRepo(t *testing.T) {
	q, vars, err := buildSubscription("issueUpdated", map[string]any{"owner": "acme", "repo": "widgets"})
	if err != nil {
		t.Fatalf("buildSubscription() error: %v", err)
	}
	want := "subscription($owner: String, $repo: String) { issueUpdated(owner: $owner, repo: $repo) { ts type v key payload } }"
	if q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}
	if vars["owner"] != "acme" || vars["repo"] != "widgets" {
		t.Fatalf("variables = %v", vars)
	}
}

func TestBuildSubscription_IssueUpdatedWithNoVarsHasNoDeclarations(t *testing.T) {
	q, vars, err := buildSubscription("issueUpdated", map[string]any{})
	if err != nil {
		t.Fatalf("buildSubscription() error: %v", err)
	}
	want := "subscription { issueUpdated { ts type v key payload } }"
	if q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}
	if len(vars) != 0 {
		t.Fatalf("variables = %v, want empty", vars)
	}
}

func TestBuildSubscription_ClaudeSessionUpdatedWithHostId(t *testing.T) {
	q, vars, err := buildSubscription("claudeSessionUpdated", map[string]any{"hostId": "box1"})
	if err != nil {
		t.Fatalf("buildSubscription() error: %v", err)
	}
	want := "subscription($hostId: String) { claudeSessionUpdated(hostId: $hostId) { ts type v key payload } }"
	if q != want {
		t.Fatalf("query = %q, want %q", q, want)
	}
	if vars["hostId"] != "box1" {
		t.Fatalf("variables = %v", vars)
	}
}

func TestBuildSubscription_RejectsUnsupportedField(t *testing.T) {
	if _, _, err := buildSubscription("notAField", nil); err == nil {
		t.Fatal("expected error for a field outside the allowlist")
	}
}

func TestBuildSubscription_RejectsVarNotInFieldsArgList(t *testing.T) {
	if _, _, err := buildSubscription("issueUpdated", map[string]any{"bogus": "x"}); err == nil {
		t.Fatal("expected error: issueUpdated has no `bogus` argument")
	}
}

// ---------- wsEndpoint ----------

func TestWsEndpoint_HTTPBecomesWS(t *testing.T) {
	got := wsEndpoint("http://127.0.0.1:7788/graphql")
	want := "ws://127.0.0.1:7788/graphql"
	if got != want {
		t.Fatalf("wsEndpoint() = %q, want %q", got, want)
	}
}

func TestWsEndpoint_HTTPSBecomesWSS(t *testing.T) {
	got := wsEndpoint("https://example.test:9999/graphql")
	want := "wss://example.test:9999/graphql"
	if got != want {
		t.Fatalf("wsEndpoint() = %q, want %q", got, want)
	}
}

// ---------- AC2: bearer only on non-loopback with a configured token ----------

func writeTestConfig(t *testing.T, listen, token string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	var b strings.Builder
	b.WriteString("hostId = \"test\"\n")
	b.WriteString("listen = \"" + listen + "\"\n")
	b.WriteString("dataDir = \"" + filepath.Join(dir, "data") + "\"\n")
	if token != "" {
		b.WriteString("[tokens]\nlocal = \"" + token + "\"\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSubscribeAuthHeader_EmptyOnLoopbackEvenWithToken(t *testing.T) {
	cfgPath := writeTestConfig(t, "127.0.0.1:7788", "sekrit")
	got := subscribeAuthHeader("ws://127.0.0.1:9999/graphql", cfgPath)
	if got != "" {
		t.Fatalf("subscribeAuthHeader() = %q, want empty for a loopback ws host", got)
	}
}

func TestSubscribeAuthHeader_BearerOnNonLoopbackWithToken(t *testing.T) {
	cfgPath := writeTestConfig(t, "127.0.0.1:7788", "sekrit")
	got := subscribeAuthHeader("ws://example.test:9999/graphql", cfgPath)
	want := "Bearer sekrit"
	if got != want {
		t.Fatalf("subscribeAuthHeader() = %q, want %q", got, want)
	}
}

func TestSubscribeAuthHeader_EmptyOnNonLoopbackWithNoConfiguredToken(t *testing.T) {
	got := subscribeAuthHeader("ws://example.test:9999/graphql", filepath.Join(t.TempDir(), "missing.toml"))
	if got != "" {
		t.Fatalf("subscribeAuthHeader() = %q, want empty with no token configured", got)
	}
}

// ---------- AC7: protocol handshake ----------

func TestRunSubscribe_SendsConnectionInitThenSubscribeWithID(t *testing.T) {
	var gotInit, gotSub wsFrame
	srv := fakeSubServer(func(conn *coderws.Conn, _ *http.Request) {
		init, err := srvRead(conn)
		if err != nil {
			return
		}
		gotInit = init
		if err := srvWrite(conn, map[string]any{"type": "connection_ack"}); err != nil {
			return
		}
		sub, err := srvRead(conn)
		if err != nil {
			return
		}
		gotSub = sub
		env := map[string]any{"ts": "2026-01-01T00:00:00Z", "type": "tmux.event", "v": float64(1), "key": "k1", "payload": "{}"}
		_ = srvWrite(conn, map[string]any{"id": sub["id"], "type": "next", "payload": map[string]any{"data": map[string]any{"tmuxEvents": env}}})
	})
	defer srv.Close()

	var stdout, stderr strings.Builder
	opts := subscribeOptions{Field: "tmuxEvents", Once: true, Endpoint: srv.URL + "/graphql", ConfigPath: filepath.Join(t.TempDir(), "missing.toml")}
	code := runSubscribe(context.Background(), opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if gotInit["type"] != "connection_init" {
		t.Fatalf("first client frame type = %v, want connection_init", gotInit["type"])
	}
	if gotSub["type"] != "subscribe" {
		t.Fatalf("second client frame type = %v, want subscribe", gotSub["type"])
	}
	if id, _ := gotSub["id"].(string); id == "" {
		t.Fatalf("subscribe frame carries no id: %v", gotSub)
	}
	payload, _ := gotSub["payload"].(map[string]any)
	q, _ := payload["query"].(string)
	if !strings.Contains(q, "tmuxEvents") {
		t.Fatalf("subscribe payload query = %q, want it to reference tmuxEvents", q)
	}
}

// ---------- AC4 + AC5: output shape + --once ----------

func TestRunSubscribe_OncePrintsOneCompactJSONLineThenExitsZero(t *testing.T) {
	srv := fakeSubServer(func(conn *coderws.Conn, _ *http.Request) {
		sub, err := ackAndSubscribe(conn)
		if err != nil {
			return
		}
		env := map[string]any{"ts": "2026-01-01T00:00:00Z", "type": "tmux.event", "v": float64(1), "key": "k1", "payload": "{}"}
		_ = srvWrite(conn, map[string]any{"id": sub["id"], "type": "next", "payload": map[string]any{"data": map[string]any{"tmuxEvents": env}}})
	})
	defer srv.Close()

	var stdout, stderr strings.Builder
	opts := subscribeOptions{Field: "tmuxEvents", Once: true, Endpoint: srv.URL + "/graphql", ConfigPath: filepath.Join(t.TempDir(), "missing.toml")}
	code := runSubscribe(context.Background(), opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}

	out := strings.TrimRight(stdout.String(), "\n")
	lines := strings.Split(out, "\n")
	if len(lines) != 1 || lines[0] == "" {
		t.Fatalf("stdout = %q, want exactly one line", stdout.String())
	}
	if strings.Contains(lines[0], "\n") || strings.Contains(lines[0], "  ") {
		t.Fatalf("stdout line = %q, want compact (no newlines/indentation) JSON", lines[0])
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("stdout line is not valid JSON: %v (%q)", err, lines[0])
	}
	if got["key"] != "k1" || got["type"] != "tmux.event" {
		t.Fatalf("decoded data = %v, want the tmuxEvents envelope fields", got)
	}
}

// ---------- AC6: reconnect with injectable retry/backoff ----------

func TestRunSubscribe_ReconnectsAfterADropThenSucceeds(t *testing.T) {
	var conns int32
	srv := fakeSubServer(func(conn *coderws.Conn, _ *http.Request) {
		n := atomic.AddInt32(&conns, 1)
		sub, err := ackAndSubscribe(conn)
		if err != nil {
			return
		}
		if n == 1 {
			// Simulate a drop: close without ever pushing a next frame.
			return
		}
		env := map[string]any{"ts": "2026-01-01T00:00:00Z", "type": "tmux.event", "v": float64(1), "key": "k2", "payload": "{}"}
		_ = srvWrite(conn, map[string]any{"id": sub["id"], "type": "next", "payload": map[string]any{"data": map[string]any{"tmuxEvents": env}}})
	})
	defer srv.Close()

	var stdout, stderr strings.Builder
	opts := subscribeOptions{
		Field: "tmuxEvents", Once: true, Endpoint: srv.URL + "/graphql",
		ConfigPath: filepath.Join(t.TempDir(), "missing.toml"),
		MaxRetries: 2, Backoff: time.Millisecond,
	}
	code := runSubscribe(context.Background(), opts, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 after a successful reconnect; stderr=%q", code, stderr.String())
	}
	if atomic.LoadInt32(&conns) < 2 {
		t.Fatalf("server saw %d connection(s), want at least 2 (a drop then a reconnect)", conns)
	}
	if !strings.Contains(stdout.String(), `"key":"k2"`) {
		t.Fatalf("stdout = %q, want the event pushed on the second connection", stdout.String())
	}
}

func TestRunSubscribe_ExhaustsRetriesExitsTwoWithStderr(t *testing.T) {
	srv := fakeSubServer(func(conn *coderws.Conn, _ *http.Request) {
		// Every connection drops immediately after the handshake, never subscribing.
		_, _ = srvRead(conn) // connection_init
		_ = srvWrite(conn, map[string]any{"type": "connection_ack"})
	})
	defer srv.Close()

	var stdout, stderr strings.Builder
	opts := subscribeOptions{
		Field: "tmuxEvents", Once: true, Endpoint: srv.URL + "/graphql",
		ConfigPath: filepath.Join(t.TempDir(), "missing.toml"),
		MaxRetries: 1, Backoff: time.Millisecond,
	}
	code := runSubscribe(context.Background(), opts, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stderr.Len() == 0 {
		t.Fatal("stderr is empty, want an error naming the exhausted retries")
	}
}

func TestRunSubscribe_UnreachableEndpointExitsTwo(t *testing.T) {
	var stdout, stderr strings.Builder
	opts := subscribeOptions{
		Field: "tmuxEvents", Once: true, Endpoint: "http://127.0.0.1:1/graphql",
		ConfigPath: filepath.Join(t.TempDir(), "missing.toml"),
		MaxRetries: 1, Backoff: time.Millisecond,
	}
	code := runSubscribe(context.Background(), opts, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 for a connection that is never acceptable", code)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}
