package tmux

import (
	"fmt"
	"strconv"
	"strings"
)

// Key grammar (EDR §"Key grammar"), consistent with github's
// <kind>:<scope>@<hostId>. Unlike github, @<hostId> is ALWAYS present — a pane is
// only meaningful per box. tmux forbids ':' and '.' in session names, so the scope
// is unambiguous without escaping.
//
//	server  := "tmuxServer:" <hostId>                       "@" <hostId>
//	session := "session:"    <session>                      "@" <hostId>
//	window  := "window:"     <session>":"<index>            "@" <hostId>
//	pane    := "pane:"       <session>":"<window>"."<pane>   "@" <hostId>
//
// Each kind is one row in kindSpecs: adding a kind is a new row, not new code.
type kindSpec struct {
	kind     string // leading token
	typename string // GraphQL typename
}

var kindSpecs = []kindSpec{
	{kind: "tmuxServer", typename: "TmuxServer"},
	{kind: "session", typename: "TmuxSession"},
	{kind: "window", typename: "TmuxWindow"},
	{kind: "pane", typename: "TmuxPane"},
}

var specByKind = func() map[string]kindSpec {
	m := map[string]kindSpec{}
	for _, s := range kindSpecs {
		m[s.kind] = s
	}
	return m
}()

// serverKey / sessionKey / paneKey format each kind from its parts.
func serverKey(host string) string { return "tmuxServer:" + host + "@" + host }

func sessionKey(session, host string) string { return "session:" + session + "@" + host }

func windowKey(session string, index int, host string) string {
	return fmt.Sprintf("window:%s:%d@%s", session, index, host)
}

func paneKey(session string, window, pane int, host string) string {
	return fmt.Sprintf("pane:%s:%d.%d@%s", session, window, pane, host)
}

// typename maps a key's kind to its GraphQL typename, or "" for an unknown kind.
func typename(key string) string {
	kind, _, _ := strings.Cut(key, ":")
	if s, ok := specByKind[kind]; ok {
		return s.typename
	}
	return ""
}

// splitHost separates the trailing "@<hostId>" (required for every tmux kind).
func splitHost(key string) (body, host string, err error) {
	at := strings.LastIndexByte(key, '@')
	if at < 0 || at == len(key)-1 {
		return "", "", fmt.Errorf("tmux: key %q missing @hostId", key)
	}
	return key[:at], key[at+1:], nil
}

// parsePaneKey is the round-trip inverse of paneKey. It rejects a malformed key
// rather than returning a partial one: an unknown kind, a missing @hostId, an
// embedded ':'/'.' beyond the grammar, an empty or non-integer index.
func parsePaneKey(key string) (session string, window, pane int, host string, err error) {
	body, host, err := splitHost(key)
	if err != nil {
		return "", 0, 0, "", err
	}
	kind, scope, ok := strings.Cut(body, ":")
	if !ok || kind != "pane" {
		return "", 0, 0, "", fmt.Errorf("tmux: key %q is not a pane key", key)
	}
	// scope := <session>":"<window>"."<pane>  — exactly one ':' and one '.'.
	sess, rest, ok := strings.Cut(scope, ":")
	if !ok || sess == "" {
		return "", 0, 0, "", fmt.Errorf("tmux: pane key %q malformed scope", key)
	}
	winStr, paneStr, ok := strings.Cut(rest, ".")
	if !ok {
		return "", 0, 0, "", fmt.Errorf("tmux: pane key %q missing window.pane", key)
	}
	if strings.ContainsAny(paneStr, ":.") {
		return "", 0, 0, "", fmt.Errorf("tmux: pane key %q has trailing separators", key)
	}
	window, err = strconv.Atoi(winStr)
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("tmux: pane key %q bad window index: %w", key, err)
	}
	pane, err = strconv.Atoi(paneStr)
	if err != nil {
		return "", 0, 0, "", fmt.Errorf("tmux: pane key %q bad pane index: %w", key, err)
	}
	return sess, window, pane, host, nil
}

// parseWindowKey is the round-trip inverse of windowKey. Like parsePaneKey it
// rejects a malformed key rather than returning a partial one: an unknown kind, a
// missing @hostId, an embedded ':'/'.' beyond the grammar, an empty or non-integer
// index.
func parseWindowKey(key string) (session string, index int, host string, err error) {
	body, host, err := splitHost(key)
	if err != nil {
		return "", 0, "", err
	}
	kind, scope, ok := strings.Cut(body, ":")
	if !ok || kind != "window" {
		return "", 0, "", fmt.Errorf("tmux: key %q is not a window key", key)
	}
	// scope := <session>":"<index> — exactly one ':', no '.'.
	sess, idxStr, ok := strings.Cut(scope, ":")
	if !ok || sess == "" {
		return "", 0, "", fmt.Errorf("tmux: window key %q malformed scope", key)
	}
	if strings.ContainsAny(idxStr, ":.") {
		return "", 0, "", fmt.Errorf("tmux: window key %q has trailing separators", key)
	}
	index, err = strconv.Atoi(idxStr)
	if err != nil {
		return "", 0, "", fmt.Errorf("tmux: window key %q bad index: %w", key, err)
	}
	return sess, index, host, nil
}

// isIdle reports whether a pane running cmd is a free slot: its current command is
// one of the configured idle shells (D5). A busy pane is never offered as free.
func isIdle(cmd string, idleShells []string) bool {
	for _, sh := range idleShells {
		if cmd == sh {
			return true
		}
	}
	return false
}
