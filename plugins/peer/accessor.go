package peer

import (
	"context"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/internal/single"
)

// accessor.go bridges the running plugin to the graph `peers` resolver. The plugin
// never imports graph; graph reads this package-level accessor instead.
//
// Decision — a process-level `active` singleton, not core's Resolver.Events seam:
// the `peers` query needs live PLUGIN STATE (per-peer liveness from peer_state),
// but the only reader core hands a plugin's graph resolver is the events channel —
// there is no "give me plugin X" handle. Rather than a core change (LOCKED), the
// plugin publishes itself here on New and the resolver delegates through Peers().
// A pointer swap on New keeps it test-safe: each newTestPlugin/New re-Stores, so a
// test reads its own instance, and a nil/unstarted pointer yields no peers, never a
// panic. Single-process assumption holds — one supergraph binary, one peer plugin.
var active single.Ptr[Plugin]

// PeerView is one peer's liveness for the graph `peers` resolver (a black-box view,
// so graph never imports the plugin's internal types).
type PeerView struct {
	HostID       string
	URL          string
	LastSeenAt   *time.Time
	StaleSince   *time.Time
	LagSeconds   float64
	MirroredKeys int
}

// Peers returns every configured peer's liveness snapshot. The graph `peers`
// resolver delegates here; a nil/unstarted plugin yields no peers rather than a
// panic.
func Peers(ctx context.Context) ([]PeerView, error) {
	p := active.Get()
	if p == nil || p.store == nil {
		return nil, nil
	}
	states, err := p.store.states(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]PeerView, 0, len(states))
	for _, s := range states {
		n, _ := p.store.countForHost(ctx, s.Host)
		out = append(out, PeerView{
			HostID: s.Host, URL: s.URL, LastSeenAt: s.LastSeenAt,
			StaleSince: s.StaleSince, LagSeconds: s.LagSeconds, MirroredKeys: n,
		})
	}
	return out, nil
}
