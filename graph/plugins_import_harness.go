//go:build harness

package graph

// Harness-only reference plugins, compiled in ONLY under the `harness` build tag so
// they never register in a production binary. See graph/plugins_import.go for the rule.
import (
	// fakeok stays "ok" while a sibling plugin panics (panic-isolation demo, AC-CORE-4).
	_ "github.com/drewdrewthis/supergraph/plugins/fakeok"
	// fakeremote stands in for a remote box's source executor in the peer @local scenarios.
	_ "github.com/drewdrewthis/supergraph/plugins/fakeremote"
)
