package executor

import (
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
)

// ownAllocations is what f allocates, heap objects and bytes, counted from a
// memory profile of every allocation made with [allocatingIn]'s frame on the
// stack: f's own, on its goroutine. The runtime's counts ([runtime.MemStats])
// are the whole process's, so another goroutine's allocations land in them:
// a package's leftover goroutines, a linked package's own (#2160). f must
// allocate on its own goroutine; one it starts allocates outside the count.
func ownAllocations(f func()) (objects, bytes uint64) {
	ownMeasuring.Lock()
	defer ownMeasuring.Unlock()
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	objectsBefore, bytesBefore := profiledIn()
	allocatingIn(f)
	objectsAfter, bytesAfter := profiledIn()
	return objectsAfter - objectsBefore, bytesAfter - bytesBefore
}

// ownMeasuring serializes [ownAllocations]' writes of runtime.MemProfileRate,
// which cases measuring at once would otherwise race on.
var ownMeasuring sync.Mutex

// allocatingIn is the frame [ownAllocations] counts under.
//
//go:noinline
func allocatingIn(f func()) { f() }

// profiledIn is what the memory profile records allocated under
// [allocatingIn], after two collections publish every allocation made before
// them (the profile may be up to two cycles old, runtime.MemProfile).
func profiledIn() (objects, bytes uint64) {
	runtime.GC()
	runtime.GC()
	marker := runtime.FuncForPC(reflect.ValueOf(allocatingIn).Pointer()).Name()
	var records []runtime.MemProfileRecord
	n, ok := runtime.MemProfile(nil, true)
	for !ok {
		records = make([]runtime.MemProfileRecord, n+64)
		n, ok = runtime.MemProfile(records, true)
	}
	for _, r := range records[:n] {
		frames := runtime.CallersFrames(r.Stack())
		for {
			f, more := frames.Next()
			if f.Function == marker {
				objects += uint64(r.AllocObjects)
				bytes += uint64(r.AllocBytes)
				break
			}
			if !more {
				break
			}
		}
	}
	return objects, bytes
}

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
