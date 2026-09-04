//go:build harness

package graph

// fakeok is a harness-only reference plugin used by the panic-isolation demo
// (make dev / dev-check, features -tags harness): it stays "ok" while a sibling
// plugin panics. It is compiled in ONLY under the `harness` build tag so it never
// registers in a production binary. See graph/plugins_import.go for the rule.
import _ "github.com/drewdrewthis/supergraph/plugins/fakeok"
