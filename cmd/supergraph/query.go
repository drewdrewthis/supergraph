package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
)

func queryCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "query '<graphql>'",
		Short: "Run a GraphQL query against the local server and print JSON",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			cfg, err := core.LoadConfig(configPath)
			if err != nil {
				return err
			}
			return runQuery(cfg.Listen, args[0])
		},
	}
}

func runQuery(listen, query string) error {
	body, _ := json.Marshal(map[string]string{"query": query})
	resp, err := http.Post("http://"+listen+"/graphql", "application/json", bytes.NewReader(body))
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
