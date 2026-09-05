package tmux

import (
	"context"
	"time"
)

// SlotRow is the free/busy projection over one pane (D5): a Slot has no key of its
// own, it points at the pane it projects. A pane offered as free must be both an
// idle shell AND not stale (a killed pane leaves the free set — AC-TMUX-PANE-DEATH).
type SlotRow struct {
	HostID, Kind, PaneKey string
	Free                  bool
	StaleSince            *time.Time
}

// Sessions returns the cached sessions for host (empty when the plugin is not yet
// running). It is the accessor graph/tmux.resolvers.go delegates to.
func Sessions(ctx context.Context, host string) ([]SessionRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	return p.store.scanSessions(ctx, host)
}

// Panes returns the cached panes for host.
func Panes(ctx context.Context, host string) ([]PaneRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	return p.store.scanPanes(ctx, host)
}

// Slots projects every pane to a Slot; freeOnly keeps only the panes that are idle
// shells and not stale (the freeSlots op, the local warm-read half of S2).
func Slots(ctx context.Context, host string, freeOnly bool) ([]SlotRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	panes, err := p.store.scanPanes(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]SlotRow, 0, len(panes))
	for _, pr := range panes {
		free := pr.Free && pr.StaleSince == nil
		if freeOnly && !free {
			continue
		}
		out = append(out, SlotRow{HostID: pr.HostID, Kind: p.cfg.slotKind, PaneKey: pr.Key, Free: free, StaleSince: pr.StaleSince})
	}
	return out, nil
}

// PaneForBranch returns panes whose session worktree is on branch (issue↔branch).
func PaneForBranch(ctx context.Context, branch string) ([]PaneRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	return p.store.panesForBranch(ctx, branch)
}
