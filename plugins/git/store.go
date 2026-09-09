package git

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// store is the git plugin's SQLite state, layered over the core.Store (which owns
// the events ring and cursors). It holds the repo/worktree read model the reconcile
// poll keeps current — one row per configured root and per live worktree under it.
type store struct{ core *core.Store }

const rfc = time.RFC3339Nano

// RepoNode is one cached repo root, keyed "<hostID>:<root>". It is the gqlgen-bound
// Go type for GraphQL `Repo`, so its exported fields ARE the flattened scalar
// fields; the `worktrees` edge is deliberately absent so gqlgen emits a
// field-resolver stub for the cross-plugin join in graph/ (the D2 seam).
type RepoNode struct {
	Key, HostID, Slug, Root string
	StaleSince              *time.Time
}

// WorktreeNode is one cached worktree, keyed "<hostID>:<cleanPath>". It is the
// gqlgen-bound Go type for GraphQL `Worktree`. Ahead/Behind are *int on purpose:
// nil (no upstream / detached) must stay distinct from a genuine 0 "level with
// upstream" (AC-GIT-AHEAD-BEHIND), so they are stored as SQL NULL, never coerced.
// Branch is *string for the same reason: a detached HEAD has no branch, and the
// schema's nullable `branch` must render GraphQL null there, never "" (AC-GIT-WORKTREES).
// RepoSlug is denormalised via a JOIN on scan so the resolver can build github
// cache keys without a second lookup. The tmuxSession/issue/pullRequest join edges
// are absent so gqlgen stubs them in graph/ (D2).
type WorktreeNode struct {
	Key, HostID, RepoKey, RepoSlug, Path, Head string
	Branch                                     *string
	Detached                                   bool
	Ahead, Behind                              *int
	StaleSince                                 *time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS git_repos (
	key          TEXT PRIMARY KEY,
	host_id      TEXT,
	slug         TEXT,
	root         TEXT,
	last_seen_at TEXT,
	stale_since  TEXT
);
CREATE TABLE IF NOT EXISTS git_worktrees (
	key          TEXT PRIMARY KEY,
	host_id      TEXT,
	repo_key     TEXT,
	path         TEXT,
	branch       TEXT,
	head         TEXT,
	detached     INTEGER,
	ahead        INTEGER,
	behind       INTEGER,
	last_seen_at TEXT,
	stale_since  TEXT
);
CREATE INDEX IF NOT EXISTS git_worktrees_repo ON git_worktrees(repo_key);`

func (s *store) migrate(ctx context.Context) error {
	if _, err := s.db().ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("git: migrate: %w", err)
	}
	return nil
}

func (s *store) db() *sql.DB { return s.core.DB() }

// upsertRepo writes one repo root, clearing stale_since (a seen root is live).
func (s *store) upsertRepo(ctx context.Context, r RepoNode, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO git_repos (key, host_id, slug, root, last_seen_at, stale_since)
		 VALUES (?,?,?,?,?,NULL)
		 ON CONFLICT(key) DO UPDATE SET host_id=excluded.host_id, slug=excluded.slug,
		   root=excluded.root, last_seen_at=excluded.last_seen_at, stale_since=NULL`,
		r.Key, r.HostID, r.Slug, r.Root, now.UTC().Format(rfc))
	if err != nil {
		return fmt.Errorf("git: upsert repo: %w", err)
	}
	return nil
}

// upsertWorktree writes one worktree, clearing stale_since. Ahead/Behind bind
// through nullInt, and Branch through nullStr, so a nil pointer persists as SQL
// NULL, not 0/"" (AC-GIT-AHEAD-BEHIND, AC-GIT-WORKTREES).
func (s *store) upsertWorktree(ctx context.Context, r WorktreeNode, now time.Time) error {
	_, err := s.db().ExecContext(ctx,
		`INSERT INTO git_worktrees (key, host_id, repo_key, path, branch, head, detached, ahead, behind, last_seen_at, stale_since)
		 VALUES (?,?,?,?,?,?,?,?,?,?,NULL)
		 ON CONFLICT(key) DO UPDATE SET host_id=excluded.host_id, repo_key=excluded.repo_key,
		   path=excluded.path, branch=excluded.branch, head=excluded.head, detached=excluded.detached,
		   ahead=excluded.ahead, behind=excluded.behind, last_seen_at=excluded.last_seen_at, stale_since=NULL`,
		r.Key, r.HostID, r.RepoKey, r.Path, nullStr(r.Branch), r.Head, b2i(r.Detached),
		nullInt(r.Ahead), nullInt(r.Behind), now.UTC().Format(rfc))
	if err != nil {
		return fmt.Errorf("git: upsert worktree: %w", err)
	}
	return nil
}

// markWorktreesStaleForRepo stamps stale_since on every currently-live worktree
// under repoKey whose key is NOT in keepKeys, and returns the keys it newly staled.
// The repoKey filter is load-bearing: a reconcile that skips one root on a git error
// must never stale-mark another root's rows (AC-GIT-RECONCILE). The returned keys ARE
// the removal set — the `WHERE stale_since IS NULL` guard means only live→stale
// transitions are reported, satisfying "became stale that was not stale before".
func (s *store) markWorktreesStaleForRepo(ctx context.Context, repoKey string, keepKeys []string, now time.Time) ([]string, error) {
	keep := map[string]bool{}
	for _, k := range keepKeys {
		keep[k] = true
	}
	rows, err := s.db().QueryContext(ctx,
		`SELECT key FROM git_worktrees WHERE repo_key=? AND stale_since IS NULL`, repoKey)
	if err != nil {
		return nil, err
	}
	var gone []string
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var k string
			if err = rows.Scan(&k); err != nil {
				return
			}
			if !keep[k] {
				gone = append(gone, k)
			}
		}
		err = rows.Err()
	}()
	if err != nil {
		return nil, err
	}
	ts := now.UTC().Format(rfc)
	for _, k := range gone {
		if _, err := s.db().ExecContext(ctx,
			`UPDATE git_worktrees SET stale_since=? WHERE key=? AND stale_since IS NULL`, ts, k); err != nil {
			return nil, err
		}
	}
	return gone, nil
}

// scanRepos returns cached repos, optionally filtered by host ("" = all).
func (s *store) scanRepos(ctx context.Context, hostID string) ([]RepoNode, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT key, host_id, slug, root, stale_since FROM git_repos ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RepoNode
	for rows.Next() {
		var (
			r     RepoNode
			stale sql.NullString
		)
		if err := rows.Scan(&r.Key, &r.HostID, &r.Slug, &r.Root, &stale); err != nil {
			return nil, err
		}
		r.StaleSince = nsTime(stale)
		if hostID == "" || r.HostID == hostID {
			out = append(out, r)
		}
	}
	return out, rows.Err()
}

// scanWorktrees returns cached worktrees under repoKey, LEFT JOINing git_repos so
// each row carries its repo slug (the resolver needs it for github cache keys).
func (s *store) scanWorktrees(ctx context.Context, repoKey string) ([]WorktreeNode, error) {
	rows, err := s.db().QueryContext(ctx,
		`SELECT w.key, w.host_id, w.repo_key, COALESCE(r.slug,''), w.path, w.branch, w.head,
		        w.detached, w.ahead, w.behind, w.stale_since
		 FROM git_worktrees w LEFT JOIN git_repos r ON w.repo_key = r.key
		 WHERE w.repo_key = ? ORDER BY w.key`, repoKey)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []WorktreeNode
	for rows.Next() {
		var (
			r             WorktreeNode
			branch        sql.NullString
			detached      int
			ahead, behind sql.NullInt64
			stale         sql.NullString
		)
		if err := rows.Scan(&r.Key, &r.HostID, &r.RepoKey, &r.RepoSlug, &r.Path, &branch, &r.Head,
			&detached, &ahead, &behind, &stale); err != nil {
			return nil, err
		}
		r.Branch = nsStr(branch)
		r.Detached = detached == 1
		r.Ahead, r.Behind = niInt(ahead), niInt(behind)
		r.StaleSince = nsTime(stale)
		out = append(out, r)
	}
	return out, rows.Err()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// nullInt binds a *int as SQL NULL when nil, so an unknown ahead/behind never
// persists as 0 (AC-GIT-AHEAD-BEHIND).
func nullInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// niInt reads a nullable integer column back into *int, preserving the nil/0
// distinction the upstream comparison depends on.
func niInt(n sql.NullInt64) *int {
	if !n.Valid {
		return nil
	}
	v := int(n.Int64)
	return &v
}

// nullStr binds a *string as SQL NULL when nil, so a detached HEAD's absent
// branch never persists as "" (AC-GIT-WORKTREES).
func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// nsStr reads a nullable text column back into *string, preserving the nil/""
// distinction on the way back out.
func nsStr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	v := n.String
	return &v
}

// nsTime parses a nullable RFC3339 column into an optional time.
func nsTime(ns sql.NullString) *time.Time {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	t, err := time.Parse(rfc, ns.String)
	if err != nil {
		return nil
	}
	return &t
}
