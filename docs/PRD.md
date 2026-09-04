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

Today's system, `orchard-daemon` (Go, gqlgen — see ADR-009 in the knowledge repo), has drifted:
mutations turned into actions, the pickup loop bypasses it and reads a GitHub Projects board via
shell, its cache went a month stale unnoticed, and it only covers one repo at a time (ADR-009 rule
#6 forbids `gh` enumeration, so it can't scan the org).

**First consumer:** an always-on worker. Any new open GitHub issue in the `drewdrewthis` org
(no label needed) → orchardist sees it sub-second → claims it (assignee/`grinding` label, for
dedup only) → launches the `ship` skill. The **ship worker** owns the PR from there: it
subscribes to `checkRunUpdated` and drives its own PR to green (review-clerk + tests gate
before ready). The **orchardist** only dispatches and watches slots — it does not drive CI.
Owner pinged on Telegram when the PR is ready.

The owner keeps hitting **GitHub API rate limits**. The supergraph fixes this with webhooks,
anchored lookups, and caching — that's the quota fix.

**Long-term:** wire in Todoist, Gmail, Calendar, Slack, Discord. The real value is **cross-source
joins** — issue ↔ task ↔ email ↔ running session ↔ PR — in one query.

## 4. Assumptions

- Go stays the implementation language. **One Go binary, cross-compiled, OS-detected at
  start; the laptop is a normal peer. Only the two spike boxes are Linux.**
- A GitHub org-level App webhook covers new repos automatically.
- Mesh networking (WireGuard/Tailscale) already exists between boxes.
- SQLite per plugin is fast enough for this workload.
- gqlgen subscriptions over the mesh are sufficient for peer mirroring. **UNVERIFIED — spike must prove.**

## 5. User stories

- As the **owner**, I want to open an issue and walk away, so that a PR shows up without me watching.
- As **orchardist**, I want to query free slots across boxes, so that I can dispatch work to any idle machine.
- As the **ship worker**, I want to subscribe to PR CI status, so that I react the moment a check finishes.
- As the **owner**, I want to ask "what is everything touching issue #N" across GitHub/Todoist/email/tmux/claude, so that I get one answer instead of five lookups.
- As a **developer**, I want to add a new plugin (a Go package, not a process) in an afternoon following the contract doc, so that the system grows without core changes.
- As an **operator**, I want to see plugin lag on a health endpoint and get a Telegram alert, so that stale data never goes unnoticed again.

**Acceptance criteria:**

- **S1** — p95 time from issue-open to PR-open ≤ 15 min over 10 seeded issues, zero human action, including the review-clerk + tests gate. Evidence: journald timestamps + PR-open screenshots.
- **S2** — cross-box free-slot query (tmux plugin) p95 < 1 s warm, seeded realistic db. Evidence: timing screenshot.
- **S3** — p95 < 1 s from check-run webhook receipt to subscription push. Evidence: paired journald lines (webhook-in ts, push-out ts).
- **S4** — one query for issue #N returns ≥ 1 row each from github + claude (`ClaudeSession`) with source tags; an absent source shows an explicit stale marker, never omission. Evidence: query result screenshot.
- **S5** — template plugin compiles in and serves `_health` with `git diff --stat core/` = 0 files. Evidence: diff output + `_health` screenshot.
- **S6** — Telegram alert lands < 60 s after plugin lag crosses 5 min. Evidence: Telegram screenshot + lag log.

### Failure-surface ACs

- **F1 Freshness** — p95 ingest-to-queryable < 1 s for github and claude events. Evidence: paired log timestamps.
- **F2 Reconcile** — a deliberately dropped webhook is present after the next hourly reconcile; org backfill from empty db completes < 60 min. Evidence: log + timer.
- **F3 New repo** — a repo created in the org after boot appears in the graph with zero config within one reconcile. Evidence: query screenshot.
- **F4 Double-dispatch** — two orchestrators racing on one new issue (claim via assignee/`grinding` label) yield exactly one ship run; the loser sees the claim. Evidence: two logs + one PR.
- **F5 Panic isolation** — a plugin that panics (e.g. the claude plugin) shows `stale`; `_health` and the other plugins (including tmux) keep answering. Evidence: `_health` screenshot after induced panic.
- **F6 Alert positive-fire** — the canary/alert path delivers a real Telegram message when armed (control test), not only on stall. Evidence: screenshot.
- **F7 Quota** — GitHub API calls stay under 500/hour at steady state with 20 repos. Evidence: rate-limit header log.
- **F8 Stale peer** — a stopped box shows `stale since T` on its peers (tmux + claude data included) < 30 s after stop; no peer-of-peer rows exist. Evidence: query screenshot + grep.

**Milestones:** spike → EDR → core → plugin contract + harness → github plugin → tmux plugin →
claude plugin → subscriptions + health → drewdrewthis worker → todoist.

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
  **Core is locked before any plugin starts; plugin internals (fsnotify vs poll, socket paths,
  cursors) are decided in each plugin's EDR, not here.**
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
- **Subscriptions**: the binary pushes events like "issue opened" to actors (e.g.
  orchardist). Actors own all side effects — the supergraph never does.
- **Actor-level claim**: before acting on a new issue, the worker must claim it (assign
  self / add `grinding` label) and re-check, not just trust ingest dedup. Ingest-level
  idempotency alone is not enough — a prior double-dispatch incident got through on ingest
  idempotency.
- **Canary**: every plugin pushes a synthetic canary event end-to-end on a timer. A missing
  canary triggers a Telegram alert. A canary alert that never fires is treated as **broken**,
  not as "all clear."
- **Versioning**: event payload carries `v` + upcasters (never rewrite the log); schema changes
  are add-only with `@deprecated`; envelope path is versioned; schema-diff check runs in CI.
- Ship a [plugin contract doc](docs/plugin-contract.md) (stub, written in the core tier of the
  spike), a template plugin, and a one-command local dev harness.
- **CLI + install**:

  ```
  supergraph serve                 # foreground, logs to stdout (dev)
  supergraph install | uninstall   # writes systemd --user unit (Linux) or launchd plist (macOS)
  supergraph start | stop | restart | status
  supergraph query '<graphql>'     # against local endpoint
  ```

  Local endpoint `http://127.0.0.1:7788/graphql`; `_health` at `/health`; peer endpoint bound to
  the mesh IP only. Logs: `journalctl --user -u supergraph` (Linux) / `log show --predicate
  'process == "supergraph"'` (macOS). Config: `~/.config/supergraph/config.toml` (hostId, peers,
  tokens); per-plugin SQLite under `~/.local/share/supergraph/<plugin>.db`. **Upgrade**:
  `install` is idempotent; `restart` after replacing the binary.
- **Schema sketch** (illustrative, not final — the EDR owns the real schema):
  ```graphql
  type Issue { repo, number, title, state, labels, updatedAt, hostId, source, staleSince }
  type TmuxSession { hostId, name, worktree, branch, lastSeenAt, staleSince }
  type ClaudeSession { hostId, uuid, cwd, issueNumber, lastSeenAt, staleSince }
  type ClaudeInstance { hostId, pane, pid, tmuxSession: TmuxSession, claudeSession: ClaudeSession, lastSeenAt, staleSince }
  type Slot { hostId, kind, free: Boolean, staleSince }
  type Query { issue(repo, number), issues(repo, state), tmuxSessions(hostId), claudeSessions(hostId), slots(hostId), touching(issueNumber): [Node] }
  type Subscription { issueOpened(org), checkRunUpdated(repo, prNumber), pluginLag(threshold) }
  type Health { plugin, lastEventAt, cursor, lagSeconds, state }
  ```
  Node naming follows ADR-009's tool-prefix convention (`Tmux*`, `Claude*`, `Github*`).
- **Plugin isolation**: each plugin runs in its own goroutine with `recover()`; a panic marks
  the plugin `stale`, binary and other plugins keep serving.
- **Auth**: per-box bearer token (env) for peer subscribe; HMAC on the GitHub webhook. mTLS
  deferred.
- **Peer loop rule**: a `peer` plugin never re-serves peer-tagged rows.
- **First cross-plugin join key**: issue number ↔ branch/worktree. Full node taxonomy deferred.
- **Backfill**: org-wide first boot is measured in the spike; hourly reconcile must complete
  inside one hour or the design fails.
- **Logs**: structured to journald with rotation. **Alerts**: Telegram, named bot + chat id —
  **day-1 blocker, owner: drewdrewthis, needed before spike test (b)**.
- **Tests (BDD)**: every plugin ships `.feature` files; the scenario ↔ e2e bijection from ADR-009
  is the contract, carried forward, not retired. S1–S6 and F1–F8 (§5) are the first scenarios.
- **Build plan**: tiered. Core first — ingest envelope, plugin contract, SQLite base, GraphQL
  server, `_health`, dev harness, `.feature` test runner. Then plugins fan out in parallel — github,
  tmux (`TmuxServer/Session/Window/Pane`, hostId-keyed), claude (`ClaudeSession` durable jsonl-backed;
  `ClaudeInstance` = pane + pid), peer, telegram alert — each against its own `.feature` files.
  Lead manages the build, coders implement, review-clerk gates PRs before ready.
- **Spike boxes**: hetzner-agents + langwatch-dev (Linux/systemd) — the only two Linux boxes in
  the spike. One Go binary, cross-compiled, OS-detected at start; the laptop is a normal peer.
- **"Sub-second"** = p95, warm cache, against a seeded db of realistic size.

## 7. Open questions

None open — resolved by owner 2026-09-04:

- **Queue trigger**: new open issue, no label gate (§3). Assignee/`grinding` label stays as the
  claim mechanism, for dedup only.
- **macOS/laptop**: one Go binary, cross-compiled, OS-detected at start; the laptop is a normal peer.
- **Concurrency cap**: 1 concurrent ship worker, hard cap. Telegram ping on every worker launch.

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
