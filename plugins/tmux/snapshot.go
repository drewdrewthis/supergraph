package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// paneFmt / sessFmt are the tab-separated field sets list-panes/list-sessions emit
// (EDR §Measurements: the full reconcile field set).
const (
	paneFmt = "#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_pid}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_active}"
	sessFmt = "#{session_name}\t#{session_path}"
)

// reconcile runs one backstop poll: it lists every session and pane on the watched
// server, upserts them, marks vanished panes stale, and emits one tmux.snapshot.
// An errored/timed-out poll (server down, stub hang) returns the error and emits
// NOTHING — so /health crosses to stale rather than reading a false snapshot
// (owner T1, AC-TMUX-POLL-ERROR).
func (p *Plugin) reconcile(ctx context.Context) error {
	// A poll that cannot reach the server marks every local entity stale (no emit —
	// owner T1). This is also what keeps entities stale after the server dies: it re-
	// marks anything a reconcile that was in flight at server-death time upserted back
	// to live AFTER watch's one-shot server.down ran (the control watcher and this
	// poll write the store concurrently).
	sessOut, err := p.run(ctx, p.tmuxArgs("list-sessions", "-F", sessFmt)...)
	if err != nil {
		_ = p.store.markAllStale(ctx, p.now())
		return fmt.Errorf("tmux: list-sessions: %w", err)
	}
	paneOut, err := p.run(ctx, p.tmuxArgs("list-panes", "-a", "-F", paneFmt)...)
	if err != nil {
		_ = p.store.markAllStale(ctx, p.now())
		return fmt.Errorf("tmux: list-panes: %w", err)
	}
	now := p.now()

	prevLive, err := p.store.livePaneRows(ctx)
	if err != nil {
		return err
	}

	for _, sr := range parseSessions(p.hostID, sessOut, p.branchOf) {
		if err := p.store.upsertSession(ctx, sr, now); err != nil {
			return err
		}
	}

	seen := map[string]bool{}
	panes := parsePanes(p.hostID, paneOut, p.cfg.idleShells)
	for _, pr := range panes {
		old, existed := prevLive[pr.Key]
		if err := p.store.upsertPane(ctx, pr, now); err != nil {
			return err
		}
		seen[pr.Key] = true
		// Emit tmux.pane.updated for a NEW pane and for a live pane whose free/busy,
		// command, or path changed since the stored row — a busied/freed slot is a
		// read-model change downstream consumers must see, not only pane creation.
		if !existed || paneChanged(old, pr) {
			p.emitPane(ctx, "tmux.pane.updated", pr, now)
		}
	}

	for k := range prevLive {
		if !seen[k] {
			if err := p.store.markPaneStale(ctx, k, now); err != nil {
				return err
			}
			p.emit(ctx, "tmux.pane.closed", k, map[string]any{"key": k}, now)
		}
	}

	p.emit(ctx, "tmux.snapshot", serverKey(p.hostID),
		map[string]any{"hostId": p.hostID, "paneCount": len(panes)}, now)
	_ = p.store.core.SetCursor(ctx, "snapshot:lastAt", now.UTC().Format(rfc))
	return nil
}

// paneChanged reports whether a live pane's reconcile-visible state moved: its
// free/busy classification, current command, or current path (the fields a slot
// consumer keys on). pid/active are excluded — a pane keeping the same command
// through a pid churn is not a read-model change worth an event.
func paneChanged(old, cur PaneRow) bool {
	return old.Free != cur.Free || old.Cmd != cur.Cmd || old.Path != cur.Path
}

// parsePanes turns list-panes tab output into pane rows, classifying free/busy.
func parsePanes(host string, out []byte, idleShells []string) []PaneRow {
	var rows []PaneRow
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		win, err1 := strconv.Atoi(f[1])
		pane, err2 := strconv.Atoi(f[2])
		pid, _ := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			continue
		}
		rows = append(rows, PaneRow{
			Key:     paneKey(f[0], win, pane, host),
			HostID:  host,
			Session: f[0],
			Window:  win,
			Pane:    pane,
			Pid:     pid,
			Cmd:     f[4],
			Path:    f[5],
			Active:  f[6] == "1",
			Free:    isIdle(f[4], idleShells),
		})
	}
	return rows
}

// parseSessions turns list-sessions tab output into session rows, resolving each
// session's git branch from its worktree path via branchOf.
func parseSessions(host string, out []byte, branchOf func(context.Context, string) string) []SessionRow {
	var rows []SessionRow
	for _, line := range splitLines(out) {
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		rows = append(rows, SessionRow{
			Key:      sessionKey(f[0], host),
			HostID:   host,
			Name:     f[0],
			Worktree: f[1],
			Branch:   branchOf(context.Background(), f[1]),
		})
	}
	return rows
}

func splitLines(out []byte) []string {
	var xs []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(l) != "" {
			xs = append(xs, l)
		}
	}
	return xs
}

// gitBranch resolves the checked-out branch of a worktree path, or "" when the path
// is not a git worktree (a session cwd need not be one).
func gitBranch(ctx context.Context, path string) string {
	if path == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--abbrev-ref", "HEAD").Output() //nolint:gosec // G204: path is the operator's own tmux session cwd; fixed git subcommand
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// emitPane is emit with a pane's full payload.
func (p *Plugin) emitPane(ctx context.Context, typ string, r PaneRow, now time.Time) {
	p.emit(ctx, typ, r.Key, map[string]any{
		"key": r.Key, "session": r.Session, "window": r.Window, "pane": r.Pane,
		"pid": r.Pid, "cmd": r.Cmd, "path": r.Path, "active": r.Active, "free": r.Free,
	}, now)
}

// emit builds and sends one envelope through the captured emit closure.
func (p *Plugin) emit(ctx context.Context, typ, key string, payload map[string]any, now time.Time) {
	p.emitMu.RLock()
	emit := p.emitFn
	p.emitMu.RUnlock()
	if emit == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = emit(ctx, core.Envelope{TS: now, Source: p.Name(), Type: typ, V: 1, Key: key, Payload: body})
}

// tmuxArgs prepends the socket selector (-L name or -S path) to a tmux subcommand.
func (p *Plugin) tmuxArgs(sub ...string) []string {
	var a []string
	if p.cfg.socket != "" {
		if strings.Contains(p.cfg.socket, "/") {
			a = append(a, "-S", p.cfg.socket)
		} else {
			a = append(a, "-L", p.cfg.socket)
		}
	}
	return append(a, sub...)
}
