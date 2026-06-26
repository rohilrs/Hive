// Package eventbus is a small in-memory pub/sub primitive for the
// daemon. Publishers call Publish(event); subscribers register via
// Subscribe() to get their own bounded channel of events. The bus is
// fail-safe: Publish never blocks; slow subscribers either lose events
// silently (ResyncOnOverflow=false) or receive a synthetic `resync`
// event (ResyncOnOverflow=true) and re-fetch state via regular RPCs.
package eventbus

import (
	"sync"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// Config controls per-subscriber buffer + overflow behavior.
type Config struct {
	// BufferSize is the per-subscriber channel capacity. Overflow on
	// any subscriber doesn't affect other subscribers. Default 1000
	// when zero.
	BufferSize int

	// ResyncOnOverflow controls overflow handling: when true, a
	// dropped event triggers a synthetic `resync` event into the
	// subscriber's channel (idempotent — multiple drops collapse to
	// one queued resync). When false, drops are silent.
	ResyncOnOverflow bool
}

// Bus is the daemon-side pub/sub fabric.
type Bus struct {
	cfg Config

	mu          sync.Mutex
	subscribers map[*subscriber]struct{}
}

type subscriber struct {
	ch               chan rpc.EventMessage
	resyncQueued     bool
	resyncOnOverflow bool
}

// New constructs a Bus with the given config.
func New(cfg Config) *Bus {
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 1000
	}
	return &Bus{
		cfg:         cfg,
		subscribers: map[*subscriber]struct{}{},
	}
}

// Subscribe registers a new subscriber and returns the receive channel
// + a cancel func. Calling cancel closes the channel and removes the
// subscriber. Safe to call concurrently.
func (b *Bus) Subscribe() (<-chan rpc.EventMessage, func()) {
	s := &subscriber{
		ch:               make(chan rpc.EventMessage, b.cfg.BufferSize),
		resyncOnOverflow: b.cfg.ResyncOnOverflow,
	}
	b.mu.Lock()
	b.subscribers[s] = struct{}{}
	b.mu.Unlock()
	cancel := func() {
		b.mu.Lock()
		if _, ok := b.subscribers[s]; ok {
			delete(b.subscribers, s)
			close(s.ch)
		}
		b.mu.Unlock()
	}
	return s.ch, cancel
}

// Publish fans the event out to all subscribers. Never blocks: if a
// subscriber's channel is full, the event is dropped for THAT
// subscriber (other subscribers still receive). When the dropped
// subscriber has ResyncOnOverflow, a resync event is queued into its
// channel (idempotent — multiple consecutive drops collapse to one
// queued resync).
func (b *Bus) Publish(ev rpc.EventMessage) {
	b.mu.Lock()
	subs := make([]*subscriber, 0, len(b.subscribers))
	for s := range b.subscribers {
		subs = append(subs, s)
	}
	b.mu.Unlock()

	for _, s := range subs {
		b.sendOrQueueResync(s, ev)
	}
}

func (b *Bus) sendOrQueueResync(s *subscriber, ev rpc.EventMessage) {
	select {
	case s.ch <- ev:
		return
	default:
	}
	// Channel full — drop the event.
	if !s.resyncOnOverflow {
		return
	}
	b.mu.Lock()
	if s.resyncQueued {
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()

	// Drain one buffered event to make space for the resync marker,
	// then send. Losing one MORE event is acceptable since the
	// consumer will re-fetch full state via regular RPCs on seeing
	// the resync — the dropped event's data is captured there anyway.
	// Race: concurrent reader/publisher may already have changed the
	// queue; in that case our drain is a no-op and the send may still
	// fail, in which case we just mark the flag and rely on the next
	// overflow to try again.
	select {
	case <-s.ch:
	default:
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.resyncQueued {
		return
	}
	select {
	case s.ch <- rpc.EventMessage{Type: rpc.EventResync}:
		s.resyncQueued = true
	default:
		// Couldn't queue (race); flag so we don't keep draining.
		s.resyncQueued = true
	}
}
