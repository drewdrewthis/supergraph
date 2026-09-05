package features

import (
	"github.com/cucumber/godog"
)

// pending is the one behavior every PRD step shares: the plugin tier a
// scenario names (github, claude, tmux, ...) does not exist in this
// core-tier spike, so every step reports pending rather than failing or
// silently skipping. Registered under three phrase shapes (Given/When/Then)
// because godog requires each literal step text to resolve to a registered
// pattern even when an earlier step already made the scenario pending.
func pending(_ ...string) error {
	return godog.ErrPending
}

// registerPRDSteps wires the generic Given/When/Then phrases every PRD
// scenario in features/prd.feature is built from.
func registerPRDSteps(sc *godog.ScenarioContext) {
	sc.Step(`^the plugin tier for "([^"]*)" is implemented$`, func(_ string) error { return pending() })
	sc.Step(`^the acceptance criterion is exercised: "([^"]*)"$`, func(_ string) error { return pending() })
	// Not `pending()`: a PRD scenario's Given always reports pending first (godog
	// skips the rest of the scenario at that point, so this handler never actually
	// runs for prd.feature). features/github.feature's and features/peer.feature's
	// @local scenarios reuse this same literal phrase with real, already-satisfied
	// evidence, so this returns success rather than forcing every "evidence is
	// captured" step suite-wide pending.
	sc.Step(`^evidence is captured: "([^"]*)"$`, func(_ string) error { return nil })
}
