# Core-Tier Implementation Plan — Spike (issue #1)

**Scope:** the core shell that must be **locked before any plugin** (PRD §6, milestone "core").
Plugins (tmux, claude, github, peer, telegram) are OUT of scope except one **template** plugin
that proves S5/F5. Source of truth: [docs/PRD.md](../PRD.md) §5 (ACs) + §6 (design/CLI/build plan).

Greenfield: repo has only `README.md`, `.gitignore`, `docs/PRD.md`. All paths below are **new files**.

---

## 1. Goal

A locked core binary: ingest envelope, Go plugin contract, per-plugin SQLite base, gqlgen GraphQL
server with websocket subscriptions on `127.0.0.1:7788/graphql`, `_health` (JSON + GraphQL),
goroutine+recover plugin isolation, canary timer hook, full CLI with OS-detected install, config,
one-command dev harness, a godog `.feature` runner, and a **template plugin** that compiles in with
`git diff --stat core/` = 0.

## 2. Non-goals

- Any real data-source plugin (github/tmux/claude/peer/telegram) — template only.
- Peer/mesh mirroring, WireGuard, `hostId` peer taxonomy beyond the field existing on the envelope key.
- The actual Telegram alert delivery (telegram plugin) — core exposes the canary + lag **signal** only.
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
  canary.go                # timer hook: synthetic envelope per plugin through the real emit path
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
type Plugin interface {
	Name() string                                   // stable id: SQLite filename + health key
	Migrate(ctx context.Context, s *Store) error    // add-only migrations at boot on own db
	Start(ctx context.Context, emit Emit) error     // run listener/poller until ctx done; calls emit
	Health(ctx context.Context) HealthStatus        // freshness snapshot for _health
}

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
6. **Isolation supervisor + canary** (`core/supervisor.go`, `canary.go`). **Contract:** each `Start` runs in its own goroutine wrapped in `recover()`; panic sets that plugin `stale`, others keep serving; canary timer emits a synthetic envelope per plugin through the real `emit` path on an interval. **Done:** unit test — panicking plugin → its `state==stale`, second plugin still `ok`; canary test asserts synthetic envelope observed within interval; a plugin that stops emitting crosses threshold → `stale`.
7. **GraphQL server + WS subscriptions** (`core/server.go`). **Contract:** gqlgen handler with POST + `transport.Websocket` bound to `127.0.0.1:7788`; `emit` fans out to subscribers. **Done:** integration test — start server, open WS subscription, `emit` one event, assert push received < 1s.
8. **Config loader** (`core/config.go`). **Contract:** parse `~/.config/supergraph/config.toml` (hostId, peers, tokens); **missing `hostId` is a startup error, not a silent default.** **Done:** unit test parses sample toml; missing hostId → non-nil error.
9. **CLI** (`cmd/supergraph/*.go`, `core/service/*`). **Contract:** `serve` (foreground, stdout logs); `install/uninstall` write OS-detected unit idempotently; `start/stop/restart/status`; `query '<graphql>'` POSTs to local endpoint, prints JSON. **Done:** `supergraph serve` boots; `supergraph query '{ __typename }'` round-trips; `install` run twice → exit 0 both, one unit present.
10. **Dev harness** (`Makefile`). **Contract:** one command builds, points dbs at a temp dir, runs server with the template plugin. **Done:** `make dev` → server up, `/health` shows `template` ok.
11. **`.feature` runner** (`features/`, `features/suite_test.go` godog). **Contract:** `go test ./features/...` runs `.feature` scenarios; unmet scenario fails red. **Done:** met scenario green; a deliberately-unmet scenario → `go test` exit ≠ 0.
12. **Template plugin** (`plugins/template/*`, `graph/plugins_import.go` blank import, regen). **Contract:** implements `core.Plugin`, self-registers via `init()`, emits a demo event, exposes health, **includes a panic-inject path** for F5. **Done:** compiles in, `/health` shows template; `git diff --stat core/` = 0; induce panic → template `stale`, `/health` + base query still answer.
13. **Plugin contract doc stub** (`docs/plugin-contract.md`). **Contract:** documents Envelope, Plugin interface, SQLite/migration rules, `extend type` + registration steps, `.feature` requirement. **Done:** doc's interface block matches `core/plugin.go` verbatim (grep the exact `Start(ctx context.Context, emit Emit) error` line in both).

## 7. Risks

1. **[one-way-ish] gqlgen zero-core-edit registration (D2) is UNPROVEN.** That gqlgen compiles `extend type Query` across globbed plugin schemas AND preserves delegating resolver bodies on regen is the spike's whole goal #3. Mitigation: prove it at Step 4/12; fall back to manual embed in `graph/resolver.go` (still non-core, S5 still holds).
2. **Subscription sufficiency (D4).** PRD §4 flags gqlgen subscriptions over the mesh UNVERIFIED. Core proves **local** push only (AC-CORE-8); peer/mesh is a later tier — do not over-claim here.
3. **modernc SQLite concurrency (D1).** Goroutine-per-plugin + ring auto-prune must not deadlock reads during writes. Mitigation: one writer per db file, WAL + `busy_timeout`; validated in Step 3.
4. **Canary false-green (PRD §6, F6 spirit).** "A canary that never fires is broken, not all-clear." Core must expose a positive-fire signal, not just absence — AC-CORE-5 asserts the synthetic event is actually observed.

## 8. AC draft
<!-- ACs ready for ac-reviewer -->

Core-tier acceptance criteria. Each is falsifiable with a named evidence shape (proof-it-works). Threshold `T` = lag threshold from config (test uses a short value).

- **AC-CORE-1 (Envelope contract).** An `Envelope{TS,Source,Type,V,Key,Payload}` JSON-marshals and unmarshals to an equal value; `Payload` survives as opaque `json.RawMessage`; a `V:2` payload round-trips unchanged (no rewrite). Fails if any field is dropped/reordered-lossy or `Payload` is re-parsed. **Evidence:** `go test ./core -run Envelope -v` stdout, PASS lines quoted.
- **AC-CORE-2 (SQLite ring + cursor).** With ring capacity N, appending N+5 events leaves exactly N rows (oldest pruned); `SetCursor("since","X")` then reopening the db returns `"X"`. Fails if row count ≠ N or cursor is empty after reopen. **Evidence:** test stdout showing `rows=N` and `cursor=X`.
- **AC-CORE-3 (Health endpoint shape).** `GET /health` returns HTTP 200 and a JSON array; each element has exactly `{plugin,lastEventAt,cursor,lagSeconds,state}` with `state ∈ {starting,ok,stale}`; the `health` GraphQL query returns the same set. `lastEventAt` is `null` (not `""`/epoch) before any event. Fails if a key is missing, `state` is outside the enum, or the two endpoints disagree. **Evidence:** `curl -s 127.0.0.1:7788/health | jq` output + the GraphQL query result, both quoted.
- **AC-CORE-4 (Panic isolation — F5).** Inducing a panic in the template plugin's `Start` sets its `_health` `state` to `stale` within one health interval; `GET /health` still returns 200 and a second (non-panicking) fake plugin still reads `ok`; the base `{ __typename }` query still returns 200. Fails if the process exits, `/health` errors, or the sibling plugin flips off `ok`. **Evidence:** `/health` JSON captured after induced panic (template `stale`, other `ok`) + exit-code-0 server still serving.
- **AC-CORE-5 (Canary positive-fire).** With the canary timer at interval I, a synthetic envelope for each registered plugin is observed on the emit path within 2·I (positive fire, per F6 spirit); and a plugin whose `Start` stops emitting for > T crosses to `state=stale`. Fails if no synthetic event is observed (never-fired = broken) or a stalled plugin stays `ok`. **Evidence:** test log showing the captured synthetic envelope(s) + the stale transition timestamp.
- **AC-CORE-6 (CLI serve + query round-trip).** `supergraph serve` binds `127.0.0.1:7788`; in a second shell `supergraph query '{ __typename }'` prints JSON containing `"Query"` and exits 0; binding a second `serve` fails with a clear "address in use" error (not a silent success). **Evidence:** terminal capture of both commands' stdout + exit codes.
- **AC-CORE-7 (Install idempotent, OS-detected).** On Linux, `supergraph install` writes a `systemd --user` unit and enables it; on macOS it writes a launchd plist. Running `install` twice both exit 0 and leave exactly one unit/plist (no duplicate, no error on second run); `uninstall` removes it. Fails if the second run errors or a duplicate unit appears. **Evidence:** `ls`/`systemctl --user status` (or `launchctl list`) before/after two installs, quoted.
- **AC-CORE-8 (Local subscription push).** A websocket GraphQL subscription client connected to `127.0.0.1:7788/graphql` receives a pushed message < 1s after a matching event is `emit`ted; closing the socket stops delivery with no server error. Fails if no message arrives within 1s or the server logs an error on client disconnect. **Evidence:** test/CLI capture of the subscribe → emit → received-payload sequence with timestamps.
- **AC-CORE-9 (.feature runner red/green).** `go test ./features/...` executes `.feature` scenarios via godog; a scenario with all steps satisfied reports pass; a scenario with one deliberately-unmet step makes `go test` exit non-zero and names the failing step. Fails if an unmet scenario is reported green or skipped. **Evidence:** two `go test ./features/...` runs — one exit 0, one exit ≠ 0 with the failing step name quoted.
- **AC-CORE-10 (S5 — zero core edit).** After adding the template plugin (new files under `plugins/template/` + `graph/plugins_import.go` + regenerated `graph/`), `git diff --stat core/` reports **0 files changed**, and the running binary serves `_health` for `template`. Fails if any file under `core/` appears in the diff. **Evidence:** `git diff --stat core/` output (empty) + `/health` screenshot showing `template`.
- **AC-CORE-11 (Config validation).** A valid `config.toml` with `hostId` loads and exposes hostId/peers/tokens; a config **missing `hostId`** makes `serve` exit non-zero with a message naming `hostId` (never a silent default). Fails if a missing hostId boots anyway. **Evidence:** two `serve` attempts — one boots, one exits with the quoted error.

## 9. Handoff

- ACs ready for ac-reviewer (see §8 AC draft above).
- Implementation → **coder** for Steps 2,3,5,7,8,11,12 (contracts explicit); **advanced-coder** for Step 4 (gqlgen glob/follow-schema is the judgment-bearing seam, D2 risk) and Step 6 (isolation/canary design); **fast-coder** for Step 1 (mechanical bootstrap) and Step 13 (doc from the interface). Per `~/.knowledge/modules/shared/records/model-selection.md`.
