package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// BenchmarkAThreadChainAtItsBounds measures [threadChain] at THE BOUNDS, the
// ground of [chainCheckEvery] among them, and, where a case sets count, beside
// the resumed count ([resumedReading]) in the one walk ([walkThread]) a node's
// collection reads its main thread in. Each case reads its thread from a file,
// so what it holds is the read's, and its check holds the chain to the answer
// its thread is built for, so a change to a cheaper answer fails rather than be
// measured. Run one case a process under GODEBUG=gctrace=1 (the commands are
// above the cases): between the two lines a case writes to stderr around its
// read, HELD AT MOST is the largest live heap (`start->end->live MB`'s third
// figure) at GOGC=10, a cycle every tenth of the heap's growth so none goes
// unsampled; the PEAK is the largest start or end at the default GOGC. A
// sampling ticker would be a clock in test code.
func BenchmarkAThreadChainAtItsBounds(b *testing.B) {
	// A thread past 5,242,880 bytes is modelled only path-free ([threadChain],
	// THE LARGE FILE), and every thread at the records bound is past it (40
	// bytes a line at 2^17 lines; a record's uuid and key take 45). So a case
	// at a bound begins every line `{"parentUuid":`, holds no compaction
	// boundary, and in a main thread reaches every record by the dispatch from
	// its last; a step that rule keeps out is measured in a subagent's thread
	// or at the most records a thread under that size holds. Each case's
	// thread is described at its writer.
	//
	//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAThreadChainAtItsBounds$/^<case>$' -benchtime 1x -benchmem
	//	GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAThreadChainAtItsBounds$/^<case>$' -benchtime 1x -benchmem
	for _, bc := range []struct {
		name  string
		agent string
		write func(t *benchThread)
		check func(b *testing.B, read benchRead, t *benchThread)
		// count reads the resumed count beside the chain; refused, that the
		// walk ends at a line past the layer's bounds ([errThreadBound]).
		count, refused bool
	}{
		{"records_at_the_bound", "", benchAtTheBound, benchAtTheBoundRead, false, false},
		{"leaf_set_at_the_bound", "", benchLeafSet, benchLeafSetRead, false, false},
		{"calls_at_the_bound", "", benchCalls, benchCallsRead, false, false},
		{"retractions_and_kept_at_the_bound", "", benchRetractionsAndKept, benchRetractionsAndKeptRead, false, false},
		{"every_variant_distinct", "", benchEveryVariant, benchEveryVariantRead, false, false},
		{"past_five_mebibytes", "", benchPastFive, benchPastFiveRead, false, false},
		{"fallback_at_the_bound", ctAgent, benchFallback, benchFallbackRead, false, false},
		{"newer_row_at_the_size_bound", "", benchNewerRow, benchNewerRowRead, false, false},
		{"windows_merging_at_the_bound", "", benchWindows, benchWindowsRead, false, false},
		{"the_reading_line_beside_it", "", benchReadingLine, benchReadingLineRead, false, false},
		{"the_reading_line_past_it", "", benchReadingLinePast, benchReadingLinePastRead, false, true},
		{"the_reading_line_at_both_bounds", "", benchReadingLineAtBoth, benchReadingLineAtBothRead, false, false},
		{"a_whole_pdf_line_beside_it", "", benchWholePDF, benchWholePDFRead, false, false},
		{"a_collections_read", "", benchCollection, benchCollectionRead, false, false},
		{"a_collections_pass", "", benchCollection, benchCollectionPassRead, true, false},
		{"the_reading_line_beside_the_count", "", benchReadingLine, benchReadingLineCountRead, true, false},
		{"the_counts_id_line_beside_it", "", benchIDLine, benchCountsLineRead, true, true},
		{"the_counts_iteration_line_beside_it", "", benchIterationLine, benchCountsLineRead, true, true},
		{"the_counts_edit_line_beside_it", "", benchEditLine, benchCountsLineRead, true, true},
		{"what_the_count_keeps_beside_it", "", benchCountKeeps, benchCountKeepsRead, true, false},
	} {
		b.Run(bc.name, func(b *testing.B) {
			t := benchWrite(b, filepath.Join(b.TempDir(), "thread.jsonl"), bc.write)
			// Collect what writing the thread left, so the trace's heaps after
			// this line are the read's own.
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			fmt.Fprintf(os.Stderr, "chain-bench %s: the read starts\n", bc.name)
			b.ResetTimer()
			var read benchRead
			for range b.N {
				read = benchReadThread(b, t.file, bc.agent, bc.count)
			}
			b.StopTimer()
			runtime.GC()
			runtime.ReadMemStats(&after)
			fmt.Fprintf(os.Stderr, "chain-bench %s: the read ends\n", bc.name)
			if refused := errors.Is(read.err, errThreadBound); refused != bc.refused {
				b.Fatalf("the walk ends with %v; want it refused at a line past the layer's bounds %t", read.err,
					bc.refused)
			}
			// Reported after ResetTimer, which deletes every metric reported
			// before it. look-ms is [benchLooks]'s longest stretch; filed-MiB
			// is live as the walk ended and live-MiB once End decided, each less
			// what was live before the read; end-alloc-MiB bounds what End
			// adds ([benchReadThread]).
			b.ReportMetric(float64(t.bytes)/(1<<20), "thread-MiB")
			b.ReportMetric(float64(read.c.handles), "records")
			b.ReportMetric(float64(read.end.Microseconds())/1000, "end-ms")
			b.ReportMetric(float64(read.look.Microseconds())/1000, "look-ms")
			b.ReportMetric(float64(int64(read.filed)-int64(before.HeapAlloc))/(1<<20), "filed-MiB")
			b.ReportMetric(float64(read.endAlloc)/(1<<20), "end-alloc-MiB")
			b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), "live-MiB")
			bc.check(b, read, t)
			runtime.KeepAlive(read)
		})
	}
}

// benchThread is a thread a case writes to its file, and what its check needs.
type benchThread struct {
	file  string
	w     *bufio.Writer
	bytes int64
	// records counts the records written; blocks is the results in the last
	// line where that is the reading's worst line; handles are records the
	// case's check names.
	records, blocks int
	handles         []chainHandle
}

// line writes s, one line of the thread, and its break.
func (t *benchThread) line(s string) {
	t.w.WriteString(s)
	t.w.WriteByte('\n')
	t.bytes += int64(len(s)) + 1
}

// record writes s as the next record and answers its handle: every line a
// case writes is a record, so a handle is its line's number.
func (t *benchThread) record(s string) chainHandle {
	t.line(s)
	t.records++
	return chainHandle(t.records)
}

// benchWrite writes the thread write makes to file.
func benchWrite(b *testing.B, file string, write func(t *benchThread)) *benchThread {
	b.Helper()
	f, err := os.Create(file)
	if err != nil {
		b.Fatal(err)
	}
	t := &benchThread{file: file, w: bufio.NewWriterSize(f, 1<<20)}
	write(t)
	if err := t.w.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	t.w = nil
	return t
}

// benchRead is a thread read into a chain: the chain, the message See handed
// back for the last line (held as a reader that reads its facts at End holds
// it), End's time and longest stretch without a look, the heap live as the
// walk ended, what End allocated, the count where the case reads one, and
// errThreadBound where a line past the layer's bounds ended the walk, which
// leaves no reading, so End never runs.
type benchRead struct {
	c               *threadChain
	held            threadMessage
	end, look       time.Duration
	filed, endAlloc uint64
	count           *resumedReading
	err             error
	// escalated is whether the walk took the large slot ([passBudget]).
	escalated bool
}

// benchChain reads the walk ([walkThread]) into the chain: every whole line to
// See, holding the message it hands back until the next, and every refused
// line to Unread.
type benchChain struct {
	c    *threadChain
	held threadMessage
	err  error
}

var _ threadReading = (*benchChain)(nil)

// line hands l to See, holding the message it hands back.
func (r *benchChain) line(l threadLine) { _, r.held, _ = r.c.See(l) }

// refused hands broken to Unread.
func (r *benchChain) refused(broken *threadLineError) { r.c.Unread(broken) }

// end keeps what ended the walk: nil at the thread's end.
func (r *benchChain) end(_ bool, err error) { r.err = err }

// records is how many items the chain holds, which the walk holds to its
// base ([recordKeeper], [chainReading.records]).
func (r *benchChain) records() int { return r.c.items() }

// benchReadThread reads file into a chain of agent's in one walk, then End;
// where count is set, the resumed count and the next request's estimate read
// the same walk after the chain, as a node's collection does. The chain goes first so the message See hands
// back is held while the count reads the same line: the costlier order.
func benchReadThread(b *testing.B, file, agent string, count bool) benchRead {
	b.Helper()
	f, err := os.Open(file)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	read := benchRead{c: newThreadChain(agent)}
	chain := &benchChain{c: read.c}
	readings := []threadReading{chain}
	if count {
		// A collection that counts asks the next request's estimate beside it.
		read.count = new(resumedReading)
		readings = append(readings, read.count, new(nextReading))
	}
	large := new(passBudget)
	walkThread(b.Context(), f, threadPlainMax, large, readings...)
	read.escalated = large.took != nil
	large.release()
	if chain.err != nil && !errors.Is(chain.err, errThreadBound) {
		b.Fatalf("the thread reads to %v", chain.err)
	}
	read.held, read.err = chain.held, chain.err
	// What the chain filed is what is live after a collection outside the
	// timer; End adds at most what it allocates, since it frees nothing the
	// chain filed before it returns.
	var filed, ended runtime.MemStats
	b.StopTimer()
	runtime.GC()
	runtime.ReadMemStats(&filed)
	b.StartTimer()
	read.filed = filed.HeapAlloc
	if read.err != nil {
		return read
	}
	looks := &benchLooks{Context: b.Context()}
	looks.start = time.Now()
	looks.last = looks.start
	if err := read.c.End(looks); err != nil {
		b.Fatalf("End answers %v", err)
	}
	read.end, read.look = looks.ended()
	runtime.ReadMemStats(&ended)
	read.endAlloc = ended.TotalAlloc - filed.TotalAlloc
	return read
}

// benchLooks is End's context, keeping the longest stretch End runs without a
// look at it, from its start and to its return included: how long End runs on
// once its caller gives it up.
type benchLooks struct {
	context.Context
	start, last time.Time
	longest     time.Duration
}

// Err is a look End takes at its context.
func (l *benchLooks) Err() error {
	now := time.Now()
	stretch := now.Sub(l.last) //fixedwait:measure End's stretch between two looks, reported, deciding nothing
	l.longest, l.last = max(l.longest, stretch), now
	return l.Context.Err()
}

// ended is End's time and longest stretch without a look, once it returned.
func (l *benchLooks) ended() (end, look time.Duration) {
	now := time.Now()
	end = now.Sub(l.start) //fixedwait:measure End's time, reported, deciding nothing
	look = now.Sub(l.last) //fixedwait:measure End's last stretch without a look, reported, deciding nothing
	return end, max(l.longest, look)
}

// benchModelled holds the chain to modelling its thread in orders distinct
// orders, and answers them.
func benchModelled(b *testing.B, c *threadChain, orders int) []chainOrder {
	b.Helper()
	if why, unknown := c.Unknown(); unknown {
		b.Fatalf("the chain cannot tell the thread (%s); want it modelled", why)
	}
	var got []chainOrder
	c.EachOrder(func(o chainOrder) bool {
		got = append(got, o)
		return true
	})
	if len(got) != orders {
		b.Fatalf("the chain hands %d distinct orders; want %d", len(got), orders)
	}
	return got
}

// benchHolds holds the chain to what each case at a bound shares: every record
// written filed, filed items in all (records, prefixes and listed uuids), the
// walk from leaf (unchecked for noChainRecord), and path-free as anySize.
func benchHolds(b *testing.B, c *threadChain, t *benchThread, filed int, leaf chainHandle, anySize bool) {
	b.Helper()
	if c.handles != t.records || c.filed != filed {
		b.Fatalf("the chain filed %d records and %d items; want %d and %d", c.handles, c.filed, t.records, filed)
	}
	if got, ok := c.Leaf(); leaf != noChainRecord && (!ok || got != leaf) {
		b.Fatalf("the chain walks from %d (%t); want %d", got, ok, leaf)
	}
	if c.AnySize() != anySize {
		b.Fatalf("the thread of %d bytes reads as path-free %t; want %t", t.bytes, c.AnySize(), anySize)
	}
}

// A record's side keys, right after its parent: a main thread's, and
// ctAgent's own in its subagent's thread.
const (
	benchMain = `"isSidechain":false,`
	benchOwn  = `"isSidechain":true,"agentId":"` + ctAgent + `",`
)

// benchRoot is the parent of a record that roots a chain.
const benchRoot = "null"

// benchRecord is record n as Claude Code writes it, which every large-file
// reader keys by its own uuid: parent as written (benchRoot or a quoted uuid),
// side, type and keys, each ending in a comma, then uuid and time, ms after
// rcBase.
func benchRecord(parent, side, kind, keys string, n, ms int) string {
	return `{"parentUuid":` + parent + `,` + side + `"type":"` + kind + `",` + keys + `"uuid":"` + rcUUID(n) +
		`","timestamp":"` + rcAt(ms) + `","version":"2.1.310"}`
}

// benchOf is the parent naming record n.
func benchOf(n int) string { return `"` + rcUUID(n) + `"` }

// benchNowhere is a number past every record a case writes, one per n, so its
// uuid ([rcUUID]) names no record.
func benchNowhere(n int) int { return 1<<26 + n }

// benchReply is a reply's keys: its id and its content, a JSON list.
func benchReply(id, content string) string {
	return `"message":{"id":"` + id + `","type":"message","role":"assistant","model":"claude-sonnet-4-5",` +
		`"content":` + content + `,"stop_reason":"end_turn"},`
}

// benchCall is a reply's content of one call under id.
func benchCall(id string) string {
	return `[{"type":"tool_use","id":"` + id + `","name":"Bash","input":{}}]`
}

// A person's prompt's keys, and an attachment's that follows no call.
const (
	benchPrompt = `"message":{"role":"user","content":"a prompt"},`
	benchNote   = `"attachment":{"type":"total_tokens_reminder","text":"tokens left"},`
)

// benchAtTheBound writes records_at_the_bound: threadChainMaxRecords replies,
// each of its own id making one call of its own id, so the uuid index, reply
// ids and call ids each reach their bound. Most name a parent written after
// them ([benchAtTheBoundParent]), so End resolves parents late, and the walk
// from the last takes every record.
func benchAtTheBound(t *benchThread) {
	benchAtTheBoundTo(t, threadChainMaxRecords+1)
}

// benchAtTheBoundTo writes records_at_the_bound's records before record last.
func benchAtTheBoundTo(t *benchThread, last int) {
	for k := 1; k < last; k++ {
		t.record(benchRecord(benchAtTheBoundParent(k), benchMain, "assistant", benchReply(fmt.Sprintf("msg_%08d", k),
			benchCall(fmt.Sprintf("toolu_%08d", k))), k, k))
	}
}

// benchAtTheBoundParent is the parent records_at_the_bound's record k names:
// the first roots the chain, the one before the last names the first, the
// last the second, and every other the one written after it, so the walk
// hands 1, n-1 back to 2, then n.
func benchAtTheBoundParent(k int) string {
	switch n := threadChainMaxRecords; k {
	case 1:
		return benchRoot
	case n - 1:
		return benchOf(1)
	case n:
		return benchOf(2)
	}
	return benchOf(k + 1)
}

// benchAtTheBoundRead holds records_at_the_bound's chain to every record
// handed, root first, walking from the last.
func benchAtTheBoundRead(b *testing.B, read benchRead, t *benchThread) {
	n := threadChainMaxRecords
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, n, chainHandle(n), true)
	if o.Len() != n || o.At(0) != 1 || o.At(1) != chainHandle(n-1) || o.At(n-1) != chainHandle(n) ||
		len(read.c.occurs) != threadChainMaxCalls {
		b.Fatalf("the chain hands %d records from %d to %d, with %d calls; want %d from 1 to %d, with %d", o.Len(),
			o.At(0), o.At(o.Len()-1), len(read.c.occurs), n, n, threadChainMaxCalls)
	}
}

// benchLeafSet writes leaf_set_at_the_bound: threadChainMaxRecords
// attachments, the first the root and each other under the one at half its
// place, so half are terminals. With no user or assistant record the single
// path from the last finds no turn, so the leaf set walks up from every
// terminal and, empty, falls back to the newest record. records_at_the_bound's
// single path finds its leaf, so the leaf set needs this case.
func benchLeafSet(t *benchThread) {
	t.record(benchRecord(benchRoot, benchMain, "attachment", benchNote, 1, 1))
	for k := 2; k <= threadChainMaxRecords; k++ {
		t.record(benchRecord(benchOf(k/2), benchMain, "attachment", benchNote, k, k))
	}
}

// benchLeafSetRead holds leaf_set_at_the_bound's chain to the newest record as
// its leaf and the walk up from it, one record per halving, root first.
func benchLeafSetRead(b *testing.B, read benchRead, t *benchThread) {
	n := threadChainMaxRecords
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, n, chainHandle(n), true)
	if depth := bits.Len(uint(n)); o.Len() != depth || o.At(0) != 1 || o.At(1) != 2 ||
		o.At(o.Len()-1) != chainHandle(n) {
		b.Fatalf("the chain hands %d records from %d, %d to %d; want %d from 1, 2 to %d", o.Len(), o.At(0),
			o.At(1), o.At(o.Len()-1), depth, n)
	}
}

// benchEach is the calls, or results, each of calls_at_the_bound's records holds.
const benchEach = 1 << 10

// benchCalls writes calls_at_the_bound: a prompt, then replies of benchEach
// calls, each under a record of benchEach results answering none of them, so
// the check by call id (whyResultByCall) reads every one; every id its own,
// every record on one chain, to threadChainMaxCalls call blocks.
func benchCalls(t *benchThread) {
	t.record(benchRecord(benchRoot, benchMain, "user", benchPrompt, 1, 0))
	uses, results := make([]string, benchEach), make([]string, benchEach)
	for p := range threadChainMaxCalls / (2 * benchEach) {
		for k := range benchEach {
			uses[k] = fmt.Sprintf(`{"type":"tool_use","id":"toolu_use_%03d_%04d","name":"Bash","input":{}}`, p, k)
			results[k] = fmt.Sprintf(`{"type":"tool_result","tool_use_id":"toolu_result_%03d_%04d","content":"ok"}`,
				p, k)
		}
		n := t.records
		reply := t.record(benchRecord(benchOf(n), benchMain, "assistant", benchReply(fmt.Sprintf("msg_%03d", p),
			"["+strings.Join(uses, ",")+"]"), n+1, n+1))
		t.record(benchRecord(benchOf(int(reply)), benchMain, "user", `"message":{"role":"user","content":[`+
			strings.Join(results, ",")+`]},"sourceToolAssistantUUID":"`+rcUUID(int(reply))+`",`, n+2, n+2))
	}
}

// benchCallsRead holds calls_at_the_bound's chain to every record handed and
// every call filed.
func benchCallsRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, t.records, chainHandle(t.records), true)
	if o.Len() != t.records || len(read.c.occurs) != threadChainMaxCalls {
		b.Fatalf("the chain hands %d records and filed %d calls; want %d and %d", o.Len(), len(read.c.occurs),
			t.records, threadChainMaxCalls)
	}
}

// benchRetractionsAndKept writes retractions_and_kept_at_the_bound: a prompt;
// a retry whose retraction list names a quarter of the records bound, each
// prefix its own; two compactions' boundaries, each listing a quarter in uuids
// no record carries, so both lists are held as the newer is filed; and records
// on a chain to half the bound. A boundary makes no thread path-free, so End
// answers whyLargeFile at its first step: the case measures the filing.
func benchRetractionsAndKept(t *benchThread) {
	n := threadChainMaxRecords
	quarter := n / 4
	named := make([]int, quarter)
	for i := range named {
		named[i] = benchNowhere(i)
	}
	t.record(benchRecord(benchRoot, benchMain, "user", benchPrompt, 1, 0))
	t.record(rcRetry(1, 2, 1, rcQuoted(named...)))
	for _, from := range []int{quarter, 2 * quarter} {
		for i := range named {
			named[i] = benchNowhere(from + i)
		}
		boundary := t.records + 1
		t.record(rcBoundary(boundary, boundary, `{"preservedMessages":{"anchorUuid":"`+rcUUID(1)+`","uuids":[`+
			rcQuoted(named...)+`]}}`))
	}
	for k := t.records + 1; k <= n/2; k++ {
		t.record(benchRecord(benchOf(k-1), benchMain, "user", benchPrompt, k, k))
	}
}

// benchRetractionsAndKeptRead holds retractions_and_kept_at_the_bound's chain
// to the records bound filled, not passed, and the thread whyLargeFile.
func benchRetractionsAndKeptRead(b *testing.B, read benchRead, t *benchThread) {
	if why, _ := read.c.Unknown(); why != whyLargeFile || read.c.past || read.c.filed != threadChainMaxRecords ||
		t.records != threadChainMaxRecords/2 {
		b.Fatalf("the chain filed %d items of %d records, past its bound %t, and reads the thread as %q; want %d, "+
			"%d, false and %q", read.c.filed, t.records, read.c.past, why, threadChainMaxRecords,
			threadChainMaxRecords/2, whyLargeFile)
	}
}

// benchEveryVariant writes every_variant_distinct, the most distinct orders
// the chain hands, four: a prompt, a chain of replies, a reply the retry after
// it takes back with a quarter of the records bound's worth of the replies
// before it (a list 2.1.322 reads and 2.1.310 does not), the leaf, a reply
// under the retry, and three tails under it: 3 s after, 2 s after, and one
// 5 s after under that, so warm_mochi's depth walk hands 2, 5, 3 and the
// breadth walk by time 2, 3, 5. Records and prefixes fill the records bound.
// Not six: q1t moves the leaf only where no record hangs off the checkpoint
// (2.1.322 @293812260, `ke.parentUuid===r`), a leaf with no tails, so its
// variants hand one order; past 5,242,880 bytes a leaf q1t moves is off the
// last record's walk, so never path-free.
func benchEveryVariant(t *benchThread) {
	n := threadChainMaxRecords
	quarter := n / 4
	t.record(benchRecord(benchRoot, benchMain, "user", benchPrompt, 1, 0))
	for k := 2; k <= n-quarter-6; k++ {
		t.record(benchRecord(benchOf(k-1), benchMain, "assistant", benchReply(fmt.Sprintf("msg_%08d", k),
			rcSaid("a")), k, k))
	}
	refused := t.records + 1
	t.record(benchRecord(benchOf(refused-1), benchMain, "assistant", benchReply("msg_refused", rcSaid("no")), refused,
		refused))
	named := []int{refused}
	for k := 2; len(named) < quarter; k++ {
		named = append(named, k)
	}
	retry := t.record(rcRetry(refused, refused+1, refused+1, rcQuoted(named...)))
	leaf := int(retry) + 1
	t.handles = []chainHandle{t.record(benchRecord(benchOf(int(retry)), benchMain, "assistant",
		benchReply("msg_leaf", rcSaid("done")), leaf, leaf))}
	t.record(rcAttach(leaf, leaf+1, leaf+3000, rcNote))
	t.record(rcAttach(leaf, leaf+2, leaf+2000, rcNote))
	t.record(rcAttach(leaf+2, leaf+3, leaf+5000, rcNote))
}

// benchEveryVariantRead holds every_variant_distinct's chain to four orders,
// warm_mochi's and the rest per release, the retraction taking a quarter of
// the bound out of 2.1.322's and 2.1.330's.
func benchEveryVariantRead(b *testing.B, read benchRead, t *benchThread) {
	orders := benchModelled(b, read.c, 4)
	benchHolds(b, read.c, t, threadChainMaxRecords, t.handles[0], true)
	want := []chainVariants{variantsMochi &^ variant310SpringMochi, allVariants &^ variantsMochi &^ variants310,
		variant310SpringMochi, variant310Off | variant310Spring}
	lens := []int{t.records - threadChainMaxRecords/4, t.records - threadChainMaxRecords/4, t.records, t.records}
	for i, o := range orders {
		if o.Variants() != want[i] || o.Len() != lens[i] {
			b.Fatalf("order %d of the variants %013b hands %d records; want the variants %013b to hand %d", i,
				o.Variants(), o.Len(), want[i], lens[i])
		}
	}
	if slices.Equal(orders[0].records, orders[1].records) || slices.Equal(orders[2].records, orders[3].records) {
		b.Fatal("the tails' two orders are one")
	}
}

// benchNested is a nested object's uuid, which no record carries, and its
// time: a reader must key the line by the depth-one uuid its byte scan finds.
func benchNested(n int) string {
	return `"uuid":"` + rcUUID(benchNowhere(n)) + `","timestamp":"` + rcAt(n) + `",`
}

// benchPastFive writes past_five_mebibytes: threadChainMaxRecords records,
// half replies on one chain and half attachments the dispatch reaches off it
// (one under each reply but the last, two under the first), each line holding
// a nested uuid before its own, so the large-file byte facts take every step
// on each line and the dispatch's coverage passes every record.
func benchPastFive(t *benchThread) {
	replies := threadChainMaxRecords / 2
	reply := func(parent string, k int) chainHandle {
		n := t.records + 1
		return t.record(benchRecord(parent, benchMain, "assistant", `"message":{"id":"`+fmt.Sprintf("msg_%08d", k)+
			`",`+benchNested(n)+`"type":"message","role":"assistant","model":"claude-sonnet-4-5",`+
			`"content":[{"type":"text","text":"a"}],"stop_reason":"end_turn"},`, n, n))
	}
	note := func(under chainHandle) {
		n := t.records + 1
		t.record(benchRecord(benchOf(int(under)), benchMain, "attachment", `"attachment":{"type":"total_tokens_reminder",`+
			benchNested(n)+`"text":"tokens left"},`, n, n))
	}
	last := reply(benchRoot, 1)
	note(last)
	note(last)
	for k := 2; k <= replies; k++ {
		last = reply(benchOf(int(last)), k)
		if k < replies {
			note(last)
		}
	}
	t.handles = []chainHandle{last}
}

// benchPastFiveRead holds past_five_mebibytes's chain to path-free past the
// small path's bound, the replies handed.
func benchPastFiveRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, threadChainMaxRecords, t.handles[0], true)
	if t.bytes <= claudeResumeLargeFile || o.Len() != threadChainMaxRecords/2 {
		b.Fatalf("a thread of %d bytes hands %d records; want one past %d bytes to hand %d", t.bytes, o.Len(),
			claudeResumeLargeFile, threadChainMaxRecords/2)
	}
}

// benchFallback writes fallback_at_the_bound: threadChainMaxRecords replies of
// ctAgent's own, each of its own id, naming a parent no record is, all at one
// time, so the walk takes the fallback at every record. A subagent's, since a
// main thread so is never path-free: the prune, from its last record, keeps
// no other.
func benchFallback(t *benchThread) {
	for k := 1; k <= threadChainMaxRecords; k++ {
		t.record(benchRecord(benchOf(benchNowhere(k)), benchOwn, "assistant",
			benchReply(fmt.Sprintf("msg_%08d", k), rcSaid("a")), k, 0))
	}
}

// benchFallbackRead holds fallback_at_the_bound's chain to every record an own
// leaf, the first the leaf, and every record handed, the last first.
func benchFallbackRead(b *testing.B, read benchRead, t *benchThread) {
	n := threadChainMaxRecords
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, n, 1, true)
	if o.Len() != n || o.At(0) != chainHandle(n) || o.At(n-1) != 1 || len(read.c.OwnLeaves()) != n {
		b.Fatalf("the chain hands %d records from %d to %d, of %d own leaves; want %d from %d to 1, of %d", o.Len(),
			o.At(0), o.At(o.Len()-1), len(read.c.OwnLeaves()), n, n, n)
	}
}

// benchNewerRow writes newer_row_at_the_size_bound: as many prompts as
// 5,242,880 bytes hold, each under the one written after it, the last the
// root, and the first, the deepest, the newest, so the newer-row rule marks
// every record under the last row. Not past that size: a main thread whose
// last row has a record below it then is never path-free, since the dispatch
// hands a record only under a parent written before it.
func benchNewerRow(t *benchThread) {
	prompt := func(parent string, n, ms int) string {
		return benchRecord(parent, benchMain, "user", `"message":{"role":"user","content":"x"},`, n, ms)
	}
	m := claudeResumeLargeFile / (len(prompt(benchOf(2), 1, 0)) + 1)
	for k := 1; k <= m; k++ {
		parent, ms := benchOf(k+1), k
		switch k {
		case 1:
			ms = m + claudeFallbackMS
		case m:
			parent = benchRoot
		}
		t.record(prompt(parent, k, ms))
	}
}

// benchNewerRowRead holds newer_row_at_the_size_bound's chain to every record
// handed, the last row first.
func benchNewerRowRead(b *testing.B, read benchRead, t *benchThread) {
	m := t.records
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, m, 1, false)
	if t.bytes > claudeResumeLargeFile || o.Len() != m || o.At(0) != chainHandle(m) || o.At(m-1) != 1 {
		b.Fatalf("a thread of %d bytes hands %d records from %d to %d; want one of at most %d to hand %d from %d to 1",
			t.bytes, o.Len(), o.At(0), o.At(o.Len()-1), claudeResumeLargeFile, m, m)
	}
}

// windows_merging_at_the_bound's records each window recovers of its reply,
// and the length of each hook's chain.
const (
	benchSiblings = 6
	benchHung     = 3
)

// benchWindows writes windows_merging_at_the_bound: a prompt; m replies' first
// records, each making a call; six more records of each under its first, round
// by round from the last reply to the first, so each window's fall before
// those already merged; each reply's second record, a walked attachment under
// it bearing a hook's chain, and the next second record under that; and the
// leaf under the last walked attachment. The walk is threadChainMaxSpread
// records, the longest the recovery spreads, in m windows that each open
// before the one before ends and rescan the earlier ones' walked attachments.
// The last hook's chain takes what the records bound leaves.
func benchWindows(t *benchThread) {
	m := (threadChainMaxSpread - 2) / 3
	id := func(k int) string { return fmt.Sprintf("msg_%05d", k) }
	record := func(parent chainHandle, kind, keys string) chainHandle {
		n := t.records + 1
		return t.record(benchRecord(benchOf(int(parent)), benchMain, kind, keys, n, n))
	}
	t.record(benchRecord(benchRoot, benchMain, "user", benchPrompt, 1, 1))
	first := make([]chainHandle, m+1)
	under := chainHandle(1)
	for k := 1; k <= m; k++ {
		first[k] = record(under, "assistant", benchReply(id(k), benchCall(fmt.Sprintf("toolu_%05d", k))))
		under = first[k]
	}
	for range benchSiblings {
		for k := m; k >= 1; k-- {
			record(first[k], "assistant", benchReply(id(k), rcSaid("more")))
		}
	}
	more := threadChainMaxRecords - (2 + m*(3+benchSiblings+benchHung))
	for k := 1; k <= m; k++ {
		second := record(under, "assistant", benchReply(id(k), rcSaid("said")))
		under = record(second, "attachment", benchNote)
		chain := benchHung
		if k == m {
			chain += more
		}
		for hung, i := under, 0; i < chain; i++ {
			hung = record(hung, "attachment", benchNote)
		}
	}
	t.handles = []chainHandle{record(under, "assistant", benchReply("msg_leaf", rcSaid("done")))}
}

// benchWindowsRead holds windows_merging_at_the_bound's chain to the records
// bound filled and every record handed, from the leaf.
func benchWindowsRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, threadChainMaxRecords, t.handles[0], true)
	if t.records != threadChainMaxRecords || o.Len() != threadChainMaxRecords {
		b.Fatalf("a thread of %d records hands %d; want %d of %d", t.records, o.Len(), threadChainMaxRecords,
			threadChainMaxRecords)
	}
}

// benchReadingLine writes the_reading_line_beside_it: records_at_the_bound's
// thread, then the reading's worst line ([benchReadingLineLast]), which See
// reads beside the chain at both bounds before the line passes them.
func benchReadingLine(t *benchThread) {
	benchAtTheBound(t)
	benchReadingLineLast(t, 0)
}

// benchReadingLinePast writes the_reading_line_past_it: the_reading_line_beside_it
// with one result more on its last line, whose facts then pass [threadLineMax].
func benchReadingLinePast(t *benchThread) {
	benchAtTheBound(t)
	benchReadingLineLast(t, 1)
}

// benchReadingLineAtBoth writes the_reading_line_at_both_bounds:
// records_at_the_bound's thread whose last record is the reading's worst line,
// its results the last of the calls bound, so the chain reaches both bounds
// at that line and End runs beside its facts. The records before it that
// would pass the calls bound make no call.
func benchReadingLineAtBoth(t *benchThread) {
	n := threadChainMaxRecords
	head := `{"parentUuid":` + benchAtTheBoundParent(n) + `,"isSidechain":false,"type":"user",` +
		`"message":{"role":"user","content":`
	tail := `},"uuid":"` + rcUUID(n) + `","timestamp":"` + rcAt(n) + `","version":"2.1.310"}`
	t.blocks = benchFactsHeld(head, benchSmallest, tail)
	for k := 1; k < n; k++ {
		content := `[{"type":"text","text":"no call"}]`
		if k <= threadChainMaxCalls-t.blocks {
			content = benchCall(fmt.Sprintf("toolu_%08d", k))
		}
		t.record(benchRecord(benchAtTheBoundParent(k), benchMain, "assistant",
			benchReply(fmt.Sprintf("msg_%08d", k), content), k, k))
	}
	t.record(head + "[" + strings.Repeat(benchSmallest+",", t.blocks-1) + benchSmallest + "]" + tail)
}

// benchReadingLineAtBothRead holds the_reading_line_at_both_bounds's chain to
// both bounds reached and neither passed, every result of its last line held.
func benchReadingLineAtBothRead(b *testing.B, read benchRead, t *benchThread) {
	if why, unknown := read.c.Unknown(); unknown || read.c.handles != threadChainMaxRecords ||
		len(read.c.occurs) != threadChainMaxCalls || len(read.held.Calls()) != t.blocks {
		b.Fatalf("the chain filed %d records and %d calls, reads the thread as %q, and handed back a line of %d "+
			"results; want %d, %d, a thread it places, and %d", read.c.handles, len(read.c.occurs), why,
			len(read.held.Calls()), threadChainMaxRecords, threadChainMaxCalls, t.blocks)
	}
}

// benchSmallest is the smallest result a user record holds.
const benchSmallest = `{"type":"tool_result","tool_use_id":"a"}`

// benchReadingLineLast writes, under the record that fills the records bound,
// a user record of as many of the smallest results as a line's facts hold
// within [threadLineMax] ([benchFactsHeld]), and more besides.
func benchReadingLineLast(t *benchThread, more int) {
	n := t.records + 1
	head := `{"parentUuid":"` + rcUUID(threadChainMaxRecords) + `","isSidechain":false,"type":"user",` +
		`"message":{"role":"user","content":`
	tail := `},"uuid":"` + rcUUID(n) + `","timestamp":"` + rcAt(n) + `","version":"2.1.310"}`
	t.blocks = benchFactsHeld(head, benchSmallest, tail) + more
	t.record(head + "[" + strings.Repeat(benchSmallest+",", t.blocks-1) + benchSmallest + "]" + tail)
}

// benchReadingLineRead holds the_reading_line_beside_it's chain to whyBound
// after See read the line, every result held.
func benchReadingLineRead(b *testing.B, read benchRead, t *benchThread) {
	if why, _ := read.c.Unknown(); why != whyBound || read.c.handles != threadChainMaxRecords ||
		len(read.held.Calls()) != t.blocks {
		b.Fatalf("the chain filed %d records, reads the thread as %q, and handed back a line of %d results; want %d, "+
			"%q and %d", read.c.handles, why, len(read.held.Calls()), threadChainMaxRecords, whyBound, t.blocks)
	}
}

// benchReadingLinePastRead holds the_reading_line_past_it to the walk ended at
// its last line, every record before it filed: what it costs is the refusal's.
func benchReadingLinePastRead(b *testing.B, read benchRead, _ *benchThread) {
	if read.c.handles != threadChainMaxRecords || !strings.Contains(read.err.Error(), "facts run past") {
		b.Fatalf("the chain filed %d records, the walk ended by %v; want %d, and the line's facts past their bound",
			read.c.handles, read.err, threadChainMaxRecords)
	}
}

// benchPDF is each of the two copies of a document's base64 a whole-PDF Read
// writes on its one line, the result's and the tool's own: 55,924,056 bytes
// in all, the longest line measured.
const benchPDF = 27_962_028

// benchPDFLine is the user record answering call, a Read of a whole PDF whose
// base64 is data, under parent: a line no reader reads but for its call.
func benchPDFLine(parent, uuid, call, at, data string) string {
	return `{"parentUuid":"` + parent + `","isSidechain":false,"type":"user","message":{"role":"user","content":` +
		`[{"type":"tool_result","tool_use_id":"` + call + `","content":[{"type":"document","source":` +
		`{"type":"base64","media_type":"application/pdf","data":"` + data + `"}}]}]},"uuid":"` + uuid +
		`","timestamp":"` + at + `","toolUseResult":{"type":"pdf","file":{"base64":"` + data +
		`","filePath":"/w/x.pdf","originalSize":20971520}},"version":"2.1.310"}`
}

// benchWholePDF writes a_whole_pdf_line_beside_it: records_at_the_bound's
// thread, then a whole PDF's Read result under its last record, answering
// its call.
func benchWholePDF(t *benchThread) {
	benchAtTheBound(t)
	n := t.records + 1
	t.record(benchPDFLine(rcUUID(threadChainMaxRecords), rcUUID(n), fmt.Sprintf("toolu_%08d", threadChainMaxRecords),
		rcAt(n), strings.Repeat("A", benchPDF)))
}

// benchWholePDFRead holds a_whole_pdf_line_beside_it's chain to whyBound after
// See read the line, its one result held.
func benchWholePDFRead(b *testing.B, read benchRead, t *benchThread) {
	if why, _ := read.c.Unknown(); why != whyBound || read.c.handles != threadChainMaxRecords ||
		len(read.held.Calls()) != 1 || t.bytes < 2*benchPDF {
		b.Fatalf("the chain filed %d records, reads the thread as %q, and handed back a line of %d results; want %d, "+
			"%q and 1", read.c.handles, why, len(read.held.Calls()), threadChainMaxRecords, whyBound)
	}
}

// benchCollection writes a_collections_read's thread: to within a mebibyte of
// threadPlainMax, lines of 2,053 bytes with the break, a real main thread's
// average (probe P6): a prompt, then a call and its result in turn, on one
// chain.
func benchCollection(t *benchThread) {
	const line = 2053
	fill := strings.Repeat("the harness's own words, as a tool answered them and the model read them. ", line/32)
	padded := func(head, tail string) string {
		return head + fill[:line-1-len(head)-len(tail)] + tail
	}
	lines := int((threadPlainMax - 1<<20) / line)
	t.record(benchRecord(benchRoot, benchMain, "user", benchPrompt, 1, 1))
	for t.records < lines {
		n := t.records + 1
		call := fmt.Sprintf("toolu_%08d", n/2)
		var record string
		if n%2 == 0 {
			record = benchRecord(benchOf(n-1), benchMain, "assistant", `"message":{"id":"`+fmt.Sprintf("msg_%08d", n)+
				`","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[{"type":"tool_use","id":"`+
				call+`","name":"Bash","input":{"command":"@"}}],"stop_reason":"tool_use"},`, n, n)
		} else {
			record = benchRecord(benchOf(n-1), benchMain, "user", `"message":{"role":"user","content":[`+
				`{"type":"tool_result","tool_use_id":"`+call+`","content":"@"}]},"sourceToolAssistantUUID":"`+
				rcUUID(n-1)+`",`, n, n)
		}
		at := strings.IndexByte(record, '@')
		t.record(padded(record[:at], record[at+1:]))
	}
}

// benchCollectionRead holds a_collections_read's chain to path-free within the
// plaintext's bound, every record handed.
func benchCollectionRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, t.records, chainHandle(t.records), true)
	if t.bytes > threadPlainMax || o.Len() != t.records {
		b.Fatalf("a thread of %d bytes hands %d records; want one of at most %d to hand %d", t.bytes, o.Len(),
			threadPlainMax, t.records)
	}
}

// benchCountRead holds the count beside the chain to reading the thread whole,
// a chained line among it.
func benchCountRead(b *testing.B, read benchRead) {
	b.Helper()
	if read.count == nil || read.count.unread || !read.count.chained {
		b.Fatal("the count beside the chain did not read the thread whole")
	}
}

// benchReadingLineCountRead holds the_reading_line_beside_the_count as
// the_reading_line_beside_it, the count read whole.
func benchReadingLineCountRead(b *testing.B, read benchRead, t *benchThread) {
	benchReadingLineRead(b, read, t)
	benchCountRead(b, read)
}

// benchInvalid is n bytes no UTF-8 holds, each decoded as U+FFFD, three bytes
// ([threadDecoding]): the most a line's string grows as it is decoded.
func benchInvalid(n int) string { return strings.Repeat("\xff", n) }

// benchCountsReply writes records_at_the_bound's last record, which fills both
// bounds, as a reply with message keys keys, [threadValueMax] bytes of invalid
// UTF-8 at keys's "@": the count's own worst lines, each a value it reads
// whole past that bound, so the walk ends there, the chain holding all it
// filed.
func benchCountsReply(t *benchThread, keys string) {
	n := threadChainMaxRecords
	head, tail, _ := strings.Cut(benchRecord(benchAtTheBoundParent(n), benchMain, "assistant", `"message":{`+keys+
		`,"type":"message","role":"assistant","content":`+benchCall(fmt.Sprintf("toolu_%08d", n))+
		`,"stop_reason":"tool_use"},`, n, n), "@")
	t.record(head + benchInvalid(threadValueMax) + tail)
}

// The usage every line benchCountsReply writes counts: two tokens.
const benchUsage = `"input_tokens":1,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":1`

// benchIterationLine writes records_at_the_bound's thread, its last record a
// reply whose usage lists one iteration whose type fills the line.
func benchIterationLine(t *benchThread) {
	benchAtTheBoundTo(t, threadChainMaxRecords)
	benchCountsReply(t, `"id":"msg_count","model":"claude-sonnet-4-5","usage":{`+benchUsage+
		`,"iterations":[{"type":"@"}]}`)
}

// benchEditLine writes records_at_the_bound's thread, its last record a reply
// whose applied context edits list one whose type fills the line.
func benchEditLine(t *benchThread) {
	benchAtTheBoundTo(t, threadChainMaxRecords)
	benchCountsReply(t, `"id":"msg_count","model":"claude-sonnet-4-5",`+
		`"context_management":{"applied_edits":[{"type":"@"}]},"usage":{`+benchUsage+`}`)
}

// benchIDLine writes records_at_the_bound's thread, its last record a reply
// whose id fills the line.
func benchIDLine(t *benchThread) {
	benchAtTheBoundTo(t, threadChainMaxRecords)
	benchCountsReply(t, `"id":"@","model":"claude-sonnet-4-5","usage":{`+benchUsage+`}`)
}

// benchCountsLineRead holds the walk ended at the last line, its value past
// [threadValueMax], every record before it filed, and the count unread.
func benchCountsLineRead(b *testing.B, read benchRead, _ *benchThread) {
	if read.c.handles != threadChainMaxRecords-1 || read.c.past || read.count == nil || !read.count.unread ||
		!strings.Contains(read.err.Error(), "a value is read whole within") {
		b.Fatalf("the chain filed %d records, past its bound %t, the walk ended by %v, the count unread %t; want "+
			"%d, false, a value past its bound, and unread", read.c.handles, read.c.past, read.err,
			read.count != nil && read.count.unread, threadChainMaxRecords-1)
	}
}

// benchCountKeeps writes what_the_count_keeps_beside_it, the most the count
// keeps from line to line: a result record whose uuid is [threadValueMax]
// bytes as written, the first chained record, under a parent no record is, so
// a restart; a reply whose id is as long, the count's run; a sidechain's
// replies, each of its own id and call, to the records bound, no main
// thread's to the count; and the reading's worst line.
func benchCountKeeps(t *benchThread) {
	head, tail, _ := strings.Cut(`{"parentUuid":"`+rcUUID(benchNowhere(0))+`","isSidechain":false,"type":"user",`+
		`"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_00000001","content":"ok"}]},`+
		`"uuid":"@","timestamp":"`+rcAt(1)+`"}`, "@")
	t.record(head + benchInvalid(threadValueMax-2) + tail)
	head, tail, _ = strings.Cut(benchRecord(benchOf(benchNowhere(1)), benchMain, "assistant",
		benchReply("@", benchCall("toolu_00000002")), 2, 2), "@")
	t.record(head + benchInvalid(threadValueMax-2) + tail)
	for k := 3; k <= threadChainMaxRecords; k++ {
		t.record(benchRecord(benchOf(k-1), `"isSidechain":true,`, "assistant", benchReply(fmt.Sprintf("msg_%08d", k),
			benchCall(fmt.Sprintf("toolu_%08d", k))), k, k))
	}
	benchReadingLineLast(t, 0)
}

// benchCountKeepsRead holds what_the_count_keeps_beside_it to the chain past
// its bound at the reading's line, every result held, and the count read
// whole, keeping of the restart's uuid and the run's id at most a uuid's bytes
// and a digest.
func benchCountKeepsRead(b *testing.B, read benchRead, t *benchThread) {
	benchCountRead(b, read)
	if read.c.handles != threadChainMaxRecords || !read.c.past || len(read.held.Calls()) != t.blocks ||
		!read.count.flagged || len(read.count.newest) > sandbox.RestartIDMaxBytes {
		b.Fatalf("the chain filed %d records, past its bound %t, and handed back a line of %d results, and the "+
			"count keeps a restart of %d bytes (flagged %t); want %d, true, %d, and at most %d (true)",
			read.c.handles, read.c.past, len(read.held.Calls()), len(read.count.newest), read.count.flagged,
			threadChainMaxRecords, t.blocks, sandbox.RestartIDMaxBytes)
	}
}

// benchCollectionPassRead holds a_collections_pass as a_collections_read, the
// count read whole.
func benchCollectionPassRead(b *testing.B, read benchRead, t *benchThread) {
	benchCollectionRead(b, read, t)
	benchCountRead(b, read)
}
