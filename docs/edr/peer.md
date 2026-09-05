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

**Hard constraints (owner):** ≤ **690 LOC prod** for `plugins/peer/**` excluding tests (measured 649
+ 5%; the 700 was an estimate, not a cap); the seeded remote lives in `plugins/fakeremote` (a separate
harness-only package, not counted); **zero `core/` + `server/` diff**; per-peer bearer token; **loop rule — a peer plugin never
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
cap it). `remotePlugin` (test-only override → routes the whole fan-out at the seeded `fakeremote`
executor; production leaves it empty and routes per op via `mirror.go`'s `opPlugins`). `mirrorTTLSeconds`
is reserved (not consumed in the spike — the warm read serves until an explicit `refresh`).

## LOC budget (prod; measured actuals — cap **690** for `plugins/peer/**`; tests excluded)
The 700-line estimate was an owner-blessed estimate, not a cap. Measured actuals below; the
`make loc-peer` gate is set to **690** = measured 649 + 5% rounded up to a multiple of 10 (per the
owner's LOC-budget rule). `graph/peer.resolvers.go` lives outside `plugins/peer/` so `make loc-peer`
does not count it; it is listed for completeness against the estimate.

| File (`plugins/peer/`) | Est. | Actual | Responsibility |
|---|---:|---:|---|
| `peer.go` | 150 | **230** | wiring: `Register`/`New`/`Name`/`Migrate`/`Start`/`Routes`/`Cursor`/config parse **+ the HTTP executor (warm-read vs proxy, `servedResult` wire shape) + the `Peers` graph accessor** — the executor and the resolver-accessor were folded here rather than a 6th file, which is where the +80 over estimate lands |
| `store.go` | 150 | **142** | `peer_nodes`+`peer_state`: upsert, per-host scan, foreign-host drop, `markSeen`/`markStale` transitions, `states` |
| `client.go` | 130 | **141** | outbound GraphQL POST + `ping` + `pluginLag` WS subscribe (graphql-transport-ws) + bearer |
| `mirror.go` | 140 | **80** | pull-through proxy: `@hostId` routing, loop guards (D5), tag + re-emit (came in lean — the op→plugin table + two guards) |
| `liveness.go` | 100 | **56** | per-peer WS loop, backoff, `staleSince` transitions, `peer.stale` emit (WS-primary, no separate poll loop → leaner than estimate) |
| **`plugins/peer/` total** | 670 | **649** | **under the 690 gate; the executor+accessor folded into `peer.go` account for the peer.go overrun, offset by lean mirror/liveness** |
| resolver (`graph/peer.resolvers.go`, delegates) | 30 | 24 | `peers` → `peer.Peers` accessor → store (not counted by `make loc-peer`) |
- **AC-PEER-LOC** guards `plugins/peer/**` (same portable `sed` strip formula as `make loc-github`), fails > **690**.

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
`features/peer.feature`. The `@local` scenarios boot **two** supergraph processes (a consumer with the
peer plugin + a remote running the harness-only `plugins/fakeremote` source executor, seeded from
config); each cross-box AC also has an **`@live @pending`** twin (steps return `godog.ErrPending`) for
the real two-box run. Summary:

- **@S2** (@local + @live) a warmed cross-box free-slot read serves from the local mirror with **zero
  remote calls** over N=20, p95 < 1 s (measured off the fakeremote request log).
- **@F8** (@local + @live) stop the remote → consumer `peers` shows `staleSince` < 30 s; mirrored rows
  still readable, flagged stale; **no peer-of-peer rows** (a foreign `@boxC` row was seeded and dropped).
- **@AC-PEER-MIRROR** a proxied named op returns remote rows tagged `@remoteHostId` + `lastSeenAt`, cached in `peer_nodes`; the served body equals the remote's.
- **@AC-PEER-REEMIT** each mirrored node emits `peer.node.mirrored` preserving the original `@remoteHostId` key; core `/health` "peer" `lastEventAt` advances.
- **@AC-PEER-LOOP** the remote offers a foreign `@boxC` row (asserted present via a *direct* query to the remote); the consumer drops it (no `@boxC` in `peer_nodes`, drop logged) and **never invokes the remote's own peer executor** (asserted: the remote process log carries no `peer: executor` line). Negative control for "no peer-of-peer rows".
- **@AC-PEER-UNKNOWN** *(added by AC review)* a named op for an **unconfigured host** returns empty and makes **no outbound hop** (the remote's request count is unchanged) — never invents a mesh hop.
- **@AC-PEER-AUTH** remote bound **non-loopback** (`:PORT` ⇒ core mandates + enforces tokens): the **absent-Authorization** branch → 401 → peer unreachable, nothing mirrored; the **wrong-token** branch → 401, still nothing mirrored; the **correct token** → proxy succeeds, ≥1 row cached.
- **@AC-PEER-RECONNECT** remote stopped → `staleSince` set < 30 s; restarted on the same port → within **4 s** (≤ 2 × a 2 s test `backoffMaxSeconds`) the backoff loop reconnects to `pluginLag` and `staleSince` clears; the next proxy refreshes the mirror.
- **@AC-PEER-HEALTH** *(grounded by AC review)* `/health` shows a "peer" entry; with the remote never up and nothing mirrored, its `lastEventAt` **stays null** (state `starting`) — **no synthetic heartbeat is fabricated**; `peers` shows `staleSince` set, derived only from the failed connection attempt.
- **@AC-PEER-ZEROCORE** `git diff --stat origin/main -- core server` = 0 files (integration/diff check) **and** no `core/` file imports a plugin package.
- **@AC-PEER-LOC** `plugins/peer/**` prod LOC ≤ **690** (measured 649 + 5%).

## AC review: applied (owner-confirmed decisions)
Applied to `features/peer.feature` + this EDR per `~/.knowledge/.../acceptance-criteria.md`. Owner
decisions: **P1** pull-through mirror + `pluginLag` WS liveness (no push); **P2** peers under
`[plugins.peer]`; **P3** F8 is a plugin-agnostic key-join against a seeded remote in `@local`, real
tmux+claude co-boot is `@live @pending`; **S2 split** — peer owns the *cross-box* S2 (two loopback
processes), tmux owns the single-box free-pane half (future, noted in `features/prd.feature`).
Must-Fix landed: AUTH absent-Authorization branch (401 + nothing mirrored); RECONNECT stale→clear
**bounded** (within 4 s, ≤ 2 × a small test `backoffMaxSeconds`); `@live @pending` S2/F8/AUTH twins.
Should-Fix landed: LOOP asserts the remote **offered** the `@boxC` row and it was dropped (+ empty
`@boxC` in `peer_nodes`); **AC-PEER-UNKNOWN** (unknown host → empty, no hop); HEALTH grounded
(`lastEventAt` null until a real mirror emit, no synthetic heartbeat). Coverage map updated.
Evidence shapes are harness log lines + query results (the `@local` godog harness emits log lines,
not journald).
<!-- AC review applied 2026-09-05 -->

<!-- ACs ready for ac-reviewer -->

Reference-implementation caveat honestly owned: in `@local`, the plugin-level `remotePlugin`
config override points the whole fan-out at `fakeremote`, and the warm read serves **all** cached
rows for a host rather than op-scoping them (a spike simplification — no `@AC-PEER-*` needs op-scoped
warm reads). Production routes each op to its owning source plugin via `mirror.go`'s `opPlugins`
table; op-scoped warm-read partitioning is a follow-up, not a spike requirement.

---

## Coder step plan — file-partitioned, one wave (no shared-file contention)
1. **AS BUILT** `plugins/fakeremote/` (build tag `harness`) — a stand-in remote *source executor*:
   a real plugin serving `/plugins/fakeremote/graphql`, returning a config-seeded node set (incl. a
   plantable **foreign-host** `@boxC` row for AC-PEER-LOOP) in the github executor wire shape, logging
   each request for the "zero remote calls" assertion. The `@local` scenarios boot a real second
   `supergraph serve` process (harness supports per-scenario binary+port) running it; unit tests use an
   `httptest` server (`plugins/peer/peer_test.go`). The base `/graphql pluginLag` stream (liveness) and
   the mesh token rule come from core for free, so no bespoke fake was needed.
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
