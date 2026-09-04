package graph

import (
	"context"
	"testing"
	"time"

	"github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/drewdrewthis/supergraph/core"
)

// TestHealthQueryResolvesInjectedStatuses builds the real executable schema with a
// fake Health func and drives a GraphQL query through it, proving the base seam:
// the schema compiles, the injected func is reached, and the enum serializes to the
// lowercase HealthState values.
func TestHealthQueryResolvesInjectedStatuses(t *testing.T) {
	now := time.Now()
	fake := []core.HealthStatus{
		{Plugin: "alpha", Cursor: "c1", LagSeconds: 1, LastEventAt: &now, State: core.HealthOK},
		{Plugin: "beta", Cursor: "c2", LagSeconds: 99, LastEventAt: &now, State: core.HealthStale},
	}
	res := &Resolver{
		Health: func(ctx context.Context) []core.HealthStatus { return fake },
	}
	srv := handler.NewDefaultServer(NewExecutableSchema(Config{Resolvers: res}))
	c := client.New(srv)

	var resp struct {
		Health []struct {
			Plugin string
			State  string
		}
	}
	c.MustPost(`{ health { plugin state } }`, &resp)

	if len(resp.Health) != 2 {
		t.Fatalf("want 2 health rows, got %d: %+v", len(resp.Health), resp.Health)
	}
	got := map[string]string{}
	for _, h := range resp.Health {
		got[h.Plugin] = h.State
	}
	if got["alpha"] != "ok" {
		t.Errorf("alpha state = %q, want %q", got["alpha"], "ok")
	}
	if got["beta"] != "stale" {
		t.Errorf("beta state = %q, want %q", got["beta"], "stale")
	}
}
