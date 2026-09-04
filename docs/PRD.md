# Supergraph PRD

**Status:** living doc — links over prose, keep this to one page.

## 1. Project specifics

- **Owner:** drewdrewthis
- **Status:** PRD draft
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

Today's system, `orchard-daemon` (Go, gqlgen — see ADR-009 in the knowledge repo), has drifted:
mutations turned into actions, the pickup loop bypasses it and reads a GitHub Projects board via
shell, its cache went a month stale unnoticed, and it only covers one repo at a time (ADR-009 rule
#6 forbids `gh` enumeration, so it can't scan the org).

**First consumer:** an always-on worker. A GitHub issue in the `drewdrewthis` org gets label
`ready` → orchardist sees it sub-second → launches the `ship` skill → PR driven to green → owner
pinged on Telegram.

The owner keeps hitting **GitHub API rate limits**. The supergraph fixes this with webhooks,
anchored lookups, and caching — that's the quota fix.

**Long-term:** wire in Todoist, Gmail, Calendar, Slack, Discord. The real value is **cross-source
joins** — issue ↔ task ↔ email ↔ running session ↔ PR — in one query.

## 4. Assumptions

- Go stays the implementation language.
- A GitHub org-level App webhook covers new repos automatically.
- Mesh networking (WireGuard/Tailscale) already exists between boxes.
- SQLite per plugin is fast enough for this workload.
- gqlgen subscriptions over the mesh are sufficient for peer mirroring. **UNVERIFIED — spike must prove.**

## 5. User stories

- As the **owner**, I want to label an issue `ready` and walk away, so that a PR shows up without me watching.
- As **orchardist**, I want to query free slots across boxes, so that I can dispatch work to any idle machine.
- As the **ship worker**, I want to subscribe to PR CI status, so that I react the moment a check finishes.
- As the **owner**, I want to ask "what is everything touching issue #N" across GitHub/Todoist/email/sessions, so that I get one answer instead of five lookups.
- As a **developer**, I want to add a new plugin (a Go package, not a process) in an afternoon following the contract doc, so that the system grows without core changes.
- As an **operator**, I want to see plugin lag on a health endpoint and get a Telegram alert, so that stale data never goes unnoticed again.

**Acceptance criteria:**

- **S1** — p95 time from `ready` label to PR-open ≤ 15 min over 10 seeded issues, zero human action. Evidence: journald timestamps + PR-open screenshots.
- **S2** — cross-box free-slot query p95 < 1 s warm, seeded realistic db. Evidence: timing screenshot.
- **S3** — p95 < 1 s from check-run webhook receipt to subscription push. Evidence: paired journald lines (webhook-in ts, push-out ts).
- **S4** — one query for issue #N returns ≥ 1 row each from github + sessions with source tags; an absent source shows an explicit stale marker, never omission. Evidence: query result screenshot.
- **S5** — template plugin compiles in and serves `_health` with `git diff --stat core/` = 0 files. Evidence: diff output + `_health` screenshot.
- **S6** — Telegram alert lands < 60 s after plugin lag crosses 5 min. Evidence: Telegram screenshot + lag log.

### Failure-surface ACs

- **F1 Freshness** — p95 ingest-to-queryable < 1 s for github and sessions events. Evidence: paired log timestamps.
- **F2 Reconcile** — a deliberately dropped webhook is present after the next hourly reconcile; org backfill from empty db completes < 60 min. Evidence: log + timer.
- **F3 New repo** — a repo created in the org after boot appears in the graph with zero config within one reconcile. Evidence: query screenshot.
- **F4 Double-dispatch** — two orchestrators racing on one `ready` issue yield exactly one ship run; the loser sees the claim. Evidence: two logs + one PR.
- **F5 Panic isolation** — a plugin that panics shows `stale`; `_health` and the other plugins keep answering. Evidence: `_health` screenshot after induced panic.
- **F6 Alert positive-fire** — the canary/alert path delivers a real Telegram message when armed (control test), not only on stall. Evidence: screenshot.
- **F7 Quota** — GitHub API calls stay under 500/hour at steady state with 20 repos. Evidence: rate-limit header log.
- **F8 Stale peer** — a stopped box shows `stale since T` on its peers < 30 s after stop; no peer-of-peer rows exist. Evidence: query screenshot + grep.

**Milestones:** spike → EDR → core → plugin contract + harness → github plugin → sessions
plugin → subscriptions + health → drewdrewthis worker → todoist.

**Spike pass/fail:**
(a) cross-box free-slot query meets **S2** (p95 < 1 s warm, seeded realistic db);
(b) canary/lag alert meets **S6** and passes **F6** (real Telegram message on a control-armed test, not just on stall);
(c) a stopped peer box meets **F8** (`stale since T` within 30 s, no peer-of-peer rows).
**Kill criterion:** if (b) fails → stop.

## 6. User interaction & design

High-level only — full design goes in a future EDR under `docs/edr/`.

- **No router, no federation.** One binary per box. Plugins are Go packages compiled into that
  binary, not separate processes. The binary serves **one GraphQL endpoint** (gqlgen) per box.
- **Core** = the binary shell + a tiny ingest envelope `{ts, source, type, v, key, payload}`.
- **Plugin** = a Go package with a fixed contract: same envelope, own listener/poller,
  **own SQLite file** (state tables + a short auto-pruned event ring + cursors), own migrations
  at boot, and a `_health` endpoint (lastEventAt, cursor, lag). Entity keys keep `hostId` for
  per-box identity. **Rebuildable:** delete the db, restart, reconcile refills it.
- **Multi-box = the `peer` plugin.** To see another box's data, a box runs a `peer` plugin that
  queries + subscribes to that box's normal GraphQL endpoint over the WireGuard/Tailscale mesh,
  mirrors what it gets into its own SQLite, and serves it back tagged with `hostId` +
  `lastSeenAt`. Data is duplicated per box on purpose — it's a cache, not a shared source of
  truth. A down box reads as **stale since T** (from `lastSeenAt`), never as empty.
- **GitHub plugin**: the hourly `since`-cursor reconcile is the **primary correctness path** —
  it is what guarantees no issue is ever missed. Webhooks (org-level GitHub App push + boot
  redelivery) are a **latency optimization only**, not a correctness dependency. Webhook
  ingress lands on the **hetzner-agents box** (the only box with a public IP) and is an
  **accepted single point of failure**; the hourly reconcile covers its downtime.
- **Subscriptions**: the binary pushes events like "issue labeled ready" to actors (e.g.
  orchardist). Actors own all side effects — the supergraph never does.
- **Actor-level claim**: before acting on a `ready` issue, the worker must claim it (assign
  self / add `grinding` label) and re-check, not just trust ingest dedup. Ingest-level
  idempotency alone is not enough — a prior double-dispatch incident got through on ingest
  idempotency.
- **Canary**: every plugin pushes a synthetic canary event end-to-end on a timer. A missing
  canary triggers a Telegram alert. A canary alert that never fires is treated as **broken**,
  not as "all clear."
- **Versioning**: event payload carries `v` + upcasters (never rewrite the log); schema changes
  are add-only with `@deprecated`; envelope path is versioned; schema-diff check runs in CI.
- Ship a plugin contract doc, a template plugin, and a one-command local dev harness.
- **Plugin isolation**: each plugin runs in its own goroutine with `recover()`; a panic marks
  the plugin `stale`, binary and other plugins keep serving.
- **Auth**: per-box bearer token (env) for peer subscribe; HMAC on the GitHub webhook. mTLS
  deferred.
- **Peer loop rule**: a `peer` plugin never re-serves peer-tagged rows.
- **First cross-plugin join key**: issue number ↔ branch/worktree. Full node taxonomy deferred.
- **Backfill**: org-wide first boot is measured in the spike; hourly reconcile must complete
  inside one hour or the design fails.
- **Logs**: structured to journald with rotation. **Alerts**: Telegram, named bot + chat id
  (to be filled).
- **Tests**: plain Go tests per plugin + one end-to-end per plugin. ADR-009's `.feature`
  bijection gate is retired.
- **Spike boxes**: hetzner-agents + langwatch-dev (Linux/systemd). macOS is not a v1 target.
- **"Sub-second"** = p95, warm cache, against a seeded db of realistic size.

## 7. Open questions

**Owner decision pending** — each row shows the recommended default:

| Question | Recommended default | Owner | Status |
|---|---|---|---|
| Queue source for drewdrewthis: `ready` label vs a Projects board? | `ready` label | drewdrewthis | owner decision pending |
| macOS/laptop in v1? | No | drewdrewthis | owner decision pending |
| Unattended Opus cap — how many concurrent ship workers? | 1 concurrent, hard cap, Telegram ping on every launch | drewdrewthis | owner decision pending |

## 8. What we're not doing

- Lifecycle ops in the core — no launching or killing sessions.
- Event sourcing — no keeping events forever.
- Redis or any shared cache.
- An external federation router (Apollo/Cosmo/Bramble) — `peer` plugin instead.
- Migrating the langwatch fleet in v1.
- Gmail/Todoist in v1 (Todoist is the first post-MVP plugin).
- A TUI — consumers are existing tools.

## 9. Risks

Ranked, one line each:

1. **Complexity as rot vector** — more moving parts than the one process that already rotted.
   Mitigation: single-binary decision keeps each box a plain process, no router to run.
2. **Freshness alerting unproven** — the canary/lag alert path has never fired for real.
3. **Webhook ingress SPOF** — GitHub webhook ingress lands on one box (hetzner-agents, the
   only public IP).
4. **Double-dispatch** — ingest-level idempotency alone already failed once; needs an
   actor-level claim.
