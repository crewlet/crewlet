package executor

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAnOpenCodeReadingAtItsBounds measures an OpenCode holdings
// reading's time and memory at its bounds, which set internal/sandbox's
// holdingsReadWait and [HoldingsReadsAtOnce]: the largest export a release's
// own runs kept, 255 MiB of ordinary steps, of idle messages and of one step's
// shortest items, one message holding one answer the reading digests at 32 and
// at 64 MiB, one of minimal items at 64 MiB, and 200 classes over 4,096 parts.
// `_pair` runs the reading and a person's read of the export at once,
// `_person` the person's alone. Run one case at a time, reading
// GODEBUG=gctrace=1 between its stderr lines: held at most is the largest live
// heap at GOGC=10.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAnOpenCodeReadingAtItsBounds$/^<case>$' -benchtime 1x -benchmem
func BenchmarkAnOpenCodeReadingAtItsBounds(b *testing.B) {
	for _, bc := range []struct {
		name            string
		write           func(*testing.B, io.Writer) []sandbox.LoggedCall
		reading, person bool
		// refused is a reading that reads the export to its end and then
		// answers it is no export.
		refused bool
	}{
		{"measured", benchMeasured, true, false, false},
		{"ordinary", benchOrdinary, true, false, false},
		{"idle", benchIdle, true, false, false},
		{"shortest_items", benchShortestItems, true, false, true},
		{"answer_32", benchAnswer(32 << 20), true, false, false},
		{"answer_64", benchAnswer(64 << 20), true, false, false},
		{"answer_64_person", benchAnswer(64 << 20), false, true, false},
		{"answer_64_pair", benchAnswer(64 << 20), true, true, false},
		{"minimal_items", benchMinimalItems, true, false, false},
		{"classes", benchClasses, true, false, false},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "export.json.gz")
			calls := benchExportFile(b, file, bc.write)
			ask := sandbox.HoldingsAsk{Calls: calls, Resumer: "2.2.7"}
			open := func(context.Context) (io.ReadCloser, error) { return benchOpen(file) }
			fmt.Fprintf(os.Stderr, "oc-reading-bench %s: the read starts\n", bc.name)
			b.ResetTimer()
			for range b.N {
				var wg sync.WaitGroup
				if bc.reading {
					wg.Go(func() {
						got, err := NewOpenCode().ReadHoldings(b.Context(), ohArchive("2.2.7"), open, ask)
						if bc.refused && !errors.Is(err, errOpenCodeMalformed) || !bc.refused && (err != nil || !got.Read) {
							b.Errorf("the reading = %+v, %v", got, err)
						}
					})
				}
				if bc.person {
					wg.Go(func() {
						if _, err := ReadTranscriptAs(b.Context(), ReadByPerson, TranscriptFilter{}, OpenCodeName,
							openCodeArchiveFormat, open, 0); err != nil {
							b.Errorf("the person's read: %v", err)
						}
					})
				}
				wg.Wait()
			}
			b.StopTimer()
			fmt.Fprintf(os.Stderr, "oc-reading-bench %s: the read ends\n", bc.name)
		})
	}
}

// benchOpen opens the export file buffered, as an object store's body reads.
func benchOpen(file string) (io.ReadCloser, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	return struct {
		io.Reader
		io.Closer
	}{bufio.NewReader(f), f}, nil
}

// benchExportFile writes the export write makes to file, gzipped through a
// buffer so the writer holds little, and answers the calls it asks about.
func benchExportFile(b *testing.B, file string, write func(*testing.B, io.Writer) []sandbox.LoggedCall) []sandbox.LoggedCall {
	b.Helper()
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	w := bufio.NewWriterSize(zw, 1<<16)
	calls := write(b, w)
	if err = w.Flush(); err != nil {
		b.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		b.Fatal(err)
	}
	if err = f.Close(); err != nil {
		b.Fatal(err)
	}
	return calls
}

// benchHead and benchTail open and close an export of [ohSession].
const (
	benchHead = `{"info":{"id":"` + ohSession + `","title":"a run"},"messages":[` + ohUser
	benchTail = `]}`
)

// benchMeasured is the largest export a release's own runs kept (its
// truncation case), with the calls its round logged, its session renamed
// [ohSession].
func benchMeasured(b *testing.B, w io.Writer) []sandbox.LoggedCall {
	b.Helper()
	f := ohFixtures(b, "2.2.7")["truncation"]
	zr, err := gzip.NewReader(bytes.NewReader(f.archive))
	if err != nil {
		b.Fatal(err)
	}
	doc, err := io.ReadAll(zr)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = w.Write(bytes.Replace(doc, []byte(f.session), []byte(ohSession), 1))
	return f.calls
}

// benchOrdinary is about 255 MiB of ordinary steps, each a call answered with
// a kibibyte, the first 200 asked about.
func benchOrdinary(b *testing.B, w io.Writer) []sandbox.LoggedCall {
	b.Helper()
	_, _ = io.WriteString(w, benchHead)
	answer := strings.Repeat("an ordinary answer line. ", 40)
	var calls []sandbox.LoggedCall
	for i, n := 1, 0; n < 255<<20; i++ {
		args := `{"n":` + strconv.Itoa(i) + `}`
		step := "," + ohStep(ohDone("echo", args, answer))
		_, _ = io.WriteString(w, step)
		n += len(step)
		if i <= sandbox.MaxBridgeCalls {
			calls = append(calls, ohCall(i, "echo", args, answer))
		}
	}
	_, _ = io.WriteString(w, benchTail)
	return calls
}

// benchIdle is about 255 MiB of `{"type":"idle"}` messages, the shortest a
// reading reads past, a step of one call after them.
func benchIdle(b *testing.B, w io.Writer) []sandbox.LoggedCall {
	b.Helper()
	_, _ = io.WriteString(w, benchHead)
	benchRepeat(w, `,{"type":"idle"}`)
	_, _ = io.WriteString(w, ","+ohStep(ohDone("echo", `{"t":1}`, "done"))+benchTail)
	return []sandbox.LoggedCall{ohCall(1, "echo", `{"t":1}`, "done")}
}

// benchShortestItems is one step of about 255 MiB of the shortest item a
// content list holds, `0`: the slowest export a reading was measured on
// (the empty object, the empty list, an answer's empty parts and a time's
// empty names each read faster), refused at the step's end.
func benchShortestItems(b *testing.B, w io.Writer) []sandbox.LoggedCall {
	b.Helper()
	_, _ = io.WriteString(w, benchHead+","+ohStepOpen)
	benchRepeat(w, `0,`)
	_, _ = io.WriteString(w, ohDone("echo", `{"t":1}`, "done")+ohStepClose+benchTail)
	return []sandbox.LoggedCall{ohCall(1, "echo", `{"t":1}`, "done")}
}

// benchRepeat writes element to w over and over, about 255 MiB of it.
func benchRepeat(w io.Writer, element string) {
	run := strings.Repeat(element, (64<<10)/len(element))
	for n := 0; n < 255<<20; n += len(run) {
		_, _ = io.WriteString(w, run)
	}
}

// benchAnswer is one step of size bytes holding one answer the reading
// digests, as large as the step leaves room for.
func benchAnswer(size int) func(*testing.B, io.Writer) []sandbox.LoggedCall {
	return func(b *testing.B, w io.Writer) []sandbox.LoggedCall {
		b.Helper()
		frame := ohStep(ohDone("echo", `{"t":1}`, ""))
		text := strings.Repeat("line of the log the tool printed, plain words. ", (size-len(frame))/47)
		_, _ = io.WriteString(w, benchHead+","+ohStep(ohDone("echo", `{"t":1}`, text))+benchTail)
		return []sandbox.LoggedCall{ohCut(ohCall(1, "echo", `{"t":1}`, text))}
	}
}

// benchMinimalItems is one step of 64 MiB of minimal items.
func benchMinimalItems(b *testing.B, w io.Writer) []sandbox.LoggedCall {
	b.Helper()
	const item = `{"type":"reasoning","text":""},`
	_, _ = io.WriteString(w, benchHead+","+ohStepOpen)
	for n := 0; n < 64<<20; n += len(item) {
		_, _ = io.WriteString(w, item)
	}
	_, _ = io.WriteString(w, ohDone("echo", `{"t":1}`, "done")+ohStepClose+benchTail)
	return []sandbox.LoggedCall{ohCall(1, "echo", `{"t":1}`, "done")}
}

// benchClasses is 200 classes, each of one call, over 4,096 parts in steps
// of 16, every class's parts more than its calls.
func benchClasses(b *testing.B, w io.Writer) []sandbox.LoggedCall {
	b.Helper()
	_, _ = io.WriteString(w, benchHead)
	var calls []sandbox.LoggedCall
	for i := range sandbox.MaxBridgeCalls {
		calls = append(calls, ohCall(i+1, "echo", `{"c":`+strconv.Itoa(i)+`}`, "class "+strconv.Itoa(i)))
	}
	for step := range 4096 / 16 {
		parts := make([]string, 16)
		for i := range parts {
			class := (step*16 + i) % sandbox.MaxBridgeCalls
			parts[i] = ohDone("echo", `{"c":`+strconv.Itoa(class)+`}`, "class "+strconv.Itoa(class))
		}
		_, _ = io.WriteString(w, ","+ohStep(parts...))
	}
	_, _ = io.WriteString(w, benchTail)
	return calls
}
