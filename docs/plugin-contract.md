# Plugin Contract

## Envelope

```go
type Envelope struct {
	// TS is the event time: source-assigned when the source carries one, else the
	// ingest time. Kept distinct from row insert order so late-arriving events sort
	// correctly by their real occurrence time.
	TS time.Time `json:"ts"`
	// Source is the emitting plugin's stable name (e.g. "github", "template"). It
	// doubles as the plugin's SQLite filename and health key, so it must be stable.
	Source string `json:"source"`
	// Type is the event type within a source (e.g. "issue.opened"). It lets
	// consumers route without decoding Payload.
	Type string `json:"type"`
	// V is the Payload schema version. Upcasters key off it; core never inspects it
	// beyond carrying it, so a bump never forces a core change.
	V int `json:"v"`
	// Key is the idempotency / entity key (e.g. "issue:org/repo#12@host"). It scopes
	// dedup and per-entity ordering, and carries the @host suffix for later peer/mesh
	// disambiguation.
	Key string `json:"key"`
	// Payload is the versioned event body, opaque to core. It is stored and returned
	// byte-identical so no core change is needed when a plugin evolves its schema.
	Payload json.RawMessage `json:"payload"`
}
```

## Plugin interface

The block below is **verbatim identical** to the one in `core/plugin.go` (CI diffs
the two sed-extracted blocks; AC-CORE-16). Do not reword it here alone.

```go
type Plugin interface {
	// Name is the plugin's stable id. It is used as the SQLite filename and the
	// health key, so it must never change across versions of a plugin.
	Name() string
	// Migrate runs add-only migrations against the plugin's own Store at boot. It
	// must be idempotent: core calls it every start.
	Migrate(ctx context.Context, s *Store) error
	// Start runs the plugin's listener/poller until ctx is done, calling emit for
	// each event. It runs in its own recover-guarded goroutine so a panic here
	// isolates to this plugin (marks it stale) rather than taking down the process.
	Start(ctx context.Context, emit Emit) error
}
```

### Optional interfaces

A plugin MAY implement either of these; core type-asserts for them, so a plugin that
ignores them still satisfies the required contract above.

```go
// CursorReporter surfaces a resume cursor in the health snapshot. Cursor is called
// while core holds the health-aggregator lock, so it must be cheap and non-blocking
// (an in-memory read, never I/O).
type CursorReporter interface {
	Cursor(ctx context.Context) string
}

// HTTPRoutes gives a plugin its own HTTP surface (e.g. a webhook receiver). Core
// mounts each pattern under /plugins/<name>/<pattern> — so a new HTTP source (the
// github webhook plugin, S5) needs zero core edit. Patterns are path-relative to the
// prefix; a leading slash is optional.
type HTTPRoutes interface {
	Routes() map[string]http.Handler
}
```

`/plugins/<name>/` routes are dispatched per-request against the running plugin, so a
plugin's routes work regardless of startup order. An unknown plugin, a plugin without
`HTTPRoutes`, or an unmatched pattern is a 404.

## HealthState and HealthStatus

```go
// HealthState is the coarse liveness classification core derives from event lag.
// It is a string so it serializes directly into the _health JSON and GraphQL enum
// without a translation table.
type HealthState string

const (
	// HealthStarting is the state before a plugin's first event has been observed.
	HealthStarting HealthState = "starting"
	// HealthOK means the plugin has emitted recently, within the lag threshold.
	HealthOK HealthState = "ok"
	// HealthStale means lag has crossed the threshold (or the plugin panicked): the
	// data is no longer trustworthy as current.
	HealthStale HealthState = "stale"
)

// HealthStatus is one plugin's freshness snapshot. Its JSON shape is the contract
// shared by GET /health and the health GraphQL query — the two must stay identical.
type HealthStatus struct {
	Plugin string `json:"plugin"`
	// LastEventAt is nil (JSON null) until the first event, deliberately distinct
	// from a zero time so consumers can tell "never emitted" from "emitted at epoch".
	LastEventAt *time.Time  `json:"lastEventAt"`
	Cursor      string      `json:"cursor"`
	LagSeconds  float64     `json:"lagSeconds"`
	State       HealthState `json:"state"`
}
```

## Registration

In `plugins/<name>/<name>.go`, self-register from `init()`:

```go
func init() {
	core.Register("<name>", New)
}
```

Add a blank import to `graph/plugins_import.go`:

```go
import _ "github.com/drewdrewthis/supergraph/plugins/<name>"
```

## SQLite rules

- Plugin owns `<DataDir>/<name>.db`; core opens it and wraps as a `Store`.
- Plugin owns STATE tables — create them add-only in `Migrate` using `IF NOT EXISTS`.
- Core owns `events` ring (auto-pruned to capacity N) and `cursors` table.
- Do NOT write to another plugin's db; do NOT write the `events` or `cursors` tables.

## Schema

Create `plugins/<name>/schema/<name>.graphqls` using `extend type Query` and `extend type Subscription`:

```graphql
extend type Query {
  <name>Field: String
}

extend type Subscription {
  <name>Events: String
}
```

Run `go run github.com/99designs/gqlgen generate` to produce stubs in `graph/<name>.resolvers.go`.

Fill stubs by delegating through the `core.Registry`:

```go
func (r *queryResolver) <Name>Field(ctx context.Context) (string, error) {
	// delegate to plugin resolver
}
```

## Health

A plugin does **not** implement a `Health()` method. Core derives every field of the
`HealthStatus` snapshot itself, from the emit path:

- `LastEventAt`: stamped by core on each `emit()` (nil until the first event).
- `LagSeconds` / `State`: derived by core from `LastEventAt` and the lag threshold.
- `Cursor`: pulled from the plugin's optional `CursorReporter.Cursor()`; empty if the
  plugin does not implement it.

## Liveness

Core emits **no** synthetic heartbeat. Every `HealthStatus` field derives only from a plugin's real `emit()` calls: a plugin that emits nothing keeps `lastEventAt` at its last real event (or `null` if it never emitted) and, once lag passes the threshold, shows `stale`. That `stale` is the intended liveness signal — a silent plugin is a stale plugin. Alerting is an external poller of the `_health` endpoint.

An **unconfigured** plugin (no config section, or missing required credentials) must go dormant instead: log once and return from `Start` without spawning any supervisor/poll loop, so it never emits — a plugin with nothing to say should say nothing, not retry-and-log-fail forever (see `plugins/github`'s no-token case).

### Shutdown

`cmd/supergraph serve` joins the Supervisor's `Run(ctx)` goroutine before the process exits — it does not return the instant `srv.Shutdown` completes. This matters because a plugin's `Start` is the only place it can reap its own children (e.g. `plugins/github`'s `gh webhook forward` child, killed via `exec.CommandContext` when `ctx` is cancelled): if the process exits before that kill lands, the child is orphaned instead of terminated.

Consequence for plugin authors: **`Start` must return promptly once `ctx` is cancelled**, after reaping any subprocess/goroutine it spawned. Do not block shutdown on unrelated work (a long poll, an un-cancellable network call) — the supervisor's join has a bounded deadline (the same deadline as HTTP shutdown); a plugin that blocks past it logs a warning but does not stop the process from exiting anyway, at which point its still-running children are orphaned exactly as before.

## Tests

Every plugin ships `plugins/<name>/<name>.feature` with BDD scenarios. Each scenario defines a complete e2e flow and maps one-to-one with a step in the test harness. Run all scenarios with `go test ./features/...` (godog).

## Isolation

`Start()` runs in its own goroutine wrapped in `recover()`. A panic in `Start()` isolates to that plugin (marks state `stale` via health check) and does NOT crash the process or affect other plugins.

## Checklist

1. Create `plugins/<name>/` directory tree.
2. Implement `core.Plugin` interface in `plugins/<name>/<name>.go`; call `core.Register("<name>", New)` from `init()`.
3. Create plugin's migrations in `Migrate()`, creating STATE tables with `IF NOT EXISTS`.
4. Create `plugins/<name>/schema/<name>.graphqls` with `extend type Query`/`extend type Subscription`.
5. Run `go run github.com/99designs/gqlgen generate`; fill resolver stubs in `graph/<name>.resolvers.go` delegating through `core.Registry`.
6. Add blank import to `graph/plugins_import.go`: `import _ "github.com/drewdrewthis/supergraph/plugins/<name>"`.
7. Create `plugins/<name>/<name>.feature` with BDD scenarios covering plugin behavior.
8. Verify: `git diff --stat core/` must be **0 files changed**; `go build ./...` succeeds; `go test ./features/...` passes.
