package core

import (
	"context"
	"sync"
)

// subBuffer is the per-subscriber channel depth on the Bus. A subscriber that falls
// this far behind starts losing the oldest events rather than stalling the emit path.
const subBuffer = 256

// Bus fans out emitted envelopes to live subscribers. Each subscriber gets its own
// buffered channel; a full channel drops the event for THAT subscriber only (select
// with a default) so one slow consumer can never block AppendEvent, the canary, or a
// sibling subscriber. Dropping is acceptable here because subscriptions are a live
// tail, not a durable log — the Store is the durable record.
type Bus struct {
	mu   sync.Mutex
	next int
	subs map[int]chan Envelope
}

// NewBus builds an empty Bus.
func NewBus() *Bus {
	return &Bus{subs: map[int]chan Envelope{}}
}

// Subscribe returns a channel of future envelopes. The subscription lives until ctx
// is cancelled, at which point the channel is removed and closed so ranging callers
// terminate cleanly.
func (b *Bus) Subscribe(ctx context.Context) <-chan Envelope {
	ch := make(chan Envelope, subBuffer)
	b.mu.Lock()
	id := b.next
	b.next++
	b.subs[id] = ch
	b.mu.Unlock()

	go func() {
		<-ctx.Done()
		b.mu.Lock()
		delete(b.subs, id)
		close(ch)
		b.mu.Unlock()
	}()
	return ch
}

// publish delivers e to every subscriber, dropping for any whose buffer is full.
func (b *Bus) publish(e Envelope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- e:
		default: // slow subscriber: drop rather than block the shared emit path
		}
	}
}
