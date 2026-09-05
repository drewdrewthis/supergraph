package tmux

import "testing"

func TestPaneKeyRoundTrips(t *testing.T) {
	key := paneKey("main", 1, 0, "drudru-lan")
	if key != "pane:main:1.0@drudru-lan" {
		t.Fatalf("format got %q", key)
	}
	sess, win, pane, host, err := parsePaneKey(key)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if sess != "main" || win != 1 || pane != 0 || host != "drudru-lan" {
		t.Fatalf("round-trip got %q %d %d %q", sess, win, pane, host)
	}
}

func TestParsePaneKeyRejectsMalformed(t *testing.T) {
	bad := []string{
		"pane:main:1.0",     // missing @hostId
		"pane:main:1.0@",    // empty hostId
		"session:main@h",    // wrong kind
		"bogus:main:1.0@h",  // unknown kind
		"pane:main:1@h",     // missing .pane
		"pane:main:1.@h",    // empty pane index
		"pane:main:.0@h",    // empty window index
		"pane:main:x.0@h",   // non-int window
		"pane:main:1.0.2@h", // trailing separator
		"pane:main@h",       // missing window.pane entirely
	}
	for _, k := range bad {
		if _, _, _, _, err := parsePaneKey(k); err == nil {
			t.Errorf("expected error for %q, got none", k)
		}
	}
}

func TestTypename(t *testing.T) {
	cases := map[string]string{
		"pane:main:1.0@h": "TmuxPane",
		"session:main@h":  "TmuxSession",
		"tmuxServer:h@h":  "TmuxServer",
		"bogus:x@h":       "",
	}
	for k, want := range cases {
		if got := typename(k); got != want {
			t.Errorf("typename(%q)=%q want %q", k, got, want)
		}
	}
}

func TestIsIdle(t *testing.T) {
	shells := []string{"zsh", "bash", "sh", "fish"}
	if !isIdle("zsh", shells) {
		t.Error("zsh should be idle")
	}
	if isIdle("sleep", shells) {
		t.Error("sleep should be busy")
	}
}
