package core

import (
	"fmt"
	"sync"
)

// Factory constructs a Plugin from its resolved config. Plugins register a Factory
// (not an instance) so core controls when and with what config each is built.
type Factory func(cfg PluginConfig) (Plugin, error)

var (
	regMu     sync.Mutex
	factories = map[string]Factory{}
)

// Register records a plugin factory under name. Plugins call it from their own
// init(), so adding a plugin never edits this file (the S5 zero-core-edit seam).
// It panics on a duplicate name: two plugins sharing a name would collide on the
// SQLite filename and health key, and that is a build-time programmer error, not a
// runtime condition to recover from.
func Register(name string, f Factory) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := factories[name]; exists {
		panic(fmt.Sprintf("core: duplicate plugin registration for name %q", name))
	}
	factories[name] = f
}

// Factories returns a copy of the registered factories so callers can range over
// them without holding the lock or risking mutation of the registry.
func Factories() map[string]Factory {
	regMu.Lock()
	defer regMu.Unlock()
	out := make(map[string]Factory, len(factories))
	for name, f := range factories {
		out[name] = f
	}
	return out
}
