# EDR: git plugin — local worktree read model

Engineering Design Record. Owns the internals issue #29 and epic #30 leave to the plugin.
Act-doc: decisions + steps, not prose. Mirrors the shape of [tmux EDR](./tmux.md) and
[github-query EDR](./github-query.md).

**Design (2026-09-09).** The git plugin is a **read model of the LOCAL git worktrees** under a
set of configured repo roots. Part of epic #30: it replaces `cmd/orchard-sidebar`'s 30 s
`workView { repos { worktrees { branch path ahead behind tmuxSession pr issue } } }` poll against
orchard-daemon. A low-cadence **reconcile poll** (`git worktree list --porcelain`, then per
worktree `git rev-list --left-right --count HEAD...@{upstream}`) upserts a per-plugin SQLite
cache and stale-marks vanished worktrees. It serves `Repo`/`Worktree` over the gqlgen
`extend type` glob seam and republishes changes as hash-gated `git.worktree.*` events — no
`HTTPRoutes`, no webhook. The cross-plugin join edges (`Repo.worktrees`,
`Worktree.tmuxSession`/`issue`/`pullRequest`) are resolved in `graph/git_map.go` (D6), not in
the plugin.

**Hard constraints (owner / issue #29):** zero core diff (S5); reuses `internal/issuekey` for
branch→issue/PR derivation rather than reimplementing parsing; LOC budget `loc-git`
(§LOC budget).

---

## Decisions

| # | Decision | Chosen | Rejected |
|---|----------|--------|----------|
| **D1** | **Ahead/behind resolve against the branch's own upstream** | `git rev-list --left-right --count HEAD...@{upstream}`, run in the worktree's own directory (`worktreeNode`, `reconcile.go`). This is the same ref `git status -sb` compares against, which is the form AC-GIT-AHEAD-BEHIND is stated against. | Comparing every worktree to a hardcoded `origin/main` — confidently wrong for any branch based elsewhere (a branch off a release branch, or in a fork). |
| **D2** | **`ahead`/`behind` are nullable, and so is `branch`** | `WorktreeNode.Ahead`/`.Behind` are `*int`; `.Branch` is `*string`. `nil` = no upstream / detached HEAD; `0` = genuinely level with upstream. `""` is never used as an absence marker. The distinction is carried parser (`parseAheadBehind` returns `(nil,nil)` on any malformed/failed `rev-list`) → nullable SQLite column (`nullInt`/`nullStr` bind nil as SQL `NULL`, never 0/"") → nullable GraphQL field (`ahead: Int`, `behind: Int`, `branch: String`), and asserted distinctly at each layer. | Coercing a missing upstream or a detached HEAD to `0`/`""` — indistinguishable from a genuinely level branch or (if `""` were used) a branch literally named `""`. A live run caught `branch` rendering as `""` for a detached HEAD, which is why it became `*string` rather than a plain Go `string` with an empty-string sentinel. |
| **D3** | **Per-root reconcile isolation** | `reconcile()` runs each configured root independently (`reconcile.go`); a `git worktree list` failure in one root `continue`s to the next root with no upsert and no stale-mark for that root. Stale-marking (`markWorktreesStaleForRepo`) is scoped by `repo_key`. | A single global mark-all-stale-then-reupsert pass — one root going momentarily unreadable (e.g. a network mount hiccup) would blank every repo's rows the instant it failed, not just its own. |
| **D4** | **`markWorktreesStaleForRepo` returns the keys it newly staled** | The `WHERE stale_since IS NULL` guard means only live→stale transitions are reported (`store.go`); `reconcile()` uses that returned slice directly as the removal-event set (`p.forget(k)` + `git.worktree.removed` emit) — no second query needed. | tmux's convention: a void `markPaneStale` plus a separate diff query to compute what changed. Diverges deliberately here because the removal set is exactly what the caller (the emit loop) needs, and computing it inside the single UPDATE avoids a second round trip. |
| **D5** | **Vanished worktrees are stale-marked, not deleted** | A worktree removed between reconciles gets a non-nil `staleSince` and stays visible in `repos { worktrees { ... staleSince } }`. | Deleting the row — the sidebar would have the worktree silently vanish from the list instead of showing it as gone-since-`T`. |
| **D6** | **Cross-plugin joins live in `graph/`, in `git_map.go`** | No plugin imports another; `git` publishes a `single.Ptr[Plugin]` singleton (`current`) and exports `Repos`/`WorktreesForRepo` accessors (`resolver.go`). `graph/git_map.go` holds `tmuxSessionForWorktree`, `splitRepoSlug`, `pullRequestForWorktree` — outside `graph/git.resolvers.go` because gqlgen's follow-schema regen preserves only resolver METHOD bodies, and these are free functions plus package-level state a regen would otherwise clobber. Same seam as `graph/github_map.go` (github-query EDR D2/D3). | Having the git plugin import `tmux`/`github` directly to self-join — cross-plugin coupling, and the join is a view concern `graph/` is already positioned for (github-query EDR D2, same rejection). |
| **D7** | **The PR join is two-source, most-recent-wins** | `pullRequestForWorktree` (`git_map.go`) gathers `direct` — the issue-key-derived cache lookup (`issuekey.FromBranch(branch)` → `pr:<slug>#<N>`), accepted only when `direct.HeadRefName == branch` (PR #N and issue #N are different nodes in the same repo, so a hit on the wrong node must be rejected) — as one candidate alongside every matching PR from a scan of `github.CachedPRs(owner, repo)`, never as a short-circuit. Across all candidates the **largest `Number` wins**: a branch reused across PRs (a merged `#5`, later an open `#12`) must resolve to the current PR, since the sidebar reader expects the PR that's live now, not whichever one the issue-key derivation or scan order happened to surface first. Neither path filters by state: a closed or merged PR on the branch is still returned, because the sidebar renders PR state. | Returning `direct` immediately on a `HeadRefName` match without consulting `cached` — makes selection depend on whether the issue-key-derived lookup happened to hit, so the two paths could disagree when several PRs sit on one branch. Also rejected: smallest-`Number` tie-break — wrong direction, since it prefers a branch's first (often merged/closed) PR over its current one. |
| **D8** | **The tmux join compares `filepath.Clean` paths only; symlinks are out of scope** | `tmuxSessionForWorktree` (`git_map.go`) matches `filepath.Clean(session.Worktree) == filepath.Clean(worktree.Path)`. When more than one session sits on one path, the match tie-breaks on the lexicographically smallest `Name`, keeping the result deterministic across calls (`tmux.Sessions` ordering is not guaranteed). | Resolving symlinks before comparing — a session on a symlinked path that differs textually from the worktree's reported path will not join. **Known limitation, documented, not silent**: a worktree opened through a symlinked path will not show its tmux session. |
| **D9** | **Events are hash-gated** | `emitIfChanged` (`reconcile.go`) computes `worktreeHash` over branch/head/detached/ahead/behind/staleness and only emits `git.worktree.updated` when the hash moved from the last emit; an unchanged reconcile emits nothing. `ptrStrBranch`/`ptrStr` render a nil pointer as a sentinel (`"\x00nil"`) distinct from any real branch name or a genuine `""`/`0` — the nil/0/"" distinction from D2 must survive into the hash or the gate would collapse a "went from unknown to level" transition into a no-op. Same convention landed for github in #32. | Hashing the fields naively (e.g. `%s` on a nil `*string`) — Go's default nil-pointer formatting or a coerced `""` would hash-collide a detached HEAD with a branch literally named `""`, silently swallowing real transitions. |
| **D10** | **`worktreeUpdated` subscription added beyond the issue's original scope** | `extend type Subscription { worktreeUpdated: GitEvent! }` (`schema/git.graphqls`) and its resolver (`graph/git.resolvers.go`) were added though issue #29's body doesn't ask for one. Without a subscriber, `git.worktree.updated`/`.removed` envelopes would have no consumer, and epic #30's stated goal is that the sidebar drops **all** polling — a repos-query-only plugin would leave the 30 s poll in place. | Shipping only the `repos` query and leaving the sidebar to keep polling it — technically satisfies issue #29's letter but not epic #30's goal. |
| **D11** | **Zero core changes** | Schema arrives via the `plugins/*/schema/*.graphqls` glob (`gqlgen.yml`), registration via one blank import in `graph/plugins_import.go`, joins in `graph/git_map.go` + `graph/git.resolvers.go`. No `cmd/` delta, no `core/` edit. | N/A — this is the seam every plugin in this repo uses; see tmux EDR D6 / github-query EDR D2. |

## Schema (`plugins/git/schema/git.graphqls`, `extend type` only)

```graphql
type Repo {
  hostId: String!
  slug: String!
  root: String!
  staleSince: Time
  worktrees: [Worktree!]!
}

type Worktree {
  hostId: String!
  repoSlug: String!
  path: String!
  branch: String
  head: String!
  detached: Boolean!
  ahead: Int
  behind: Int
  staleSince: Time
  tmuxSession: TmuxSession
  issue: Issue
  pullRequest: PullRequest
}

type GitEvent { ts: Time!  type: String!  v: Int!  key: String!  payload: String! }

extend type Query { repos(hostId: String): [Repo!]! }
extend type Subscription { worktreeUpdated: GitEvent! }
```

`Repo.worktrees` and `Worktree.{tmuxSession,issue,pullRequest}` are deliberately absent from the
bound Go structs (`git.RepoNode`/`git.WorktreeNode`) so gqlgen emits field-resolver stubs for the
D6 cross-plugin join, exactly as tmux/github do for their own join edges.

## Config `[plugins.git]`

| Key | Default | Meaning |
|---|---|---|
| `repos` | `[]` | List of repo root paths to watch. A leading `~/` is expanded to the user's home dir (`expandRoot`, `git.go`); each path is `filepath.Clean`ed so config and porcelain-reported paths key the same. |
| `reconcileIntervalSeconds` | `30` | Backstop poll cadence = worst-case staleness bound. |

With no `[plugins.git]` section, or `repos = []`, `Start` blocks on `ctx.Done()` without ever
polling (AC-GIT-DORMANT) — the plugin registers, `/health` serves a `git` entry, but no operator
repo is touched until a root is configured. `Cursor()` reports `"dormant: no [plugins.git]
config"` in that state instead of an unexplained empty string.

## Measurements / Acceptance Criteria

ACs are drawn from issue #29's last "Planning comment" (the authoritative v2 set). One-line
statement each; full evidence requirements live in the issue.

- **AC-GIT-WORKTREES** — `repos { slug root worktrees { branch path } }` returns one `Repo` per
  configured root, matching `git worktree list --porcelain`; `branch` null for a detached HEAD.
- **AC-GIT-AHEAD-BEHIND** — `ahead`/`behind` match `git status -sb`'s counts against
  `@{upstream}`; both `null` for no-upstream/detached, never `0` (D1, D2).
- **AC-GIT-SLUG** — `Repo.slug` is `owner/name` parsed from the `origin` remote (ssh or https
  form); falls back to the root's directory basename with no `origin`.
- **AC-GIT-TMUX-JOIN** — `worktree.tmuxSession` matches by `filepath.Clean`d path; symlinks
  out of scope (D8).
- **AC-GIT-ISSUE-JOIN** — `worktree.issue` resolves via `internal/issuekey.FromBranch` against a
  warm github cache; cold cache or no `issueN` prefix returns `null`.
- **AC-GIT-PR-JOIN** — `worktree.pullRequest` resolves via the two-source, most-recent-wins join
  (D7); state is never filtered.
- **AC-GIT-RECONCILE** — reconciles on start and every `reconcileIntervalSeconds`; a removed
  worktree stays visible with non-null `staleSince`; a git failure in one root leaves other roots'
  rows untouched (D3, D5).
- **AC-GIT-SUBSCRIBE** — a worktree add/remove/ahead-behind move emits exactly one
  `git.worktree.*` envelope; an unchanged reconcile emits nothing (D9, D10).
- **AC-GIT-HOSTID** — `repos(hostId:)` filters correctly; an unknown host returns `[]`.
- **AC-GIT-DORMANT** — no `[plugins.git]` section or `repos = []` starts the server without error
  and `repos` returns `[]`.
- **AC-GIT-CI** — `make loc-git` and `make features-git` exist and are wired into both
  `ci.yml` and `macos.yml`.
- **AC-GIT-ZERO-CORE** — `git diff --name-only origin/main...HEAD -- core/` is empty (D11).

## LOC budget (prod; tests excluded) — `loc-git`

Strip formula (portable GNU/BSD sed), mirroring `loc-tmux`:
`find plugins/git -name '*.go' ! -name '*_test.go' | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$/d' | wc -l`

Per the owner rule, the cap is **measured + 5%, rounded up to a multiple of 10**.

| File (`plugins/git/`) | Responsibility |
|---|---|
| `git.go` | wiring: `init`/`New`/`Name`/`Migrate`/`Start` (dormant when unconfigured, D-AC-GIT-DORMANT) + `Cursor` + config parse/defaults + `single.Ptr` singleton |
| `worktree.go` | pure parsers: `parseWorktreeList` (porcelain stanzas), `parseAheadBehind`, `slugFromRemoteURL`/`segmentsToSlug`/`RepoSlug` — no process execution, exhaustively unit-testable |
| `reconcile.go` | per-root reconcile loop (D3), worktree-node build (D1), hash-gated emit (D9), event payload/envelope construction |
| `store.go` | SQLite: migrate, upsert repo/worktree (D2's null binding), `markWorktreesStaleForRepo` (D4), scan repos/worktrees |
| `resolver.go` | exported `Repos`/`WorktreesForRepo` accessors via the package-singleton seam (D6) |
| **Total** | **602** — `loc-git` cap **640** (measured + 5%, rounded up to 10); growth from 567 via symlink resolution in `expandRoot`, `worktreeUpdated` subscription entry, and design-review hardening (per-root error isolation with `errors.Join`, reconcile logging, stderr separation, `gitStore` testability seam). |

## Handoff

- Decisions D1–D11 above; ACs already implemented and traceable to the issue #29 v2 set.
- Cross-plugin join lives in `graph/git_map.go` + `graph/git.resolvers.go`, mirroring
  `graph/github_map.go` (github-query EDR D2/D3).
