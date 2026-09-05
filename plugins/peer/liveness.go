package peer

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// liveness.go runs one goroutine per configured peer. It keeps a WS-client
// subscription to the remote's base /graphql pluginLag stream: a live subscription
// is the liveness signal (a healthy remote pushes nothing), a pushed frame mirrors
// the remote's worst lag, and a drop/dial-fail/401 sets staleSince and reconnects
// with exponential backoff (D7). staleSince is preserved at the first-failure time
// across the whole outage and cleared only on a fresh connect.

// runLiveness supervises one peer's pluginLag subscription until ctx is done.
func (p *Plugin) runLiveness(ctx context.Context, pr peerCfg) {
	backoff := backoffInitial
	everUp := false
	for ctx.Err() == nil {
		err := p.subscribe(ctx, pr, p.cfg.staleThreshold.Seconds(),
			func() {
				everUp = true
				backoff = backoffInitial
				_ = p.store.markSeen(ctx, pr.HostID, p.now(), 0)
			},
			func(lag float64) { _ = p.store.markSeen(ctx, pr.HostID, p.now(), lag) },
		)
		if ctx.Err() != nil {
			return
		}
		// Dropped, dial-failed, or 401. Record staleSince at the first failure and
		// emit peer.stale ONLY on a genuine up→down transition — a peer that was
		// never reachable has no transition, so it emits nothing and the plugin's
		// /health lastEventAt stays null (AC-PEER-HEALTH, grounded).
		if trans, _ := p.store.markStale(ctx, pr.HostID, p.now()); trans && everUp {
			p.emitStale(ctx, pr.HostID)
		}
		var ua errUnauthorized
		if errors.As(err, &ua) {
			log.Printf("peer: %s rejected token (401)", pr.HostID)
			everUp = false // a 401 is not a live peer; do not treat the next drop as a transition
		}
		if !p.sleep(ctx, backoff) {
			return
		}
		if backoff *= 2; backoff > p.cfg.backoffMax {
			backoff = p.cfg.backoffMax
		}
	}
}

// emitStale emits one peer.stale envelope for an up→down transition (F8 signal; a
// local poller of the bus can alert on it).
func (p *Plugin) emitStale(ctx context.Context, host string) {
	payload, _ := json.Marshal(map[string]any{"staleSince": p.now().UTC().Format(time.RFC3339Nano)})
	p.doEmit(ctx, core.Envelope{
		TS: p.now(), Source: "peer", Type: "peer.stale", V: 1,
		Key: "peer:" + host, Payload: payload,
	})
}

// backoffInitial is the first reconnect delay; it doubles up to cfg.backoffMax.
const backoffInitial = 500 * time.Millisecond

// sleepCtx sleeps for d or until ctx is done, reporting false when ctx ended.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
