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

// defaultQueriesDir is where `query --op` looks for plugins/github/queries/NAME.graphql
// relative to the current working directory, unless --queries-dir overrides it.
const defaultQueriesDir = "./plugins/github/queries"

func queryCmd() *cobra.Command {
	var endpoint, op, queriesDir string
	var varArgs []string
	cmd := &cobra.Command{
		Use:   "query ['<graphql>'] [--op NAME [--var k=v ...]]",
		Short: "Run a GraphQL query and print JSON: a raw query string, or a named plugin op",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			if op != "" {
				return runNamedQuery(githubEndpoint(endpoint, configPath), op, varArgs, queriesDir)
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
		"named operation to load from plugins/github/queries/NAME.graphql and run against the github plugin")
	cmd.Flags().StringArrayVar(&varArgs, "var", nil,
		"operation variable as key=value; value is parsed as JSON when it parses, else kept as a string (repeatable)")
	cmd.Flags().StringVar(&queriesDir, "queries-dir", defaultQueriesDir,
		"directory to search for named .graphql operation files")
	return cmd
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

// runNamedQuery loads plugins/github/queries/<op>.graphql, parses --var
// key=value pairs into GraphQL variables, and posts the operation to the
// github plugin's endpoint.
func runNamedQuery(endpoint, op string, varArgs []string, queriesDir string) error {
	queryText, err := loadNamedQuery(op, queriesDir)
	if err != nil {
		return err
	}
	variables, err := parseVars(varArgs)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"query": queryText, "variables": variables})
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
