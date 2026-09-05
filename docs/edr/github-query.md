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
No core/ or server/ edit. No new webhook/subscription. No Todoist/email join (deferred). No PR
body/title `#N` scan in v1 (see Owner question).

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
`claude/keys.go` owns a near-copy (`^issue-?(\d+)\b`) and tmux has no derivation at all, so the
three would drift. Extract a **shared `plugins/internal/issuekey`** package (the `pluginconfig`
PR is already creating `plugins/internal/`) exporting `issuekey.FromBranch(branch) int`; point
`claude` and `tmux` at it and delete the claude copy — one regex, one source of truth. `graph/`
imports it directly. (`<N>-slug` branch form is **deferred**, owner 2026-09-05.)
- `Issue.claudeSessions(N)` → `claude.QuerySessions(ctx, nil, &N)` (claude keys sessions by the
  derived issue number — consistent by construction).
- `Issue.tmuxPanes(N)` → branch set `B` = `{s.GitBranch | s ∈ claudeSessions(N)}` ∪
  `{pr.HeadRefName | pr ∈ cachedPRs(repo), issuekey.FromBranch(pr.HeadRefName) == N}` ∪
  `{pr.HeadRefName | pr ∈ cachedPRs(repo), N ∈ closesIssues(pr.body, pr.title)}`; return
  `⋃ tmux.PaneForBranch(b)` deduped by pane key.
- `closesIssues(body,title)` scans the cached PR **body+title** with the GitHub closing-keyword
  regex `(?i)(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+(#|[\w.-]+/[\w.-]+#)(\d+)` (~20 LOC, data
  already cached once D5 selects `body`). A bare `#N` mention with no keyword does **not** link
  (AC-GHQ-MENTION-NOLINK).
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
  `plugins/internal/issuekey`); the `<N>-slug` form is **deferred**. `labels` mapped as `[String!]!`
  names (not a `Label` type) to stay minimal.
- Closing-keyword false-positives: the regex `(?i)(close[sd]?|fix(e[sd])?|resolve[sd]?)\s+...#(\d+)`
  is GitHub's own linking grammar; a bare `#N` mention must not link (AC-GHQ-MENTION-NOLINK guards it).

---

## Build plan (numbered, TDD; ZEROCORE + LOC gated)

1. **Failing feature first.** Land `features/github-query.feature` (below) with steps `@pending`.
2. **shared derivation**: add `plugins/internal/issuekey` (`FromBranch`, anchored regex
   `^issue-?(\d+)([/-]|$)`); repoint `claude/keys.go` + `tmux` at it, delete the claude copy.
   Closing-keyword scan `closesIssues(body,title)` lands in `graph/` (~20 LOC). `~30 LOC` (shared
   pkg + scan, outside the github budget).
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
11. **LOC gate**: rerun `make loc-github`; move the cap to **measured + 5%** (est. ~1525) in the
    [github EDR](./github.md) budget table — one ratchet line, owner-approved by this EDR.
12. **ZEROCORE gate**: `git diff --stat core/ server/` is empty; `gqlgen.yml` + `graph/` +
    `plugins/**` only.

**LOC estimate.** `plugins/github/**` prod: **~+105** (store 12 + wiring 3 + query.go 90) →
**~1450** (from 1345). New cap **measured + 5% ≈ 1525**. `graph/` (outside budget): ~+110 hand +
generated. claude: +2.

---

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
- **AC-GHQ-JOIN-VIA-PR-HEADREF** — a PR whose headRefName derives to #N attaches its pane via the
  PR-headref branch-set contributor with **no** claude session feeding the branch. Evidence: pane
  key present for #N. (Isolates the PR-headref path from AC-GHQ-JOIN-HIT.)
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
