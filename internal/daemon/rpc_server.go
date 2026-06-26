package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"

	"github.com/rohilrs/Hive/pkg/rpc"
)

// RPCServer accepts connections on the daemon's Unix socket and dispatches
// JSON-RPC-shaped requests to the appropriate handler. The wire format is
// one request per line, with the response written back as one line.
type RPCServer struct{ d *Daemon }

// NewRPCServer constructs an RPCServer bound to the given Daemon.
func NewRPCServer(d *Daemon) *RPCServer { return &RPCServer{d: d} }

// Accept blocks accepting connections until ctx is canceled or the
// listener is closed. Each accepted connection is handled in its own
// goroutine.
func (s *RPCServer) Accept(ctx context.Context, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				// Listener closed or transient error; if ctx is still live
				// the next iteration will pick up the cancellation.
				if errors.Is(err, net.ErrClosed) {
					return
				}
				continue
			}
		}
		go s.handleConn(ctx, conn)
	}
}

func (s *RPCServer) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil && err != io.EOF {
		return
	}
	if len(line) == 0 {
		return
	}

	var envelope struct {
		ID     string          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		s.writeError(conn, "", rpc.ErrParseError, "invalid JSON")
		return
	}

	// Phase 3.5a: streaming branch. events.subscribe holds the
	// connection open and writes events forever (line-delimited
	// EventMessage JSON) until the client closes or the daemon
	// shuts down.
	if envelope.Method == rpc.MethodSubscribeEvents {
		s.streamEvents(ctx, conn, envelope.ID)
		return
	}

	// Phase 6.1a: chat.send streaming branch. Holds the connection open
	// for the duration of one agent turn, writing line-delimited Frame
	// response envelopes, then closes (via the deferred conn.Close).
	if envelope.Method == rpc.MethodChatSend {
		s.streamChat(ctx, conn, envelope.ID, envelope.Params)
		return
	}

	result, rpcErr := s.dispatch(ctx, envelope.Method, envelope.Params)
	if rpcErr != nil {
		s.writeError(conn, envelope.ID, rpcErr.Code, rpcErr.Message)
		return
	}
	resp := rpc.Response[json.RawMessage]{
		ID:     envelope.ID,
		Result: &result,
	}
	payload, _ := json.Marshal(resp)
	conn.Write(payload)
	conn.Write([]byte("\n"))
}

// streamEvents subscribes to the daemon's bus and writes each event
// as a line-delimited JSON EventMessage. Returns when the connection
// breaks (client disconnect) or the daemon shuts down.
//
// Wire format: first line is a response envelope acknowledging the
// subscribe (so the client knows the stream is established);
// subsequent lines are bare EventMessage JSON (no envelope) so the
// client's read loop can decode each line as an EventMessage directly.
func (s *RPCServer) streamEvents(ctx context.Context, conn net.Conn, reqID string) {
	if s.d.bus == nil {
		s.writeError(conn, reqID, rpc.ErrInternal, "no event bus configured")
		return
	}
	ch, cancel := s.d.bus.Subscribe()
	defer cancel()

	ackData := map[string]any{"subscribed": true}
	ack := rpc.Response[map[string]any]{ID: reqID, Result: &ackData}
	payload, _ := json.Marshal(ack)
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			line, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := conn.Write(append(line, '\n')); err != nil {
				return
			}
		}
	}
}

func (s *RPCServer) writeError(w io.Writer, id string, code int, msg string) {
	raw, _ := json.Marshal(rpc.Response[json.RawMessage]{
		ID:    id,
		Error: &rpc.RPCError{Code: code, Message: msg},
	})
	w.Write(raw)
	w.Write([]byte("\n"))
}
