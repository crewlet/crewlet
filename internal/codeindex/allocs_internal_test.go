package codeindex

import (
	"reflect"
	"runtime"
	"testing"
)

// allocations is the bytes and heap objects (a shared 16-byte tiny block as one) f allocates, from a memory
// profile of every allocation under allocatedBy's frame; runtime.MemStats would also count the runtime's own.
// Every call measured here allocates, so a reading of none fails rather than passing every bound.
func allocations(t testing.TB, f func()) (bytes, objects uint64) {
	t.Helper()
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	bytesBefore, objectsBefore := profiledUnderAllocatedBy()
	allocatedBy(f)
	bytesAfter, objectsAfter := profiledUnderAllocatedBy()
	if bytesAfter == bytesBefore {
		t.Fatal("the memory profile saw nothing allocated under allocatedBy: it missed the measured call")
	}
	return bytesAfter - bytesBefore, objectsAfter - objectsBefore
}

// allocatedBy is the frame allocations counts under. Not the package's frames: a test's own sit under the GCs.
//
//go:noinline
func allocatedBy(f func()) { f() }

// profiledUnderAllocatedBy is what the memory profile records allocated under allocatedBy, after a GC
// publishes every allocation made before it.
func profiledUnderAllocatedBy() (bytes, objects uint64) {
	runtime.GC()
	marker := runtime.FuncForPC(reflect.ValueOf(allocatedBy).Pointer()).Name()
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
				bytes += uint64(r.AllocBytes)
				objects += uint64(r.AllocObjects)
				break
			}
			if !more {
				break
			}
		}
	}
	return bytes, objects
}
