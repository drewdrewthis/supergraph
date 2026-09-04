package features

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"
)

// TestMain builds the supergraph binary once into a temp dir and resolves the
// module root, so every scenario drives the same freshly-built binary as a
// subprocess rather than re-building per scenario.
func TestMain(m *testing.M) {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "features: getwd:", err)
		os.Exit(1)
	}
	repoRoot = filepath.Dir(wd) // features/ -> module root

	tmp, err := os.MkdirTemp("", "sg-bin-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "features: mktemp:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmp)

	binPath = filepath.Join(tmp, "supergraph")
	build := exec.Command("go", "build", "-o", binPath, "../cmd/supergraph")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "features: build supergraph:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// tagExpr computes the default godog tag filter: exclude the deliberately-unmet
// scenario (AC-CORE-9 red proof), exclude the real-service scenarios (opt-in via
// FEATURES_SERVICE + an explicit FEATURES_TAGS), and exclude the OTHER OS's
// install/lifecycle scenarios. FEATURES_TAGS overrides the whole expression, so
// `FEATURES_TAGS=@unmet go test ./features/` runs only the red scenario.
func tagExpr() string {
	if v := os.Getenv("FEATURES_TAGS"); v != "" {
		return v
	}
	expr := "~@unmet && ~@service"
	if runtime.GOOS != "linux" {
		expr += " && ~@linux"
	}
	if runtime.GOOS != "darwin" {
		expr += " && ~@darwin"
	}
	return expr
}

func TestFeatures(t *testing.T) {
	suite := godog.TestSuite{
		Name:                "supergraph",
		ScenarioInitializer: InitializeScenario,
		Options: &godog.Options{
			Format:    "pretty",
			Paths:     []string{".", "../plugins/template"},
			Tags:      tagExpr(),
			Strict:    true, // undefined/pending steps FAIL, never skip (AC-CORE-9)
			Randomize: 0,
			TestingT:  t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("godog suite reported a non-zero status")
	}
}

// runEmbeddedMetSuite runs a tiny in-process godog suite over a single satisfied
// scenario and returns its exit status. AC-CORE-9's green step uses it to show the
// runner reports 0 for a fully-satisfied scenario, without recursively invoking
// `go test` on this same package.
func runEmbeddedMetSuite() int {
	const feature = `Feature: embedded green check
  Scenario: a satisfied scenario is green
    Given a satisfied step
`
	suite := godog.TestSuite{
		Name: "embedded-met",
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			sc.Step(`^a satisfied step$`, func() error { return nil })
		},
		Options: &godog.Options{
			Format:          "pretty",
			Output:          io.Discard,
			Strict:          true,
			FeatureContents: []godog.Feature{{Name: "embedded", Contents: []byte(feature)}},
		},
	}
	return suite.Run()
}
