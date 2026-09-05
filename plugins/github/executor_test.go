package github

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

// TestLoadOps: the embedded named ops parse their # op/# scope/# keys headers.
func TestLoadOps(t *testing.T) {
	ops := loadOps()
	li, ok := ops["openIssues"]
	if !ok {
		t.Fatal("openIssues not loaded")
	}
	if li.scope != "repo:{owner}/{repo}" {
		t.Errorf("openIssues scope = %q", li.scope)
	}
	if len(li.keys) != 1 || li.keys[0] != "issue:{owner}/{repo}#{number}" {
		t.Errorf("openIssues keys = %v", li.keys)
	}
	if pt, ok := ops["issue"]; !ok || pt.scope != "" {
		t.Errorf("issue op = %+v (%v)", pt, ok)
	}
}

func newExecPlugin(t *testing.T) (*Plugin, *fakegh.Server) {
	t.Helper()
	srv := fakegh.New()
	t.Cleanup(srv.Close)
	p := newPlugin(t, map[string]any{
		"baseURL":    srv.URL,
		"graphqlURL": srv.URL + "/graphql",
		"token":      "t",
	})
	return p, srv
}

// TestRunPointOp: a point op resolves each declared key through the read-through
// cache and returns it as a node.
func TestRunPointOp(t *testing.T) {
	p, srv := newExecPlugin(t)
	srv.AddIssue("o", "r", 5, "Boom", "open")
	ctx := context.Background()

	res, err := p.runOp(ctx, p.ops["issue"], map[string]any{"owner": "o", "repo": "r", "number": 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 1 || res.Nodes[0].Key != "issue:o/r#5" {
		t.Fatalf("nodes = %+v", res.Nodes)
	}
	if res.Nodes[0].Typename != "Issue" {
		t.Errorf("typename = %q", res.Nodes[0].Typename)
	}
}

// TestRunListOp: a list op fetches via GraphQL, stores each returned node under its
// own key, and caches the list tagged with its covering scope — so a later purge of
// one member evicts the whole list but not a sibling member (declared-key scoping).
func TestRunListOp(t *testing.T) {
	p, srv := newExecPlugin(t)
	srv.AddIssue("o", "r", 5, "Five", "open")
	srv.AddIssue("o", "r", 6, "Six", "open")
	srv.AddIssue("o", "r", 7, "Seven", "closed") // excluded by state filter
	ctx := context.Background()

	res, err := p.runOp(ctx, p.ops["openIssues"], map[string]any{"owner": "o", "repo": "r"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Scope != "repo:o/r" {
		t.Errorf("scope = %q", res.Scope)
	}
	keys := map[string]bool{}
	for _, n := range res.Nodes {
		keys[n.Key] = true
	}
	if !keys["issue:o/r#5"] || !keys["issue:o/r#6"] {
		t.Fatalf("nodes = %+v", res.Nodes)
	}
	lk := listKey("openIssues", "repo:o/r")
	if n, _ := p.store.get(ctx, lk); n == nil {
		t.Fatal("list result not stored")
	}

	// purging one member evicts the list, leaves the sibling node.
	if _, err := p.store.purge(ctx, "issue:o/r#5"); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.store.get(ctx, lk); n != nil {
		t.Errorf("list survived member purge")
	}
	if n, _ := p.store.get(ctx, "issue:o/r#6"); n == nil {
		t.Errorf("sibling member was evicted")
	}
}

// TestHandleGraphQLRawPassthrough: a raw {query} is rejected unless it is a pure
// introspection query — a substring match on "__type" false-matches "__typename",
// so the guard must parse and check top-level selections instead.
func TestHandleGraphQLRawPassthrough(t *testing.T) {
	p, _ := newExecPlugin(t)

	cases := []struct {
		name    string
		query   string
		wantErr bool
	}{
		{"typename false match", `{__typename}`, true},
		{"pure __schema", `{__schema{types{name}}}`, false},
		{"pure __type", `query{__type(name:"Issue"){fields{name}}}`, false},
		{"mixed with data field", `{repository(owner:"o",name:"r"){id} __schema{types{name}}}`, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"query": c.query})
			req := httptest.NewRequest(http.MethodPost, "/plugins/github/graphql", bytes.NewReader(body))
			w := httptest.NewRecorder()
			p.handleGraphQL(w, req)
			if c.wantErr && w.Code != http.StatusBadRequest {
				t.Errorf("query %q: status = %d, want 400", c.query, w.Code)
			}
			if !c.wantErr && w.Code == http.StatusBadRequest {
				t.Errorf("query %q: status = %d, want non-400", c.query, w.Code)
			}
		})
	}
}
