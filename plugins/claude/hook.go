package claude

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// maxHookBody caps an inbound hook POST. Claude Code hook payloads are small (a few
// fields plus a tool_input we do not even read), so 1 MiB is ample and bounds memory.
const maxHookBody = 1 << 20

// handleHook is the /plugins/claude/hook ingress: it validates and folds ONE
// lifecycle hook payload into session state, synchronously, so the row is queryable
// the instant the POST returns 200 (F1 is met by construction on this path). Auth is
// core's: when Listen is non-loopback, core's bearer middleware rejects an
// unauthenticated POST before this handler runs (AC-CLAUDE-HOOK-AUTH), so the handler
// itself assumes an authorised caller. A schema-invalid payload is 400 with nothing
// stored (AC-CLAUDE-HOOK-SCHEMA).
func (p *Plugin) handleHook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHookBody)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad body", readErrStatus(err))
		return
	}
	var hp hookPayload
	if err := json.Unmarshal(body, &hp); err != nil {
		http.Error(w, "bad payload", http.StatusBadRequest) // e.g. non-numeric issue_number
		return
	}
	if hp.SessionID == "" || !knownEvents[hp.Event] {
		http.Error(w, "unknown event or missing session", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	fi := projectHook(hp)
	if fi.ts == "" {
		fi.ts = p.now().Format(rfc)
	}
	applied, err := p.store.applyFold(ctx, fi, p.hostID, p.now())
	if err != nil {
		http.Error(w, "fold failed", http.StatusInternalServerError)
		return
	}
	if applied {
		p.emitForFold(ctx, fi)
	}
	w.WriteHeader(http.StatusOK)
}

// emitForFold emits the envelopes a folded transition produces: always
// session.updated, plus tool.used / session.input / session.ended / instance.updated
// as the event warrants. Every payload is metadata-only (Privacy).
func (p *Plugin) emitForFold(ctx context.Context, fi foldInput) {
	p.emitSessionUpdated(ctx, fi.sid)
	if fi.event == "PreToolUse" && fi.tool != "" {
		p.emit(ctx, "claude.tool.used", sessionKey(fi.sid, p.hostID), map[string]any{"sid": fi.sid, "tool": fi.tool})
	}
	if fi.permissionAsk {
		p.emit(ctx, "claude.session.input", sessionKey(fi.sid, p.hostID), map[string]any{"sid": fi.sid, "awaitingInput": true})
	}
	if fi.event == "SessionEnd" {
		p.emit(ctx, "claude.session.ended", sessionKey(fi.sid, p.hostID), map[string]any{"sid": fi.sid})
	}
	if fi.pane != "" {
		p.emit(ctx, "claude.instance.updated", instanceKey(fi.pane, p.hostID), map[string]any{"sid": fi.sid, "pane": fi.pane, "pid": fi.pid})
	}
}

// readErrStatus maps a body read error to 413 when the MaxBytesReader cap tripped,
// else 400 for an ordinary malformed body.
func readErrStatus(err error) int {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
