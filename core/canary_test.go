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

// AC-CORE-4: after a plugin panics in Start, the canary must NOT keep firing for it
// (that would advance lastEventAt and mask the crash as ok). Across many intervals the
// dead plugin stays stale and receives zero canary envelopes, while a live sibling
// keeps getting them and stays ok.
func TestCanaryNoHeartbeatForDeadPlugin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &recorder{}
	bad := newFake("template", true, rec)
	ok := newFake("fakeok", false, rec)
	cfg := Config{HostID: "h", DataDir: t.TempDir(), LagThresholdSeconds: 300}
	h := NewHealthAggregator(cfg.LagThresholdSeconds)
	bus := NewBus()
	sv := NewSupervisor(cfg, map[string]Factory{
		"template": factoryFor(bad),
		"fakeok":   factoryFor(ok),
	}, h, bus)

	sv.startAll(ctx)
	defer sv.Stop()

	<-ok.emitted
	waitForState(t, h, "template", HealthStale) // panic recovered + canary stopped

	sub := bus.Subscribe(ctx)
	go NewCanary(sv, 0.01).Run(ctx) // 10ms

	time.Sleep(80 * time.Millisecond) // >= 5 intervals of observation

	var sawSiblingCanary bool
drain:
	for {
		select {
		case e := <-sub:
			if e.Type == "canary" && e.Source == "template" {
				t.Fatalf("dead plugin received a canary heartbeat: %+v", e)
			}
			if e.Type == "canary" && e.Source == "fakeok" {
				sawSiblingCanary = true
			}
		default:
			break drain
		}
	}
	if !sawSiblingCanary {
		t.Error("live sibling stopped receiving canaries")
	}
	byName := snapByName(t, h)
	if byName["template"].State != HealthStale {
		t.Errorf("dead plugin state = %s, want stale", byName["template"].State)
	}
	if byName["fakeok"].State != HealthOK {
		t.Errorf("sibling state = %s, want ok", byName["fakeok"].State)
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
