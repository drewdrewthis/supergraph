# EDR: GitHub plugin

Engineering Design Record. Owns internals the [PRD](../PRD.md) §6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.
Owner decisions 2026-09-05: **no GitHub App**, **PAT only**, **webhook-push ingest**, **vendored
GitHub SDL served as whole JSON nodes**.

## What it watches
Repos owned by the PAT account (`drewdrewthis`, a **User** — `type:User`, `/orgs/...` 404s → use
`/user*`/`/users/{u}/*`). Discovery = anchored `GET /user/repos?affiliation=owner&per_page=100`
(paginated). A repo appearing here after boot is picked up next reconcile with zero config → **F3**.

## Auth
**PAT only**, env `GITHUB_TOKEN` or `[plugins.github].token` (scope `repo`; 5000 REST req/h,
5000 GraphQL points/h confirmed). No App, no JWT, no installation token.

## Ingest = webhooks (push), never polling
Primary freshness is webhook push. Reachability without a public IP, two configurable modes:
- **`forward` (default):** the plugin supervises a `gh webhook forward` subprocess
  (`cli/gh-webhook`; flags `-E/--events`, `-R/--repo`, `-U/--url`, `-S/--secret`, `-H/--github-host`).
  It creates the hook and streams deliveries over an **outbound websocket**, POSTing each to our
  local `--url` (`http://127.0.0.1:7788/plugins/github/webhook`) — no inbound ingress. Plugin
  restarts it on exit (supervised).
- **`tunnel`:** operator points a Cloudflare/Tailscale-Funnel URL at the same handler; plugin
  creates the repo hook itself via `POST /repos/{o}/{r}/hooks`. One hook per discovered repo, each
  with its own HMAC secret.

Webhook handler verifies `X-Hub-Signature-256` (`sha256=`+hex HMAC-SHA256, UTF-8) with **`hmac.Equal`**
against that hook's secret; bad/absent → **401**, no emit; good → map `X-GitHub-Event`+`action` to a
Type, upsert the node JSON, emit, 200.

## Correctness paths (webhooks are latency-only)
1. **Hourly since-cursor reconcile** — GraphQL pull per repo, advances `since`. Primary guarantee.
2. **Boot redelivery** — per repo `GET /repos/{o}/{r}/hooks/{id}/deliveries` (**PAT-ok**, verified;
   `repo`/`read:repo_hook` scope), replay undelivered/failed past the stored delivery id via
   `POST .../deliveries/{id}/attempts`, dedupe by `X-GitHub-Delivery` in `github_deliveries`.
3. **`/notifications` poll (secondary)** — `GET /notifications` with `If-Modified-Since`; **304 =
   zero quota**, poll at the server's `X-Poll-Interval`. Used for repos we cannot hook (other
   owners' orgs) and as a cheap reconcile signal; a 200 feeds the same cursor/store.

## Storage = whole JSON node, three tiers
Store each object as the **complete JSON node as fetched** (no field curation) and serve it back
whole. Tiers:
- **(a) Baseline at boot, per repo** — one paginated anchored GraphQL fetch each for open issues,
  open PRs, checks, reviews, comments, labels, refs, project items. **Shallow:** issues/PRs with
  scalars+labels+assignees, one page each; nested connections come later via read-through/webhooks.
  Measured against one hour → **F2**.
- **(b) Webhooks** keep nodes current.
- **(c) Read-through on miss** for the long tail — one anchored fetch, store, serve; **TTL** for
  churny no-webhook fields (e.g. Actions logs).

## GitHub GraphQL limits (govern the baseline)
100 nodes/page, 500k nodes/query, **5000 points/h (~1 point / 100 nodes)**. Every query reads
`rateLimit{remaining,resetAt}`; backfill **pauses near the floor** until `resetAt`, never hard-fails.
Shallow baseline keeps point cost low; F2 measures the full baseline inside one hour.

## Event types emitted (Type; whole node in Payload, V:1)
`issue.opened|edited|closed`, `pr.opened|edited|closed|synchronize`, `checkRun.updated`,
`review.submitted`, `comment.created`. Envelope `{TS,Source:"github",Type,V:1,Key,Payload=nodeJSON}`.

## Entities & keys
- issue: `issue:<owner>/<repo>#<n>@<hostId>` · pr: `pr:<owner>/<repo>#<n>@<hostId>`
- checkRun: `checkRun:<owner>/<repo>@<checkRunId>@<hostId>`

## Cursor semantics
Per-repo `since` = newest `updatedAt` ingested, in core's `cursors` table (`since:<owner>/<repo>`),
queried with a **60 s overlap** (`since-60s`) so a same-second edit is not skipped.
`notif:lastModified` and per-repo `hook:<o>/<r>:lastDeliveryId` also live in core cursors.
`CursorReporter.Cursor()` returns a compact summary (in-memory).

## Schema = vendored GitHub SDL (no Github* prefix)
Vendor `octokit/graphql-schema`'s `schema.graphql` (measured: 64k lines, 1636 types), **strip the
Mutation root** (read-only plugin), keep GitHub's own type names (`Issue`, `PullRequest`, `CheckRun`,
…) — **no `Github*` prefix**. Our additions ONLY via `extend type` (`origin`, `fetchedAt`, and
`Tmux*`/`Claude*` joins later). **CI check:** our served schema is a strict **subset** of the vendored
upstream (no invented fields on GitHub types).

## Executor decision — JSON-backed dynamic resolver (measured)
Bench in `/tmp/sg-sdl-bench` (deduped SDL, 1636 types, this box, go1.26):

| Approach | Codegen | Generated source | Build/binary | Startup |
|---|---|---|---|---|
| **gqlgen codegen** | **FAILS on stock SDL** (`merging type systems failed: unable to bind to interface … Query does not satisfy Node`) after emitting **46,089 lines / 1.71 MB of models alone** in ~5.2 s | 1.7 MB models + full exec + ~1600 resolver stubs to hand-fill | multi-MB binary delta; ~1600 methods to maintain | n/a |
| **JSON-backed (gqlparser AST + one dynamic executor)** | none | **zero generated code** | **+3.26 MB** dep footprint | **~29 ms** to load+validate the full SDL |

**Pick: JSON-backed.** gqlgen does not even complete on GitHub's real SDL without manual model
bindings/surgery, and its 46k-line model dump + per-type resolvers fight decision #4 ("whole node,
no curation"). A single `gqlparser`-validated dynamic executor serves stored JSON nodes directly.
**Core-fit — no core change:** github does **not** join the `gqlgen.yml` glob
(`plugins/*/schema/*.graphqls`) — dumping 1636 types there would break core codegen. Instead github
serves its own `/plugins/github/graphql` via the existing **`HTTPRoutes`** seam (zero core edit;
`git diff --stat core/` = 0). The glob stays for small `extend`-only plugins.

## Named operations + CLI
`plugins/github/queries/*.graphql` (~10): `openIssues`, `issue`, `prWithChecks`, `prsAwaitingReview`,
`myClaimed`, `openPRs`, `checkRunsForPR`, `issueComments`, `repoLabels`, `paneForBranch`
(**@pending** — needs the tmux plugin). Run via `supergraph query --op NAME --var k=v`; introspect via
`supergraph schema <Type>`. **CLI lives in `cmd/` (not core):** `cmd/supergraph/query.go` today takes a
raw query string and POSTs `/graphql`. Changes (cmd/ only): add `--op` (load the named `.graphql`) +
repeatable `--var k=v`, target the github route `/plugins/github/graphql`, and add a `schema <Type>`
subcommand that prints the vendored type. Core untouched.

## SQLite state tables (add-only, `IF NOT EXISTS`)
- `github_nodes(id,typename,owner,repo,number,node_json,fetched_at,updated_at, PK(id))`
- `github_hooks(owner,repo,hook_id,secret, PK(owner,repo))`
- `github_deliveries(delivery_id,seen_at, PK(delivery_id))`

## Config keys `[plugins.github]`
`token` (or env `GITHUB_TOKEN`), `ingress` (`"forward"`|`"tunnel"`), `tunnelURL` (tunnel mode),
`webhookSecret` (or env; per-hook secrets derived), `owner` (="drewdrewthis"),
`reconcileIntervalSeconds` (3600), `readThroughTTLSeconds`, `baseURL` (test-only → fake server).

## Failure modes
- Webhook transport down → reconcile + boot redelivery heal.
- HMAC mismatch → 401, logged, no emit.
- GraphQL points floor → pause to `resetAt`, never hard-fail.
- `gh webhook forward` subprocess exits → supervised restart.
- `/notifications` 304 spam → honor `X-Poll-Interval`, zero quota.
- `since` clock skew → 60 s overlap.
- Repos in other owners' orgs (no hook rights) → `/notifications` path only.

## Acceptance criteria

### PRD ACs (verbatim; github scope; p95 over N≥20)
- **S3** — p95 < 1 s from check-run webhook receipt to subscription push (N≥20 signed webhooks). Evidence: paired journald lines.
- **F1 (github half)** — p95 ingest-to-queryable < 1 s for github events (N≥20). Evidence: paired log timestamps.
- **F2** — a dropped webhook is present after the next hourly reconcile; **full baseline backfill from empty completes < 60 min**. Evidence: log + timer.
- **F3** — a repo created after boot appears in the graph with zero config within one reconcile. Evidence: query screenshot.
- **F7** — GitHub API usage stays under budget at steady state, 20 repos (GraphQL points + REST < 500/h). Evidence: rate-limit / `rateLimit` log.

### Plugin-level ACs
- **AC-GH-HMAC** — bad/absent `X-Hub-Signature-256` → 401, no emit; valid → 200 + one event. `@local`
- **AC-GH-FORWARD** — the `gh webhook forward` subprocess is supervised and restarted on exit; its deliveries reach the handler. `@local` (fake `gh webhook forward` stub) + `@live @pending`
- **AC-GH-REDELIVER** — a failed repo-hook delivery past the stored id is replayed via `/attempts` and emitted **exactly once** (deduped by `X-GitHub-Delivery`). `@local` (fake `/repos/*/hooks/*/deliveries`+`/attempts`) + `@live @pending`
- **AC-GH-NOTIFY-304** — `/notifications` with `If-Modified-Since` returning **304** costs zero quota and triggers no re-fetch; a 200 with changes feeds the cursor/store. `@local` (fake 304 path) + `@live @pending`
- **AC-GH-RATELIMIT-FLOOR** — near the GraphQL points floor the backfill pauses until `resetAt` and resumes; never hard-fails; `/health` shows no forced-stale. `@local` (fake `rateLimit` floor)
- **AC-GH-OVERLAP** — an event edited within the `since-60s` window is still ingested. `@local`
- **AC-GH-CURSOR** — after restart the `since` cursor is unchanged **and** the next fetch carries `since=` equal to it (fake server records the received `since`). `@local`
- **AC-GH-JSONNODE** — a query for a node not in store triggers one anchored fetch, stores it, and serves the **complete** JSON node. `@local`
- **AC-GH-SCHEMA-SUBSET** — the served github schema is a strict subset of the vendored upstream SDL (no `Github*` prefix; `extend` adds only `origin`/`fetchedAt`). `@local` (CI test)
- **AC-GH-NAMEDOP** — `supergraph query --op openIssues` runs the named `.graphql`; `supergraph schema Issue` prints the type. `@local`
- **AC-GH-RATELOG** — every GitHub response's rate headers / GraphQL `rateLimit{remaining,resetAt}` are logged. `@local` + `@live @pending`
- **AC-GH-ZEROCORE** — `git diff --stat core/` = 0 (github serves via `HTTPRoutes`, not the gqlgen glob). `@local`

### Live vs local split (mirrors `features/github.feature` tag-for-tag)
**`@local`:** S3, F1 (N≥20 p95), F2 (drop-heal + fake baseline timing), F3 (fake `/user/repos` gains a
repo), AC-GH-HMAC, -FORWARD (stub), -REDELIVER, -NOTIFY-304, -RATELIMIT-FLOOR, -OVERLAP, -CURSOR,
-JSONNODE, -SCHEMA-SUBSET, -NAMEDOP, -RATELOG, -ZEROCORE — all against a **fake GitHub `httptest`
server** (GraphQL + REST hooks/deliveries + `/notifications` 304 + settable `rateLimit`/headers) +
**synthetic signed webhook POSTs** + a **fake `gh webhook forward` stub**.
**`@live @pending`:** F2 (live <60 min), F3 (real new repo), F7 (real budget), AC-GH-FORWARD,
-REDELIVER, -NOTIFY-304, -RATELOG (real GitHub) — need env `GITHUB_TOKEN`+`GITHUB_ORG`; `@pending`
until run against live GitHub.

## AC review: applied
Prior ac-reviewer Must/Should-Fixes remain folded in (N≥20 p95; rate-limit floor recovery;
tag-for-tag @live/@local with @live=@pending; F3 local proof; `since-60s`; falsifiable cursor). The
App-based items are superseded by the PAT/webhook redesign above.

## Step sequence (coder-sized; fake-GitHub server first)
1. **Fake GitHub `httptest` server** (`plugins/github/internal/fakegh`): GraphQL endpoint (paginated
   connections + `rateLimit`), REST `/user/repos`, `/repos/*/hooks` + `/deliveries` + `/attempts`,
   `/notifications` (304 path), settable rate fields. Everything tests against it.
2. Vendor + strip: `plugins/github/schema/github.graphql` from octokit SDL, Mutation removed; a CI
   subset check vs upstream.
3. Scaffold `plugins/github/github.go`: `Register`, `New`, `Name`, `Migrate`, `Start`, `HTTPRoutes`
   (`webhook` + `graphql`), `CursorReporter`; `baseURL` override.
4. `Migrate`: the three state tables.
5. JSON-backed executor: load SDL with `gqlparser`, dynamic field walker serving `github_nodes`;
   read-through on miss + TTL (covers AC-GH-JSONNODE/-SCHEMA-SUBSET).
6. Baseline backfill (shallow, paginated, `rateLimit` floor-pause) + hourly since-cursor reconcile
   with `since-60s` overlap (covers F1/F2/F3/AC-GH-OVERLAP/-CURSOR/-RATELIMIT-FLOOR).
7. Webhook handler + HMAC (`hmac.Equal`) + node upsert + emit (covers S3/AC-GH-HMAC).
8. Ingress supervisor: `gh webhook forward` subprocess (restart on exit) / tunnel hook-create;
   boot redelivery from repo-hook deliveries; `/notifications` If-Modified-Since poll
   (covers AC-GH-FORWARD/-REDELIVER/-NOTIFY-304).
9. Named ops `plugins/github/queries/*.graphql`; `cmd/supergraph/query.go` `--op`/`--var` + `schema`
   subcommand (cmd/ only) (covers AC-GH-NAMEDOP).
10. `features/steps_github_test.go` wiring the feature to fakegh + signed POST + forward stub; blank
    import in `graph/plugins_import.go`; verify `git diff --stat core/` = 0 (covers AC-GH-ZEROCORE).

## Handoff
- ACs ready for ac-reviewer (see §Acceptance criteria).
- Implementation → coder; steps 1–2 (fake server, vendor+strip) can start in parallel; step 5 (executor) is the judgment-heavy core.
