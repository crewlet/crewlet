package executor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAGeminiReadingAtItsBounds measures a Gemini CLI holdings reading's
// time and memory at its bounds, which set internal/sandbox's
// holdingsReadWait and [HoldingsReadsAtOnce]: one `$patch` line of about 255
// MiB of the shortest part a content list holds, about 255 MiB of `$set`
// lines, 2^14 ids with the run's records, calls and answers at 2^13, one
// answer the reading digests at 32 and at 64 MiB, and 200 classes over 4,096
// parts. `_pair` runs the reading and a person's read of the file at once,
// `_person` the person's alone. Run one case at a time, reading
// GODEBUG=gctrace=1 between its stderr lines: held at most is the largest live
// heap at GOGC=10.
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAGeminiReadingAtItsBounds$/^<case>$' -benchtime 1x -benchmem
func BenchmarkAGeminiReadingAtItsBounds(b *testing.B) {
	for _, bc := range []struct {
		name            string
		write           func(io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart)
		reading, person bool
	}{
		{"patch", benchGeminiPatch, true, false},
		{"set", benchGeminiSet, true, false},
		{"ids", benchGeminiIDs, true, false},
		{"answer_32", benchGeminiAnswer(32 << 20), true, false},
		{"answer_64", benchGeminiAnswer(64 << 20), true, false},
		{"answer_64_person", benchGeminiAnswer(64 << 20), false, true},
		{"answer_64_pair", benchGeminiAnswer(64 << 20), true, true},
		{"classes", benchGeminiClasses, true, false},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "session.tar.gz")
			calls, start := benchGeminiFile(b, file, bc.write)
			archive := ghArchive("0.70.0")
			archive.RunStart = start
			ask := sandbox.HoldingsAsk{Calls: calls, Resumer: "0.70.0"}
			open := func(context.Context) (io.ReadCloser, error) { return benchOpen(file) }
			// THE WRITE'S GARBAGE COLLECTED FIRST, so the heap the trace shows
			// between the lines is the read's.
			runtime.GC()
			fmt.Fprintf(os.Stderr, "gemini-reading-bench %s: the read starts\n", bc.name)
			b.ResetTimer()
			for range b.N {
				var wg sync.WaitGroup
				if bc.reading {
					wg.Go(func() {
						if got, err := NewGeminiCLI().ReadHoldings(b.Context(), archive, open, ask); err != nil ||
							!got.Read {
							b.Errorf("the reading = %+v, %v", got.Read, err)
						}
					})
				}
				if bc.person {
					wg.Go(func() {
						if _, err := ReadTranscriptAs(b.Context(), ReadByPerson, TranscriptFilter{}, GeminiCLIName,
							geminiArchiveFormat, open, 0); err != nil {
							b.Errorf("the person's read: %v", err)
						}
					})
				}
				wg.Wait()
			}
			b.StopTimer()
			fmt.Fprintf(os.Stderr, "gemini-reading-bench %s: the read ends\n", bc.name)
		})
	}
}

// benchGeminiFile writes the chat file write makes to file as the run's
// script packs it, a gzipped tar of it alone, and answers the calls it asks
// about and the run's start.
func benchGeminiFile(b *testing.B, file string, write func(io.Writer) ([]sandbox.LoggedCall,
	sandbox.RunStart)) ([]sandbox.LoggedCall, sandbox.RunStart) {

	b.Helper()
	chat, err := os.Create(file + ".chat")
	if err != nil {
		b.Fatal(err)
	}
	w := bufio.NewWriterSize(chat, 1<<16)
	_, _ = io.WriteString(w, ghHeader+"\n")
	calls, start := write(w)
	if err = w.Flush(); err != nil {
		b.Fatal(err)
	}
	size, err := chat.Seek(0, io.SeekCurrent)
	if err != nil {
		b.Fatal(err)
	}
	if _, err = chat.Seek(0, io.SeekStart); err != nil {
		b.Fatal(err)
	}
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	tw := tar.NewWriter(zw)
	if err = tw.WriteHeader(&tar.Header{Name: ghMember(ghSession), Typeflag: tar.TypeReg, Mode: 0o600,
		Size: size}); err != nil {
		b.Fatal(err)
	}
	if _, err = io.Copy(tw, chat); err != nil {
		b.Fatal(err)
	}
	for _, c := range []io.Closer{tw, zw, f, chat} {
		if err = c.Close(); err != nil {
			b.Fatal(err)
		}
	}
	if err = os.Remove(file + ".chat"); err != nil {
		b.Fatal(err)
	}
	return calls, start
}

// benchGeminiRound writes a round of one call answered "done" and answers
// the call.
func benchGeminiRound(w io.Writer) []sandbox.LoggedCall {
	_, _ = io.WriteString(w, ghReply("r", ghCallOf("c", "echo", `{"t":1}`))+"\n"+
		ghAnswers("u", ghAnswerOf("c", "echo", "done"))+"\n")
	return []sandbox.LoggedCall{ohCall(1, "echo", `{"t":1}`, "done")}
}

// benchGeminiRepeat writes element to w over and over, about 255 MiB of it.
func benchGeminiRepeat(w io.Writer, element string) {
	run := strings.Repeat(element, (64<<10)/len(element))
	for n := 0; n < 255<<20; n += len(run) {
		_, _ = io.WriteString(w, run)
	}
}

// benchGeminiPatch is a round, then one `$patch` line restating a person's
// record with about 255 MiB of `{}` parts, the shortest a content list holds.
func benchGeminiPatch(w io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart) {
	calls := benchGeminiRound(w)
	_, _ = io.WriteString(w, `{"id":"p","type":"user","content":"x"}`+"\n"+
		`{"$patch":{"updates":[{"id":"p","content":[`)
	benchGeminiRepeat(w, `{},`)
	_, _ = io.WriteString(w, "{}]}]}}\n")
	return calls, ghBegan
}

// benchGeminiSet is about 255 MiB of `$set` lines, then a round.
func benchGeminiSet(w io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart) {
	benchGeminiRepeat(w, `{"$set":{"lastUpdated":"2026-11-23T03:07:18.330Z"}}`+"\n")
	return benchGeminiRound(w), ghBegan
}

// benchGeminiIDs is 2^14 ids: 2^13 records before the run's start, then
// 2^13 of the run's, the last two a reply of 2^13 calls and a record of 2^13
// answers, the first 200 calls asked about.
func benchGeminiIDs(w io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart) {
	for i := range geminiHoldingsRunMost {
		_, _ = io.WriteString(w, `{"id":"before-`+strconv.Itoa(i)+`","type":"user","content":"x"}`+"\n")
	}
	for i := range geminiHoldingsRunMost - 2 {
		_, _ = io.WriteString(w, `{"id":"run-`+strconv.Itoa(i)+`","type":"user","content":"x"}`+"\n")
	}
	parts, answers := make([]string, geminiHoldingsRunMost), make([]string, geminiHoldingsRunMost)
	var calls []sandbox.LoggedCall
	for i := range parts {
		id, args := "mcp_crewlet_echo__mcp_crewlet_echo_1795403238388_"+strconv.Itoa(i), `{"n":`+strconv.Itoa(i)+`}`
		parts[i] = ghCallOf(id, "echo", args)
		answers[i] = ghAnswerOf(id, "echo", "answer "+strconv.Itoa(i))
		if i < sandbox.MaxBridgeCalls {
			calls = append(calls, ohCall(i+1, "echo", args, "answer "+strconv.Itoa(i)))
		}
	}
	_, _ = io.WriteString(w, ghReply("r", parts...)+"\n"+ghAnswers("u", answers...)+"\n")
	return calls, sandbox.RunStart{Lines: int64(geminiHoldingsRunMost + 1)}
}

// benchGeminiAnswer is a round whose one answer is about size bytes.
func benchGeminiAnswer(size int) func(io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart) {
	return func(w io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart) {
		text := strings.Repeat("line of the log the tool printed, plain words. ", size/47)
		_, _ = io.WriteString(w, ghReply("r", ghCallOf("c", "echo", `{"t":1}`))+"\n"+
			ghAnswers("u", ghAnswerOf("c", "echo", text))+"\n")
		return []sandbox.LoggedCall{ohCut(ohCall(1, "echo", `{"t":1}`, text))}, ghBegan
	}
}

// benchGeminiClasses is 200 classes, each of one call, over 4,096 parts in
// rounds of 16, every class's parts more than its calls.
func benchGeminiClasses(w io.Writer) ([]sandbox.LoggedCall, sandbox.RunStart) {
	var calls []sandbox.LoggedCall
	for i := range sandbox.MaxBridgeCalls {
		calls = append(calls, ohCall(i+1, "echo", `{"c":`+strconv.Itoa(i)+`}`, "class "+strconv.Itoa(i)))
	}
	for round := range 4096 / 16 {
		parts, answers := make([]string, 16), make([]string, 16)
		for i := range parts {
			class, id := (round*16+i)%sandbox.MaxBridgeCalls, "c"+strconv.Itoa(round*16+i)
			parts[i] = ghCallOf(id, "echo", `{"c":`+strconv.Itoa(class)+`}`)
			answers[i] = ghAnswerOf(id, "echo", "class "+strconv.Itoa(class))
		}
		r := strconv.Itoa(round)
		_, _ = io.WriteString(w, ghReply("r"+r, parts...)+"\n"+ghAnswers("u"+r, answers...)+"\n")
	}
	return calls, ghBegan
}
