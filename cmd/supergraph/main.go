// Command supergraph is the single binary per box: it serves the aggregated
// GraphQL/health API and manages its own OS service. Subcommands are wired with
// cobra; core owns all behaviour, this package only parses flags and prints.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/drewdrewthis/supergraph/core"
)

// configPath is the resolved --config value, shared by every subcommand.
var configPath string

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "supergraph",
		Short:         "Per-host data-source aggregator (GraphQL + health)",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", core.DefaultConfigPath(),
		"path to config.toml")
	root.AddCommand(serveCmd(), queryCmd())
	root.AddCommand(serviceCmds()...)
	return root
}

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
