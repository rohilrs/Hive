package daemon

import (
	"context"
	"time"

	"github.com/rohilrs/Hive/internal/daemon/eventbus"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// startHeartbeat fires a daemon.heartbeat event every `period`. Stops
// when ctx is canceled. Zero period disables heartbeats entirely.
func startHeartbeat(ctx context.Context, bus *eventbus.Bus, period time.Duration) {
	if period <= 0 || bus == nil {
		return
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			bus.Publish(rpc.EventMessage{
				Type: rpc.EventDaemonHeartbeat,
				Data: map[string]any{"ts": now.Unix()},
			})
		}
	}
}
