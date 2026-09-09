package claude

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The state-file channel is the plugin's THIRD ingest source (issue #28), added
// alongside the hook POST (hook.go) and the transcript tail (tail.go). It is the ONLY
// prompt-bearing path: a Claude Code session's own state file carries first_prompt and
// last_response, which the privacy whitelist deliberately drops from the other two
// channels (redact.go, EDR §Privacy). It is opt-in — dormant unless stateDir is set —
// because an enabled channel opts that box's truncated prompt text into peer-mesh
// replication (plugins/peer/mirror.go maps the `claude` op into the mesh).

// missionMaxRunes / lastResponseMaxRunes cap the two prompt-bearing fields. The bound
// is in RUNES, not bytes, so a multibyte prompt is never split mid-rune into invalid
// UTF-8 (AC-CLAUDE-MISSION-TRUNC / -LASTRESPONSE).
const (
	missionMaxRunes      = 120
	lastResponseMaxRunes = 200
)

// scanStateFiles folds every *.json under stateDir through the state-file whitelist.
// A disabled channel (empty stateDir) reads no directory at all. It is driven off the
// same tail ticker as the transcript scan (tail.go scanOnce), so file-sourced mission
// and state land within one scan interval.
func (p *Plugin) scanStateFiles(ctx context.Context) {
	if p.stateDir == "" {
		return
	}
	files, _ := filepath.Glob(filepath.Join(p.stateDir, "*.json"))
	titles := p.paneTitleMap(ctx)
	for _, f := range files {
		p.scanStateFile(ctx, f, titles)
	}
}

// scanStateFile ingests one state file. A malformed/truncated JSON body or an invalid
// sid is skipped silently so a mid-write file never aborts the scan
// (AC-CLAUDE-STATEFILE-MALFORMED); the next tick re-reads it once whole. It emits
// claude.session.updated only when the row actually changed (AC-CLAUDE-STATEFILE-EMIT).
func (p *Plugin) scanStateFile(ctx context.Context, path string, titles map[string]string) {
	data, err := os.ReadFile(path) //nolint:gosec // path comes from our own stateDir glob
	if err != nil {
		return
	}
	var sf stateFile
	if json.Unmarshal(data, &sf) != nil || !validSessionID(sf.SID) {
		return
	}
	changed, err := p.store.applyStateFile(ctx, sf, titles[sf.Pane], p.hostID, p.now())
	if err == nil && changed {
		p.emitSessionUpdated(ctx, sf.SID)
	}
}

// paneTitleMap builds a pane-id → title map once per scan from `tmux list-panes`. It
// is gated by paneTitles (default false): when off, tmux is NEVER spawned. tmux being
// absent or exiting non-zero yields a nil map, so titles stay null and no error
// surfaces (AC-CLAUDE-PANE-TITLE).
func (p *Plugin) paneTitleMap(ctx context.Context) map[string]string {
	if !p.paneTitles {
		return nil
	}
	out, err := exec.CommandContext(ctx, "tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_title}").Output()
	if err != nil {
		return nil
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if id, title, ok := strings.Cut(line, "\t"); ok && id != "" {
			m[id] = title
		}
	}
	return m
}

// truncRunes returns at most n runes of s. The result is valid UTF-8 and a prefix of
// s (range over a string steps rune-by-rune, so the cut is always on a rune boundary).
func truncRunes(s string, n int) string {
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// expandTilde resolves a leading "~/" in a configured path against home, so an
// operator can write stateDir = "~/.local/state/claude-sessions/state" (AC-CLAUDE-STATEDIR-TILDE).
func expandTilde(path, home string) string {
	if home != "" && strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
