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
	// Health returns a freshness snapshot for the _health endpoint. It is polled by
	// core, not pushed, so it must be cheap and non-blocking.
	Health(ctx context.Context) HealthStatus
}
```

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

`Health()` must return a fresh `HealthStatus` snapshot including:

- `LastEventAt`: timestamp of the most recent event emitted (nil if no events yet).
- `Cursor`: opaque string identifying the last processed checkpoint (for dedup/resume).
- `LagSeconds`: seconds since `LastEventAt` (core will derive state from lag threshold).

## Canary / heartbeat

Core emits a synthetic `canary` event envelope per plugin on a periodic interval, sent through the normal `emit()` path. Plugins receive no alert injection code; alerting is an external poller of the `_health` endpoint.

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
