package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// WHAT THE CHAIN COSTS AT ITS BOUNDS, in time and in memory: the figures
// [threadChain]'s THE BOUNDS states, End's time among them, and the ground of
// [chainCheckEvery]. Each case writes its thread to a file first and reads it
// from there a line at a time, through [threadReader] into the chain as a
// reader does (every whole line to See or Unread, End at the file's end, the
// message See hands back for a line held until the next, and the last one's
// through End), so what a case holds is the chain's and the read's, never the
// file's. It reports the thread's MiB and the records the chain filed; End's
// own milliseconds; the longest End ran between two looks at its context, from
// its start to its return (look-ms: how long it runs on once its caller gives
// it up); the MiB live as End starts, after a collection the timer leaves out
// (filed-MiB), and what End allocates (end-alloc-MiB), which bounds what End
// adds to it, since End lets go of nothing the chain filed before it returns;
// and the MiB live once End decided, the chain and the last message held,
// after a collection forced (live-MiB), each less what was live before the
// read. Its peak heap is read off the runtime's GC trace (GODEBUG=gctrace=1),
// one case a process, between the two lines it writes to standard error
// around its read: a sampling ticker would be a clock in test code. Each case
// holds the chain to the answer its thread is built for, so a change that
// turns a case into a cheaper answer fails it rather than measure that.
//
//	GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkAThreadChainAtItsBounds$/^<case>$' -benchtime 1x -benchmem
//
// A THREAD OF MORE THAN 5,242,880 BYTES IS MODELLED ONLY WHERE IT IS PATH-FREE
// ([threadChain], THE LARGE FILE), and every thread at the records bound is
// past it (5,242,880 bytes are 40 a line over 2^17 lines, and a record's uuid
// takes 45 with its key alone), so each case at a bound is one the large-file
// readers read as the small path does: its records' lines begin
// `{"parentUuid":`, it holds no compaction's boundary, and in the main thread
// the dispatch from its last record alone reaches every record. A step that
// rule keeps from any main thread past 5,242,880 bytes is measured where it
// runs: in a subagent's thread, which no reader starts a walk in, or at the
// most records a thread under that size holds.
//
// The cases:
//
//   - records_at_the_bound: [threadChainMaxRecords] replies, each of its own id
//     making one call of its own id, so the uuid index, the reply ids and the
//     call ids are each at their bound; all but the first and the last naming
//     a parent written after them, so End resolves every parent late, and the
//     walk from the last record takes every one;
//   - leaf_set_at_the_bound: [threadChainMaxRecords] attachments, each under
//     the one written at half its place, so half of them are terminals, and
//     none a user's or an assistant's record: the single path from the last
//     finds no turn, the leaf set walks up from every terminal, and, empty,
//     falls back to the newest record of all. records_at_the_bound's single
//     path finds its leaf, as every start whose walk up meets a user or
//     assistant record does, so the leaf set needs a case of its own;
//   - calls_at_the_bound: [threadChainMaxCalls] call blocks, every id its own,
//     in replies of 1,024 calls and records of 1,024 results under them, which
//     answer none of them, so the results' check by call id (whyResultByCall)
//     reads every one;
//   - retractions_and_kept_at_the_bound: the records bound filled half by
//     records, a quarter by retraction prefixes, each its own, and a quarter by
//     the uuids the newest of two compactions' boundaries lists, each list a
//     quarter of the bound, so the earlier list and the newer are both held as
//     the newer is filed. A boundary makes no thread path-free, so End answers
//     whyLargeFile at its first step: the case measures the filing;
//   - every_variant_distinct: the most distinct orders the chain hands, four: a
//     user record's retraction list, which 2.1.322 reads and 2.1.310 does not,
//     naming a quarter of the bound in prefixes, beside a tail two deep whose
//     times interleave, so each release's records part into warm_mochi's order
//     and the order without it, at the records bound. Not six: q1t moves the
//     leaf only where no record hangs off the checkpoint (2.1.322 @293812260,
//     `ke.parentUuid===r`), so the leaf it moves from has no tails, and its
//     variants hand one order; and past 5,242,880 bytes a leaf q1t moves is off
//     the last record's walk, so no such thread is path-free;
//   - past_five_mebibytes: the records bound past 5,242,880 bytes, half of it
//     replies on the walk and half attachments the dispatch reaches off it,
//     each line holding a nested `"uuid":"` its time follows before its own,
//     so the large-file byte facts take their every step on each line, and the
//     dispatch's coverage passes every record;
//   - fallback_at_the_bound: a subagent's thread of the records bound, every
//     parent naming no record and every time the same, so the walk takes the
//     fallback at every record, through its next-unmet pointers. A main thread
//     so is never path-free: the prune, walking from its last record alone,
//     keeps no other;
//   - newer_row_at_the_size_bound: the most records a thread of 5,242,880 bytes
//     holds, every one below the last row and the newest the deepest, so the
//     newer-row rule marks every record under the last row. A main thread past
//     that size whose last row has a record below it is never path-free: the
//     dispatch hands a record only under a parent written before it;
//   - windows_merging_at_the_bound: the records bound with a walk of
//     [threadChainMaxSpread] records, the longest the recovery spreads, whose
//     replies' records interleave so that every window opens before the one
//     before it ends: 10,922 windows, each recovering six records of its reply
//     off the walk, written so that each window's fall before the ones already
//     merged, and hanging a hook's chain off a walked attachment its window
//     ends at, every window rescanning the walked attachments of the ones
//     before it;
//   - the_reading_line_beside_it: records_at_the_bound's thread, then a line at
//     [threadLineMax] of the smallest results a message holds, the reading's
//     worst line, which See decodes beside the chain at both bounds before the
//     line passes them: the coincident worst of the reading and the chain;
//   - a_collections_read: a main thread to within a mebibyte of
//     [threadPlainMax], of lines of the 2,053 bytes probe P6 measured a real
//     main thread's average, a call and its result in turn, read as a node's
//     collection reads it.
func BenchmarkAThreadChainAtItsBounds(b *testing.B) {
	for _, bc := range []struct {
		name  string
		agent string
		write func(t *benchThread)
		check func(b *testing.B, read benchRead, t *benchThread)
	}{
		{"records_at_the_bound", "", benchAtTheBound, benchAtTheBoundRead},
		{"leaf_set_at_the_bound", "", benchLeafSet, benchLeafSetRead},
		{"calls_at_the_bound", "", benchCalls, benchCallsRead},
		{"retractions_and_kept_at_the_bound", "", benchRetractionsAndKept, benchRetractionsAndKeptRead},
		{"every_variant_distinct", "", benchEveryVariant, benchEveryVariantRead},
		{"past_five_mebibytes", "", benchPastFive, benchPastFiveRead},
		{"fallback_at_the_bound", ctAgent, benchFallback, benchFallbackRead},
		{"newer_row_at_the_size_bound", "", benchNewerRow, benchNewerRowRead},
		{"windows_merging_at_the_bound", "", benchWindows, benchWindowsRead},
		{"the_reading_line_beside_it", "", benchReadingLine, benchReadingLineRead},
		{"a_collections_read", "", benchCollection, benchCollectionRead},
	} {
		b.Run(bc.name, func(b *testing.B) {
			t := benchWrite(b, filepath.Join(b.TempDir(), "thread.jsonl"), bc.write)
			// WHAT WRITING THE THREAD LEFT is collected before the read starts,
			// so the trace's heaps after it are the read's own.
			var before, after runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&before)
			fmt.Fprintf(os.Stderr, "chain-bench %s: the read starts\n", bc.name)
			b.ResetTimer()
			var read benchRead
			for range b.N {
				read = benchReadThread(b, t.file, bc.agent)
			}
			b.StopTimer()
			runtime.GC()
			runtime.ReadMemStats(&after)
			fmt.Fprintf(os.Stderr, "chain-bench %s: the read ends\n", bc.name)
			// REPORTED ONCE THE TIMER IS RESET, which deletes every metric
			// reported before it.
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

// benchThread is a thread a case writes to its file, a line at a time, and
// what the case's check needs of it.
type benchThread struct {
	file  string
	w     *bufio.Writer
	bytes int64
	// records is how many records the case wrote, blocks the results its last
	// line holds where that is the reading's worst line, and handles the
	// records the case names by their place in the file.
	records, blocks int
	handles         []chainHandle
}

// line writes s, one line of the thread, and its break.
func (t *benchThread) line(s string) {
	t.w.WriteString(s)
	t.w.WriteByte('\n')
	t.bytes += int64(len(s)) + 1
}

// record writes s, the thread's next record, and answers its handle: every
// line a case writes is a record, so a record's handle is its line's number.
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
// back for the last line, as a reader that reads its facts at End holds it,
// End's time, the longest End ran between two looks at its context, the heap
// live as End started, and what End allocated.
type benchRead struct {
	c               *threadChain
	held            threadMessage
	end, look       time.Duration
	filed, endAlloc uint64
}

// benchReadThread reads the thread in file into a chain of agent's, every
// whole line to See or Unread and End at its end.
func benchReadThread(b *testing.B, file, agent string) benchRead {
	b.Helper()
	f, err := os.Open(file)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	read := benchRead{c: newThreadChain(agent)}
	r := newThreadReader(f, threadPlainMax)
	for {
		line, err := r.Next()
		var broken *threadLineError
		switch {
		case err == nil:
			_, read.held, _ = read.c.See(line)
			continue
		case errors.As(err, &broken):
			read.c.Unread(broken)
			continue
		case !errors.Is(err, io.EOF):
			b.Fatalf("the thread reads to %v", err)
		}
		break
	}
	// WHAT THE CHAIN FILED is what is live once a collection the timer leaves
	// out has run, and what End adds to it at most is what it allocates: it
	// lets go of nothing the chain filed before it returns.
	var filed, ended runtime.MemStats
	b.StopTimer()
	runtime.GC()
	runtime.ReadMemStats(&filed)
	b.StartTimer()
	looks := &benchLooks{Context: b.Context()}
	looks.start = time.Now()
	looks.last = looks.start
	if err := read.c.End(looks); err != nil {
		b.Fatalf("End answers %v", err)
	}
	read.end, read.look = looks.ended()
	runtime.ReadMemStats(&ended)
	read.filed, read.endAlloc = filed.HeapAlloc, ended.TotalAlloc-filed.TotalAlloc
	return read
}

// benchLooks is End's context, keeping the longest stretch End runs between
// two looks at it, the one from its start to its first look and the one from
// its last look to its return among them: how long End runs on once its
// caller gives it up.
type benchLooks struct {
	context.Context
	start, last time.Time
	longest     time.Duration
}

// Err is the context's error, a look End takes at it.
func (l *benchLooks) Err() error {
	now := time.Now()
	stretch := now.Sub(l.last) //fixedwait:measure End's stretch between two looks, reported, deciding nothing
	l.longest, l.last = max(l.longest, stretch), now
	return l.Context.Err()
}

// ended is End's time and the longest stretch it ran without a look, once it
// returned.
func (l *benchLooks) ended() (end, look time.Duration) {
	now := time.Now()
	end = now.Sub(l.start) //fixedwait:measure End's time, reported, deciding nothing
	look = now.Sub(l.last) //fixedwait:measure End's last stretch without a look, reported, deciding nothing
	return end, max(l.longest, look)
}

// benchModelled holds the chain to a thread it models, handing orders
// distinct orders, and answers them.
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

// benchHolds holds the chain to what each case at a bound shares: it filed
// every record the case wrote, the records bound filled as the case means it
// (filed, every record and every prefix and listed uuid), the leaf it walked
// from leaf (none for 0), and whether it is path-free.
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

// The keys a record says its side by right after its parent: a main
// thread's, and ctAgent's own in its subagent's thread.
const (
	benchMain = `"isSidechain":false,`
	benchOwn  = `"isSidechain":true,"agentId":"` + ctAgent + `",`
)

// benchRoot is the parent of a record that roots a chain.
const benchRoot = "null"

// benchRecord is record n, as Claude Code writes one and every large-file
// reader takes it for a slot keyed by its own uuid: its parent as written
// (benchRoot, or a quoted uuid), its side, its type and its keys, each ending
// in a comma, then its uuid and its time, ms after rcBase.
func benchRecord(parent, side, kind, keys string, n, ms int) string {
	return `{"parentUuid":` + parent + `,` + side + `"type":"` + kind + `",` + keys + `"uuid":"` + rcUUID(n) +
		`","timestamp":"` + rcAt(ms) + `"}`
}

// benchOf is the parent naming record n.
func benchOf(n int) string { return `"` + rcUUID(n) + `"` }

// benchNowhere is n's number past every record a case writes, one each n, so
// its uuid ([rcUUID]) is one no record carries.
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

// benchAtTheBound writes threadChainMaxRecords replies, each of its own id and
// making one call under an id of its own: the first roots the chain, the last
// names the second as its parent, each reply from the second the one written
// after it, and the one before the last the first, so the walk from the last
// takes every record, root first the first, then the one before the last back
// to the second, then the last.
func benchAtTheBound(t *benchThread) {
	n := threadChainMaxRecords
	for k := 1; k <= n; k++ {
		parent := benchOf(k + 1)
		switch k {
		case 1:
			parent = benchRoot
		case n - 1:
			parent = benchOf(1)
		case n:
			parent = benchOf(2)
		}
		t.record(benchRecord(parent, benchMain, "assistant", benchReply(fmt.Sprintf("msg_%08d", k),
			benchCall(fmt.Sprintf("toolu_%08d", k))), k, k))
	}
}

// benchAtTheBoundRead holds records_at_the_bound's chain to its thread: every
// record handed, root first, from the last.
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

// benchLeafSet writes threadChainMaxRecords attachments, the first rooting
// the thread and each other under the one written at half its place.
func benchLeafSet(t *benchThread) {
	t.record(benchRecord(benchRoot, benchMain, "attachment", benchNote, 1, 1))
	for k := 2; k <= threadChainMaxRecords; k++ {
		t.record(benchRecord(benchOf(k/2), benchMain, "attachment", benchNote, k, k))
	}
}

// benchLeafSetRead holds leaf_set_at_the_bound's chain to its thread: the
// leaf set empty, so the leaf the newest record, the last, and the walk up
// from it, one record at each halving of its place, handed root first.
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

// benchEach is how many calls, or results, calls_at_the_bound's records each
// hold.
const benchEach = 1 << 10

// benchCalls writes a prompt, then replies of benchEach calls each, each
// answered by a record of benchEach results under it whose ids are none of
// its calls', every id its own and every record on one chain, to
// threadChainMaxCalls call blocks.
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

// benchCallsRead holds calls_at_the_bound's chain to its thread: every record
// handed, every call filed.
func benchCallsRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, t.records, chainHandle(t.records), true)
	if o.Len() != t.records || len(read.c.occurs) != threadChainMaxCalls {
		b.Fatalf("the chain hands %d records and filed %d calls; want %d and %d", o.Len(), len(read.c.occurs),
			t.records, threadChainMaxCalls)
	}
}

// benchRetractionsAndKept writes a prompt, a retry whose retraction list
// names a quarter of the records bound, each entry of a prefix its own; two
// compactions' boundaries, each listing a quarter of the bound in uuids no
// record carries, the newer's in place of the older's; and records on a chain
// to half the bound.
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
// to its thread: the records bound filled, not passed, and the thread, which
// holds a boundary, whyLargeFile.
func benchRetractionsAndKeptRead(b *testing.B, read benchRead, t *benchThread) {
	if why, _ := read.c.Unknown(); why != whyLargeFile || read.c.past || read.c.filed != threadChainMaxRecords ||
		t.records != threadChainMaxRecords/2 {
		b.Fatalf("the chain filed %d items of %d records, past its bound %t, and reads the thread as %q; want %d, "+
			"%d, false and %q", read.c.filed, t.records, read.c.past, why, threadChainMaxRecords,
			threadChainMaxRecords/2, whyLargeFile)
	}
}

// benchEveryVariant writes a prompt, a chain of replies, a reply the retry
// after it takes back with a quarter of the records bound's worth of the
// replies before it, the leaf, a reply under the retry, and three tails under
// the leaf, two deep with their times interleaved: one under the leaf written
// 3 s after it, one 2 s after, and one under that 5 s after, so warm_mochi's
// walk by depth hands them 2 s, 5 s, 3 s, and the walk by breadth sorted by
// time 2 s, 3 s, 5 s. Records and prefixes fill the records bound.
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

// benchEveryVariantRead holds every_variant_distinct's chain to its thread:
// four orders, one for warm_mochi's variants and one for the rest of each
// release's, the retraction taking a quarter of the bound out of 2.1.322's.
func benchEveryVariantRead(b *testing.B, read benchRead, t *benchThread) {
	orders := benchModelled(b, read.c, 4)
	benchHolds(b, read.c, t, threadChainMaxRecords, t.handles[0], true)
	want := []chainVariants{variant322SpringMochi | variant322SpringMochiMetcalfe,
		variant322Off | variant322Spring | variant322SpringMetcalfe, variant310SpringMochi, variant310Off | variant310Spring}
	lens := []int{t.records - threadChainMaxRecords/4, t.records - threadChainMaxRecords/4, t.records, t.records}
	for i, o := range orders {
		if o.Variants() != want[i] || o.Len() != lens[i] {
			b.Fatalf("order %d of the variants %08b hands %d records; want the variants %08b to hand %d", i,
				o.Variants(), o.Len(), want[i], lens[i])
		}
	}
	if slices.Equal(orders[0].records, orders[1].records) || slices.Equal(orders[2].records, orders[3].records) {
		b.Fatal("the tails' two orders are one")
	}
}

// benchNested is the keys of a nested object's uuid that its time follows, a
// uuid no record carries: a reader keys a line holding it by the uuid at its
// object's depth one, its own, found by a scan of its bytes.
func benchNested(n int) string {
	return `"uuid":"` + rcUUID(benchNowhere(n)) + `","timestamp":"` + rcAt(n) + `",`
}

// benchPastFive writes threadChainMaxRecords records: half replies on one
// chain, the first rooting it, and half attachments the dispatch reaches off
// it, one under each reply but the last and two under the first, each line
// holding a nested uuid its time follows before its own.
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

// benchPastFiveRead holds past_five_mebibytes's chain to its thread: past the
// small path's bound, path-free, the replies handed.
func benchPastFiveRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, threadChainMaxRecords, t.handles[0], true)
	if t.bytes <= claudeResumeLargeFile || o.Len() != threadChainMaxRecords/2 {
		b.Fatalf("a thread of %d bytes hands %d records; want one past %d bytes to hand %d", t.bytes, o.Len(),
			claudeResumeLargeFile, threadChainMaxRecords/2)
	}
}

// benchFallback writes threadChainMaxRecords replies of ctAgent's own, each of
// its own id, each naming a parent no record is and all written at one time.
func benchFallback(t *benchThread) {
	for k := 1; k <= threadChainMaxRecords; k++ {
		t.record(benchRecord(benchOf(benchNowhere(k)), benchOwn, "assistant",
			benchReply(fmt.Sprintf("msg_%08d", k), rcSaid("a")), k, 0))
	}
}

// benchFallbackRead holds fallback_at_the_bound's chain to its thread: every
// record an own leaf, the first the leaf, and the walk taking the next in the
// file at each, so every record handed, the last first.
func benchFallbackRead(b *testing.B, read benchRead, t *benchThread) {
	n := threadChainMaxRecords
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, n, 1, true)
	if o.Len() != n || o.At(0) != chainHandle(n) || o.At(n-1) != 1 || len(read.c.OwnLeaves()) != n {
		b.Fatalf("the chain hands %d records from %d to %d, of %d own leaves; want %d from %d to 1, of %d", o.Len(),
			o.At(0), o.At(o.Len()-1), len(read.c.OwnLeaves()), n, n, n)
	}
}

// benchNewerRow writes as many prompts as 5,242,880 bytes hold, each naming as
// its parent the one written after it and the last rooting the chain, so
// every record is below the last row; the first, the deepest, is the newest.
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

// benchNewerRowRead holds newer_row_at_the_size_bound's chain to its thread:
// the newer row leads, so every record is handed, the last row first.
func benchNewerRowRead(b *testing.B, read benchRead, t *benchThread) {
	m := t.records
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, m, 1, false)
	if t.bytes > claudeResumeLargeFile || o.Len() != m || o.At(0) != chainHandle(m) || o.At(m-1) != 1 {
		b.Fatalf("a thread of %d bytes hands %d records from %d to %d; want one of at most %d to hand %d from %d to 1",
			t.bytes, o.Len(), o.At(0), o.At(o.Len()-1), claudeResumeLargeFile, m, m)
	}
}

// The shape of windows_merging_at_the_bound: how many records of its reply
// each window recovers, and how many records each hangs.
const (
	benchSiblings = 6
	benchHung     = 3
)

// benchWindows writes windows_merging_at_the_bound's thread: a prompt; m
// replies' first records, each making a call; six more records of each reply
// under its first, written round by round, each round from the last reply to
// the first; then each reply's second record, a walked attachment under it,
// and a hook's chain of three attachments under that, the next reply's
// second record under the walked attachment; and the leaf, a reply under the
// last walked attachment. The walk is the prompt, the first records, each
// second record with its walked attachment, and the leaf, threadChainMaxSpread
// records; the last walked attachment's chain takes what the records bound
// leaves.
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

// benchWindowsRead holds windows_merging_at_the_bound's chain to its thread:
// the records bound filled and every record handed, from the leaf.
func benchWindowsRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, threadChainMaxRecords, t.handles[0], true)
	if t.records != threadChainMaxRecords || o.Len() != threadChainMaxRecords {
		b.Fatalf("a thread of %d records hands %d; want %d of %d", t.records, o.Len(), threadChainMaxRecords,
			threadChainMaxRecords)
	}
}

// benchReadingLine writes records_at_the_bound's thread, then a user record
// under its last reply of the smallest results a message holds, as many as a
// line at threadLineMax holds.
func benchReadingLine(t *benchThread) {
	benchAtTheBound(t)
	n := threadChainMaxRecords
	head := `{"parentUuid":"` + rcUUID(n) + `","isSidechain":false,"type":"user","message":{"role":"user","content":[`
	tail := `]},"uuid":"` + rcUUID(n+1) + `","timestamp":"` + rcAt(n+1) + `"}`
	const block = `{"type":"tool_result","tool_use_id":"a"}`
	t.blocks = (threadLineMax - 1 - len(head) - len(tail) + 1) / (len(block) + 1)
	t.record(head + strings.Repeat(block+",", t.blocks-1) + block + tail)
}

// benchReadingLineRead holds the_reading_line_beside_it's chain to its thread:
// the line passes the chain's bound after See decoded it, every result held.
func benchReadingLineRead(b *testing.B, read benchRead, t *benchThread) {
	if why, _ := read.c.Unknown(); why != whyBound || read.c.handles != threadChainMaxRecords ||
		len(read.held.Calls()) != t.blocks {
		b.Fatalf("the chain filed %d records, reads the thread as %q, and handed back a line of %d results; want %d, "+
			"%q and %d", read.c.handles, why, len(read.held.Calls()), threadChainMaxRecords, whyBound, t.blocks)
	}
}

// benchCollection writes a main thread to within a mebibyte of threadPlainMax
// of lines of 2,053 bytes, a real main thread's average (probe P6), break
// included: a prompt, then a reply making a call and a record of its result
// in turn, each on the one chain.
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

// benchCollectionRead holds a_collections_read's chain to its thread: within
// the plaintext's bound, path-free, every record handed.
func benchCollectionRead(b *testing.B, read benchRead, t *benchThread) {
	o := benchModelled(b, read.c, 1)[0]
	benchHolds(b, read.c, t, t.records, chainHandle(t.records), true)
	if t.bytes > threadPlainMax || o.Len() != t.records {
		b.Fatalf("a thread of %d bytes hands %d records; want one of at most %d to hand %d", t.bytes, o.Len(),
			threadPlainMax, t.records)
	}
}
