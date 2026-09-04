//go:build harness

// Package fakeok is a second reference plugin whose only job is to stay healthy
// while a sibling plugin fails. It exists so the panic-isolation contract
// (AC-CORE-4) is provable end to end: when the template plugin panics in Start,
// a live server must still report a DIFFERENT plugin as "ok". It carries no data
// source and contributes no GraphQL schema — it just emits a heartbeat on a short
// interval so its /health entry reads "ok" independent of the canary. It is built
// only under the `harness` tag so it never ships in a production binary.
package fakeok

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/drewdrewthis/supergraph/core"
)

// tickInterval is the heartbeat cadence. It is short and fixed (not configurable)
// so fakeok stays "ok" on its own, without depending on the canary — the panic
// scenario deliberately runs the canary at a long interval.
const tickInterval = time.Second

func init() {
	core.Register("fakeok", New)
}

// Plugin is a plugin that emits a heartbeat and never panics.
type Plugin struct {
	hostID string
	hb     core.Heartbeat
}

// New builds the fakeok plugin from its resolved config.
func New(cfg core.PluginConfig) (core.Plugin, error) {
	return &Plugin{hostID: cfg.HostID}, nil
}

// Name returns the plugin's stable id.
func (p *Plugin) Name() string { return "fakeok" }

// Migrate is a no-op: fakeok keeps no state tables of its own.
func (p *Plugin) Migrate(_ context.Context, _ *core.Store) error { return nil }

// Start emits one hello immediately, then a heartbeat every tickInterval until
// ctx is done. It never panics — that is the whole point of this plugin.
func (p *Plugin) Start(ctx context.Context, emit core.Emit) error {
	if err := p.emit(ctx, emit, "fakeok.hello"); err != nil {
		return err
	}
	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.emit(ctx, emit, "fakeok.tick"); err != nil {
				return err
			}
		}
	}
}

func (p *Plugin) emit(ctx context.Context, emit core.Emit, eventType string) error {
	n, now := p.hb.Mark()

	payload, err := json.Marshal(map[string]any{"n": n})
	if err != nil {
		return fmt.Errorf("fakeok: marshal payload: %w", err)
	}
	return emit(ctx, core.Envelope{
		TS:      now,
		Source:  p.Name(),
		Type:    eventType,
		V:       1,
		Key:     "fakeok:heartbeat@" + p.hostID,
		Payload: payload,
	})
}
