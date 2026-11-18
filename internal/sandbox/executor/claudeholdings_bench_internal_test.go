package executor

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
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

// BenchmarkAReadingAtItsBounds measures a reading's time and memory at its
// bounds, which set internal/sandbox's holdingsReadWait and
// [SeatTranscriptReadsAtOnce]; the ordinary case is probe P6's median kept
// session, 1.51 MiB sealed. A case reads its gzipped archive from a file, so
// it holds only the reading's memory, and fails on any answer but its own;
// one at a bound of the resume's model ([threadChain], THE BOUNDS) reports
// either as its read metric. Run one case at a time, reading GODEBUG=gctrace=1
// between its stderr lines: held at most is the largest live heap at GOGC=10
// (a cycle every tenth of growth), the peak the largest start or end heap at
// the default GOGC. Times at the 32 MiB bounds overstate.
//
//	[GOGC=10] GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAReadingAtItsBounds$/^<case>$' -benchtime 1x -benchmem
func BenchmarkAReadingAtItsBounds(b *testing.B) {
	const (
		line     = 2053           // a real main thread's average line, probe P6
		measured = 118.0 / 32     // what a session at the archive's bound inflates by, probe P6
		bound    = scrubExpansion // what one must inflate by to reach the plaintext's bound
	)
	ordinary := int64(math.Round(1.51 * measured * (1 << 20)))
	atBound := int64(math.Round(measured * executorkeep.SessionMaxBytes))
	recordsCall := func(i int) string { return fmt.Sprintf("toolu_%08d", i+1) }
	callsCall := func(i int) string { return fmt.Sprintf("toolu_use_000_%04d", i) }
	for _, bc := range []struct {
		name  string
		write func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk
		// read is what the case's reading is: benchReads, a reading;
		// benchReadsEither, either, reported.
		read benchReading
		// held is how many of its asked calls the reading holds.
		held int
	}{
		{"an_ordinary_session", benchSessionLines(ordinary, line, measured, false), benchReads,
			sandbox.MaxBridgeCalls},
		{"a_session_at_the_archives_bound", benchSessionLines(atBound, line, measured, false), benchReads,
			sandbox.MaxBridgeCalls},
		{"lines_to_the_plaintexts_bound", benchSessionLines(threadPlainMax-benchSlack, line, bound, false), benchReads,
			sandbox.MaxBridgeCalls},
		{"subagents_at_their_bound", benchSubagents, benchReads, sandbox.MaxBridgeCalls},
		{"long_lines_to_the_plaintexts_bound", benchSessionLines(threadPlainMax-benchSlack,
			int((threadPlainMax-benchSlack)/4), bound, true), benchReads, 0},
		{"a_whole_pdf_read", benchWholePDFSession(measured), benchReads, 1},
		{"a_line_of_the_smallest_results", benchBlocks(benchSmallest, benchFactsHeld), benchReads, 0},
		{"a_line_of_blocks_to_the_plaintexts_bound", benchBlocksLine, benchReads, 0},
		{"a_subagents_lines_of_blocks_to_both_bounds", benchSubagentBlocks, benchReads, 0},
		{"legs_of_ids_at_the_value_bound", benchLongLegs, benchReads, 0},
		{"metadata_of_ids_most_of_their_bound", benchLongMetas, benchReads, 0},
		{"a_thread_at_its_records_bound", benchChainThread(benchAtTheBound, recordsCall), benchReadsEither, 0},
		{"a_thread_at_its_retraction_and_kept_bound", benchChainThread(benchRetractionsAndKept, nil),
			benchReadsEither, 0},
		{"a_thread_at_its_calls_bound", benchChainThread(benchCalls, callsCall), benchReadsEither, 0},
		{"the_smallest_results_beside_a_thread_at_its_bounds", benchChainThread(benchReadingLine, recordsCall),
			benchReadsEither, 0},
		{"the_smallest_results_at_both_of_a_threads_bounds", benchChainThread(benchReadingLineAtBoth, recordsCall),
			benchReadsEither, 0},
		{"tiny_lines_to_the_lines_bound", benchLinesToTheBound(`{"type":"x"}`), benchReads, 0},
	} {
		b.Run(bc.name, func(b *testing.B) {
			file := filepath.Join(b.TempDir(), "archive.tar.gz")
			ask, plain, gzipped := benchArchive(b, file, bc.write)
			archive := sandbox.SessionArchive{Harness: ClaudeCodeName, SessionID: ctSession,
				Format: claudeArchiveFormat, Version: holdingsWriterFloor}
			// AN ASK A READING TAKES, as a relaunch's continuing release sends it:
			// one refused before anything opens would measure nothing.
			ask.Resumer = hsResumer
			if err := (ClaudeCode{}).checkHoldings(archive, ask); err != nil {
				b.Fatalf("the case's ask is refused: %v", err)
			}
			// WHAT WRITING THE ARCHIVE LEFT is collected before the reading
			// starts, so the trace's heaps after it are the reading's own.
			runtime.GC()
			fmt.Fprintf(os.Stderr, "reading-bench %s: the read starts\n", bc.name)
			b.ResetTimer()
			// REPORTED AFTER THE RESET, which deletes every metric reported before.
			b.ReportMetric(float64(plain)/(1<<20), "plain-MiB")
			b.ReportMetric(float64(gzipped)/(1<<20), "archive-MiB")
			var got sandbox.SessionHoldings
			for range b.N {
				f, err := os.Open(file)
				if err != nil {
					b.Fatal(err)
				}
				got, err = ClaudeCode{}.readHoldings(b.Context(), bufio.NewReader(f), archive, ask,
					boundsFor(executorkeep.SessionMaxBytes))
				_ = f.Close()
				var own *holdingsError
				switch {
				case bc.read != benchReads && errors.As(err, &own):
					b.ReportMetric(0, "read")
				case err != nil || !got.Read:
					b.Fatalf("a reading = %+v, %v; want one", got.Read, err)
				default:
					b.ReportMetric(1, "read")
				}
			}
			b.StopTimer()
			fmt.Fprintf(os.Stderr, "reading-bench %s: the read ends\n", bc.name)
			// WHAT IT HOLDS, held to the case's count: holding less would measure
			// a cheaper reading than the case names.
			held, told := 0, ""
			for _, c := range ask.Calls {
				if ok, _ := got.Holds(c); ok {
					held++
				} else if told == "" {
					r := got.Calls[c.ID]
					told = fmt.Sprintf("%s, its records %+v and its thread's %+v", c.ID, r, got.Subagents[r.Thread])
				}
			}
			b.ReportMetric(float64(held), "held")
			if held != bc.held {
				b.Fatalf("the reading holds %d of the asked calls, telling first %s; want %d", held, told, bc.held)
			}
		})
	}
}

// benchReading is what a case's reading is: one, or either, reported as its
// read metric.
type benchReading int

const (
	benchReads benchReading = iota
	benchReadsEither
)

// benchSlack is what a case written to the plaintext's bound leaves of it for
// its tar's own blocks: its members' headers and the zeros that end it.
const benchSlack = 1 << 20

// benchArchive writes the archive write makes to file, gzipped, and answers
// what it asks and the archive's size in plaintext and gzipped.
func benchArchive(b *testing.B, file string,
	write func(*testing.B, *tar.Writer) sandbox.HoldingsAsk) (ask sandbox.HoldingsAsk, plain, gzipped int64) {

	b.Helper()
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	gzippedCount := &counted{w: f}
	zw := gzip.NewWriter(gzippedCount)
	plainCount := &counted{w: zw}
	tw := tar.NewWriter(plainCount)
	ask = write(b, tw)
	for _, err := range []error{tw.Close(), zw.Close(), f.Close()} {
		if err != nil {
			b.Fatal(err)
		}
	}
	return ask, plainCount.n, gzippedCount.n
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

// benchSessionLines writes a main thread of about plain bytes of lines of n
// bytes each, its break included, each inflating from its gzip by about ratio:
// a model's call and its result in turn, or, where prompts is set, a person's
// prompts. The first [sandbox.MaxBridgeCalls] calls are asked.
func benchSessionLines(plain int64, n int, ratio float64,
	prompts bool) func(*testing.B, *tar.Writer) sandbox.HoldingsAsk {

	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		fill := newBenchFill()
		lines := plain / int64(n)
		if err := tw.WriteHeader(&tar.Header{Name: hsMain, Mode: 0o600, Size: lines * int64(n),
			Typeflag: tar.TypeReg}); err != nil {
			b.Fatal(err)
		}
		var ask sandbox.HoldingsAsk
		for i := range lines {
			id := fmt.Sprintf("toolu_bench%08d", i/2)
			// EACH RECORD UNDER THE ONE BEFORE, each reply under an id of its own
			// and each record at a time of its own, as a real session writes
			// them: the resume starts from the newest dated record, and a thread
			// of no time holds nothing.
			parent := `"` + ctUUID(int(i)+1) + `"`
			if i == 0 {
				parent = "null"
			}
			own := `"uuid":"` + ctUUID(int(i)+2) + `","timestamp":"` + rcAt(int(i)) + `","version":"2.1.310"}`
			var record string
			switch {
			case prompts:
				head := `{"parentUuid":` + parent + `,"isSidechain":false,"type":"user","message":` +
					`{"role":"user","content":"`
				tail := `"},` + own
				record = head + fill.text(n-1-len(head)-len(tail), n, ratio) + tail
			case i%2 == 0:
				head := `{"parentUuid":` + parent + `,"isSidechain":false,"message":{"id":"` +
					fmt.Sprintf("msg_bench_%08d", i/2) + `","type":"message","role":"assistant",` +
					`"model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"` + id +
					`","name":"mcp__crewlet__slow","input":{"note":"`
				tail := `"}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":100,` +
					`"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":20}},` +
					`"type":"assistant",` + own
				record = head + fill.text(n-1-len(head)-len(tail), n, ratio) + tail
				if len(ask.Calls) < sandbox.MaxBridgeCalls {
					ask.Calls = append(ask.Calls, sandbox.LoggedCall{ID: id, Asked: true})
				}
			default:
				head := `{"parentUuid":` + parent + `,"isSidechain":false,"type":"user","message":` +
					`{"role":"user","content":[{"tool_use_id":"` + id + `","type":"tool_result","content":` +
					`[{"type":"text","text":"`
				tail := `"}]}]},` + own
				text := fill.text(n-1-len(head)-len(tail), n, ratio)
				record = head + text + tail
				// THE OUTPUT ITS LOG KEPT, the result's own text, so the
				// reading holds each asked call its session holds.
				if k := int(i / 2); k < len(ask.Calls) {
					ask.Calls[k].Output = text
				}
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
		// IDS OF NO BATCH'S SPLIT FORM, as a real call's are: a main thread
		// holding a result under an id that ends in `_` and digits answers
		// no leg ([splitBase]).
		agent, started, call := fmt.Sprintf("a%016x", i), fmt.Sprintf("toolu_agent%04d", i),
			fmt.Sprintf("toolu_sub%04d", i)
		output := "sub " + call + " done"
		// EACH RECORD ITS OWN UUID, the main thread one chain: a uuid two
		// records carry is no reading.
		parent, use, answer := ctUUID(1), ctUUID(1000+2*i), ctUUID(1001+2*i)
		if i > 0 {
			parent = ctUUID(999 + 2*i)
		}
		// EACH CALL'S REPLY UNDER AN ID OF ITS OWN, each record at a time of its
		// own, as a real session writes them.
		main.WriteString(benchAt(strings.Replace(ctAssistant(parent, use, hsCallOf(started, claudeToolAgent,
			hsAgentInput)), `"id":"msg_test"`, `"id":"msg_agent_`+fmt.Sprintf("%04d", i)+`"`, 1), 2*i+1) + "\n")
		main.WriteString(benchAt(ctResult(use, answer, `[{"tool_use_id":"`+started+`","type":"tool_result",`+
			`"content":[{"type":"text","text":"the subagent's report"}]}]`, hsCompleted(agent)), 2*i+2) + "\n")
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

// benchAt is record written at ms milliseconds after [rcBase] in place of
// [ctStamp].
func benchAt(record string, ms int) string {
	return strings.Replace(record, `"timestamp":"`+ctStamp+`"`, `"timestamp":"`+rcAt(ms)+`"`, 1)
}

// benchLongLegs writes a main thread of 96 Agent calls, each under an id as
// long as a value the layer reads whole ([threadValueMax]), which the call-id
// rule refuses, so the reading must keep none of them ([holdingsWalk.leg]).
func benchLongLegs(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
	var main strings.Builder
	main.WriteString(ctBrief(ctUUID(1)) + "\n")
	for i := range 96 {
		id := fmt.Sprintf("toolu_%02d_", i) + strings.Repeat("l", threadValueMax-len(`"toolu_00_"`))
		main.WriteString(ctAssistant(ctUUID(1+i), ctUUID(2+i), hsCallOf(id, claudeToolAgent, hsAgentInput)) + "\n")
	}
	benchMember(b, tw, hsMain, main.String())
	return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
}

// benchLongMetas writes [holdingsMaxAgents] subagents' metadata, each naming
// its starting call by an id of 65,000 bytes the call-id rule refuses, so the
// reading must keep none of them ([subagentMetaOf]).
func benchLongMetas(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
	long := strings.Repeat("t", 65000)
	for i := range holdingsMaxAgents {
		benchMember(b, tw, claudeProjectPrefix+ctSession+"/subagents/agent-"+fmt.Sprintf("a%016x", i)+".meta.json",
			strings.Replace(ctMeta, hsStarted, long, 1))
	}
	benchMember(b, tw, hsMain, ctBrief(ctUUID(1))+"\n")
	return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
}

// benchChainThread writes the main thread write writes, one of the chain's own
// cases at its bounds (BenchmarkAThreadChainAtItsBounds), the first
// [sandbox.MaxBridgeCalls] of whose calls asked names are asked, each by its
// place from 0: nil asks a call the thread holds none of.
func benchChainThread(write func(*benchThread),
	asked func(i int) string) func(*testing.B, *tar.Writer) sandbox.HoldingsAsk {

	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		t := benchWrite(b, filepath.Join(b.TempDir(), "thread.jsonl"), write)
		f, err := os.Open(t.file)
		if err != nil {
			b.Fatal(err)
		}
		defer f.Close()
		if err := tw.WriteHeader(&tar.Header{Name: hsMain, Mode: 0o600, Size: t.bytes,
			Typeflag: tar.TypeReg}); err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(tw, f); err != nil {
			b.Fatal(err)
		}
		if asked == nil {
			return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
		}
		var ask sandbox.HoldingsAsk
		for i := range sandbox.MaxBridgeCalls {
			ask.Calls = append(ask.Calls, sandbox.LoggedCall{ID: asked(i), Asked: true})
		}
		return ask
	}
}

// benchBlocks writes a main thread of one user record of a list of block, a
// JSON object, as many times as count answers for the line's head and tail.
func benchBlocks(block string, count func(head, block, tail string) int) func(*testing.B,
	*tar.Writer) sandbox.HoldingsAsk {

	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		head := `{"parentUuid":"` + ctUUID(1) + `","isSidechain":false,"type":"user","message":{"role":"user",` +
			`"content":`
		tail := `},"uuid":"` + ctUUID(2) + `","version":"2.1.310"}`
		blocks := count(head, block, tail)
		record := head + "[" + strings.Repeat(block+",", blocks-1) + block + "]" + tail
		benchMember(b, tw, hsMain, ctBrief(ctUUID(1))+"\n"+record+"\n")
		return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
	}
}

// benchBlocksLine writes a main thread of one user record of blocks of an
// empty kind to the plaintext's bound, written as it is made: every block one
// no reader names and none charged to the line's facts.
func benchBlocksLine(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
	const block = `,{"type":""}`
	brief := ctBrief(ctUUID(1)) + "\n"
	head := `{"parentUuid":"` + ctUUID(1) + `","isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":[` + block[1:]
	tail := `]},"uuid":"` + ctUUID(2) + `","version":"2.1.310"}` + "\n"
	n := (int(threadPlainMax-benchSlack) - len(brief) - len(head) - len(tail)) / len(block)
	if err := tw.WriteHeader(&tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg,
		Size: int64(len(brief) + len(head) + n*len(block) + len(tail))}); err != nil {
		b.Fatal(err)
	}
	w := bufio.NewWriterSize(tw, 1<<20)
	_, _ = w.WriteString(brief + head)
	chunk := strings.Repeat(block, 1<<16)
	for left := n; left > 0; left -= min(left, 1<<16) {
		_, _ = w.WriteString(chunk[:min(left, 1<<16)*len(block)])
	}
	_, _ = w.WriteString(tail)
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
	return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
}

// benchSubagentBlocks writes a main thread of a brief and a subagent's thread
// of user records of blocks of an empty kind, of the length at which
// [threadLinesMax] lines fill the plaintext's bound: of the threads measured,
// the slowest a reading reads, since a subagent's is read on past every line
// that leaves it not whole. It sets internal/sandbox's holdingsReadWait.
func benchSubagentBlocks(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
	brief := ctBrief(ctUUID(1)) + "\n"
	benchMember(b, tw, hsMain, brief)
	const head, block, last = `{"type":"user","isSidechain":true,"message":{"content":[`, `{"type":""},`,
		`{"type":""}]}}` + "\n"
	n := threadLinesMax - 1
	fill := (int(threadPlainMax-benchSlack)-len(brief))/n - len(head) - len(last)
	line := head + strings.Repeat(block, fill/len(block)) + last
	if err := tw.WriteHeader(&tar.Header{Name: claudeProjectPrefix + ctSession + "/subagents/agent-" + ctAgent + ".jsonl",
		Mode: 0o600, Typeflag: tar.TypeReg, Size: int64(n * len(line))}); err != nil {
		b.Fatal(err)
	}
	w := bufio.NewWriterSize(tw, 1<<20)
	chunk := strings.Repeat(line, 1<<10)
	for left := n; left > 0; left -= min(left, 1<<10) {
		_, _ = w.WriteString(chunk[:min(left, 1<<10)*len(line)])
	}
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
	return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
}

// benchLinesToTheBound writes a main thread of a brief and then line, to
// [threadLinesMax] lines in all, each read into its facts and passed over by
// the model as bookkeeping.
func benchLinesToTheBound(line string) func(*testing.B, *tar.Writer) sandbox.HoldingsAsk {
	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		brief, l := ctBrief(ctUUID(1))+"\n", line+"\n"
		n := threadLinesMax - 1
		if err := tw.WriteHeader(&tar.Header{Name: hsMain, Mode: 0o600, Typeflag: tar.TypeReg,
			Size: int64(len(brief) + n*len(l))}); err != nil {
			b.Fatal(err)
		}
		w := bufio.NewWriterSize(tw, 1<<20)
		_, _ = w.WriteString(brief)
		chunk := strings.Repeat(l, 1<<12)
		for left := n; left > 0; left -= min(left, 1<<12) {
			_, _ = w.WriteString(chunk[:min(left, 1<<12)*len(l)])
		}
		if err := w.Flush(); err != nil {
			b.Fatal(err)
		}
		return hsAsk(sandbox.LoggedCall{ID: "toolu_bench_asked", Asked: true})
	}
}

// benchWholePDFSession writes a main thread of a whole PDF's Read, its result's
// base64 inflating from its gzip by about ratio, then one call, asked, and
// its result.
func benchWholePDFSession(ratio float64) func(*testing.B, *tar.Writer) sandbox.HoldingsAsk {
	return func(b *testing.B, tw *tar.Writer) sandbox.HoldingsAsk {
		const read, asked, output = "toolu_bench_pdf", "toolu_bench_asked", "asked done"
		data := newBenchFill().text(benchPDF, 2*benchPDF, ratio)
		lines := append([]string{
			ctAssistant(ctUUID(1), ctUUID(2), hsCallOf(read, "Read", `{"file_path":"/w/x.pdf"}`)),
			benchPDFLine(ctUUID(2), ctUUID(3), read, ctStamp, data),
		}, hsCall(ctUUID(3), 4, asked, `[{"type":"text","text":"`+output+`"}]`)...)
		benchMember(b, tw, hsMain, hsThread(lines...))
		return hsAsk(hsLogged(asked, output))
	}
}
