package git

import "context"

// Repos returns the cached repo roots for host (empty when the plugin is not yet
// running). It is the accessor graph/ delegates to for the repos query — a nil or
// unstarted instance yields no rows rather than a panic (single.Ptr convention).
func Repos(ctx context.Context, hostID string) []RepoNode {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil
	}
	out, err := p.store.scanRepos(ctx, hostID)
	if err != nil {
		return nil
	}
	return out
}

// WorktreesForRepo returns the cached worktrees under repoKey (the Repo.worktrees
// join graph/ resolves against). Empty when the plugin is not yet running.
func WorktreesForRepo(ctx context.Context, repoKey string) []WorktreeNode {
	p := getCurrent()
	if p == nil || p.store == nil {
		return nil
	}
	out, err := p.store.scanWorktrees(ctx, repoKey)
	if err != nil {
		return nil
	}
	return out
}
