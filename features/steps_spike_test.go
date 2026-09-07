package features

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cucumber/godog"
)

// registerSpikeSteps wires features/spike-measure.feature. The @local scenarios are
// cheap filesystem/`make -n` checks over the committed harness + results doc; the
// @slow scenario runs scripts/spike-measure.sh end to end (build + real tmux + two
// serves) and is excluded from the default suite (see suite_test.go tagExpr). Steps
// close over a tiny local state so nothing is added to the shared world.
func registerSpikeSteps(sc *godog.ScenarioContext) {
	s := &spikeState{}
	lit := func(p string) *regexp.Regexp { return regexp.MustCompile("^" + regexp.QuoteMeta(p) + "$") }

	sc.Step(lit("the repo root"), func() error { return nil })
	sc.Step(lit("the file `scripts/spike-measure.sh` exists and is executable"), s.scriptExecutable)

	sc.Step(lit("`make spike-measure -n` is run"), s.makeDryRun)
	sc.Step(lit("the resolved recipe names `scripts/spike-measure.sh`"), s.recipeNamesScript)

	sc.Step(lit("the results doc `docs/spike-results.md`"), s.loadDoc)
	sc.Step(lit("it has a section heading for the (a) latency AC and one for the (c) peer AC"), s.docHasABHeadings)
	sc.Step(lit(`it has an "## Environment" section and a "Blocked on owner credentials" section`), s.docHasEnvBlocked)
	sc.Step(lit(`it has a "What the always-on worker needs from the graph" section`), s.docHasWorker)
	sc.Step(lit("each of the (a) and (c) sections carries a PASS or FAIL token"), s.docHasVerdicts)

	sc.Step(lit("`scripts/spike-measure.sh` is run to completion"), s.runFull)
	sc.Step(lit("the SUMMARY block reports a primary p95 verdict and a stale-marking time"), s.summaryHasNumbers)
	sc.Step(lit("the generated results doc has the required sections and a PASS/FAIL token per AC"), s.generatedDocHasSectionsAndVerdicts)
	sc.Step(lit("the committed `docs/spike-results.md` is unchanged in git"), s.committedDocUntouched)
	sc.Step(lit("no leftover spike process or tmux `-L sgmeasure` session remains"), s.noLeftovers)
}

type spikeState struct {
	makeOut string
	doc     string
	summary string
}

func (s *spikeState) path(rel string) string { return filepath.Join(repoRoot, rel) }

func (s *spikeState) scriptExecutable() error {
	fi, err := os.Stat(s.path("scripts/spike-measure.sh"))
	if err != nil {
		return err
	}
	if fi.Mode()&0o111 == 0 {
		return fmt.Errorf("scripts/spike-measure.sh is not executable (mode %v)", fi.Mode())
	}
	return nil
}

func (s *spikeState) makeDryRun() error {
	cmd := exec.Command("make", "-C", repoRoot, "spike-measure", "-n")
	out, err := cmd.CombinedOutput()
	s.makeOut = string(out)
	if err != nil {
		return fmt.Errorf("make -n: %v: %s", err, out)
	}
	return nil
}

func (s *spikeState) recipeNamesScript() error {
	if !strings.Contains(s.makeOut, "spike-measure.sh") {
		return fmt.Errorf("make -n output does not name spike-measure.sh: %q", s.makeOut)
	}
	return nil
}

func (s *spikeState) loadDoc() error {
	b, err := os.ReadFile(s.path("docs/spike-results.md"))
	if err != nil {
		return err
	}
	s.doc = string(b)
	return nil
}

func (s *spikeState) docHasABHeadings() error {
	// The (a) latency and (c) peer sections are markdown H2s that begin with "## (a)"
	// and "## (c)"; require both.
	for _, h := range []string{"## (a)", "## (c)"} {
		if !strings.Contains(s.doc, h) {
			return fmt.Errorf("results doc missing section heading %q", h)
		}
	}
	return nil
}

func (s *spikeState) docHasEnvBlocked() error {
	for _, h := range []string{"## Environment", "Blocked on owner credentials"} {
		if !strings.Contains(s.doc, h) {
			return fmt.Errorf("results doc missing section %q", h)
		}
	}
	return nil
}

func (s *spikeState) docHasWorker() error {
	if !strings.Contains(s.doc, "What the always-on worker needs from the graph") {
		return fmt.Errorf("results doc missing the worker-needs section")
	}
	return nil
}

func (s *spikeState) docHasVerdicts() error {
	// A PASS or FAIL token must appear in the (a) block and in the (c) block. Split on
	// the two headings so a token in one section does not satisfy the other.
	ia := strings.Index(s.doc, "## (a)")
	ic := strings.Index(s.doc, "## (c)")
	if ia < 0 || ic < 0 || ic <= ia {
		return fmt.Errorf("results doc (a)/(c) sections not both present in order")
	}
	blockA := s.doc[ia:ic]
	blockC := s.doc[ic:]
	if !hasVerdict(blockA) {
		return fmt.Errorf("(a) section carries no PASS/FAIL token")
	}
	if !hasVerdict(blockC) {
		return fmt.Errorf("(c) section carries no PASS/FAIL token")
	}
	return nil
}

func hasVerdict(block string) bool {
	return strings.Contains(block, "PASS") || strings.Contains(block, "FAIL")
}

func (s *spikeState) runFull() error {
	cmd := exec.Command("bash", s.path("scripts/spike-measure.sh"))
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	s.summary = string(out)
	if err != nil {
		return fmt.Errorf("spike-measure.sh failed: %v\n%s", err, tail(string(out), 2000))
	}
	return nil
}

func (s *spikeState) summaryHasNumbers() error {
	if !strings.Contains(s.summary, "SPIKE SUMMARY") {
		return fmt.Errorf("no SPIKE SUMMARY block in output:\n%s", tail(s.summary, 2000))
	}
	// Both latency paths (HTTP + CLI), the kill-anchored stale figure, and the
	// no-peer-of-peer verdict must all be present (AC-SPIKE-LATENCY/PEER-STALE).
	for _, k := range []string{"primary=", "primary_cli=", "t_stale_ms=", "t_stale_kill_ms=", "peer_of_peer=", `"verdict"`} {
		if !strings.Contains(s.summary, k) {
			return fmt.Errorf("SUMMARY missing %q", k)
		}
	}
	return nil
}

// resultsDocPath extracts the results_doc=<path> the summary records (the temp-dir file
// a bare run writes), so the @slow scenario can assert its content without the run
// having touched the committed doc.
func (s *spikeState) resultsDocPath() (string, error) {
	for _, line := range strings.Split(s.summary, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "results_doc="); ok {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("SUMMARY has no results_doc= line:\n%s", tail(s.summary, 2000))
}

func (s *spikeState) generatedDocHasSectionsAndVerdicts() error {
	p, err := s.resultsDocPath()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p) //nolint:gosec // path comes from our own script's SUMMARY
	if err != nil {
		return fmt.Errorf("reading generated results doc %q: %w", p, err)
	}
	// Clean up the standalone temp doc the bare run created (it lives outside $WORK, so
	// the script's teardown does not remove it). Never touch a real committed doc.
	if strings.Contains(filepath.Base(p), "sg-spike-results-") {
		defer func() { _ = os.Remove(p) }()
	}
	doc := string(b)
	for _, h := range []string{"## (a)", "## (c)", "## Environment", "Blocked on owner credentials",
		"What the always-on worker needs from the graph"} {
		if !strings.Contains(doc, h) {
			return fmt.Errorf("generated results doc missing section %q", h)
		}
	}
	ia, ic := strings.Index(doc, "## (a)"), strings.Index(doc, "## (c)")
	if ia < 0 || ic <= ia {
		return fmt.Errorf("generated doc (a)/(c) sections not both present in order")
	}
	if !hasVerdict(doc[ia:ic]) {
		return fmt.Errorf("generated (a) section carries no PASS/FAIL token")
	}
	if !hasVerdict(doc[ic:]) {
		return fmt.Errorf("generated (c) section carries no PASS/FAIL token")
	}
	return nil
}

func (s *spikeState) committedDocUntouched() error {
	cmd := exec.Command("git", "-C", repoRoot, "status", "--porcelain", "docs/spike-results.md")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git status: %v: %s", err, out)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("a bare spike run dirtied docs/spike-results.md: %s", out)
	}
	return nil
}

func (s *spikeState) noLeftovers() error {
	// The script's own trap tears everything down; assert nothing survived.
	if out, _ := exec.Command("pgrep", "-f", "sg-spike").Output(); len(strings.TrimSpace(string(out))) != 0 {
		return fmt.Errorf("leftover sg-spike process(es): %s", out)
	}
	if err := exec.Command("tmux", "-L", "sgmeasure", "list-sessions").Run(); err == nil {
		return fmt.Errorf("tmux -L sgmeasure server still running after teardown")
	}
	return nil
}
