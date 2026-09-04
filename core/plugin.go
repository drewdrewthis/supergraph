package core

import (
	"context"
	"time"
)

// Emit hands one event to core for persistence (the SQLite ring) and subscription
// fan-out. Plugins receive it in Start and are the only writers of events; keeping
// emission behind this func lets core own the write path (ring pruning, fan-out)
// without the plugin touching the DB directly.
type Emit func(ctx context.Context, e Envelope) error

// Plugin is the FIXED contract for every data source compiled into the binary.
// It is deliberately small: adding a source must not require editing core, so the
// four methods below are the whole surface core depends on.
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

// PluginConfig is the per-plugin construction input handed to a Factory. Core
// resolves HostID and DataDir centrally; Raw carries the plugin's own toml section
// undecoded so core never needs to know a plugin's config schema.
type PluginConfig struct {
	// HostID is this box's identity, used to stamp envelope keys for later peer
	// disambiguation.
	HostID string
	// DataDir is the base directory under which the plugin's SQLite file lives.
	DataDir string
	// Raw is the plugin-specific [plugins.<name>] toml section, left opaque to core.
	Raw map[string]any
}
