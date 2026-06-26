package verdict

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

func Forward(sockPath string, frame Frame) (*Ack, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial verdict socket: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	payload, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(payload, '\n')); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	if !sc.Scan() {
		return nil, fmt.Errorf("no ack: %w", sc.Err())
	}
	var ack Ack
	if err := json.Unmarshal(sc.Bytes(), &ack); err != nil {
		return nil, fmt.Errorf("decode ack: %w", err)
	}
	return &ack, nil
}
