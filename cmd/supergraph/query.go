package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
)

// defaultQueryEndpoint is the loopback GraphQL URL `query` targets when no config is
// present, so the command works out of the box without a full config file.
const defaultQueryEndpoint = "http://127.0.0.1:7788/graphql"

// pluginsWithOpRoute lists plugins that serve a POST {op,variables} route which keys
// each returned node under its own cache key (github's caching path). Any other
// --plugin has no such route, so `--op` falls back to posting the loaded query TEXT to
// core's /graphql — a plain read, no keying.
var pluginsWithOpRoute = map[string]bool{"github": true}

func queryCmd() *cobra.Command {
	var endpoint, op, queriesDir, plugin string
	var varArgs []string
	cmd := &cobra.Command{
		Use:   "query ['<graphql>'] [--op NAME [--plugin P] [--var k=v ...]]",
		Short: "Run a GraphQL query and print JSON: a raw query string, or a named plugin op",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if op != "" {
				url, opRoute := pluginEndpoint(plugin, endpoint, configPath)
				return runNamedQuery(url, op, varArgs, queriesDirFor(queriesDir, plugin), opRoute)
			}
			if len(args) != 1 {
				return fmt.Errorf("query: pass a GraphQL string, or --op NAME")
			}
			return runQuery(resolveEndpoint(endpoint, configPath), args[0])
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultQueryEndpoint,
		"GraphQL endpoint URL (overrides the config's listen address)")
	cmd.Flags().StringVar(&op, "op", "",
		"named operation to load from plugins/<plugin>/queries/NAME.graphql and run")
	cmd.Flags().StringVar(&plugin, "plugin", "github",
		"plugin that owns the --op query dir and route (github uses its op route; others post the query to core /graphql)")
	cmd.Flags().StringArrayVar(&varArgs, "var", nil,
		"operation variable as key=value; value is parsed as JSON when it parses, else kept as a string (repeatable)")
	cmd.Flags().StringVar(&queriesDir, "queries-dir", "",
		"directory to search for named .graphql operation files (default ./plugins/<plugin>/queries)")
	return cmd
}

// queriesDirFor resolves the op search dir: an explicit --queries-dir wins, else it is
// derived from --plugin so each plugin's ops live under its own tree.
func queriesDirFor(dir, plugin string) string {
	if dir != "" {
		return dir
	}
	return "./plugins/" + plugin + "/queries"
}

// pluginEndpoint resolves where a named op is posted and whether that target is a
// plugin op route. github posts {op,variables} to /plugins/github/graphql (keying
// path); every other plugin has no op route, so its query text goes to core /graphql.
func pluginEndpoint(plugin, endpoint, cfgPath string) (url string, opRoute bool) {
	base := strings.TrimSuffix(resolveEndpoint(endpoint, cfgPath), "/graphql")
	if pluginsWithOpRoute[plugin] {
		return base + "/plugins/" + plugin + "/graphql", true
	}
	return base + "/graphql", false
}

func schemaCmd() *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:   "schema <Type>",
		Short: "Print a readable listing of a GraphQL type served by the github plugin",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runSchema(githubEndpoint(endpoint, configPath), args[0])
		},
	}
	cmd.Flags().StringVar(&endpoint, "endpoint", defaultQueryEndpoint,
		"GraphQL endpoint URL (overrides the config's listen address)")
	return cmd
}

// resolveEndpoint chooses the endpoint without requiring a full config: an explicit
// --endpoint wins; otherwise, if the config file exists AND loads, its Listen is
// used; otherwise the loopback default. A missing or invalid config (including one
// without hostId) is NOT fatal for `query` — it silently falls back to the default.
func resolveEndpoint(endpoint, cfgPath string) string {
	if endpoint != defaultQueryEndpoint {
		return endpoint
	}
	if _, err := os.Stat(cfgPath); err == nil {
		if cfg, err := core.LoadConfig(cfgPath); err == nil {
			return "http://" + cfg.Listen + "/graphql"
		}
	}
	return endpoint
}

// githubEndpoint derives the github plugin's GraphQL endpoint from the same
// --endpoint/config resolution as the base `query` path, so `--op`/`schema`
// default to the same host:port without a second flag. It trims a trailing
// "/graphql" (present on the default and a config-derived value) before
// appending the plugin's own route.
func githubEndpoint(endpoint, cfgPath string) string {
	base := strings.TrimSuffix(resolveEndpoint(endpoint, cfgPath), "/graphql")
	return base + "/plugins/github/graphql"
}

func runQuery(endpoint, query string) error {
	body, _ := json.Marshal(map[string]string{"query": query})
	raw, err := postGraphQL(endpoint, body)
	if err != nil {
		return err
	}
	return printGraphQLResult(raw)
}

// runNamedQuery loads NAME.graphql from the search path, parses --var key=value pairs
// into GraphQL variables, and posts the operation. When opRoute is true (github) it
// posts {op, variables} by NAME so the plugin's runOp/extractAndStore path keys each
// returned node (AC-GH-NAMEDOP-KEYS). Otherwise it posts the loaded query TEXT as
// {query, variables} to core /graphql — a plain read for plugins with no op route.
func runNamedQuery(endpoint, op string, varArgs []string, queriesDir string, opRoute bool) error {
	query, err := loadNamedQuery(op, queriesDir)
	if err != nil {
		return err
	}
	variables, err := parseVars(varArgs)
	if err != nil {
		return err
	}
	req := map[string]any{"query": query, "variables": variables}
	if opRoute {
		req = map[string]any{"op": op, "variables": variables}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}
	raw, err := postGraphQL(endpoint, body)
	if err != nil {
		return err
	}
	return printGraphQLResult(raw)
}

// loadNamedQuery resolves NAME.graphql by search path: --queries-dir (default
// ./plugins/github/queries, relative to cwd), then
// $XDG_CONFIG_HOME/supergraph/queries (or ~/.config/supergraph/queries when
// XDG_CONFIG_HOME is unset). Deliberately NOT embedded from plugins/github, to
// avoid a cmd→plugins build dependency on files owned by another package.
func loadNamedQuery(op, queriesDir string) (string, error) {
	candidates := []string{filepath.Join(queriesDir, op+".graphql")}
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "supergraph", "queries", op+".graphql"))
	} else if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "supergraph", "queries", op+".graphql"))
	}
	for _, path := range candidates {
		// path is built from --queries-dir/XDG config search roots (operator
		// config, not attacker input), so a variable path here is intended.
		if data, err := os.ReadFile(path); err == nil { //nolint:gosec // operator-supplied search path, not attacker input
			return string(data), nil
		}
	}
	return "", fmt.Errorf("named operation %q not found (searched %s)", op, strings.Join(candidates, ", "))
}

// parseVars turns "--var k=v" pairs into a variables map. Each value is
// parsed as JSON when it parses (so --var limit=5 or --var open=true work),
// else kept as a plain string.
func parseVars(varArgs []string) (map[string]any, error) {
	variables := map[string]any{}
	for _, kv := range varArgs {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --var %q: expected key=value", kv)
		}
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err == nil {
			variables[key] = parsed
		} else {
			variables[key] = value
		}
	}
	return variables, nil
}

// postGraphQL POSTs a pre-encoded {query, variables} body and returns the raw
// response bytes.
func postGraphQL(endpoint string, body []byte) ([]byte, error) {
	// endpoint comes from --endpoint or the operator's own config Listen, not
	// attacker input, so posting to a variable URL here is intended.
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body)) //nolint:gosec // operator-supplied endpoint, not attacker input
	if err != nil {
		return nil, fmt.Errorf("query transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return raw, nil
}

// printGraphQLResult decodes a GraphQL envelope, surfacing GraphQL-level
// errors as a non-zero exit distinct from a transport failure, and
// pretty-prints the data payload.
func printGraphQLResult(raw []byte) error {
	var envelope struct {
		Data   json.RawMessage   `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode response %q: %w", string(raw), err)
	}
	if len(envelope.Errors) > 0 {
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, raw, "", "  ")
		return fmt.Errorf("graphql errors: %s", pretty.String())
	}

	var pretty bytes.Buffer
	if err := json.Indent(&pretty, envelope.Data, "", "  "); err != nil {
		fmt.Println(string(envelope.Data))
		return nil
	}
	fmt.Println(pretty.String())
	return nil
}
