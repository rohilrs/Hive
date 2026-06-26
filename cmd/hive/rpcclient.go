package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// rpcCallRaw issues an RPC and returns the raw `result` JSON (which may
// be an object OR an array, depending on the method).
func rpcCallRaw(method string, params map[string]any) (json.RawMessage, error) {
	return rpcCallRawWithTimeout(method, params, 10*time.Second)
}

// rpcCallRawWithTimeout is rpcCallRaw with a caller-supplied read
// deadline. Use for RPCs whose server-side handler can take longer than
// 10s (decompose/roadmap.decompose spawn Sonnet via subscription, which
// is ~15-30s end-to-end).
func rpcCallRawWithTimeout(method string, params map[string]any, readTimeout time.Duration) (json.RawMessage, error) {
	sockPath := daemonSocketPath()
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w (is `hive daemon` running?)", err)
	}
	defer conn.Close()

	req := map[string]any{
		"id":     fmt.Sprintf("cli-%d", time.Now().UnixNano()),
		"method": method, "params": params,
	}
	raw, _ := json.Marshal(req)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		return nil, err
	}

	_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
	buf := make([]byte, 256*1024)
	n, err := conn.Read(buf)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(buf[:n], &resp); err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, errors.New(resp.Error.Message)
	}
	return resp.Result, nil
}

// rpcCall is the object-result convenience wrapper. Methods returning a
// JSON array (e.g. task.list) must use rpcCallRaw instead.
func rpcCall(method string, params map[string]any) (map[string]any, error) {
	raw, err := rpcCallRaw(method, params)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func daemonSocketPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".hive", "daemon.sock")
}
