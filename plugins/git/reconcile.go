package git

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// runner is the seam that lets the reconcile tests drive a fake git instead of a
// real repo (AC-GIT-RECONCILE's one-root-error isolation branch). dir is the cwd
// the git subcommand runs in, so per-worktree upstream counts run against the right
// checkout without a `-C` flag baked into args.
type runner func(ctx context.Context, dir string, args ...string) (string, error)

// gitRunner is the production runner: it runs the operator's git in dir and returns
// STDOUT ONLY, surfacing a non-zero exit as an error so a failed root is skipped
// rather than read as empty (AC-GIT-RECONCILE). Stdout is kept separate from
// stderr deliberately: parseAheadBehind expects exactly two whitespace-separated
// fields, and a `warning:` git writes to stderr (e.g. a dubious-ownership notice)
// would otherwise merge into stdout and collapse the parse to (nil, nil).
func gitRunner(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // G204: operator-configured repo roots, fixed git subcommands, not attacker input
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return string(out), fmt.Errorf("%w: %s", err, ee.Stderr)
		}
		return string(out), err
	}
	return string(out), nil
}

// reconcile runs one backstop poll across every configured root, INDEPENDENTLY: a
// git failure OR a store failure in one root is logged-and-skipped so it can
// neither abort the other roots nor stale-mark their rows (AC-GIT-RECONCILE, and
// the store-error isolation this join extends D3 to cover). For each surviving
// root it upserts the repo and its non-bare worktrees, computes ahead/behind
// against each worktree's own upstream, stale-marks the vanished, and emits
// hash-gated events. Store errors are collected and returned joined so the caller
// (Start) can log that something went wrong instead of the failure vanishing.
func (p *Plugin) reconcile(ctx context.Context) error {
	now := p.now()
	total := 0
	var errs []error
	for _, root := range p.cfg.roots {
		wtOut, err := p.run(ctx, root, "worktree", "list", "--porcelain")
		if err != nil {
			// Skip THIS root only: no upsert, no stale-mark — its cached rows stay as
			// they were so one root's git error never corrupts another's read model.
			continue
		}

		remote, _ := p.run(ctx, root, "remote", "get-url", "origin")
		slug := RepoSlug(remote, root)
		repoKey := p.hostID + ":" + root
		if err := p.store.upsertRepo(ctx, RepoNode{Key: repoKey, HostID: p.hostID, Slug: slug, Root: root}, now); err != nil {
			// Skip THIS root only, same as a git failure above — a store error must
			// not stop reconcile from reaching later roots.
			errs = append(errs, fmt.Errorf("root %s: upsert repo: %w", root, err))
			continue
		}

		var seen []string
		rootFailed := false
		for _, wt := range parseWorktreeList(wtOut) {
			// Bare worktrees have no branch state and are not sidebar rows — skip them.
			if wt.Bare {
				continue
			}
			node := p.worktreeNode(ctx, repoKey, slug, wt)
			if err := p.store.upsertWorktree(ctx, node, now); err != nil {
				errs = append(errs, fmt.Errorf("root %s: upsert worktree %s: %w", root, node.Key, err))
				rootFailed = true
				continue
			}
			seen = append(seen, node.Key)
			total++
			p.emitIfChanged(ctx, node, now)
		}
		if rootFailed {
			// A worktree upsert failed: skip this root's stale-marking pass too — seen
			// is incomplete, so marking against it would wrongly stale-mark worktrees
			// that never got a chance to upsert this round.
			continue
		}

		gone, err := p.store.markWorktreesStaleForRepo(ctx, repoKey, seen, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("root %s: mark stale: %w", root, err))
			continue
		}
		for _, k := range gone {
			p.forget(k)
			p.emit(ctx, "git.worktree.removed", k, map[string]any{"key": k}, now)
		}
	}

	p.mu.Lock()
	p.wtCount = total
	p.mu.Unlock()
	return errors.Join(errs...)
}

// worktreeNode builds one worktree row, resolving ahead/behind against the
// worktree's OWN upstream (@{upstream}, the same ref `git status -sb` compares
// against — AC-GIT-AHEAD-BEHIND). A branch with no upstream makes rev-list fail, so
// Ahead/Behind stay nil, kept distinct from a genuine 0.
func (p *Plugin) worktreeNode(ctx context.Context, repoKey, slug string, wt Worktree) WorktreeNode {
	node := WorktreeNode{
		Key:      p.hostID + ":" + filepath.Clean(wt.Path),
		HostID:   p.hostID,
		RepoKey:  repoKey,
		RepoSlug: slug,
		Path:     wt.Path,
		Branch:   branchPtr(wt.Branch),
		Head:     wt.Head,
		Detached: wt.Detached,
	}
	if out, err := p.run(ctx, wt.Path, "rev-list", "--left-right", "--count", "HEAD...@{upstream}"); err == nil {
		node.Ahead, node.Behind = parseAheadBehind(out)
	}
	return node
}

// branchPtr turns worktree.go's "" (Detached or Bare, by parseWorktreeList's own
// contract) into nil, so a detached HEAD stores/renders as null, not "" (AC-GIT-WORKTREES).
func branchPtr(branch string) *string {
	if branch == "" {
		return nil
	}
	return &branch
}

// emitIfChanged emits git.worktree.updated only when the worktree is new or its
// reconcile-visible state moved since the last emit — the hash gate that keeps an
// unchanged reconcile silent while a changed one emits exactly once (AC-GIT-SUBSCRIBE).
func (p *Plugin) emitIfChanged(ctx context.Context, n WorktreeNode, now time.Time) {
	h := worktreeHash(n)
	p.mu.Lock()
	prev, existed := p.hashes[n.Key]
	p.hashes[n.Key] = h
	p.mu.Unlock()
	if existed && prev == h {
		return
	}
	p.emit(ctx, "git.worktree.updated", n.Key, worktreePayload(n), now)
}

// forget drops a vanished worktree's hash so a later reappearance counts as new and
// re-emits, rather than being silenced by a stale hash from before it was removed.
func (p *Plugin) forget(key string) {
	p.mu.Lock()
	delete(p.hashes, key)
	p.mu.Unlock()
}

// worktreeHash is a stable digest of the fields a subscriber keys on: branch, head,
// detached, and ahead/behind. StaleSince is deliberately excluded — worktreeNode
// never sets it (a stale transition goes through forget + the "removed" event, not
// this hash), so including it would always compare false and add nothing.
// ptrStr/ptrStrBranch render a nil as a sentinel distinct from any real value, so a
// nil branch never hash-collides with a branch literally named "\x00" — and never
// with a genuine "" either, matching the nil/0 discipline ahead/behind already gets.
func worktreeHash(n WorktreeNode) string {
	return fmt.Sprintf("%s|%s|%v|%s|%s",
		ptrStrBranch(n.Branch), n.Head, n.Detached, ptrStr(n.Ahead), ptrStr(n.Behind))
}

func ptrStr(p *int) string {
	if p == nil {
		return ""
	}
	return fmt.Sprintf("%d", *p)
}

// ptrStrBranch renders a nil branch as a NUL-prefixed sentinel that no real branch
// name can contain, so nil never hash-collides with a genuine "" (AC-GIT-WORKTREES).
func ptrStrBranch(p *string) string {
	if p == nil {
		return "\x00nil"
	}
	return *p
}

// worktreePayload is the event body carrying the worktree's fields. Ahead/Behind
// stay pointers so a null upstream serializes as JSON null, not 0.
func worktreePayload(n WorktreeNode) map[string]any {
	return map[string]any{
		"key": n.Key, "hostId": n.HostID, "repoKey": n.RepoKey, "repoSlug": n.RepoSlug,
		"path": n.Path, "branch": n.Branch, "head": n.Head, "detached": n.Detached,
		"ahead": n.Ahead, "behind": n.Behind,
	}
}

// emit builds and sends one V:1 envelope through the captured emit closure — the
// first schema for git.worktree.* (issue #29), matching every other plugin's first
// version (tmux/snapshot.go, claude/claude.go, github/proxy.go, peer/liveness.go).
func (p *Plugin) emit(ctx context.Context, typ, key string, payload map[string]any, now time.Time) {
	p.emitMu.RLock()
	emit := p.emitFn
	p.emitMu.RUnlock()
	if emit == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = emit(ctx, core.Envelope{TS: now, Source: p.Name(), Type: typ, V: 1, Key: key, Payload: body})
}
