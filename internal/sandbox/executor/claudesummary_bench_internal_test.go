package executor

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAClaudeSummaryLineAtItsBound measures the walk's read of a Claude
// Code summary's line ([claudeSummaryReading]) at its bounds, off a real direct box's
// file: a line of claudeSummaryLineMaxBytes whose content is line breaks
// written escaped, its text just within summaryReadMaxBytes (escaped, the
// worst), and a line of plain words at summaryReadMaxBytes (plain). Each case
// prints when the read starts and ends, for its live heap to be read off
// GODEBUG=gctrace=1 at GOGC=10, one case a process:
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAClaudeSummaryLineAtItsBound$/^<case>$' -benchtime 1x
func BenchmarkAClaudeSummaryLineAtItsBound(b *testing.B) {
	for _, bc := range []struct {
		name    string
		content func(room int) string
	}{
		{name: "escaped", content: func(room int) string { return strings.Repeat("\n", room/2) }},
		{name: "plain", content: func(int) string { return strings.Repeat("w", summaryReadMaxBytes) }},
	} {
		b.Run(bc.name, func(b *testing.B) {
			local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: b.TempDir()})
			if err != nil {
				b.Fatal(err)
			}
			box, err := local.Create(b.Context(), sandbox.Spec{})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = box.Close(b.Context()) })
			file := box.Home() + "/thread.jsonl"
			want := summaryBenchWrite(b, box, file, bc.content)
			runtime.GC()
			fmt.Fprintf(os.Stderr, "summary-line-bench %s: the read starts\n", bc.name)
			b.ResetTimer()
			for range b.N {
				start := time.Now()
				body, err := box.OpenFile(b.Context(), file)
				if err != nil {
					b.Fatal(err)
				}
				r := &claudeSummaryReading{}
				walkThread(b.Context(), body, threadPlainMax, new(passBudget), r)
				_ = body.Close()
				got := r.reading()
				elapsed := time.Since(start) //fixedwait:measure the read's time is what the case reports
				fmt.Fprintf(os.Stderr, "summary-line-bench %s: the read ends in %s, %d bytes of text\n", bc.name,
					elapsed, len(r.text.buf))
				if got.Unread != "" || len(r.text.buf) != want {
					b.Fatalf("the read held %d bytes (%s), want %d", len(r.text.buf), got.Unread, want)
				}
			}
			b.StopTimer()
		})
	}
}

// summaryBenchWrite writes file, a boundary and a summary's line whose content
// is what content makes of the room claudeSummaryLineMaxBytes leaves it, and
// is its text's length; the line is let go of on return, before the read.
func summaryBenchWrite(b *testing.B, box sandbox.Sandbox, file string, content func(room int) string) int {
	b.Helper()
	boundary := `{"type":"system","subtype":"compact_boundary"}` + "\n"
	room := claudeSummaryLineMaxBytes - len(summaryLineOf(`{"content":""}`)) - 1
	words := content(room)
	line := summaryLineOf(`{"content":`+boundedQuoted(words)+`}`) + "\n"
	if len(line) > claudeSummaryLineMaxBytes || len(words) > summaryReadMaxBytes {
		b.Fatalf("the line is %d bytes holding %d, past its bounds", len(line), len(words))
	}
	if err := box.WriteFile(b.Context(), file, []byte(boundary+line)); err != nil {
		b.Fatal(err)
	}
	return len(words)
}
