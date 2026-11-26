package executor

import (
	"sync/atomic"
	"testing"
)

// noiseSink keeps each of [allocateMeanwhile]'s allocations on the heap.
var noiseSink atomic.Pointer[[]byte]

// allocateMeanwhile starts a goroutine that allocates four KiB at a time until
// the case ends, as a package's leftover goroutines or a linked package's own
// do while a case measures, and answers how many it has allocated.
func allocateMeanwhile(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b := make([]byte, 4<<10)
			noiseSink.Store(&b)
			n.Add(1)
		}
	}()
	t.Cleanup(func() {
		close(stop)
		<-done
	})
	return &n
}

// A READER'S COST IS ITS OWN ALLOCATIONS: what another goroutine allocates
// while a line is read, a package's leftover goroutines or a linked package's
// own (crewlet/crewlet#2160, where linking nats-server's server package into
// this package's binary put some 20 KiB in every reading of a 64 MiB line),
// counts against no reader. Here a goroutine allocates 4 KiB at a time beside
// a 16 MiB prompt's reading, and the reading still costs at most 16 KiB more
// than its short twin's, as TestALineNoReaderReadsCostsItsReaderNothing holds.
//
// Mutation: count the process's allocations ([runtime.MemStats]) — the
// goroutine's land in the reading.
func TestAReadersCostIsItsOwnAllocations(t *testing.T) {
	ctx := t.Context()
	line := func(n int) []scanPart {
		return scanLine(scanLit(`{"type":"user","message":{"role":"user","content":"`), scanRun('x', n),
			scanLit(`"},"uuid":"`+ctUUID(1)+`","timestamp":"`+ctStamp+`"}`))
	}
	twin := line(1)
	twinCost, _, twinErr := scanAllocated(ctx, twin, twin, 3)
	noise := allocateMeanwhile(t)
	cost, _, err := scanAllocated(ctx, twin, line(16<<20), 2)
	if twinErr != nil || err != nil {
		t.Fatalf("the line reads as %v, its twin as %v; want both records", err, twinErr)
	}
	if noise.Load() == 0 {
		t.Fatal("the premise: the other goroutine allocated nothing while the line was read")
	}
	if cost > twinCost+16<<10 {
		t.Errorf("a 16 MiB prompt read beside another goroutine's allocations costs its reader %d bytes; want at "+
			"most 16 KiB more than its twin, %d bytes: the other goroutine's count against it", cost, twinCost)
	}
}
