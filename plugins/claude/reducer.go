package claude

import "strings"

// State is a Claude Code session's coarse liveness, ported from the prior-art
// reducer (orchardist/claude-session-state fold-state.sh:78-105). It is stored as a
// string so it serializes straight into the graph row.
type State string

const (
	stateIdle    State = "idle"    // SessionStart, Stop — alive, awaiting a prompt
	stateWorking State = "working" // UserPromptSubmit, Pre/PostToolUse — actively running
	stateInput   State = "input"   // a permission Notification — blocked on the human
	stateEnded   State = "ended"   // SessionEnd — row kept, marked stale, never deleted
)

// Lifecycle events core recognises (the 7 Claude Code hook events, measured
// fold-state.sh:65-105). An unknown event name is rejected by the hook handler (400,
// AC-CLAUDE-HOOK-SCHEMA), never folded.
var knownEvents = map[string]bool{
	"SessionStart": true, "UserPromptSubmit": true, "PreToolUse": true,
	"PostToolUse": true, "Notification": true, "Stop": true, "SessionEnd": true,
}

// fold is the pure state-machine port: given the previous state and the classified
// event (name + notification classification), it returns the next state, whether a
// tool call was made (PreToolUse), and whether the session ended. It takes NO prompt,
// response, or tool_input body — only the structural event name and, for a
// Notification, the boolean "is this a real permission ask" (computed by isNag from
// the message but never stored).
func fold(prev State, event string, permissionAsk bool) (next State, toolCall, ended bool) {
	switch event {
	case "SessionStart", "Stop":
		return stateIdle, false, false
	case "UserPromptSubmit", "PostToolUse":
		return stateWorking, false, false
	case "PreToolUse":
		return stateWorking, true, false
	case "Notification":
		if permissionAsk {
			return stateInput, false, false
		}
		// The "waiting for your input" idle nag is NOT a permission gate: it fires when
		// the session is idle awaiting the user, so it resolves to idle (never input).
		return stateIdle, false, false
	case "SessionEnd":
		return stateEnded, false, true
	}
	return prev, false, false
}

// isNag reports whether a Notification is the idle "waiting for your input" nag
// rather than a real permission ask. Only the classification survives; the message
// body itself is dropped by the caller (Privacy).
func isNag(message string) bool {
	return strings.Contains(strings.ToLower(message), "waiting for your input")
}
