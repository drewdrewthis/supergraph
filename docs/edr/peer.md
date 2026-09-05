# EDR: peer plugin — multi-box federation as a lazy mirror

Engineering Design Record. Owns internals the [PRD](../PRD.md) §6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.

**Live design (2026-09-05).** Multi-box = the `peer` plugin (PRD §6, §8: "No external federation
router — `peer` plugin instead"). A box sees another box's data by running a `peer` plugin that (1)
**pull-proxies** named queries to the remote's per-plugin executors (`/plugins/<name>/graphql`,
github-style HTTPRoutes seam), (2) tracks each remote's liveness by a **WS-client subscription to the
remote's already-existing base `/graphql` `pluginLag`** stream, and (3) mirrors what it fetches into its
**own SQLite**, serving it back tagged `@hostId` + `lastSeenAt`. Duplicated on purpose — a cache, not a
shared source of truth (PRD §6). A down box reads as **stale since T**, never empty (F8).

**The design needs ZERO core change.** It uses only the plugin contract's outbound freedom (a plugin
may open any client connection from `Start`), its own SQLite, `emit`, `HTTPRoutes`, and the
`extend type Query` seam. Two *optional* core seams are listed below (§Core change requests) for a
future push-based variant; **neither is required for the spike.**

**Hard constraints (owner):** ≤ **700 LOC prod** for `plugins/peer/**` excluding tests and
`internal/fakepeer`; zero core diff; per-peer bearer token; **loop rule — a peer plugin never
re-serves peer-tagged rows** (PRD §6); F8 stale-since ≤ 30 s; `@local` boots **two** supergraph
processes on loopback (the harness already supports per-scenario binary+port).

---

## The core seam problem, stated up front
A plugin's `Start` receives only `emit` (outbound). Core does **not** hand a plugin a reader over the
local bus or over *other* plugins' envelopes (`sv.Bus()`, `EventsChannel` are core/server internals).
The base `/graphql` exposes `query{ health, ping }`, `subscription{ pluginLag }`, and each plugin's own
`extend`-ed fields — but **no unified "stream every envelope" subscription** (the wired
`core.EventsChannel`/`Resolver.Events` func is bound only to per-plugin `<name>Events` fields, e.g.
`templateEvents`). So a **remote box cannot be asked to push its full envelope stream** without a core
change. This single fact drives the pull-vs-push decision (D2).

---

## Decisions (options → recommendation)

### D1 — Where the peer list lives
| Option | Core change | Note |
|---|---|---|
| **A (rec)** peers under `[plugins.peer]` Raw (`[[plugins.peer.peers]]` host/url/token/hostId) | **none** — plugin reads `cfg.Raw` | duplicates the reserved top-level `[peers]`, but keeps core locked |
| B plumb `core.Config.Peers` → `PluginConfig` | CCR-2 | avoids duplication; core is LOCKED, own PR |

**Recommend A.** `core.Config.Peers` is today "unused beyond carrying the field" (`core/config.go:29`);
the peer plugin reads its own peers from Raw. Leave top-level `[peers]` reserved.

### D2 — How a box subscribes to a peer's event stream — **pull, not push**
| Option | Core change | Freshness | Verdict |
|---|---|---|---|
| **A (rec)** WS-client to remote base `/graphql` **`pluginLag(thresholdSeconds)`** for liveness + **lazy pull** of data on query | **none** (`pluginLag` already in `base.graphqls`) | data mirrored on read/refresh; liveness sub-second via `pluginLag` push + WS-drop | ships the spike |
| B dedicated `/plugins/peer/stream` (SSE/WS) re-broadcasting the remote's whole bus | **CCR-1 + a bus-read seam** AND both boxes must run `peer` | true push of every envelope | over-scoped, two core changes |
| C subscribe to each remote plugin's `<name>Events` field | none, but must know remote's plugin set + each bespoke payload type | push per known plugin | fragile, non-generic |

**Recommend A.** No spike AC needs remote→local *push*: the orchardist subscribes to **local**
`issueOpened` on its own box; the cross-box ACs are **S2** (warm query < 1 s) and **F8** (detect a
stopped peer < 30 s) — both a query + a liveness signal, which `pluginLag` + WS-drop deliver with zero
core change. Push (Option B) is a real future feature; it is gated on CCR-1 and is **not** built now.
The tradeoff owned honestly: a mirrored value can be stale between pulls — exactly the cache semantics
the PRD chose ("Duplicated on purpose").

### D3 — How data is mirrored and served (query fan-out)
- **Fan-out:** the peer plugin serves its own `/plugins/peer/graphql` executor (github HTTPRoutes
  pattern) and an `extend type Query { peers: [Peer!]! }` field. A named op targeting a host
  (`--op freeSlots --var host=<hostId>`) is **proxied to that peer's `/plugins/<targetPlugin>/graphql`**
  over the mesh, the returned nodes are cached in `peer_nodes` keyed by their **original key** (which
  already carries `@<remoteHostId>` per the envelope contract), stamped `lastSeenAt`, and served back.
- **Key-grammar routing:** the router reads the `@<hostId>` suffix. `@localHostId` (or none) → not
  peer's concern (local plugins answer). `@<knownRemote>` → proxy/serve from mirror. `@<unknown>` →
  empty + logged (never invents a hop).
- **Warm path (S2):** a cached row is served from `peer_nodes` with **no upstream call** → local read,
  < 1 s.

### D4 — Re-emit preserves origin `@hostId`
For every node mirrored or refreshed, emit **one** local envelope:
`{TS:now, Source:"peer", Type:"peer.node.mirrored", V:1, Key:<original key, incl. @remoteHostId>,
Payload:{peer:<hostId>, node:<json>, lastSeenAt}}`. Effects: core `/health` "peer" `lastEventAt`
advances (liveness derived from the emit path, per contract §Health), and cross-plugin joins key on the
**same** `issue:o/r#5` / `tmux:...@host` grammar the origin used — the mirror is join-compatible.

### D5 — Loop prevention (PRD "never re-serves peer-tagged rows"; F8 "no peer-of-peer rows")
Two guards, both enforced in `mirror.go`:
1. **Never proxy to a remote's `peer` plugin** — the fan-out only targets a remote's *source* plugins
   (github/tmux/claude/template), never `/plugins/peer/graphql`.
2. **Drop foreign-host rows** — any fetched node whose key `@hostId` ≠ the remote's declared `hostId`
   (i.e. it is the remote's own mirror of a *third* box) is dropped, not stored. Also drop keys whose
   `@hostId` == the local host. Together these guarantee the mirror holds only first-hop rows → no
   peer-of-peer rows (F8), no cycle.

### D6 — Auth — per-peer bearer
Each `[[plugins.peer.peers]]` carries `token`. It is sent as `Authorization: Bearer <token>` on **both**
the `pluginLag` WS handshake and every query proxy. The remote's mesh endpoint is a **non-loopback
bind**, so core already *mandates* tokens there (`core/config.go:72`) and enforces bearer on every route
including `/health` (`server/server.go:103`). A 401 marks that peer unreachable → `staleSince` set,
nothing mirrored. Token never logged.

### D7 — Reconnect / backoff / staleness
`liveness.go` runs one goroutine per peer: dial the `pluginLag` WS, on each push (or a fallback
`query{ health }` poll at `livenessPollSeconds`) set `lastSeenAt=now`, `staleSince=null`. On WS
drop/dial-fail/401: **exponential backoff** (like github's `gh webhook forward` supervisor), and keep
`staleSince` at the first-failure time. A peer whose `lastSeenAt` age or reported `pluginLag` crosses
`staleThresholdSeconds` (default 30, F8) reads **`stale since T`** in `peers`. Mirror rows survive a
stale peer (served with their own `lastSeenAt`), they are just flagged.

---

## Schema (`plugins/peer/schema/peer.graphqls`, `extend` only — zero core diff)
```graphql
extend type Query {
  peers: [Peer!]!                 # liveness + last-seen per configured peer
}
type Peer {
  hostId: String!
  url: String!
  lastSeenAt: Time
  staleSince: Time                # non-null ⇒ stale since T (F8)
  lagSeconds: Float!              # remote's own worst pluginLag, mirrored
  mirroredKeys: Int!             # rows currently cached for this peer
}
```
Data fan-out (issues/slots/tmux rows across a box) rides the `/plugins/peer/graphql` **executor**
(named-op + `@hostId` routing), not a bespoke typed field per source — the mirror is source-agnostic,
keyed by the origin's grammar.

## Envelopes emitted (`{Source:"peer",V:1,...}`)
- **`peer.node.mirrored`** — `Key`=original `@remoteHostId` key, `Payload={peer,node,lastSeenAt}` — a
  node was fetched/refreshed from a peer. Drives `/health` "peer" `lastEventAt`.
- **`peer.stale`** — `Key=peer:<hostId>`, `Payload={staleSince}` — a peer crossed the stale threshold
  (F8 signal; also lets a local poller alert).

## SQLite state (`plugins/peer/peer.db`, add-only, `IF NOT EXISTS`)
- `peer_nodes(key TEXT PK, peer_host, node_json, last_seen_at)` — the mirror; `key` carries
  `@remoteHostId`, so `peer_host` is derivable but stored for cheap per-peer scan/evict.
- `peer_state(host TEXT PK, url, last_seen_at, stale_since, lag_seconds)` — per-peer liveness, source
  of the `peers` query and staleness. (Per-peer liveness cannot live in core `/health`, which is
  per-*plugin*, not per-peer — core `/health` shows one "peer" entry for the plugin's overall liveness.)
- cursors: none required (pull-through is stateless between reads); `livenessCursor` optional via
  `CursorReporter` returning `peers=<n> stale=<m>`.

## Config keys `[plugins.peer]`
`[[plugins.peer.peers]]` × N: `hostId`, `url` (mesh, e.g. `http://10.0.3.222:7788`), `token`.
Plugin-level: `staleThresholdSeconds` (30, F8), `livenessPollSeconds` (10, fallback when no `pluginLag`
push), `backoffMaxSeconds` (60), `mirrorTTLSeconds` (0 = serve until a refresh; a churny deployment may
cap it). `baseURLOverride` (test-only → fakepeer).

## LOC budget (prod, cap **700**; tests + `internal/fakepeer` excluded)
| File (`plugins/peer/`) | Budget | Responsibility |
|---|---:|---|
| `peer.go` | 150 | wiring: `Register`/`New`/`Name`/`Migrate`/`Start`/`HTTPRoutes`/`CursorReporter`/config parse |
| `client.go` | 130 | outbound GraphQL POST + `pluginLag` WS subscribe (graphql-transport-ws) + bearer |
| `store.go` | 150 | `peer_nodes`+`peer_state`: upsert, get, per-peer scan, foreign-host drop, stale set |
| `mirror.go` | 140 | pull-through proxy: `@hostId` routing, loop guards (D5), tag + re-emit |
| `liveness.go` | 100 | per-peer WS/poll loop, backoff, `staleSince` transitions, `peer.stale` emit |
| resolver stub (`graph/peer.resolvers.go`, delegates) | 30 | `peers` → registry → plugin |
| **Total** | **700** | schema `.graphqls` not counted; CLI reuses github's `--op/--var` (cmd/ only) |
- **AC-PEER-LOC** guards it (same `sed` strip formula as `make loc-github`), fails > 700.

---

## Core change requests (NONE required; two OPTIONAL, each its own PR)
Core is LOCKED; the recommended design touches **zero** core files. Listed only for a future push variant:

- **CCR-1 (optional, only for D2 Option B push):** expose a base-schema envelope subscription.
  `core.EventsChannel` + `Resolver.Events` are **already wired** in `server/server.go:45` but bound to
  no base field. Minimal diff: add to `core/graph/schema/base.graphqls`
  ```graphql
  type EnvelopeMsg { ts: Time!  source: String!  type: String!  v: Int!  key: String!  payload: String! }
  extend type Subscription { events(source: String): EnvelopeMsg! }
  ```
  + a ~6-line resolver delegating to `r.Events`, + regen. ~15 lines net. **Not needed for the spike.**
- **CCR-2 (optional, only for D1 Option B):** pass peers to plugins. Add `Peers []Peer` to
  `core.PluginConfig` and one line in `Config.PluginConfigFor` (`core/config.go:117`). ~3 lines.
  **Not needed** — D1-A reads peers from Raw.

## Failure modes
- Peer process down → WS drop → backoff reconnect; `staleSince` set < 30 s (F8); mirror still served, flagged stale.
- Wrong/missing token → 401 → peer unreachable, `staleSince` set, nothing mirrored.
- Remote returns a foreign-host row (its own mirror of a 3rd box) → dropped (D5-2); no peer-of-peer rows.
- Remote schema/op unknown → empty result + log; never a hop invention.
- Clock skew on `lastSeenAt` → staleness is age-based on the local clock only (never trusts remote ts for the threshold).
- Mesh partition heals → next `pluginLag` push or poll clears `staleSince`, next query re-fills mirror.

---

## Acceptance criteria
PRD ACs claimed by peer: **S2** (cross-box free-slot query p95 < 1 s warm) and **F8** (stopped box shows
`stale since T` < 30 s, no peer-of-peer rows). Plugin ACs (`@AC-PEER-*`) are the BDD contract in
`features/peer.feature`. Summary of the set (all `@local`, two loopback processes unless noted):

- **@S2** cross-box free-slot query p95 < 1 s warm over N≥20, seeded mirror.
- **@F8** stop the remote → consumer `peers` shows `staleSince` < 30 s; mirrored rows still readable, flagged; **no peer-of-peer rows**.
- **@AC-PEER-MIRROR** a proxied named op returns remote rows tagged `@remoteHostId` + `lastSeenAt`, cached in `peer_nodes`.
- **@AC-PEER-REEMIT** each mirrored node emits `peer.node.mirrored` preserving the original `@remoteHostId` key; core `/health` "peer" `lastEventAt` advances.
- **@AC-PEER-LOOP** the remote holds a foreign-host row (its own mirror of box C); the consumer drops it and never proxies the remote's `/plugins/peer/graphql` — negative control for F8's "no peer-of-peer rows".
- **@AC-PEER-AUTH** remote bound at `:PORT` (non-loopback ⇒ core mandates tokens): correct bearer mirrors; wrong/absent token → 401 → peer marked unreachable, nothing mirrored.
- **@AC-PEER-RECONNECT** remote stopped then restarted; backoff reconnects; `staleSince` clears and the mirror refreshes on the next query.
- **@AC-PEER-HEALTH** `/health` shows a "peer" entry; with every peer unreachable, its lag grows and state → `stale`, derived only from the emit path.
- **@AC-PEER-ZEROCORE** `git diff --stat core/` = 0 files (integration/diff check, not two-process).
- **@AC-PEER-LOC** `plugins/peer/**` prod LOC ≤ 700.

## AC review: drafted, pending ac-reviewer
Sharpness applied per `~/.knowledge/modules/shared/records/principles/acceptance-criteria.md`: each AC
is falsifiable (F8 asserts a *bound* < 30 s **and** a negative "no peer-of-peer rows"; AC-PEER-AUTH
carries both the pass and the 401 branch; AC-PEER-MIRROR asserts the tag **and** the cache write).
Negative controls: AC-PEER-LOOP (foreign rows dropped), AC-PEER-AUTH (401 branch). Evidence shapes are
harness log lines + query results, not "journald" (the `@local` godog harness emits log lines).
<!-- ACs ready for ac-reviewer -->

---

## Coder step plan — file-partitioned, one wave (no shared-file contention)
1. `plugins/peer/internal/fakepeer/` — a stand-in remote: an httptest server exposing `/graphql`
   (`health`, `pluginLag` WS push, seedable), `/plugins/<name>/graphql` (returns seeded nodes incl. a
   plantable **foreign-host** row for AC-PEER-LOOP), settable stop/401. *Everything tests against it.*
   (Or, for the two-real-process @local runs, boot an actual second `supergraph` binary — harness
   supports per-scenario binary+port; fakepeer is for the unit/fast paths.)
2. `plugins/peer/store.go` + `Migrate` (two tables) — mirror + per-peer state.
3. `plugins/peer/client.go` — GraphQL POST + `pluginLag` WS subscribe + bearer.
4. `plugins/peer/mirror.go` — pull-through proxy, `@hostId` routing, loop guards (D5), tag + re-emit.
5. `plugins/peer/liveness.go` — per-peer WS/poll loop, backoff, `staleSince`, `peer.stale` emit.
6. `plugins/peer/peer.go` — wiring (`Register`/`Start`/`HTTPRoutes`/`CursorReporter`); blank import in
   `graph/plugins_import.go`; `plugins/peer/queries/*.graphql` (reuse `freeSlots`, add nothing github owns).
7. `plugins/peer/schema/peer.graphqls` + regen `graph/`; fill `peers` resolver (delegates via registry).
8. `make loc-peer` CI step (AC-PEER-LOC) + `git diff --stat core/`=0 (AC-PEER-ZEROCORE).
9. `features/steps_peer_test.go` — boots two processes on loopback (one at `:PORT` for AUTH), signed
   proxy calls, `pluginLag` drive, stop/restart, p95 harness for S2 (N≥20), loop-guard + no-peer-of-peer
   assertions. Depends on 2–7 compiling.

Steps 2–5 are file-disjoint after step 1; `mirror.go` (step 4) is the judgment-heavy core.

## Handoff
- ACs ready for ac-reviewer (see §Acceptance criteria + `features/peer.feature`).
- Implementation → coder per the step plan above; `mirror.go` (step 4, loop-guard + routing) is the
  judgment-heavy core (advanced-coder if the loop-guard contract is co-designed with the harness).
