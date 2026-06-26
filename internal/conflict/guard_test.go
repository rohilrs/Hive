package conflict

import (
	"sort"
	"sync"
	"testing"
)

func TestCheckAndReserveNoConflict(t *testing.T) {
	g := NewGuard()
	dec := g.CheckAndReserve("r1", []string{"a.go", "b.go"})
	if !dec.Proceed {
		t.Errorf("expected Proceed=true, got %+v", dec)
	}
	if len(dec.WaitingOn) != 0 {
		t.Errorf("expected empty WaitingOn, got %v", dec.WaitingOn)
	}
}

func TestCheckAndReserveBlocksOnOverlap(t *testing.T) {
	g := NewGuard()
	_ = g.CheckAndReserve("r1", []string{"a.go", "b.go"})

	dec := g.CheckAndReserve("r2", []string{"b.go", "c.go"})
	if dec.Proceed {
		t.Errorf("expected Proceed=false")
	}
	if len(dec.WaitingOn) != 1 || dec.WaitingOn[0] != "r1" {
		t.Errorf("expected WaitingOn=[r1], got %v", dec.WaitingOn)
	}
}

func TestCheckAndReserveListsAllBlockers(t *testing.T) {
	g := NewGuard()
	_ = g.CheckAndReserve("r1", []string{"a.go"})
	_ = g.CheckAndReserve("r2", []string{"b.go"})

	dec := g.CheckAndReserve("r3", []string{"a.go", "b.go"})
	if dec.Proceed {
		t.Errorf("expected Proceed=false")
	}
	sort.Strings(dec.WaitingOn)
	if len(dec.WaitingOn) != 2 || dec.WaitingOn[0] != "r1" || dec.WaitingOn[1] != "r2" {
		t.Errorf("expected WaitingOn=[r1 r2], got %v", dec.WaitingOn)
	}
}

func TestReleaseFreesReservation(t *testing.T) {
	g := NewGuard()
	_ = g.CheckAndReserve("r1", []string{"a.go"})

	g.Release("r1")

	dec := g.CheckAndReserve("r2", []string{"a.go"})
	if !dec.Proceed {
		t.Errorf("expected Proceed=true after Release, got %+v", dec)
	}
}

func TestReleaseUnknownRunIsNoop(t *testing.T) {
	g := NewGuard()
	g.Release("does-not-exist") // should not panic
}

func TestCheckAndReserveNoFilesProceeds(t *testing.T) {
	// A run with no predicted files can never conflict; should always proceed.
	g := NewGuard()
	_ = g.CheckAndReserve("r1", []string{"a.go"})
	dec := g.CheckAndReserve("r2", nil)
	if !dec.Proceed {
		t.Errorf("expected Proceed=true for no-files run, got %+v", dec)
	}
}

func TestConcurrentCheckAndReserve(t *testing.T) {
	g := NewGuard()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			runID := "r" + string(rune('A'+i%26))
			g.CheckAndReserve(runID, []string{"shared.go"})
		}(i)
	}
	wg.Wait()
	// Test passes if -race doesn't trip.
}
