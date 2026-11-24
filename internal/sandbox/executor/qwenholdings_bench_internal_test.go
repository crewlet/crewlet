package executor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAQwenReadingAtItsBounds measures a Qwen Code holdings reading's
// time and memory at its bounds, which set internal/sandbox's holdingsReadWait
// and [HoldingsReadsAtOnce]: 255 MiB of the least record a reading reads past
// and of real `ui_telemetry`, one 255 MiB compaction, 255 MiB of 50 KB tool
// results (0.28.4's largest record measured), one output's line of 32 and 64
// MiB and one just within [maxLineBytes], one step of 4,096 calls, and 200
// classes over 4,096 parts. `_pair` runs the reading and a person's read of the
// archive at once, `_person` the person's alone. Run one case at a time, reading
// GODEBUG=gctrace=1 between its stderr lines: held at most is the largest live
// heap at GOGC=10.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAQwenReadingAtItsBounds$/^<case>$' -benchtime 1x -benchmem
func BenchmarkAQwenReadingAtItsBounds(b *testing.B) {
	for _, bc := range []struct {
		name            string
		write           func(*testing.B, *qbChain) []sandbox.LoggedCall
		reading, person bool
	}{
		{"least_records", qbLeast, true, false},
		{"telemetry", qbTelemetry, true, false},
		{"compaction", qbCompaction, true, false},
		{"answers_50k", qbAnswers, true, false},
		{"output_32", qbOutput(32 << 20), true, false},
		{"output_64", qbOutput(64 << 20), true, false},
		{"output_64_person", qbOutput(64 << 20), false, true},
		{"output_64_pair", qbOutput(64 << 20), true, true},
		{"output_within_person", qbOutput(maxLineBytes - 128<<10), false, true},
		{"output_within_pair", qbOutput(maxLineBytes - 128<<10), true, true},
		{"step_4096", qbStep, true, false},
		{"classes", qbClasses, true, false},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "session.tar.gz")
			calls := qbArchiveFile(b, file, bc.write)
			ask := sandbox.HoldingsAsk{Calls: calls, Resumer: "0.28.4"}
			open := func(context.Context) (io.ReadCloser, error) { return benchOpen(file) }
			// THE LIVE HEAP BEFORE THE READ, the benchmark's own, which the
			// reading's held at most is read above.
			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			fmt.Fprintf(os.Stderr, "qwen-reading-bench %s: the read starts, %d bytes live\n", bc.name,
				before.HeapAlloc)
			b.ResetTimer()
			for range b.N {
				var wg sync.WaitGroup
				if bc.reading {
					wg.Go(func() {
						got, err := NewQwenCode().ReadHoldings(b.Context(), qhArchive("0.28.4"), open, ask)
						if err != nil || !got.Read {
							b.Errorf("the reading = %+v, %v", got, err)
						}
					})
				}
				if bc.person {
					wg.Go(func() {
						if _, err := ReadTranscriptAs(b.Context(), ReadByPerson, TranscriptFilter{}, QwenCodeName,
							qwenArchiveFormat, open, 0); err != nil {
							b.Errorf("the person's read: %v", err)
						}
					})
				}
				wg.Wait()
			}
			b.StopTimer()
			fmt.Fprintf(os.Stderr, "qwen-reading-bench %s: the read ends\n", bc.name)
		})
	}
}

// qbChain writes a chat file of [qhSession] a record at a time, each on the
// chain after the one before it, holding none.
type qbChain struct {
	w     io.Writer
	n     int
	bytes int
	// fill is the bytes a case writes its many records to: [qbFill], less in
	// a test of the case.
	fill int
}

// add writes the next record, rest its members after the eight.
func (c *qbChain) add(typ, provenance string, rest ...string) {
	line := qhRecordOpen(c.n, typ, provenance)
	_, _ = io.WriteString(c.w, line)
	c.bytes += len(line) + 2
	for _, r := range rest {
		_, _ = io.WriteString(c.w, r)
		c.bytes += len(r)
	}
	_, _ = io.WriteString(c.w, "}\n")
	c.n++
}

// least writes the next record as the least a reading reads past, [qhLeast].
func (c *qbChain) least() {
	line := qhLeast(c.n) + "\n"
	_, _ = io.WriteString(c.w, line)
	c.bytes += len(line)
	c.n++
}

// brief writes the run's prompt.
func (c *qbChain) brief() {
	c.add("user", "real_user", `,"executionContext":{},"message":{"role":"user","parts":[{"text":"the brief"}]},`+
		`"promptId":"p"`)
}

// step writes a model step making calls ([qhCallPart]).
func (c *qbChain) step(calls ...string) {
	c.add("assistant", "assistant_output", `,"model":"m","message":{"role":"model","parts":[`, strings.Join(calls, ","),
		`]},"usageMetadata":{}`)
}

// done writes the answer to call id of tool, its output text, quoted.
func (c *qbChain) done(id, tool, quoted string) {
	c.add("tool_result", "tool_result", `,"message":{"role":"user","parts":[{"functionResponse":{"id":"`+id+
		`","name":"mcp__crewlet__`+tool+`","response":{"output":`, quoted, `}}}]},"toolCallResult":{"callId":"`+id+
		`","status":"success","resultDisplay":`, quoted, `,"executionStatus":"success"}`)
}

// qbArchiveFile writes the chat file write makes, then its archive, a gzipped
// tar of it, to file, and answers the calls it asks about.
func qbArchiveFile(b *testing.B, file string, write func(*testing.B, *qbChain) []sandbox.LoggedCall) []sandbox.LoggedCall {
	b.Helper()
	chat := file + ".jsonl"
	cf, err := os.Create(chat)
	if err != nil {
		b.Fatal(err)
	}
	bw := bufio.NewWriterSize(cf, 1<<16)
	calls := write(b, &qbChain{w: bw, fill: qbFill})
	if err = bw.Flush(); err != nil {
		b.Fatal(err)
	}
	size, err := cf.Seek(0, io.SeekCurrent)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = cf.Seek(0, io.SeekStart); err != nil {
		b.Fatal(err)
	}
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	if err = tw.WriteHeader(&tar.Header{Name: "projects/-work-proj/chats/" + qhSession + ".jsonl",
		Typeflag: tar.TypeReg, Mode: 0o600, Size: size}); err != nil {
		b.Fatal(err)
	}
	if _, err = io.Copy(tw, cf); err != nil {
		b.Fatal(err)
	}
	for _, c := range []io.Closer{tw, zw, f, cf} {
		if err = c.Close(); err != nil {
			b.Fatal(err)
		}
	}
	if err = os.Remove(chat); err != nil {
		b.Fatal(err)
	}
	return calls
}

// qbFill is 255 MiB, what a file at the 256 MiB plain bound leaves its
// records past the run's own few.
const qbFill = 255 << 20

// qbTail writes the run's one call of echo and its answer, and answers the
// call.
func qbTail(c *qbChain) []sandbox.LoggedCall {
	c.step(qhCallPart("call_1", "echo", `{"t":1}`))
	c.done("call_1", "echo", `"done"`)
	return []sandbox.LoggedCall{ohCall(1, "echo", `{"t":1}`, "done")}
}

// qbLeast is 255 MiB of the least record a reading reads past ([qhLeast]),
// a call after them.
func qbLeast(_ *testing.B, c *qbChain) []sandbox.LoggedCall {
	c.brief()
	for c.bytes < c.fill {
		c.least()
	}
	return qbTail(c)
}

// qbTelemetry is 255 MiB of the round's own `ui_telemetry` records, a call
// after them.
func qbTelemetry(b *testing.B, c *qbChain) []sandbox.LoggedCall {
	b.Helper()
	var payloads []string
	for line := range strings.Lines(qhFixtures(b)["round"].chat) {
		var r struct {
			Subtype string          `json:"subtype"`
			Payload json.RawMessage `json:"systemPayload"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			b.Fatal(err)
		}
		if r.Subtype == "ui_telemetry" {
			payloads = append(payloads, `,"subtype":"ui_telemetry","systemPayload":`+string(r.Payload))
		}
	}
	c.brief()
	for i := 0; c.bytes < c.fill; i++ {
		c.add("system", "system", payloads[i%len(payloads)])
	}
	return qbTail(c)
}

// qbCompaction is one compaction whose history holds 255 MiB of text, a call
// after it.
func qbCompaction(_ *testing.B, c *qbChain) []sandbox.LoggedCall {
	run := strings.Repeat("a compressed history's words. ", (64<<10)/30)
	pieces := []string{`,"subtype":"chat_compression","systemPayload":{"compressedHistory":[{"role":"user",` +
		`"parts":[{"text":"`}
	for n := 0; n < c.fill; n += len(run) {
		pieces = append(pieces, run)
	}
	c.add("system", "system", append(pieces, `"}]}]}`)...)
	c.brief()
	return qbTail(c)
}

// qbAnswers is 255 MiB of steps each answered with 25,001 characters, the
// largest output 0.28.4 hands verbatim, its record 50 KB with its display's
// copy; the first 200 asked about.
func qbAnswers(_ *testing.B, c *qbChain) []sandbox.LoggedCall {
	c.brief()
	answer := strings.Repeat("x", 25_001)
	quoted := ohQuote(answer)
	var calls []sandbox.LoggedCall
	for i := 1; c.bytes < c.fill; i++ {
		id, args := "call_"+strconv.Itoa(i), `{"n":`+strconv.Itoa(i)+`}`
		c.step(qhCallPart(id, "echo", args))
		c.done(id, "echo", quoted)
		if i <= sandbox.MaxBridgeCalls {
			calls = append(calls, ohCut(ohCall(i, "echo", args, answer)))
		}
	}
	return calls
}

// qbOutput is one tool result of about size bytes, its output the reading
// digests and the log cut.
func qbOutput(size int) func(*testing.B, *qbChain) []sandbox.LoggedCall {
	return func(_ *testing.B, c *qbChain) []sandbox.LoggedCall {
		text := strings.Repeat("line of the log the tool printed, plain words. ", size/2/47)
		c.brief()
		c.step(qhCallPart("call_1", "echo", `{"t":1}`))
		c.done("call_1", "echo", ohQuote(text))
		// CLONED, so the ask's cut output keeps no slice of the text live
		// through the read, which would be the benchmark's, not the reading's.
		call := ohCut(ohCall(1, "echo", `{"t":1}`, text))
		call.Output = strings.Clone(call.Output)
		return []sandbox.LoggedCall{call}
	}
}

// qbStep is one step of [qwenStepCallsMax] calls, each answered, the first
// 200 asked about.
func qbStep(_ *testing.B, c *qbChain) []sandbox.LoggedCall {
	c.brief()
	parts := make([]string, qwenStepCallsMax)
	for i := range parts {
		parts[i] = qhCallPart(qbCallID(i), "echo", `{"n":`+strconv.Itoa(i)+`}`)
	}
	c.step(parts...)
	var calls []sandbox.LoggedCall
	for i := range parts {
		c.done(qbCallID(i), "echo", `"answer `+strconv.Itoa(i)+`"`)
		if i < sandbox.MaxBridgeCalls {
			calls = append(calls, ohCall(i+1, "echo", `{"n":`+strconv.Itoa(i)+`}`, "answer "+strconv.Itoa(i)))
		}
	}
	return calls
}

// qbCallID is a call id at the longest a reading reads, distinct by i.
func qbCallID(i int) string {
	id := "call_" + strconv.Itoa(i) + "_"
	return id + strings.Repeat("x", qwenCallIDMost-len(id))
}

// qbClasses is 200 classes, each of one call, over 4,096 parts in steps of
// 16, every class's parts more than its calls.
func qbClasses(_ *testing.B, c *qbChain) []sandbox.LoggedCall {
	c.brief()
	var calls []sandbox.LoggedCall
	for i := range sandbox.MaxBridgeCalls {
		calls = append(calls, ohCall(i+1, "echo", `{"c":`+strconv.Itoa(i)+`}`, "class "+strconv.Itoa(i)))
	}
	for step := range 4096 / 16 {
		parts := make([]string, 16)
		for i := range parts {
			class := (step*16 + i) % sandbox.MaxBridgeCalls
			parts[i] = qhCallPart("call_"+strconv.Itoa(i), "echo", `{"c":`+strconv.Itoa(class)+`}`)
		}
		c.step(parts...)
		for i := range parts {
			class := (step*16 + i) % sandbox.MaxBridgeCalls
			c.done("call_"+strconv.Itoa(i), "echo", `"class `+strconv.Itoa(class)+`"`)
		}
	}
	return calls
}

// THE QWEN BENCHMARK'S LEAST RECORDS ARE THE LEAST RECORD a reading reads
// past ([qhLeast], 266 bytes), so the most it measures held is the reading's:
// every record between the brief and the run's call is that record, and the
// reading reads the file.
//
// Mutation: write them with [qbChain.add]'s eight members, and this goes red.
func TestTheQwenLeastRecordsCaseWritesTheLeastRecord(t *testing.T) {
	t.Parallel()
	var file strings.Builder
	calls := qbLeast(nil, &qbChain{w: &file, fill: 64 << 10})
	lines := strings.Split(strings.TrimSuffix(file.String(), "\n"), "\n")
	fill := lines[1 : len(lines)-2]
	if len(fill) < (64<<10)/len(qhLeast(1))-2 {
		t.Fatalf("the case wrote %d records to 64 KiB; want the least record's worth", len(fill))
	}
	for i, line := range fill {
		if want := qhLeast(i + 1); line != want {
			t.Fatalf("record %d is %d bytes, %s; want the least record, %d bytes", i+2, len(line), line, len(want))
		}
	}
	h, err := qhRead(t, file.String(), ohBegan, calls...)
	if got := ohHeld(t, h, err, calls...); !slices.Equal(got, []int{1}) {
		t.Errorf("the case's file: held %v; want its call", got)
	}
}
