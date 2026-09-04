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

**Hard constraints (owner):** ≤ **800 LOC prod** for `plugins/github/**` excluding tests and
`internal/fakegh` (per-package budget below); zero core diff; PAT-only; HMAC per hook; per-repo hook
creation from `/user/repos` (F3); point-budget floor pause + rate-limit logging; CLI `--op/--var` +
`schema <Type>` in `cmd/` only; fakegh httptest server + fake `gh` stub for `@local`.

---

## LOC budget (prod, ≤ 800 total; tests + `internal/fakegh` excluded)
| Package/file (`plugins/github/`) | Budget | Responsibility |
|---|---:|---|
| `github.go` | 90 | plugin wiring: `Register`/`New`/`Name`/`Migrate`/`Start`/`HTTPRoutes`/`CursorReporter`/config |
| `keys.go` | 60 | key grammar: parse/format + object→key + event→key derivation |
| `store.go` | 110 | SQLite `github_nodes`: upsert, get, `deleteKeys`, etag/fetchedAt, pin eval, tag index |
| `proxy.go` | 130 | read-through resolve: miss→fetch→store→serve; ETag/304; **singleflight (borrowed ~40)** |
| `webhook.go` | 100 | HMAC verify; event→key map; upsert/purge; emit envelopes |
| `executor.go` | 120 | JSON-backed GraphQL executor + named-op loader + declared-key scoping |
| `ingest.go` | 100 | `gh webhook forward` supervisor (backoff) + redelivery + `/notifications` poll (flag) |
| `reconcile.go` | 90 | discovery `/user/repos` + hook creation + since-cursor + floor-pause + ratelog |
| **Total** | **800** | CLI delta in `cmd/supergraph/` (~60) is counted separately, not in this budget |
- **AC-GH-LOC** guards it: a CI step (`make loc-github`) runs `find plugins/github -name '*.go' ! -name '*_test.go' -not -path '*/fakegh/*' | xargs sed '/^\s*\/\//d;/^\s*$/d' | wc -l` and fails > 800.

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
`plugins/github/queries/*.graphql` (~10: `openIssues`, `issue`, `prWithChecks`, `prsAwaitingReview`,
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
- **`github.node.updated`** — `Payload={key,etag,typename}` — a node was upserted (webhook or read-through 200).
- **`github.node.purged`** — `Payload={key}` — a node/entry evicted by tag purge.
- **`github.webhook.received`** — `Payload={delivery,event,action}` — raw receipt; drives S3 timing and `/health` `lastEventAt`.
Cross-plugin joins key on `github.node.updated.Key` (e.g. `issue:o/r#5`).

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
- `github_nodes(key TEXT PK, typename, owner, repo, number, node_json, etag, pinned INT, fetched_at, updated_at)`
- `github_tags(tag TEXT, key TEXT, PK(tag,key))`   — tag index for scope/kind purge
- `github_hooks(owner,repo,hook_id,secret, PK(owner,repo))`
- `github_deliveries(delivery_id, seen_at, PK(delivery_id))`
- cursors live in core's `cursors` table (`since:<o>/<r>`, `notif:lastModified`, `hook:<o>/<r>:lastDeliveryId`).

## Config keys `[plugins.github]`
`token` (or env), `ingress` (`forward`|`tunnel`), `tunnelURL`, `webhookSecret`, `owner`,
`reconcileIntervalSeconds` (3600), `notifications` (bool, default false), `[plugins.github.pin]` (above),
`baseURL` (test-only → fakegh).

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
`features/github.feature`. Summary: **S3, F1, F2, F3, F7** (PRD) + **cache-hit-no-upstream-call,
ETag-304-zero-quota, purge-by-tag-evicts-exact-keys, immutable-pinned-never-refetched,
singleflight-coalesces-N-into-1, HMAC-401, forward-child-restart-with-redelivery,
notifications-304-behind-flag, floor-pause, ratelog, named-op-scoped-keys, cursor-persist,
LOC-budget, zerocore**. `@local` runs against fakegh + fake `gh` stub; live proofs are `@live
@pending`.

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
