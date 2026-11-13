package executor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// WHAT A READING COSTS AT ITS BOUNDS, in time and in memory: the figures the
// reading's wait ([holdingsReadWait]) and the seats' transcript slot it takes
// ([SeatTranscriptReadsAtOnce]) are stated by. Each case writes its archive to
// a file first, gzipped as the take's scrub writes one, and reads it from
// there, so what a case holds is the reading's own and never the archive's.
// Its peak heap is read off the runtime's GC trace (GODEBUG=gctrace=1), one
// case a process, after the forced collection each case takes once its archive
// is written: a sampling ticker would be a clock in test code. Each case
// reports the plaintext it reads and the archive it read it from, in MiB, its
// lines' text made of random base64, which gzip barely shrinks, and of one
// phrase, which it shrinks to nothing, in the measure each case's inflation
// asks.
//
//	go test -run '^$' -bench '^BenchmarkAReadingAtItsBounds$/^<case>$' -benchtime 1x -benchmem
//
// The cases:
//
//   - an ordinary session: probe P6's median kept session, 1.51 MiB sealed,
//     of lines of the 2,053 bytes a real main thread's average, inflating by
//     the 3.7 a session at the archive's bound does (32 MiB to about 118);
//   - a session at the archive's bound, so: about 118 MiB of such lines;
//   - lines to the plaintext's bound, [threadPlainMax], inflating by the 8 a
//     session must to reach it from the archive's bound;
//   - [holdingsMaxAgents] subagents, each with its thread and metadata and its
//     answer in the main thread, [sandbox.MaxBridgeCalls] of their calls asked;
//   - lines at the line's bound, [threadLineMax], each a user record of text,
//     to the plaintext's bound;
//   - one line at the line's bound of the smallest results a message holds, all
//     of which [threadLine.Message] keeps, and one of the smallest blocks of a
//     kind no reader names, each of which it keeps a kind of.
func BenchmarkAReadingAtItsBounds(b *testing.B) {
	const (
		line     = 2053           // a real main thread's average line, probe P6
		measured = 118.0 / 32     // what a session at the archive's bound inflates by, probe P6
		bound    = scrubExpansion // what one must inflate by to reach the plaintext's bound
	)
	ordinary := int64(math.Round(1.51 * measured * (1 << 20)))
	atBound := int64(math.Round(measured * executorkeep.SessionMaxBytes))
	for _, bc := range []struct {
		name  string
		write func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk
	}{
		{"an_ordinary_session", benchLines(ordinary, line, measured)},
		{"a_session_at_the_archives_bound", benchLines(atBound, line, measured)},
		{"lines_to_the_plaintexts_bound", benchLines(threadPlainMax-benchSlack, line, bound)},
		{"subagents_at_their_bound", benchSubagents},
		{"lines_at_the_lines_bound", benchLines(threadPlainMax-benchSlack, threadLineMax, bound)},
		{"a_line_of_the_smallest_results", benchBlocks(`{"type":"tool_result","tool_use_id":"a"}`)},
		{"a_line_of_unknown_blocks", benchBlocks(`{"type":"x"}`)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "archive.tar.gz")
			ask, plain, sealed := benchArchive(b, file, bc.write)
			archive := sandbox.SessionArchive{Harness: ClaudeCodeName, SessionID: ctSession,
				Format: claudeArchiveFormat, Version: holdingsWriterFloor}
			// WHAT WRITING THE ARCHIVE LEFT is collected before the reading
			// starts, so the trace's heaps after it are the reading's own.
			runtime.GC()
			b.ResetTimer()
			// REPORTED ONCE THE TIMER IS RESET, which deletes every metric
			// reported before it.
			b.ReportMetric(float64(plain)/(1<<20), "plain-MiB")
			b.ReportMetric(float64(sealed)/(1<<20), "sealed-MiB")
			for range b.N {
				f, err := os.Open(file)
				if err != nil {
					b.Fatal(err)
				}
				got, err := ClaudeCode{}.readHoldings(b.Context(), bufio.NewReader(f), archive, ask,
					boundsFor(executorkeep.SessionMaxBytes))
				_ = f.Close()
				if err != nil || !got.Read {
					b.Fatalf("a reading = %+v, %v; want one", got.Read, err)
				}
			}
		})
	}
}

// benchSlack is what a case written to the plaintext's bound leaves of it for
// its tar's own blocks: its members' headers and the zeros that end it.
const benchSlack = 1 << 20

// benchArchive writes the archive write makes to file, gzipped, and answers
// what it asks and the archive's size in plaintext and sealed.
func benchArchive(b *testing.B, file string,
	write func(*testing.B, *tar.Writer) sandbox.HoldingsAsk) (ask sandbox.HoldingsAsk, plain, sealed int64) {

	b.Helper()
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	sealedCount := &counted{w: f}
	zw := gzip.NewWriter(sealedCount)
	plainCount := &counted{w: zw}
	tw := tar.NewWriter(plainCount)
	ask = write(b, tw)
	for _, err := range []error{tw.Close(), zw.Close(), f.Close()} {
		if err != nil {
			b.Fatal(err)
		}
	}
	return ask, plainCount.n, sealedCount.n
}

// benchMember writes one member named name of body to tw.
func benchMember(b *testing.B, tw *tar.Writer, name, body string) {
	b.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)),
		Typeflag: tar.TypeReg}); err != nil {
		b.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		b.Fatal(err)
	}
}

// benchFill is text a line holds: random base64, which no gzip window
// repeats, and one phrase repeated, which gzip shrinks to nearly nothing.
type benchFill struct {
	pool string
	next int
}

// newBenchFill is fill, the same every run.
func newBenchFill() *benchFill {
	raw := make([]byte, 6<<20)
	_, _ = rand.NewChaCha8([32]byte{'b', 'e', 'n', 'c', 'h'}).Read(raw)
	return &benchFill{pool: base64.StdEncoding.EncodeToString(raw)}
}

// text is n bytes of fill in a line of line bytes, so much of it random that
// the line inflates from its gzip by about ratio: deflate keeps about three
// quarters of random base64, six bits of each eight, and next to nothing of
// the rest.
func (f *benchFill) text(n, line int, ratio float64) string {
	random := min(int(float64(line)/(0.75*ratio)), n, len(f.pool))
	if f.next+random > len(f.pool) {
		f.next = 0
	}
	noise := f.pool[f.next : f.next+random]
	f.next += random
	const phrase = "the harness's own words, as a tool answered them and the model read them. "
	rest := strings.Repeat(phrase, (n-random)/len(phrase)+1)[:n-random]
	return noise + rest
}

// benchLines writes a main thread of about plain bytes of lines of n bytes
// each, its break included, each inflating from its gzip by about ratio: a
// model's call and its result in turn, or, where n is the line's bound, user
// records of text. The first [sandbox.MaxBridgeCalls] calls are asked.
func benchLines(plain int64, n int, ratio float64) func(*testing.B, *tar.Writer) sandbox.HoldingsAsk {
	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		fill := newBenchFill()
		lines := plain / int64(n)
		if err := tw.WriteHeader(&tar.Header{Name: hsMain, Mode: 0o600, Size: lines * int64(n),
			Typeflag: tar.TypeReg}); err != nil {
			b.Fatal(err)
		}
		var ask sandbox.HoldingsAsk
		for i := range lines {
			id := fmt.Sprintf("toolu_bench_%08d", i/2)
			var record string
			switch {
			case n == threadLineMax:
				head := `{"parentUuid":"` + ctUUID(int(i)+1) + `","isSidechain":false,"type":"user","message":` +
					`{"role":"user","content":"`
				tail := `"},"uuid":"` + ctUUID(int(i)+2) + `"}`
				record = head + fill.text(n-1-len(head)-len(tail), n, ratio) + tail
			case i%2 == 0:
				head := `{"parentUuid":"` + ctUUID(int(i)+1) + `","isSidechain":false,"message":{"id":"msg_bench",` +
					`"type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use",` +
					`"id":"` + id + `","name":"mcp__crewlet__slow","input":{"note":"`
				tail := `"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":100,` +
					`"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":20}},` +
					`"type":"assistant","uuid":"` + ctUUID(int(i)+2) + `"}`
				record = head + fill.text(n-1-len(head)-len(tail), n, ratio) + tail
				if len(ask.Calls) < sandbox.MaxBridgeCalls {
					ask.Calls = append(ask.Calls, sandbox.LoggedCall{ID: id, Asked: true})
				}
			default:
				head := `{"parentUuid":"` + ctUUID(int(i)+1) + `","isSidechain":false,"type":"user","message":` +
					`{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":` +
					`[{"type":"text","text":"`
				tail := `"}]}]},"uuid":"` + ctUUID(int(i)+2) + `"}`
				record = head + fill.text(n-1-len(head)-len(tail), n, ratio) + tail
			}
			if _, err := tw.Write([]byte(record + "\n")); err != nil {
				b.Fatal(err)
			}
		}
		if len(ask.Calls) == 0 {
			ask.Calls = []sandbox.LoggedCall{{ID: "toolu_bench_asked", Asked: true}}
		}
		return ask
	}
}

// benchSubagents writes a session of [holdingsMaxAgents] subagents, each
// started by an Agent call of the main thread and answered by its result, its
// thread a prompt, one call and its result and a finished reply, and its
// metadata; the first [sandbox.MaxBridgeCalls] subagents' calls are asked,
// each answered as its thread holds it.
func benchSubagents(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
	var main strings.Builder
	main.WriteString(ctBrief(ctUUID(1)) + "\n")
	var ask sandbox.HoldingsAsk
	for i := range holdingsMaxAgents {
		agent, started, call := fmt.Sprintf("a%016x", i), fmt.Sprintf("toolu_agent_%04d", i),
			fmt.Sprintf("toolu_sub_%04d", i)
		output := "sub " + call + " done"
		main.WriteString(ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(started, claudeToolAgent, hsAgentInput)) + "\n")
		main.WriteString(ctResult(ctUUID(2), ctUUID(3), `[{"tool_use_id":"`+started+`","type":"tool_result",`+
			`"content":[{"type":"text","text":"the subagent's report"}]}]`, hsCompleted(agent)) + "\n")
		side := func(record string) string {
			return strings.Replace(record, `"isSidechain":false`, `"isSidechain":true,"agentId":"`+agent+`"`, 1)
		}
		thread := strings.ReplaceAll(ctSubagentPrompt(ctUUID(101)), ctAgent, agent) + "\n" +
			side(ctAssistant(ctUUID(101), ctUUID(102), hsUse(call))) + "\n" +
			side(ctResult(ctUUID(102), ctUUID(103), `[{"tool_use_id":"`+call+`","type":"tool_result","content":`+
				`[{"type":"text","text":"`+output+`"}]}]`, `""`)) + "\n" +
			side(strings.Replace(ctAssistant(ctUUID(103), ctUUID(104), `[{"type":"text","text":"done."}]`),
				`"stop_reason":"tool_use"`, `"stop_reason":"end_turn"`, 1)) + "\n"
		prefix := claudeProjectPrefix + ctSession + "/subagents/agent-" + agent
		benchMember(b, tw, prefix+".jsonl", thread)
		benchMember(b, tw, prefix+".meta.json", strings.Replace(ctMeta, hsStarted, started, 1))
		if len(ask.Calls) < sandbox.MaxBridgeCalls {
			ask.Calls = append(ask.Calls, sandbox.LoggedCall{ID: call, Asked: true, Output: output})
		}
	}
	benchMember(b, tw, hsMain, main.String())
	return ask
}

// benchBlocks writes a main thread of one user record of blocks at the line's
// bound, each block, a JSON object, as many times as fit.
func benchBlocks(block string) func(*testing.B, *tar.Writer) sandbox.HoldingsAsk {
	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		head := `{"parentUuid":"` + ctUUID(1) + `","isSidechain":false,"type":"user","message":{"role":"user",` +
			`"content":[`
		tail := `]},"uuid":"` + ctUUID(2) + `"}`
		blocks := (threadLineMax - 1 - len(head) - len(tail) + 1) / (len(block) + 1)
		record := head + strings.Repeat(block+",", blocks-1) + block + tail
		benchMember(b, tw, hsMain, ctBrief(ctUUID(1))+"\n"+record+"\n")
		return sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{{ID: "toolu_bench_asked", Asked: true}}}
	}
}
