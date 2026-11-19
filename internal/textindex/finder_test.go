package textindex_test

import (
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/textindex"
)

// finderHeldBytes is what a Finder of two terms allocates, from its construction to the end of any text:
// its struct, the two terms' index and its record of them seen. Measured at 672 bytes under both readings,
// three runs, one case a process, on Go 1.27.2 (linux/amd64): a map's layout is the runtime's.
const finderHeldBytes = 672

// A FINDER HOLDS ITS TERMS AND NOTHING IT READS: over 16 MiB handed over in writes of 32 KiB, words short
// and past MaxTermLength, escapes, characters past ASCII and a run marked and rewound every MiB, a Finder
// of two terms the text never holds allocates finderHeldBytes from its construction, and nothing while it
// reads, under either reading: what it carries along a text is its own fixed state. Measured as the bytes
// the package's code allocates (allocatedByThePackage), which a held word cannot pass under.
//
// Mutation: hold a word whole until it ends, or a held escape on the heap, and this fails naming the reading.
func TestAFinderHoldsItsTermsAndNothingItReads(t *testing.T) {
	// Not parallel: a parallel case's allocations would be counted as this one's.
	backslash := `\`
	pattern := "deploy the pipeline " + strings.Repeat("w", 3*textindex.MaxTermLength) + " café " +
		backslash + "n" + backslash + "u00e9" + backslash + "ud83d" + backslash + "ude00ab " + backslash + "q "
	chunk := []byte(strings.Repeat(pattern, (32<<10)/len(pattern)))
	const total = 16 << 20
	for _, r := range textindex.Readings {
		var f *textindex.Finder
		built := allocatedByThePackage(func() { f = r.Finder([]string{"absent", "nowhere"}) })
		read := allocatedByThePackage(func() {
			for written, i := 0, 0; written < total; written, i = written+len(chunk), i+1 {
				if i%32 == 0 {
					f.Mark()
				}
				_, _ = f.Write(chunk)
				if i%32 == 31 {
					f.Rewind()
				}
			}
			if first, all := f.Found(); first != -1 || all {
				t.Errorf("%s reading: the finder finds (%d, %v) of terms the text never holds", r, first, all)
			}
		})
		t.Logf("%s reading: allocated %d bytes made, %d reading %d bytes", r, built, read, total)
		if built > finderHeldBytes || read != 0 {
			t.Fatalf("%s reading: the finder allocated %d bytes made and %d reading, past the %d it states", r,
				built, read, finderHeldBytes)
		}
	}
}

// allocatedByThePackage is the bytes do allocates with the package's code on the stack, from a memory
// profile of every allocation; runtime.MemStats would also count the runtime's own meanwhile.
func allocatedByThePackage(do func()) uint64 {
	defer func(rate int) { runtime.MemProfileRate = rate }(runtime.MemProfileRate)
	runtime.MemProfileRate = 1
	before := profiledInThePackage()
	do()
	return profiledInThePackage() - before
}

// profiledInThePackage is the bytes the memory profile records allocated under a textindex frame, after a
// GC publishes every allocation made before it.
func profiledInThePackage() uint64 {
	runtime.GC()
	pkg := reflect.TypeFor[textindex.Finder]().PkgPath() + "."
	var records []runtime.MemProfileRecord
	n, ok := runtime.MemProfile(nil, true)
	for !ok {
		records = make([]runtime.MemProfileRecord, n+64)
		n, ok = runtime.MemProfile(records, true)
	}
	var total uint64
	for _, rec := range records[:n] {
		frames := runtime.CallersFrames(rec.Stack())
		for {
			fr, more := frames.Next()
			if strings.HasPrefix(fr.Function, pkg) {
				total += uint64(rec.AllocBytes)
				break
			}
			if !more {
				break
			}
		}
	}
	return total
}
