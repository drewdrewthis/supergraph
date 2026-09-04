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
- gqlgen supports Federation v2. **UNVERIFIED — spike must prove.**
- A self-hosted router exists that hot-reloads on schema changes. **UNVERIFIED — spike must prove.**
- A GitHub org-level App webhook covers new repos automatically.
- Mesh networking (WireGuard/Tailscale) already exists between boxes.
- SQLite per plugin is fast enough for this workload.
- One router is enough — no per-box routers needed.

## 5. User stories

| Story | Success metric |
|---|---|
| As the **owner**, I want to label an issue `ready` and walk away, so that a PR shows up without me watching. | Issue-to-PR-open time, no manual poke. |
| As **orchardist**, I want to query free slots across boxes, so that I can dispatch work to any idle machine. | Query returns live slot state in <1s. |
| As the **ship worker**, I want to subscribe to PR CI status, so that I react the moment a check finishes. | Subscription event fires within 1s of CI state change. |
| As the **owner**, I want to ask "what is everything touching issue #N" across GitHub/Todoist/email/sessions, so that I get one answer instead of five lookups. | Single query, all sources represented. |
| As a **developer**, I want to add a new plugin in an afternoon following the contract doc, so that the system grows without core changes. | New plugin registered with zero core edits. |
| As an **operator**, I want to see plugin lag on a health endpoint and get a Telegram alert, so that stale data never goes unnoticed again. | Alert fires before lag exceeds a defined threshold. |

**Milestones:** spike → EDR → core → plugin contract + harness → github plugin → sessions
plugin → subscriptions + health → drewdrewthis worker → todoist.

**Spike pass/fail:**
(a) chosen router + gqlgen Fed-v2 subgraph run together without forking either;
(b) a deliberately stalled plugin trips the lag/canary alert end-to-end;
(c) one cross-plugin query returns live state sub-second.
**Kill criterion:** if (a) needs a fork, the only viable router is unmaintained or
enterprise-licensed, or (b) fails → stop; ship a single-process worker (org webhook + hourly
reconcile + one SQLite) instead.

## 6. User interaction & design

High-level only — full design goes in a future EDR under `docs/edr/`.

- **Core** = router (Apollo Router candidate; Cosmo/Bramble as fallbacks — maintenance check
  pending) + a tiny ingest envelope `{ts, source, type, v, key, payload}` + schema composition CI.
- **Plugin** = its own process, own subgraph (gqlgen, Federation v2, `@key` includes `hostId` for
  per-box entities), own listener/poller, **own SQLite file** (state tables + a short auto-pruned
  event ring + cursors), own migrations at boot, and a `_health` endpoint (lastEventAt, cursor,
  lag). **Rebuildable:** delete the db, restart, reconcile refills it.
- **GitHub plugin**: the hourly `since`-cursor reconcile is the **primary correctness path** —
  it is what guarantees no issue is ever missed. Webhooks (org-level GitHub App push + boot
  redelivery) are a **latency optimization only**, not a correctness dependency. Webhook
  ingress lands on the **hetzner-agents box** (the only box with a public IP) and is an
  **accepted single point of failure**; the hourly reconcile covers its downtime.
- **Subscriptions**: the router pushes events like "issue labeled ready" to actors (e.g.
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
- **Multi-box**: one router on the WireGuard/Tailscale mesh; subgraphs bind to the mesh IP only.
  A down box reads as **unknown**, never as empty (this avoided a past double-dispatch bug).
- Ship a plugin contract doc, a template plugin, and a one-command local dev harness.

## 7. Open questions

| Question | Owner | Status |
|---|---|---|
| Router choice + maintenance status (Apollo Router vs Cosmo vs Bramble) | drewdrewthis | open |
| GitHub webhook redelivery retention window | drewdrewthis | open |
| Auth model — router token + per-plugin ingest secret? | drewdrewthis | open |
| Event-ring retention length | drewdrewthis | open |
| How orchardist/ship migrate off orchard-daemon | drewdrewthis | open |
| Naming of first plugins | drewdrewthis | open |
| Is federation justified before a second consumer exists, or should v1 be a single process? | drewdrewthis | open — decision needed before spike |

## 8. What we're not doing

- Lifecycle ops in the core — no launching or killing sessions.
- Event sourcing — no keeping events forever.
- Redis or any shared cache.
- Per-box routers.
- Migrating the langwatch fleet in v1.
- Gmail/Todoist in v1 (Todoist is the first post-MVP plugin).
- A TUI — consumers are existing tools.

## 9. Risks

Ranked, one line each:

1. **Complexity as rot vector** — v2 has many more moving parts than the one process that
   already rotted. Mitigation: kill criterion + single-process fallback.
2. **Router choice unproven** — no chosen router has been run against gqlgen Fed-v2 yet.
3. **Freshness alerting unproven** — the canary/lag alert path has never fired for real.
4. **Webhook ingress SPOF** — GitHub webhook ingress lands on one box (hetzner-agents, the
   only public IP).
5. **Double-dispatch** — ingest-level idempotency alone already failed once; needs an
   actor-level claim.
