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
		Health: func(_ context.Context) []core.HealthStatus { return fake },
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

// TestClaudeSessionUpdatedHostFilter proves the hostId arg filters the subscription by
// the envelope key's "@<host>" suffix (M2): only boxA's session + instance envelopes
// pass when hostId=boxA; boxB is dropped.
func TestClaudeSessionUpdatedHostFilter(t *testing.T) {
	ch := make(chan core.Envelope, 3)
	ch <- core.Envelope{Source: "claude", Type: "claude.session.updated", Key: "session:s1@boxA"}
	ch <- core.Envelope{Source: "claude", Type: "claude.session.updated", Key: "session:s2@boxB"}
	ch <- core.Envelope{Source: "claude", Type: "claude.instance.updated", Key: "instance:%23@boxA"}
	close(ch)

	res := &Resolver{Events: func(_ context.Context, source string) <-chan core.Envelope {
		if source != "claude" {
			t.Fatalf("Events source = %q, want claude", source)
		}
		return ch
	}}
	host := "boxA"
	out, err := (&subscriptionResolver{res}).ClaudeSessionUpdated(context.Background(), &host)
	if err != nil {
		t.Fatalf("resolver err: %v", err)
	}
	var keys []string
	for e := range out {
		keys = append(keys, e.Key)
	}
	want := []string{"session:s1@boxA", "instance:%23@boxA"}
	if len(keys) != len(want) || keys[0] != want[0] || keys[1] != want[1] {
		t.Fatalf("filtered keys = %v, want %v (boxB dropped)", keys, want)
	}
}

// TestIssueUpdatedRepoRequiresOwner proves repo can't be silently ignored: passing
// repo without owner is a GraphQL error, not a keyless/unfiltered subscription.
func TestIssueUpdatedRepoRequiresOwner(t *testing.T) {
	res := &Resolver{Events: func(_ context.Context, _ string) <-chan core.Envelope {
		t.Fatal("Events should not be called when arg validation fails")
		return nil
	}}
	repo := "supergraph"
	out, err := (&subscriptionResolver{res}).IssueUpdated(context.Background(), nil, &repo)
	if err == nil {
		t.Fatal("want error for repo without owner, got nil")
	}
	if out != nil {
		t.Fatalf("want nil channel on error, got %v", out)
	}
}
