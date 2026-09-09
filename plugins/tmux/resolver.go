package tmux

import (
	"context"
	"sort"
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

// WindowRow is the projection of a session's non-stale panes grouped by window
// (#27). Windows are derived, never stored (the EDR's YAGNI call on tmux_windows
// holds): name/active come from the group's panes, and a window with zero live panes
// is never emitted.
type WindowRow struct {
	HostID, Key, Session, Name string
	Index                      int
	Active                     bool
	Panes                      []PaneRow
}

// Windows returns host's windows for session, grouping non-stale panes by
// (session, window) with deterministic ordering by index. A window whose last live
// pane is gone drops out (AC-TMUX-NESTING-STALE); every pane it returns is one
// tmuxPanes also returns.
func Windows(ctx context.Context, host, session string) ([]WindowRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	panes, err := p.store.scanPanes(ctx, host)
	if err != nil {
		return nil, err
	}
	return groupWindows(panes)[session], nil
}

// WindowsBySession returns the window projection for every session on host in ONE
// pane scan, keyed by session name. The sessions resolver needs the nesting for
// every session at once, and a per-session Windows() call would rescan the whole
// pane table per session (#27 N+1).
func WindowsBySession(ctx context.Context, host string) (map[string][]WindowRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	panes, err := p.store.scanPanes(ctx, host)
	if err != nil {
		return nil, err
	}
	return groupWindows(panes), nil
}

// groupWindows is the shared grouping pass behind Windows and WindowsBySession:
// skip stale panes, group live ones by (session, window), and order each session's
// windows ascending by index.
func groupWindows(panes []PaneRow) map[string][]WindowRow {
	type key struct {
		session string
		window  int
	}
	byKey := map[key]*WindowRow{}
	order := map[string][]int{}
	for _, pr := range panes {
		if pr.StaleSince != nil {
			continue
		}
		k := key{pr.Session, pr.Window}
		w, ok := byKey[k]
		if !ok {
			w = &WindowRow{
				HostID: pr.HostID, Key: windowKey(pr.Session, pr.Window, pr.HostID),
				Session: pr.Session, Index: pr.Window, Name: pr.WindowName, Active: pr.WindowActive,
			}
			byKey[k] = w
			order[pr.Session] = append(order[pr.Session], pr.Window)
		}
		w.Panes = append(w.Panes, pr)
	}
	out := map[string][]WindowRow{}
	for session, indices := range order {
		sort.Ints(indices)
		rows := make([]WindowRow, 0, len(indices))
		for _, i := range indices {
			rows = append(rows, *byKey[key{session, i}])
		}
		out[session] = rows
	}
	return out
}

// PaneForBranch returns panes whose session worktree is on branch (issue↔branch).
func PaneForBranch(ctx context.Context, branch string) ([]PaneRow, error) {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil, nil
	}
	return p.store.panesForBranch(ctx, branch)
}
