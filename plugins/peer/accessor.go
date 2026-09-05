package peer

import (
	"context"
	"time"

	"github.com/drewdrewthis/supergraph/plugins/internal/single"
)

// active is the running plugin instance, published for graph/ resolvers; see plugins/internal/single.Ptr for the one-instance-per-process convention.
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
