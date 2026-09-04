package core

import (
	"context"
	"testing"
	"time"
)

// AC-CORE-5: the canary is observable as a POSITIVE fire — a synthetic envelope
// lands on a Bus subscription (proving it flows through the real emit path).
func TestCanaryPositiveFire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plug := newFake("template", false, &recorder{})
	cfg := Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300}
	bus := NewBus()
	sv := NewSupervisor(cfg, map[string]Factory{"template": factoryFor(plug)}, NewHealthAggregator(cfg.LagThresholdSeconds), bus)

	sv.startAll(ctx)
	defer sv.Stop()
	<-plug.emitted // plugin running + store registered before we subscribe

	sub := bus.Subscribe(ctx)
	go NewCanary(sv, 0.01).Run(ctx) // 10ms interval

	deadline := time.After(time.Second)
	for {
		select {
		case e := <-sub:
			if e.Type == "canary" && e.Source == "template" && e.Key == "canary:template" {
				return // observed positive fire
			}
		case <-deadline:
			t.Fatal("no canary envelope observed on the bus")
		}
	}
}

// AC-CORE-5: a plugin whose canary is suppressed (StopCanary) stops emitting and
// visibly crosses the lag threshold into stale.
func TestCanaryStopThenStale(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	plug := newFake("template", false, &recorder{})
	cfg := Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 0.05} // 50ms
	h := NewHealthAggregator(cfg.LagThresholdSeconds)
	sv := NewSupervisor(cfg, map[string]Factory{"template": factoryFor(plug)}, h, NewBus())

	sv.startAll(ctx)
	defer sv.Stop()
	<-plug.emitted

	go NewCanary(sv, 0.01).Run(ctx) // keeps lastEventAt fresh -> ok
	waitForState(t, h, "template", HealthOK)

	sv.StopCanary("template") // no more emits -> lag climbs past 50ms
	waitForState(t, h, "template", HealthStale)
}
