package features

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cucumber/godog"
)

// jw is the justfile scenario world: a fresh temp dir per scenario for any
// on-disk fixture (a copied plugin tree, a fake SUPERGRAPH_BIN, an argv
// capture file), torn down after. No live supergraph server is involved —
// every scenario here drives `just`/`scripts/just-check.sh` directly against
// the filesystem.
type jw struct {
	tmpDir string

	// just --list output, captured per-listing
	listOut map[string]string

	// scripts/just-check.sh run
	checkStdout string
	checkStderr string
	checkExit   int
	checkRoot   string // the tree the check ran against (repo root or a temp copy)

	// fake-binary recipe run
	recipeStdout string
	recipeStderr string
	recipeExit   int

	// argv capture (fake binary echoes its own argv as JSON to this file)
	captureFile string
	captures    [][]string // one []string per recipe invocation, in call order
}

// requireJust skips the scenario cleanly (rather than failing it) when `just`
// is not on PATH — CI's `test` job does not install it, so the suite must
// stay green there.
func requireJust() error {
	if _, err := exec.LookPath("just"); err != nil {
		return godog.ErrSkip
	}
	return nil
}

func registerJustfileSteps(sc *godog.ScenarioContext) {
	j := &jw{}

	sc.BeforeScenario(func(*godog.Scenario) {
		tmp, err := os.MkdirTemp("", "sg-justfile-*")
		if err != nil {
			panic(err)
		}
		j.tmpDir = tmp
		j.listOut = map[string]string{}
		j.captures = nil
	})
	sc.AfterScenario(func(*godog.Scenario, error) {
		if j.tmpDir != "" {
			os.RemoveAll(j.tmpDir)
		}
		*j = jw{}
	})

	// AC-JUST-LIST
	sc.Step(lit("the repo's root justfile and its plugin modules"), requireJust)
	sc.Step(lit("`just --list` is run at the repo root, and `just --list <module>` for each plugin module"), j.runAllLists)
	sc.Step(lit("every recipe row in every listing has a non-empty doc comment after its `#`"), j.assertAllListedDocumented)

	// AC-JUST-OPS
	sc.Step(lit("the repo's shipped plugins and their mod.just files"), func() error { return nil })
	sc.Step(lit("`scripts/just-check.sh` is run against the repo root"), j.runCheckAgainstRepoRoot)
	sc.Step(lit("it exits 0"), j.assertCheckExit0)

	// AC-JUST-DRIFT
	sc.Step(lit("a temp copy of the repo's plugins and scripts/just-check.sh"), j.copyPluginsAndCheckScript)
	sc.Step(lit("one plugin's `.graphql` op file is deleted from the copy"), j.deleteOneOpFile)
	sc.Step(lit("`scripts/just-check.sh` is run against the copy"), j.runCheckAgainstCopy)
	sc.Step(lit("it exits non-zero"), j.assertCheckExitNonZero)
	sc.Step(lit("its output names the missing plugin and op"), j.assertCheckNamesMissing)

	// AC-JUST-EXIT
	sc.Step(lit("`SUPERGRAPH_BIN` pointed at a fake binary that prints an error to stderr and exits 1"), j.writeFailingFakeBin)
	sc.Step(lit("a query recipe is run against that binary"), j.runRecipeAgainstFailingBin)
	sc.Step(lit("the recipe exits non-zero"), j.assertRecipeExitNonZero)
	sc.Step(lit(`stdout is empty and does not contain "null"`), j.assertRecipeStdoutEmptyNoNull)

	// AC-JUST-CWD
	sc.Step(lit("`SUPERGRAPH_BIN` pointed at a fake binary that echoes its own argv as JSON"), j.writeArgvEchoFakeBin)
	sc.Step(lit("the current working directory is a temp dir outside the repo"), j.makeForeignCwd)
	sc.Step(lit("a tmux plugin recipe is run with `just --justfile` pointed at the repo's justfile"), j.runTmuxRecipeFromForeignCwd)
	sc.Step(lit("the captured argv's `--queries-dir` is an absolute path under the repo's `plugins/tmux/queries`"), j.assertCwdQueriesDirUnderTmux)
	sc.Step(lit("the file it names exists"), j.assertCwdOpFileExists)

	// AC-JUST-COLLIDE
	sc.Step(lit("`just github branch-ref` is run"), j.runGithubBranchRef)
	sc.Step(lit("`just tmux pane-for-branch` is run"), j.runTmuxPaneForBranch)
	sc.Step(lit("each recipe's captured `--queries-dir` resolves under its own plugin's `queries` directory"), j.assertEachOwnQueriesDir)
	sc.Step(lit("both plugins' `paneForBranch.graphql` files exist and are different files"), j.assertCollideFilesDifferButExist)
}

// ---------- helpers ----------

// runJust runs `just` with the given args and working directory, returning
// stdout, stderr, and the exit code. env, if non-nil, is appended to the
// current process environment (so SUPERGRAPH_BIN/ARGV_CAPTURE_FILE overrides
// stick without dropping PATH).
func runJust(dir string, env []string, args ...string) (stdout, stderr string, exit int) {
	cmd := exec.Command("just", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	stdout, stderr = so.String(), se.String()
	exit = exitCode(err)
	return
}

// ---------- AC-JUST-LIST ----------

func (j *jw) runAllLists() error {
	out, stderr, exit := runJust(repoRoot, nil, "--list")
	if exit != 0 {
		return fmt.Errorf("just --list exit=%d stderr=%s", exit, stderr)
	}
	j.listOut["<root>"] = out

	for _, mod := range []string{"github", "claude", "tmux"} {
		out, stderr, exit := runJust(repoRoot, nil, "--list", mod)
		if exit != 0 {
			return fmt.Errorf("just --list %s exit=%d stderr=%s", mod, exit, stderr)
		}
		j.listOut[mod] = out
	}
	return nil
}

// assertAllListedDocumented parses every `just --list` block gathered above
// and fails if any recipe row lacks a `#`-prefixed doc comment. Asserted as a
// property (no undocumented recipe), never as a hardcoded recipe count, so
// adding recipes later can't silently rot this check.
func (j *jw) assertAllListedDocumented() error {
	if len(j.listOut) == 0 {
		return fmt.Errorf("no `just --list` output captured")
	}
	for label, out := range j.listOut {
		lines := strings.Split(out, "\n")
		found := 0
		for _, line := range lines {
			trimmed := strings.TrimRight(line, " \t\r")
			if trimmed == "" || trimmed == "Available recipes:" {
				continue
			}
			if !strings.HasPrefix(line, "    ") {
				continue // not a recipe row
			}
			found++
			idx := strings.Index(trimmed, "#")
			if idx == -1 {
				return fmt.Errorf("%s: recipe row has no doc comment: %q", label, trimmed)
			}
			doc := strings.TrimSpace(trimmed[idx+1:])
			if doc == "" {
				return fmt.Errorf("%s: recipe row has an empty doc comment: %q", label, trimmed)
			}
		}
		if found == 0 {
			return fmt.Errorf("%s: no recipe rows parsed from:\n%s", label, out)
		}
	}
	return nil
}

// ---------- AC-JUST-OPS / AC-JUST-DRIFT ----------

func (j *jw) runCheckAgainstRepoRoot() error {
	j.checkRoot = repoRoot
	return j.runCheckScript(filepath.Join(repoRoot, "scripts", "just-check.sh"))
}

// copyPluginsAndCheckScript builds an isolated copy of just the pieces
// scripts/just-check.sh reads (scripts/just-check.sh itself plus every
// plugins/*/{mod.just,queries/**}) under a temp dir, so the negative case
// can delete a file without ever touching the real repo tree.
func (j *jw) copyPluginsAndCheckScript() error {
	dst := filepath.Join(j.tmpDir, "repo-copy")
	if err := os.MkdirAll(filepath.Join(dst, "scripts"), 0o755); err != nil {
		return err
	}
	if err := copyFile(
		filepath.Join(repoRoot, "scripts", "just-check.sh"),
		filepath.Join(dst, "scripts", "just-check.sh"),
		0o755,
	); err != nil {
		return err
	}

	pluginsSrc := filepath.Join(repoRoot, "plugins")
	entries, err := os.ReadDir(pluginsSrc)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := copyDir(filepath.Join(pluginsSrc, e.Name()), filepath.Join(dst, "plugins", e.Name())); err != nil {
			return err
		}
	}
	j.checkRoot = dst
	return nil
}

func (j *jw) deleteOneOpFile() error {
	target := filepath.Join(j.checkRoot, "plugins", "tmux", "queries", "paneForBranch.graphql")
	if _, err := os.Stat(target); err != nil {
		return fmt.Errorf("expected op file %s to exist in the copy before deleting it: %w", target, err)
	}
	return os.Remove(target)
}

func (j *jw) runCheckAgainstCopy() error {
	return j.runCheckScript(filepath.Join(j.checkRoot, "scripts", "just-check.sh"))
}

func (j *jw) runCheckScript(scriptPath string) error {
	cmd := exec.Command(scriptPath)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	j.checkStdout = so.String()
	j.checkStderr = se.String()
	j.checkExit = exitCode(err)
	return nil
}

func (j *jw) assertCheckExit0() error {
	if j.checkExit != 0 {
		return fmt.Errorf("just-check.sh exit=%d stdout=%s stderr=%s", j.checkExit, j.checkStdout, j.checkStderr)
	}
	return nil
}

func (j *jw) assertCheckExitNonZero() error {
	if j.checkExit == 0 {
		return fmt.Errorf("just-check.sh exit=0, expected non-zero; stdout=%s", j.checkStdout)
	}
	return nil
}

func (j *jw) assertCheckNamesMissing() error {
	if !strings.Contains(j.checkStderr, "MISSING") || !strings.Contains(j.checkStderr, "tmux/paneForBranch") {
		return fmt.Errorf("stderr does not name the missing tmux/paneForBranch op: %q", j.checkStderr)
	}
	return nil
}

// ---------- AC-JUST-EXIT ----------

func (j *jw) writeFailingFakeBin() error {
	if err := requireJust(); err != nil {
		return err
	}
	path := filepath.Join(j.tmpDir, "supergraph")
	script := "#!/usr/bin/env bash\necho \"fake supergraph: induced query failure\" >&2\nexit 1\n"
	return os.WriteFile(path, []byte(script), 0o755)
}

func (j *jw) runRecipeAgainstFailingBin() error {
	fakeBin := filepath.Join(j.tmpDir, "supergraph")
	out, stderr, exit := runJust(repoRoot, []string{"SUPERGRAPH_BIN=" + fakeBin}, "tmux", "panes")
	j.recipeStdout, j.recipeStderr, j.recipeExit = out, stderr, exit
	return nil
}

func (j *jw) assertRecipeExitNonZero() error {
	if j.recipeExit == 0 {
		return fmt.Errorf("recipe exit=0 against a failing binary; stdout=%q", j.recipeStdout)
	}
	return nil
}

func (j *jw) assertRecipeStdoutEmptyNoNull() error {
	trimmed := strings.TrimSpace(j.recipeStdout)
	if trimmed != "" {
		return fmt.Errorf("stdout not empty: %q", j.recipeStdout)
	}
	if strings.Contains(j.recipeStdout, "null") {
		return fmt.Errorf("stdout contains a masked-success null: %q", j.recipeStdout)
	}
	return nil
}

// ---------- AC-JUST-CWD / AC-JUST-COLLIDE (shared argv-echo fake binary) ----------

// writeArgvEchoFakeBin writes a fake `supergraph` that, when ARGV_CAPTURE_FILE
// is set, appends its own argv (as a JSON array, one line per invocation) to
// that file, then prints a harmless `{}` to stdout so the recipe's `| jq`
// stage sees valid JSON and the recipe itself still exits 0 — the point of
// this fake binary is capturing what argv `just` built, not exercising the
// real query path.
func (j *jw) writeArgvEchoFakeBin() error {
	if err := requireJust(); err != nil {
		return err
	}
	path := filepath.Join(j.tmpDir, "supergraph")
	script := `#!/usr/bin/env bash
if [ -n "${ARGV_CAPTURE_FILE:-}" ]; then
  {
    printf '['
    first=1
    for a in "$@"; do
      if [ "$first" -eq 1 ]; then first=0; else printf ','; fi
      esc=${a//\\/\\\\}
      esc=${esc//\"/\\\"}
      printf '"%s"' "$esc"
    done
    printf ']\n'
  } >> "$ARGV_CAPTURE_FILE"
fi
echo '{}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		return err
	}
	j.captureFile = filepath.Join(j.tmpDir, "capture.jsonl")
	return nil
}

func (j *jw) makeForeignCwd() error {
	dir := filepath.Join(j.tmpDir, "foreign-cwd")
	return os.MkdirAll(dir, 0o755)
}

func (j *jw) recipeEnv() []string {
	fakeBin := filepath.Join(j.tmpDir, "supergraph")
	return []string{"SUPERGRAPH_BIN=" + fakeBin, "ARGV_CAPTURE_FILE=" + j.captureFile}
}

// runInDir runs `just` with the argv-capture env from a given working
// directory, appending the resulting captured argv line (if any) to
// j.captures.
func (j *jw) runInDir(dir string, args ...string) error {
	_, stderr, exit := runJust(dir, j.recipeEnv(), args...)
	if exit != 0 {
		return fmt.Errorf("just %v exit=%d stderr=%s", args, exit, stderr)
	}
	return j.loadLatestCapture()
}

func (j *jw) loadLatestCapture() error {
	data, err := os.ReadFile(j.captureFile)
	if err != nil {
		return fmt.Errorf("read capture file: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 || lines[len(lines)-1] == "" {
		return fmt.Errorf("no argv captured in %s", j.captureFile)
	}
	var argv []string
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &argv); err != nil {
		return fmt.Errorf("parse captured argv %q: %w", lines[len(lines)-1], err)
	}
	j.captures = append(j.captures, argv)
	return nil
}

// queriesDirFromArgv finds the value that follows a literal "--queries-dir"
// token in a captured argv slice.
func queriesDirFromArgv(argv []string) (string, error) {
	for i, a := range argv {
		if a == "--queries-dir" && i+1 < len(argv) {
			return argv[i+1], nil
		}
	}
	return "", fmt.Errorf("no --queries-dir in argv: %v", argv)
}

func (j *jw) runTmuxRecipeFromForeignCwd() error {
	dir := filepath.Join(j.tmpDir, "foreign-cwd")
	justfilePath := filepath.Join(repoRoot, "justfile")
	return j.runInDir(dir, "--justfile", justfilePath, "tmux", "panes")
}

func (j *jw) assertCwdQueriesDirUnderTmux() error {
	if len(j.captures) == 0 {
		return fmt.Errorf("no captured argv")
	}
	dir, err := queriesDirFromArgv(j.captures[len(j.captures)-1])
	if err != nil {
		return err
	}
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("--queries-dir is not absolute: %q", dir)
	}
	want := filepath.Join(repoRoot, "plugins", "tmux", "queries")
	if dir != want {
		return fmt.Errorf("--queries-dir=%q, want %q", dir, want)
	}
	j.captures[len(j.captures)-1] = append(j.captures[len(j.captures)-1], "__resolved_dir__="+dir)
	return nil
}

func (j *jw) assertCwdOpFileExists() error {
	if len(j.captures) == 0 {
		return fmt.Errorf("no captured argv")
	}
	dir, err := queriesDirFromArgv(j.captures[len(j.captures)-1])
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "panes.graphql")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("op file does not exist: %s: %w", path, err)
	}
	return nil
}

// ---------- AC-JUST-COLLIDE ----------

func (j *jw) runGithubBranchRef() error {
	return j.runInDir(repoRoot, "github", "branch-ref", "foo/bar", "main")
}

func (j *jw) runTmuxPaneForBranch() error {
	return j.runInDir(repoRoot, "tmux", "pane-for-branch", "main")
}

func (j *jw) assertEachOwnQueriesDir() error {
	if len(j.captures) != 2 {
		return fmt.Errorf("want 2 captured recipe invocations, got %d", len(j.captures))
	}
	githubDir, err := queriesDirFromArgv(j.captures[0])
	if err != nil {
		return fmt.Errorf("github capture: %w", err)
	}
	tmuxDir, err := queriesDirFromArgv(j.captures[1])
	if err != nil {
		return fmt.Errorf("tmux capture: %w", err)
	}
	wantGithub := filepath.Join(repoRoot, "plugins", "github", "queries")
	wantTmux := filepath.Join(repoRoot, "plugins", "tmux", "queries")
	if githubDir != wantGithub {
		return fmt.Errorf("github --queries-dir=%q, want %q", githubDir, wantGithub)
	}
	if tmuxDir != wantTmux {
		return fmt.Errorf("tmux --queries-dir=%q, want %q", tmuxDir, wantTmux)
	}
	return nil
}

func (j *jw) assertCollideFilesDifferButExist() error {
	githubFile := filepath.Join(repoRoot, "plugins", "github", "queries", "paneForBranch.graphql")
	tmuxFile := filepath.Join(repoRoot, "plugins", "tmux", "queries", "paneForBranch.graphql")
	gb, err := os.ReadFile(githubFile)
	if err != nil {
		return fmt.Errorf("github paneForBranch.graphql missing: %w", err)
	}
	tb, err := os.ReadFile(tmuxFile)
	if err != nil {
		return fmt.Errorf("tmux paneForBranch.graphql missing: %w", err)
	}
	if githubFile == tmuxFile {
		return fmt.Errorf("both plugins resolved to the same file path: %s", githubFile)
	}
	if bytes.Equal(gb, tb) {
		return fmt.Errorf("github and tmux paneForBranch.graphql are byte-identical (expected different query bodies)")
	}
	return nil
}

// ---------- generic file/dir copy helpers (AC-JUST-DRIFT fixture) ----------

func copyFile(src, dst string, mode os.FileMode) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, data, mode)
}

func copyDir(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, info.Mode())
	})
}
