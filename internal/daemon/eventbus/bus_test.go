package eventbus

import (
	"sync"
	"testing"
	"time"

	"github.com/rohilrs/Hive/pkg/rpc"
)

func TestBusPublishesToSubscriber(t *testing.T) {
	b := New(Config{BufferSize: 4})

	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(rpc.EventMessage{Type: rpc.EventRunStarted, Data: map[string]any{"run_id": "r1"}})

	select {
	case ev := <-ch:
		if ev.Type != rpc.EventRunStarted {
			t.Errorf("got type=%v want run.started", ev.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestBusFansOutToMultipleSubscribers(t *testing.T) {
	b := New(Config{BufferSize: 4})

	chA, cancelA := b.Subscribe()
	defer cancelA()
	chB, cancelB := b.Subscribe()
	defer cancelB()

	b.Publish(rpc.EventMessage{Type: rpc.EventRunStarted})

	for i, ch := range []<-chan rpc.EventMessage{chA, chB} {
		select {
		case ev := <-ch:
			if ev.Type != rpc.EventRunStarted {
				t.Errorf("sub %d got type=%v", i, ev.Type)
			}
		case <-time.After(time.Second):
			t.Fatalf("sub %d timeout", i)
		}
	}
}

func TestBusOverflowEmitsResync(t *testing.T) {
	b := New(Config{BufferSize: 2, ResyncOnOverflow: true})

	ch, cancel := b.Subscribe()
	defer cancel()

	for i := 0; i < 5; i++ {
		b.Publish(rpc.EventMessage{Type: rpc.EventStageStarted, Data: map[string]any{"i": i}})
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	var sawResync bool
loop:
	for time.Now().Before(deadline) {
		select {
		case ev := <-ch:
			if ev.Type == rpc.EventResync {
				sawResync = true
				break loop
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !sawResync {
		t.Errorf("never saw resync event after overflow")
	}
}

func TestBusCancelStopsDelivery(t *testing.T) {
	b := New(Config{BufferSize: 4})

	ch, cancel := b.Subscribe()
	cancel()

	b.Publish(rpc.EventMessage{Type: rpc.EventRunStarted})

	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("ch not closed after cancel")
		}
	case <-time.After(200 * time.Millisecond):
		t.Errorf("read on cancelled ch blocked")
	}
}

func TestBusPublishConcurrent(t *testing.T) {
	b := New(Config{BufferSize: 1024})

	ch, cancel := b.Subscribe()
	defer cancel()

	const N = 100
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			b.Publish(rpc.EventMessage{Type: rpc.EventStageEnded, Data: map[string]any{"i": i}})
		}(i)
	}
	wg.Wait()

	got := 0
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		select {
		case <-ch:
			got++
		default:
		}
	}
	if got != N {
		t.Errorf("received %d events; want %d", got, N)
	}
}

func TestBusPublishNeverBlocks(t *testing.T) {
	b := New(Config{BufferSize: 1, ResyncOnOverflow: false})
	_, cancel := b.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			b.Publish(rpc.EventMessage{Type: rpc.EventStageStarted})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked")
	}
}
