package core

import (
	"strings"
	"sync"
	"testing"
)

// resetRegistry clears package state so tests do not leak factories into each other.
func resetRegistry() {
	regMu.Lock()
	defer regMu.Unlock()
	factories = map[string]Factory{}
}

func fakeFactory(_ PluginConfig) (Plugin, error) { return nil, nil }

func TestRegisterAndFactories(t *testing.T) {
	resetRegistry()

	Register("alpha", fakeFactory)
	Register("beta", fakeFactory)

	got := Factories()
	if len(got) != 2 {
		t.Fatalf("want 2 factories, got %d", len(got))
	}
	if _, ok := got["alpha"]; !ok {
		t.Errorf("missing alpha")
	}
	if _, ok := got["beta"]; !ok {
		t.Errorf("missing beta")
	}
}

// Factories returns a copy: mutating the result must not corrupt the registry.
func TestFactoriesReturnsCopy(t *testing.T) {
	resetRegistry()
	Register("alpha", fakeFactory)

	got := Factories()
	delete(got, "alpha")

	if _, ok := Factories()["alpha"]; !ok {
		t.Fatalf("mutating returned map affected registry")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	resetRegistry()
	Register("dup", fakeFactory)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on duplicate registration")
		}
		if !strings.Contains(toStr(r), "dup") {
			t.Errorf("panic message must name the duplicate, got: %v", r)
		}
	}()
	Register("dup", fakeFactory)
}

// Register is safe under concurrent callers (plugins self-register from init()).
func TestRegisterConcurrent(t *testing.T) {
	resetRegistry()
	var wg sync.WaitGroup
	names := []string{"a", "b", "c", "d", "e"}
	for _, n := range names {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			Register(name, fakeFactory)
		}(n)
	}
	wg.Wait()
	if len(Factories()) != len(names) {
		t.Fatalf("want %d factories, got %d", len(names), len(Factories()))
	}
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return ""
}
