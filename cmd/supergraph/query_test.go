package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestGithubEndpoint_AppendsPluginRouteToDefault(t *testing.T) {
	got := githubEndpoint(defaultQueryEndpoint, "/does/not/exist.toml")
	want := "http://127.0.0.1:7788/plugins/github/graphql"
	if got != want {
		t.Fatalf("githubEndpoint() = %q, want %q", got, want)
	}
}

func TestGithubEndpoint_AppendsPluginRouteToExplicitEndpoint(t *testing.T) {
	got := githubEndpoint("http://example.test:9999", "/does/not/exist.toml")
	want := "http://example.test:9999/plugins/github/graphql"
	if got != want {
		t.Fatalf("githubEndpoint() = %q, want %q", got, want)
	}
}

func TestParseVars_ParsesJSONWhenItParsesElseKeepsString(t *testing.T) {
	got, err := parseVars([]string{"owner=acme", "count=5", "open=true", "labels=[\"a\",\"b\"]"})
	if err != nil {
		t.Fatalf("parseVars() error: %v", err)
	}
	if got["owner"] != "acme" {
		t.Errorf("owner = %v, want string acme", got["owner"])
	}
	if got["count"] != float64(5) {
		t.Errorf("count = %v (%T), want float64(5)", got["count"], got["count"])
	}
	if got["open"] != true {
		t.Errorf("open = %v, want bool true", got["open"])
	}
	labels, ok := got["labels"].([]any)
	if !ok || len(labels) != 2 {
		t.Errorf("labels = %v, want 2-element slice", got["labels"])
	}
}

func TestParseVars_RejectsMissingEquals(t *testing.T) {
	if _, err := parseVars([]string{"nokv"}); err == nil {
		t.Fatal("expected error for --var without '='")
	}
}

func TestLoadNamedQuery_FindsFileInQueriesDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "openIssues.graphql"), []byte("query openIssues { x }"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := loadNamedQuery("openIssues", dir)
	if err != nil {
		t.Fatalf("loadNamedQuery() error: %v", err)
	}
	if got != "query openIssues { x }" {
		t.Fatalf("loadNamedQuery() = %q", got)
	}
}

func TestLoadNamedQuery_FallsBackToXDGConfigHome(t *testing.T) {
	xdg := t.TempDir()
	queriesDir := filepath.Join(xdg, "supergraph", "queries")
	if err := os.MkdirAll(queriesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(queriesDir, "issue.graphql"), []byte("query issue { y }"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CONFIG_HOME", xdg)

	got, err := loadNamedQuery("issue", filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("loadNamedQuery() error: %v", err)
	}
	if got != "query issue { y }" {
		t.Fatalf("loadNamedQuery() = %q", got)
	}
}

func TestLoadNamedQuery_ErrorsWhenNotFoundAnywhere(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := loadNamedQuery("missingOp", t.TempDir()); err == nil {
		t.Fatal("expected error for missing named operation")
	}
}

func TestRunNamedQuery_PostsOpAndVariablesToPluginEndpoint(t *testing.T) {
	dir := t.TempDir()
	queryFile := "query openIssues($owner:String!,$repo:String!){ x }"
	if err := os.WriteFile(filepath.Join(dir, "openIssues.graphql"), []byte(queryFile), 0o600); err != nil {
		t.Fatal(err)
	}

	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"x":1}}`))
	}))
	defer srv.Close()

	err := runNamedQuery(srv.URL+"/plugins/github/graphql", "openIssues",
		[]string{"owner=acme", "repo=widgets"}, dir, true)
	if err != nil {
		t.Fatalf("runNamedQuery() error: %v", err)
	}
	if gotPath != "/plugins/github/graphql" {
		t.Errorf("posted to %q, want /plugins/github/graphql", gotPath)
	}
	if gotBody["op"] != "openIssues" {
		t.Errorf("posted op = %v, want %q", gotBody["op"], "openIssues")
	}
	if _, hasQuery := gotBody["query"]; hasQuery {
		t.Errorf("posted body carried a raw query field; want op-only body: %v", gotBody)
	}
	vars, _ := gotBody["variables"].(map[string]any)
	if vars["owner"] != "acme" || vars["repo"] != "widgets" {
		t.Errorf("posted variables = %v", vars)
	}
}

func TestRunNamedQuery_SurfacesGraphQLErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "issue.graphql"), []byte("query issue { x }"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"errors":[{"message":"boom"}]}`))
	}))
	defer srv.Close()

	err := runNamedQuery(srv.URL, "issue", nil, dir, true)
	if err == nil {
		t.Fatal("expected error for graphql errors payload")
	}
}

func TestRunNamedQuery_ErrorsWhenOpFileMissing(t *testing.T) {
	if err := runNamedQuery("http://unused.invalid", "nope", nil, t.TempDir(), true); err == nil {
		t.Fatal("expected error when named op file is missing")
	}
}
