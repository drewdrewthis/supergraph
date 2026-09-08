package github

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

// njson unmarshals a stored node body into the resolver's projection so two bodies
// can be compared for the fields the schema exposes (title/state/url/updatedAt/
// body/labels/assignees/headRefName/baseRefName).
func njson(raw []byte) nodeJSON {
	var j nodeJSON
	_ = json.Unmarshal(raw, &j)
	return j
}

// seedIssueViaListPath warms issue:o/r#N into the store through the real openIssues
// list path (extractAndStore), which writes the canonical GraphQL shape with no
// etag — exactly what production does on a since-cursor pull.
func seedIssueViaListPath(t *testing.T, p *Plugin, ctx context.Context) {
	t.Helper()
	op := p.ops["openIssues"]
	vars := map[string]any{"owner": "o", "repo": "r"}
	data, err := p.graphqlAt(ctx, p.cfg.graphqlURL, op.query, vars)
	if err != nil {
		t.Fatalf("openIssues fetch: %v", err)
	}
	for _, kt := range op.keys {
		p.extractAndStore(ctx, kt, vars, data)
	}
}

// TestRevalidateKeepsIssueGraphQLShape: a node written by the openIssues list path
// and the same node after a reconcile revalidate unmarshal to identical nodeJSON —
// labels, assignees, and updatedAt survive rather than being clobbered by the flat
// REST body (the live reconcile-shape bug).
func TestRevalidateKeepsIssueGraphQLShape(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug", "p1"}, []string{"alice"})

	seedIssueViaListPath(t, p, ctx)
	before, _ := p.store.get(ctx, "issue:o/r#5")
	if before == nil {
		t.Fatal("issue not seeded by the list path")
	}

	p.revalidate(ctx)

	after, _ := p.store.get(ctx, "issue:o/r#5")
	if after == nil {
		t.Fatal("issue vanished after revalidate")
	}
	if !reflect.DeepEqual(njson(before.JSON), njson(after.JSON)) {
		t.Fatalf("revalidate changed the node shape:\n before=%s\n after =%s", before.JSON, after.JSON)
	}
	got := njson(after.JSON)
	if len(got.Labels.Nodes) != 2 || got.Labels.Nodes[0].Name != "bug" {
		t.Errorf("labels lost after revalidate: %+v", got.Labels.Nodes)
	}
	if len(got.Assignees.Nodes) != 1 || got.Assignees.Nodes[0].Login != "alice" {
		t.Errorf("assignees lost after revalidate: %+v", got.Assignees.Nodes)
	}
	if got.UpdatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("updatedAt lost after revalidate: %q", got.UpdatedAt)
	}
}

// TestRevalidateKeepsPRGraphQLShape: same parity guarantee for a pull request,
// covering headRefName/baseRefName/body. fakegh has no openPRs op, so the list path
// is simulated by storing the canonical body (no etag) the pr point op will serve.
func TestRevalidateKeepsPRGraphQLShape(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichPR("o", "r", 9, "Nine", "OPEN", "2026-09-02T00:00:00Z", "feature/x", "main", "closes #1", []string{"bug"}, "", "")

	fixture, _ := srv.Node("pr:o/r#9")
	raw, _ := json.Marshal(fixture.Body)
	if err := p.store.upsert(ctx, &node{
		Key: "pr:o/r#9", Typename: "PullRequest", JSON: raw, ContentHash: contentHash(raw),
		FetchedAt: p.now(), UpdatedAt: p.now(),
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := p.store.get(ctx, "pr:o/r#9")

	p.revalidate(ctx)

	after, _ := p.store.get(ctx, "pr:o/r#9")
	if after == nil {
		t.Fatal("pr vanished after revalidate")
	}
	if !reflect.DeepEqual(njson(before.JSON), njson(after.JSON)) {
		t.Fatalf("revalidate changed the PR shape:\n before=%s\n after =%s", before.JSON, after.JSON)
	}
	got := njson(after.JSON)
	if got.HeadRef != "feature/x" || got.BaseRef != "main" {
		t.Errorf("head/base ref lost after revalidate: head=%q base=%q", got.HeadRef, got.BaseRef)
	}
	if len(got.Labels.Nodes) != 1 || got.Labels.Nodes[0].Name != "bug" {
		t.Errorf("labels lost after revalidate: %+v", got.Labels.Nodes)
	}
	if got.UpdatedAt != "2026-09-02T00:00:00Z" || got.Body != "closes #1" {
		t.Errorf("updatedAt/body lost after revalidate: updatedAt=%q body=%q", got.UpdatedAt, got.Body)
	}
}

// TestPointFetchMissStoresGraphQLShape: a cache-miss point read (issue/pr) stores
// the canonical GraphQL shape, not the flat REST body — the sibling defect to the
// revalidate clobber.
func TestPointFetchMissStoresGraphQLShape(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug"}, []string{"alice"})

	n, err := p.resolve(ctx, "issue:o/r#5")
	if err != nil || n == nil {
		t.Fatalf("resolve: %v %v", n, err)
	}
	got := njson(n.JSON)
	if len(got.Labels.Nodes) != 1 || got.Labels.Nodes[0].Name != "bug" {
		t.Errorf("point-fetch miss stored a shape without GraphQL labels: %s", n.JSON)
	}
	if len(got.Assignees.Nodes) != 1 || got.Assignees.Nodes[0].Login != "alice" {
		t.Errorf("point-fetch miss stored a shape without GraphQL assignees: %s", n.JSON)
	}
	if got.UpdatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("point-fetch miss lost updatedAt: %q", got.UpdatedAt)
	}
}

// TestRevalidateEmitsOnlyOnChange: a first-store (no etag) followed by a revalidate
// returning identical content emits nothing; a subsequent upstream change emits
// exactly one github.node.updated (finding B — an etag change with an identical
// body must stay silent).
func TestRevalidateEmitsOnlyOnChange(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	rec := &recorder{}
	rec.install(p)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug"}, []string{"alice"})

	seedIssueViaListPath(t, p, ctx) // no etag stored; content_hash set
	rec.ev = nil

	p.revalidate(ctx) // unconditional 200, identical body → no event
	if n := len(rec.events()); n != 0 {
		t.Fatalf("no-change revalidate emitted %d events, want 0: %+v", n, rec.events())
	}

	srv.Mutate("issue:o/r#5", func(b map[string]any) { b["title"] = "Five!" })
	rec.ev = nil

	p.revalidate(ctx) // changed body → exactly one event
	ev := rec.events()
	if len(ev) != 1 || ev[0].Type != "github.node.updated" {
		t.Fatalf("changed revalidate emits = %+v, want one github.node.updated", ev)
	}
}

// TestFetchKeepsStoredShapeWhenGraphQLPointOpFails: REST 200s for an issue but the
// GraphQL point op (repository.issue) comes back empty — the stored GraphQL-shaped
// node must be kept as-is (labels/assignees/updatedAt intact) rather than clobbered
// with the flat REST body, and no github.node.updated fires (CodeRabbit finding,
// proxy.go canonicalBody fallthrough).
func TestFetchKeepsStoredShapeWhenGraphQLPointOpFails(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	rec := &recorder{}
	rec.install(p)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug", "p1"}, []string{"alice"})

	seedIssueViaListPath(t, p, ctx)
	before, _ := p.store.get(ctx, "issue:o/r#5")
	if before == nil {
		t.Fatal("issue not seeded by the list path")
	}

	// GraphQL endpoint now answers every query with an empty repository (as if the
	// point op errored or the node vanished); REST (srv) still serves the issue.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{}}}`))
	}))
	defer broken.Close()
	p.cfg.graphqlURL = broken.URL
	rec.ev = nil

	n, outcome, err := p.fetchNode(ctx, "issue:o/r#5", before)
	if err != nil {
		t.Fatalf("fetchNode: %v", err)
	}
	if outcome != outcomeOther {
		t.Errorf("outcome = %v, want outcomeOther", outcome)
	}
	if n == nil || !reflect.DeepEqual(njson(n.JSON), njson(before.JSON)) {
		t.Fatalf("fetchNode did not keep the stored node:\n before=%s\n got   =%s", before.JSON, n.JSON)
	}

	after, _ := p.store.get(ctx, "issue:o/r#5")
	if !reflect.DeepEqual(njson(before.JSON), njson(after.JSON)) {
		t.Fatalf("store was clobbered with the flat REST body:\n before=%s\n after =%s", before.JSON, after.JSON)
	}
	got := njson(after.JSON)
	if len(got.Labels.Nodes) != 2 || got.Labels.Nodes[0].Name != "bug" {
		t.Errorf("labels lost after failed canonicalization: %+v", got.Labels.Nodes)
	}
	if len(got.Assignees.Nodes) != 1 || got.Assignees.Nodes[0].Login != "alice" {
		t.Errorf("assignees lost after failed canonicalization: %+v", got.Assignees.Nodes)
	}
	if got.UpdatedAt != "2026-09-01T00:00:00Z" {
		t.Errorf("updatedAt lost after failed canonicalization: %q", got.UpdatedAt)
	}
	if evs := rec.events(); len(evs) != 0 {
		t.Errorf("emitted %d events on a failed canonicalization, want 0: %+v", len(evs), evs)
	}
}

// TestNonPinnedNodesCarriesJSONForReconcileShapeGuard: nonPinnedNodes must return
// each node's stored JSON, not just key/etag/content_hash — otherwise fetchNode's
// isGraphQLShape guard (proxy.go) always sees a nil body on the reconcile path and
// can never fire, so a failed canonicalization during reconcile silently clobbers a
// GraphQL-shaped node with the flat REST body (the bug this test pins, mirroring
// TestFetchKeepsStoredShapeWhenGraphQLPointOpFails but through revalidate/
// nonPinnedNodes instead of a direct fetchNode call).
func TestNonPinnedNodesCarriesJSONForReconcileShapeGuard(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	rec := &recorder{}
	rec.install(p)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug", "p1"}, []string{"alice"})

	seedIssueViaListPath(t, p, ctx)
	before, _ := p.store.get(ctx, "issue:o/r#5")
	if before == nil {
		t.Fatal("issue not seeded by the list path")
	}

	nodes, err := p.store.nonPinnedNodes(ctx)
	if err != nil {
		t.Fatalf("nonPinnedNodes: %v", err)
	}
	var got *node
	for _, n := range nodes {
		if n.Key == "issue:o/r#5" {
			got = n
		}
	}
	if got == nil {
		t.Fatal("nonPinnedNodes did not return the seeded issue")
	}
	if len(got.JSON) == 0 {
		t.Fatal("nonPinnedNodes returned a nil/empty JSON body — the reconcile-path shape guard can never fire")
	}

	// GraphQL endpoint now answers every query with an empty repository, as
	// revalidate would hit on a real canonicalization failure during reconcile.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{}}}`))
	}))
	defer broken.Close()
	p.cfg.graphqlURL = broken.URL
	rec.ev = nil

	p.revalidate(ctx)

	after, _ := p.store.get(ctx, "issue:o/r#5")
	if !reflect.DeepEqual(njson(before.JSON), njson(after.JSON)) {
		t.Fatalf("reconcile clobbered the stored node with the flat REST body:\n before=%s\n after =%s", before.JSON, after.JSON)
	}
	if evs := rec.events(); len(evs) != 0 {
		t.Errorf("emitted %d events on a failed reconcile canonicalization, want 0: %+v", len(evs), evs)
	}
}

// TestReconcileLogsSummary: revalidate logs one INFO line with the pass tally.
func TestReconcileLogsSummary(t *testing.T) {
	srv := fakegh.New()
	defer srv.Close()
	p := newReconcilePlugin(t, srv)
	ctx := context.Background()
	srv.AddRepo("o", "r")
	srv.AddRichIssue("o", "r", 5, "Five", "OPEN", "2026-09-01T00:00:00Z", []string{"bug"}, nil)
	seedIssueViaListPath(t, p, ctx)

	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	p.revalidate(ctx)

	if !strings.Contains(buf.String(), "github: reconcile checked=1 notModified=0 fetched=1 changed=0") {
		t.Errorf("missing reconcile summary line; got:\n%s", buf.String())
	}
}
