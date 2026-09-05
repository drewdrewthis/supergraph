# EDR: GitHub plugin — event-invalidated caching proxy

Engineering Design Record. Owns internals the [PRD](../PRD.md) §6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.

**Live design (2026-09-05 owner reframe + GO on Option C).** The github plugin is a **lazy,
event-invalidated caching proxy** in front of GitHub REST + GraphQL — **not** a model of GitHub. No
eager baseline, no vendored full SDL. It fetches on miss, caches parsed JSON nodes keyed by **object
id**, honors **ETag/304** (free reads), **pins immutable nodes**, and lets **webhook events purge
exactly the entries that touch the changed object (tag purge = keyed delete)**. Prior art
`brunoborges/ghx` was read in full (see [History](#history)); it is **not vendored** — we borrow only
its singleflight pattern (~40 LOC, `ghx src/internal/daemon/handler.go:203`).

**Hard constraints (owner):** ≤ **1570 LOC prod** (raised 800→1300→1350→1540→1570, Option A) for `plugins/github/**` excluding tests and
`internal/fakegh` (per-package budget below); zero core diff; PAT-only; HMAC per hook; per-repo hook
creation from `/user/repos` (F3); point-budget floor pause + rate-limit logging; CLI `--op/--var` +
`schema <Type>` in `cmd/` only; fakegh httptest server + fake `gh` stub for `@local`.

---

## LOC budget (prod, cap **1570** total; tests + `internal/fakegh` excluded)
Cap raised 800→1300→1350→1540→**1570** by owner (Option A, 2026-09-05). The
1350→1540 ratchet paid for the typed cache-only reads + the PRD cross-plugin join
([github-query EDR](./github-query.md)): `query.go` (typed `IssueNode`/`PRNode` +
`mapIssue`/`mapPR` + exported `Issue`/`PullRequest`/`IssuesForRepo`/`CachedPRs`
accessors), `store.go`'s `nodesByKind`, and `github.go`'s `current` accessor pointer
(measured 1466). The 1540→1570 ratchet paid for the security-review fixes:
`nodesByKind`'s LIKE-escaping + `IssuesForRepo`/`CachedPRs`/`Issue`/`PullRequest`
owner/repo validation (`safeName`/`safeKey`, S1), the per-request join memo on
`IssueNode`, and the `single.Ptr` alignment of `current`.
New cap = measured **1486** + 5% rounded up to 10. Table shows **Actual** LOC (EDR
strip formula), not aspirational budgets.
| Package/file (`plugins/github/`) | Actual | Responsibility |
|---|---:|---|
| `github.go` | 151 | plugin wiring: `Register`/`New`/`Name`/`Migrate`/`Start`/`HTTPRoutes`/`CursorReporter`/config + `current` accessor pointer (github-query D2) |
| `keys.go` | 185 | key grammar as a **data table** (`kindSpecs`): parse + object→key + event→key + REST path + typename + `safeName` (S1) |
| `client.go` | 106 | shared GitHub HTTP + rate-limit layer: REST/GraphQL calls, auth, ratelog, floor-pause |
| `store.go` | 236 | SQLite `github_nodes`+`github_tags`+`github_hooks`+`github_deliveries`: upsert, get, purge, tag index, hooks, deliveries prune, non-pinned scan, pin, `nodesByKind` + `escapeLike` LIKE-escaping (github-query, S1) |
| `query.go` | 146 | typed cache-only reads (github-query D4/D5): `IssueNode`/`PRNode` (+ per-request join `issueMemo`) + `mapIssue`/`mapPR` + `safeKey` (S1) + exported `Issue`/`PullRequest`/`IssuesForRepo`/`CachedPRs` |
| `proxy.go` | 118 | read-through resolve: miss→fetch→store→serve; ETag/304; **singleflight (borrowed ~40)**; pin eval |
| `webhook.go` | 63 | HMAC verify (1 MiB body cap, S2); event→key; purge; emit envelopes; delivery dedup |
| `executor.go` | 255 | JSON-backed GraphQL executor + named-op loader + declared-key scoping (point/list) + list read path (U2) + body cap (S2) + parsed introspection allowlist (raw-query guard hardening) |
| `ingest.go` | 116 | `gh webhook forward` supervisor (backoff) + redelivery + `/notifications` poll (flag) |
| `reconcile.go` | 110 | discovery `/user/repos` + hook creation + since-cursor + revalidation (P1) + deliveries prune (S3) |
| **Total** | **1486** | cap **1570**; CLI delta in `cmd/supergraph/` (~70) is counted separately, not in this budget |
- **AC-GH-LOC / AC-GHQ-LOC** guard it: a CI step (`make loc-github`) runs `find plugins/github -name '*.go' ! -name '*_test.go' -not -path '*/fakegh/*' | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$/d' | wc -l` and fails > 1570 (POSIX `[[:space:]]`, portable across GNU/BSD sed). The shared `internal/issuekey` package (28 LOC) has its own **`make loc-issuekey`** gate (cap 30), outside this budget.
- The `strOr`/`boolOr`/`intOr`/`toInt`/`readErrStatus` config helpers moved to `plugins/internal/pluginconfig` (−39 from the pre-migration 1350 baseline); the typed `Query.issue`/`pullRequest`/`issuesForRepo` reads plus the cross-plugin join accessors (`query.go`, `nodesByKind`, `current`) added the github-query surface (**1466 measured**, cap 1540); the security-review fixes (LIKE-escaping, owner/repo validation, join memo, `single.Ptr`) then landed at **1486 measured**; cap = 1486 × 1.05 rounded up to 10 = **1570** (owner rule).

## Cache key = object id (full grammar)
Every cached node and every purge target is one canonical key. `@<hostId>` suffix is **optional**,
elided for `github.com`, present only for GHES.
```
key      := <kind>":"<scope>[ "#"<number> | "/"<id> ] [ "@"<hostId> ]
scope    := <owner>"/"<repo>            # except user/org kinds
issue    := "issue:"   <owner>/<repo> "#" <number>
pr       := "pr:"      <owner>/<repo> "#" <number>
checkRun := "checkRun:"<owner>/<repo> "/" <checkRunId>
review   := "review:"  <owner>/<repo> "#" <prNumber> "/" <reviewId>
comment  := "comment:" <owner>/<repo> "#" <number> "/" <commentId>
label    := "label:"   <owner>/<repo> "/" <name>
ref      := "ref:"     <owner>/<repo> "/" <refName>
release  := "release:" <owner>/<repo> "/" <tag>
commit   := "commit:"  <owner>/<repo> "/" <sha>
repo     := "repo:"    <owner>/<repo>
user     := "user:"    <login>
```
- **Tag index.** Every stored entry (single node OR a named-op list result) carries a **tag set**:
  its own key plus its **scope prefix** (`repo:<owner>/<repo>`) and **kind** (`issue`,`pr`,…). Purge
  takes a key and deletes (a) the exact-key entry and (b) any list-result entry whose declared scope
  covers that key's `(scopePrefix, kind)`. This makes webhook `issue:o/r#5` evict the `#5` node **and**
  every cached `openIssues(repo=o/r)` result — exactly, no over-purge across kinds, no miss on lists.

## Read-through + ETag/304 (per node)
Each `github_nodes` row stores `node_json`, **`etag`**, `fetched_at`, `updated_at`, and `pinned`.
On a query that resolves to key K:
1. **Hit, fresh/pinned** → serve stored node, **no upstream call** (F7 mechanism; AC cache-hit).
2. **Miss / expired (and not pinned)** → **one anchored fetch** (REST `GET` by id, or the op's GraphQL)
   sending `If-None-Match: <stored etag>` when we have one:
   - **304** → serve stored node, bump `fetched_at`, **zero quota** (AC ETag-304).
   - **200** → store `node_json`+`etag`+`fetched_at`, emit `github.node.updated`, serve.
3. **Singleflight** coalesces N concurrent misses on K into **one** upstream fetch (borrowed pattern;
   AC singleflight).

## Freshness contract (honest)
This is a cache, not a mirror — freshness is **event-driven, not time-driven**. A **non-pinned mutable
node** (open issue/PR, check run, label, ref) is **served from cache until an event purges it**: a
webhook, a `/notifications` change (if enabled), or a since-cursor reconcile. Between events the node
**can be stale**, and that is by design.
- **Worst-case staleness window = the reconcile interval** (`reconcileIntervalSeconds`, default **3600
  s / 1 h**) — the guaranteed upper bound when webhooks and notifications both miss, holding for **every
  non-pinned mutable kind**, not just open issues. Each reconcile pass revalidates every cached
  non-pinned node against upstream via a conditional `GET` (`If-None-Match`): a **304** costs zero body
  quota and only bumps `fetched_at`; a **200** upserts the changed node and emits `github.node.updated`.
  A list-discovered node carrying an empty etag simply does a plain `GET` and stores the returned etag.
  Webhooks/redelivery normally purge in sub-second (S3/F1); reconcile revalidation is the floor.
- **Optional per-kind TTL** (`[plugins.github.ttl]`, e.g. `checkRun = 30`, default **off**) caps
  staleness for a churny kind by expiring it early so the next read conditionally refetches (ETag/304,
  cheap). Off by default so steady-state stays at zero quota (F7).
- **Pinned nodes** (immutable, §below) are exempt — never stale in a way that matters.
The negative control **AC-GH-STALE** proves this honestly: a node mutated upstream with **no event
arriving** is served stale until reconcile heals it.

## Immutable pin list (config, explicit defaults)
Pinned nodes are **never TTL-expired and never conditionally refetched** (a webhook can still force-evict
them). `[plugins.github.pin]`:
```
commits            = true   # commit:*  — immutable by sha
releases           = true   # release:* — immutable by tag
mergedPRsAfterDays  = 7     # pr:*  closed/merged  > 7d ago  → pinned
closedIssuesAfterDays = 30  # issue:* closed        > 30d ago → pinned
```
Proposed defaults above; `mergedPRsAfterDays`/`closedIssuesAfterDays` give recently-closed objects a
grace window (late edits/comments) before pinning.

## Named operations scope every GraphQL read to object ids
`plugins/github/queries/*.graphql` (~10: `openIssues`, `issue`, `pr`, `prsAwaitingReview`,
`myClaimed`, `openPRs`, `checkRunsForPR`, `issueComments`, `repoLabels`, `paneForBranch` **@pending**).
Each op **declares the keys it touches** in a header directive the executor parses:
```graphql
# op: openIssues
# scope: repo:{owner}/{repo}      # list op → result tagged with this prefix + kind issue
# keys:  issue:{owner}/{repo}#{number}
query openIssues($owner:String!,$repo:String!){ ... }

# op: issue
# keys:  issue:{owner}/{repo}#{number}   # point op → exactly one key
query issue($owner:String!,$repo:String!,$number:Int!){ ... }
```
- **Point op** (`# keys:` resolves to concrete keys from vars) → executor resolves each key via
  read-through (§above) and serves the stored nodes; result is uncached or tagged by its keys.
- **List op** (`# scope:`) → executor runs the op's GraphQL against GitHub on miss, stores each node it
  returns, caches the **list result** tagged with the scope prefix; a later webshook purge covering that
  scope evicts the list result too. **AC named-op-scoped-keys** proves a purge of one contained id
  evicts the list result.

## Envelopes emitted (`{TS,Source:"github",Type,V:1,Key,Payload}`)
These are what `/health` `lastEventAt` derives from and what tmux/claude join on:
- **`github.node.updated`** — `Payload={key,etag,typename}` — a node was upserted (webhook, read-through 200, or reconcile revalidation 200).
- **`github.node.purged`** — `Payload={key}` — a node/entry evicted by tag purge; an accepted webhook emits exactly this one envelope, and `/health` `lastEventAt` / S3 timing derive from it.
Cross-plugin joins key on `github.node.updated.Key` (e.g. `issue:o/r#5`). There is deliberately **no**
`github.webhook.received` envelope — an accepted webhook emits only the purge (AC-GH-HMAC: "exactly one github event is emitted").

## Auth — PAT only
Env `GITHUB_TOKEN` or `[plugins.github].token` (scope `repo`; 5000 REST/h, 5000 GraphQL points/h). No
App, no JWT. PAT never persisted; passed to fetches in-process.

## Ingest = webhooks (push), supervised
- **`forward` (default):** the plugin supervises a **`gh webhook forward` child process** with
  **exponential backoff restart**. It streams deliveries over an outbound websocket, POSTing each to
  `http://127.0.0.1:7788/plugins/github/webhook` — no inbound ingress. On start/restart it runs
  **redelivery from the last-seen delivery id** (`GET /repos/{o}/{r}/hooks/{id}/deliveries` →
  `POST .../deliveries/{id}/attempts`), deduped by `X-GitHub-Delivery`.
- **`tunnel`:** operator points a Funnel URL at the same handler; plugin creates one repo hook per
  discovered repo (`POST /repos/{o}/{r}/hooks`), each with its own HMAC secret.
Handler verifies `X-Hub-Signature-256` (`sha256=`+hex HMAC-SHA256) with **`hmac.Equal`**; bad/absent →
**401**, no emit. Good → map `X-GitHub-Event`+`action` to a node key, **upsert or purge**, emit, 200.

## Correctness paths (webhooks are latency-only)
1. **Since-cursor reconcile** (default hourly) — GraphQL pull per repo, advances `since` with a **60 s
   overlap** (`since-60s`). Primary drop-heal guarantee.
2. **Boot/restart redelivery** — replays undelivered/failed delivery ids (above). Faster heal.
3. **`/notifications` secondary, behind `notifications = true`** — `GET /notifications` with
   `If-Modified-Since`; **304 = zero quota**, poll at `X-Poll-Interval`. Off by default.
- **F2 heal window:** a dropped webhook is guaranteed present after the **next since-cursor reconcile
  (≤ one reconcile interval)**; redelivery and (if enabled) `/notifications` heal it sooner.

## Cold start — no baseline (owner: NO backfill)
There is **no bulk backfill**. An empty cache means **every first read is a read-through miss served by
one anchored fetch** (§Read-through), then cached. Cold start is therefore bounded by **per-read
latency**, not a 60-minute bulk job: first read of each named op completes in **< 2 s** against fakegh
and **populates exactly that op's declared keys** — nothing more (AC-GH-COLDSTART).
> **PRD delta (owner/peer to apply):** PRD F2's "**full baseline backfill from empty db < 60 min**"
> clause is obsolete under the caching-proxy design and should be **reworded** to "cold cache serves
> each read via one read-through fetch; no bulk backfill." Tracked here; PRD edit is owner/peer's.

## Discovery (F3)
`GET /user/repos?affiliation=owner&per_page=100` (paginated). A repo appearing after boot is picked up
next reconcile with **zero config** and gets a hook created (forward/tunnel).

## Rate-limit discipline (F7)
Every response's REST `x-ratelimit-*` and GraphQL `rateLimit{remaining,resetAt}` are **logged**. Near
the points floor the reconcile/read-through **pauses until `resetAt`**, never hard-fails; `/health`
shows no forced-stale. Cache hits + 304s keep steady-state usage under budget.

## CLI (`cmd/` only, core untouched)
`cmd/supergraph/query.go`: add `--op` (load named `.graphql`) + repeatable `--var k=v`, target
`/plugins/github/graphql`; add `schema <Type>` subcommand printing the served type. `git diff --stat
core/` = 0 (github serves via the **`HTTPRoutes`** seam, not the gqlgen glob).

## SQLite state (add-only, `IF NOT EXISTS`)
- `github_nodes(key TEXT PK, typename, node_json, etag, pinned INT, fetched_at, updated_at)` — **key-only**; owner/repo/number are parsed from the key, never stored as columns
- `github_tags(tag TEXT, key TEXT, PK(tag,key))`   — tag index for scope/kind purge
- `github_hooks(owner,repo,hook_id, PK(owner,repo))` — **no `secret` column** (S4): the secret lives only in config, never persisted to disk
- `github_deliveries(delivery_id, seen_at, PK(delivery_id))`
- cursors live in core's `cursors` table (`since:<o>/<r>`, `notif:lastModified`, `hook:<o>/<r>:lastDeliveryId`).

## Config keys `[plugins.github]`
`token` (or env), `ingress` (`forward`|`tunnel`), `tunnelURL`, `webhookSecret`, `owner`,
`reconcileIntervalSeconds` (3600 — also the worst-case staleness window), `notifications` (bool,
default false), `[plugins.github.pin]` (above), `[plugins.github.ttl]` (per-kind seconds, default off),
`baseURL` (test-only → fakegh).

**Single `webhookSecret` (approved deviation, 2026-09-05):** one secret verifies *all*
hooks and *all* redelivered events, rather than a per-hook secret. This assumes every
hook the plugin manages lives on **one owner's repos** (the same owner discovered via
`/user/repos`) — a safe assumption for the single-tenant, one-worker-per-box v1. If a
future version manages hooks across multiple owners/orgs, this must become a per-hook
secret keyed by repo, or cross-owner events will fail HMAC verification.

**`webhookSecret` is argv-visible on the `forward` ingress (S5):** the supervisor spawns
`gh webhook forward --secret <webhookSecret>`, so the secret appears in the process list
(`ps`, `/proc/<pid>/cmdline`) to any local user while the child runs. Accepted for the
single-operator v1 box; a multi-tenant host would pass the secret via env or stdin instead.

**Single `github.node.purged` per accepted webhook (approved deviation):** an accepted
webhook emits exactly one envelope (the purge), not a separate `github.webhook.received`
— the feature contract (AC-GH-HMAC: "exactly one github event is emitted") governs.

## Failure modes
- Webhook transport down → reconcile + redelivery heal.
- HMAC mismatch → 401, logged, no emit.
- GraphQL points floor → pause to `resetAt`, never hard-fail.
- `gh webhook forward` exits → backoff restart + redelivery.
- `/notifications` 304 spam → honor `X-Poll-Interval`, zero quota (only when enabled).
- `since` clock skew → 60 s overlap.
- Repos we cannot hook → `/notifications` path only (if enabled) + reconcile.

---

## Acceptance criteria
PRD ACs (verbatim, github scope, p95 over N≥20 where stated) + plugin ACs are the BDD contract in
`features/github.feature`. Summary: **S3, F1, F2, F3, F7** (PRD) + **cache-hit-no-upstream-call (etag-checked),
stale-until-reconcile (negative control), cold-start-no-baseline,
ETag-304-zero-quota, purge-by-tag-evicts-exact-keys, immutable-pinned-never-refetched,
singleflight-coalesces-N-into-1, HMAC-401, forward-child-restart-with-redelivery,
notifications-304-behind-flag, floor-pause, ratelog, named-op-scoped-keys, cursor-persist,
LOC-budget, zerocore**. `@local` runs against fakegh + fake `gh` stub; live proofs are `@live
@pending`.

## AC review: applied
Prior ac-reviewer Must-Fixes folded in:
1. **Honest freshness contract** — added §Freshness contract: non-pinned mutable nodes are served from
   cache until an event purges them; worst-case staleness = reconcile interval (default 1 h); optional
   per-kind `ttlSeconds` (default off). Added negative-control **AC-GH-STALE** (a node mutated upstream
   with no event is served stale until reconcile) and made **AC-GH-CACHE-HIT falsifiable** (asserts
   zero upstream calls **and** served etag == stored etag).
2. **Cold start, no baseline** — reinterpreted PRD F2's "backfill < 60 min": no bulk backfill; empty
   cache → every read is one read-through fetch. Added **AC-GH-COLDSTART** (first read of each named op
   < 2 s, populates exactly the declared keys) and a PRD-delta note (owner/peer reword PRD F2).
Earlier App-based AC items remain superseded by the PAT/webhook/caching-proxy design.

---

## Coder step plan — two file-partitioned waves (no shared-file contention)

**Wave 1 — proxy + plugin + CLI** (fakegh first; each file owned by one coder task):
1. `plugins/github/internal/fakegh/` — httptest GitHub: GraphQL (paginated + `rateLimit`), REST
   `/user/repos`, `/repos/*/hooks`+`/deliveries`+`/attempts`, `/notifications` (304), ETag/`If-None-Match`
   echo, settable rate fields; **fake `gh` stub binary** (`internal/fakegh/cmd/gh`). *Everything tests against it.*
2. `plugins/github/keys.go` + `store.go` + `Migrate` (the four tables) — key grammar + node/tag store.
3. `plugins/github/proxy.go` — read-through + ETag/304 + singleflight (borrow `ghx handler.go:203`) + pin eval.
4. `plugins/github/webhook.go` — HMAC + event→key + upsert/purge + emit envelopes.
5. `plugins/github/executor.go` — JSON-backed executor + named-op loader (`# op/# scope/# keys`) + key scoping.
6. `plugins/github/ingest.go` + `reconcile.go` — forward supervisor+backoff, redelivery, `/notifications`
   (flag); discovery, hook create, since-cursor, floor-pause, ratelog.
7. `plugins/github/github.go` — wiring (`Register`/`Start`/`HTTPRoutes`/`CursorReporter`); blank import in
   `graph/plugins_import.go`; `plugins/github/queries/*.graphql`.
8. `cmd/supergraph/query.go` — `--op`/`--var` + `schema <Type>` (cmd/ only).
9. `make loc-github` CI step (AC-GH-LOC) + `git diff --stat core/`=0 check (AC-GH-ZEROCORE).

**Wave 2 — godog steps** (wires `features/github.feature` to wave-1 code; own files):
10. `features/steps_github_test.go` — Given/When/Then over fakegh + signed webhook POST + forward stub;
    p95 harness for S3/F1 (N≥20); LOC-budget step; purge/singleflight/304/pin assertions.
11. `features/prd.feature` edit (remove github-owned S3/F2/F3/F7; F1→claude-only) already applied.

Wave 1 tasks 2–8 are file-disjoint and parallelizable after task 1; task 3 (proxy) is the judgment-heavy
core. Wave 2 depends on Wave 1 compiling.

---

## History (superseded 2026-09-05 by the caching-proxy reframe)
Kept for provenance; **do not implement**:
- **Vendored full GitHub SDL (64k lines, 1636 types) served as whole nodes** — replaced by a **lean
  schema** covering only the types named ops touch; the subset-CI check is dropped.
- **Whole-node store with a three-tier baseline** — the whole-JSON-node store **substrate stays**, but
  the **eager per-repo baseline (tier a) is dropped**: read-through on miss + webhook upsert only.
- **`gqlgen` codegen measurement** — moot without a full SDL; the JSON-backed dynamic executor stays.
- Earlier App-based ingest ideas — superseded by PAT-only + `gh webhook forward` (still current).

### Appendix — executor bench (retained; still justifies "no gqlgen codegen")
Bench in `/tmp/sg-sdl-bench` (deduped SDL, 1636 types, go1.26):

| Approach | Codegen | Generated source | Build/binary | Startup |
|---|---|---|---|---|
| **gqlgen codegen** | **FAILS on stock SDL** (`… Query does not satisfy Node`) after emitting **46,089 lines / 1.71 MB of models** in ~5.2 s | 1.7 MB models + ~1600 resolver stubs | multi-MB binary delta | n/a |
| **JSON-backed (gqlparser AST + one dynamic executor)** | none | **zero generated code** | **+3.26 MB** dep | **~29 ms** to load+validate the full SDL |

The lean-schema reframe only reinforces the JSON-backed pick: far fewer types, still zero codegen.

## Handoff
- ACs ready for ac-reviewer (see `features/github.feature`, rewritten to the cache-proxy contract).
- Implementation → coder per the two-wave plan above; proxy.go (Wave 1 step 3) is the judgment-heavy core.

---

## AC review

**Must-Fix**
1. **No negative-control / bounded-staleness AC, and no TTL defined for non-pinned mutable nodes.** `github_nodes` has no `ttl`/`maxAge` config key — only pin grace windows + `reconcileIntervalSeconds`. So "fresh" in read-through step 1 and in AC-GH-CACHE-HIT ("already in the store, fresh") is **not falsifiable**. The honest downside of a caching proxy is unstated: with no webhook and no reconcile yet, a query serves a node **stale vs GitHub up to one reconcile interval**. Define the freshness window (a TTL, or "always 304-revalidate non-pinned"), then add a negative-control scenario: node cached, no event arrives, query serves the stale node until reconcile/webhook — asserting the accepted bound.
2. **F2 drops the PRD's second measurable clause.** PRD §5 F2 = dropped-webhook heal **and** "org backfill from empty db completes < 60 min" (§6: "must complete inside one hour or the design fails"). No AC covers cold-start backfill timing. Add a measurable backfill-from-empty-db AC (p95/max < 60 min).

**Should-Fix**
3. **F7 has no local proof.** Only @live @pending; cache-hit/304 is F7's mechanism but no local quota-accounting AC (N reads → M upstream calls). Add one against fakegh.
4. **Bijection break.** F2/F3/AC-GH-FORWARD/AC-GH-NOTIFY-304/AC-GH-RATELOG each carry both a @local and @live scenario under one tag — two scenarios per AC. Use distinct tags (e.g. `@F2-LIVE`) or state the local+live pairing convention.
5. **AC-GH-NAMEDOP-KEYS bundles** the unrelated `supergraph schema Issue` print assertion — split it out.
6. **S3/F1 evidence names "journald"** but the @local godog harness emits log lines, not journald — align the evidence shape.
