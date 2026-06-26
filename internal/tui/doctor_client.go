package tui

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/rohilrs/Hive/internal/doctor"
	"github.com/rohilrs/Hive/pkg/rpc"
)

// tuiDoctorClient adapts the TUI's *Client to the narrow doctor.RPCClient
// interface so the Doctor modal can reuse the full internal/doctor checks
// without an internal/daemon import. It mirrors cmd/hive/cmd_doctor.go's
// doctorRPCClient — same three RPCs (daemon.status / daemon.health /
// sources.status), same field-for-field JSON unmarshal — but dials over the
// TUI client's UDS helpers (Client.call returns the raw result JSON, which is
// exactly what we unmarshal here).
type tuiDoctorClient struct {
	c *Client
}

// newTUIDoctorClient wraps the TUI Client. The returned value is handed to
// doctor.Run, which calls it on a background goroutine (see app.go's
// TabDoctorRequest handler) because the checks do blocking RPC + filesystem
// reads that must stay off the Bubbletea UI thread.
func newTUIDoctorClient(c *Client) doctor.RPCClient {
	return &tuiDoctorClient{c: c}
}

// Status pings daemon.status. A successful response (even an empty result)
// means the socket is reachable; any error means it isn't — doctor compares
// the error against its own sentinel to decide skip-vs-error for the
// daemon-dependent checks.
func (d *tuiDoctorClient) Status(ctx context.Context) error {
	_, err := d.c.call(rpc.MethodStatus, map[string]any{})
	return err
}

// Health calls daemon.health and unmarshals into a doctor.HealthSnapshot.
// Both sides share the same JSON tags so the unmarshal is direct.
func (d *tuiDoctorClient) Health(ctx context.Context) (doctor.HealthSnapshot, error) {
	raw, err := d.c.call(rpc.MethodHealth, map[string]any{})
	if err != nil {
		return doctor.HealthSnapshot{}, err
	}
	var snap doctor.HealthSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		return doctor.HealthSnapshot{}, fmt.Errorf("unmarshal health: %w", err)
	}
	return snap, nil
}

// SourcesStatus calls sources.status and unmarshals into a map keyed by
// source name. Same JSON-tag mirroring as Health; an empty result is a
// valid "no bound sources" answer (returns a nil map, not an error).
func (d *tuiDoctorClient) SourcesStatus(ctx context.Context) (map[string]doctor.SourceStatusEntry, error) {
	raw, err := d.c.call(rpc.MethodSourcesStatus, map[string]any{})
	if err != nil {
		return nil, err
	}
	var out map[string]doctor.SourceStatusEntry
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse sources_status: %w", err)
	}
	return out, nil
}
