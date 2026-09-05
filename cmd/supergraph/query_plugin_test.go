package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// The --plugin flag derives the op search dir from the plugin name, unless an explicit
// --queries-dir overrides it.
func TestQueriesDirFor(t *testing.T) {
	if got := queriesDirFor("", "claude"); got != "./plugins/claude/queries" {
		t.Errorf("queriesDirFor(\"\",claude) = %q", got)
	}
	if got := queriesDirFor("", "github"); got != "./plugins/github/queries" {
		t.Errorf("queriesDirFor(\"\",github) = %q", got)
	}
	if got := queriesDirFor("/custom/dir", "claude"); got != "/custom/dir" {
		t.Errorf("explicit --queries-dir should win, got %q", got)
	}
}

// github posts named ops to its own op route; a plugin with no op route (claude) posts
// query text to core /graphql.
func TestPluginEndpoint(t *testing.T) {
	missingCfg := filepath.Join(t.TempDir(), "no-config.toml")

	url, opRoute := pluginEndpoint("github", defaultQueryEndpoint, missingCfg)
	if !opRoute || !strings.HasSuffix(url, "/plugins/github/graphql") {
		t.Fatalf("github: url=%q opRoute=%v; want .../plugins/github/graphql,true", url, opRoute)
	}
	url, opRoute = pluginEndpoint("claude", defaultQueryEndpoint, missingCfg)
	if opRoute || !strings.HasSuffix(url, "/graphql") || strings.Contains(url, "/plugins/") {
		t.Fatalf("claude: url=%q opRoute=%v; want core /graphql,false", url, opRoute)
	}
}

// A claude op file loads and, on the fallback path, is posted as query text (opRoute
// false) — asserting the file the CLI advertises actually resolves.
func TestLoadClaudeNamedQuery(t *testing.T) {
	q, err := loadNamedQuery("sessionsForIssue", "../../plugins/claude/queries")
	if err != nil {
		t.Fatalf("load claude op: %v", err)
	}
	if !strings.Contains(q, "claudeSessions(issueNumber:") {
		t.Fatalf("sessionsForIssue.graphql missing the issueNumber query: %q", q)
	}
}
