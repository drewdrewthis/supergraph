package github

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// TestStartDormantWhenNoToken: with no [plugins.github] token (and no
// GITHUB_TOKEN env), Start must return immediately without spawning the `gh
// webhook forward` supervisor — proven by pointing ghPath at a fake exec that
// records every invocation, then asserting it recorded zero.
func TestStartDormantWhenNoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	dir := t.TempDir()
	invocations := filepath.Join(dir, "invocations.log")
	stub := filepath.Join(dir, "gh-stub.sh")
	script := "#!/bin/sh\necho invoked >> " + invocations + "\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write stub: %v", err)
	}

	p := newPlugin(t, map[string]any{"ingress": "forward"})
	p.cfg.ghPath = stub
	if p.cfg.token != "" {
		t.Fatal("plugin resolved a token with none configured")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Start(ctx, func(context.Context, core.Envelope) error { return nil }) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dormant Start did not return promptly (spawned a supervisor loop?)")
	}

	if _, err := os.Stat(invocations); err == nil {
		t.Fatal("dormant Start spawned the forward child (fake exec recorded an invocation)")
	}
}

func TestCursorDormantWhenNoToken(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	p := newPlugin(t, map[string]any{})
	if p.cfg.token != "" {
		t.Fatal("plugin resolved a token with none configured")
	}
	if got := p.Cursor(context.Background()); got != "dormant: no [plugins.github] config" {
		t.Fatalf("Cursor = %q, want dormant marker", got)
	}
}
