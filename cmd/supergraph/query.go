package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
)

// defaultQueryEndpoint is the loopback GraphQL URL `query` targets when no config is
// present, so the command works out of the box without a full config file.
const defaultQueryEndpoint = "http://127.0.0.1:7788/graphql"

func queryCmd() *cobra.Command {
	var endpoint string
	cmd := &cobra.Command{
		Use:   "query '<graphql>'",
		Short: "Run a GraphQL query against the local server and print JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return runQuery(resolveEndpoint(endpoint, configPath), args[0])
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

func runQuery(endpoint, query string) error {
	body, _ := json.Marshal(map[string]string{"query": query})
	// endpoint comes from --endpoint or the operator's own config Listen, not
	// attacker input, so posting to a variable URL here is intended.
	resp, err := http.Post(endpoint, "application/json", bytes.NewReader(body)) //nolint:gosec // operator-supplied endpoint, not attacker input
	if err != nil {
		return fmt.Errorf("query transport: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	// Decode to surface GraphQL-level errors as a non-zero exit, distinct from a
	// transport failure above.
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
