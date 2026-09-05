# EDR: GitHub typed Query + cross-plugin join (the PRD first join)

Engineering Design Record. Owns the internals the [PRD](../PRD.md) §6 ("first cross-plugin
join key: issue# ↔ branch/worktree ↔ tmux pane ↔ claude session") leaves to the plugins.
Act-doc: decisions + numbered steps, not prose. Extends [github EDR](./github.md) (cache
grammar, list ops), reuses the [claude](./claude.md) / [tmux](./tmux.md) accessors.

**Goal (one sentence).** Make the PRD first join expressible in ONE GraphQL query by adding
typed **`issue` / `pullRequest` / `issuesForRepo`** Query fields served from the github plugin's
SQLite cache ONLY, plus **`Issue.{tmuxPanes,claudeSessions}`** and the same on `PullRequest`,
joined by branch name through a single convention shared across the three plugins.

**Non-goals.** No upstream hop from these fields (a miss is `null`/`[]`; warm stays the proxy
`/plugins/github/graphql` path). No cache-semantics change (TTL/pin/purge/reconcile untouched).
No core/ or server/ edit. No new webhook/subscription. No Todoist/email join (deferred). No
cross-repo `owner/repo#N` closing refs in v1 (owner-accepted default 2026-09-05; only same-repo
`#N` closes link — see D3).

---

## Decisions

**D1 — Reads are cache-only; the warm path is unchanged.**
- `Query.issue(key)` / `Query.pullRequest(key)` → `store.get(ctx, key)`; a miss returns `null`,
  **never** `resolve`/`httpGraphQL`. `key` is the full cache key (`issue:o/r#5`, `pr:o/r#7`) —
  the same grammar `keys.go` already parses; resolver rejects a wrong-kind key with `null`.
- `Query.issuesForRepo(owner, repo)` → the cached issue nodes under scope `repo:owner/repo`
  (new `store.nodesByKind`), mapped to `[Issue!]!`; a cold repo returns `[]`. Warming is still
  done by running the `openIssues` named op through the proxy (unchanged).

**D2 — The cross-plugin join lives in `graph/`, not in the plugins (Option i, recommended).**
`graph/` already imports every plugin, so the join resolver calls each plugin's **exported
accessor** — no plugin imports another (rejects Option ii coupling):
- github publishes `var current atomic.Pointer[Plugin]` set in `Migrate` (mirror
  `tmux.getCurrent`), backing exported `github.Issue/PullRequest/IssuesForRepo`.
- claude: `claude.QuerySessions(ctx, host, issue)` already exists (issue-number filter).
- tmux: `tmux.PaneForBranch(ctx, branch)` already exists (exact-branch filter).

**D3 — One join key, three plugins: a single shared branch→issue derivation.**
Owner 2026-09-05: the derivation is the **anchored** regex `^issue-?(\d+)([/-]|$)` — matches
`issue1/spike-core`, `issue-12`, `issue12-foo`; rejects `fix/issue3`, `myissue4`. Today
`claude/keys.go` owns a near-copy (`^issue-?(\d+)\b`); tmux never had a regex copy of its
own (it consumes branches through `tmux.PaneForBranch`/`tmux.Sessions`, deriving nothing),
so without a shared package the claude copy and this new github/graph derivation would
drift. Extract a **shared `internal/issuekey`** package exporting
`issuekey.FromBranch(branch) int`; point `claude` and `tmux` at it and delete the claude copy —
one regex, one source of truth. It lives at the **module-root `internal/`**, not
`plugins/internal/`: Go's internal rule would confine a `plugins/internal/` package to importers
rooted at `plugins/`, but `graph/` (outside `plugins/`) must import it too, so module-root
`internal/` is the only placement all three callers (`plugins/*`, `graph/`) can reach. `graph/`
imports it directly. (`<N>-slug` branch form is **deferred**, owner 2026-09-05.)
- `Issue.claudeSessions(N)` → `claude.QuerySessions(ctx, nil, &N)` (claude keys sessions by the
  derived issue number — consistent by construction).
- `Issue.tmuxPanes(N)` → branch set `B` = `{s.GitBranch | s ∈ claudeSessions(N)}` ∪
  `{t.Branch | t ∈ tmux.Sessions, issuekey.FromBranch(t.Branch) == N}` ∪
  `{pr.HeadRefName | pr ∈ cachedPRs(repo), issuekey.FromBranch(pr.HeadRefName) == N}` ∪
  `{pr.HeadRefName | pr ∈ cachedPRs(repo), N ∈ closesIssues(pr.body, pr.title)}`; return
  `⋃ tmux.PaneForBranch(b)` deduped by pane key. The tmux-session contributor (2nd term)
  is load-bearing for AC-GHQ-BRANCH-REGEX and AC-GHQ-JOIN-CONSISTENT, which seed only a
  pane (its backing tmux session carries the branch) and no claude session — see the
  Deviations note; the branch set is memoised per request so both `Issue.tmuxPanes` and
  `Issue.claudeSessions` compute it once (see D2 / Deviations).
- `closesIssues(body,title)` scans the cached PR **body+title** with the GitHub closing-keyword
  regex `(?i)(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+(#|[\w.-]+/[\w.-]+#)(\d+)` (data already cached
  once D5 selects `body`); it lives in `internal/issuekey.ClosingRefs`. A bare `#N` mention with no
  keyword does **not** link (AC-GHQ-MENTION-NOLINK). **Owner 2026-09-05 (accepted default):
  cross-repo `owner/repo#N` closing refs are ignored in v1** — the regex still matches them but
  `ClosingRefs` drops any ref whose prefix is not a bare `#`, so only same-repo `#N` closes link.
- `Issue.claudeSessions(N)` likewise unions sessions on any such PR branch.
- `PullRequest.tmuxPanes` → `tmux.PaneForBranch(headRefName)`.
- `PullRequest.claudeSessions` → `claude.QuerySessions(ctx, nil, &issuekey.FromBranch(headRefName))`.

**D4 — `Issue`/`PullRequest` bind to concrete Go structs (mirror `Health→core.HealthStatus`).**
Define `type Issue`/`type PullRequest` in `plugins/github/schema/github.graphqls` and bind each
to a hand-written struct `github.IssueNode` / `github.PRNode` via `gqlgen.yml` `models:` autobind.
Scalar fields (`number,title,state,url,updatedAt,labels[,headRefName,baseRefName]`) live on the
struct → no resolver. The join fields `tmuxPanes`/`claudeSessions` are **absent** from the struct,
so gqlgen generates `Issue`/`PullRequest` field-resolver stubs — implemented in
`graph/github_map.go` (the map-file pattern that survives follow-schema regen). Value returns,
`omit_slice_element_pointers` — same as every existing plugin field.

**D5 — Cached JSON must carry the new fields, so the warm queries select them.**
Today `queries/{issue,pr,openIssues,openPRs}.graphql` select only `number title state id`. Add
`url updatedAt labels(first:20){nodes{name}}` (+ `headRefName baseRefName body` for PR — `body`
is **required** so the closing-keyword scan in D3 has data; `title` is already selected). The node
JSON stored by the executor then carries them; `IssueNode`/`PRNode` map from that octokit JSON.
No key-extraction change (`hasAll` still keys on `{number}`).
- **Note (raw-proxy surface).** `body` now lands in the cached PR blob, so the raw
  `/plugins/github/graphql` proxy route returns the PR body **verbatim** to any caller
  that selects it — a surface that did not carry `body` before. This is not a new exposure
  boundary: that route is the same loopback-only, token-gated executor as before (a PR body
  is already readable by the PAT that fetched it), so no redaction is added; it is called out
  here only because the cached field set grew. The typed `Issue`/`PullRequest` GraphQL types
  deliberately do **not** expose `body` (D4) — it is cached for the D3 closing-keyword scan only.

**Alternatives considered.**
- (ii) github imports claude+tmux to self-join — rejected: cross-plugin coupling, and the join
  is a view concern that `graph/` is already positioned for.
- Let gqlgen fully generate `Issue`/`PullRequest` models — rejected: can't then map from cached
  JSON nor bind labels cleanly; the autobind path matches `Health` and keeps `graph/` logic-free.
- Join key = raw branch string end-to-end (no issue-number derivation) — rejected: claude keys
  sessions by issue number, so a number-based key reuses `QuerySessions` unchanged and stays
  consistent; branch is still the key for `tmux.PaneForBranch` where the exact ref is known.

**Risks.**
- gqlgen regen (`go generate ./...`) rewrites `graph/generated.go` + `graph/model` — fully
  regenerable, not a one-way door; CI must run it and the diff must be committed.
- Per-issue field resolvers: selecting the join fields inside `issuesForRepo` fans out
  N×(claude+tmux). Bounded (list `first:100`); the `issuesForRepo` AC does not select joins, and
  the p95 AC measures the single-issue join. Note in schema doc-comment.
- Convention: owner 2026-09-05 fixed the derivation to anchored `^issue-?(\d+)([/-]|$)` (shared
  `internal/issuekey`); the `<N>-slug` form is **deferred**. `labels` mapped as `[String!]!`
  names (not a `Label` type) to stay minimal.
- Closing-keyword false-positives: the regex `(?i)(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+...#(\d+)`
  is GitHub's own linking grammar; a bare `#N` mention must not link (AC-GHQ-MENTION-NOLINK guards it).

---

## Build plan (numbered, TDD; ZEROCORE + LOC gated)

1. **Failing feature first.** Land `features/github-query.feature` (below) with steps `@pending`.
2. **shared derivation**: add `internal/issuekey` (`FromBranch`, anchored regex
   `^issue-?(\d+)([/-]|$)`); repoint `claude/keys.go` at it, delete the claude copy (tmux
   had no copy to repoint). The closing-keyword scan lands in the same package as
   `internal/issuekey.ClosingRefs` (NOT `graph/`), so `graph/github_map.go` only calls it.
   `~28 LOC` (shared pkg = `FromBranch` + `ClosingRefs`, outside the github budget; own
   `make loc-issuekey` gate, cap 30).
3. **github/store.go**: add `nodesByKind(ctx, kind, scope) ([]*node, error)` (SELECT by typename
   + `key LIKE '<kind>:<scope>#%'`). `~12 LOC`.
4. **github/github.go**: `var current atomic.Pointer[Plugin]`; `current.Store(p)` in `Migrate`.
   `~3 LOC`.
5. **github/query.go (new)**: `IssueNode`/`PRNode` structs + `mapIssue`/`mapPR` (cached JSON →
   struct) + exported `Issue`/`PullRequest`/`IssuesForRepo` reading `current().store`. `~90 LOC`.
6. **github/queries/**: add `url updatedAt labels{...}` (+`headRefName baseRefName` for PR) to
   `issue,pr,openIssues,openPRs`. (`.graphql`, excluded from Go LOC.)
7. **github/schema/github.graphqls**: `type Issue`, `type PullRequest`, the 3 `extend type Query`
   fields, and the 4 join fields.
8. **gqlgen.yml**: bind `Issue→github.IssueNode`, `PullRequest→github.PRNode`. Run gqlgen.
9. **graph/github.resolvers.go** (regen stubs) + **graph/github_map.go (new)**: implement
   `Query.Issue/PullRequest/IssuesForRepo` (map plugin node→model) and the 4 join resolvers per
   D3. `~110 LOC in graph/` (not in the github budget; `generated.go` excluded).
10. **Un-`@pending`** the `@local` scenarios; keep `@live @pending` ones pending.
11. **LOC gate**: rerun `make loc-github`; move the cap to **measured + 5% rounded up to 10 = 1570**
    (measured **1486**) in the [github EDR](./github.md) budget table — one ratchet line, with the
    per-file attribution. The shared `internal/issuekey` (28 LOC) gets its own
    `make loc-issuekey` gate (cap 30) + CI step, outside the github budget.
12. **ZEROCORE gate**: `git diff --stat core/ server/` is empty; `gqlgen.yml` + `graph/` +
    `plugins/**` only.

**LOC actual.** `plugins/github/**` prod landed at **1466** (typed reads + join accessors;
cap 1540), then the security review added **+20** → **1486** (`store.go` `escapeLike` + escaped
`nodesByKind` +5 · `query.go` `safeKey` + owner/repo validation + per-request `issueMemo` +15 ·
`github.go` `single.Ptr` swap net 0). New cap **measured + 5% rounded up to 10 = 1570**. `graph/`
(outside budget): `github_map.go` (join helpers — branch set memoised per request, claude read
leads with `QuerySessions(&N)`) + generated stubs. claude: `keys.go` repointed to `issuekey`
(net ~-6). shared `internal/issuekey`: 28 (own gate, cap 30; `FromBranch` + `ClosingRefs`, both
unit-tested in `issuekey_test.go` to 100%).

---

## Deviations (as-built vs. this EDR)

Recorded where the shipped code diverges from or refines the decisions above, so a later
reader trusts the code over a stale sentence.

1. **`internal/issuekey` sits at the module root, not `plugins/internal/`.** Go's internal
   rule confines a `plugins/internal/` package to importers rooted at `plugins/`, but
   `graph/` (outside `plugins/`) must import the derivation too. Module-root `internal/` is
   the only placement all callers (`plugins/claude`, `plugins/tmux` seeding, `graph/`) reach.
   It carries **both** `FromBranch` and `ClosingRefs` — the closing-keyword scan lives here,
   NOT in `graph/` as build-plan step 2 first sketched; `graph/github_map.go` only calls it.
2. **The branch set has a 4th contributor: tmux sessions whose branch derives to N.** D3's
   formula (now updated) lists it as the 2nd term. It is load-bearing: AC-GHQ-BRANCH-REGEX
   and AC-GHQ-JOIN-CONSISTENT seed only a pane (whose backing tmux session carries the
   branch) and no claude session, so without this term those panes would not attach.
3. **tmux never had its own regex copy.** D3's original wording implied all three plugins
   held a derivation; tmux consumes branches through `tmux.PaneForBranch`/`tmux.Sessions`
   and derives nothing. Only `claude/keys.go` was repointed at `issuekey` (its copy deleted);
   tmux had nothing to repoint. (D3 wording corrected.)
4. **The @local test server enables `[plugins.claude]`.** The join reads claude sessions
   through the running claude plugin (`claude.QuerySessions`), so `features/github_helpers_test.go`
   configures `[plugins.claude]` at an empty `projectsDir` with a long scan interval — harmless
   to the github-only scenarios. tmux is left unconfigured on purpose: its `Migrate` still
   publishes its accessor (so seeded panes read back) but its reconcile loop stays dormant and
   never clobbers a seeded row.
5. **The branch set is memoised per request; the claude read leads with `QuerySessions(&N)`.**
   Both `Issue.tmuxPanes` and `Issue.claudeSessions` compute the branch set once, via a
   `sync.Once`-guarded `issueMemo` hung off `IssueNode` (a *pointer* field, so a value copy of
   `IssueNode` in `IssuesForRepo` does not trip vet copylocks). `claudeSessionsForIssue` leads
   with the issue-number-filtered `QuerySessions(ctx, nil, &N)`, then adds sessions sitting on a
   linked branch the join reached another way (a PR-closes branch). That remainder is still a
   membership filter over the session list rather than a per-branch query: claude exposes **no**
   branch filter and its `issue_number` is independently settable (`plugins/claude/redact.go`),
   so a per-branch `QuerySessions` would not be behaviour-identical. Removing that scan is
   deferred to a future claude branch accessor.
6. **Security-review fixes (S1) hardened the cache-only reads.** `nodesByKind` escapes the LIKE
   metacharacters in its scope (`escapeLike` + `ESCAPE '\'`) so a repo named `a_b` cannot match
   `axb` and a `%` scope cannot match every repo; `IssuesForRepo`/`CachedPRs` reject an
   unsafe owner/repo via `safeName`, and `Issue`/`PullRequest` reject an unsafe key scope via
   `safeKey`, returning nil/`[]` rather than driving a lookup. See the D5 raw-proxy note for the
   PR-`body` surface. The `current` accessor was aligned to `plugins/internal/single.Ptr` to
   match claude/tmux/peer.

## AC draft
<!-- ACs ready for ac-reviewer -->

Sharp per [acceptance-criteria](../../../.knowledge/modules/shared/records/principles/acceptance-criteria.md):
each is testable, falsifiable, names its evidence. `@local` = fakegh + fake claude/tmux stores,
no PAT; `@live @pending` = needs a real PAT/GitHub.

- **AC-GHQ-HIT** — `issue(key)` on a node already in the store returns the complete node with
  **zero** fakegh requests recorded during the query. Evidence: fakegh request counter == 0.
- **AC-GHQ-MISS** — `issue(key)` / `pullRequest(key)` on an absent key returns `null` with **zero**
  fakegh requests. Evidence: result is JSON `null`; fakegh counter == 0. (Negative control for HIT.)
- **AC-GHQ-LIST** — `issuesForRepo(owner,repo)` after the `openIssues` op is warmed returns exactly
  the warmed issues, **zero** upstream during the read; a never-warmed repo returns `[]`. Evidence:
  returned `number` set == warmed set; fakegh counter == 0 on the read.
- **AC-GHQ-HEADREF** — `pullRequest(key).headRefName` is present and equals the cached PR's branch.
  Evidence: field non-empty, equals fixture branch.
- **AC-GHQ-JOIN-HIT** — for issue #N whose branch (`issueN/...`) matches a live tmux pane and a
  claude session, the one-query join returns that pane (via `tmuxPanes`) **and** that session (via
  `claudeSessions`). Evidence: pane key + session id both present in one response.
- **AC-GHQ-JOIN-EMPTY** — for issue #N with no matching branch anywhere, `tmuxPanes` and
  `claudeSessions` are both `[]` (never omitted, never error). Evidence: both arrays empty in the
  response. (Negative control for JOIN-HIT.)
- **AC-GHQ-JOIN-CONSISTENT** — one shared derivation (`issuekey.FromBranch`) drives both the claude
  filter and the tmux branch set: a pane and a session on a **boundary** branch (`issue12-foo`, the
  no-slash form) both attach to issue #12 — a form the two plugins can only agree on if they share
  the regex. Evidence: both sides present in one response for the boundary fixture.
- **AC-GHQ-BRANCH-REGEX** — the shared regex `^issue-?(\d+)([/-]|$)` attaches a pane for
  `issue1/spike-core`,`issue-12`,`issue12-foo` and **never** for `fix/issue3`,`myissue4`. Evidence:
  scenario-outline pass — tmuxPanes contains/omits the pane per row.
- **AC-GHQ-PR-CLOSES** — a cached PR whose **body** closes #N (GitHub closing-keyword regex)
  attaches that PR's branch pane+session to issue #N even when the branch has no `issue-?N` prefix.
  Evidence: pane key + session id present for #N in one response.
- **AC-GHQ-MENTION-NOLINK** — a cached PR body with a bare `#N` mention and **no** closing keyword
  does not link its branch to #N. Evidence: tmuxPanes empty. (Negative control for PR-CLOSES.)
- **AC-GHQ-JOIN-VIA-PR-HEADREF** — a PR whose headRefName (`feature/widget`) derives to **no**
  issue but whose body closes #N attaches that PR's head-branch pane **and** session to #N. Because
  the branch self-derives to nothing, neither the tmux-session nor the claude-issue-number
  contributor can attach it — only the PR head-branch contributor (reached by the closing-keyword
  scan of the body) adds it. Evidence: pane key + session id present for #N. (Isolates the PR
  head-branch contributor from AC-GHQ-JOIN-HIT, where the branch self-derives and the pane's own
  tmux session + the claude session each supply it directly.)
- **AC-GHQ-P95** — the one-query PRD join, run as the exact query
  `{ issue(key:"issue:o/r#5"){ number tmuxPanes{key} claudeSessions{sessionId} } }`, has **p95 < 1s**
  over **N=20 paired** samples on a warm cache. Evidence: 20 paired latencies, p95 line logged.
- **AC-GHQ-ZEROCORE** — `git diff --stat core/ server/` is empty after the change. Evidence: empty
  diffstat in CI.
- **AC-GHQ-LOC** — `make loc-github` passes against the ratcheted cap (measured + 5%). Evidence:
  the loc-github step exits 0; the EDR budget line shows the new measured total + cap.
- **AC-GHQ-LIVE-WARM** `@live @pending` — against real GitHub, running `openIssues` then
  `issuesForRepo` returns the same issues the REST API lists. Evidence: parity screenshot.

## Handoff
- ACs ready for ac-reviewer (see §AC draft above).
- Implementation → coder (steps 2–4, 6 are mechanical → fast-coder; steps 5, 9 need judgment →
  coder), per ~/.knowledge/modules/shared/records/model-selection.md.
