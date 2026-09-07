package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	coderws "github.com/coder/websocket"

	"github.com/drewdrewthis/supergraph/core"
)

// defaultSubscribeMaxRetries and defaultSubscribeBackoff apply when the caller
// leaves subscribeOptions.MaxRetries/Backoff at their zero value, so a bare
// `supergraph subscribe <field>` reconnects a sensible number of times without
// requiring every caller to spell out retry tuning.
const (
	defaultSubscribeMaxRetries = 3
	defaultSubscribeBackoff    = 500 * time.Millisecond
)

// subscriptionFields is the allowlist of plugin subscription fields `subscribe`
// may dial, each mapped to its permitted String argument names. Rejecting
// anything outside this set (AC-SUB-1) keeps the CLI from posting an arbitrary
// subscription to core's /graphql over an open websocket.
var subscriptionFields = map[string][]string{
	"issueUpdated":         {"owner", "repo"},
	"checkRunUpdated":      {},
	"claudeSessionUpdated": {"hostId"},
	"tmuxEvents":           {},
}

// subscribeOptions carries `supergraph subscribe`'s resolved flags into
// runSubscribe, decoupled from cobra so unit tests can drive it directly.
type subscribeOptions struct {
	Field      string
	VarArgs    []string
	Once       bool
	Endpoint   string
	ConfigPath string
	MaxRetries int           // 0 -> defaultSubscribeMaxRetries
	Backoff    time.Duration // 0 -> defaultSubscribeBackoff
	// ReadyNotify makes subscribeOnce print a single "subscribed" line to stderr
	// once the subscribe frame has been sent, so an orchestrating caller can wait
	// until the server has registered the subscription before triggering the event
	// it wants to observe. Without it a fire-and-forget caller races the handshake
	// and can emit its trigger before the bus subscription exists (live pub-sub has
	// no replay), so the first event is silently missed. Hidden; used by the BDD
	// harness's background --once scenario.
	ReadyNotify bool
}

// subscribeCmd registers `supergraph subscribe <field>`, mirroring queryCmd's
// flag conventions (--var, --endpoint) plus subscription-specific --once and
// retry tuning. --config is the root's persistent flag (configPath), same as
// query/schema.
func subscribeCmd() *cobra.Command {
	var endpoint string
	var varArgs []string
	var once bool
	var maxRetries int
	var backoff time.Duration
	var readyNotify bool
	cmd := &cobra.Command{
		Use:   "subscribe <field>",
		Short: "Subscribe to a plugin GraphQL field over graphql-transport-ws and print each event as JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			opts := subscribeOptions{
				Field:       args[0],
				VarArgs:     varArgs,
				Once:        once,
				Endpoint:    endpoint,
				ConfigPath:  configPath,
				MaxRetries:  maxRetries,
				Backoff:     backoff,
				ReadyNotify: readyNotify,
			}
			// runSubscribe's exit codes (0/1/2) are load-bearing (AC-SUB-1,
			// AC-SUB-5, AC-SUB-6); os.Exit here bypasses cobra's own
			// RunE-error-means-exit-1 handling in main so those codes reach
			// the shell unmodified.
			os.Exit(runSubscribe(c.Context(), opts, c.OutOrStdout(), c.ErrOrStderr()))
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&varArgs, "var", nil,
		"subscription variable as key=value (repeatable)")
	cmd.Flags().BoolVar(&once, "once", false, "exit 0 after printing the first event")
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultQueryEndpoint,
		"GraphQL endpoint URL (overrides the config's listen address)")
	cmd.Flags().IntVar(&maxRetries, "max-retries", 0,
		"reconnect attempts after a drop before giving up (0 = default 3)")
	cmd.Flags().DurationVar(&backoff, "backoff", 0,
		"delay between reconnect attempts (0 = default 500ms)")
	cmd.Flags().BoolVar(&readyNotify, "ready-notify", false,
		"print a `subscribed` line to stderr once the subscription is established")
	_ = cmd.Flags().MarkHidden("ready-notify")
	return cmd
}

// runSubscribe validates the field/vars, resolves the endpoint and auth header,
// and dials the graphql-transport-ws connection with reconnect/backoff. It never
// panics or calls os.Exit itself so tests can call it directly; the returned
// code is the process's intended exit status (0 ok, 1 usage error, 2 exhausted
// retries).
func runSubscribe(ctx context.Context, opts subscribeOptions, stdout, stderr io.Writer) (exitCode int) {
	vars, err := parseVars(opts.VarArgs)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}
	query, variables, err := buildSubscription(opts.Field, vars)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error:", err)
		return 1
	}

	httpEndpoint := resolveEndpoint(opts.Endpoint, opts.ConfigPath)
	wsURL := wsEndpoint(httpEndpoint)
	authHeader := subscribeAuthHeader(wsURL, opts.ConfigPath)

	maxRetries := opts.MaxRetries
	if maxRetries == 0 {
		maxRetries = defaultSubscribeMaxRetries
	}
	backoff := opts.Backoff
	if backoff == 0 {
		backoff = defaultSubscribeBackoff
	}

	maxAttempts := maxRetries + 1
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := subscribeOnce(ctx, wsURL, authHeader, opts.Field, query, variables, opts.Once, opts.ReadyNotify, stdout, stderr); err != nil {
			lastErr = err
			if attempt == maxAttempts-1 {
				break
			}
			select {
			case <-ctx.Done():
				_, _ = fmt.Fprintln(stderr, "error:", ctx.Err())
				return 2
			case <-time.After(backoff):
			}
			continue
		}
		return 0
	}
	_, _ = fmt.Fprintf(stderr, "error: subscribe %q: exhausted %d retries: %v\n", opts.Field, maxRetries, lastErr)
	return 2
}

// buildSubscription renders the subscription document for field, declaring only
// the variables actually present in vars (sorted for a deterministic query
// string), and returns the filtered variables map alongside it. It rejects any
// field outside subscriptionFields and any var key the field doesn't declare as
// an argument (AC-SUB-1, AC-SUB-3).
func buildSubscription(field string, vars map[string]any) (query string, variables map[string]any, err error) {
	allowed, ok := subscriptionFields[field]
	if !ok {
		return "", nil, fmt.Errorf("unknown subscription field %q", field)
	}
	allowedSet := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		allowedSet[a] = true
	}

	keys := make([]string, 0, len(vars))
	for k := range vars {
		if !allowedSet[k] {
			if len(allowed) == 0 {
				return "", nil, fmt.Errorf("field %q takes no arguments (got %q)", field, k)
			}
			return "", nil, fmt.Errorf("field %q has no argument %q", field, k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)

	variables = make(map[string]any, len(keys))
	for _, k := range keys {
		variables[k] = vars[k]
	}

	if len(keys) == 0 {
		return fmt.Sprintf("subscription { %s { ts type v key payload } }", field), variables, nil
	}

	decls := make([]string, len(keys))
	fieldArgs := make([]string, len(keys))
	for i, k := range keys {
		decls[i] = fmt.Sprintf("$%s: String", k)
		fieldArgs[i] = fmt.Sprintf("%s: $%s", k, k)
	}
	query = fmt.Sprintf("subscription(%s) { %s(%s) { ts type v key payload } }",
		strings.Join(decls, ", "), field, strings.Join(fieldArgs, ", "))
	return query, variables, nil
}

// wsEndpoint rewrites an http(s) GraphQL endpoint to its ws(s) equivalent, same
// host/port/path, for dialing graphql-transport-ws.
func wsEndpoint(httpEndpoint string) string {
	switch {
	case strings.HasPrefix(httpEndpoint, "https://"):
		return "wss://" + strings.TrimPrefix(httpEndpoint, "https://")
	case strings.HasPrefix(httpEndpoint, "http://"):
		return "ws://" + strings.TrimPrefix(httpEndpoint, "http://")
	default:
		return httpEndpoint
	}
}

// subscribeAuthHeader returns the Authorization header value to present on the
// websocket dial: empty for a loopback wsURL (the zero-config local case, same
// as hookEndpoint's loopback bind), otherwise "Bearer <token>" for the first
// configured token by sorted name, or empty when no config/token is available.
func subscribeAuthHeader(wsURL, cfgPath string) string {
	host := wsURL
	if u, err := url.Parse(wsURL); err == nil && u.Host != "" {
		host = u.Host
	}
	if core.ListenIsLoopback(host) {
		return ""
	}
	cfg, err := core.LoadConfig(cfgPath)
	if err != nil || len(cfg.Tokens) == 0 {
		return ""
	}
	names := make([]string, 0, len(cfg.Tokens))
	for name := range cfg.Tokens {
		names = append(names, name)
	}
	sort.Strings(names)
	return "Bearer " + cfg.Tokens[names[0]]
}

// subscribeOnce dials one graphql-transport-ws connection, runs the
// connection_init/subscribe handshake (AC-SUB-7), and prints each pushed
// event's data.<field> object as one compact JSON line to stdout. With once
// set it returns nil as soon as the first event prints; otherwise it keeps
// printing until the server sends complete or the connection errors. Any
// dial, handshake, or read failure is returned so the caller's retry loop can
// reconnect.
func subscribeOnce(ctx context.Context, wsURL, authHeader, field, query string, variables map[string]any, once, readyNotify bool, stdout, stderr io.Writer) error {
	dialOpts := &coderws.DialOptions{Subprotocols: []string{"graphql-transport-ws"}}
	if authHeader != "" {
		dialOpts.HTTPHeader = http.Header{"Authorization": {authHeader}}
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	conn, _, err := coderws.Dial(dctx, wsURL, dialOpts)
	cancel()
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer func() { _ = conn.Close(coderws.StatusNormalClosure, "") }()

	if err := wsSubWrite(ctx, conn, map[string]any{"type": "connection_init", "payload": map[string]any{}}); err != nil {
		return err
	}
	if err := waitForAck(ctx, conn); err != nil {
		return err
	}

	const subID = "1"
	sub := map[string]any{
		"id":      subID,
		"type":    "subscribe",
		"payload": map[string]any{"query": query, "variables": variables},
	}
	if err := wsSubWrite(ctx, conn, sub); err != nil {
		return err
	}
	// The subscribe frame is flushed and the server has acked connection_init, so
	// the server's read loop will register the bus subscription before any event a
	// caller now triggers over a fresh HTTP round-trip can be published.
	if readyNotify {
		_, _ = fmt.Fprintln(stderr, "subscribed")
	}

	for {
		m, err := wsSubRead(ctx, conn)
		if err != nil {
			return err
		}
		switch m["type"] {
		case "next":
			if err := printEvent(stdout, m, field); err != nil {
				return err
			}
			if once {
				return nil
			}
		case "error":
			return fmt.Errorf("subscription error: %v", m["payload"])
		case "complete":
			return nil
		}
	}
}

// waitForAck reads frames until connection_ack, treating an early error frame
// as a handshake failure.
func waitForAck(ctx context.Context, conn *coderws.Conn) error {
	for {
		m, err := wsSubRead(ctx, conn)
		if err != nil {
			return err
		}
		switch m["type"] {
		case "connection_ack":
			return nil
		case "error":
			return fmt.Errorf("connection_init error: %v", m["payload"])
		}
	}
}

// printEvent extracts payload.data.<field> from a `next` frame and writes it as
// one compact JSON line, the CLI's whole output contract (AC-SUB-4).
func printEvent(stdout io.Writer, frame map[string]any, field string) error {
	payload, _ := frame["payload"].(map[string]any)
	data, _ := payload["data"].(map[string]any)
	line, err := json.Marshal(data[field])
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if _, err := stdout.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

func wsSubWrite(ctx context.Context, c *coderws.Conn, v map[string]any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return c.Write(wctx, coderws.MessageText, b)
}

func wsSubRead(ctx context.Context, c *coderws.Conn) (map[string]any, error) {
	_, b, err := c.Read(ctx)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode frame: %w", err)
	}
	return m, nil
}
