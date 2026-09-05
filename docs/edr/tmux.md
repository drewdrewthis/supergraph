# EDR: tmux plugin — server-wide control-mode watcher over local SQLite

Engineering Design Record. Owns the internals the [PRD](../PRD.md) §6 and [plugin
contract](../plugin-contract.md) leave to the plugin. Act-doc: decisions + steps, not prose.
Mirrors the shape of [github EDR](./github.md).

**Design (2026-09-05).** The tmux plugin is a **read model of the local tmux server**: one
long-lived **control-mode client** (`tmux -C attach`) streams server-wide structural
notifications into a per-plugin SQLite cache; a low-cadence **reconcile poll** (`list-panes -a`
/ `list-sessions`) backstops missed events and recomputes free/busy. It serves
`TmuxServer/Session/Window/Pane` + a `Slot` projection over one GraphQL endpoint via the gqlgen
`extend type` glob seam — **no `HTTPRoutes`, no webhook, no fake server**: `@local` scenarios run
against a **real tmux server on a temp socket** (`-L <name>`), which is hermetic and needs no
credentials or hardware. Its reason to exist in the spike is **S2** (cross-box free-slot query)
and the **issue ↔ branch/worktree** join the github plugin already reserves as
`paneForBranch @pending`.

**Hard constraints (owner / PRD):** ≤ **600 LOC prod** for `plugins/tmux/**` excluding tests;
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
| **D7** | **Backoff reconnect** | On `%exit`/EOF (server died or was killed) the control client marks all local entities `staleSince` and **exponential-backoff reconnects**; a returning server re-attaches and a fresh poll re-populates. | Exit the plugin (would drop the source permanently); tight reconnect loop (busy-spins while tmux is down). |

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
window   := "window:"     <session>":"<window> "@" <hostId>
pane     := "pane:"       <session>":"<window>"."<pane> "@" <hostId>
```
`<window>`/`<pane>` are tmux indices (ints). Example: `pane:main:1.0@drudru-lan`.
- **Slot** has no key of its own — it is a **projection** over `pane` keys where the pane is free.
- The parse/format lives in `keys.go` as a **data table** (`kindSpecs`), mirroring
  `github/keys.go`: `parse(key) → (kind,scope,hostId)`, `paneKey(session,window,pane,host)`,
  `typename(kind)`, and the free/busy classifier.

## Envelopes emitted (`{TS, Source:"tmux", Type, V:1, Key, Payload}`)

`/health` `lastEventAt` derives from these; the star is `tmux.pane.*`:
- **`tmux.pane.updated`** — `Payload={key,session,window,pane,pid,cmd,path,active,free}` — pane
  added or changed (from `%window-add`/`%window-pane-changed`/`%layout-change`, or a reconcile diff).
- **`tmux.pane.closed`** — `Payload={key}` — pane gone (layout no longer lists it, or reconcile miss); marks `staleSince`.
- **`tmux.session.updated`** / **`tmux.session.closed`** — `Payload={key,worktree,branch}` / `{key}`.
- **`tmux.window.updated`** / **`tmux.window.closed`** — `Payload={key}`.
- **`tmux.snapshot`** — `Payload={hostId,paneCount}` — one per **successful** reconcile poll; the
  liveness signal for a structurally-idle but alive server (§Freshness, Owner Q1).
- **`tmux.server.down`** — `Payload={hostId}` — `%exit`/EOF from the control client; marks every
  local session/window/pane `staleSince` (the local shape of F8's stale-peer, before the peer plugin).

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

Named ops loaded and run via `supergraph query --op NAME` (core CLI; no `cmd/` delta):
- **`freeSlots`** — `slots(hostId){ hostId kind free staleSince }` filtered `free:true` — the **S2** op.
- **`sessions`** — `tmuxSessions(hostId){ … worktree branch lastSeenAt staleSince }`.
- **`panes`** — `tmuxPanes(hostId){ … pid cmd path active free staleSince }`.
- **`paneForBranch`** — `paneForBranch(branch){ … }` — the github/claude join (issue ↔ branch).
- **`slotsForHost`** — full `slots(hostId)` (free + busy) for dashboards.

## Schema (`plugins/tmux/schema/tmux.graphqls`, `extend type` only)

Non-GitHub types keep the `Tmux*` prefix (PRD §6). Fields from the PRD sketch:
```graphql
type TmuxServer  { hostId: String!  lastSeenAt: Time  staleSince: Time }
type TmuxSession { hostId: String!  name: String!  worktree: String  branch: String  lastSeenAt: Time  staleSince: Time }
type TmuxWindow  { hostId: String!  session: TmuxSession!  index: Int!  name: String  staleSince: Time }
type TmuxPane    { hostId: String!  window: TmuxWindow!  index: Int!  pid: Int!  cmd: String!  path: String  active: Boolean!  free: Boolean!  staleSince: Time }
type Slot        { hostId: String!  kind: String!  free: Boolean!  pane: TmuxPane  staleSince: Time }
extend type Query {
  tmuxSessions(hostId: String): [TmuxSession!]!
  tmuxPanes(hostId: String): [TmuxPane!]!
  slots(hostId: String): [Slot!]!
  paneForBranch(branch: String!): [TmuxPane!]!
}
extend type Subscription { tmuxEvents(hostId: String): TmuxPane! }
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
- `tmux_windows(key TEXT PK, session_key, idx, name, last_seen_at, stale_since)`
- `tmux_panes(key TEXT PK, window_key, idx, pid, cmd, path, active INT, free INT, last_seen_at, stale_since)`
- `hostId`/indices are parsed from the key, not stored as columns (github convention).
- Cursor: `snapshot:lastAt` (last successful reconcile time) in core's `cursors` table; `Cursor()` reports it.

## LOC budget (prod, cap **600**; tests excluded)

Strip formula (mirrors github's, portable GNU/BSD sed):
`find plugins/tmux -name '*.go' ! -name '*_test.go' | xargs sed -E '/^[[:space:]]*\/\//d;/^[[:space:]]*$/d' | wc -l` — fails > 600.

| File (`plugins/tmux/`) | Budget | Responsibility |
|---|---:|---|
| `tmux.go` | 80 | wiring: `Register`/`New`/`Name`/`Migrate`/`Start`/`CursorReporter` + config parse |
| `keys.go` | 70 | key grammar data table: parse + object→key + typename + free/busy classifier |
| `control.go` | 150 | control-mode client: spawn `tmux -C attach`, hold stdin, `no-output`, parse `%`-notifications → envelopes, `%exit`→`server.down`, backoff reconnect |
| `snapshot.go` | 120 | `list-panes -a`/`list-sessions` parse → upsert; reconcile diff → `*.closed`/`staleSince`; `tmux.snapshot` emit; free/busy recompute |
| `store.go` | 120 | SQLite: migrate 3 tables, upsert, get, close(staleSince), scan-free, paneForBranch |
| `resolver.go` | 60 | GraphQL resolvers for sessions/panes/slots/freeSlots/paneForBranch via `core.Registry` |
| **Total** | **600** | cap **600**; no `cmd/` delta (serves via gqlgen glob seam, D6) |

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

- **AC-TMUX-S2 (PRD S2).** Against a **seeded realistic db** (warm), `supergraph query --op freeSlots`
  returns free slots with **p95 < 1 s over N≥20 samples**. Fails if p95 ≥ 1 s or a free slot is
  omitted. **Evidence:** timing capture over the 20 reads (github-style p95 harness). *(Cross-box
  fan-out is the peer plugin's; this is the local warm-read half S2 actually measures.)*
- **AC-TMUX-EVENTS-CONTROL.** With the plugin watching a temp-socket server via control mode,
  splitting a pane / opening a window **in a different session** makes the new pane queryable within
  `T`. Fails if a server-wide event is missed or arrives after `T`. **Evidence:** the mutating
  `tmux` command + the `tmuxPanes` query showing the new pane, with timestamps.
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
- **AC-TMUX-KEY-GRAMMAR.** A pane key round-trips `pane:<session>:<window>.<pane>@<hostId>`
  (`parse(format(x)) == x`); the parser rejects a malformed key rather than returning a partial one.
  Fails if round-trip differs or a bad key parses silently. **Evidence:** round-trip assertion output.
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
- **AC-TMUX-LOC.** The strip-formula count for `plugins/tmux/**` (excluding `*_test.go`) is **≤ 600**.
  **Evidence:** `make loc-tmux` output ≤ 600.
- **AC-TMUX-F8 (cross-box, @pending).** After the peer plugin exists, a stopped box shows its tmux
  data as `stale since T` on a peer **< 30 s**, with no peer-of-peer rows. **@pending** — needs the
  peer plugin + a second box. **Evidence (deferred):** peer query screenshot + grep.

## prd.feature deltas (for a later coder — this EDR does NOT touch prd.feature)

Mirror the github pattern (github removed its owned S3/F2/F3/F7 from prd.feature):
1. **Retire the S2 scenario from `features/prd.feature`.** `features/tmux.feature` now owns **S2**
   as a concrete `@local @tmux @S2` scenario (`freeSlots` p95 < 1 s warm). Remove
   `@pending @plugin-tier @S2` and its line in the AC Coverage Map to preserve the scenario↔AC
   bijection (no double-ownership).
2. **F5 stays in prd.feature (claude-owned trigger).** Its evidence already reads "…the other
   plugins, including tmux, keep answering". No move; `tmux.feature`'s `AC-TMUX-ISOLATION` covers
   tmux's half locally. Leave the prd.feature F5 scenario as-is.
3. **F8 stays in prd.feature (peer-owned).** `tmux.feature` carries a `@pending @F8` cross-reference
   for the tmux-data portion only; do **not** move F8 out of prd.feature.

No other prd.feature edits. Everything else tmux-specific lives in `features/tmux.feature`.

## Handoff

- ACs ready for ac-reviewer (see §Acceptance criteria above + `features/tmux.feature`).
- Implementation → **coder** for `keys.go`/`store.go`/`snapshot.go`/`resolver.go`/`tmux.go`
  (contracts explicit); **advanced-coder** for `control.go` (the control-mode parser + backoff
  reconnect + `%exit` handling is the judgment-bearing seam). Wave 2 (godog steps in
  `features/steps_tmux_test.go`) depends on Wave 1 compiling. Per
  `~/.knowledge/modules/shared/records/model-selection.md`.
- One prd.feature edit (delta #1 above) is a **fast-coder** task, gated on this EDR landing.
