package features

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/cucumber/godog"

	"github.com/drewdrewthis/supergraph/plugins/github/fakegh"
)

// registerGitSteps wires every Given/When/Then phrase in features/git.feature.
// Steps are black-box: real `git` (init/commit/worktree/remote) against temp repos
// created per scenario, a real `supergraph serve` subprocess reading a generated
// config.toml, and GraphQL queries via `supergraph query` (the same pattern
// steps_tmux_test.go / tmux_helpers_test.go already use for the tmux plugin) — the
// plugins/git package is never imported, so these scenarios exercise the plugin
// only through the surfaces its real consumer uses. @tmux scenarios additionally
// spin up a real tmux server on a private `-L` socket, reusing killTmuxServer from
// tmux_helpers_test.go for verified teardown.
func registerGitSteps(sc *godog.ScenarioContext) {
	g := &gitWorld{}

	sc.Before(func(ctx context.Context, s *godog.Scenario) (context.Context, error) {
		if !hasTag(s, "@git") {
			return ctx, nil
		}
		return ctx, g.init()
	})
	sc.After(func(ctx context.Context, s *godog.Scenario, _ error) (context.Context, error) {
		if hasTag(s, "@git") {
			g.cleanup()
		}
		return ctx, nil
	})

	// ---------- AC-GIT-WORKTREES ----------
	sc.Step(lit("two configured git repo roots, each with a second worktree checked out on its own branch"), g.seedTwoRootsWithSecondWorktrees)
	sc.Step(lit("one of the roots also has a worktree checked out with a detached HEAD"), g.addDetachedWorktreeToFirstRoot)
	sc.Step(lit("`repos` is queried for both hosts' repos"), g.startAndQueryAll)
	sc.Step(lit("each Repo's worktrees match that root's own `git worktree list --porcelain` output exactly"), g.assertWorktreesMatchPorcelain)
	sc.Step(lit("no worktree from one repo appears under the other repo"), g.assertNoCrossRepoWorktrees)
	sc.Step(lit("the detached-HEAD worktree reports a null branch"), g.assertDetachedBranchNull)

	// ---------- AC-GIT-AHEAD-BEHIND ----------
	sc.Step(regexp.MustCompile(`^a repo root with a worktree on a branch with an upstream (\d+) commits ahead, (\d+) behind$`), g.seedAheadBehind)
	sc.Step(lit("a repo root with a worktree on a branch level with its upstream"), g.seedLevelWithUpstream)
	sc.Step(lit("a repo root with a worktree on a branch with no upstream configured"), g.seedNoUpstream)
	sc.Step(lit("`repos` is queried for that repo"), g.startAndQuerySingle)
	sc.Step(regexp.MustCompile(`^the worktree's ahead/behind is ahead (\d+|null) and behind (\d+|null)$`), g.assertAheadBehind)

	// ---------- AC-GIT-SLUG ----------
	sc.Step(lit("a repo root with an `origin` remote `git@github.com:acme/widget.git`"), g.seedRepoWithOriginRemote)
	sc.Step(lit("a repo root with no `origin` remote"), g.seedRepoWithNoOriginRemote)
	sc.Step(lit(`the repo's slug is "acme/widget"`), g.assertSlugEquals("acme/widget"))
	sc.Step(lit("the repo's slug is its directory basename"), g.assertSlugIsDirBasename)

	// ---------- AC-GIT-TMUX-JOIN ----------
	sc.Step(lit(`a repo root with two worktrees, "attended" and "unattended"`), g.seedAttendedUnattendedWorktrees)
	sc.Step(lit(`a tmux server on a private socket with a session whose current path is the "attended" worktree`), g.seedTmuxSessionOnAttended)
	sc.Step(lit("a supergraph server watches both that repo and that tmux socket"), g.startWithGitAndTmux)
	sc.Step(lit(`the "attended" worktree's tmuxSession is not null`), g.assertAttendedTmuxSessionNotNull)
	sc.Step(lit(`the "unattended" worktree's tmuxSession is null`), g.assertUnattendedTmuxSessionNull)

	// ---------- AC-TMUX-GIT-JOIN-FIELDS ----------
	sc.Step(lit("a repo root with a worktree whose path hosts a tmux session with two windows of panes"), g.seedWorktreeWithTmuxWindows)
	sc.Step(lit("a real terminal client attaches to that session"), g.attachRealClientToJoinSession)
	sc.Step(lit("`repos` is queried for that repo and `tmuxSessions` is queried directly"), g.queryJoinAndTopLevelUntilAttached)
	sc.Step(lit("the worktree's tmuxSession windows and panes match the real tmux server's structure"), g.assertJoinWindowsMatchReal)
	sc.Step(lit("the worktree's tmuxSession reads attached"), g.assertJoinAttached)
	sc.Step(lit("the worktree's tmuxSession's createdAt is non-null and non-zero"), g.assertJoinCreatedAtNonZero)
	sc.Step(lit("the worktree's tmuxSession equals the session returned by the top-level tmuxSessions query"), g.assertJoinEqualsTopLevel)

	// ---------- AC-GIT-ISSUE-JOIN / AC-GIT-PR-JOIN ----------
	sc.Step(regexp.MustCompile("^a repo root whose origin is `([^`]+)`, with a worktree on branch \"([^\"]*)\"$"), g.seedJoinWorktree)
	sc.Step(regexp.MustCompile("^GitHub issue #(\\d+) in `([^`]+)` is warm-cached with title \"([^\"]*)\"$"), g.seedWarmIssue)
	sc.Step(regexp.MustCompile("^the worktree's issue is GitHub issue #(\\d+) with title \"([^\"]*)\"$"), g.assertIssueJoin)
	sc.Step(lit("the worktree's issue is null"), g.assertIssueNull)
	sc.Step(regexp.MustCompile("^GitHub PR #(\\d+) in `([^`]+)` is warm-cached with headRefName \"([^\"]*)\"$"),
		func(n, slug, head string) error { return g.seedWarmPR(n, slug, head, "") })
	sc.Step(regexp.MustCompile("^GitHub PR #(\\d+) in `([^`]+)` is warm-cached with headRefName \"([^\"]*)\" and state \"([^\"]*)\"$"), g.seedWarmPR)
	sc.Step(regexp.MustCompile(`^the worktree's pullRequest is GitHub PR #(\d+)$`),
		func(n string) error { return g.assertPRJoin(n) })
	sc.Step(regexp.MustCompile("^the worktree's pullRequest is GitHub PR #(\\d+) with state \"([^\"]*)\"$"), g.assertPRJoinState)

	// ---------- AC-GIT-RECONCILE ----------
	sc.Step(lit("a supergraph server watching a repo root with a second worktree"), g.startWithSecondWorktree)
	sc.Step(lit("that worktree is removed with `git worktree remove`"), g.removeSecondWorktree)
	sc.Step(lit("the next reconcile runs"), g.awaitReconcileMarksStale)
	sc.Step(lit("`repos` still returns that worktree's path"), g.assertRemovedWorktreeStillPresent)
	sc.Step(lit("its staleSince is not null"), g.assertRemovedWorktreeStale)

	// ---------- AC-GIT-SUBSCRIBE ----------
	sc.Step(lit("a supergraph server watching a repo root"), g.startBareRepoRoot)
	sc.Step(lit("I run `supergraph subscribe worktreeUpdated --once` against it in the background"), g.subscribeWorktreeUpdatedBG)
	sc.Step(lit("a second worktree is added to that root"), g.addSecondWorktreeToRoot)
	sc.Step(lit("the worktree subscribe process exits 0 within 5s"), func() error { return g.sw().waitSubscribeExit0() })
	sc.Step(lit("its stdout is exactly one compact JSON line naming type `git.worktree.updated`"), func() error {
		return g.sw().assertSubscribeOneLineOfType("git.worktree.updated")
	})

	// ---------- AC-GIT-HOSTID ----------
	sc.Step(lit(`a supergraph server watching a repo root with hostId "test"`), g.startRepoRootHostTest)
	sc.Step(lit("`repos(hostId: \"test\")` is queried"), g.queryReposHostTest)
	sc.Step(lit("at least one repo row is returned"), g.assertAtLeastOneRepoRow)
	sc.Step(lit("`repos(hostId: \"nope\")` is queried"), g.queryReposHostNope)
	sc.Step(lit("zero repo rows are returned"), g.assertZeroRepoRows)

	// ---------- AC-GIT-DORMANT ----------
	sc.Step(lit("a supergraph server started with no `[plugins.git]` section configured"), g.startDormant)
	sc.Step(lit("`repos` is queried"), g.queryReposNoFilter)
}

// ==================== world ====================

// gitWorld is the git plugin scenarios' mutable per-scenario state. It wraps a
// fresh *world (reused for dataDir/cfgPath/listen/startServe/stopServe/runCLI, the
// same pattern ghWorld and tmuxWorld already use) plus real temp git repos and the
// config knobs a git.feature Given/When step needs.
type gitWorld struct {
	sw_       *world
	roots     []string // configured [plugins.git] repos
	dirs      []string // every temp dir created this scenario, for cleanup
	hostID    string
	reconcile int
	dormant   bool
	started   bool

	tmuxSocket string

	// AC-GIT-WORKTREES
	rootA, rootB     string
	worktreeA2       string
	worktreeDetached string
	lastRepos        []repoJSON

	// AC-GIT-AHEAD-BEHIND / AC-GIT-SLUG (single repo root scenarios)
	singleRoot string

	// AC-GIT-TMUX-JOIN
	attendedPath   string
	unattendedPath string

	// AC-GIT-RECONCILE
	removedPath string

	// AC-GIT-HOSTID
	hostQueryResult []repoJSON

	// AC-GIT-ISSUE-JOIN / AC-GIT-PR-JOIN
	githubEnabled bool
	ghFake        *fakegh.Server

	// AC-TMUX-GIT-JOIN-FIELDS
	joinWorktreePath string
	joinSessionName  string
	attachCmd        *exec.Cmd
	tmuxJoinWorktree *joinWorktreeDetail
	joinTopLevel     *joinTmuxDetail
}

func (g *gitWorld) init() error {
	*g = gitWorld{}
	g.sw_ = &world{}
	if err := g.sw_.init(); err != nil {
		return err
	}
	g.hostID = "test"
	g.reconcile = 1
	return nil
}

// sw exposes the embedded *world for steps registered directly against world
// methods (waitSubscribeExit0, assertSubscribeOneLineOfType).
func (g *gitWorld) sw() *world { return g.sw_ }

func (g *gitWorld) cleanup() {
	// Reap the AC-TMUX-GIT-JOIN-FIELDS real attach client first, mirroring
	// tmuxWorld.cleanup: killTmuxServer's pgrep sweep would eventually catch an
	// orphaned attach process too, but killing it explicitly here avoids leaving a
	// `script`-wrapped process hanging on a pty this scenario is done with.
	if g.attachCmd != nil && g.attachCmd.Process != nil {
		_ = g.attachCmd.Process.Kill()
	}
	if g.sw_ != nil {
		g.sw_.stopSubscribeBG()
		g.sw_.cleanup()
	}
	if g.tmuxSocket != "" {
		killTmuxServer(g.tmuxSocket)
	}
	if g.ghFake != nil {
		g.ghFake.Close()
	}
	for _, d := range g.dirs {
		_ = os.RemoveAll(d)
	}
}

// ==================== config + query plumbing ====================

func (g *gitWorld) writeConfigAndStart() error {
	var b strings.Builder
	fmt.Fprintf(&b, "hostId = %q\n", g.hostID)
	fmt.Fprintf(&b, "listen = %q\n", g.sw_.listen)
	fmt.Fprintf(&b, "dataDir = %q\n", g.sw_.dataDir)
	b.WriteString("lagThresholdSeconds = 30.000000\n")
	b.WriteString("[plugins.template]\n")
	b.WriteString("intervalSeconds = 1\n")
	if !g.dormant {
		b.WriteString("[plugins.git]\n")
		b.WriteString("repos = [")
		for i, r := range g.roots {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "%q", r)
		}
		b.WriteString("]\n")
		fmt.Fprintf(&b, "reconcileIntervalSeconds = %d\n", g.reconcile)
	}
	if g.tmuxSocket != "" {
		b.WriteString("[plugins.tmux]\n")
		fmt.Fprintf(&b, "socket = %q\n", g.tmuxSocket)
		b.WriteString("eventSource = \"poll\"\n")
		b.WriteString("reconcileIntervalSeconds = 1\n")
	}
	// AC-GIT-ISSUE-JOIN / AC-GIT-PR-JOIN: the join scenarios point the github
	// plugin at a per-scenario fakegh server (same shape as ghWorld.writeConfig in
	// features/github_helpers_test.go, minimal-copied here since gitWorld renders
	// its own single config.toml alongside [plugins.git]/[plugins.tmux]).
	if g.githubEnabled && g.ghFake != nil {
		b.WriteString("[plugins.github]\n")
		fmt.Fprintf(&b, "token = %q\n", "test-token")
		fmt.Fprintf(&b, "baseURL = %q\n", g.ghFake.URL)
		fmt.Fprintf(&b, "graphqlURL = %q\n", g.ghFake.URL+"/graphql")
		b.WriteString("ingress = \"tunnel\"\n")
		b.WriteString("webhookSecret = \"test-webhook-secret\"\n")
		fmt.Fprintf(&b, "selfURL = %q\n", "http://"+g.sw_.listen)
		b.WriteString("reconcileIntervalSeconds = 3600\n")
		b.WriteString("notifications = false\n")
		b.WriteString("[plugins.github.pin]\n")
		b.WriteString("commits = true\nreleases = true\nmergedPRsAfterDays = 7\nclosedIssuesAfterDays = 30\n")
		b.WriteString("[plugins.github.ttl]\n")
		b.WriteString("issue = 3600\npr = 3600\ncheckRun = 3600\n")
	}
	if err := os.WriteFile(g.sw_.cfgPath, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return g.sw_.startServe()
}

func (g *gitWorld) ensureStarted() error {
	if g.started {
		return nil
	}
	g.started = true
	if err := g.writeConfigAndStart(); err != nil {
		return err
	}
	return g.waitForFirstReconcile()
}

// waitForFirstReconcile blocks until the plugin's first reconcile has scanned
// every configured root, so a step immediately following ensureStarted never
// races the reconcile loop's async initial scan (mirrors tmuxWorld.waitPanes,
// the same "first reconcile after startServe returns" race in the tmux
// harness). Without this, `repos`/`worktrees` queried right after startup can
// observe a root before its worktrees have been enumerated even once.
func (g *gitWorld) waitForFirstReconcile() error {
	if len(g.roots) == 0 {
		return nil
	}
	want := make(map[string]int, len(g.roots))
	for _, root := range g.roots {
		real, err := realWorktreeList(root)
		if err != nil {
			return err
		}
		want[root] = len(real)
	}
	return eventually(g.within(), func() error {
		repos, err := g.queryRepos("")
		if err != nil {
			return err
		}
		for root, n := range want {
			var repo *repoJSON
			for i := range repos {
				if repos[i].Root == root {
					repo = &repos[i]
					break
				}
			}
			if repo == nil {
				return fmt.Errorf("no Repo row for root %q yet", root)
			}
			if len(repo.Worktrees) != n {
				return fmt.Errorf("root %q: got %d worktrees so far, want %d", root, len(repo.Worktrees), n)
			}
		}
		return nil
	})
}

type worktreeJSON struct {
	HostID     string  `json:"hostId"`
	RepoSlug   string  `json:"repoSlug"`
	Path       string  `json:"path"`
	Branch     *string `json:"branch"`
	Head       string  `json:"head"`
	Detached   bool    `json:"detached"`
	Ahead      *int    `json:"ahead"`
	Behind     *int    `json:"behind"`
	StaleSince *string `json:"staleSince"`
	TmuxSess   *struct {
		Typename string `json:"__typename"`
	} `json:"tmuxSession"`
	Issue       *joinIssueJSON `json:"issue"`
	PullRequest *joinPRJSON    `json:"pullRequest"`
}

// joinIssueJSON / joinPRJSON are the AC-GIT-ISSUE-JOIN / AC-GIT-PR-JOIN fields'
// wire shape, selected minimally by repoFields below.
type joinIssueJSON struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

type joinPRJSON struct {
	Number      int    `json:"number"`
	HeadRefName string `json:"headRefName"`
	State       string `json:"state"`
}

type repoJSON struct {
	HostID     string         `json:"hostId"`
	Slug       string         `json:"slug"`
	Root       string         `json:"root"`
	StaleSince *string        `json:"staleSince"`
	Worktrees  []worktreeJSON `json:"worktrees"`
}

const repoFields = `hostId slug root staleSince worktrees { hostId repoSlug path branch head detached ahead behind staleSince tmuxSession { __typename } issue { number title } pullRequest { number headRefName state } }`

func (g *gitWorld) queryRepos(hostFilter string) ([]repoJSON, error) {
	var gql string
	if hostFilter != "" {
		gql = fmt.Sprintf(`{ repos(hostId: %q) { %s } }`, hostFilter, repoFields)
	} else {
		gql = fmt.Sprintf(`{ repos { %s } }`, repoFields)
	}
	g.sw_.runCLI("query", gql)
	if g.sw_.lastExit != 0 {
		return nil, fmt.Errorf("query exit %d: %s", g.sw_.lastExit, g.sw_.lastStderr)
	}
	var d struct {
		Repos []repoJSON `json:"repos"`
	}
	if err := json.Unmarshal([]byte(g.sw_.lastStdout), &d); err != nil {
		return nil, fmt.Errorf("parse %q: %w", g.sw_.lastStdout, err)
	}
	return d.Repos, nil
}

func (g *gitWorld) within() time.Duration {
	d := time.Duration(g.reconcile)*3*time.Second + 3*time.Second
	if d < 10*time.Second {
		d = 10 * time.Second
	}
	return d
}

// ==================== git repo helpers ====================

func (g *gitWorld) newDir(prefix string) (string, error) {
	dir, err := os.MkdirTemp("", "sg-git-"+prefix+"-*")
	if err != nil {
		return "", err
	}
	// macOS $TMPDIR (/var/folders/...) and /tmp are symlinks into /private/...
	// `git worktree list --porcelain`, and the plugin's own root canonicalisation
	// via filepath.EvalSymlinks, always report the fully-resolved path. Resolve
	// here, at creation time, so every stored root/worktree path and every
	// expected value the harness compares against already agree with what the
	// server reports — no per-callsite resolution, no loosened assertions.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	g.dirs = append(g.dirs, resolved)
	return resolved, nil
}

func runGit(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %v (dir=%s): %w: %s", args, dir, err, out)
	}
	return string(out), nil
}

// initRepoWithCommit git-inits dir and creates one commit on branch "main" so
// there is always a real HEAD to worktree-add or detach from.
func initRepoWithCommit(dir string) error {
	// Renames to "main" via `branch -M` after the commit (matching
	// tmpGitRepoOnBranch in tmux_helpers_test.go) rather than `git init -b main`,
	// so this works regardless of the runner's git version / init.defaultBranch.
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", "init"},
		{"branch", "-M", "main"},
	} {
		if _, err := runGit(dir, args...); err != nil {
			return err
		}
	}
	return nil
}

func gitCommitEmpty(dir, msg string) error {
	_, err := runGit(dir, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "--allow-empty", "-q", "-m", msg)
	return err
}

func gitWorktreeAddBranch(root, path, branch string) error {
	_, err := runGit(root, "worktree", "add", "-q", "-b", branch, path)
	return err
}

func gitWorktreeAddDetached(root, path string) error {
	_, err := runGit(root, "worktree", "add", "-q", "--detach", path)
	return err
}

func gitWorktreeRemove(root, path string) error {
	_, err := runGit(root, "worktree", "remove", "--force", path)
	return err
}

// worktreePorcelainEntry mirrors one `git worktree list --porcelain` block.
type worktreePorcelainEntry struct {
	Path     string
	Branch   *string // nil when detached
	Detached bool
}

func parseWorktreePorcelain(out string) []worktreePorcelainEntry {
	var entries []worktreePorcelainEntry
	var cur *worktreePorcelainEntry
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "worktree "):
			if cur != nil {
				entries = append(entries, *cur)
			}
			cur = &worktreePorcelainEntry{Path: strings.TrimPrefix(line, "worktree ")}
		case strings.HasPrefix(line, "branch "):
			if cur != nil {
				b := strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
				cur.Branch = &b
			}
		case line == "detached":
			if cur != nil {
				cur.Detached = true
			}
		}
	}
	if cur != nil {
		entries = append(entries, *cur)
	}
	return entries
}

func realWorktreeList(root string) ([]worktreePorcelainEntry, error) {
	out, err := runGit(root, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktreePorcelain(out), nil
}

func findWorktreeByPath(repos []repoJSON, path string) *worktreeJSON {
	for i := range repos {
		for j := range repos[i].Worktrees {
			if repos[i].Worktrees[j].Path == path {
				return &repos[i].Worktrees[j]
			}
		}
	}
	return nil
}

// ==================== AC-GIT-WORKTREES ====================

func (g *gitWorld) seedTwoRootsWithSecondWorktrees() error {
	rootA, err := g.newDir("rootA")
	if err != nil {
		return err
	}
	rootB, err := g.newDir("rootB")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(rootA); err != nil {
		return err
	}
	if err := initRepoWithCommit(rootB); err != nil {
		return err
	}
	wtA2, err := g.newDir("wtA2")
	if err != nil {
		return err
	}
	wtB2, err := g.newDir("wtB2")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(wtA2) // worktree add requires the target not already exist
	_ = os.RemoveAll(wtB2)
	if err := gitWorktreeAddBranch(rootA, wtA2, "featA"); err != nil {
		return err
	}
	if err := gitWorktreeAddBranch(rootB, wtB2, "featB"); err != nil {
		return err
	}
	g.rootA, g.rootB = rootA, rootB
	g.worktreeA2 = wtA2
	g.roots = []string{rootA, rootB}
	return nil
}

func (g *gitWorld) addDetachedWorktreeToFirstRoot() error {
	wt, err := g.newDir("wtA3detached")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(wt)
	if err := gitWorktreeAddDetached(g.rootA, wt); err != nil {
		return err
	}
	g.worktreeDetached = wt
	return nil
}

func (g *gitWorld) startAndQueryAll() error {
	if err := g.ensureStarted(); err != nil {
		return err
	}
	repos, err := g.queryRepos("")
	if err != nil {
		return err
	}
	g.lastRepos = repos
	return nil
}

func (g *gitWorld) assertWorktreesMatchPorcelain() error {
	for _, root := range []string{g.rootA, g.rootB} {
		real, err := realWorktreeList(root)
		if err != nil {
			return err
		}
		var repo *repoJSON
		for i := range g.lastRepos {
			if g.lastRepos[i].Root == root {
				repo = &g.lastRepos[i]
				break
			}
		}
		if repo == nil {
			return fmt.Errorf("no Repo row for root %q", root)
		}
		if len(repo.Worktrees) != len(real) {
			return fmt.Errorf("root %q: got %d worktrees, git reports %d", root, len(repo.Worktrees), len(real))
		}
		for _, want := range real {
			got := findWorktreeByPath(g.lastRepos, want.Path)
			if got == nil {
				return fmt.Errorf("root %q: worktree %q missing from GraphQL result", root, want.Path)
			}
			if want.Branch == nil {
				if got.Branch != nil {
					return fmt.Errorf("worktree %q: git reports detached, GraphQL reports branch %q", want.Path, *got.Branch)
				}
			} else if got.Branch == nil || *got.Branch != *want.Branch {
				return fmt.Errorf("worktree %q: git branch %q, GraphQL branch %v", want.Path, *want.Branch, got.Branch)
			}
		}
	}
	return nil
}

func (g *gitWorld) assertNoCrossRepoWorktrees() error {
	for i := range g.lastRepos {
		for _, wt := range g.lastRepos[i].Worktrees {
			if wt.RepoSlug != g.lastRepos[i].Slug {
				return fmt.Errorf("worktree %q has repoSlug %q, expected parent repo's slug %q", wt.Path, wt.RepoSlug, g.lastRepos[i].Slug)
			}
		}
	}
	// stronger cross-check: rootA's worktree set must not include any path under rootB and vice versa.
	realA, err := realWorktreeList(g.rootA)
	if err != nil {
		return err
	}
	realB, err := realWorktreeList(g.rootB)
	if err != nil {
		return err
	}
	bPaths := map[string]bool{}
	for _, w := range realB {
		bPaths[w.Path] = true
	}
	for _, w := range realA {
		if bPaths[w.Path] {
			return fmt.Errorf("path %q claimed by both repo roots (test setup bug)", w.Path)
		}
	}
	for _, repo := range g.lastRepos {
		if repo.Root == g.rootA {
			for _, wt := range repo.Worktrees {
				found := false
				for _, w := range realA {
					if w.Path == wt.Path {
						found = true
					}
				}
				if !found {
					return fmt.Errorf("rootA's Repo row includes worktree %q, which belongs to rootB", wt.Path)
				}
			}
		}
	}
	return nil
}

func (g *gitWorld) assertDetachedBranchNull() error {
	wt := findWorktreeByPath(g.lastRepos, g.worktreeDetached)
	if wt == nil {
		return fmt.Errorf("detached worktree %q not found in GraphQL result", g.worktreeDetached)
	}
	if !wt.Detached {
		return fmt.Errorf("worktree %q: detached=false, want true", g.worktreeDetached)
	}
	if wt.Branch != nil {
		return fmt.Errorf("worktree %q: branch = %q, want null (detached HEAD)", g.worktreeDetached, *wt.Branch)
	}
	return nil
}

// ==================== AC-GIT-AHEAD-BEHIND ====================

// setupUpstreamPair creates a bare origin plus this scenario's work clone, with
// one initial commit already pushed (so "main" tracks "origin/main"). Callers
// then diverge ahead/behind as needed before querying.
func (g *gitWorld) setupUpstreamPair() (origin, work string, err error) {
	origin, err = g.newDir("origin")
	if err != nil {
		return "", "", err
	}
	if _, err = runGit(origin, "init", "-q", "--bare"); err != nil {
		return "", "", err
	}
	// A bare repo's HEAD is just a symref, and `init --bare` points it at
	// whatever init.defaultBranch the runner has configured (main locally,
	// master on CI). Pin it to "main" here, once, so every clone of this
	// origin — the initial "work" clone below and any later clone such as
	// "otherclone" in seedAheadBehind — resolves the same default branch
	// regardless of the runner's git config. Each clone still checks out
	// "main" explicitly rather than trusting clone-time HEAD resolution,
	// since a later push to origin (e.g. creating refs/heads/main for the
	// first time) does not itself move origin's HEAD.
	if _, err = runGit(origin, "symbolic-ref", "HEAD", "refs/heads/main"); err != nil {
		return "", "", err
	}
	work, err = g.newDir("work")
	if err != nil {
		return "", "", err
	}
	_ = os.RemoveAll(work)
	if _, err = runGit(g.dirs[0], "clone", "-q", origin, work); err != nil {
		return "", "", err
	}
	if _, err = runGit(work, "checkout", "-q", "-B", "main"); err != nil {
		return "", "", err
	}
	if err = gitCommitEmpty(work, "init"); err != nil {
		return "", "", err
	}
	if _, err = runGit(work, "push", "-q", "-u", "origin", "main"); err != nil {
		return "", "", err
	}
	return origin, work, nil
}

func (g *gitWorld) seedAheadBehind(aheadStr, behindStr string) error {
	origin, work, err := g.setupUpstreamPair()
	if err != nil {
		return err
	}
	var ahead, behind int
	fmt.Sscanf(aheadStr, "%d", &ahead)
	fmt.Sscanf(behindStr, "%d", &behind)

	// behind: push extra commits from a SECOND clone so work's remote-tracking ref
	// is stale until it fetches.
	if behind > 0 {
		other, err := g.newDir("otherclone")
		if err != nil {
			return err
		}
		_ = os.RemoveAll(other)
		if _, err := runGit(g.dirs[0], "clone", "-q", origin, other); err != nil {
			return err
		}
		// Pin the branch explicitly rather than trusting clone-time HEAD
		// resolution — see the comment in setupUpstreamPair.
		if _, err := runGit(other, "checkout", "-q", "-B", "main", "origin/main"); err != nil {
			return err
		}
		for i := 0; i < behind; i++ {
			if err := gitCommitEmpty(other, fmt.Sprintf("upstream %d", i)); err != nil {
				return err
			}
		}
		if _, err := runGit(other, "push", "-q", "origin", "main"); err != nil {
			return err
		}
	}
	// ahead: commit locally in work without pushing.
	for i := 0; i < ahead; i++ {
		if err := gitCommitEmpty(work, fmt.Sprintf("local %d", i)); err != nil {
			return err
		}
	}
	// fetch (not merge) so work's remote-tracking ref reflects the pushed-behind
	// commits, matching what `git status -sb` compares against.
	if _, err := runGit(work, "fetch", "-q", "origin"); err != nil {
		return err
	}
	g.singleRoot = work
	g.roots = []string{work}
	return nil
}

func (g *gitWorld) seedLevelWithUpstream() error {
	_, work, err := g.setupUpstreamPair()
	if err != nil {
		return err
	}
	g.singleRoot = work
	g.roots = []string{work}
	return nil
}

func (g *gitWorld) seedNoUpstream() error {
	dir, err := g.newDir("noupstream")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(dir); err != nil {
		return err
	}
	if _, err := runGit(dir, "checkout", "-q", "-b", "topic"); err != nil {
		return err
	}
	g.singleRoot = dir
	g.roots = []string{dir}
	return nil
}

func (g *gitWorld) startAndQuerySingle() error {
	if err := g.ensureStarted(); err != nil {
		return err
	}
	repos, err := g.queryRepos("")
	if err != nil {
		return err
	}
	g.lastRepos = repos
	return nil
}

func (g *gitWorld) assertAheadBehind(aheadStr, behindStr string) error {
	wt := findWorktreeByPath(g.lastRepos, g.singleRoot)
	if wt == nil {
		return fmt.Errorf("no worktree found for root %q", g.singleRoot)
	}
	if err := assertIntOrNull(aheadStr, wt.Ahead, "ahead"); err != nil {
		return err
	}
	return assertIntOrNull(behindStr, wt.Behind, "behind")
}

func assertIntOrNull(want string, got *int, label string) error {
	if want == "null" {
		if got != nil {
			return fmt.Errorf("%s = %d, want null", label, *got)
		}
		return nil
	}
	var w int
	fmt.Sscanf(want, "%d", &w)
	if got == nil {
		return fmt.Errorf("%s = null, want %d", label, w)
	}
	if *got != w {
		return fmt.Errorf("%s = %d, want %d", label, *got, w)
	}
	return nil
}

// ==================== AC-GIT-SLUG ====================

func (g *gitWorld) seedRepoWithOriginRemote() error {
	dir, err := g.newDir("originslug")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(dir); err != nil {
		return err
	}
	if _, err := runGit(dir, "remote", "add", "origin", "git@github.com:acme/widget.git"); err != nil {
		return err
	}
	g.singleRoot = dir
	g.roots = []string{dir}
	return nil
}

func (g *gitWorld) seedRepoWithNoOriginRemote() error {
	dir, err := g.newDir("noorigin")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(dir); err != nil {
		return err
	}
	g.singleRoot = dir
	g.roots = []string{dir}
	return nil
}

func (g *gitWorld) assertSlugEquals(want string) func() error {
	return func() error {
		if err := g.startAndQuerySingle(); err != nil {
			return err
		}
		for _, r := range g.lastRepos {
			if r.Root == g.singleRoot {
				if r.Slug != want {
					return fmt.Errorf("slug = %q, want %q", r.Slug, want)
				}
				return nil
			}
		}
		return fmt.Errorf("no Repo row for root %q", g.singleRoot)
	}
}

func (g *gitWorld) assertSlugIsDirBasename() error {
	if err := g.startAndQuerySingle(); err != nil {
		return err
	}
	base := dirBasename(g.singleRoot)
	for _, r := range g.lastRepos {
		if r.Root == g.singleRoot {
			if r.Slug != base {
				return fmt.Errorf("slug = %q, want directory basename %q", r.Slug, base)
			}
			return nil
		}
	}
	return fmt.Errorf("no Repo row for root %q", g.singleRoot)
}

func dirBasename(p string) string {
	parts := strings.Split(strings.TrimRight(p, "/"), "/")
	return parts[len(parts)-1]
}

// ==================== AC-GIT-TMUX-JOIN ====================

func (g *gitWorld) seedAttendedUnattendedWorktrees() error {
	root, err := g.newDir("tmuxjoinroot")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(root); err != nil {
		return err
	}
	attended, err := g.newDir("attended")
	if err != nil {
		return err
	}
	unattended, err := g.newDir("unattended")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(attended)
	_ = os.RemoveAll(unattended)
	if err := gitWorktreeAddBranch(root, attended, "attended"); err != nil {
		return err
	}
	if err := gitWorktreeAddBranch(root, unattended, "unattended"); err != nil {
		return err
	}
	g.roots = []string{root}
	g.attendedPath, g.unattendedPath = attended, unattended
	return nil
}

func (g *gitWorld) seedTmuxSessionOnAttended() error {
	socket := fmt.Sprintf("sg-test-git-%d-%d", os.Getpid(), time.Now().UnixNano())
	if _, err := exec.Command("tmux", "-f", "/dev/null", "-L", socket, "new-session", "-d", "-s", "attended", "-c", g.attendedPath).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %w", err)
	}
	g.tmuxSocket = socket
	return nil
}

func (g *gitWorld) startWithGitAndTmux() error {
	return g.ensureStarted()
}

func (g *gitWorld) queryAndStoreForJoin() error {
	repos, err := g.queryRepos("")
	if err != nil {
		return err
	}
	g.lastRepos = repos
	return nil
}

func (g *gitWorld) assertAttendedTmuxSessionNotNull() error {
	return eventually(g.within(), func() error {
		if err := g.queryAndStoreForJoin(); err != nil {
			return err
		}
		wt := findWorktreeByPath(g.lastRepos, g.attendedPath)
		if wt == nil {
			return fmt.Errorf("attended worktree not found")
		}
		if wt.TmuxSess == nil {
			return fmt.Errorf("attended worktree's tmuxSession is null, want non-null")
		}
		return nil
	})
}

func (g *gitWorld) assertUnattendedTmuxSessionNull() error {
	wt := findWorktreeByPath(g.lastRepos, g.unattendedPath)
	if wt == nil {
		return fmt.Errorf("unattended worktree not found")
	}
	if wt.TmuxSess != nil {
		return fmt.Errorf("unattended worktree's tmuxSession is non-null, want null")
	}
	return nil
}

// ==================== AC-TMUX-GIT-JOIN-FIELDS ====================
//
// Regression for the #27+#29 drift: graph/git_map.go's tmuxSessionForWorktree and
// graph/tmux.resolvers.go's tmuxSessions both now build their TmuxSession through
// the single graph/tmux_helpers.go sessionToModel mapper. Before that fix, the git
// join built its own TmuxSession by hand and left `windows`/`attached` zero-valued
// — windows nil on a non-nullable field (a query-time error) and attached always
// false (a wrong answer that looks right, since false is a valid non-null value).
// This queries THROUGH the git join with a real tmux session that has real
// structure and a real attached client, and asserts real values — not mere
// non-null — against both the live tmux server and the top-level `tmuxSessions`
// query, which is the one direct proof that both paths agree.

// joinPaneDetail/joinWindowDetail/joinTmuxDetail are the minimal wire shape this
// scenario needs from TmuxSession, shared by both the git-join query (nested under
// Worktree.tmuxSession) and the top-level tmuxSessions query, so the two JSON
// payloads can be compared directly.
type joinPaneDetail struct {
	PaneID string `json:"paneId"`
}

type joinWindowDetail struct {
	Index int              `json:"index"`
	Panes []joinPaneDetail `json:"panes"`
}

type joinTmuxDetail struct {
	Name      string             `json:"name"`
	Attached  bool               `json:"attached"`
	CreatedAt *string            `json:"createdAt"`
	Windows   []joinWindowDetail `json:"windows"`
}

type joinWorktreeDetail struct {
	Path        string          `json:"path"`
	TmuxSession *joinTmuxDetail `json:"tmuxSession"`
}

func (g *gitWorld) seedWorktreeWithTmuxWindows() error {
	root, err := g.newDir("tmuxjoinfields")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(root); err != nil {
		return err
	}
	wt, err := g.newDir("joinwt")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(wt)
	if err := gitWorktreeAddBranch(root, wt, "joinwt"); err != nil {
		return err
	}
	g.roots = []string{root}
	g.joinWorktreePath = wt

	socket := fmt.Sprintf("sg-test-git-joinfields-%d-%d", os.Getpid(), time.Now().UnixNano())
	sessionName := "join"
	if out, err := exec.Command("tmux", "-f", "/dev/null", "-L", socket, "new-session", "-d", "-s", sessionName, "-c", wt).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %s", out)
	}
	// window 0: 1 pane (created above). window 1: 2 panes (new-window + a split).
	if out, err := exec.Command("tmux", "-L", socket, "new-window", "-t", sessionName).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-window: %s", out)
	}
	if out, err := exec.Command("tmux", "-L", socket, "split-window", "-t", sessionName+":1").CombinedOutput(); err != nil {
		return fmt.Errorf("tmux split-window: %s", out)
	}
	g.tmuxSocket = socket
	g.joinSessionName = sessionName
	return nil
}

// attachRealClientToJoinSession starts exactly one real (non-control-mode) attach
// via the shared scriptAttachCmd/`script`-pty helper (features/tmux_helpers_test.go)
// — reused, not reimplemented, and started only once: repeated pty attach cycles
// exhaust a finite system-wide pool (see AC-TMUX-ATTACHED-LIVE's doc comment).
// Whether the real tmux server has actually registered the client, and whether the
// join reflects it, is confirmed later by queryJoinAndTopLevelUntilAttached
// polling the live GraphQL query — not assumed here.
func (g *gitWorld) attachRealClientToJoinSession() error {
	cmd, ok := scriptAttachCmd(g.tmuxSocket, g.joinSessionName)
	if !ok {
		return godog.ErrPending
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start real attach via script: %w", err)
	}
	g.attachCmd = cmd
	return nil
}

func (g *gitWorld) queryJoinTmuxDetail() (*joinWorktreeDetail, error) {
	gql := `{ repos { worktrees { path tmuxSession { name attached createdAt windows { index panes { paneId } } } } } }`
	g.sw_.runCLI("query", gql)
	if g.sw_.lastExit != 0 {
		return nil, fmt.Errorf("query exit %d: %s", g.sw_.lastExit, g.sw_.lastStderr)
	}
	var d struct {
		Repos []struct {
			Worktrees []joinWorktreeDetail `json:"worktrees"`
		} `json:"repos"`
	}
	if err := json.Unmarshal([]byte(g.sw_.lastStdout), &d); err != nil {
		return nil, fmt.Errorf("parse %q: %w", g.sw_.lastStdout, err)
	}
	for _, r := range d.Repos {
		for i := range r.Worktrees {
			if r.Worktrees[i].Path == g.joinWorktreePath {
				return &r.Worktrees[i], nil
			}
		}
	}
	return nil, fmt.Errorf("no worktree row for path %q", g.joinWorktreePath)
}

func (g *gitWorld) queryTopLevelTmuxDetail() (*joinTmuxDetail, error) {
	gql := `{ tmuxSessions { name attached createdAt windows { index panes { paneId } } } }`
	g.sw_.runCLI("query", gql)
	if g.sw_.lastExit != 0 {
		return nil, fmt.Errorf("query exit %d: %s", g.sw_.lastExit, g.sw_.lastStderr)
	}
	var d struct {
		TmuxSessions []joinTmuxDetail `json:"tmuxSessions"`
	}
	if err := json.Unmarshal([]byte(g.sw_.lastStdout), &d); err != nil {
		return nil, fmt.Errorf("parse %q: %w", g.sw_.lastStdout, err)
	}
	for i := range d.TmuxSessions {
		if d.TmuxSessions[i].Name == g.joinSessionName {
			return &d.TmuxSessions[i], nil
		}
	}
	return nil, fmt.Errorf("no top-level tmuxSessions row named %q", g.joinSessionName)
}

// queryJoinAndTopLevelUntilAttached polls both the git-join query and the
// top-level tmuxSessions query (the git plugin's tmux config runs eventSource
// "poll", so attached only flips on the next reconcile) until the join's
// tmuxSession reads attached — the state the AC needs, since a permanently-false
// attached must be asserted as a failure, not sidestepped. Once true, both
// payloads are stashed for the Then steps.
func (g *gitWorld) queryJoinAndTopLevelUntilAttached() error {
	return eventually(g.within(), func() error {
		wt, err := g.queryJoinTmuxDetail()
		if err != nil {
			return err
		}
		if wt.TmuxSession == nil {
			return fmt.Errorf("worktree's tmuxSession is null")
		}
		if !wt.TmuxSession.Attached {
			return fmt.Errorf("worktree's tmuxSession not attached yet")
		}
		top, err := g.queryTopLevelTmuxDetail()
		if err != nil {
			return err
		}
		g.tmuxJoinWorktree = wt
		g.joinTopLevel = top
		return nil
	})
}

func sortWindows(ws []joinWindowDetail) []joinWindowDetail {
	out := make([]joinWindowDetail, len(ws))
	copy(out, ws)
	sort.Slice(out, func(i, j int) bool { return out[i].Index < out[j].Index })
	for i := range out {
		panes := make([]joinPaneDetail, len(out[i].Panes))
		copy(panes, out[i].Panes)
		sort.Slice(panes, func(a, b int) bool { return panes[a].PaneID < panes[b].PaneID })
		out[i].Panes = panes
	}
	return out
}

func (g *gitWorld) assertJoinWindowsMatchReal() error {
	if g.tmuxJoinWorktree == nil || g.tmuxJoinWorktree.TmuxSession == nil {
		return fmt.Errorf("no queried tmuxSession to check")
	}
	wOut, err := exec.Command("tmux", "-L", g.tmuxSocket, "list-windows", "-t", g.joinSessionName, "-F", "#{window_index}").CombinedOutput()
	if err != nil {
		return fmt.Errorf("list-windows: %s", wOut)
	}
	real := map[int][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(wOut)), "\n") {
		idx, perr := strconv.Atoi(strings.TrimSpace(line))
		if perr != nil {
			continue
		}
		pOut, err := exec.Command("tmux", "-L", g.tmuxSocket, "list-panes", "-t", fmt.Sprintf("%s:%d", g.joinSessionName, idx), "-F", "#{pane_id}").CombinedOutput()
		if err != nil {
			return fmt.Errorf("list-panes window %d: %s", idx, pOut)
		}
		var ids []string
		for _, id := range strings.Split(strings.TrimSpace(string(pOut)), "\n") {
			if id != "" {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		real[idx] = ids
	}

	got := map[int][]string{}
	for _, w := range g.tmuxJoinWorktree.TmuxSession.Windows {
		var ids []string
		for _, p := range w.Panes {
			ids = append(ids, p.PaneID)
		}
		sort.Strings(ids)
		got[w.Index] = ids
	}

	if !reflect.DeepEqual(real, got) {
		return fmt.Errorf("join tmuxSession windows/panes = %v, real tmux server = %v", got, real)
	}
	return nil
}

func (g *gitWorld) assertJoinAttached() error {
	if g.tmuxJoinWorktree == nil || g.tmuxJoinWorktree.TmuxSession == nil {
		return fmt.Errorf("no queried tmuxSession to check")
	}
	if !g.tmuxJoinWorktree.TmuxSession.Attached {
		return fmt.Errorf("worktree's tmuxSession.attached = false, want true (a real client is attached)")
	}
	return nil
}

func (g *gitWorld) assertJoinCreatedAtNonZero() error {
	if g.tmuxJoinWorktree == nil || g.tmuxJoinWorktree.TmuxSession == nil {
		return fmt.Errorf("no queried tmuxSession to check")
	}
	raw := g.tmuxJoinWorktree.TmuxSession.CreatedAt
	if raw == nil {
		return fmt.Errorf("worktree's tmuxSession.createdAt is null, want non-null")
	}
	ts, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return fmt.Errorf("createdAt %q does not parse as a timestamp: %w", *raw, err)
	}
	if ts.IsZero() || ts.Year() < 2000 {
		return fmt.Errorf("createdAt %q is the zero time, want a real session creation time", *raw)
	}
	return nil
}

func (g *gitWorld) assertJoinEqualsTopLevel() error {
	if g.tmuxJoinWorktree == nil || g.tmuxJoinWorktree.TmuxSession == nil {
		return fmt.Errorf("no queried join tmuxSession to compare")
	}
	if g.joinTopLevel == nil {
		return fmt.Errorf("no queried top-level tmuxSessions row to compare")
	}
	join := *g.tmuxJoinWorktree.TmuxSession
	join.Windows = sortWindows(join.Windows)
	top := *g.joinTopLevel
	top.Windows = sortWindows(top.Windows)
	if !reflect.DeepEqual(join, top) {
		return fmt.Errorf("git join's tmuxSession %+v does not equal the top-level tmuxSessions row %+v", join, top)
	}
	return nil
}

// ==================== AC-GIT-RECONCILE ====================

func (g *gitWorld) startWithSecondWorktree() error {
	root, err := g.newDir("reconcileroot")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(root); err != nil {
		return err
	}
	second, err := g.newDir("reconcilewt2")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(second)
	if err := gitWorktreeAddBranch(root, second, "doomed"); err != nil {
		return err
	}
	g.roots = []string{root}
	g.removedPath = second
	return g.ensureStarted()
}

func (g *gitWorld) removeSecondWorktree() error {
	return gitWorktreeRemove(g.roots[0], g.removedPath)
}

func (g *gitWorld) awaitReconcileMarksStale() error {
	return eventually(g.within(), func() error {
		repos, err := g.queryRepos("")
		if err != nil {
			return err
		}
		wt := findWorktreeByPath(repos, g.removedPath)
		if wt == nil {
			return fmt.Errorf("worktree %q disappeared entirely instead of being marked stale", g.removedPath)
		}
		if wt.StaleSince == nil {
			return fmt.Errorf("worktree %q not yet marked stale", g.removedPath)
		}
		g.lastRepos = repos
		return nil
	})
}

func (g *gitWorld) assertRemovedWorktreeStillPresent() error {
	if findWorktreeByPath(g.lastRepos, g.removedPath) == nil {
		return fmt.Errorf("worktree %q not present after reconcile", g.removedPath)
	}
	return nil
}

func (g *gitWorld) assertRemovedWorktreeStale() error {
	wt := findWorktreeByPath(g.lastRepos, g.removedPath)
	if wt == nil || wt.StaleSince == nil {
		return fmt.Errorf("worktree %q staleSince is null", g.removedPath)
	}
	return nil
}

// ==================== AC-GIT-SUBSCRIBE ====================

func (g *gitWorld) startBareRepoRoot() error {
	root, err := g.newDir("subroot")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(root); err != nil {
		return err
	}
	g.roots = []string{root}
	return g.ensureStarted()
}

// subscribeWorktreeUpdatedBG mirrors world.startSubscribeBG (steps_subscribe_test.go)
// but for the git plugin's `worktreeUpdated` field, reusing the same --ready-notify
// handshake so the next step's git-worktree-add can't race the subscription.
func (g *gitWorld) subscribeWorktreeUpdatedBG() error {
	w := g.sw_
	full := []string{
		"--config", w.cfgPath,
		"subscribe", "worktreeUpdated", "--once",
		"--endpoint", "http://" + w.listen + "/graphql",
		"--ready-notify",
	}
	cmd := exec.Command(binPath, full...)
	w.subStdout = &safeBuf{}
	w.subStderr = &safeBuf{}
	cmd.Stdout = w.subStdout
	cmd.Stderr = w.subStderr
	if err := cmd.Start(); err != nil {
		return err
	}
	w.subCmd = cmd
	w.subDone = make(chan struct{})
	go func() {
		w.subWaitErr = cmd.Wait()
		close(w.subDone)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(w.subStderr.String(), "subscribed") {
			return nil
		}
		select {
		case <-w.subDone:
			return fmt.Errorf("subscribe process exited before subscribing; stderr=%q", w.subStderr.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	return fmt.Errorf("subscribe process never reported subscribed within 5s; stderr=%q", w.subStderr.String())
}

func (g *gitWorld) addSecondWorktreeToRoot() error {
	second, err := g.newDir("subwt2")
	if err != nil {
		return err
	}
	_ = os.RemoveAll(second)
	return gitWorktreeAddBranch(g.roots[0], second, "new-worktree")
}

// ==================== AC-GIT-HOSTID ====================

func (g *gitWorld) startRepoRootHostTest() error {
	root, err := g.newDir("hostidroot")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(root); err != nil {
		return err
	}
	g.roots = []string{root}
	g.hostID = "test"
	return g.ensureStarted()
}

func (g *gitWorld) queryReposHostTest() error {
	repos, err := g.queryRepos("test")
	if err != nil {
		return err
	}
	g.hostQueryResult = repos
	return nil
}

func (g *gitWorld) queryReposHostNope() error {
	repos, err := g.queryRepos("nope")
	if err != nil {
		return err
	}
	g.hostQueryResult = repos
	return nil
}

func (g *gitWorld) assertAtLeastOneRepoRow() error {
	if len(g.hostQueryResult) == 0 {
		return fmt.Errorf("got 0 repo rows, want >= 1")
	}
	return nil
}

func (g *gitWorld) assertZeroRepoRows() error {
	if len(g.hostQueryResult) != 0 {
		return fmt.Errorf("got %d repo rows, want 0: %+v", len(g.hostQueryResult), g.hostQueryResult)
	}
	return nil
}

// ==================== AC-GIT-DORMANT ====================

func (g *gitWorld) startDormant() error {
	g.dormant = true
	g.roots = nil
	return g.ensureStarted()
}

func (g *gitWorld) queryReposNoFilter() error {
	repos, err := g.queryRepos("")
	if err != nil {
		return err
	}
	g.hostQueryResult = repos
	return nil
}

// ==================== AC-GIT-ISSUE-JOIN / AC-GIT-PR-JOIN ====================

// seedJoinWorktree makes a repo root (the git config entry AND, since no second
// worktree is added, the worktree GraphQL will report) whose origin remote is
// slug and whose checked-out branch is branch. It also spins up a per-scenario
// fakegh server (github.com/drewdrewthis/supergraph/plugins/github/fakegh, the
// same fake the github.feature/github-query.feature scenarios use) so a later
// "warm-cached" step can populate the plugin's cache through a real fetch.
func (g *gitWorld) seedJoinWorktree(slug, branch string) error {
	if _, _, ok := strings.Cut(slug, "/"); !ok {
		return fmt.Errorf("bad slug %q, want owner/repo", slug)
	}
	dir, err := g.newDir("joinroot")
	if err != nil {
		return err
	}
	if err := initRepoWithCommit(dir); err != nil {
		return err
	}
	if _, err := runGit(dir, "checkout", "-q", "-b", branch); err != nil {
		return err
	}
	if _, err := runGit(dir, "remote", "add", "origin", "git@github.com:"+slug+".git"); err != nil {
		return err
	}
	g.singleRoot = dir
	g.roots = []string{dir}
	g.githubEnabled = true
	g.ghFake = fakegh.New()
	return nil
}

// ghwForJoin builds a ghWorld sharing this scenario's *world and fakegh server so
// the join steps can call its production postOp helper (features/github_helpers_test.go)
// to genuinely fetch-and-cache through the plugin's own read-through path, rather
// than hand-rolling a second way to populate github_nodes.
func (g *gitWorld) ghwForJoin() *ghWorld {
	return &ghWorld{sw: g.sw_, fake: g.ghFake}
}

// seedWarmIssue registers issue #n in slug's fake repo and warms the github
// plugin's cache for it via a real "issue" op fetch (postOp, same as F1/coldstart
// in steps_github_test.go), then starts the git+github server if it isn't already.
func (g *gitWorld) seedWarmIssue(numStr, slug, title string) error {
	n := atoiMust(numStr)
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok {
		return fmt.Errorf("bad slug %q, want owner/repo", slug)
	}
	g.ghFake.AddRepo(owner, repo)
	g.ghFake.AddIssue(owner, repo, n, title, "open")
	if err := g.ensureStarted(); err != nil {
		return err
	}
	_, status, err := g.ghwForJoin().postOp("issue", map[string]any{"owner": owner, "repo": repo, "number": n})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("warm issue #%d for %s/%s: status %d", n, owner, repo, status)
	}
	return nil
}

// seedWarmPR registers PR #n with the given headRefName (and state, defaulting to
// "OPEN") in slug's fake repo and warms the cache via a real "pr" op fetch, so
// AC-GIT-PR-JOIN's direct-hit and headRefName-fallback paths both read a node the
// plugin actually populated, not one hand-inserted by the test.
func (g *gitWorld) seedWarmPR(numStr, slug, head, state string) error {
	n := atoiMust(numStr)
	owner, repo, ok := strings.Cut(slug, "/")
	if !ok {
		return fmt.Errorf("bad slug %q, want owner/repo", slug)
	}
	st := state
	if st == "" {
		st = "OPEN"
	}
	g.ghFake.AddRepo(owner, repo)
	g.ghFake.AddRichPR(owner, repo, n, fmt.Sprintf("pr %d", n), st,
		time.Now().UTC().Format(time.RFC3339), head, "main", "", nil, "", "")
	if err := g.ensureStarted(); err != nil {
		return err
	}
	_, status, err := g.ghwForJoin().postOp("pr", map[string]any{"owner": owner, "repo": repo, "number": n})
	if err != nil {
		return err
	}
	if status != 200 {
		return fmt.Errorf("warm PR #%d for %s/%s: status %d", n, owner, repo, status)
	}
	return nil
}

func (g *gitWorld) joinWorktree() (*worktreeJSON, error) {
	wt := findWorktreeByPath(g.lastRepos, g.singleRoot)
	if wt == nil {
		return nil, fmt.Errorf("no worktree found for root %q", g.singleRoot)
	}
	return wt, nil
}

func (g *gitWorld) assertIssueJoin(numStr, title string) error {
	n := atoiMust(numStr)
	wt, err := g.joinWorktree()
	if err != nil {
		return err
	}
	if wt.Issue == nil {
		return fmt.Errorf("worktree's issue is null, want #%d %q", n, title)
	}
	if wt.Issue.Number != n {
		return fmt.Errorf("issue number = %d, want %d", wt.Issue.Number, n)
	}
	if wt.Issue.Title != title {
		return fmt.Errorf("issue title = %q, want %q", wt.Issue.Title, title)
	}
	return nil
}

func (g *gitWorld) assertIssueNull() error {
	wt, err := g.joinWorktree()
	if err != nil {
		return err
	}
	if wt.Issue != nil {
		return fmt.Errorf("worktree's issue = %+v, want null", wt.Issue)
	}
	return nil
}

func (g *gitWorld) assertPRJoin(numStr string) error {
	n := atoiMust(numStr)
	wt, err := g.joinWorktree()
	if err != nil {
		return err
	}
	if wt.PullRequest == nil {
		return fmt.Errorf("worktree's pullRequest is null, want #%d", n)
	}
	if wt.PullRequest.Number != n {
		return fmt.Errorf("pullRequest number = %d, want %d", wt.PullRequest.Number, n)
	}
	return nil
}

func (g *gitWorld) assertPRJoinState(numStr, state string) error {
	if err := g.assertPRJoin(numStr); err != nil {
		return err
	}
	wt, err := g.joinWorktree()
	if err != nil {
		return err
	}
	if wt.PullRequest.State != state {
		return fmt.Errorf("pullRequest state = %q, want %q", wt.PullRequest.State, state)
	}
	return nil
}
