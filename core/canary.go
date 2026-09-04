// Package core is the binary shell: the ingest envelope, plugin contract, SQLite
// base, supervisor, health aggregation, and canary. Plugins compile against this
// package and never edit it (PRD §6, the S5 zero-core-edit seam).
package core

import (
	"context"
	"time"
)

// Canary fires a synthetic event for every running plugin on a fixed interval,
// through the SAME emit path a real event uses. Its purpose is a POSITIVE liveness
// signal: a canary that never fires is a broken canary, not an all-clear (PRD §6
// F6). Because the synthetic event advances health.lastEventAt, a plugin whose
// canary is suppressed (StopCanary) will visibly cross the lag threshold into stale.
type Canary struct {
	sv       *Supervisor
	interval time.Duration
}

// NewCanary builds a canary firing every intervalSeconds against sv's plugins.
func NewCanary(sv *Supervisor, intervalSeconds float64) *Canary {
	return &Canary{
		sv:       sv,
		interval: time.Duration(intervalSeconds * float64(time.Second)),
	}
}

// Run ticks until ctx is cancelled, firing a canary envelope for each non-suppressed
// running plugin on every tick.
func (c *Canary) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, name := range c.sv.canaryTargets() {
				_ = c.sv.fireCanary(ctx, name)
			}
		}
	}
}
