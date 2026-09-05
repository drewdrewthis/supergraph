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

// version is stamped at build time via
// -ldflags "-X main.version=$(git describe --tags --always --dirty)". It defaults
// to "dev" for a plain `go build`/`go run`. A git-describe stamp
// (v1.2.3-4-gabc1234-dirty) is valid semver, so a bare release-tag check cannot
// treat it as a release — see
// sol.2026-09-03-git-describe-version-string-is-valid-semver-not-non-semver.
var version = "dev"

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "supergraph",
		Short:         "Per-host data-source aggregator (GraphQL + health)",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", core.DefaultConfigPath(),
		"path to config.toml")
	root.AddCommand(serveCmd(), queryCmd(), schemaCmd())
	root.AddCommand(serviceCmds()...)
	return root
}

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
