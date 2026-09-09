// Package git is a read model of the LOCAL git worktrees under a set of configured
// repo roots (issue #29). A low-cadence reconcile poll runs `git worktree list
// --porcelain` and, per non-bare worktree, `git rev-list --left-right --count
// HEAD...@{upstream}` to recompute ahead/behind, upserting the result into a
// per-plugin SQLite cache and stale-marking vanished worktrees. It serves Repo /
// Worktree over the gqlgen extend-type glob seam and republishes changes as
// hash-gated git.worktree.* events — no HTTPRoutes, no webhook. All state lives in
// the plugin's own db; core is untouched (S5). The cross-plugin join edges
// (Repo.worktrees, Worktree.tmuxSession/issue/pullRequest) are resolved in graph/
// (D2), not here.
package git

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/drewdrewthis/supergraph/core"
	"github.com/drewdrewthis/supergraph/plugins/internal/pluginconfig"
	"github.com/drewdrewthis/supergraph/plugins/internal/single"
)

func init() { core.Register("git", New) }

// config is the resolved [plugins.git] section: the repo roots to watch and the
// reconcile cadence. configured is false when no section is present, keeping the
// plugin dormant so it never polls repos the operator did not opt in (AC-GIT-DORMANT).
type config struct {
	roots             []string
	reconcileInterval time.Duration
	configured        bool
}

// gitStore is store's method set, named so reconcile_test.go can wrap the real
// *store with a fake that fails one root's writes on demand — the same
// injectable-seam pattern as runner (AC-GIT-RECONCILE's store-error isolation
// branch), without changing store.go itself.
type gitStore interface {
	migrate(ctx context.Context) error
	upsertRepo(ctx context.Context, r RepoNode, now time.Time) error
	upsertWorktree(ctx context.Context, r WorktreeNode, now time.Time) error
	markWorktreesStaleForRepo(ctx context.Context, repoKey string, keepKeys []string, now time.Time) ([]string, error)
	scanRepos(ctx context.Context, hostID string) ([]RepoNode, error)
	scanWorktrees(ctx context.Context, repoKey string) ([]WorktreeNode, error)
}

// Plugin is the git plugin. It holds the store, resolved config, captured emit, the
// injectable git runner the tests replace with a fake, and the in-memory emit gate
// (per-worktree state hashes + the reconcile worktree count for Cursor).
type Plugin struct {
	cfg    config
	hostID string
	store  gitStore
	run    runner
	now    func() time.Time

	emitMu sync.RWMutex
	emitFn core.Emit

	mu      sync.Mutex
	hashes  map[string]string
	wtCount int
}

// current is the running instance, published for graph/ resolvers; see
// plugins/internal/single.Ptr for the one-instance-per-process convention.
var current single.Ptr[Plugin]

func getCurrent() *Plugin { return current.Get() }

// New builds the plugin from its resolved config and registers it as the current
// instance for the graph resolvers. Each root's leading "~/" is expanded and the
// path cleaned so config and the porcelain paths key consistently.
func New(cfg core.PluginConfig) (core.Plugin, error) {
	c := config{
		reconcileInterval: 30 * time.Second,
		configured:        len(cfg.Raw) > 0,
	}
	for _, r := range pluginconfig.Strs(cfg.Raw, "repos") {
		c.roots = append(c.roots, expandRoot(r))
	}
	if n := pluginconfig.Int(cfg.Raw, "reconcileIntervalSeconds", 0); n > 0 {
		c.reconcileInterval = time.Duration(n) * time.Second
	}

	p := &Plugin{
		cfg:    c,
		hostID: cfg.HostID,
		run:    gitRunner,
		now:    func() time.Time { return time.Now().UTC() },
		hashes: map[string]string{},
	}
	// Publish early (store still nil): resolvers called before Migrate runs see a
	// live instance whose nil-store branch returns [] rather than reading through a
	// stale/absent current — see the second Set below for why Migrate must ALSO call it.
	current.Set(p)
	return p, nil
}

// Name is the stable plugin id (SQLite filename + health key).
func (p *Plugin) Name() string { return "git" }

// Migrate creates the plugin's two state tables and stashes the store for the
// reconcile loop and the graph resolvers.
func (p *Plugin) Migrate(ctx context.Context, s *core.Store) error {
	p.store = &store{core: s}
	// Same pointer as New's Set, but load-bearing on its own: single.Ptr wraps
	// atomic.Pointer, so THIS Store call is what happens-before a concurrent
	// resolver's Get — without it the plain p.store write above would be a data
	// race with any goroutine reading p.store through current.Get().
	current.Set(p)
	return p.store.migrate(ctx)
}

// Start captures emit, then runs the reconcile loop until ctx is done. With no
// [plugins.git] section or no roots the plugin stays dormant — it never polls the
// operator's repos unless git roots are configured (AC-GIT-DORMANT).
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	p.emitMu.Lock()
	p.emitFn = emit
	p.emitMu.Unlock()

	if !p.cfg.configured || len(p.cfg.roots) == 0 {
		<-ctx.Done()
		return nil
	}

	p.logReconcileErr(p.reconcile(ctx))
	ticker := time.NewTicker(p.cfg.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			p.logReconcileErr(p.reconcile(ctx))
		}
	}
}

// logReconcileErr surfaces a reconcile error instead of discarding it: a
// persistently failing store would otherwise reconcile into silence, with a
// frozen Cursor as the only symptom. err is the aggregated per-root error
// reconcile returns (errors.Join); nil means every root reconciled cleanly.
func (p *Plugin) logReconcileErr(err error) {
	if err != nil {
		log.Printf("git: reconcile: %v", err)
	}
}

// Cursor surfaces the reconcile high-water mark in the health snapshot (optional
// core.CursorReporter). With no config Start blocks forever without reconciling, so
// the cursor would otherwise stay "" with no explanation — surface why instead.
func (p *Plugin) Cursor(ctx context.Context) string {
	if !p.cfg.configured || len(p.cfg.roots) == 0 {
		return "dormant: no [plugins.git] config"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return fmt.Sprintf("worktrees=%d", p.wtCount)
}

// expandRoot expands a leading "~/" to the user's home dir, cleans the path, and
// resolves symlinks. `git worktree list --porcelain` always reports fully resolved
// paths, so an unresolved root (e.g. "/tmp/x" on macOS, where /tmp is itself a
// symlink into /private/tmp) would disagree with its own worktrees' paths and break
// the Worktree.tmuxSession path-based join. EvalSymlinks errors when the path
// doesn't exist yet (an unconfigured/typo'd root); the cleaned path is still
// reported as configured in that case, and reconcile already skips roots whose git
// invocation fails.
func expandRoot(r string) string {
	if strings.HasPrefix(r, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			r = filepath.Join(home, r[2:])
		}
	}
	cleaned := filepath.Clean(r)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return resolved
	}
	return cleaned
}
