# Core-Tier Implementation Plan — Spike (issue #1)

**Scope:** the core shell that must be **locked before any plugin** (PRD §6, milestone "core").
Plugins (github, tmux, claude, peer) are OUT of scope except one **template** plugin
that proves S5/F5. Source of truth: [docs/PRD.md](../PRD.md) §5 (ACs) + §6 (design/CLI/build plan).

Greenfield: repo has only `README.md`, `.gitignore`, `docs/PRD.md`. All paths below are **new files**.

---

## 1. Goal

A locked core binary: ingest envelope, Go plugin contract, per-plugin SQLite base, gqlgen GraphQL
server with websocket subscriptions on `127.0.0.1:7788/graphql`, `_health` (JSON + GraphQL),
goroutine+recover plugin isolation, full CLI with OS-detected install, config,
one-command dev harness, a godog `.feature` runner, and a **template plugin** that compiles in with
`git diff --stat core/` = 0.

## 2. Non-goals

- Any real data-source plugin (github, tmux, claude, peer) — template only.
- Peer/mesh mirroring, WireGuard, `hostId` peer taxonomy beyond the field existing on the envelope key.
- Any alert sender or synthetic heartbeat in the binary — `/health` reports lastEventAt/lagSeconds/state derived only from real plugin emits; alerting is an external poller of `/health` (out of scope, owner decision 2026-09-04).
- Final GraphQL schema (the EDR owns it) — core ships only base types + the extend seam.
- Event sourcing, backfill/reconcile logic, webhook ingress — those are the github plugin's tier.

## 3. Module + package layout

Go module: **`github.com/drewdrewthis/supergraph`** (matches repo path). Go 1.26.

```
cmd/supergraph/            # cobra CLI entrypoint (serve/install/.../query)
core/                      # LOCKED SHELL — never edited to add a plugin (S5 diff scope)
  envelope.go              # Envelope type
  plugin.go                # Plugin interface, Emit, HealthStatus/HealthState
  registry.go              # Register()/Factories() — append-only, plugins self-register via init()
  store.go                 # per-plugin SQLite base: open + WAL, event ring, cursors
  health.go                # aggregator: []HealthStatus, state derivation from lag
  supervisor.go            # goroutine-per-plugin + recover() -> stale
  server.go                # gqlgen handler mount: POST + transport.Websocket on 127.0.0.1:7788
  config.go                # ~/.config/supergraph/config.toml loader (hostId, peers, tokens)
  service/                 # process mgmt: systemd_linux.go (--user unit), launchd_darwin.go (plist)
  graph/schema/base.graphqls   # base: Health, root Query, Subscription, scalars
graph/                     # gqlgen OUTPUT (NOT core/): generated.go, models, resolver.go, *.resolvers.go
  plugins_import.go        # blank-imports each plugin package (the one compile-in seam)
plugins/template/          # the S5/F5 template plugin (own pkg, schema, migrations, .feature)
  template.go  schema/template.graphqls  template.feature
features/                  # godog suite wired to `go test` (S1..S6/F-scenarios land here later)
gqlgen.yml  tools.go       # codegen config (schema glob) + pinned gqlgen tool
Makefile                   # `make dev` one-command harness
```

## 4. Key interfaces (exact Go)

```go
package core

// Envelope is the single ingest unit every plugin emits. Immutable once emitted; core never
// rewrites it (versioning is add-only via V + upcasters, PRD §6).
type Envelope struct {
	TS      time.Time       `json:"ts"`      // event time (source-assigned, else ingest time)
	Source  string          `json:"source"`  // plugin name, e.g. "github", "template"
	Type    string          `json:"type"`    // event type within source, e.g. "issue.opened"
	V       int             `json:"v"`        // payload schema version, for upcasters
	Key     string          `json:"key"`      // idempotency/entity key, e.g. "issue:org/repo#12@host"
	Payload json.RawMessage `json:"payload"`  // versioned, opaque to core
}

// Emit hands one event to core for persistence (ring) + subscription fan-out.
type Emit func(ctx context.Context, e Envelope) error

// Plugin is the FIXED contract. Every data source is a Plugin compiled into the binary.
// Core derives freshness from the emit path — a plugin has no Health() method.
type Plugin interface {
	Name() string                                   // stable id: SQLite filename + health key
	Migrate(ctx context.Context, s *Store) error    // add-only migrations at boot on own db
	Start(ctx context.Context, emit Emit) error     // run listener/poller until ctx done; calls emit
}

// Optional, type-asserted by core (a plugin may implement neither):
type CursorReporter interface { Cursor(ctx context.Context) string }        // cursor for _health snapshot
type HTTPRoutes interface { Routes() map[string]http.Handler }              // mounted at /plugins/<name>/<pattern> (S5)

type HealthState string
const (
	HealthStarting HealthState = "starting"
	HealthOK       HealthState = "ok"
	HealthStale    HealthState = "stale"
)

type HealthStatus struct {
	Plugin      string      `json:"plugin"`
	LastEventAt *time.Time  `json:"lastEventAt"`  // nil until first event
	Cursor      string      `json:"cursor"`
	LagSeconds  float64     `json:"lagSeconds"`
	State       HealthState `json:"state"`
}

// Registry — plugins self-register from their own init(); adding a plugin does NOT edit this file.
type Factory func(cfg PluginConfig) (Plugin, error)
func Register(name string, f Factory) // panics on duplicate name
func Factories() map[string]Factory

// Store is core's per-plugin SQLite base (open/WAL/ring/cursors). Plugin owns its STATE tables.
type Store struct { /* *sql.DB + ring capacity */ }
func (s *Store) DB() *sql.DB
func (s *Store) AppendEvent(ctx context.Context, e Envelope) error       // auto-prunes ring to N
func (s *Store) SetCursor(ctx context.Context, name, value string) error
func (s *Store) Cursor(ctx context.Context, name string) (string, error)
```

## 5. Key decisions (with alternatives)

| # | Decision | Chosen | Alternatives rejected |
|---|----------|--------|-----------------------|
| D1 | **SQLite driver** | `modernc.org/sqlite` v1.58.0 (**pure Go, no cgo**) | `mattn/go-sqlite3` (cgo) — breaks the PRD's single cross-compiled binary; needs a C toolchain per target. modernc is slightly slower but the workload is light (PRD §4). |
| D2 | **gqlgen zero-core-edit registration** (goal #3, S5) | Schema **glob** `plugins/*/schema/*.graphqls` in `gqlgen.yml`, each plugin `extend`s `type Query`/`Subscription`; `resolver.layout: follow-schema` writes stubs into `graph/`; stub body delegates to the plugin fetched from `core.Registry`. Compile-in = one blank import in `graph/plugins_import.go`. **All of this lives outside `core/`.** | (a) Manual embed of plugin resolver structs in `graph/resolver.go` — kept as the **fallback** if follow-schema delegation misbehaves (still non-core). (b) Runtime schema stitching of N ExecutableSchemas — gqlgen has no clean runtime multi-schema stitch; rejected. |
| D3 | **`.feature` runner** | `github.com/cucumber/godog` v0.16.0, wired to `go test` (canonical Cucumber-for-Go). Verified fetchable. | Hand-rolled gherkin parser — reinventing a solved problem; rejected. |
| D4 | **Subscription transport** | gqlgen `transport.Websocket` (graphql-ws) on the same `/graphql`. | SSE — gqlgen supports it, but WS is the proven default and the peer plugin will need WS over the mesh. |
| D5 | **Process mgmt** | `install` writes a `systemd --user` unit on Linux / `launchd` plist on macOS (OS-detected at runtime); idempotent via overwrite + reload + enable. | A separate installer script — CLI-owned keeps one binary, matches PRD §6 CLI block. |
| D6 | **Config parser** | `pelletier/go-toml/v2` | `BurntSushi/toml` — fine but go-toml/v2 is faster/maintained. Minor. |
| D7 | **CLI framework** | `spf13/cobra` | stdlib flag — cobra gives subcommands + status output cheaply. |

## 6. Ordered implementation steps (coder-sized)

Each step: **files → contract → done-check.** TDD: test/`.feature` first where noted.

1. **Module bootstrap.** `go mod init github.com/drewdrewthis/supergraph`; add deps (gqlgen, modernc/sqlite, godog, go-toml/v2, cobra); `tools.go` pins gqlgen. **Done:** `go build ./...` + `go vet ./...` clean on empty scaffold.
2. **Envelope + Plugin contract + registry** (`core/envelope.go`, `plugin.go`, `registry.go`). **Contract:** §4 signatures exactly; `Register` panics on duplicate. **Done:** `go test ./core -run 'Envelope|Registry'` — JSON round-trip incl. `V`, `Payload` opaque; register/list/duplicate-panic.
3. **SQLite base `Store`** (`core/store.go`). **Contract:** open at `~/.local/share/supergraph/<name>.db` with WAL + `busy_timeout`; `AppendEvent` auto-prunes ring to capacity N; cursors persist. **Done:** unit test opens temp db, appends N+5, asserts ring capped at N; cursor round-trips across reopen.
4. **gqlgen scaffold + base schema** (`gqlgen.yml`, `core/graph/schema/base.graphqls`, `graph/*`). **Contract:** glob + follow-schema config; `go run github.com/99designs/gqlgen generate` produces `graph/generated.go`; base `health` query resolves. **Done:** `go build ./...`; resolver test hits `{ health { plugin state } }`.
5. **Health aggregator + `/health` JSON + `health` query** (`core/health.go`). **Contract:** aggregate `HealthStatus` over registered plugins; `state` derived from `lagSeconds` vs threshold; both endpoints return identical shape. **Done:** unit test with 2 fake plugins → JSON `[{plugin,lastEventAt,cursor,lagSeconds,state}]`.
6. **Isolation supervisor** (`core/supervisor.go`). **Contract:** each `Start` runs in its own goroutine wrapped in `recover()`; panic sets that plugin `stale` (forcedStale, sticky until restart), others keep serving. No synthetic heartbeat — health derives from real emits only. **Done:** unit test — panicking plugin → its `state==stale`, second plugin still `ok`; a plugin that stops emitting crosses threshold → `stale`.
7. **GraphQL server + WS subscriptions** (`core/server.go`). **Contract:** gqlgen handler with POST + `transport.Websocket` bound to `127.0.0.1:7788`; `emit` fans out to subscribers. **Done:** integration test — start server, open WS subscription, `emit` one event, assert push received < 1s.
8. **Config loader** (`core/config.go`). **Contract:** parse `~/.config/supergraph/config.toml` (hostId, peers, tokens); **missing `hostId` is a startup error, not a silent default.** **Done:** unit test parses sample toml; missing hostId → non-nil error.
9. **CLI** (`cmd/supergraph/*.go`, `core/service/*`). **Contract:** `serve` (foreground, stdout logs); `install/uninstall` write OS-detected unit idempotently; **`start/stop/restart/status` drive the installed service** — `start` launches it, `status` reports running/not-running + pid + listening port and exits 0, `stop` frees the port, `restart` picks up a replaced binary; `query '<graphql>'` POSTs to local endpoint, prints JSON. **Done (lifecycle, AC-CORE-14):** `install` → `start` → `status` (running, :7788) → `stop` → `status` (not running); `install` run twice → exit 0 both, one unit present; `supergraph query '{ __typename }'` round-trips.
10. **Dev harness** (`Makefile`). **Contract:** one command builds, points dbs at a temp dir, runs server with the template plugin. **Done:** `make dev` → server up, `/health` shows `template` ok.
11. **`.feature` runner** (`features/`, `features/suite_test.go` godog). **Contract:** `go test ./features/...` runs `.feature` scenarios; unmet scenario fails red. **Done:** met scenario green; a deliberately-unmet scenario → `go test` exit ≠ 0.
12. **Template plugin** (`plugins/template/*`, `graph/plugins_import.go` blank import, regen). **Contract:** implements `core.Plugin`, self-registers via `init()` (Register → Factories, no `core/` edit), `Migrate` creates its state tables at boot (idempotent), emits a demo `V:2` event, exposes health, **contributes a `templatePing` query field and a `templateEvents` subscription field via `extend type Query`/`extend type Subscription`** (proves the D2 merged-schema seam), and **includes a panic-inject path** for F5. **Done:** compiles in, `/health` shows template, `git diff --stat core/` = 0; `query '{ templatePing }'` returns resolver data through the merged schema; `templateEvents` pushes ≥1 message; induce panic → template `stale`, `/health` + base query still answer.
13. **Plugin contract doc stub** (`docs/plugin-contract.md`). **Contract:** documents Envelope, Plugin interface, SQLite/migration rules, `extend type` + registration steps, `.feature` requirement. **Done:** doc's interface block matches `core/plugin.go` verbatim (grep the exact `Start(ctx context.Context, emit Emit) error` line in both).

## 7. Risks

1. **[one-way-ish] gqlgen zero-core-edit registration (D2) is UNPROVEN.** That gqlgen compiles `extend type Query` across globbed plugin schemas AND preserves delegating resolver bodies on regen is the spike's whole goal #3. Mitigation: prove it at Step 4/12; fall back to manual embed in `graph/resolver.go` (still non-core, S5 still holds).
2. **Subscription sufficiency (D4).** PRD §4 flags gqlgen subscriptions over the mesh UNVERIFIED. Core proves **local** push only (AC-CORE-8); peer/mesh is a later tier — do not over-claim here.
3. **modernc SQLite concurrency (D1).** Goroutine-per-plugin + ring auto-prune must not deadlock reads during writes. Mitigation: one writer per db file, WAL + `busy_timeout`; validated in Step 3.
4. **Synthetic-heartbeat false-green (PRD §6, F6 spirit) — RESOLVED by removal.** The synthetic per-plugin heartbeat was dropped (owner decision 2026-09-04): it advanced `lastEventAt` on its own and could mask a wedged plugin as ok. Health now derives only from real plugin emits, so a plugin that emits nothing crosses the lag threshold into `stale` — the intended signal — with no false-green to defend against.

## 8. AC draft
<!-- ACs ready for ac-reviewer -->

Core-tier acceptance criteria. **Primary evidence is use-proof: an observation from the running binary this turn** (unit/`go test` runs stay as regression backfill, not as the AC evidence). Threshold `T` = lag threshold from config (test uses a short value); interval `I` = a plugin's own emit interval.

- **AC-CORE-1 (Envelope contract, use-proof).** The template plugin emits a `V:2` event through the **running binary**; reading it back via `query`/subscription returns a `Payload` that is **byte-identical** to what was emitted and `v == 2` (no rewrite, opaque round-trip). Fails if `Payload` bytes differ, `v != 2`, or any envelope field is dropped. **Evidence:** emitted bytes vs `supergraph query`/subscription stdout, both quoted; JSON-equal shown. (Regression backfill: `go test ./core -run Envelope`.)
- **AC-CORE-2 (SQLite ring + cursor, use-proof).** Driving the running binary to emit N+5 template events, a query of the ring shows exactly **N rows** (oldest pruned); after `stop`+`start` the plugin's cursor is unchanged. Fails if row count ≠ N or the cursor resets on restart. **Evidence:** query/`sqlite3 <db>` row count = N + cursor value before/after restart, quoted. (Backfill: `go test ./core` ring/cursor unit test.)
- **AC-CORE-3 (Health endpoint shape).** `GET /health` returns HTTP 200 and a JSON array; each element has exactly `{plugin,lastEventAt,cursor,lagSeconds,state}` with `state ∈ {starting,ok,stale}`; the `health` GraphQL query returns the same set. `lastEventAt` is `null` (not `""`/epoch) before any event. Fails if a key is missing, `state` is outside the enum, or the two endpoints disagree. **Evidence:** `curl -s 127.0.0.1:7788/health | jq` output + the GraphQL query result, both quoted.
- **AC-CORE-4 (Panic isolation — F5).** In a serve harness with **two registered plugins — `template` and a second `fakeok` fake that never panics** — inducing a panic in `template`'s `Start` sets `template` `_health` `state=stale` within one interval; `GET /health` still returns 200, `fakeok` still reads `ok`, and the base `{ __typename }` query still returns 200. Fails if the process exits, `/health` errors, or `fakeok` flips off `ok`. **Evidence:** `/health` JSON after induced panic (`template` stale, `fakeok` ok) + server still serving (exit-code-0 process). (Backfill: `go test ./core` two-fake unit test.)
- **AC-CORE-5 — removed (owner decision 2026-09-04):** the synthetic canary heartbeat is gone. Core emits nothing on its own; `/health.lastEventAt/lagSeconds/state` derive only from real plugin emits, so a plugin that stops emitting crosses the lag threshold into `stale` (that stale IS the liveness signal). Positive-freshness is now covered by each plugin's own tick advancing `lastEventAt` (see AC-CORE-3 and the template/fakeok emits), and stale detection by AC-CORE-4's forced-stale path.
- **AC-CORE-6 (CLI serve + query round-trip).** `supergraph serve` binds `127.0.0.1:7788`; in a second shell `supergraph query '{ __typename }'` prints JSON containing `"Query"` and exits 0; binding a second `serve` fails with a clear "address in use" error (not a silent success). **Evidence:** terminal capture of both commands' stdout + exit codes.
- **AC-CORE-7a (Install idempotent, Linux/systemd).** On Linux, `supergraph install` writes a `systemd --user` unit and enables it (requires a live user session / lingering). Running `install` twice both exit 0 and leave exactly one unit (no duplicate, no error on second run); `uninstall` removes it. Fails if the second run errors or a duplicate unit appears. **Evidence:** captured on **Linux spike boxes (hetzner-agents, langwatch-dev): `systemctl --user status supergraph`** — before/after two installs, quoted.
- **AC-CORE-7b (Install idempotent, macOS/launchd).** On macOS, `supergraph install` writes a launchd plist. Running `install` twice both exit 0 and leave exactly one plist (no duplicate, no error on second run); `uninstall` removes it. Fails if the second run errors or a duplicate plist appears. **Evidence:** captured on **macOS (dev box): `launchctl list | grep supergraph`** — before/after two installs, quoted.
- **AC-CORE-8 (Local subscription push).** A websocket GraphQL subscription client subscribed to **`templateEvents`** on `127.0.0.1:7788/graphql` receives a pushed message < 1s after a matching event is `emit`ted; closing the socket stops delivery with no server error. Fails if no message arrives within 1s or the server logs an error on client disconnect. **Evidence:** CLI/client capture of subscribe(`templateEvents`) → emit → received-payload with timestamps.
- **AC-CORE-9 (.feature runner red/green).** `go test ./features/...` executes `.feature` scenarios via godog; a scenario with all steps satisfied reports pass; a scenario with one deliberately-unmet step makes `go test` exit non-zero and names the failing step. Fails if an unmet scenario is reported green or skipped. **Evidence:** two `go test ./features/...` runs — one exit 0, one exit ≠ 0 with the failing step name quoted.
- **AC-CORE-10 (S5 — zero core edit).** After adding the template plugin (new files under `plugins/template/` + `graph/plugins_import.go` + regenerated `graph/`), `git diff --stat core/` reports **0 files changed**, and the running binary serves `_health` for `template`. Fails if any file under `core/` appears in the diff. **Evidence:** `git diff --stat core/` output (empty) + `/health` screenshot showing `template`.
- **AC-CORE-10b (Extend seam resolves).** Through the merged schema, `supergraph query '{ templatePing }'` returns the template resolver's data (not null, not a schema error), and the template's `extend type Subscription` field `templateEvents` pushes ≥1 message to a subscriber. This proves D2 — the plugin's `extend type Query`/`Subscription` fields resolve without a `core/` edit. Fails if `templatePing` errors/returns null or `templateEvents` never pushes. **Evidence:** `supergraph query '{ templatePing }'` stdout + a `templateEvents` received message.
- **AC-CORE-11 (Config validation).** A valid `config.toml` with `hostId` loads and exposes hostId/peers/tokens; a config **missing `hostId`** makes `serve` exit non-zero with a message naming `hostId` (never a silent default). Fails if a missing hostId boots anyway. **Evidence:** two `serve` attempts — one boots, one exits with the quoted error.
- **AC-CORE-12 (Registry contract).** A plugin that calls `core.Register` from its own `init()` appears in `Factories()` at boot **with no edit under `core/`**; registering a duplicate name **panics with a message naming the duplicate**. Fails if the plugin is absent from the boot listing, or a duplicate registers silently / panics without naming the name. **Evidence:** `serve` boot log listing `template` among factories + a deliberate duplicate-registration build whose panic message quotes the duplicate name.
- **AC-CORE-13 (Migrate at boot, idempotent).** First `serve` against an empty data dir creates the template plugin's tables **before `Start`**; a second `serve` re-runs `Migrate` with no error and no duplicate objects. Fails if tables are missing after boot 1, `Start` runs before `Migrate`, or boot 2 errors. **Evidence:** `sqlite3 <template.db> .tables` after boot 1 and boot 2 (identical, non-empty), quoted.
- **AC-CORE-14 (CLI lifecycle).** After `install`: `start` → the service is listening on `127.0.0.1:7788`; `status` exits 0 with a running summary (pid + port); `stop` frees the port and a subsequent `status` reports **not running**; after replacing the binary, `restart` serves the **new** build. Fails if `status` misreports state, `stop` leaves the port bound, or `restart` keeps serving the old build. **Evidence:** `start`→`status`→`stop`→`status` capture with a port check (`lsof -i:7788`/`curl`) at each step + a version marker showing the new build after `restart`.
- **AC-CORE-15 (`make dev` one command).** `make dev` builds, points the plugin dbs at a temp dir, and starts the server with the template plugin; after a bounded wait, `GET /health` returns 200 with `template` present. Fails if it needs a second command, writes to the real `~/.local/share` data dir, or `/health` lacks `template` within the wait. **Evidence:** capture of `make dev` + the `curl 127.0.0.1:7788/health` showing `template`.
- **AC-CORE-16 (Contract doc matches code).** The `type Plugin interface { ... }` block in `docs/plugin-contract.md` is **verbatim identical** to the one in `core/plugin.go`. Fails if a `diff` of the two sed-extracted blocks is non-empty. **Evidence:** `diff <(sed -n '/type Plugin interface/,/}/p' core/plugin.go) <(sed -n '/type Plugin interface/,/}/p' docs/plugin-contract.md)` returns empty (exit 0).
- **AC-CORE-17 (Concurrent read during write).** With the store under WAL + `busy_timeout`, a writer loop appending template events runs **concurrently** with a GraphQL read that returns rows — no `"database is locked"` (SQLITE_BUSY) error surfaces on either side. Fails if any lock/busy error appears in server logs or client output during the concurrent run. **Evidence:** capture of the write-loop + concurrent read completing, with a `grep -i 'database is locked'` over the run log returning nothing.

## 9. Handoff

- ACs ready for ac-reviewer (see §8 AC draft above).
- Implementation → **coder** for Steps 2,3,5,7,8,11,12 (contracts explicit); **advanced-coder** for Step 4 (gqlgen glob/follow-schema is the judgment-bearing seam, D2 risk) and Step 6 (isolation design); **fast-coder** for Step 1 (mechanical bootstrap) and Step 13 (doc from the interface). Per `~/.knowledge/modules/shared/records/model-selection.md`.
