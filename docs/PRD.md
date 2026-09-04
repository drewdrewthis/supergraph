# Supergraph PRD

**Status:** living doc — links over prose, keep this to one page.

## 1. Project specifics

- **Owner:** drewdrewthis
- **Status:** PRD accepted 2026-09-04; spike next
- **Target:** spike within 2 weeks
- **Repo:** https://github.com/drewdrewthis/supergraph

## 2. Goals / objectives

- **Sub-second reads** on any query.
- **Freshness is the real metric** — measure event-in latency, not just read speed.
- **Zero-edit plugin registration** — a new plugin needs no core code change.
- **Org-wide GitHub coverage**, including new repos, automatically.
- **Multi-box** — a machine is just a subgraph on another host.
- **Self-heals** after an outage, with **near-zero ops**.

## 3. Background & strategic fit

The owner runs fleets of Claude agents across several boxes (laptop, hetzner-agents, langwatch-dev).
Today's system, `orchard-daemon` (Go, gqlgen — see ADR-009), has drifted: mutations turned into
actions, the pickup loop bypasses it and reads a GitHub Projects board via shell, its cache went a
month stale unnoticed, and it only covers one repo at a time (ADR-009 rule #6 forbids `gh`
enumeration, so it can't scan the org).

**First consumer:** an always-on worker. Any new open GitHub issue in the `drewdrewthis` org
(no label needed) → orchardist sees it sub-second → claims it (assignee/`grinding` label, for
dedup only) → launches the `ship` skill. The **ship worker** owns the PR from there: it
subscribes to `checkRunUpdated` and drives its own PR to green (review-clerk + tests gate
before ready). The **orchardist** only dispatches and watches slots — it does not drive CI.
Owner pinged on Telegram when the PR is ready. The owner also keeps hitting **GitHub API rate
limits**; webhooks + anchored lookups + caching is the quota fix.

**Long-term:** wire in Todoist, Gmail, Calendar, Slack, Discord. The real value is **cross-source
joins** — issue ↔ task ↔ email ↔ running session ↔ PR — in one query.

## 4. Assumptions

- Go stays the implementation language. **One Go binary, cross-compiled, OS-detected at
  start; the laptop is a normal peer. Only the two spike boxes are Linux.**
- PAT auth + `gh webhook forward` (official gh extension, outbound websocket) makes webhooks
  reachable with no public IP; the plugin creates one repo webhook per discovered repo.
- Mesh networking (WireGuard/Tailscale) already exists between boxes.
- SQLite per plugin is fast enough for this workload.
- gqlgen subscriptions over the mesh are sufficient for peer mirroring. **UNVERIFIED — spike must prove.**
- **Canary dropped**: a dead feed is noticed because the orchardist stops picking up issues; no synthetic heartbeat.

## 5. User stories

- As the **owner**, I want to open an issue and walk away, so that a PR shows up without me watching.
- As **orchardist**, I want to query free slots across boxes, so that I can dispatch work to any idle machine.
- As the **ship worker**, I want to subscribe to PR CI status, so that I react the moment a check finishes.
- As the **owner**, I want to ask "what is everything touching issue #N" across GitHub/Todoist/email/tmux/claude, so that I get one answer instead of five lookups.
- As a **developer**, I want to add a new plugin (a Go package, not a process) in an afternoon following the contract doc, so that the system grows without core changes.
- As an **operator**, I want to see plugin lag on a health endpoint and have an external poller alert on staleness, so that stale data never goes unnoticed again.

**Acceptance criteria:**

- **S1** — p95 time from issue-open to PR-open ≤ 15 min over 10 seeded issues, zero human action, including the review-clerk + tests gate. Evidence: journald timestamps + PR-open screenshots.
- **S2** — cross-box free-slot query (tmux plugin) p95 < 1 s warm, seeded realistic db. Evidence: timing screenshot.
- **S3** — p95 < 1 s from check-run webhook receipt to subscription push. Evidence: paired journald lines (webhook-in ts, push-out ts).
- **S4** — one query for issue #N returns ≥ 1 row each from github + claude (`ClaudeSession`) with source tags; an absent source shows an explicit stale marker, never omission. Evidence: query result screenshot.
- **S5** — template plugin compiles in and serves `_health` with `git diff --stat core/` = 0 files. Evidence: diff output + `_health` screenshot.
- **S6** — `/health` reports last real event time and lag per plugin: `state: stale` and growing `lagSeconds` within 60 s of a plugin stalling. Evidence: `/health` screenshots + lag log.

### Failure-surface ACs

- **F1 Freshness** — p95 ingest-to-queryable < 1 s for github and claude events. Evidence: paired log timestamps.
- **F2 Reconcile/cache** — a dropped webhook is present after the next hourly reconcile; cold-cache first read of the orchardist's working set is < 1 s p95 after warm, webhook purge → refetch < 2 s. Evidence: log + timer.
- **F3 New repo** — a repo created in the org after boot appears in the graph with zero config within one reconcile. Evidence: query screenshot.
- **F4 Double-dispatch** — two orchestrators racing on one new issue (claim via assignee/`grinding` label) yield exactly one ship run; the loser sees the claim. Evidence: two logs + one PR.
- **F5 Panic isolation** — a plugin that panics (e.g. the claude plugin) shows `stale`; `_health` and the other plugins (including tmux) keep answering. Evidence: `_health` screenshot after induced panic.
- **F6** — removed (canary dropped).
- **F7 Quota** — an agent loop re-reading the same issues/PRs costs zero upstream calls until a webhook purges them, measured. Evidence: `rateLimit{remaining,resetAt}` log across a repeated-read loop.
- **F8 Stale peer** — a stopped box shows `stale since T` on its peers (tmux + claude data included) < 30 s after stop; no peer-of-peer rows exist. Evidence: query screenshot + grep.

**Milestones:** spike → EDR → core → plugin contract + harness → github plugin → tmux plugin →
claude plugin → subscriptions + health → drewdrewthis worker → todoist.

**Spike pass/fail:** (a) cross-box free-slot query meets **S2** (p95 < 1 s warm, seeded realistic
db); (c) a stopped peer box meets **F8** (`stale since T` within 30 s, no peer-of-peer rows).
**Kill criterion:** if (c) fails → single-box fallback.

## 6. User interaction & design

High-level only — full design goes in a future EDR under `docs/edr/`.

- **No router, no federation.** One binary per box. Plugins are Go packages compiled into that
  binary, not separate processes. The binary serves **one GraphQL endpoint** (gqlgen) per box.
- **Core** = the binary shell + a tiny ingest envelope `{ts, source, type, v, key, payload}`.
  **Core is locked before any plugin starts; plugin internals (fsnotify vs poll, socket paths,
  cursors) are decided in each plugin's EDR, not here.**
- **Plugin** = a Go package with a fixed contract: same envelope, own listener/poller,
  **own SQLite file** (state tables + a short auto-pruned event ring + cursors), own migrations
  at boot, and a `_health` endpoint (lastEventAt, cursor, lag). Entity keys keep `hostId` for
  per-box identity. **Rebuildable:** delete the db, restart, reconcile refills it.
- **Multi-box = the `peer` plugin.** To see another box's data, a box runs a `peer` plugin that
  queries + subscribes to that box's normal GraphQL endpoint over the mesh, mirrors it into its
  own SQLite, and serves it back tagged `hostId` + `lastSeenAt`. Duplicated on purpose — it's a
  cache, not a shared source of truth. A down box reads as **stale since T**, never as empty.
- **GitHub plugin — auth & ingest**: **no GitHub App**, auth is a **PAT**, ingest is **webhooks
  (push), never polling as primary**, reachable via `gh webhook forward` (official extension,
  outbound websocket, supervised by the plugin) — no public IP needed; tunnel URL is a config
  alternative. One repo webhook per discovered repo via anchored `/user/repos` (drives F3), each
  with its own HMAC secret. Hourly `since`-cursor reconcile + redelivery-on-boot stay **primary
  correctness** even if the tunnel drops. **Secondary channel**: `/notifications` polled with
  `If-Modified-Since` (304 = zero quota) at `X-Poll-Interval` — covers repos we can't hook (other
  owners), doubles as reconcile signal, same cursor/store.
- **Caching architecture — event-invalidated proxy, not a model of GitHub.** No baseline; nothing
  fetched unasked. Reads coalesce (singleflight) and batch; entries are tagged with the object ids
  they contain; a webhook **purges by tag, never refreshes**. REST conditional requests (304 =
  zero quota) revalidate. Immutable nodes (commits, tags, releases, old merged PRs) pin forever;
  anything not webhook-covered gets a short TTL floor. Prior art: [ghx](https://github.com/brunoborges/ghx)
  (Go, MIT, `gh`-caching daemon, singleflight, no webhook invalidation) — adopt/fork vs build is
  an EDR call (§7).
- **Subscriptions**: the binary pushes events like "issue opened" to actors (e.g. orchardist);
  actors own all side effects — the supergraph never does. **Actor-level claim**: before acting on
  a new issue, the worker must claim it (assign self / add `grinding` label) and re-check —
  ingest-level idempotency alone already let a double-dispatch through once.
- **No canary.** Core `/health` reports per-plugin `lastEventAt`, `lagSeconds`, `state`, derived
  from real events only — no synthetic heartbeat. Alerting is out of scope for the binary; an
  external poller of `/health` (cron, uptime-kuma, or the other box's `peer` plugin) does it.
  **Versioning**: event payload carries `v` + upcasters, schema changes add-only with
  `@deprecated`, schema-diff check in CI. Ship a [plugin contract doc](docs/plugin-contract.md)
  (stub, core tier), a template plugin, a one-command local dev harness.
- **CLI + install**: `supergraph serve|install|uninstall|start|stop|restart|status|query
  '<graphql>'` — `serve` is foreground dev, install/start/stop write+drive a systemd --user unit
  (Linux) or launchd plist (macOS), `query` hits the local endpoint
  `http://127.0.0.1:7788/graphql` (`_health` at `/health`; peer endpoint bound to the mesh IP
  only). Config: `~/.config/supergraph/config.toml`; per-plugin SQLite under
  `~/.local/share/supergraph/<plugin>.db`. **Upgrade**: `install` is idempotent, `restart` after
  replacing the binary.
- **GitHub schema**: vendored from GitHub's own GraphQL SDL (`octokit/graphql-schema`), mutations
  stripped, type names **unprefixed** (`Issue`, `PullRequest` — they're GitHub's own); our
  additions land only via `extend type` (`origin`, `fetchedAt`, joins to `Tmux*`/`Claude*`); CI
  asserts a strict subset of upstream. Each cache entry is the **whole JSON node** — no
  hand-curated field subsets — but it's a cache entry, not a permanent copy (caching architecture
  above). `Tmux*`/`Claude*` keep tool prefixes (no upstream to match).
- **Schema sketch** (illustrative, non-GitHub types only — the EDR owns the real schema):
  ```graphql
  type TmuxSession { hostId, name, worktree, branch, lastSeenAt, staleSince }
  type ClaudeSession { hostId, uuid, cwd, issueNumber, lastSeenAt, staleSince }
  type ClaudeInstance { hostId, pane, pid, tmuxSession: TmuxSession, claudeSession: ClaudeSession, staleSince }
  type Slot { hostId, kind, free: Boolean, staleSince }
  type Query { issue(repo, number), tmuxSessions(hostId), claudeSessions(hostId), slots(hostId), touching(issueNumber): [Node] }
  type Subscription { issueOpened(org), checkRunUpdated(repo, prNumber), pluginLag(threshold) }
  type Health { plugin, lastEventAt, cursor, lagSeconds, state }
  ```
- **Named operations**: ~10 ops under `queries/*.graphql`, run via `supergraph query --op NAME`;
  `supergraph schema <Type>` prints one type — agents never load the full schema.
- **Plugin isolation**: goroutine per plugin with `recover()`, a panic marks it `stale`, others
  keep serving. **Auth**: per-box bearer token (env) for peer subscribe, mTLS deferred. **Peer
  loop rule**: a `peer` plugin never re-serves peer-tagged rows. **First cross-plugin join key**:
  issue number ↔ branch/worktree (full node taxonomy deferred).
- **Reconcile** must finish inside the hour or the design fails. **Logs**: journald with rotation.
  **Alerts**: external poller, sink TBD. **Tests (BDD)**: every plugin ships `.feature` files;
  ADR-009's scenario ↔ e2e bijection carries forward — S1–S6 and F1–F8 (§5) are the first
  scenarios. **"Sub-second"** = p95, warm cache, against a seeded db of realistic size (§4).
- **Build plan**: tiered. Core first (envelope, plugin contract, SQLite base, GraphQL server,
  `_health`, dev harness, `.feature` runner), then spike plugins fan out in parallel — github, tmux
  (`TmuxServer/Session/Window/Pane`), claude (`ClaudeSession` jsonl-backed, `ClaudeInstance` =
  pane+pid), peer, each against its own `.feature` files; lead manages, coders implement,
  review-clerk gates PRs before ready.

## 7. Open questions

Resolved by owner 2026-09-04:

- **Queue trigger**: new open issue, no label gate (§3); `grinding` label is claim-only, for dedup.
  **macOS/laptop**: one cross-compiled binary, OS-detected at start; laptop is a normal peer (§4).
- **Concurrency cap**: 1 concurrent ship worker, hard cap. Telegram ping on every worker launch.

Still open: adopt/fork [ghx](https://github.com/brunoborges/ghx) vs build our own cache — EDR
decides with numbers.

## 8. What we're not doing

- Lifecycle ops in the core — no launching or killing sessions.
- Event sourcing — no keeping events forever.
- Redis or any shared cache.
- An external federation router (Apollo/Cosmo/Bramble) — `peer` plugin instead.
- Migrating the langwatch fleet in v1.
- Gmail/Todoist in v1 (Todoist is the first post-MVP plugin).
- A TUI — consumers are existing tools.
- An alert sender inside the binary — alerting is an external poller of `/health`.
- A GitHub App (auth is a PAT), polling as primary ingest (webhooks are primary, `/notifications`
  secondary only), hand-curated field subsets (whole JSON node instead), or **owning a copy of
  GitHub** — we're a cache, not a replica.

## 9. Risks

Ranked, one line each:

1. **Complexity as rot vector** — more moving parts than the one process that already rotted. Mitigation: single-binary decision keeps each box a plain process, no router to run.
2. **External poller must exist and be verified** — a `/health` nobody polls is the old stale cache again.
3. **Forward-tunnel reliance** — `gh webhook forward` must stay supervised; hourly reconcile backstops it.
4. **Double-dispatch** — ingest-level idempotency alone already failed once; needs an actor-level claim.
