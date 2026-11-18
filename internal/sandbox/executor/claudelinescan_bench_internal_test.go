package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

// BenchmarkAReaderAtALineOfAnyLength measures one line on a fresh
// [threadReader], generated as it is read ([scanGenerated]), so the read holds
// only the reader's own: its time, and held-KiB, what is live once Next
// returned, the reader and the line kept, less what was live before. A line no
// reader reads holds the reader's fixed buffers and its few facts, at most
// half a mebibyte whatever its length; the line at the facts bound holds its
// facts besides, within a quarter over [threadLineMax] for the heap's size
// classes. Run one case a process, under GODEBUG=gctrace=1 for the heaps:
//
//	GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAReaderAtALineOfAnyLength$/^<case>$' -benchtime 3x -benchmem
func BenchmarkAReaderAtALineOfAnyLength(b *testing.B) {
	const fixed = 512 << 10
	pdf := strings.Split(benchPDFLine(ctUUID(1), ctUUID(2), "toolu_1", ctStamp, "@"), "@")
	head := `{"parentUuid":"` + ctUUID(1) + `","isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":`
	tail := `},"uuid":"` + ctUUID(2) + `","timestamp":"` + ctStamp + `","version":"2.1.310"}`
	smallest := benchFactsHeld(head, benchSmallest, tail)
	for _, bc := range []struct {
		name string
		line []scanPart
		// most is the most the reader and its line may hold; calls, the calls
		// the line hands its reader.
		most, calls int
	}{
		{"a_whole_pdf_line", scanLine(scanLit(pdf[0]), scanRun('A', benchPDF), scanLit(pdf[1]),
			scanRun('A', benchPDF), scanLit(pdf[2])), fixed, 1},
		{"a_prompt_of_64_mib", scanLine(scanLit(head+`"`), scanRun('x', 64<<20), scanLit(`"`+tail)), fixed, 0},
		{"the_smallest_results_at_the_facts_bound", scanLine(scanLit(head), scanList(benchSmallest, smallest),
			scanLit(tail)), threadLineMax*5/4 + fixed, smallest},
	} {
		b.Run(bc.name, func(b *testing.B) {
			b.SetBytes(scanLength(bc.line))
			fmt.Fprintf(os.Stderr, "reader-bench %s: the read starts\n", bc.name)
			held := int64(0)
			for range b.N {
				var before, after runtime.MemStats
				b.StopTimer()
				runtime.GC()
				runtime.ReadMemStats(&before)
				b.StartTimer()
				r := newThreadReader(&scanGenerated{parts: bc.line}, threadPlainMax)
				line, err := r.Next(b.Context())
				b.StopTimer()
				m, merr := line.Message()
				if err != nil || merr != nil || len(m.Calls()) != bc.calls {
					b.Fatalf("the line reads as %v, %v, handing %d calls; want a record of %d", err, merr,
						len(m.Calls()), bc.calls)
				}
				runtime.GC()
				runtime.ReadMemStats(&after)
				held = max(held, int64(after.HeapAlloc)-int64(before.HeapAlloc))
				runtime.KeepAlive(r)
				runtime.KeepAlive(line)
				b.StartTimer()
			}
			b.StopTimer()
			fmt.Fprintf(os.Stderr, "reader-bench %s: the read ends\n", bc.name)
			b.ReportMetric(float64(held)/(1<<10), "held-KiB")
			b.ReportMetric(float64(bc.calls), "calls")
			if held > int64(bc.most) {
				b.Fatalf("the reader and its line hold %d bytes; want at most %d", held, bc.most)
			}
		})
	}
}

// benchFactsHeld is how many copies of block a line of head, a list of them,
// and tail holds within [threadLineMax], found by reading it: that many read,
// and one more is refused, its facts past the bound. Each block must charge
// the meter ([lineMeter]), so a call per byte charged is never held.
func benchFactsHeld(head, block, tail string) int {
	line := func(n int) []scanPart { return scanLine(scanLit(head), scanList(block, n), scanLit(tail)) }
	held, refused := 1, scanCallsHeld(1, 0)+1
	for refused-held > 1 {
		n := held + (refused-held)/2
		if _, err := scanRead(context.Background(), line(n)); errors.Is(err, errThreadBound) {
			refused = n
		} else {
			held = n
		}
	}
	return held
}
