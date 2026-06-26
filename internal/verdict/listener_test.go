package verdict

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestListenerReceivesFrame(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	listener, err := Listen(sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer listener.Close()

	go func() {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			t.Errorf("Dial: %v", err)
			return
		}
		defer conn.Close()
		payload, _ := json.Marshal(Frame{
			RunID: "r1", Stage: "review", Verdict: "APPROVE", Confidence: 92,
		})
		conn.Write(append(payload, '\n'))
		var ack Ack
		_ = json.NewDecoder(conn).Decode(&ack)
		if !ack.OK {
			t.Errorf("ack not OK: %+v", ack)
		}
	}()

	select {
	case f := <-listener.Frames():
		if f.Verdict != "APPROVE" {
			t.Errorf("verdict=%s", f.Verdict)
		}
	case <-ctx.Done():
		t.Fatal("timed out")
	}
}

func TestForwarderEndToEndWithFileRefs(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	listener, _ := Listen(sockPath)
	defer listener.Close()

	refs := []FileRef{
		{Path: "internal/foo/bar.go", Line: 42, Comment: "missing nil check", Reasoning: "panics on empty input"},
	}
	ack, err := Forward(sockPath, Frame{
		RunID: "r2", Stage: "review",
		Verdict: "CHANGES_REQUESTED", Confidence: 80,
		FileRefs: refs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Errorf("ack not OK: %+v", ack)
	}

	select {
	case f := <-listener.Frames():
		if len(f.FileRefs) != 1 {
			t.Fatalf("file_refs len=%d", len(f.FileRefs))
		}
		got := f.FileRefs[0]
		if got.Path != "internal/foo/bar.go" || got.Line != 42 ||
			got.Comment != "missing nil check" || got.Reasoning != "panics on empty input" {
			t.Errorf("file_ref=%+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("listener didn't receive frame")
	}
}

func TestListenerRejectsMissingFileRefsOnChangesRequested(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	listener, _ := Listen(sockPath)
	defer listener.Close()

	ack, err := Forward(sockPath, Frame{
		RunID: "r3", Stage: "review",
		Verdict: "CHANGES_REQUESTED", Confidence: 75,
		// no FileRefs
	})
	if err != nil {
		t.Fatal(err)
	}
	if ack.OK {
		t.Errorf("expected ack rejection, got OK")
	}
	if ack.Error != AckErrFileRefsMissing {
		t.Errorf("ack.Error=%q want %q", ack.Error, AckErrFileRefsMissing)
	}

	select {
	case f := <-listener.Frames():
		t.Errorf("expected no frame published, got %+v", f)
	case <-time.After(200 * time.Millisecond):
		// good — rejection should NOT publish to the channel
	}
}

func TestListenerRejectionChannelDelivers(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	listener, _ := Listen(sockPath)
	defer listener.Close()

	_, err := Forward(sockPath, Frame{
		RunID: "r5", Stage: "review",
		Verdict: "CHANGES_REQUESTED", Confidence: 70,
	})
	if err != nil {
		t.Fatal(err)
	}

	select {
	case ack := <-listener.Rejections():
		if ack.OK || ack.Error != AckErrFileRefsMissing {
			t.Errorf("ack=%+v", ack)
		}
	case <-time.After(time.Second):
		t.Fatal("no rejection delivered")
	}
}

func TestListenerAcceptsApproveWithoutFileRefs(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "verdict.sock")
	listener, _ := Listen(sockPath)
	defer listener.Close()

	ack, err := Forward(sockPath, Frame{
		RunID: "r4", Stage: "review",
		Verdict: "APPROVE", Confidence: 95,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Errorf("expected ack OK for APPROVE without FileRefs, got %+v", ack)
	}

	select {
	case f := <-listener.Frames():
		if f.Verdict != "APPROVE" {
			t.Errorf("verdict=%s", f.Verdict)
		}
	case <-time.After(time.Second):
		t.Fatal("listener didn't receive APPROVE frame")
	}
}

func TestListenerSubmitInjectsFrame(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "v.sock")
	l, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	frame := Frame{RunID: "r1", Stage: "review", Verdict: "APPROVE", Confidence: 90}
	ack, err := l.Submit(frame)
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if !ack.OK {
		t.Errorf("ack.OK=false, want true; error=%q", ack.Error)
	}
	select {
	case f := <-l.Frames():
		if f.Verdict != "APPROVE" {
			t.Errorf("got verdict=%s", f.Verdict)
		}
	case <-time.After(time.Second):
		t.Fatal("no frame delivered")
	}
}

func TestListenerSubmitRejectsChangesWithoutFileRefs(t *testing.T) {
	sockPath := filepath.Join(t.TempDir(), "v.sock")
	l, err := Listen(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	ack, err := l.Submit(Frame{
		RunID: "r1", Stage: "review",
		Verdict: "CHANGES_REQUESTED", Confidence: 80,
	})
	if err == nil {
		t.Fatal("expected validation error for CHANGES_REQUESTED without FileRefs")
	}
	if ack == nil {
		t.Fatal("expected ack alongside error")
	}
	if ack.OK {
		t.Errorf("ack.OK=true, want false on rejection")
	}
	if ack.Error != AckErrFileRefsMissing {
		t.Errorf("ack.Error=%q, want %q", ack.Error, AckErrFileRefsMissing)
	}
	// Rejection MUST also be delivered on the Rejections() channel —
	// pins parity with the UDS handler at listener.go::handle.
	select {
	case rej := <-l.Rejections():
		if rej.Error != AckErrFileRefsMissing {
			t.Errorf("rejection on channel: Error=%q, want %q", rej.Error, AckErrFileRefsMissing)
		}
	case <-time.After(time.Second):
		t.Fatal("rejection not delivered on Rejections() channel")
	}
}

// TestFrameSummaryRoundTrip verifies that a Frame with a populated Summary
// field passes through the listener unchanged, and that an absent Summary
// produces an empty string (zero value) on the received Frame.
func TestFrameSummaryRoundTrip(t *testing.T) {
	t.Run("with_summary", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "verdict.sock")
		l, err := Listen(sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()

		want := "Error handling is inconsistent throughout the package; callers must check all returned errors."
		ack, err := Forward(sockPath, Frame{
			RunID: "r-sum", Stage: "review",
			Verdict: "APPROVE", Confidence: 88,
			Summary: want,
		})
		if err != nil {
			t.Fatal(err)
		}
		if !ack.OK {
			t.Fatalf("ack not OK: %+v", ack)
		}

		select {
		case f := <-l.Frames():
			if f.Summary != want {
				t.Errorf("Summary=%q want %q", f.Summary, want)
			}
		case <-time.After(time.Second):
			t.Fatal("no frame delivered")
		}
	})

	t.Run("absent_summary_is_empty", func(t *testing.T) {
		sockPath := filepath.Join(t.TempDir(), "verdict.sock")
		l, err := Listen(sockPath)
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()

		ack, err := Forward(sockPath, Frame{
			RunID: "r-nosub", Stage: "review",
			Verdict: "APPROVE", Confidence: 95,
			// Summary intentionally omitted
		})
		if err != nil {
			t.Fatal(err)
		}
		if !ack.OK {
			t.Fatalf("ack not OK: %+v", ack)
		}

		select {
		case f := <-l.Frames():
			if f.Summary != "" {
				t.Errorf("expected empty Summary, got %q", f.Summary)
			}
		case <-time.After(time.Second):
			t.Fatal("no frame delivered")
		}
	})
}
