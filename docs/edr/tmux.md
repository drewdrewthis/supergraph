# EDR: tmux plugin — server-wide control-mode watcher over local SQLite

Engineering Design Record. Owns the internals the [PRD](../PRD.md) §6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.
Mirrors the shape of [github EDR](./github.md).

**Design (2026-09-05).** The tmux plugin is a **read model of the local tmux server**: one
long-lived **control-mode client** (`tmux -C attach`) streams server-wide structural
notifications into a per-plugin SQLite cache; a low-cadence **reconcile poll** (`list-panes -a`
/ `list-sessions`) backstops missed events and recomputes free/busy. It serves flat
`TmuxSession`/`TmuxPane` + a `Slot` (by `paneKey`) projection over one GraphQL endpoint via the gqlgen
`extend type` glob seam — **no `HTTPRoutes`, no webhook, no fake server**: `@local` scenarios run
against a **real tmux server on a temp socket** (`-L <name>`), which is hermetic and needs no
credentials or hardware. Its reason to exist in the spike is the **local warm free-slot read**
(the on-box half that S2's cross-box query fans out over — the fan-out itself is the **peer
plugin's**, so this feature owns only `AC-TMUX-FREESLOTS-WARM`, not S2) and the
**issue ↔ branch/worktree** join the github plugin already reserves as `paneForBranch @pending`.

**Hard constraints (owner / PRD):** ≤ **820 LOC prod** for `plugins/tmux/**` excluding tests
(600 sketch → 800 for the control client + poll + read model → 860 for the user-test bug fixes
B1/B2 and review Shoulds → **820** after the config helpers moved to `plugins/internal/pluginconfig`;
measured actual **776**, see §LOC budget);
**zero core diff** (S5); Linux + macOS from one binary; single tmux server per box (v1); no
mutation of the user's tmux config (see D2); freshness is event-driven with a reconcile floor
(honest-staleness, mirrors github's AC-GH-STALE).

---

## Decisions

| # | Decision | Chosen | Rejected |
|---|----------|--------|----------|
| **D1** | **Primary event source** | **Control mode** — one `tmux -C attach` client, stdin held open, `refresh-client -f no-output` to suppress pane content. Server-wide structural stream; self-signals server death via `%exit`/EOF. | Polling-only (misses sub-poll churn, worse latency); `wait-for` (rendezvous primitive, not a feed). |
| **D2** | **No global-hook mutation** | The plugin **never** calls `set-hook -g`. Control mode is a passive read-only client. | `set-hook -g pane-died/after-split-window …` — **clobbers the user's own global hooks** (a single global slot per hook) and mutates the live server the operator is using. Rejected on that alone; measured to work but destructive. |
| **D3** | **Reconcile poll as backstop + liveness** | A `reconcileIntervalSeconds` (default **15 s**) poll runs `list-panes -a -F` + `list-sessions -F`, upserts, recomputes free/busy, and marks vanished entities `staleSince`. Each **successful** poll emits one `tmux.snapshot` envelope (§Freshness). | Event-only (an idle-but-alive server would read `stale`; free/busy can change with no structural event — a shell returns to prompt). |
| **D4** | **No fake tmux** | `@local` runs a **real** tmux server on a private socket (`tmux -L sg-test-<pid>`), created/torn down per scenario. Measured working (§Measurements). | An `internal/faketmux` httptest-style stub — pointless when the real dependency is a local, cheap, hermetic process. (github needs `fakegh` because GitHub is remote + credentialed; tmux is not.) |
| **D5** | **Free/busy = idle-shell classification** | A pane is **free** when `pane_current_command ∈ idleShells` (default `["zsh","bash","sh","fish"]`), else **busy**. `Slot` is a projection over panes, not a stored table. | Storing an explicit slot table (derivable, would drift); claude-awareness in tmux (belongs to the claude plugin — see Owner Q2). |
| **D6** | **Serve via gqlgen glob seam** | `plugins/tmux/schema/tmux.graphqls` uses `extend type Query`/`Subscription`; resolvers delegate through `core.Registry` — same seam the template plugin proves (AC-CORE-10b). No `cmd/` delta. | github's `HTTPRoutes` path — tmux has no webhook receiver, so the glob seam is the right (and simpler) one. |
| **D7** | **Probe-gated backoff reconnect** | Each cycle **probes liveness with a read-only `list-sessions` BEFORE attaching** — a bare `tmux -C attach` on a dead socket makes tmux fork a *new* server (busy-spin + leaked clients), so a non-zero probe means down and the client never attaches (B2). On an up→down transition the client marks all local entities `staleSince` and emits `tmux.server.down` **exactly once**, then stays silent while down so `/health` lag crosses the stale threshold honestly (B1); it **exponential-backoffs to `reconnectBackoffMaxSeconds` (30 s cap)** and re-attaches when the server returns. An attach that falls out in **< 2 s** (`minStableAttach`) is a failed/immediate exit and does **not** reset the backoff. | Exit the plugin (drops the source permanently); attach-first (auto-spawns a server on a dead socket — B2); re-emitting `server.down` every reconnect (core records each emit as liveness, so `/health` never goes stale — B1). |

## Measurements (this machine — `tmux 3.6a`, `/opt/homebrew/bin/tmux`, darwin)

Run on a private socket `tmux -L sgprobe`, no attach to the operator's server:
- **Control mode is server-wide.** Attached to session `s1`, the client still received
  `%unlinked-window-add`, `%sessions-changed`, `%window-add`, `%window-pane-changed`,
  `%layout-change`, `%unlinked-window-close`, `%session-window-changed` for mutations made in a
  **different** session (`s3`). One client watches the whole server.
- **`%output` is fully suppressible.** After `refresh-client -f no-output`, sending
  `echo NOISE` into a pane produced **0** `%output` lines — the event stream stays structural.
- **Pane death is observable.** `kill-pane` produced `%window-pane-changed` + a `%layout-change`
  whose layout string no longer lists the dead pane id (no dedicated `%pane-exited` in 3.6a — the
  layout delta and, for a last pane, `%window-close`/`%unlinked-window-close` carry the truth).
- **`list-panes -a -F`** yields, per pane across all sessions:
  `#{session_name}:#{window_index}.#{pane_index}`, `#{pane_pid}`, `#{pane_current_command}`,
  `#{pane_current_path}`, `#{pane_active}`, `#{pane_title}`. This is the full snapshot/reconcile field set.
- **Control mode needs stdin held open** or it emits `%exit` immediately — the client must keep the
  write end of its stdin pipe open for the process lifetime (measured: closing stdin ⇒ instant `%exit`).
- **Global hooks work but are singular** — `set-hook -g after-split-window '…'` fired, but there is
  one global slot per hook name, so setting it overwrites whatever the operator had. Reinforces D2.

## Key grammar (consistent with github's `<kind>:<scope>[…]@<hostId>`)

`@<hostId>` is **always present** for tmux (unlike github, where it is elided for `github.com`) —
a pane is only meaningful per box. tmux session names **cannot contain `:` or `.`** (tmux forbids
them), so the scope is unambiguous without escaping.
```
key      := <kind>":"<scope>"@"<hostId>
server   := "tmuxServer:" <hostId> "@" <hostId>          # scope == hostId
session  := "session:"    <session>            "@" <hostId>
pane     := "pane:"       <session>":"<window>"."<pane> "@" <hostId>
```
`<window>`/`<pane>` are tmux indices (ints). Example: `pane:main:1.0@drudru-lan`.
- **Slot** has no key of its own — it is a **projection** over `pane` keys where the pane is free.
- The parse/format lives in `keys.go` as a **data table** (`kindSpecs`), mirroring
  `github/keys.go`: `parse(key) → (kind,scope,hostId)`, `paneKey(session,window,pane,host)`,
  `typename(kind)`, and the free/busy classifier.

## Envelopes emitted (`{TS, Source:"tmux", Type, V:1, Key, Payload}`)

`/health` `lastEventAt` derives from these. **These four are the only envelopes emitted** — the
session/window envelopes the sketch imagined are **not implemented** (sessions are upserted but
carry no per-entity event; the read model is pane-centric):
- **`tmux.pane.updated`** — `Payload={key,session,window,pane,pid,cmd,path,active,free}` — pane
  **new** or **changed** vs the stored row (`free`/`busy`, `cmd`, or `path` moved) on a reconcile
  diff. An unchanged pane on a later poll does **not** re-emit (no event flood — S-A).
- **`tmux.pane.closed`** — `Payload={key}` — pane gone (reconcile miss); marks `staleSince`.
- **`tmux.snapshot`** — `Payload={hostId,paneCount}` — one per **successful** reconcile poll; the
  liveness signal for a structurally-idle but alive server (§Freshness, Owner Q1).
- **`tmux.server.down`** — `Payload={hostId}`, key `tmuxServer:<host>@<host>` — emitted **once per
  up→down transition** (B1), never on repeated failed reconnects; marks every local session/pane
  `staleSince` (the local shape of F8's stale-peer, before the peer plugin).

Cross-plugin join: the claude plugin's `ClaudeInstance` keys on `pane:…@host` + `pid`; the github
join is issue-number ↔ `session.branch`/`worktree` via `paneForBranch`.

## Freshness contract (honest)

Event-driven with a poll floor — same honesty as github's AC-GH-STALE.
- A structural change normally reflects **sub-second** (control-mode push → upsert → queryable).
- **Worst-case staleness = `reconcileIntervalSeconds` (default 15 s)** — the bound when a
  control-mode notification is missed (client reconnecting) or when free/busy flips with no
  structural event (a shell returns to its prompt). The reconcile poll heals both.
- **Liveness of a quiet server**: an alive server with no structural churn still answers the
  reconcile `list-panes`, and each successful poll emits `tmux.snapshot`, advancing `lastEventAt`
  so `/health` does not falsely read `stale`. This is an **observation of the external tmux
  server** (it answered), **not** a synthetic self-heartbeat (which core bans) — see Owner Q1.
- **Server down**: `%exit`/EOF emits `tmux.server.down`, all local entities go `staleSince`, and no
  further `tmux.snapshot` is emitted → `/health` for tmux crosses into `stale` (correct).

## Reads / queries (`plugins/tmux/queries/*.graphql`, ~5)

The five ops ship as `plugins/tmux/queries/*.graphql`, posted as query text to core `/graphql` by
`supergraph query --plugin tmux --op NAME [--var hostId=box]` (core CLI; no `cmd/` delta):
- **`freeSlots`** — `freeSlots(hostId){ hostId kind free paneKey staleSince }` (server-side
  `free:true`, non-stale) — the local warm-read half of **S2**.
- **`sessions`** — `tmuxSessions(hostId){ hostId name worktree branch lastSeenAt staleSince }`.
- **`panes`** — `tmuxPanes(hostId){ hostId key session window pane pid cmd path active free staleSince }`.
- **`paneForBranch`** — `paneForBranch(branch){ … }` — the github/claude join (issue ↔ branch).
- **`slotsForHost`** — full `slots(hostId)` (free + busy) for dashboards.

## Schema (`plugins/tmux/schema/tmux.graphqls`, `extend type` only)

Non-GitHub types keep the `Tmux*` prefix (PRD §6). **Shipped schema — flattened to scalars**
(the PRD sketch's nested `TmuxServer`/`TmuxWindow` objects and `Slot.pane`/`TmuxPane.window`
object references were **dropped**: no AC exercises them and each would add a per-field resolver
for no read the CLI ops make. `Slot` points at its pane by `paneKey` scalar; `tmuxEvents` is a
flat `TmuxEvent` envelope, not a live `TmuxPane`):
```graphql
type TmuxSession { hostId: String!  name: String!  worktree: String  branch: String  lastSeenAt: Time  staleSince: Time }
type TmuxPane    { hostId: String!  key: String!  session: String!  window: Int!  pane: Int!  pid: Int!  cmd: String!  path: String  active: Boolean!  free: Boolean!  staleSince: Time }
type Slot        { hostId: String!  kind: String!  free: Boolean!  paneKey: String  staleSince: Time }
type TmuxEvent   { ts: Time!  type: String!  v: Int!  key: String!  payload: String! }
extend type Query {
  tmuxSessions(hostId: String): [TmuxSession!]!
  tmuxPanes(hostId: String): [TmuxPane!]!
  slots(hostId: String): [Slot!]!
  freeSlots(hostId: String): [Slot!]!
  paneForBranch(branch: String!): [TmuxPane!]!
}
extend type Subscription { tmuxEvents: TmuxEvent! }
```

## Config keys `[plugins.tmux]`

| Key | Default | Meaning |
|---|---|---|
| `socket` | `""` (default server) | tmux `-L <name>` or `-S <path>`; empty ⇒ the operator's default server. |
| `eventSource` | `"control"` | `control` (D1) or `poll` (fallback if control-mode attach is unavailable). |
| `reconcileIntervalSeconds` | `15` | Backstop poll cadence = worst-case staleness bound. |
| `idleShells` | `["zsh","bash","sh","fish"]` | A pane whose `pane_current_command` is in this set is **free** (D5). |
| `slotKind` | `"worker"` | The `kind` label on every emitted `Slot`. |
| `reconnectBackoffMaxSeconds` | `30` | Cap on control-mode reconnect backoff (D7). |

`hostId` is inherited from core config, not repeated here. No `token`/`secret` — tmux is local.

## SQLite state (add-only, `IF NOT EXISTS`; cursors in core's `cursors` table)

- `tmux_sessions(key TEXT PK, name, worktree, branch, last_seen_at, stale_since)`
- `tmux_panes(key TEXT PK, session, window INT, pane INT, pid INT, cmd, path, active INT, free INT, last_seen_at, stale_since)`
- **No `tmux_windows` table** — the sketch's third table was dropped as YAGNI (the read model is
  pane-centric; `session`/`window`/`pane` are denormalized onto `tmux_panes` for query-simple scans
  and the branch join, so no key parsing happens in SQL).
- `hostId` is parsed from the key, not stored as a column (github convention).
- Cursor: `snapshot:lastAt` (last successful reconcile time) in core's `cursors` table; `Cursor()` reports it.

## LOC budget (prod, cap **820**; tests excluded) — measured actuals

Strip formula (portable GNU/BSD sed), run by `make loc-tmux`:
`find plugins/tmux -name '*.go' ! -name '*_test.go' | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$/d' | wc -l` — fails > 820.

**Cap 600 → 800 → 860 → 820:** the control-mode client + reconcile poll + SQLite read model + flattened
resolver seam landed at **770** (600 sketch overrun, owner-ratified 800). The user-test bug fixes
(B1 once-per-transition `server.down`, B2 probe-before-attach + `minStableAttach` backoff guard) and
the review Shoulds (S-A change-detected `pane.updated`, S-B scan-error/reply-frame handling) took it to
**813** (cap 860). The `strOr`/`intOr`/`strsOr` config helpers then moved to `plugins/internal/pluginconfig`
(−37 in `tmux.go`), dropping the measure to **776**; per the owner rule the cap is set to **measured + 5%
rounded up to a multiple of 10 = 820**. No compression for the number.

`active`/`getCurrent` (already `atomic.Pointer[Plugin]`, not the RWMutex the post-tier §A doc
described) was moved to `plugins/internal/single.Ptr[Plugin]` — LOC-neutral, measured holds at
**776**, cap stays 820.

| File (`plugins/tmux/`) | Actual | Responsibility |
|---|---:|---|
| `tmux.go` | 127 | wiring: `init`/`New`/`Name`/`Migrate`/`Start` (dormant when unconfigured) + `Cursor` + config parse/defaults (coercion via `plugins/internal/pluginconfig`) + `attach`/`serverAlive`/clock/exec seams + `atomic.Pointer` singleton |
| `keys.go` | 79 | key grammar data table: parse + object→key + typename + host split + free/busy classifier |
| `control.go` | 144 | control-mode client: probe-gated `tmux -C attach`, hold stdin, `refresh-client -f no-output`, structural-notification → reconcile trigger, `%begin/%end/%error` skip + scan-error log, once-per-transition `server.down`, `minStableAttach` + backoff reconnect |
| `snapshot.go` | 158 | `list-panes -a`/`list-sessions` parse → upsert; reconcile diff → new/changed `pane.updated` + `pane.closed`/`staleSince`; `tmux.snapshot` emit (only on success — owner T1); git-branch resolve; free/busy recompute |
| `store.go` | 218 | SQLite: migrate tables, upsert session/pane, scan, markStale/markAllStale, `livePaneRows`/`livePaneKeys`, scan-free, paneForBranch join |
| `resolver.go` | 50 | exported read funcs for sessions/panes/slots/freeSlots/paneForBranch via the package-singleton seam (no `core.Registry` instance accessor exists; server injects only Health/Lag/Events) |
| **Total** | **776** | cap **820**; no `cmd/` delta (serves via gqlgen glob seam, D6) |

**Deviations from the sketch, attributed (every real one):**
- **Schema flattened to scalars** — the sketch's nested `TmuxServer`/`TmuxWindow` object types and
  `Slot.pane`/`TmuxPane.window` references were dropped; `Slot` points by `paneKey` scalar. **Added**
  `freeSlots(hostId)` query and a flat `TmuxEvent` envelope type; `tmuxEvents` returns `TmuxEvent!`,
  not a live `TmuxPane`. No AC exercises the nested objects and each would only add resolver LOC.
- **Session/window envelopes not implemented** — only `tmux.pane.updated`/`.closed`/`tmux.snapshot`/
  `tmux.server.down` are emitted; the read model is pane-centric (sessions are upserted, not evented).
- **`tmux_windows` table dropped (YAGNI)**; `session`/`window`/`pane` denormalized onto `tmux_panes`
  (`store.go` heavier: query-simple scans + the branch join, no key parsing in SQL).
- **Package-singleton seam** — `atomic.Pointer[Plugin]` (`active`) mirrors the peer/claude convention;
  graph resolvers reach the live store through it because core exposes no `Registry` instance accessor.
- **Poll + control reconcile, not per-delta parse** — any structural notification triggers a fresh
  `list-panes` (simpler and more version-robust than parsing each `%`-delta); `control.go` grew for the
  B1/B2 probe/backoff/transition logic and the S-B scan-error + reply-frame handling.
- **`tmux.go` dormant-when-unconfigured** `Start` so the plugin never attaches to the operator's real
  server unless `[plugins.tmux]` is set, plus injectable seams for hermetic unit tests.

## Failure modes

- Control-mode attach unavailable / `%exit` on boot → fall back to `eventSource:"poll"` cadence; `/health` still fed by `tmux.snapshot`.
- tmux server killed → `tmux.server.down`, all local `staleSince`, backoff reconnect (D7).
- Missed `%`-notification (reconnect gap) → next reconcile poll heals within `reconcileIntervalSeconds`.
- Session name edge cases → tmux forbids `:`/`.`, grammar stays unambiguous (AC-TMUX-KEY-GRAMMAR).
- Free/busy flip with no structural event → reconcile poll recomputes (bounded by the interval).

---

## Acceptance criteria
<!-- ACs ready for ac-reviewer -->

`@local` runs against a **real tmux server on a temp socket** (`-L`); `@pending` only where a
second box / peer plugin / credentials are needed. Primary evidence is **use-proof from the
running binary + real tmux this turn**. `T` = `reconcileIntervalSeconds` (test uses a short value).

- **AC-TMUX-FREESLOTS-WARM (local half of S2; S2 fan-out is the peer's).** Against a warm cache seeded
  with idle and busy panes, `freeSlots` returns every idle pane and no busy pane, with **p95 < 1 s
  over N≥20 samples**. Fails if p95 ≥ 1 s, a free slot is omitted, or a busy pane is offered as free.
  **Evidence:** the 20 measured query latencies + the free-key set. *(Cross-box fan-out belongs to the
  peer plugin; prd.feature S2 is untouched. This feature is tagged `@AC-TMUX-FREESLOTS-WARM`, not `@S2`.)*
- **AC-TMUX-EVENTS-CONTROL (falsifiable: only control mode can satisfy it).** The plugin watches a
  temp-socket server via control mode **with `reconcileIntervalSeconds` set far beyond the observation
  window** (e.g. 3600 s) so a periodic poll cannot be the thing that observes the change. Then panes
  are created **in a different session than the one the control client attached to**, each timed from
  the mutating `tmux` command to the moment it is queryable (paired timestamps). The **p95
  split-to-queryable latency is < 1 s over N≥20**, and the cached pane count equals the real
  `list-panes -a` count (no `%output` noise ingested). Fails if any new pane is not observed before the
  (huge) reconcile interval elapses, if p95 ≥ 1 s, or if the cached count drifts from tmux's. Because
  the poll is disabled within the window, only the control-mode push path can make this pass.
  **Evidence:** the 20 paired split→queryable latencies + the count-equality check.
- **AC-TMUX-PANE-DEATH.** `kill-pane` on a tracked pane marks it `staleSince`/closed (from the
  `%layout-change` delta) and removes it from `freeSlots` within `T`. Fails if the dead pane still
  appears in `freeSlots`. **Evidence:** `freeSlots` before/after the kill, quoted.
- **AC-TMUX-FREE-BUSY (D5).** A pane running a non-idle-shell command (`sleep 30`) reads **busy**
  (absent from `freeSlots`); when it returns to the shell prompt it reads **free** again within `T`.
  Fails if a busy pane is offered as a free slot. **Evidence:** `freeSlots` in both states, quoted.
- **AC-TMUX-SERVER-DOWN.** `kill-server` on the watched socket emits `tmux.server.down`, marks every
  local session/pane `staleSince` within `T`, and the plugin **backoff-reconnects** (no crash, no
  process exit) — a server brought back is re-populated by the next poll. Fails if the plugin exits,
  panics, or leaves entities non-stale after the server is gone. **Evidence:** `/health` (tmux path
  fed by snapshot then going stale), query showing `staleSince`, and a post-restart repopulated query.
- **AC-TMUX-NO-HOOK-CLOBBER (D2).** A user-set global hook (`set-hook -g after-split-window …`)
  present on the socket **survives** a full plugin run — the plugin never issues `set-hook -g`.
  Fails if the operator's hook is altered or removed. **Evidence:** `show-hooks -g` before/after,
  byte-identical; plus `grep -R 'set-hook' plugins/tmux` returning nothing.
- **AC-TMUX-PANE-FOR-BRANCH (join key).** `paneForBranch(branch)` returns exactly the pane(s) whose
  session worktree is on that branch (the issue↔branch join github reserves as `paneForBranch`).
  Fails if it returns a pane on another branch or misses the matching one. **Evidence:** query result
  vs the seeded worktree/branch, quoted.
- **AC-TMUX-KEY-GRAMMAR.** A **live** pane key from the running binary round-trips
  `pane:<session>:<window>.<pane>@<hostId>` (`parse(format(x)) == x`) via the `tmuxPanes` query, and
  the compiled parser **rejects** malformed keys rather than returning a partial one: an embedded `:`
  or `.` in the session segment, an unknown kind prefix, an empty window/pane index, a non-numeric
  index, and a missing `@hostId`. Fails if the live round-trip differs or any malformed key parses
  silently. **Evidence:** the live-key round-trip from the query + the malformed-rejection table
  exercised through the compiled parser (`plugins/tmux/keys_test.go`).
- **AC-TMUX-POLL-ERROR (owner T1: an errored/timed-out poll must NOT emit).** A reconcile that
  errors or times out (injected via a stub `tmuxPath` that succeeds on the first poll, then exits
  non-zero) emits **no** `tmux.snapshot`; with emits stopped, the `tmux` `/health` entry crosses from
  `ok` to `stale` within the lag threshold. Fails if a failed poll advances health or emits a
  snapshot. **Evidence:** `/health` `ok` after the first (successful) poll, then `stale` after the
  stub is tripped, with no intervening snapshot. *(A successful reconcile poll IS a real emit; only
  the errored/timed-out one must not emit.)*
- **AC-TMUX-STALE (negative control, honest freshness).** A pane closed while the control client is
  mid-reconnect (no `%`-notification observed) is still marked `staleSince` by the **next reconcile
  poll**, bounding staleness at `T`. Fails if the vanished pane stays `free`/live past `T`.
  **Evidence:** the closed pane still-free at `t<T`, then stale after the poll, with timestamps.
- **AC-TMUX-ISOLATION (F5, tmux half).** With tmux co-registered alongside a sibling plugin that
  panics in `Start`, tmux keeps serving `freeSlots` and `/health` still answers. Fails if tmux stops
  answering when the sibling panics. **Evidence:** `freeSlots` + `/health` after the induced panic.
  *(F5's trigger scenario stays claude-owned in prd.feature; this covers tmux's "keeps answering".)*
- **AC-TMUX-CURSOR.** The reconcile cursor (`snapshot:lastAt`) persists across `stop`+`start` and is
  surfaced by `Cursor()` in `/health`. Fails if the cursor resets on restart. **Evidence:** cursor
  value before/after restart, quoted.
- **AC-TMUX-ZEROCORE (S5).** After adding `plugins/tmux/**` + the `graph/plugins_import.go` blank
  import + regenerated `graph/`, `git diff --stat core/` reports **0 files changed** and `/health`
  serves a `tmux` entry. **Evidence:** empty `git diff --stat core/` + `/health` showing `tmux`.
- **AC-TMUX-LOC.** The strip-formula count for `plugins/tmux/**` (excluding `*_test.go`) is **≤ 820**
  (measured **776**). **Evidence:** `make loc-tmux` output ≤ 820.
- **AC-TMUX-STALE-PEER (cross-box, @pending; peer-owned).** After the peer plugin exists, a stopped box
  shows its tmux data as `stale since T` on a peer **< 30 s**, with no peer-of-peer rows. **@pending** —
  needs the peer plugin + a second box. Tagged `@AC-TMUX-STALE-PEER` (a tmux-scoped cross-reference; F8
  itself stays peer-owned in prd.feature, unmoved). **Evidence (deferred):** peer query screenshot + grep.

## prd.feature deltas — **none** (this EDR / feature does NOT touch prd.feature)

S2 is **peer-owned** (cross-box fan-out): this feature owns only the on-box warm read as
`AC-TMUX-FREESLOTS-WARM` and does **not** claim `@S2`, so prd.feature's S2 scenario stays exactly
as-is — there is no S2 retirement. Likewise:
1. **S2 stays in prd.feature (peer-owned).** `tmux.feature` carries `@AC-TMUX-FREESLOTS-WARM` for the
   local half only. Do **not** remove or retag prd.feature S2, and do not add `@S2` in tmux.feature.
2. **F5 stays in prd.feature (claude-owned trigger).** Its evidence already reads "…the other
   plugins, including tmux, keep answering". `tmux.feature`'s `AC-TMUX-ISOLATION` covers tmux's half
   locally. Leave the prd.feature F5 scenario as-is.
3. **F8 stays in prd.feature (peer-owned).** `tmux.feature` carries a `@pending @AC-TMUX-STALE-PEER`
   cross-reference for the tmux-data portion only; do **not** move or retag F8 in prd.feature.

No prd.feature edits at all. Everything tmux-specific lives in `features/tmux.feature`.

## Handoff

- ACs ready for ac-reviewer (see §Acceptance criteria above + `features/tmux.feature`).
- Implementation → **coder** for `keys.go`/`store.go`/`snapshot.go`/`resolver.go`/`tmux.go`
  (contracts explicit); **advanced-coder** for `control.go` (the control-mode parser + backoff
  reconnect + `%exit` handling is the judgment-bearing seam). Wave 2 (godog steps in
  `features/steps_tmux_test.go`) depends on Wave 1 compiling. Per
  `~/.knowledge/modules/shared/records/model-selection.md`.
- No prd.feature edit is required (S2/F5/F8 stay peer/claude-owned; see §prd.feature deltas).
