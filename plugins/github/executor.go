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
	var req struct {
		Query     string         `json:"query"`
		Op        string         `json:"op"`
		Variables map[string]any `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")

	if req.Op != "" {
		op, ok := p.ops[req.Op]
		if !ok {
			http.Error(w, "unknown op", http.StatusNotFound)
			return
		}
		res, err := p.runOp(ctx, op, req.Variables)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(res)
		return
	}
	data, err := p.httpGraphQL(ctx, req.Query, req.Variables)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// runOp executes a named op: a list op fetches from GitHub, stores each returned
// node under its own key, and caches the list result tagged with its covering
// scope; a point op resolves each declared key through the read-through cache.
func (p *Plugin) runOp(ctx context.Context, op namedOp, vars map[string]any) (queryResult, error) {
	if op.scope == "" {
		var nodes []nodeResult
		for _, kt := range op.keys {
			key := substitute(kt, vars)
			n, err := p.resolve(ctx, key)
			if err != nil {
				return queryResult{}, err
			}
			if n != nil {
				nodes = append(nodes, nodeResult{Key: n.Key, ETag: n.ETag, Typename: n.Typename, Node: n.JSON})
			}
		}
		return queryResult{Nodes: nodes}, nil
	}

	scope := substitute(op.scope, vars)
	data, err := p.httpGraphQL(ctx, op.query, vars)
	if err != nil {
		return queryResult{}, err
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
			Key: key, Typename: typenameFor(key), JSON: body,
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
