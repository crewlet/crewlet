package executor

import (
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"strings"
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
// Not for a reader that uses sync.Pool at a scale near its bound: the two
// collections before each reading empty the pools, which under -race also drop
// a quarter of what is put back, so refills land at random (see
// [allocationsOf]); the line reader's json v2 decoder refill is a few hundred
// bytes against its 16 KiB. [ownAllocationsWithin] counts such a reader's part.
func ownAllocations(f func()) (objects, bytes uint64) { return ownAllocationsWithin(f) }

// ownAllocationsWithin is [ownAllocations] of what f allocates within the
// functions within names, each a prefix of a function's name a frame below
// f's must open with: a part of a reading, counted alone where the rest draws
// on pools whose refills land at random, which the part does not.
func ownAllocationsWithin(f func(), within ...string) (objects, bytes uint64) {
	ownMeasuring.Lock()
	defer ownMeasuring.Unlock()
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	objectsBefore, bytesBefore := profiledIn(within)
	allocatingIn(f)
	objectsAfter, bytesAfter := profiledIn(within)
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
// [allocatingIn], and within a function within names where it names any,
// after two collections publish every allocation made before them (the
// profile may be up to two cycles old, runtime.MemProfile).
func profiledIn(within []string) (objects, bytes uint64) {
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
		in := len(within) == 0
		for {
			f, more := frames.Next()
			if f.Function == marker {
				if in {
					objects += uint64(r.AllocObjects)
					bytes += uint64(r.AllocBytes)
				}
				break
			}
			in = in || slices.ContainsFunc(within, func(name string) bool { return strings.HasPrefix(f.Function, name) })
			if !more {
				break
			}
		}
	}
	return objects, bytes
}

// methodsOf is the name every method of m's receiver type is compiled under
// up to the method's own, closures inside them included
// ([ownAllocationsWithin]): a rename moves it with the type.
func methodsOf(m any) string {
	name := runtime.FuncForPC(reflect.ValueOf(m).Pointer()).Name()
	return name[:strings.LastIndex(name, ".")+1]
}

// functionOf is the name f is compiled under, closures inside it included
// ([ownAllocationsWithin]).
func functionOf(f any) string { return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name() }

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

// requireNoStrays fails t, naming each by its stack, when a goroutine runs
// beside the caller that is not the test binary's own: a measurement read off
// the process's counts ([allocationsOf], [heldBy], [allocated]) would take its
// allocations for the measured reader's, as #2160's took a linked package's
// timer for a reader's. It runs before each reading; a reader's own goroutines
// start after it.
func requireNoStrays(t testing.TB) {
	t.Helper()
	if strays := strayGoroutines(); len(strays) > 0 {
		t.Fatalf("%d goroutine(s) run beside this measurement, which reads the process's allocation counts "+
			"and would count theirs as the reader's; end them before measuring:\n\n%s", len(strays),
			strings.Join(strays, "\n\n"))
	}
}

// strayGoroutines is the stack of every goroutine but the caller's that is
// not parked in the testing package (its main, a case waiting on a subtest,
// a parallel case waiting its turn), from an all-goroutine dump: the
// runtime's own are not in it. runtime.NumGoroutine alone counts the parked
// cases too, which a package's parallel cases make any number.
func strayGoroutines() []string {
	buf := make([]byte, 1<<16)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	var strays []string
	for i, g := range strings.Split(strings.TrimSpace(string(buf)), "\n\n") {
		if i > 0 && !parkedInTesting(g) {
			strays = append(strays, g)
		}
	}
	return strays
}

// parkedInTesting reports whether a goroutine's dump has the testing package
// as its innermost frame outside the runtime: the binary's own, waiting.
func parkedInTesting(g string) bool {
	for line := range strings.Lines(g) {
		if strings.HasPrefix(line, "goroutine ") || strings.HasPrefix(line, "\t") {
			continue
		}
		if strings.HasPrefix(line, "runtime.") || strings.HasPrefix(line, "sync.") ||
			strings.HasPrefix(line, "internal/") {
			continue
		}
		return strings.HasPrefix(line, "testing.")
	}
	return false
}

// strayWitness is a testing.TB whose Fatalf keeps what it said and ends the
// call it fails by panicking it out, so a case can see a measurement refuse.
type strayWitness struct {
	testing.TB
	said string
}

func (w *strayWitness) Helper() {}

func (w *strayWitness) Fatalf(format string, args ...any) {
	w.said = fmt.Sprintf(format, args...)
	panic(w)
}

// refusal is what measure said failing w, or nothing where it did not fail it.
func (w *strayWitness) refusal(measure func(testing.TB)) (said string) {
	defer func() {
		if r := recover(); r != nil && r != w {
			panic(r)
		}
		said = w.said
	}()
	measure(w)
	return ""
}

// A MEASUREMENT REFUSES A GOROUTINE BESIDE IT: allocationsOf, heldBy and
// imagepass's allocated read the process's allocation counts, where another
// goroutine's allocations land as the measured reader's, so each fails its
// case naming any goroutine but the test binary's own, by its stack, rather
// than hand back a skewed figure. A goroutine the case leaves running is one.
//
// Mutation: drop requireNoStrays from allocationsOf — the measurement goes on.
func TestAMeasurementRefusesAGoroutineBesideIt(t *testing.T) {
	stop, ended := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ended)
		<-stop
	}()
	for name, measure := range map[string]func(testing.TB){
		"allocationsOf": func(tb testing.TB) { allocationsOf(tb, func() {}) },
		"heldBy":        func(tb testing.TB) { heldBy(tb, func() any { return nil }) },
		"allocated": func(tb testing.TB) {
			_, _ = allocated(tb, func() error { return nil })
		},
	} {
		said := (&strayWitness{TB: t}).refusal(measure)
		if !strings.Contains(said, "TestAMeasurementRefusesAGoroutineBesideIt.func1") {
			t.Errorf("%s beside a goroutine the case left running said %q; want it refused, naming the "+
				"goroutine by its stack", name, said)
		}
	}
	close(stop)
	<-ended
}
