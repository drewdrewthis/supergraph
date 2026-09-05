// Package single holds the one-running-plugin-instance-per-process convention: each
// plugin publishes its live instance here on New/Migrate so a package-level Get lets
// graph/ resolvers read plugin state without a core-injected accessor (core is
// LOCKED, so this is the zero-core-edit seam — see docs/edr/peer.md, claude.md,
// tmux.md). One process runs one instance of a given plugin, so a lock-free pointer
// swap is sufficient: New/Migrate calls Set, resolvers call Get, and a nil/unstarted
// Get yields the caller's own "no rows" zero value rather than a panic.
package single

import "sync/atomic"

// Ptr publishes and reads a single running instance of T.
type Ptr[T any] struct {
	p atomic.Pointer[T]
}

// Set publishes v as the current instance.
func (s *Ptr[T]) Set(v *T) { s.p.Store(v) }

// Get returns the current instance, or nil if none has been Set yet.
func (s *Ptr[T]) Get() *T { return s.p.Load() }
