package graph

// Non-resolver helpers for the tmux resolvers. They live outside
// tmux.resolvers.go because gqlgen's follow-schema regeneration preserves only
// resolver METHOD bodies and would otherwise strip (or mis-place) free functions.
import (
	"context"

	"github.com/drewdrewthis/supergraph/graph/model"
	"github.com/drewdrewthis/supergraph/plugins/tmux"
)

// hostArg maps the optional hostId argument to the "" (all hosts) the plugin
// accessors expect.
func hostArg(h *string) string {
	if h == nil {
		return ""
	}
	return *h
}

// optStr maps an empty string to a null GraphQL field.
func optStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func slotsModel(ctx context.Context, host string, freeOnly bool) ([]model.Slot, error) {
	rows, err := tmux.Slots(ctx, host, freeOnly)
	if err != nil {
		return nil, err
	}
	out := make([]model.Slot, 0, len(rows))
	for _, s := range rows {
		out = append(out, model.Slot{HostID: s.HostID, Kind: s.Kind, Free: s.Free, PaneKey: optStr(s.PaneKey), StaleSince: s.StaleSince})
	}
	return out, nil
}

func panesToModel(rows []tmux.PaneRow) []model.TmuxPane {
	out := make([]model.TmuxPane, 0, len(rows))
	for _, p := range rows {
		out = append(out, model.TmuxPane{
			HostID: p.HostID, Key: p.Key, Session: p.Session, Window: p.Window, Pane: p.Pane,
			Pid: p.Pid, Cmd: p.Cmd, Path: optStr(p.Path), Active: p.Active, Free: p.Free,
			PaneID: p.PaneID, StaleSince: p.StaleSince,
		})
	}
	return out
}

// windowsToModel maps already-grouped window rows to the generated model — a pure
// mapping, no ctx/store access, so TmuxSessions can scan the pane table once for
// every session instead of once per session (#27 N+1).
func windowsToModel(rows []tmux.WindowRow) []model.TmuxWindow {
	out := make([]model.TmuxWindow, 0, len(rows))
	for _, w := range rows {
		out = append(out, model.TmuxWindow{
			HostID: w.HostID, Key: w.Key, Session: w.Session, Index: w.Index,
			Name: optStr(w.Name), Active: w.Active, Panes: panesToModel(w.Panes),
		})
	}
	return out
}

// sessionToModel is the single TmuxSession mapper. Both the tmuxSessions query and
// the git plugin's Worktree.tmuxSession join go through it so a new TmuxSession
// field cannot be populated on one path and silently zero-valued on the other
// (#27 + #29: windows is non-nullable, so a nil slice is a query-time error, and a
// zero-valued attached is a wrong answer that looks right).
func sessionToModel(s tmux.SessionRow, windows []tmux.WindowRow) model.TmuxSession {
	return model.TmuxSession{
		HostID: s.HostID, Name: s.Name, Worktree: optStr(s.Worktree), Branch: optStr(s.Branch),
		Attached: s.Attached, CreatedAt: s.CreatedAt, Windows: windowsToModel(windows),
		LastSeenAt: s.LastSeenAt, StaleSince: s.StaleSince,
	}
}
