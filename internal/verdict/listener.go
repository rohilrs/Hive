package verdict

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
)

type Listener struct {
	ln         net.Listener
	sockPath   string
	frames     chan Frame
	rejections chan Ack
	closed     chan struct{}
	once       sync.Once
}

func Listen(sockPath string) (*Listener, error) {
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	if err := os.Chmod(sockPath, 0600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("chmod %s: %w", sockPath, err)
	}
	l := &Listener{
		ln:         ln,
		sockPath:   sockPath,
		frames:     make(chan Frame, 4),
		rejections: make(chan Ack, 4),
		closed:     make(chan struct{}),
	}
	go l.accept()
	return l, nil
}

func (l *Listener) Frames() <-chan Frame   { return l.frames }
func (l *Listener) Rejections() <-chan Ack { return l.rejections }

func (l *Listener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		_ = l.ln.Close()
		_ = os.Remove(l.sockPath)
	})
	return nil
}

func (l *Listener) accept() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			select {
			case <-l.closed:
				return
			default:
				continue
			}
		}
		go l.handle(conn)
	}
}

func (l *Listener) handle(conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 0, 64*1024), 1*1024*1024)
	if !sc.Scan() {
		writeAck(conn, Ack{OK: false, Error: "no frame received"})
		return
	}
	var f Frame
	if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
		writeAck(conn, Ack{OK: false, Error: "invalid frame: " + err.Error()})
		return
	}
	if f.Verdict == "CHANGES_REQUESTED" && len(f.FileRefs) == 0 {
		ack := Ack{OK: false, Error: AckErrFileRefsMissing}
		writeAck(conn, ack)
		select {
		case l.rejections <- ack:
		case <-l.closed:
		}
		return
	}
	writeAck(conn, Ack{OK: true})
	select {
	case l.frames <- f:
	case <-l.closed:
	}
}

// Submit injects a Frame as if it had arrived over the UDS socket.
// Used by the daemon's HTTP MCP route to bypass the per-stage UDS hop
// when claude calls hive_submit_review_verdict via HTTP transport.
//
// Same validation as the UDS handler: CHANGES_REQUESTED without
// FileRefs is rejected (returns the rejection Ack + an error).
func (l *Listener) Submit(f Frame) (*Ack, error) {
	if f.Verdict == "CHANGES_REQUESTED" && len(f.FileRefs) == 0 {
		ack := Ack{OK: false, Error: AckErrFileRefsMissing}
		select {
		case l.rejections <- ack:
		case <-l.closed:
			return &ack, fmt.Errorf("listener closed")
		default:
		}
		return &ack, fmt.Errorf("CHANGES_REQUESTED missing file_refs")
	}
	select {
	case l.frames <- f:
	case <-l.closed:
		return nil, fmt.Errorf("listener closed")
	default:
		return nil, fmt.Errorf("frame channel full")
	}
	return &Ack{OK: true}, nil
}

func writeAck(conn net.Conn, a Ack) {
	raw, _ := json.Marshal(a)
	conn.Write(append(raw, '\n'))
}
