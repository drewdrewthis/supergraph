package graph

import (
	"context"

	"github.com/drewdrewthis/supergraph/core"
)

// Resolver is the DI root for the graph package. It holds only funcs supplied by
// core at server-build time; graph/ carries no business logic, so plugin fields
// (added via extend + follow-schema stubs) delegate through core, never inline.
type Resolver struct {
	// Health returns the current per-plugin freshness snapshots.
	Health func(ctx context.Context) []core.HealthStatus
	// Lag streams a plugin's health whenever its lag crosses threshold seconds.
	Lag func(ctx context.Context, threshold float64) <-chan core.HealthStatus
	// Events streams envelopes emitted by one source (e.g. "template"), filtered
	// server-side so a plugin's subscription resolver never touches the Bus
	// directly. Generic across sources so this one field serves every plugin's
	// event subscription, keeping graph/ free of per-plugin business logic.
	Events func(ctx context.Context, source string) <-chan core.Envelope
}
