package daemon

import (
	"github.com/rohilrs/Hive/internal/daemon/eventbus"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// busPublisher implements pipeline.EventPublisher over *eventbus.Bus.
// Pipeline doesn't import eventbus directly — it sees this adapter
// through the EventPublisher interface.
type busPublisher struct {
	bus *eventbus.Bus
}

func newBusPublisher(bus *eventbus.Bus) *busPublisher { return &busPublisher{bus: bus} }

func (p *busPublisher) Publish(ev rpc.EventMessage) {
	if p.bus == nil {
		return
	}
	p.bus.Publish(ev)
}
