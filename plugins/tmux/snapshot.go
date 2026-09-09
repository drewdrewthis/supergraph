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
	paneFmt = "#{session_name}\t#{window_index}\t#{pane_index}\t#{pane_pid}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_active}\t#{pane_id}\t#{window_name}\t#{window_active}"
	sessFmt = "#{session_name}\t#{session_path}\t#{session_created}"
	// clientFmt drives the attached probe. #{client_control_mode} distinguishes the
	// plugin's own `tmux -C attach` client (control=1) from a human client (#27 §3).
	clientFmt = "#{client_session}\t#{client_control_mode}"
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

	// attached is derived from list-clients, never #{session_attached} (poisoned by
	// our own control client — #27 §3). attachedSessions returns empty on ANY error,
	// so a failed clients probe reads as "zero clients" and never aborts the reconcile
	// nor marks sessions stale (AC-TMUX-CLIENTS-PROBE-FAILSAFE).
	attached := p.attachedSessions(ctx)

	prevLive, err := p.store.livePaneRows(ctx)
	if err != nil {
		return err
	}
	prevSessions, err := p.store.liveSessionRows(ctx)
	if err != nil {
		return err
	}

	for _, sr := range parseSessions(p.hostID, sessOut, p.branchOf) {
		sr.Attached = attached[sr.Name]
		old, existed := prevSessions[sr.Key]
		if err := p.store.upsertSession(ctx, sr, now); err != nil {
			return err
		}
		// Emit tmux.session.updated (the fifth envelope) for a new or changed session:
		// an attach/detach, a createdAt fill, or a worktree/branch move is what the
		// tmuxEvents attach subscriber sees (AC-TMUX-ATTACHED-LIVE).
		if !existed || sessionChanged(old, sr) {
			p.emitSession(ctx, sr, now)
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

// attachedSessions returns the set of session names with at least one HUMAN client
// attached. #{session_attached} is NOT used: the plugin's own `tmux -C attach`
// control-mode client makes its session read attached=1 with zero human clients
// (#27 §3, measured), a false positive the sidebar highlight keys on. Instead it
// reads list-clients and counts only clients whose #{client_control_mode} != "1".
// ANY error (a zero-client server exits non-zero on some tmux versions) yields an
// empty map — a failed clients probe means "no clients", never a reconcile abort
// (AC-TMUX-CLIENTS-PROBE-FAILSAFE).
func (p *Plugin) attachedSessions(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	raw, err := p.run(ctx, p.tmuxArgs("list-clients", "-F", clientFmt)...)
	if err != nil {
		return out
	}
	for _, line := range splitLines(raw) {
		f := strings.Split(line, "\t")
		if len(f) < 2 || f[0] == "" {
			continue
		}
		if f[1] != "1" { // control-mode clients (our own) never count as a human attach
			out[f[0]] = true
		}
	}
	return out
}

// sessionChanged reports whether a live session's reconcile-visible state moved:
// attached (a human client came or went), createdAt, worktree, or branch. A new or
// changed session drives one tmux.session.updated envelope.
func sessionChanged(old, cur SessionRow) bool {
	return old.Attached != cur.Attached ||
		!sameTime(old.CreatedAt, cur.CreatedAt) ||
		old.Worktree != cur.Worktree ||
		old.Branch != cur.Branch
}

// sameTime compares two optional times by instant (nil == nil, nil != set).
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
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
		r := PaneRow{
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
		}
		// pane_id/window_name/window_active are appended to paneFmt for #27; live tmux
		// always emits all ten fields, a shorter line leaves the #27 fields zero.
		if len(f) >= 10 {
			r.PaneID, r.WindowName, r.WindowActive = f[7], f[8], f[9] == "1"
		}
		rows = append(rows, r)
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
		sr := SessionRow{
			Key:      sessionKey(f[0], host),
			HostID:   host,
			Name:     f[0],
			Worktree: f[1],
			Branch:   branchOf(context.Background(), f[1]),
		}
		if len(f) >= 3 {
			sr.CreatedAt = parseUnixSeconds(f[2])
		}
		rows = append(rows, sr)
	}
	return rows
}

// parseUnixSeconds parses tmux's #{session_created} (unix epoch SECONDS) into a UTC
// time, or nil when the field is empty or unparseable — never a fabricated zero time.
func parseUnixSeconds(s string) *time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	secs, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	t := time.Unix(secs, 0).UTC()
	return &t
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

// emitSession emits tmux.session.updated with the session's join-relevant payload
// (createdAt is null when tmux reported no #{session_created}).
func (p *Plugin) emitSession(ctx context.Context, r SessionRow, now time.Time) {
	var created any
	if r.CreatedAt != nil {
		created = r.CreatedAt.UTC().Format(rfc)
	}
	p.emit(ctx, "tmux.session.updated", r.Key, map[string]any{
		"key": r.Key, "name": r.Name, "attached": r.Attached,
		"createdAt": created, "worktree": r.Worktree, "branch": r.Branch,
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
