// Package server builds the process's HTTP surface: the gqlgen GraphQL endpoint
// (POST/GET/WebSocket) and the /health JSON endpoint. It lives outside core/
// because it imports graph (which imports core), so putting it in core would form
// an import cycle. It is the single place that wires the injected resolver funcs
// to the running Supervisor.
package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
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
		Health: func(ctx context.Context) []core.HealthStatus { return sv.Health().Snapshot() },
		Lag: func(ctx context.Context, threshold float64) <-chan core.HealthStatus {
			return lagStream(ctx, sv, threshold)
		},
	}

	gql := handler.New(graph.NewExecutableSchema(graph.Config{Resolvers: res}))
	gql.AddTransport(transport.Options{})
	gql.AddTransport(transport.GET{})
	gql.AddTransport(transport.POST{})
	gql.AddTransport(transport.Websocket{
		KeepAlivePingInterval: 10 * time.Second,
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
		ErrorFunc: func(ctx context.Context, err error) {
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

	return &http.Server{Addr: cfg.Listen, Handler: mux}
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
