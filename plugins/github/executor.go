package github

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/parser"
)

//go:embed queries/*.graphql
var queryFS embed.FS

// namedOp is one loaded queries/*.graphql operation. scope is the list result's
// covering prefix template ("" for a point op); keys are the object-key templates
// the op touches; query is the full document (its `#`-prefixed header lines are
// valid GraphQL comments and pass through to GitHub unchanged).
type namedOp struct {
	name  string
	scope string
	keys  []string
	query string
}

var placeholderRe = regexp.MustCompile(`\{([a-zA-Z]+)\}`)

// isIntrospectionOnly reports whether query is a pure introspection document:
// every top-level selection on every operation is exactly __schema or __type
// (a substring check false-matches "{__typename}", so this parses instead).
func isIntrospectionOnly(query string) bool {
	doc, err := parser.ParseQuery(&ast.Source{Input: query})
	if err != nil || len(doc.Operations) == 0 {
		return false
	}
	for _, op := range doc.Operations {
		if len(op.SelectionSet) == 0 {
			return false
		}
		for _, sel := range op.SelectionSet {
			field, ok := sel.(*ast.Field)
			if !ok || (field.Name != "__schema" && field.Name != "__type") {
				return false
			}
		}
	}
	return true
}

// loadOps parses every embedded .graphql file's `# op / # scope / # keys` header.
func loadOps() map[string]namedOp {
	ops := map[string]namedOp{}
	entries, _ := queryFS.ReadDir("queries")
	for _, e := range entries {
		b, err := queryFS.ReadFile("queries/" + e.Name())
		if err != nil {
			continue
		}
		op := namedOp{query: string(b)}
		for _, line := range strings.Split(string(b), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(t, "# op:"):
				op.name = strings.TrimSpace(strings.TrimPrefix(t, "# op:"))
			case strings.HasPrefix(t, "# scope:"):
				op.scope = strings.TrimSpace(strings.TrimPrefix(t, "# scope:"))
			case strings.HasPrefix(t, "# keys:"):
				op.keys = append(op.keys, strings.TrimSpace(strings.TrimPrefix(t, "# keys:")))
			}
		}
		if op.name != "" {
			ops[op.name] = op
		}
	}
	return ops
}

// nodeResult is one resolved/stored object in a query response.
type nodeResult struct {
	Key      string          `json:"key"`
	ETag     string          `json:"etag"`
	Typename string          `json:"typename"`
	Node     json.RawMessage `json:"node"`
}

// queryResult is the JSON the executor returns. Data carries a list op's raw
// GraphQL data; Nodes carries the objects stored under their own keys; Scope names
// the covering tag a purge evicts on.
type queryResult struct {
	Data  map[string]json.RawMessage `json:"data,omitempty"`
	Nodes []nodeResult               `json:"nodes,omitempty"`
	Scope string                     `json:"scope,omitempty"`
}

// handleGraphQL serves POST {op|query, variables} at /plugins/github/graphql. A
// named op runs through the scoped executor; a raw query is proxied to GitHub.
func (p *Plugin) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxGraphQLBody) // S2: cap before decode
	var req struct {
		Query     string         `json:"query"`
		Op        string         `json:"op"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", pluginconfig.ReadErrStatus(err))
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")

	// A raw {query} bypasses declared-key scoping and stores nothing, so it is
	// rejected — except introspection (`supergraph schema <Type>`), the one raw query
	// that legitimately needs the upstream schema and touches no cache (P2).
	if req.Op == "" {
		if !isIntrospectionOnly(req.Query) {
			http.Error(w, `request must set "op": raw {query} passthrough is disabled`, http.StatusBadRequest)
			return
		}
		data, err := p.httpGraphQL(ctx, req.Query, req.Variables)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		return
	}
	op, ok := p.ops[req.Op]
	if !ok {
		http.Error(w, "unknown op", http.StatusNotFound)
		return
	}
	// S1: reject unsafe owner/repo variables before any key/path is built.
	if !safeName(coerce(req.Variables["owner"])) || !safeName(coerce(req.Variables["repo"])) {
		http.Error(w, "invalid owner/repo in variables", http.StatusBadRequest)
		return
	}
	res, err := p.runOp(ctx, op, req.Variables)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(res)
}

// maxGraphQLBody caps the POST /graphql body (S2): the executor only needs an op
// name plus a few variables, so 256 KiB is generous and bounds pre-decode memory.
const maxGraphQLBody = 256 << 10

// runOp executes a named op: a list op fetches from GitHub, stores each returned
// node under its own key, and caches the list result tagged with its covering
// scope; a point op resolves each declared key through the read-through cache.
func (p *Plugin) runOp(ctx context.Context, op namedOp, vars map[string]any) (queryResult, error) {
	if op.scope == "" {
		var nodes []nodeResult
		var bodies []json.RawMessage
		for _, kt := range op.keys {
			key := substitute(kt, vars)
			n, err := p.resolve(ctx, key)
			if err != nil {
				return queryResult{}, err
			}
			if n != nil {
				nodes = append(nodes, nodeResult{Key: n.Key, ETag: n.ETag, Typename: n.Typename, Node: n.JSON})
				bodies = append(bodies, n.JSON)
			}
		}
		// U1: a point op returns its object(s) under data.<opName> too, so a uniform
		// {data,nodes,scope} shape renders in the CLI instead of a blank data block.
		data := json.RawMessage("null")
		if len(bodies) == 1 {
			data = bodies[0]
		} else if len(bodies) > 1 {
			data, _ = json.Marshal(bodies)
		}
		return queryResult{Data: map[string]json.RawMessage{op.name: data}, Nodes: nodes}, nil
	}

	scope := substitute(op.scope, vars)
	// U2: a cached list result serves the list body AND its member nodes with zero
	// upstream calls; it is evicted only by the member-purge tag index. A miss (or a
	// post-purge read) refetches. This makes AC-GH-CACHE-HIT hold for list ops too.
	var data map[string]any
	if cached, _ := p.store.get(ctx, listKey(op.name, scope)); cached != nil {
		_ = json.Unmarshal(cached.JSON, &data)
	} else {
		var err error
		if data, err = p.httpGraphQL(ctx, op.query, vars); err != nil {
			return queryResult{}, err
		}
	}
	raw, _ := json.Marshal(data)

	var nodes []nodeResult
	comp := ""
	for _, kt := range op.keys {
		if comp == "" {
			comp = scope + "|" + kindOf(kt)
		}
		nodes = append(nodes, p.extractAndStore(ctx, kt, vars, data)...)
	}
	if err := p.store.putList(ctx, listKey(op.name, scope), raw, comp, p.now()); err != nil {
		return queryResult{}, err
	}
	return queryResult{
		Data:  map[string]json.RawMessage{op.name: raw},
		Nodes: nodes,
		Scope: scope,
	}, nil
}

// extractAndStore resolves a key template against a list response: it substitutes
// the caller's vars, treats any remaining {field} placeholders as per-node fields,
// and stores every response object carrying those fields under its built key.
func (p *Plugin) extractAndStore(ctx context.Context, template string, vars map[string]any, data any) []nodeResult {
	resolved := substitute(template, vars)
	fields := placeholderRe.FindAllStringSubmatch(resolved, -1)
	if len(fields) == 0 {
		if n, err := p.resolve(ctx, resolved); err == nil && n != nil {
			return []nodeResult{{Key: n.Key, ETag: n.ETag, Typename: n.Typename, Node: n.JSON}}
		}
		return nil
	}
	want := make([]string, len(fields))
	for i, f := range fields {
		want[i] = f[1]
	}
	var out []nodeResult
	now := p.now()
	for _, obj := range collectObjects(data, want) {
		key := resolved
		for _, f := range want {
			key = strings.ReplaceAll(key, "{"+f+"}", coerce(obj[f]))
		}
		body, _ := json.Marshal(obj)
		n := &node{
			Key: key, Typename: typenameFor(key), JSON: body, ContentHash: contentHash(body),
			Pinned: p.evalPin(key, obj), FetchedAt: now, UpdatedAt: now,
		}
		if err := p.store.upsert(ctx, n); err != nil {
			continue
		}
		out = append(out, nodeResult{Key: key, Typename: n.Typename, Node: body})
	}
	return out
}

// substitute replaces {var} placeholders present in vars; unresolved placeholders
// (per-node fields) are left in place for extractAndStore.
func substitute(template string, vars map[string]any) string {
	return placeholderRe.ReplaceAllStringFunc(template, func(m string) string {
		name := m[1 : len(m)-1]
		if v, ok := vars[name]; ok {
			return coerce(v)
		}
		return m
	})
}

// collectObjects walks a decoded JSON value and returns every object that carries
// all of fields — the list op's per-node objects.
func collectObjects(v any, fields []string) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case map[string]any:
		if hasAll(t, fields) {
			out = append(out, t)
		}
		for _, child := range t {
			out = append(out, collectObjects(child, fields)...)
		}
	case []any:
		for _, child := range t {
			out = append(out, collectObjects(child, fields)...)
		}
	}
	return out
}

func hasAll(m map[string]any, fields []string) bool {
	for _, f := range fields {
		if _, ok := m[f]; !ok {
			return false
		}
	}
	return true
}

// coerce renders a JSON scalar for a key segment (integral floats without a decimal
// point, so issue number 5.0 → "5").
func coerce(v any) string {
	switch n := v.(type) {
	case string:
		return n
	case float64:
		if n == float64(int64(n)) {
			return strconv.FormatInt(int64(n), 10)
		}
		return strconv.FormatFloat(n, 'f', -1, 64)
	case int:
		return strconv.Itoa(n)
	case bool:
		return strconv.FormatBool(n)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}
