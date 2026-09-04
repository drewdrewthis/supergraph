// Package server builds the process's HTTP surface: the gqlgen GraphQL endpoint
// (POST/GET/WebSocket) and the /health JSON endpoint. It lives outside core/
// because it imports graph (which imports core), so putting it in core would form
// an import cycle. It is the single place that wires the injected resolver funcs
// to the running Supervisor.
package server

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	coderws "github.com/coder/websocket"

	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/99designs/gqlgen/graphql/handler/extension"
	"github.com/99designs/gqlgen/graphql/handler/lru"
	"github.com/99designs/gqlgen/graphql/handler/transport"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/graph"
)

// lagPollInterval is how often the pluginLag stream re-evaluates the snapshot in
// the absence of a bus event, so a plugin that stops emitting still crosses the
// threshold and pushes a stale status without needing a fresh event to wake it.
const lagPollInterval = time.Second

// New builds the process HTTP server: gqlgen at /graphql and the health snapshot
// at GET /health, bound to cfg.Listen. The resolver's data funcs read straight
// from the live Supervisor so the GraphQL view and /health share one source.
func New(cfg core.Config, sv *core.Supervisor) *http.Server {
	res := &graph.Resolver{
		Health: func(_ context.Context) []core.HealthStatus { return sv.Health().Snapshot() },
		Lag: func(ctx context.Context, threshold float64) <-chan core.HealthStatus {
			return lagStream(ctx, sv, threshold)
		},
		Events: func(ctx context.Context, source string) <-chan core.Envelope {
			return EventsChannel(ctx, sv, source)
		},
	}

	gql := handler.New(graph.NewExecutableSchema(graph.Config{Resolvers: res}))
	gql.AddTransport(transport.Options{})
	gql.AddTransport(transport.GET{})
	gql.AddTransport(transport.POST{})
	gql.AddTransport(transport.Websocket{
		// Set BOTH keepalive intervals: KeepAlivePingInterval drives the legacy
		// graphql-ws `ka` frames, PingPongInterval drives graphql-transport-ws
		// ping/pong. A client speaking transport-ws with only KeepAlivePingInterval
		// set hits its own unset 30s read deadline every cycle and disconnects —
		// see sol.2026-09-03-gqlgen-daemon-missing-pingponginterval-kills-graphql-transport-ws-subscriptions.
		KeepAlivePingInterval: 10 * time.Second,
		PingPongInterval:      10 * time.Second,
		// PLUGIN-TIER RULE: gqlgen's websocket transport builds one InitFunc/loader
		// context at connection OPEN and reuses it for every operation on that
		// socket for the socket's lifetime. Do NOT capture request-scoped state
		// (dataloaders, per-op caches) here or in an InitFunc: a long-lived
		// subscription carries many distinct ops over one socket, so anything cached
		// at connect serves the first op's data to later ops. Build per-operation
		// state per operation, not per connection. No dataloaders exist yet; when
		// one is added, obey this — see
		// sol.2026-09-04-gqlgen-websocket-loader-captured-at-connect-not-per-op.
		// coder/websocket is gqlgen's default impl; InsecureSkipVerify disables
		// origin checks — acceptable because the server only binds loopback.
		Implementation: transport.CoderWebsocketImplementation{
			AcceptOptions: coderws.AcceptOptions{InsecureSkipVerify: true},
		},
		// A normal peer close is not an error: a client disconnect races the
		// server's in-flight write and surfaces as a closed-connection / EOF /
		// normal-close-status / cancelled-context error. Drop those so a routine
		// disconnect is never logged as a server error (AC-CORE-8); anything else
		// is a real fault and is logged.
		ErrorFunc: func(_ context.Context, err error) {
			if isNormalClose(err) {
				return
			}
			log.Printf("server: websocket error: %v", err)
		},
	})
	gql.SetQueryCache(lru.New[*ast.QueryDocument](1000))
	gql.Use(extension.Introspection{})

	mux := http.NewServeMux()
	mux.Handle("/graphql", gql)
	mux.HandleFunc("GET /health", sv.Health().ServeHTTP)
	// Plugin HTTP surfaces (e.g. a webhook receiver) are dispatched per-request under
	// /plugins/<name>/ so a new HTTP source needs zero core edit (S5).
	mux.Handle("/plugins/", pluginRoutesHandler(sv))

	// A non-loopback bind exposes the API off-box, so wrap every route (including
	// /health) in bearer auth. Loopback binds stay open for the zero-config local
	// case. core.ListenIsLoopback is the shared predicate with LoadConfig, so the
	// mandatory-token config check and this middleware never disagree.
	var handler http.Handler = mux
	if !core.ListenIsLoopback(cfg.Listen) {
		handler = bearerAuth(mux, cfg.Tokens)
	}

	// ReadHeaderTimeout bounds how long a client may take to send request headers,
	// closing the Slowloris hole gosec G112 flags even though we bind loopback.
	return &http.Server{Addr: cfg.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
}

// pluginRoutesHandler dispatches /plugins/<name>/<pattern> to a running plugin's
// HTTPRoutes handler. It resolves the plugin per-request (not at server build) so
// routes work even though plugins start after server.New returns. An unknown plugin,
// a plugin without HTTPRoutes, or an unmatched pattern is a 404.
func pluginRoutesHandler(sv *core.Supervisor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/plugins/"), "/")
		if name == "" {
			http.NotFound(w, r)
			return
		}
		p, ok := sv.Plugins()[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		hr, ok := p.(core.HTTPRoutes)
		if !ok {
			http.NotFound(w, r)
			return
		}
		// Build a private sub-mux under this plugin's prefix so the plugin's declared
		// ServeMux patterns match as written. A leading slash on the pattern is
		// optional — it is normalised so both "/hook" and "hook" mount identically.
		sub := http.NewServeMux()
		for pattern, h := range hr.Routes() {
			sub.Handle("/plugins/"+name+"/"+strings.TrimPrefix(pattern, "/"), h)
		}
		sub.ServeHTTP(w, r)
	})
}

// bearerAuth guards next with a bearer-token check: the Authorization header must be
// `Bearer <t>` matching one configured token, compared in constant time so a wrong
// token leaks no timing signal. It never logs token values. Used only for
// non-loopback binds (see New).
func bearerAuth(next http.Handler, tokens map[string]string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		h := r.Header.Get("Authorization")
		if strings.HasPrefix(h, prefix) && tokenMatches(h[len(prefix):], tokens) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	})
}

// tokenMatches reports whether got equals any configured token. It checks every
// entry (no early return) so the comparison time does not reveal how many tokens
// exist or which matched.
func tokenMatches(got string, tokens map[string]string) bool {
	match := false
	for _, want := range tokens {
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			match = true
		}
	}
	return match
}

// isNormalClose reports whether err is a routine websocket teardown rather than a
// server fault: a closed connection, EOF, a normal/going-away close status, or a
// cancelled context — all of which a client disconnect can produce.
func isNormalClose(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrClosedPipe) || errors.Is(err, context.Canceled) {
		return true
	}
	switch coderws.CloseStatus(err) {
	case coderws.StatusNormalClosure, coderws.StatusGoingAway, coderws.StatusNoStatusRcvd:
		return true
	}
	return false
}

// lagStream pushes any plugin whose lag exceeds threshold seconds. It reacts to
// both a bus event (immediate re-evaluation the moment something is emitted) and a
// 1s poll (so time-only staleness with no new event still fires). The channel
// closes when ctx is cancelled, which the gqlgen subscription runtime uses to end
// the client stream.
func lagStream(ctx context.Context, sv *core.Supervisor, threshold float64) <-chan core.HealthStatus {
	out := make(chan core.HealthStatus)
	sub := sv.Bus().Subscribe(ctx)
	go func() {
		defer close(out)
		ticker := time.NewTicker(lagPollInterval)
		defer ticker.Stop()
		push := func() bool {
			for _, hs := range sv.Health().Snapshot() {
				if hs.LagSeconds <= threshold {
					continue
				}
				select {
				case out <- hs:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-sub:
				if !ok {
					return
				}
				if !push() {
					return
				}
			case <-ticker.C:
				if !push() {
					return
				}
			}
		}
	}()
	return out
}

// EventsChannel returns a channel of envelopes emitted by source (or all sources
// when source is ""), for a plugin's own subscription resolver to reuse instead of
// re-implementing bus fan-out. It closes when ctx is cancelled.
func EventsChannel(ctx context.Context, sv *core.Supervisor, source string) <-chan core.Envelope {
	out := make(chan core.Envelope)
	sub := sv.Bus().Subscribe(ctx)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-sub:
				if !ok {
					return
				}
				if source != "" && e.Source != source {
					continue
				}
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}
