package claude

import "encoding/json"

// This file is the PRIVACY whitelist (EDR §"Privacy — store metadata, never
// content"). Every path that turns an untrusted hook payload or transcript record
// into stored/emitted state goes through the projections here, and they copy ONLY an
// explicit list of structural fields. prompt, assistant response text, tool_input,
// and the Notification message body are never assigned to a stored/emitted field —
// so a new field added to a future payload is dropped by default, not leaked.

// foldInput is the body-free projection of one raw event: the classified transition
// plus structural metadata. It carries a tool NAME but never a tool_input body, and a
// permissionAsk boolean but never the notification message.
type foldInput struct {
	sid           string
	event         string // one of knownEvents
	ts            string // dedup timestamp (sid, event, ts)
	tool          string // tool NAME only (PreToolUse)
	permissionAsk bool
	pane          string
	pid           int
	enrich        enrichment
}

// enrichment is the whitelist of structural fields a fold may add. A zero/empty
// field never clobbers an existing value on upsert (COALESCE), so the tail can add
// gitBranch/model/PR without erasing hook-set state (AC-CLAUDE-TAIL-ENRICH).
type enrichment struct {
	cwd, gitBranch, model, prURL string
	issueNumber, prNumber        int
}

// hookPayload decodes ONLY the whitelisted fields of a hook POST. prompt and
// tool_input are deliberately absent, so their bodies never enter process memory.
// issueNumber is a *int so a non-numeric value fails decoding (400,
// AC-CLAUDE-HOOK-SCHEMA) rather than being coerced.
type hookPayload struct {
	SessionID   string `json:"session_id"`
	Event       string `json:"hook_event_name"`
	Cwd         string `json:"cwd"`
	ToolName    string `json:"tool_name"`
	Message     string `json:"message"`
	GitBranch   string `json:"gitBranch"`
	Model       string `json:"model"`
	TmuxPane    string `json:"tmux_pane"`
	Pid         int    `json:"pid"`
	IssueNumber *int   `json:"issue_number"`
	TS          string `json:"ts"`
}

// projectHook builds the body-free foldInput for a hook payload. The Notification
// message is read only to classify permissionAsk (isNag), then discarded.
func projectHook(p hookPayload) foldInput {
	fi := foldInput{
		sid: p.SessionID, event: p.Event, ts: p.TS, tool: p.ToolName,
		permissionAsk: p.Event == "Notification" && !isNag(p.Message),
		pane:          p.TmuxPane, pid: p.Pid,
		enrich: enrichment{cwd: p.Cwd, gitBranch: p.GitBranch, model: p.Model},
	}
	switch {
	case p.IssueNumber != nil:
		fi.enrich.issueNumber = *p.IssueNumber
	default:
		fi.enrich.issueNumber = issueFromBranch(p.GitBranch)
	}
	return fi
}

// transcriptRecord decodes ONLY the whitelisted fields of one JSONL transcript line.
// message.content is left raw so tool_use NAMES can be pulled without decoding tool
// inputs or assistant text.
type transcriptRecord struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	GitBranch string `json:"gitBranch"`
	Timestamp string `json:"timestamp"`
	PrNumber  int    `json:"prNumber"`
	PrURL     string `json:"prUrl"`
	Message   struct {
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// projectRecord builds the enrichment and any PreToolUse folds for one transcript
// record. It reads tool_use block NAMES only; content strings (prompts, responses)
// and tool inputs are never touched.
func projectRecord(r transcriptRecord) (enrichment, []foldInput) {
	enr := enrichment{cwd: r.Cwd, gitBranch: r.GitBranch, model: r.Message.Model, prNumber: r.PrNumber, prURL: r.PrURL}
	enr.issueNumber = issueFromBranch(r.GitBranch)
	if enr.prNumber == 0 {
		enr.prNumber = prNumberFromURL(r.PrURL)
	}
	var folds []foldInput
	var blocks []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(r.Message.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_use" {
				folds = append(folds, foldInput{sid: r.SessionID, event: "PreToolUse", ts: r.Timestamp, tool: b.Name, enrich: enr})
			}
		}
	}
	return enr, folds
}
