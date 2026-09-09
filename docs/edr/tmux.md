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

**Hard constraints (owner / PRD):** ≤ **1030 LOC prod** for `plugins/tmux/**` excluding tests
(600 sketch → 800 for the control client + poll + read model → 860 for the user-test bug fixes
B1/B2 and review Shoulds → **820** after the config helpers moved to `plugins/internal/pluginconfig`
→ **1030** for issue #27 window→pane nesting; measured actual **1002**, see §LOC budget);
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
| **D8** | **`attached` derived from `list-clients`, not `#{session_attached}`** (#27, epic #30) | Count clients whose `#{client_control_mode} != "1"` — a session is attached iff ≥1 **human** client. Any `list-clients` failure (a zero-client server errors on some versions) reads as **zero clients**, never a reconcile abort. | Reading `#{session_attached}`: the plugin's OWN `tmux -C attach` control client lands on a session and makes it read `attached=1` with zero human clients (measured, §3) — a permanent false positive on the exact field the sidebar highlight keys on. |

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
window   := "window:"     <session>":"<index>  "@" <hostId>   # #27, epic #30
pane     := "pane:"       <session>":"<window>"."<pane> "@" <hostId>
```
`<window>`/`<pane>`/`<index>` are tmux indices (ints). Example: `pane:main:1.0@drudru-lan`,
`window:main:1@drudru-lan`. `parseWindowKey` rejects a malformed window key (unknown kind,
missing `@hostId`, empty/non-numeric index, embedded `.`) exactly as `parsePaneKey` does.
- **Slot** has no key of its own — it is a **projection** over `pane` keys where the pane is free.
- The parse/format lives in `keys.go` as a **data table** (`kindSpecs`), mirroring
  `github/keys.go`: `parse(key) → (kind,scope,hostId)`, `paneKey(session,window,pane,host)`,
  `typename(kind)`, and the free/busy classifier.

## Envelopes emitted (`{TS, Source:"tmux", Type, V:1, Key, Payload}`)

`/health` `lastEventAt` derives from these. **Five envelopes are emitted** (the fifth,
`tmux.session.updated`, was added by issue #27 / epic #30 — before it, sessions were upserted but
carried no per-entity event):
- **`tmux.pane.updated`** — `Payload={key,session,window,pane,pid,cmd,path,active,free}` — pane
  **new** or **changed** vs the stored row (`free`/`busy`, `cmd`, or `path` moved) on a reconcile
  diff. An unchanged pane on a later poll does **not** re-emit (no event flood — S-A).
- **`tmux.pane.closed`** — `Payload={key}` — pane gone (reconcile miss); marks `staleSince`.
- **`tmux.snapshot`** — `Payload={hostId,paneCount}` — one per **successful** reconcile poll; the
  liveness signal for a structurally-idle but alive server (§Freshness, Owner Q1).
- **`tmux.server.down`** — `Payload={hostId}`, key `tmuxServer:<host>@<host>` — emitted **once per
  up→down transition** (B1), never on repeated failed reconnects; marks every local session/pane
  `staleSince` (the local shape of F8's stale-peer, before the peer plugin).
- **`tmux.session.updated`** — `Payload={key,name,attached,createdAt,worktree,branch}`, key
  `session:<name>@<host>` — a session **new or changed** vs the stored row on a reconcile diff
  (`attached`/`createdAt`/`worktree`/`branch` moved). This is the attach/detach the `tmuxEvents`
  subscriber sees within 1 s (the control-mode `%client-session-changed`/`%client-detached` trigger
  a fresh reconcile). **`attached` is NOT read from `#{session_attached}`**: the plugin's own
  `tmux -C attach` control client makes its session read `attached=1` with zero human clients
  (measured, #27 §3), so `attached` is derived from `list-clients`, counting only clients whose
  `#{client_control_mode} != "1"`.

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
- **`sessions`** — `tmuxSessions(hostId){ hostId name worktree branch attached createdAt lastSeenAt
  staleSince windows { key index name active panes { key paneId } } }` (the window→pane nesting is #27).
- **`panes`** — `tmuxPanes(hostId){ hostId key session window pane pid cmd path active free paneId staleSince }`.
- **`paneForBranch`** — `paneForBranch(branch){ … }` — the github/claude join (issue ↔ branch).
- **`slotsForHost`** — full `slots(hostId)` (free + busy) for dashboards.

## Schema (`plugins/tmux/schema/tmux.graphqls`, `extend type` only)

Non-GitHub types keep the `Tmux*` prefix (PRD §6). **The nested-objects decision is REVERSED by
issue #27 (epic #30).** The shipped schema was originally flattened to scalars — the PRD sketch's
nested `TmuxServer`/`TmuxWindow` objects were dropped because "no AC exercises them" — but epic #30
is the AC that now exercises them: the sidebar reads `windows { panes { paneId } }`. So `TmuxWindow`
is reintroduced as a nested object (a projection over `tmux_panes`, no stored table), and
`TmuxSession` gains `attached`/`createdAt`/`windows`, `TmuxPane` gains `paneId` (tmux's native
`#{pane_id}`). These are populated **eagerly** by the `TmuxSessions` resolver from the same store
read — no per-field resolver — so the graph seam stays plumbing-free (#27 §6). `Slot`/`TmuxEvent`
stay flat (still no AC exercises `Slot.pane`, and `tmuxEvents` remains a flat envelope):
```graphql
type TmuxSession { hostId: String!  name: String!  worktree: String  branch: String  attached: Boolean!  createdAt: Time  windows: [TmuxWindow!]!  lastSeenAt: Time  staleSince: Time }
type TmuxPane    { hostId: String!  key: String!  session: String!  window: Int!  pane: Int!  pid: Int!  cmd: String!  path: String  active: Boolean!  free: Boolean!  paneId: String!  staleSince: Time }
type TmuxWindow  { hostId: String!  key: String!  session: String!  index: Int!  name: String  active: Boolean!  panes: [TmuxPane!]! }
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

- `tmux_sessions(key TEXT PK, name, worktree, branch, attached INT, created_at TEXT, last_seen_at, stale_since)`
- `tmux_panes(key TEXT PK, session, window INT, pane INT, pid INT, cmd, path, active INT, free INT, pane_id TEXT, window_name TEXT, window_active INT, last_seen_at, stale_since)`
- **#27 columns are additive (`ALTER TABLE … ADD COLUMN`)** — `attached`/`created_at` on sessions,
  `pane_id`/`window_name`/`window_active` on panes. A DB written by the pre-#27 plugin upgrades in
  place; the `duplicate column name` error on a second start is tolerated (no-op), not fatal
  (AC-TMUX-MIGRATE-INPLACE).
- **No `tmux_windows` table** — even with the nested `TmuxWindow` type, windows stay a **projection**
  over `tmux_panes` grouped by `(session, window)` (the YAGNI call holds); `session`/`window`/`pane`
  plus `window_name`/`window_active` are denormalized onto `tmux_panes`, so no key parsing in SQL and
  a window with zero live panes is simply never emitted.
- `hostId` is parsed from the key, not stored as a column (github convention).
- Cursor: `snapshot:lastAt` (last successful reconcile time) in core's `cursors` table; `Cursor()` reports it.

## LOC budget (prod, cap **1030**; tests excluded) — measured actuals

Strip formula (portable GNU/BSD sed), run by `make loc-tmux`:
`find plugins/tmux -name '*.go' ! -name '*_test.go' | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$/d' | wc -l` — fails > 1030.

**Cap 600 → 800 → 860 → 820:** the control-mode client + reconcile poll + SQLite read model + flattened
resolver seam landed at **770** (600 sketch overrun, owner-ratified 800). The user-test bug fixes
(B1 once-per-transition `server.down`, B2 probe-before-attach + `minStableAttach` backoff guard) and
the review Shoulds (S-A change-detected `pane.updated`, S-B scan-error/reply-frame handling) took it to
**813** (cap 860). The `strOr`/`intOr`/`strsOr` config helpers then moved to `plugins/internal/pluginconfig`
(−37 in `tmux.go`), dropping the measure to **776**; per the owner rule the cap is set to **measured + 5%
rounded up to a multiple of 10 = 820**. No compression for the number.

**Cap 820 → 1030 (issue #27, epic #30):** the window→pane nesting projection + `WindowsBySession`/`groupWindows`
N+1 fix (`resolver.go`), the `tmux.session.updated` envelope + `list-clients` attached probe + session diff +
`createdAt` parse (`snapshot.go`), the five additive migration columns + `liveSessionRows`/`scanSession`
(`store.go`), and the `window` key grammar (`keys.go`) took the measure from **779** to **1002**. Per
the owner rule (measured + 5% rounded up to a multiple of 10) the formula gives 1060, but the cap is
**deliberately held at 1030** — a tightening from 1060 rather than a loosening, reserving 2.8% headroom
instead of the standard 5%.

| File (`plugins/tmux/`) | Actual | Responsibility |
|---|---:|---|
| `tmux.go` | 130 | wiring: `init`/`New`/`Name`/`Migrate`/`Start` (dormant when unconfigured) + `Cursor` + config parse/defaults (coercion via `plugins/internal/pluginconfig`) + `attach`/`serverAlive`/clock/exec seams + `single.Ptr` singleton |
| `keys.go` | 105 | key grammar data table: parse + object→key (incl. `window`) + typename + host split + free/busy classifier |
| `control.go` | 145 | control-mode client: probe-gated `tmux -C attach`, hold stdin, `refresh-client -f no-output`, structural-notification (incl. `%client-detached`/`%client-session-changed`) → reconcile trigger, `%begin/%end/%error` skip + scan-error log, once-per-transition `server.down`, `minStableAttach` + backoff reconnect |
| `snapshot.go` | 228 | `list-panes -a`/`list-sessions`/`list-clients` parse → upsert; reconcile diff → `pane.updated`/`pane.closed`/`session.updated`; `attachedSessions` probe (control-mode excluded); `createdAt` parse; `tmux.snapshot` emit (only on success — owner T1); git-branch + free/busy |
| `store.go` | 281 | SQLite: migrate tables + additive #27 columns, upsert session/pane, scan, markStale/markAllStale, `livePaneRows`/`liveSessionRows`/`livePaneKeys`, scan-free, paneForBranch join |
| `resolver.go` | 113 | exported read funcs for sessions/panes/slots/freeSlots/paneForBranch + `Windows` (non-stale panes grouped by window) + `WindowsBySession`/`groupWindows` N+1 fix for one pane scan per session via the package-singleton seam |
| **Total** | **1002** | cap **1030**; no `cmd/` delta (serves via gqlgen glob seam, D6) |

**Deviations from the sketch, attributed (every real one):**
- **Schema was flattened, then RE-NESTED by #27 (epic #30)** — the sketch's `TmuxWindow` object was
  first dropped ("no AC exercises it"), then reintroduced as a nested projection once epic #30's
  sidebar exercised `windows { panes { paneId } }`. `Slot.pane`/`TmuxPane.window` object references
  stay dropped (still no AC); `Slot` points by `paneKey` scalar; `tmuxEvents` returns a flat
  `TmuxEvent!`, not a live `TmuxPane`. The nested `TmuxWindow`/`windows`/`paneId` are populated
  eagerly by the `TmuxSessions` resolver, so no per-field resolver LOC.
- **Session envelope added by #27** — `tmux.session.updated` is the fifth envelope (attach/detach,
  `createdAt`, worktree/branch diff); before #27 only `tmux.pane.updated`/`.closed`/`tmux.snapshot`/
  `tmux.server.down` were emitted and the read model was pane-centric.
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
- **AC-TMUX-LOC.** The strip-formula count for `plugins/tmux/**` (excluding `*_test.go`) is **≤ 1030**
  (measured **1002** after #27). **Evidence:** `make loc-tmux` output ≤ 1030.

### Issue #27 (epic #30) ACs — session nesting, attach/detach, createdAt

- **AC-TMUX-ATTACHED-LIVE.** With the reconcile interval far beyond the observation window, a real
  client attaching to a tracked session flips `TmuxSession.attached` `false→true` (and detach
  `true→false`) **within 1 s** of the `tmux attach`/`detach-client` returning, observed as a
  `tmux.session.updated` envelope on an open `tmuxEvents` subscription and confirmed by a follow-up
  `tmuxSessions` query. Driven by control-mode `%client-session-changed`/`%client-detached`, not the
  backstop poll. **Evidence:** paired command-return/envelope-arrival timestamps + the two payloads + the two query results.
- **AC-TMUX-ATTACHED-NOT-SELF.** On a server with **zero human clients** where only the plugin's own
  control-mode client is attached, **every** session reads `attached: false`. **Evidence:**
  `list-clients -F '#{client_tty} #{client_control_mode}'` (only the control client) beside the
  `tmuxSessions` result showing `attached:false` throughout.
- **AC-TMUX-CLIENTS-PROBE-FAILSAFE.** A non-zero `list-clients` exit does not abort the reconcile: all
  sessions read `attached:false` and every other field (`createdAt`, panes, windows, free/busy) is
  populated unchanged, none crossed to stale. **Evidence:** the zero-client `tmuxSessions` result beside the captured non-zero exit.
- **AC-TMUX-SESSION-CREATE-LIVE.** With the reconcile interval far beyond the window, a **new** session
  produces a `tmux.session.updated` envelope carrying its `name`/`attached`/`createdAt` within 1 s of
  `new-session` returning, queryable immediately after. **Evidence:** paired timestamps + payload + query result.
- **AC-TMUX-CREATEDAT.** `TmuxSession.createdAt` matches `#{session_created}` within 1 s for every
  tracked session and survives a plugin restart. Fails on null, zero time, or >1 s drift. **Evidence:**
  `list-sessions -F '#{session_name} #{session_created}'` beside the query, before and after restart.
- **AC-TMUX-WINDOW-NESTING.** For a session with **≥2 windows, one holding ≥2 panes**,
  `tmuxSessions { windows { index name active panes { key paneId } } }` returns exactly the real window
  set with exactly the real pane keys per window — no pane under the wrong window, none dropped, no
  zero-live-pane window emitted; every `panes.key` is one `tmuxPanes` returns and every `panes.paneId`
  equals `#{pane_id}`. **Evidence:** raw `list-panes -a` beside the nested query result.
- **AC-TMUX-WINDOW-KEY-GRAMMAR.** A live `TmuxWindow.key` round-trips `window:<session>:<index>@<hostId>`
  through the compiled parser, which **rejects** malformed keys (unknown kind, missing `@hostId`, empty
  index, non-numeric index, embedded `:`/`.` beyond the grammar). **Evidence:** the live key + the parser
  invoked on each malformed key with its actual error string quoted (`plugins/tmux/keys_test.go`).
- **AC-TMUX-NESTING-STALE.** With the reconcile interval far beyond the window, killing the last pane of
  a window drops it from `TmuxSession.windows` within 1 s (control-mode driven), and killing a session
  removes all its panes from every window's nesting within 1 s; a window still holding ≥1 live pane is
  unaffected. **Evidence:** the `kill-pane`/`kill-session` timestamp + nested query before and after.
- **AC-TMUX-MIGRATE-INPLACE.** Starting the new plugin against a DB written by the pre-#27 plugin (rows
  present) adds the five columns with no row dropped or altered, and a second start over the migrated DB
  is a no-op (duplicate-column tolerated). **Evidence:** row counts + a sample row from the old DB, then
  the same after first and after second start.
- **AC-TMUX-ADDONLY-ZEROCORE.** `git diff --stat core/` reports **0 files changed**, every pre-existing
  tmux scenario passes, and `tmuxSessions`/`tmuxPanes`/`slots`/`freeSlots`/`paneForBranch` return
  unchanged shapes for their existing fields. **Evidence:** empty `git diff --stat core/` + green `make features-tmux`.
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
