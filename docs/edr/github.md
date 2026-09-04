# EDR: GitHub plugin

Engineering Design Record. Owns internals the [PRD](../PRD.md) §6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.

## What it watches
The `drewdrewthis` account's repos. **Fact check:** `drewdrewthis` is a GitHub **User**, not an
org (`gh api /users/drewdrewthis` → `"type":"User"`; `/orgs/drewdrewthis` → 404). GitHub Apps
install on user accounts too (`GET /users/{username}/installation`), and an "all repositories"
install covers repos created later — the org-App design holds for a user account (**PRD to say "account-level"**); only the discovery
endpoint differs (`/user/repos` / installation repos, not `/orgs/...`). Owner will create the App.

## Event types emitted (Type + payload v1)
- `issue.opened` | `issue.closed` | `issue.edited` — `{owner,repo,number,title,state,labels[],updatedAt}`
- `pr.opened` | `pr.closed` | `pr.synchronize` — `{owner,repo,number,title,state,headSha,updatedAt}`
- `checkRun.updated` — `{owner,repo,prNumber,checkRunId,name,status,conclusion,headSha,updatedAt}`

All wrapped in `core.Envelope{TS,Source:"github",Type,V:1,Key,Payload}`. Payload opaque to core.

## Entities & keys
- issue: `issue:<owner>/<repo>#<n>@<hostId>`
- pr: `pr:<owner>/<repo>#<n>@<hostId>`
- checkRun: `checkRun:<owner>/<repo>@<checkRunId>@<hostId>`

## Cursor semantics
Per-repo `since` = ISO-8601 timestamp of the newest `updatedAt` ingested for that repo. Stored in
**core's `cursors` table** (`Store.SetCursor`/`Cursor`), name `since:<owner>/<repo>` — never a
plugin state table (contract §SQLite). `CursorReporter.Cursor()` returns a compact summary
(`repos=20 oldest=<ts>`). Reconcile queries `since` with a 60 s overlap (`since-60s`) so an event
edited within the same second the cursor was written is not skipped. Webhook redelivery position:
`webhook:lastDeliveryId`.

## Reconcile loop (PRIMARY correctness path)
Hourly ticker **+ once on boot** (webhooks are latency-only, PRD §6). Per pass:
1. Discover repos — App: `GET /installation/repositories`; PAT spike: `GET /user/repos?affiliation=owner&per_page=100`. New repos appear automatically → **covers F3**.
2. Per repo: `GET /repos/{o}/{r}/issues?since=<cursor>&state=all&sort=updated&direction=asc&per_page=100`, follow `Link` pagination. **Gotcha:** the issues endpoint returns PRs too — skip any item carrying a `pull_request` field; fetch PRs via `/pulls` separately.
3. Emit envelopes (dedup by Key), advance `since` cursor to max `updatedAt` seen.

## Webhook route (latency optimization)
`HTTPRoutes.Routes()` → `{"webhook": handler}`, mounted by core at `/plugins/github/webhook`.
Verify `X-Hub-Signature-256` (`sha256=` + hex HMAC-SHA256 of the raw body, UTF-8) against the
secret using **`hmac.Equal`** (constant-time; docs forbid `==`). Secret from
`[plugins.github].webhookSecret` or env `GITHUB_WEBHOOK_SECRET`. Bad/absent signature → **401**,
no emit. Good → map `X-GitHub-Event` (`issues`/`pull_request`/`check_run`/`repository`) + `action`
to a Type, emit, 200. Ingress lands on hetzner-agents only (public IP) — accepted SPOF; reconcile
covers downtime.

## Auth (App-capable; PAT fallback)
`[plugins.github].auth` selects the mode:
- **`app`** (primary) — `appId`, `privateKeyPath`, `installationId`. Mint a JWT (RS256, `iss`=appId,
  short iat/exp) with **`github.com/golang-jwt/jwt/v5`**, exchange at
  `POST /app/installations/{installationId}/access_tokens` → installation token (1 h TTL).
  **Refresh before expiry** (re-mint at ~55 min). Discover install via `GET /users/drewdrewthis/installation`.
- **`pat`** (fallback) — `token` from env `GITHUB_TOKEN` or config (5000 req/h confirmed).

## Boot redelivery
On boot, `GET /app/hook/deliveries` (paginated via `cursor`); replay every **undelivered/failed**
delivery whose id is past the last-seen delivery id, via `POST /app/hook/deliveries/{id}/attempts`.
**Dedupe by `X-GitHub-Delivery`** persisted in a `github_deliveries(delivery_id PK, seen_at)` table,
so a redelivered event is never emitted twice. **REQUIRED in `app` mode; skipped with a warn log in
`pat` mode** (PAT cannot read `/app/hook/deliveries`). Cursor `webhook:lastDeliveryId` tracks position.

## Quota budget (F7)
Steady state, 20 repos: 1 repo-list page + ~20 issue-list pages (+ a few PR/check calls) ≈ **40–60
req/hour**. Webhooks cost **0** API calls. Boot backfill is a bounded one-time cost. Ceiling 500/h
(self-imposed; hard limit 5000/h) → met with margin. On `x-ratelimit-remaining: 0`, sleep to `-reset`.

## GraphQL schema extension
`plugins/github/schema/github.graphqls`, `extend type Query`/`Subscription` (payload flattened to
string per template):
- types `GithubIssue`, `GithubPullRequest`, `GithubCheckRun`
- `Query.githubIssue(repo,number)`, `Query.githubIssues(repo,state)`
- `Subscription.issueOpened(org)`, `Subscription.checkRunUpdated(repo,prNumber)`
Resolvers delegate through `core.Registry`.

## SQLite state tables (add-only, `IF NOT EXISTS`)
- `github_issues(owner,repo,number,title,state,labels_json,updated_at,host_id, PK(owner,repo,number))`
- `github_pull_requests(owner,repo,number,title,state,head_sha,updated_at,host_id, PK(owner,repo,number))`
- `github_check_runs(id,owner,repo,pr_number,name,status,conclusion,head_sha,updated_at,host_id, PK(id))`
- `github_deliveries(delivery_id,seen_at, PK(delivery_id))` — webhook dedupe by `X-GitHub-Delivery`

## Config keys `[plugins.github]`
`auth` (`"app"` | `"pat"`), `appId`/`privateKeyPath`/`installationId` (app mode), `token` (pat mode,
or env `GITHUB_TOKEN`), `webhookSecret` (or env `GITHUB_WEBHOOK_SECRET`), `owner` (="drewdrewthis"),
`reconcileIntervalSeconds` (default 3600), `baseURL` (test-only, points reconcile + App/delivery calls at the fake server).

## Failure modes
- Webhook SPOF (hetzner only public IP) → reconcile heals.
- HMAC mismatch → 401, logged, no emit.
- Rate-limit exhaustion → backoff to reset; never hard-fail the loop.
- Issues endpoint returns PRs → filter on `pull_request` field.
- `since` clock skew → 60 s overlap window.
- User-vs-org: any `/orgs/drewdrewthis/*` call 404s → use `/user*` / `/users/{u}/*`.

## Owner setup (GitHub App — owner creates it)
1. **GitHub Apps → New GitHub App** (Settings → Developer settings). Name e.g. `supergraph-drewdrewthis`.
2. **Webhook URL** = `https://<hetzner-agents-public-host>/plugins/github/webhook`; set a **Webhook secret** (→ `[plugins.github].webhookSecret`).
3. **Repository permissions (read-only):** Issues: R, Pull requests: R, Checks: R, Metadata: R.
4. **Subscribe to events:** `issues`, `pull_request`, `check_run`, `check_suite`, `repository`.
5. Generate a **private key** (.pem → `privateKeyPath`); note the **App ID** (→ `appId`).
6. **Install** the App on the `drewdrewthis` account with **All repositories** (covers F3); note the **installation id** (`GET /users/drewdrewthis/installation` → `id`, → `installationId`).

## Acceptance criteria

### PRD ACs (restated verbatim; github scope)
- **S3** — p95 < 1 s from check-run webhook receipt to subscription push (measured over **N≥20** synthetic signed webhooks, not one sample). Evidence: paired journald lines (webhook-in ts, push-out ts).
- **F1 (github half)** — p95 ingest-to-queryable < 1 s for github events (measured over **N≥20** samples). Evidence: paired log timestamps.
- **F2** — a deliberately dropped webhook is present after the next hourly reconcile; org backfill from empty db completes < 60 min. Evidence: log + timer.
- **F3** — a repo created in the org after boot appears in the graph with zero config within one reconcile. Evidence: query screenshot.
- **F7** — GitHub API calls stay under 500/hour at steady state with 20 repos. Evidence: rate-limit header log.

### Plugin-level ACs
- **AC-GH-RATELIMIT** — on `X-RateLimit-Remaining: 0`, the reconcile loop sleeps until `X-RateLimit-Reset` and resumes; it never hard-fails (no panic/exit) and `/health` shows no forced-stale for github. `@local`
- **AC-GH-OVERLAP** — an event edited within the `since-60s` overlap window is still ingested, not skipped. `@local`
- **AC-GH-HMAC** — POST to `/plugins/github/webhook` with a bad/absent `X-Hub-Signature-256` → 401, zero events emitted; a correctly-signed POST → 200 + exactly one event. `@local`
- **AC-GH-CURSOR** — after a restart the persisted `since` cursor is unchanged **and** the next reconcile's issues request carries `since=` equal to it (falsifiable: the fake server records the received `since`). `@local`
- **AC-GH-RATELOG** — every GitHub response's `x-ratelimit-limit`/`-remaining`/`-reset`/`-used` headers are logged. `@local` (fake server sets them) + `@live @pending`
- **AC-GH-PRFILTER** — an issues-endpoint item carrying a `pull_request` field is never emitted as an `issue.*` event. `@local`
- **AC-GH-REDELIVER-APP** — in `app` mode, a failed/undelivered delivery past the last-seen id is replayed via `/attempts` and emitted **exactly once** (deduped by `X-GitHub-Delivery`). `@local` (fake `/app/hook/deliveries` + `/attempts`) + `@live @pending`
- **AC-GH-REDELIVER-PAT** — in `pat` mode redelivery is skipped with a warn log and no `/app/hook/deliveries` request. `@local`
- **AC-GH-ZEROCORE** — `git diff --stat core/` = 0 files after this plugin compiles in. `@local`

### Live vs local split (mirrors `features/github.feature` tag-for-tag)
**`@local` (13):** `S3`, `F1`, `F2` (drop-heal), `F3` (fake repo-listing discovery), `AC-GH-RATELIMIT`,
`AC-GH-OVERLAP`, `AC-GH-HMAC`, `AC-GH-CURSOR`, `AC-GH-RATELOG`, `AC-GH-PRFILTER`, `AC-GH-REDELIVER-APP`,
`AC-GH-REDELIVER-PAT`, `AC-GH-ZEROCORE` — all against the fake GitHub
`httptest` server (Step 1) + synthetic signed webhook POSTs; S3/F1 fire N≥20 and compute p95.
**`@live @pending` (5):** `F2` (backfill <60 min), `F3` (real new repo), `F7` (real quota),
`AC-GH-RATELOG` (live headers), `AC-GH-REDELIVER-APP` (real missed delivery) — need env
`GITHUB_TOKEN`+`GITHUB_ORG` **and the owner-created App**, so `@pending` until it exists (§Owner setup).

<!-- ACs ready for ac-reviewer -->
## Step sequence (coder-sized; fake-GitHub server first)
1. **Fake GitHub `httptest` server** (`plugins/github/internal/fakegh`): canned `/user/repos`, `/repos/*/issues?since`, `/app/hook/deliveries`, settable `x-ratelimit-*`. Everything downstream tests against it.
2. Scaffold `plugins/github/github.go`: `init()`→`core.Register("github",New)`, `New`, `Name`, `Migrate`, `Start`; `baseURL` override from cfg.
3. `Migrate`: create the three state tables (`IF NOT EXISTS`).
4. Reconcile loop: repo discovery + per-repo `since` fetch (with `since-60s` overlap) + PR filter + emit + cursor advance (covers F1/F2/F3/AC-GH-OVERLAP).
5. Webhook handler + `Routes()`: HMAC verify (`hmac.Equal`), event→Type map, emit (covers S3/AC-GH-HMAC).
6. Rate-header logging + `Remaining:0`→sleep-to-`Reset` backoff (covers F7/AC-GH-RATELOG/AC-GH-RATELIMIT).
7. Auth modes (`app`: JWT via `golang-jwt/jwt/v5` → installation token, refresh at ~55 min; `pat` fallback) + boot redelivery from `/app/hook/deliveries` with `X-GitHub-Delivery` dedupe (covers AC-GH-REDELIVER-APP/-PAT).
8. Schema `github.graphqls` + `gqlgen generate` + resolvers delegating through `core.Registry`.
9. `features/steps_github_test.go` wiring `github.feature` to the fake server + signed POST; blank import in `graph/plugins_import.go`; verify `git diff --stat core/` = 0.

## Handoff
- ACs ready for ac-reviewer (see §Acceptance criteria above).
- Implementation → coder. Step 1 (fake server) and step 10 (import/diff) can go to fast-coder; steps 4–7 need coder judgment.

## AC review: applied

Must-Fix (S3/F1 N≥20 p95; AC-GH-RATELIMIT recovery; tag-for-tag @live/@local, @live=@pending until the App exists) and Should-Fix (F3 @local fake-listing proof; AC-GH-OVERLAP `since-60s`; AC-GH-CURSOR falsifiable via received `since=`; AC-GH-REDELIVER split -APP/-PAT) folded into this EDR + `features/github.feature`.
