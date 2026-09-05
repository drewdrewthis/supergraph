package fakegh

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// gqlRequest is the standard GraphQL POST body.
type gqlRequest struct {
	Query         string         `json:"query"`
	Variables     map[string]any `json:"variables"`
	OperationName string         `json:"operationName"`
}

// handleGraphQL dispatches the EDR's named ops against the same world and always
// includes a rateLimit{remaining,resetAt,cost} block. Points are charged once per
// request (clamped at the floor), so the near-floor pause is drivable.
func (s *Server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var req gqlRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	op := req.OperationName
	if op == "" {
		op = parseOpName(req.Query)
	}

	s.mu.Lock()
	s.chargeGQL()
	data := map[string]any{
		"rateLimit": map[string]any{
			"remaining": s.rate.gqlRemaining,
			"resetAt":   s.rate.gqlReset.UTC().Format(time.RFC3339),
			"cost":      s.rate.gqlCost,
		},
	}
	owner, _ := req.Variables["owner"].(string)
	repo, _ := req.Variables["repo"].(string)
	switch op {
	case "openIssues", "issue", "issueComments", "checkRunsForPR", "pr", "repoLabels":
		data["repository"] = s.repositoryData(op, owner, repo, req.Variables)
	}
	if strings.Contains(req.Query, "__type") {
		if t := introspectType(req.Variables); t != nil {
			data["__type"] = t
		} else {
			data["__type"] = nil
		}
	}
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

// repositoryData builds the `repository` field for an op from the world (caller
// holds mu). Point ops resolve a single node; list ops gather their scope.
func (s *Server) repositoryData(op, owner, repo string, vars map[string]any) map[string]any {
	rep := map[string]any{}
	prefix := "issue:" + owner + "/" + repo + "#"
	switch op {
	case "openIssues":
		rep["issues"] = map[string]any{"nodes": s.nodesWithPrefix(prefix, "open")}
	case "issue":
		if num, ok := numString(vars["number"]); ok {
			if n, ok := s.world.nodes[prefix+num]; ok {
				rep["issue"] = n.Body
			}
		}
	case "issueComments":
		rep["issue"] = map[string]any{"comments": map[string]any{
			"nodes": s.nodesWithPrefix("comment:"+owner+"/"+repo+"/", ""),
		}}
	case "repoLabels":
		rep["labels"] = map[string]any{"nodes": s.nodesWithPrefix("label:"+owner+"/"+repo+"/", "")}
	case "pr", "checkRunsForPR":
		if num, ok := numString(vars["number"]); ok {
			if n, ok := s.world.nodes["pr:"+owner+"/"+repo+"#"+num]; ok {
				rep["pullRequest"] = n.Body
			}
		}
	}
	return rep
}

// nodesWithPrefix returns the bodies of every node whose key starts with prefix,
// optionally filtered to a lowercase state.
func (s *Server) nodesWithPrefix(prefix, state string) []map[string]any {
	out := []map[string]any{}
	for key, n := range s.world.nodes {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if state != "" {
			if st, _ := n.Body["state"].(string); !strings.EqualFold(st, state) {
				continue
			}
		}
		out = append(out, n.Body)
	}
	return out
}

func numString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, t != ""
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	}
	return "", false
}

// parseOpName extracts the operation name from a GraphQL document
// ("query Foo(...)" / "mutation Bar {" -> "Foo"/"Bar").
func parseOpName(query string) string {
	fields := strings.Fields(query)
	for i, f := range fields {
		if f == "query" || f == "mutation" || f == "subscription" {
			if i+1 < len(fields) {
				name := fields[i+1]
				if cut := strings.IndexAny(name, "({"); cut >= 0 {
					name = name[:cut]
				}
				if name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// introspectType answers the `schema <Type>` command's __type(name:) query for the
// handful of object types the plugin surfaces (mirroring what real GitHub GraphQL
// returns), so @local can prove `supergraph schema Issue` without a live API. An
// unknown name yields nil so the CLI reports "unknown type".
func introspectType(vars map[string]any) map[string]any {
	name, _ := vars["name"].(string)
	fields, ok := introspectFields[name]
	if !ok {
		return nil
	}
	return map[string]any{"name": name, "kind": "OBJECT", "description": "", "fields": fields}
}

// introspectFields maps a GraphQL object type to a minimal, stable field listing.
var introspectFields = map[string][]map[string]any{
	"Issue": {
		scalarField("number", "Int"),
		scalarField("title", "String"),
		scalarField("state", "String"),
	},
	"PullRequest": {
		scalarField("number", "Int"),
		scalarField("title", "String"),
		scalarField("state", "String"),
	},
	"CheckRun": {
		scalarField("name", "String"),
		scalarField("status", "String"),
		scalarField("conclusion", "String"),
	},
}

// scalarField builds one introspection field entry wrapping a named scalar.
func scalarField(name, scalar string) map[string]any {
	return map[string]any{
		"name": name,
		"type": map[string]any{"name": scalar, "kind": "SCALAR", "ofType": nil},
	}
}
