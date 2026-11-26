package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// The cases here that take the process's large slot ([collectionLargeReads])
// are not parallel: the slot is the process's own, and a parallel case never
// runs beside a serial one.

// testPass is a pass's budget that tb's end gives back.
func testPass(tb testing.TB) *passBudget {
	large := new(passBudget)
	tb.Cleanup(large.release)
	return large
}

// slotWait is a case's context that says when the code under test first waits
// on it: a look at Done, which only a wait for a slot makes here ([takeSlot]).
type slotWait struct {
	context.Context
	once   sync.Once
	waited chan struct{}
}

func newSlotWait(ctx context.Context) *slotWait {
	return &slotWait{Context: ctx, waited: make(chan struct{})}
}

func (w *slotWait) Done() <-chan struct{} {
	w.once.Do(func() { close(w.waited) })
	return w.Context.Done()
}

// selfWait is a case's context that ends itself where the code under test is
// about to wait for a large slot while every one is held: in a case that holds
// none itself, a pass waiting for the slot it already holds.
type selfWait struct {
	context.Context
	cancel func()
	waited atomic.Bool
}

func (w *selfWait) Done() <-chan struct{} {
	if len(collectionLargeReads) == CollectionLargeReadsAtOnce {
		w.waited.Store(true)
		w.cancel()
	}
	return w.Context.Done()
}

// holdLargeSlot takes the process's large slot for the case, and answers what
// gives it back, once, at the latest as the case ends.
func holdLargeSlot(t *testing.T) func() {
	t.Helper()
	if len(collectionLargeReads) != 0 {
		t.Fatal("the large slot is held before the case takes it")
	}
	slots := collectionLargeReads
	slots <- struct{}{}
	free := sync.OnceFunc(func() { <-slots })
	t.Cleanup(free)
	return free
}

// slotReading is a reading that keeps a record of every line it is handed, as
// the chain files one ([recordKeeper]), and notes the large slots held as it
// reads each and how the walk ended.
type slotReading struct {
	lines []int
	held  []int
	ended error
	ends  int
}

func (r *slotReading) line(l threadLine) {
	r.lines = append(r.lines, l.N)
	r.held = append(r.held, len(collectionLargeReads))
}
func (r *slotReading) refused(e *threadLineError) { r.line(threadLine{N: -e.N}) }
func (r *slotReading) end(_ bool, err error)      { r.ended, r.ends = err, r.ends+1 }
func (r *slotReading) records() int               { return len(r.lines) }

var _ recordKeeper = (*slotReading)(nil)

// slotUser is record n of a chain of users, each under the one before it, its
// line pad bytes longer than a bare one.
func slotUser(n, pad int) string {
	return cntUser(slotParent(n), fmt.Sprintf("u%d", n), `"`+strings.Repeat("a", pad)+`"`)
}

// slotUserOf is record n with its line exactly size bytes, its break aside.
func slotUserOf(n, size int) string { return slotUser(n, size-len(slotUser(n, 0))) }

// slotChain is a thread of records 1 to n, each a bare user's line.
func slotChain(n int, last ...string) string {
	lines := make([]string, 0, n+len(last))
	for k := 1; k <= n; k++ {
		lines = append(lines, slotUser(k, 0))
	}
	return cntThread(append(lines, last...)...)
}

// slotIDBytes is the id of each full result of [resultsCharging]: just past
// 32 KiB, so it is held in whole 8 KiB pages, the most a charged byte holds.
const slotIDBytes = 32<<10 + 1

// lineCharge is what line's facts charge the meter ([lineMeter]), read whole.
func lineCharge(tb testing.TB, line string) int {
	tb.Helper()
	r := newThreadReader(strings.NewReader(line+"\n"), threadPlainMax)
	if _, err := r.Next(context.Background()); err != nil {
		tb.Fatalf("the line reads %v", err)
	}
	return r.meterMax - r.dec.meter.left
}

// resultsCharging is head, a list of results, and tail, whose facts charge the
// meter exactly charge bytes ([lineMeter]), found by reading it: each result
// under an id of its own of [slotIDBytes] but the last, cut to the rest.
func resultsCharging(tb testing.TB, head, tail string, charge int) string {
	tb.Helper()
	line := func(full, last int) string {
		var b strings.Builder
		b.WriteString(head)
		for i := range full {
			fmt.Fprintf(&b, `{"type":"tool_result","tool_use_id":"%0*d"},`, slotIDBytes, i)
		}
		b.WriteString(`{"type":"tool_result","tool_use_id":"` + strings.Repeat("z", last) + `"}`)
		b.WriteString(tail)
		return b.String()
	}
	full := charge / slotIDBytes
	for full > 0 && lineCharge(tb, line(full, 1)) > charge {
		full--
	}
	got := line(full, 1+charge-lineCharge(tb, line(full, 1)))
	if c := lineCharge(tb, got); c != charge {
		tb.Fatalf("the line meant to charge %d bytes charges %d", charge, c)
	}
	return got
}

// slotUserCharging is record n of the chain of users ([slotUser]) whose facts
// charge the meter exactly charge bytes ([resultsCharging]).
func slotUserCharging(tb testing.TB, n, charge int) string {
	tb.Helper()
	head, tail, _ := strings.Cut(cntUser(slotParent(n), fmt.Sprintf("u%d", n), "[@]"), "@")
	return resultsCharging(tb, head, tail, charge)
}

// grownScratch is what a user record under parent (as written) writes to grow
// each scratch buffer its reader holds a value in to its most within the line
// ([lineDecoder.reset]): head, every head key twice, at 32 KiB and then at
// [threadValueMax] bytes as written, a `type` of typ as written, then the
// record's own head; block, a text block holding each block key so; and
// result, a toolUseResult holding each of its keys so.
func grownScratch(parent, typ string) (head, block, result string) {
	twice := func(keys ...string) string {
		var b strings.Builder
		for _, k := range keys {
			for _, n := range []int{32 << 10, threadValueMax} {
				fmt.Fprintf(&b, `"%s":"%s",`, k, strings.Repeat("x", n-2))
			}
		}
		return b.String()
	}
	head = twice("type", "subtype", "agentId", "parentUuid", "isSidechain", "replacesSpan") + `"type":"` +
		typ + `","parentUuid":` + parent + `,"isSidechain":false,"type":"user",` +
		`"subtype":"","agentId":"","replacesSpan":false,`
	block = `{` + twice("type", "id", "name", "tool_use_id", "is_error") + `"input":{` +
		strings.TrimSuffix(twice("to"), ",") + `},"content":[{` + strings.TrimSuffix(twice("type"), ",") +
		`}],"type":"text","text":"x"}`
	result = `"toolUseResult":{` + strings.TrimSuffix(twice("status", "agentId", "success"), ",") + `},`
	return head, block, result
}

// scratchGrown is what line's reader holds within it, read whole: its buffers
// as made (the fewest bytes of three makings), the scanner's capture at the
// bound it grows to and no further ([jsonscan.Scanner.Capture]), and each
// scratch buffer and the type's text as the line grew them. It fails tb where
// one is short of what [grownScratch] grows it to, the type's text of typeText
// bytes, or where the whole passes [readerLineBytes].
func scratchGrown(tb testing.TB, line string, typeText int) int {
	tb.Helper()
	src := strings.NewReader(line + "\n")
	var r *threadReader
	made := ^uint64(0)
	for range 3 {
		_, bytes := ownAllocations(func() { r = newThreadReader(src, threadPlainMax) })
		made = min(made, bytes)
	}
	if _, err := r.Next(context.Background()); err != nil {
		tb.Fatalf("the line reads %v", err)
	}
	d, b := &r.dec, &r.dec.block
	bufs := [][]byte{b.typ, b.id, b.name, b.toolUseID, b.isError, b.to, b.resultType, d.held[0], d.held[1], d.held[2]}
	for _, h := range d.head {
		bufs = append(bufs, h.raw)
	}
	held := int(made) + threadValueMax + cap(d.typeText)
	for i, buf := range bufs {
		if cap(buf) < threadValueMax {
			tb.Fatalf("scratch buffer %d holds %d bytes, want at least %d", i, cap(buf), threadValueMax)
		}
		held += cap(buf)
	}
	if cap(d.typeText) < typeText {
		tb.Fatalf("the type's text holds %d bytes, want at least %d", cap(d.typeText), typeText)
	}
	if held > readerLineBytes {
		tb.Fatalf("the reader holds %d bytes within the line (%d as made, %d buffers grown), past the %d it is "+
			"counted at", held, made, len(bufs), readerLineBytes)
	}
	return held
}

// THE BASE COEFFICIENTS HOLD that a test can measure ([readerLineBytes],
// [chargeStringsBytes]): a line growing every scratch buffer ([grownScratch])
// leaves its reader within its figure; and a head of three strings each of
// [threadValueMax] bytes as written, each decoding to three, holds them, the
// last beside the buffer json v2 unquotes it through (AppendUnquote(nil), grown
// by append, live while the string is copied out of it), within its figure.
//
// Mutation: a scratch buffer grown to twice its value, or the strings counted
// without that buffer, and the case fails.
func TestTheBaseCoefficientsHold(t *testing.T) {
	line := slotUserGrown(t, collectionBaseRecords, collectionBaseLineCharge)
	t.Logf("the reader holds %d bytes within a line at its most", scratchGrown(t, line, 3*(threadValueMax-2)))
	raw := []byte(`"` + benchInvalid(threadValueMax-2) + `"`)
	h, err := headOf(headKeys{typ: headKeyOf(raw), subtype: headKeyOf(raw), agentID: headKeyOf(raw)})
	if err != nil {
		t.Fatalf("the head reads %v", err)
	}
	// Each invalid byte unquotes as U+FFFD; json v2 drops the error it answers.
	unquoted, _ := jsontext.AppendUnquote(nil, raw)
	held := cap(unquoted)
	for _, s := range []string{h.Type, h.Subtype, h.AgentID} {
		// A string past 32 KiB takes whole 8 KiB pages.
		held += (len(s) + 8<<10 - 1) &^ (8<<10 - 1)
	}
	if held > chargeStringsBytes {
		t.Errorf("a head's three strings hold %d bytes with the last one's decode, past the %d they are counted at",
			held, chargeStringsBytes)
	}
}

// slotUserGrown is record n of the chain of users ([slotUser]) growing every
// scratch buffer ([grownScratch]) beside results whose facts charge the meter
// exactly charge bytes ([resultsCharging]).
func slotUserGrown(tb testing.TB, n, charge int) string {
	tb.Helper()
	head, block, result := grownScratch(cntParent(slotParent(n)), benchInvalid(threadValueMax-2))
	line := resultsCharging(tb, `{"parentUuid":`+cntParent(slotParent(n))+`,"isSidechain":false,"type":"user",`+
		head+`"message":{"role":"user","content":[`+block+`,`, `]},`+result+fmt.Sprintf(`"uuid":"u%d"}`, n), charge)
	scratchGrown(tb, line, 3*(threadValueMax-2))
	return line
}

// slotSmallest is record n of the chain of users holding count of the
// smallest results ([benchSmallest]): a line of many facts and few bytes.
func slotSmallest(n, count int) string {
	return cntUser(slotParent(n), fmt.Sprintf("u%d", n), "["+strings.Repeat(","+benchSmallest, count)[1:]+"]")
}

// slotParent is the parent of record n of the chain of users.
func slotParent(n int) string {
	if n > 1 {
		return fmt.Sprintf("u%d", n-1)
	}
	return ""
}

// manyFacts is a line of the smallest results whose facts charge past
// [collectionBaseLineCharge] and whose bytes are within [scrubPieceBaseBytes],
// failing tb where they are not.
func manyFacts(tb testing.TB, n int) string {
	tb.Helper()
	line := slotSmallest(n, 5000)
	if c := lineCharge(tb, line); c <= collectionBaseLineCharge || len(line) >= scrubPieceBaseBytes {
		tb.Fatalf("the line of many facts charges %d bytes in %d, want past %d in under %d", c, len(line),
			collectionBaseLineCharge, scrubPieceBaseBytes)
	}
	return line
}

// A TYPICAL COLLECTION READ TAKES NO SLOT: the probe's two threads, and a
// thread AT both bases, [collectionBaseRecords] records kept beside a line
// whose facts charge [collectionBaseLineCharge], the line's reader holding its
// most or not ([grownScratch]), are each walked with the large slot free
// throughout, as seen from inside the walk at every line.
//
// Mutation: escalate on every pass, at either base (`>=` for `>`), or on what
// the reader holds of a line, and a line is read with the slot held.
func TestATypicalCollectionReadTakesNoSlot(t *testing.T) {
	threads := map[string]string{
		"at both bases": slotChain(collectionBaseRecords-1,
			slotUserCharging(t, collectionBaseRecords, collectionBaseLineCharge)),
		"at both bases, the reader at its most": slotChain(collectionBaseRecords-1,
			slotUserGrown(t, collectionBaseRecords, collectionBaseLineCharge)),
	}
	for _, file := range []string{
		"testdata/claude/2.1.310/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000106.jsonl",
		"testdata/claude/2.1.322/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000113.jsonl",
	} {
		raw, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Fatal(err)
		}
		threads[file] = string(raw)
	}
	for name, body := range threads {
		var (
			r     slotReading
			count resumedReading
			large passBudget
		)
		walkThread(t.Context(), strings.NewReader(body), threadPlainMax, &large, &r, &count)
		large.release()
		if r.ended != nil || len(r.lines) == 0 || bytes.Count([]byte(body), []byte("\n")) != len(r.lines) {
			t.Fatalf("%s: the walk read %d lines and ended %v, want every line and its end", name, len(r.lines),
				r.ended)
		}
		for i, held := range r.held {
			if held != 0 {
				t.Errorf("%s: line %d was read with %d large slots held, want none", name, r.lines[i], held)
				break
			}
		}
	}
}

// A LONG LINE OF FEW FACTS READS WITHOUT THE SLOT: with the slot held
// elsewhere, a walk whose reading keeps records reads to its end a prompt and
// a line of blocks of an empty kind, each 16 times [scrubPieceBaseBytes] long,
// since what no reader reads of a line is passed over unheld and its length
// takes nothing.
//
// Mutation: hold the slot for a line by its length, as the scrub holds a
// piece ([lineRun]), and the walk waits.
func TestALongLineOfFewFactsReadsWithoutTheSlot(t *testing.T) {
	long := 16 * scrubPieceBaseBytes
	const empty = `{"type":""},`
	blocks := cntUser("u1", "u2", "["+strings.Repeat(empty, long/len(empty))+empty[:len(empty)-1]+"]")
	free := holdLargeSlot(t)
	defer free()
	for name, line := range map[string]string{"a prompt": slotUserOf(2, long), "a line of empty blocks": blocks} {
		ctx := newSlotWait(t.Context())
		var r slotReading
		done := make(chan struct{})
		go func() {
			defer close(done)
			walkThread(ctx, strings.NewReader(cntThread(slotUser(1, 0), line, slotUser(3, 0))), threadPlainMax,
				testPass(t), &r)
		}()
		select {
		case <-ctx.waited:
			free()
			<-done
			t.Fatalf("%s: a walk meeting a long line of few facts waits for the large slot", name)
		case <-done:
		}
		if r.ended != nil || len(r.lines) != 3 {
			t.Errorf("%s: the walk read %d lines and ended %v, want 3 and its end", name, len(r.lines), r.ended)
		}
	}
}

// A LINE TAKES THE SLOT BEFORE ITS FACTS KEEP MORE THAN THE BASE: with the
// slot held elsewhere, a gated reader reading a line of long ids and a short
// line of many facts, each charging past [collectionBaseLineCharge], waits
// having charged at most the base; its context ended while it waits, that
// Next and every one after it answer [errLargeWait] with the context's error,
// and the reader holds nothing.
//
// Mutation: count a charge before its hold, or hold at the line's commit, and
// the reader waits having charged past the base.
func TestALineTakesTheSlotBeforeItsFactsPassTheBase(t *testing.T) {
	for name, line := range map[string]string{
		"long ids":   slotUserCharging(t, 1, 2*collectionBaseLineCharge),
		"many facts": manyFacts(t, 1),
	} {
		free := holdLargeSlot(t)
		leaving, leave := context.WithCancel(t.Context())
		r := newThreadReader(strings.NewReader(line+"\n"), threadPlainMax)
		ctx := &chargeAtWait{Context: leaving, r: r, waited: make(chan struct{})}
		r.Gate(testPass(t).gate(ctx))
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err = r.Next(leaving)
		}()
		select {
		case <-ctx.waited:
		case <-done:
			t.Fatalf("%s: the reader read its line (%v) with the large slot held elsewhere", name, err)
		}
		if ctx.charged > collectionBaseLineCharge {
			t.Errorf("%s: the reader waits for the slot having charged %d bytes, past the base's %d", name,
				ctx.charged, collectionBaseLineCharge)
		}
		leave()
		<-done
		_, again := r.Next(t.Context())
		for _, got := range []error{err, again} {
			if !errors.Is(got, errLargeWait) || !errors.Is(got, context.Canceled) {
				t.Errorf("%s: a reader whose wait its context ended answers %v, want the wait's own error", name, got)
			}
		}
		r.dropGate()
		if len(collectionLargeReads) != 1 {
			t.Errorf("%s: once the reader's wait ended, %d large slots are held, want the case's one", name,
				len(collectionLargeReads))
		}
		free()
	}
}

// chargeAtWait is a case's context that notes, at its first Done (a wait for
// the slot, [takeSlot], on the reader's own goroutine), what r's line had
// charged its meter.
type chargeAtWait struct {
	context.Context
	r       *threadReader
	once    sync.Once
	waited  chan struct{}
	charged int
}

func (w *chargeAtWait) Done() <-chan struct{} {
	w.once.Do(func() {
		w.charged = w.r.meterMax - w.r.dec.meter.left
		close(w.waited)
	})
	return w.Context.Done()
}

// A LINE OR A CHAIN PAST THE BASE WAITS FOR THE LARGE SLOT: with the slot held
// elsewhere, a walk meeting a line whose facts charge one byte past
// [collectionBaseLineCharge], or a short line of many facts past it, waits
// before that line is handed on, and one whose reading keeps one record past
// [collectionBaseRecords] waits before the next line: a reading sees no later
// line. Freed, the walk takes the slot and reads on with it held; and a walk
// whose context ends while it waits ends with the wait's error, its count
// unread, holding nothing.
//
// Mutation: `>=` for `>` at either base, escalate after the line past the base
// is handed on, or hold a line by its length, and the walk waits at another
// line or reads the short line without the slot.
func TestALineOrAChainPastTheBaseWaitsForTheLargeSlot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		before int
	}{
		{"a line", cntThread(slotUser(1, 0), slotUserCharging(t, 2, collectionBaseLineCharge+1), slotUser(3, 0)), 1},
		{"a short line of many facts", cntThread(slotUser(1, 0), manyFacts(t, 2), slotUser(3, 0)), 1},
		{"a chain", slotChain(collectionBaseRecords + 2), collectionBaseRecords + 1},
	} {
		free := holdLargeSlot(t)
		ctx := newSlotWait(t.Context())
		large := testPass(t)
		var r slotReading
		done := make(chan struct{})
		go func() {
			defer close(done)
			walkThread(ctx, strings.NewReader(tc.body), threadPlainMax, large, &r)
		}()
		waits(t, ctx, done, tc.name)
		if len(r.lines) != tc.before {
			t.Errorf("%s: the walk waits for the slot having handed %d lines, want %d", tc.name, len(r.lines),
				tc.before)
		}
		free()
		<-done
		want := strings.Count(tc.body, "\n")
		if r.ended != nil || len(r.lines) != want {
			t.Fatalf("%s: freed, the walk read %d lines and ended %v, want %d and its end", tc.name, len(r.lines),
				r.ended, want)
		}
		for i := tc.before; i < len(r.held); i++ {
			if r.held[i] != 1 {
				t.Errorf("%s: line %d was read with %d large slots held, want the walk's one", tc.name, r.lines[i],
					r.held[i])
				break
			}
		}
		large.release()
		if len(collectionLargeReads) != 0 {
			t.Fatalf("%s: the pass gave back its slot and %d are held", tc.name, len(collectionLargeReads))
		}

		free = holdLargeSlot(t)
		leaving, leave := context.WithCancel(t.Context())
		ctx = newSlotWait(leaving)
		large = testPass(t)
		var (
			cut   slotReading
			count resumedReading
		)
		done = make(chan struct{})
		go func() {
			defer close(done)
			walkThread(ctx, strings.NewReader(tc.body), threadPlainMax, large, &cut, &count)
		}()
		waits(t, ctx, done, tc.name)
		leave()
		<-done
		if !errors.Is(cut.ended, errLargeWait) || !errors.Is(cut.ended, context.Canceled) || cut.ends != 1 ||
			len(cut.lines) != tc.before {
			t.Errorf("%s: a walk whose caller left while it waited read %d lines and ended %v (%d ends), want %d "+
				"and the wait's own error once", tc.name, len(cut.lines), cut.ended, cut.ends, tc.before)
		}
		wantMark(t, count.count(0), sandbox.CountUnread)
		large.release()
		if len(collectionLargeReads) != 1 {
			t.Errorf("%s: a walk that left while it waited leaves %d large slots held, want the case's own one",
				tc.name, len(collectionLargeReads))
		}
		free()
	}
}

// A CHAIN OF FEW RECORDS AND MANY CALLS PAST THE BASE KEEPS THE SLOT: with the
// slot held elsewhere, a walk whose chain files lines of 256 results each, every
// line's facts within [collectionBaseLineCharge], waits as the chain's items,
// its call ids among them, pass [collectionBaseRecords] at line 32, before the
// next line; freed, it reads every later line holding the slot.
//
// Mutation: count the chain's records alone ([chainReading.records]) and the
// walk reads every line with no slot.
func TestAChainOfManyCallsPastTheBaseKeepsTheSlot(t *testing.T) {
	const calls = 256
	before := collectionBaseRecords/(calls+1) + 1
	lines := make([]string, before+2)
	for k := range lines {
		lines[k] = slotSmallest(k+1, calls)
	}
	if c := lineCharge(t, lines[0]); c > collectionBaseLineCharge {
		t.Fatalf("a line of %d results charges %d bytes, past the base's %d", calls, c, collectionBaseLineCharge)
	}
	free := holdLargeSlot(t)
	ctx := newSlotWait(t.Context())
	var r slotReading
	done := make(chan struct{})
	go func() {
		defer close(done)
		walkThread(ctx, strings.NewReader(cntThread(lines...)), threadPlainMax, testPass(t),
			&chainReading{c: newThreadChain("")}, &r)
	}()
	waits(t, ctx, done, "a chain of many calls")
	if len(r.lines) != before {
		t.Errorf("the walk waits for the slot having handed %d lines, want %d", len(r.lines), before)
	}
	free()
	<-done
	if r.ended != nil || len(r.lines) != len(lines) {
		t.Fatalf("freed, the walk read %d lines and ended %v, want %d and its end", len(r.lines), r.ended,
			len(lines))
	}
	for i := before; i < len(r.held); i++ {
		if r.held[i] != 1 {
			t.Errorf("line %d was read with %d large slots held, want the walk's one", r.lines[i], r.held[i])
		}
	}
}

// waits holds the case to a walk run beside it, done as it ends, that waits on
// ctx for a slot before it ends.
func waits(t *testing.T, ctx *slotWait, done <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ctx.waited:
	case <-done:
		t.Fatalf("%s: the walk read to its end with the large slot held elsewhere", name)
	}
}

// slotBox is a box whose main thread's reads note the large slots held as each
// is asked, and that ends its reader's context once the thread's reader asks
// for more than cutAt bytes (0: never).
type slotBox struct {
	*sandbox.FakeSandbox
	cutAt  int
	cancel func()
	most   atomic.Int32
}

func (b *slotBox) OpenFile(ctx context.Context, p string) (io.ReadCloser, error) {
	body, err := b.FakeSandbox.OpenFile(ctx, p)
	if err != nil {
		return nil, err
	}
	return &slotBody{ReadCloser: body, box: b}, nil
}

type slotBody struct {
	io.ReadCloser
	box  *slotBox
	read int
}

func (s *slotBody) Read(p []byte) (int, error) {
	if held := int32(len(collectionLargeReads)); held > s.box.most.Load() {
		s.box.most.Store(held)
	}
	if s.box.cutAt > 0 && s.read > s.box.cutAt {
		s.box.cancel()
	}
	n, err := s.ReadCloser.Read(p)
	s.read += n
	return n, err
}

// A PASS TAKES THE LARGE SLOT ONCE AND GIVES IT BACK: a walk meeting both
// triggers, a chain past [collectionBaseRecords] and then a line whose facts
// charge past [collectionBaseLineCharge], holds one slot, never waiting for the
// one it holds; a collection's read past the base holds the slot as it reads and none
// once it returns, at the thread's end, at a bound and with its context ended;
// and a take's meter, whose walk holds the slot for its line past the base,
// holds none once it ends, however its archive ends: whole, unreadable, cut by
// a scrub that failed, or with its context ended.
//
// Mutation: take a second slot, or give back none where the read or the meter
// ends on an error, and a case goes red.
func TestAPassTakesTheLargeSlotOnceAndGivesItBack(t *testing.T) {
	if len(collectionLargeReads) != 0 {
		t.Fatal("the large slot is held before the case reads anything")
	}
	leaving, leave := context.WithCancel(t.Context())
	defer leave()
	ctx := &selfWait{Context: leaving, cancel: leave}
	large := testPass(t)
	var r slotReading
	body := slotChain(collectionBaseRecords+1,
		slotUserCharging(t, collectionBaseRecords+2, collectionBaseLineCharge+1), slotUser(collectionBaseRecords+3, 0))
	walkThread(ctx, strings.NewReader(body), threadPlainMax, large, &r)
	if ctx.waited.Load() || r.ended != nil || len(r.lines) != collectionBaseRecords+3 {
		t.Errorf("a pass meeting both triggers waited for its own slot %t and read %d lines, ending %v; want it "+
			"never to wait, every line and its end", ctx.waited.Load(), len(r.lines), r.ended)
	}
	if last := r.held[len(r.held)-1]; last != 1 {
		t.Errorf("a pass past both bases read its last line with %d large slots held, want one", last)
	}
	large.release()

	// TWICE THE BASE, so the box is read while a read that holds the slot for
	// that line alone holds it.
	long := slotUserCharging(t, 2, 2*collectionBaseLineCharge)
	// AFTER IT, more than the reader's buffer holds, so the read is still
	// asking for lines once its context ends.
	after := make([]string, 0, 254)
	for k := 3; k <= 256; k++ {
		after = append(after, slotUser(k, 1<<10))
	}
	// PAST A BOUND as the slot is held for it: long with its uuid written again
	// after its facts, read whole, past [threadValueMax].
	bound := strings.TrimSuffix(long, "}") + `,"uuid":"` + strings.Repeat("u", threadValueMax) + `"}`
	for _, tc := range []struct {
		name  string
		body  string
		cutAt int
		mark  sandbox.CountMark
	}{
		{"at the thread's end", cntThread(slotUser(1, 0), long, cntAnchor("u2", "u3", "m1", 100, 0, 0, 50)), 0, ""},
		{"at a bound", cntThread(slotUser(1, 0), long, bound), 0, sandbox.CountUnread},
		{"its context ended", cntThread(append([]string{slotUser(1, 0), long}, after...)...),
			len(slotUser(1, 0)) + len(long) + 2, sandbox.CountUnread},
	} {
		leaving, leave := context.WithCancel(t.Context())
		fake, paths := cntBox(tc.body, nil)
		box := &slotBox{FakeSandbox: fake, cutAt: tc.cutAt, cancel: leave}
		got := ClaudeCode{}.readMainThread(leaving, box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: "2.1.322", Count: true})
		leave()
		if got.Count == nil || got.Count.Mark != tc.mark {
			t.Errorf("%s: the read counts %+v, want the mark %q", tc.name, got.Count, tc.mark)
		}
		if box.most.Load() != 1 || len(collectionLargeReads) != 0 {
			t.Errorf("%s: the read held at most %d large slots as it read and %d once it returned, want one and "+
				"none", tc.name, box.most.Load(), len(collectionLargeReads))
		}
	}

	archive := cntArchive(t, cntThread(slotUser(1, 0), long, cntAnchor("u2", "u3", "m1", 100, 0, 0, 50)))
	for _, tc := range []struct {
		name    string
		written []byte
		failed  error
		cut     bool
	}{
		{"whole", archive, nil, false},
		{"unreadable", []byte("no gzip at all"), nil, false},
		{"cut by a scrub that failed", archive[:len(archive)/2], errors.New("the scrub failed"), false},
		{"its context ended", archive, nil, true},
	} {
		leaving, leave := context.WithCancel(t.Context())
		if tc.cut {
			leave()
		}
		took := new(passBudget)
		meter := newContextMeter(leaving, ClaudeCode{}, ctSession, "2.1.322", took)
		_, _ = meter.Write(tc.written)
		meter.finish(tc.failed, 0)
		meter.count()
		leave()
		if len(collectionLargeReads) != 0 {
			t.Errorf("%s: once the meter ended, %d large slots are held, want none", tc.name, len(collectionLargeReads))
			took.release()
		}
	}
}

// TWO READERS OF ONE PASS SHARE ITS ONE SLOT, EACH WAITING WITHIN ITS OWN
// CONTEXT: a take's scrub and its meter. With the slot held elsewhere, a
// reader that asks while the other's wait is in flight waits on that wait, and
// leaves it when its own context ends, the other still waiting; once the other
// takes the slot, the pass holds that one, which a later ask is handed at once.
//
// Mutation: hold the pass's lock through the wait for the slot, and the reader
// whose context ended waits on regardless.
func TestTwoReadersOfOnePassShareItsSlotEachWithinItsOwnContext(t *testing.T) {
	free := holdLargeSlot(t)
	large := testPass(t)
	first := newSlotWait(t.Context())
	var firstErr error
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		firstErr = large.hold(first)
	}()
	<-first.waited
	leaving, leave := context.WithCancel(t.Context())
	second := newSlotWait(leaving)
	secondDone := make(chan error, 1)
	go func() { secondDone <- large.hold(second) }()
	<-second.waited
	leave()
	if err := <-secondDone; !errors.Is(err, context.Canceled) {
		t.Errorf("a reader that left while the pass's wait was in flight ended %v, want its own error", err)
	}
	free()
	<-firstDone
	if firstErr != nil || len(collectionLargeReads) != 1 {
		t.Fatalf("freed, the pass's wait ended %v holding %d large slots, want the one", firstErr,
			len(collectionLargeReads))
	}
	if err := large.hold(t.Context()); err != nil || len(collectionLargeReads) != 1 {
		t.Errorf("a later ask of the pass answered %v with %d large slots held, want the pass's one", err,
			len(collectionLargeReads))
	}
	large.release()
	if len(collectionLargeReads) != 0 {
		t.Errorf("the pass gave back its slot and %d are held", len(collectionLargeReads))
	}
}

// heldScrub is a take's scrub of archive within bounds gated by a pass whose
// slot waits within ctx, run beside the case: done closes as it ends, with its
// error in *err.
func heldScrub(ctx context.Context, archive []byte, large *passBudget, err *error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		*err = scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), io.Discard, newScrubber(nil),
			testBounds.gatedBy(ctx, large))
	}()
	return done
}

// heldExport is [heldScrub] of an export, one gzipped document.
func heldExport(ctx context.Context, export []byte, large *passBudget, err *error) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		*err = scrubDocument(bytes.NewReader(export), reopenOf(export), io.Discard, newScrubber(nil),
			testBounds.gatedBy(ctx, large))
	}()
	return done
}

// A TYPICAL TAKE'S SCRUB TAKES NO SLOT: with the large slot held elsewhere,
// the scrub of an archive holding the probe's two threads, a transcript's line
// at [scrubPieceBaseBytes], a document's string at it and a text that
// holds a key's stretch below it reads to its end without waiting for it.
//
// Mutation: gate a piece at the base (`>=` for `>`), or every piece, and the
// scrub waits.
func TestATypicalTakesScrubTakesNoSlot(t *testing.T) {
	files := map[string]string{
		"projects/p/s.jsonl":                slotUserOf(1, scrubPieceBaseBytes) + "\n",
		"tasks/s/1.json":                    `{"text":"` + strings.Repeat("a", scrubPieceBaseBytes-11) + `"}`,
		"projects/p/s/tool-results/out.txt": "-----BEGIN RSA PRIVATE KEY-----\n" + keyLine + "\n",
	}
	for _, file := range []string{
		"testdata/claude/2.1.310/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000106.jsonl",
		"testdata/claude/2.1.322/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000113.jsonl",
	} {
		raw, err := os.ReadFile(filepath.FromSlash(file))
		if err != nil {
			t.Fatal(err)
		}
		files["projects/p/"+filepath.Base(file)] = string(raw)
	}
	// A STRING AT THE BASE: the decoder holds its opening quote with its text.
	export := gzipOf(t, `{"text":"`+strings.Repeat("a", scrubPieceBaseBytes-1)+`"}`)
	free := holdLargeSlot(t)
	defer free()
	for name, scrub := range map[string]func(context.Context, *passBudget, *error) <-chan struct{}{
		"an archive": func(ctx context.Context, large *passBudget, err *error) <-chan struct{} {
			return heldScrub(ctx, tarOf(t, files), large, err)
		},
		"an export whose string is at the base": func(ctx context.Context, large *passBudget,
			err *error) <-chan struct{} {
			return heldExport(ctx, export, large, err)
		},
	} {
		ctx := newSlotWait(t.Context())
		var err error
		done := scrub(ctx, testPass(t), &err)
		select {
		case <-ctx.waited:
			t.Errorf("%s: a typical take's scrub waits for the large slot", name)
			free()
			<-done
		case <-done:
			if err != nil {
				t.Errorf("%s: a typical take's scrub: %v", name, err)
			}
		}
	}
}

// fillSource hands on at most one 64 KiB fill a read, counting what it handed.
type fillSource struct {
	r    io.Reader
	read int
}

func (s *fillSource) Read(p []byte) (int, error) {
	n, err := s.r.Read(p[:min(len(p), 64<<10)])
	s.read += n
	return n, err
}

// readAtWait is a case's context that notes, at its first Done (a wait for
// the slot, [takeSlot]), how much its source had handed on.
type readAtWait struct {
	context.Context
	src    *fillSource
	once   sync.Once
	waited chan struct{}
	read   int
}

func (w *readAtWait) Done() <-chan struct{} {
	w.once.Do(func() {
		w.read = w.src.read
		close(w.waited)
	})
	return w.Context.Done()
}

// A TAKE'S PIECE PAST THE BASE WAITS FOR THE LARGE SLOT BEFORE IT IS HELD:
// a transcript's line twice [scrubPieceBaseBytes], an export's string, a
// `.json` file and a text of short lines as long, each a text the first
// reading redacts as one (the second copies it), wait for the slot held
// elsewhere having read at most the base and two fills (the source's, and the
// scanner's or the text's read ahead) past where the piece began; freed, the
// rewrite reads to its end and holds nothing once done; with its context
// ended while it waits, it ends with [errLargeWait].
//
// Mutation: gate no piece, a line only once it is read whole, or a text by its
// lines, drop a piece's gate, or keep a hold past the rewrite, and a case goes
// red.
func TestATakesPiecePastTheBaseWaitsForTheLargeSlot(t *testing.T) {
	long := strings.Repeat("a", 2*scrubPieceBaseBytes)
	prefix := `{"text":"`
	line := prefix + long + `"}` + "\n"
	lines := strings.Repeat(keyLine+"\n", 2*scrubPieceBaseBytes/(len(keyLine)+1))
	type rewrite func(io.Writer, io.Reader, scrubber, scrubBounds) error
	pieces := []struct {
		name    string
		body    string
		rewrite rewrite
	}{
		{"a transcript's line", line, func(_ io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
			plan := spooledPlan(t)
			plan.gate = b.gate
			_, err := measureLines(src, s, plan)
			return err
		}},
		{"an export's string", prefix + long + `"}`, func(dst io.Writer, src io.Reader, s scrubber,
			b scrubBounds) error {
			return rewriteDocument(dst, src, s, b.gate)
		}},
		{"a `.json` file", prefix + long + `"}`, func(_ io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
			plan := spooledPlan(t)
			plan.gate = b.gate
			_, err := measureFile("tasks/s/1.json", src, s, plan)
			return err
		}},
		{"a text of short lines", lines, func(dst io.Writer, src io.Reader, s scrubber, b scrubBounds) error {
			return rewriteText(dst, src, s, b.gate)
		}},
	}
	most := len(prefix) + scrubPieceBaseBytes + 2*(64<<10)
	for _, tc := range pieces {
		for _, leaves := range []bool{false, true} {
			free := holdLargeSlot(t)
			leaving, leave := context.WithCancel(t.Context())
			src := &fillSource{r: strings.NewReader(tc.body)}
			ctx := &readAtWait{Context: leaving, src: src, waited: make(chan struct{})}
			large := new(passBudget)
			var err error
			done := make(chan struct{})
			go func() {
				defer close(done)
				err = tc.rewrite(io.Discard, src, newScrubber(nil), testBounds.gatedBy(ctx, large))
			}()
			select {
			case <-ctx.waited:
			case <-done:
				t.Errorf("%s: the rewrite read to its end (%v) with the large slot held elsewhere", tc.name, err)
				leave()
				free()
				continue
			}
			if ctx.read > most {
				t.Errorf("%s: the rewrite waits for the slot having read %d bytes, past the %d of the base and "+
					"two fills", tc.name, ctx.read, most)
			}
			if leaves {
				leave()
			} else {
				free()
			}
			<-done
			switch {
			case leaves && (!errors.Is(err, errLargeWait) || !errors.Is(err, context.Canceled)):
				t.Errorf("%s: a rewrite whose caller left while it waited ended %v, want the wait's own error",
					tc.name, err)
			case !leaves && err != nil:
				t.Errorf("%s: freed, the rewrite ended %v", tc.name, err)
			}
			leave()
			free()
			if len(collectionLargeReads) != 0 {
				t.Errorf("%s: once the rewrite ended, %d large slots are held, want none", tc.name,
					len(collectionLargeReads))
				large.release()
			}
		}
	}
}

// THE PLAN PAST THE BASE HOLDS THE SLOT TO THE TAKE'S END: a tar's plan one
// bit short of a byte past [scrubPieceBaseBytes] of bits takes no slot as the
// first reading plans a transcript's line that loses a loaded tool
// definition; one bit further it takes it, and holds it once the lines are
// read, until the take lets it go.
//
// Mutation: hold the plan at the base (`>=` for `>`), plan no hold in a
// transcript's lines, or drop it with the lines, and a case goes red.
func TestThePlanPastTheBaseHoldsTheSlotToTheTakesEnd(t *testing.T) {
	s := newScrubber([]string{ltServer})
	line := ltRecord("2.1.310", "", ltEcho, ltRenamed) + "\n"
	measured := spooledPlan(t)
	if _, err := measureLines(strings.NewReader(line), s, measured); err != nil {
		t.Fatal(err)
	}
	cut := measured.edits.count()
	if cut == 0 {
		t.Fatal("the line planned no cut")
	}
	for _, tc := range []struct {
		name  string
		bits  int
		holds bool
	}{
		{"a plan a bit short of past the base", (scrubPieceBaseBytes+1)*8 - 1 - cut, false},
		{"a plan past the base", (scrubPieceBaseBytes+1)*8 - cut, true},
	} {
		large := testPass(t)
		plan := spooledPlan(t)
		plan.gate = large.gate(t.Context())
		for range tc.bits {
			plan.members.put(false)
		}
		if _, err := measureLines(strings.NewReader(line), s, plan); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if held := len(collectionLargeReads) == 1; held != tc.holds {
			t.Errorf("%s: once its lines are read, the plan holds the large slot: %v, want %v", tc.name, held,
				tc.holds)
		}
		plan.letGo()
		if len(collectionLargeReads) != 0 {
			t.Errorf("%s: once the take lets the plan go, %d large slots are held, want none", tc.name,
				len(collectionLargeReads))
			large.release()
		}
	}
}

// heldReading is a reading that keeps no records and notes the large slots
// held as it is handed each line.
type heldReading struct{ held []int }

func (r *heldReading) line(threadLine)          { r.held = append(r.held, len(collectionLargeReads)) }
func (r *heldReading) refused(*threadLineError) { r.held = append(r.held, len(collectionLargeReads)) }
func (r *heldReading) end(bool, error)          {}

// A WALK THAT KEEPS NO RECORDS HOLDS THE SLOT FOR ITS LINE ALONE, as a take's
// meter and a collection's count walk: a line whose facts charge past
// [collectionBaseLineCharge] is handed on with the slot held, every line after
// it with none, and nothing is held once the walk ends.
//
// Mutation: keep every hold to the pass's end, or drop it later than the next
// line, and a later line is read with the slot held.
func TestAWalkThatKeepsNoRecordsHoldsTheSlotForItsLineAlone(t *testing.T) {
	lines := []string{slotUser(1, 0), slotUserCharging(t, 2, collectionBaseLineCharge+1), slotUser(3, 0),
		slotUser(4, 1<<10)}
	var r heldReading
	large := new(passBudget)
	defer large.release()
	walkThread(t.Context(), strings.NewReader(cntThread(lines...)), threadPlainMax, large, &r)
	if len(r.held) != len(lines) {
		t.Fatalf("the walk handed %d lines, want %d", len(r.held), len(lines))
	}
	if r.held[1] != 1 || slices.Max(r.held[2:]) != 0 {
		t.Errorf("the walk handed its lines with %v large slots held, want one for the second alone", r.held)
	}
	if len(collectionLargeReads) != 0 {
		t.Errorf("the walk ended holding %d large slots, want none", len(collectionLargeReads))
	}
}

// doneCalls is a case's context that signals every look at its Done.
type doneCalls struct {
	context.Context
	calls chan struct{}
}

func (w *doneCalls) Done() <-chan struct{} {
	select {
	case w.calls <- struct{}{}:
	default:
	}
	return w.Context.Done()
}

// A READER WHOSE SHARED WAIT ENDS WAITS ON ITS OWN: where the reader that
// began a pass's wait for the slot leaves first (a take's scrub closed while
// its meter waits), the other, waiting on that wait within a live context,
// begins its own and holds the slot once the case frees it, never proceeding
// without it.
//
// Mutation: take the end of the shared wait for the slot, and the second
// reader returns holding nothing.
func TestAReaderWhoseSharedWaitEndsWaitsOnItsOwn(t *testing.T) {
	free := holdLargeSlot(t)
	large := new(passBudget)
	defer large.release()
	leaving, leave := context.WithCancel(t.Context())
	defer leave()
	first := newSlotWait(leaving)
	firstDone := make(chan error, 1)
	go func() { firstDone <- large.hold(first) }()
	<-first.waited
	second := &doneCalls{Context: t.Context(), calls: make(chan struct{}, 4)}
	secondDone := make(chan error, 1)
	go func() { secondDone <- large.hold(second) }()
	<-second.calls
	leave()
	if err := <-firstDone; !errors.Is(err, errLargeWait) || !errors.Is(err, context.Canceled) {
		t.Errorf("the reader that left ended %v, want the wait's own error", err)
	}
	select {
	case <-second.calls:
	case err := <-secondDone:
		t.Fatalf("the reader still asking answered %v once the wait it shared ended, holding %d large slots",
			err, len(collectionLargeReads))
	}
	if len(collectionLargeReads) != 1 {
		t.Errorf("while the second reader waits, %d large slots are held, want the case's one",
			len(collectionLargeReads))
	}
	free()
	if err := <-secondDone; err != nil || len(collectionLargeReads) != 1 {
		t.Fatalf("freed, the second reader answered %v holding %d large slots, want the pass's one", err,
			len(collectionLargeReads))
	}
	large.drop()
	if len(collectionLargeReads) != 0 {
		t.Errorf("the pass dropped its one hold and %d large slots are held", len(collectionLargeReads))
	}
}

// WHAT A COLLECTION'S PASS COSTS AT ITS BASE, the figures the package doc's "A
// collection's read of a thread" states the bases by: a main thread whose
// chain files [collectionBaseRecords] items, each a record but for the call
// ids of its last two lines: a user's line growing every scratch buffer of its
// reader to its most ([grownScratch]) beside facts charging
// [collectionBaseLineCharge] exactly in the shape that holds the most a
// charged byte ([resultsCharging]), and a reply the count anchors on; read by
// the chain and the count in one walk, under a pass that must take no slot,
// what is held at most read off the GC trace as
// BenchmarkAThreadChainAtItsBounds says:
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkACollectionPassAtItsBase$' -benchtime 1x -benchmem
func BenchmarkACollectionPassAtItsBase(b *testing.B) {
	results := func(n int) string {
		grown, block, result := grownScratch(benchOf(n-2), benchInvalid(threadValueMax-2))
		head, tail, _ := strings.Cut(benchRecord(benchOf(n-2), benchMain, "user",
			grown+`"message":{"role":"user","content":[`+block+`,@]},`+result, n-1, n-1), "@")
		line := resultsCharging(b, head, tail, collectionBaseLineCharge)
		scratchGrown(b, line, 3*(threadValueMax-2))
		return line
	}
	// The chain files each result's id and the anchor's call beside the records.
	calls := strings.Count(results(collectionBaseRecords), `"tool_result"`) + 1
	n := collectionBaseRecords - calls
	t := benchWrite(b, filepath.Join(b.TempDir(), "thread.jsonl"), func(t *benchThread) {
		for k := 1; k < n-1; k++ {
			parent := benchRoot
			if k > 1 {
				parent = benchOf(k - 1)
			}
			t.record(benchRecord(parent, benchMain, "assistant", benchReply(fmt.Sprintf("msg_%08d", k),
				`[{"type":"text","text":"done"}]`), k, k))
		}
		line := results(n)
		if got := strings.Count(line, `"tool_result"`) + 1; got != calls {
			b.Fatalf("the user's line carries %d calls with the anchor's, want %d", got, calls)
		}
		t.record(line)
		t.record(benchRecord(benchOf(n-1), benchMain, "assistant", `"message":{"id":"msg_anchor",`+
			`"model":"claude-sonnet-4-5","usage":{`+benchUsage+`},"type":"message","role":"assistant","content":`+
			benchCall(fmt.Sprintf("toolu_%08d", n))+`,"stop_reason":"tool_use"},`, n, n))
	})
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fmt.Fprintf(os.Stderr, "base-bench: the read starts\n")
	b.ResetTimer()
	var read benchRead
	for range b.N {
		read = benchReadThread(b, t.file, "", true)
	}
	b.StopTimer()
	runtime.GC()
	runtime.ReadMemStats(&after)
	fmt.Fprintf(os.Stderr, "base-bench: the read ends\n")
	b.ReportMetric(float64(t.bytes)/(1<<20), "thread-MiB")
	b.ReportMetric(float64(read.c.items()), "items")
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), "live-MiB")
	benchCountRead(b, read)
	if read.c.items() != collectionBaseRecords || read.c.past || read.count.tokens != 2 || read.escalated {
		b.Fatalf("the chain holds %d items, past its bound %t, the count anchors at %d and the pass took the "+
			"large slot %t; want %d, false, 2 and false", read.c.items(), read.c.past, read.count.tokens,
			read.escalated, collectionBaseRecords)
	}
	runtime.KeepAlive(read)
}

// inABubble runs f in a synctest bubble whose large-slot pool is the bubble's
// own, so a take waiting for the slot is one the bubble sees durably blocked
// and a take that never ends is a deadlock it reports, never a hung case.
func inABubble(t *testing.T, f func(t *testing.T)) {
	t.Helper()
	synctest.Test(t, func(t *testing.T) {
		outer, outerTakes := collectionLargeReads, sessionTakes
		collectionLargeReads = make(chan struct{}, CollectionLargeReadsAtOnce)
		sessionTakes = make(chan struct{}, SessionTakesAtOnce)
		t.Cleanup(func() { collectionLargeReads, sessionTakes = outer, outerTakes })
		f(t)
	})
}

// slotTake is a take of a box whose capture is archive, of ctSession's
// session, written by Claude Code 2.1.322.
func slotTake(t *testing.T, archive []byte) sandbox.TakenSession {
	t.Helper()
	taken, err := takeWithin(t.Context(), archive)
	if err != nil || taken.Body == nil {
		t.Fatalf("the take: %v", err)
	}
	return taken
}

// takeWithin is [slotTake]'s take within ctx, its error the caller's.
func takeWithin(ctx context.Context, archive []byte) (sandbox.TakenSession, error) {
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	box.Put(p.SessionRecord(), "id "+ctSession+"\ncontinued no\nrelease 2.1.322 (Claude Code)\n")
	box.Put(p.SessionCapture(), string(archive))
	return NewClaudeCode().TakeSession(ctx, box, sandbox.SessionTake{MaxBytes: 32 << 20,
		Spool: sandbox.SessionSpool{Dir: os.TempDir(), Bytes: math.MaxInt64, Held: func(int64) {}}})
}

// countedWriter counts what is written to it.
type countedWriter struct{ n atomic.Int64 }

func (w *countedWriter) Write(p []byte) (int, error) {
	w.n.Add(int64(len(p)))
	return len(p), nil
}

// A TAKE HOLDS THE LARGE SLOT ONLY WHILE IT HOLDS A PIECE PAST THE BASE, its
// scrub and its meter one pass: run through [Runner.TakeSession] over a box
// whose capture holds a main-thread line and a later member's line each past
// [scrubPieceBaseBytes]. With the slot held by the case, the body yields
// no byte and its Close returns, ending everything the take started, the case
// still holding the slot; freed, the body reads to its end, the count reads
// the whole thread, and nothing of the take holds the slot afterwards.
//
// Mutation: leave the take's scrub ungated, keep its hold past a piece, or
// let Close leave the scrub's wait, and a case goes red or deadlocks.
func TestATakeHoldsTheLargeSlotOnlyWhileItHoldsAPiecePastTheBase(t *testing.T) {
	long := `"` + strings.Repeat("a", 2*scrubPieceBaseBytes) + `"`
	later := tarMember{h: tar.Header{Name: "projects/p/s/tool-results/out.txt", Mode: 0o600,
		Typeflag: tar.TypeReg}, body: strings.Repeat("b", 2*scrubPieceBaseBytes) + "\n"}
	archive := cntArchive(t, cntThread(cntUser("", "u1", long), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50)), later)
	inABubble(t, func(t *testing.T) {
		free := holdLargeSlot(t)
		taken := slotTake(t, archive)
		var yielded countedWriter
		reading := make(chan error, 1)
		go func() {
			_, err := io.Copy(&yielded, taken.Body)
			reading <- err
		}()
		synctest.Wait()
		if n := yielded.n.Load(); n != 0 {
			t.Errorf("with the large slot held elsewhere, the take yielded %d bytes, want none", n)
		}
		_ = taken.Body.Close()
		<-reading
		if count := taken.Count(); count == nil || count.Mark != sandbox.CountUnread {
			t.Errorf("a take closed while it waited counts %+v, want it unread", count)
		}
		if len(collectionLargeReads) != 1 {
			t.Errorf("once the take was closed, %d large slots are held, want the case's one",
				len(collectionLargeReads))
		}
		free()
	})
	inABubble(t, func(t *testing.T) {
		taken := slotTake(t, archive)
		if _, err := io.Copy(io.Discard, taken.Body); err != nil {
			t.Fatalf("the take, the slot free: %v", err)
		}
		count := taken.Count()
		_ = taken.Body.Close()
		if count == nil || count.Mark != "" {
			t.Errorf("a take read whole counts %+v, want the whole thread read", count)
		}
		if len(collectionLargeReads) != 0 {
			t.Errorf("once the take was read and counted, %d large slots are held, want none",
				len(collectionLargeReads))
		}
	})
}

// A TAKE'S CLOSE ENDS ITS METER'S WAIT FOR THE SLOT: a line the scrub holds
// within [scrubPieceBaseBytes] whose facts charge past
// [collectionBaseLineCharge] ([manyFacts]), so the meter alone waits for the
// slot held by the case, and the scrub waits on the meter. Closed before its
// end, the take returns, its meter ended with the count unread, the case
// still holding the slot.
//
// Mutation: wait the meter within the take's context alone, and the close
// waits for the slot: the bubble deadlocks.
func TestATakesCloseEndsItsMetersWaitForTheSlot(t *testing.T) {
	rnd := rand.New(rand.NewPCG(1, 2))
	noise := func(n int) string {
		raw := make([]byte, n)
		for i := range raw {
			raw[i] = byte(rnd.Uint32())
		}
		return base64.StdEncoding.EncodeToString(raw)[:n]
	}
	// AFTER IT, lines the scrub writes past what the meter's readers buffer.
	lines := []string{manyFacts(t, 1)}
	for k := 2; k <= 5; k++ {
		lines = append(lines, cntUser(fmt.Sprintf("u%d", k-1), fmt.Sprintf("u%d", k), `"`+noise(128<<10)+`"`))
	}
	archive := cntArchive(t, cntThread(lines...))
	inABubble(t, func(t *testing.T) {
		free := holdLargeSlot(t)
		taken := slotTake(t, archive)
		reading := make(chan error, 1)
		go func() {
			_, err := io.Copy(io.Discard, taken.Body)
			reading <- err
		}()
		synctest.Wait()
		select {
		case err := <-reading:
			t.Fatalf("the take read to its end (%v) while its meter waited for the slot", err)
		default:
		}
		_ = taken.Body.Close()
		<-reading
		if count := taken.Count(); count == nil || count.Mark != sandbox.CountUnread {
			t.Errorf("a take closed while its meter waited counts %+v, want it unread", count)
		}
		if len(collectionLargeReads) != 1 {
			t.Errorf("once the take was closed, %d large slots are held, want the case's one",
				len(collectionLargeReads))
		}
		free()
	})
}

// NO MORE TAKES SCRUB AT ONCE THAN [SessionTakesAtOnce]: with every slot held
// by a take nothing reads, one more waits within its ctx, and takes the slot a
// closed take gives back; one whose ctx ends while it waits is refused
// ([errTakeWait]), holding none.
//
// Mutation: take no slot, and the take past the pool returns at once.
func TestNoMoreTakesScrubAtOnceThanThePoolAllows(t *testing.T) {
	archive := cntArchive(t, cntThread(cntUser("", "u1", `"hello"`)))
	inABubble(t, func(t *testing.T) {
		held := make([]sandbox.TakenSession, 0, SessionTakesAtOnce)
		for range SessionTakesAtOnce {
			held = append(held, slotTake(t, archive))
		}
		if n := len(sessionTakes); n != SessionTakesAtOnce {
			t.Fatalf("%d takes in flight hold %d slots, want one each", SessionTakesAtOnce, n)
		}
		refused, cancel := context.WithCancel(t.Context())
		gaveUp := make(chan error, 1)
		go func() {
			_, err := takeWithin(refused, archive)
			gaveUp <- err
		}()
		type result struct {
			taken sandbox.TakenSession
			err   error
		}
		next := make(chan result, 1)
		go func() {
			taken, err := takeWithin(t.Context(), archive)
			next <- result{taken, err}
		}()
		synctest.Wait()
		select {
		case r := <-next:
			t.Fatalf("a take past the pool's %d returned (%v) while every slot was held",
				SessionTakesAtOnce, r.err)
		case err := <-gaveUp:
			t.Fatalf("a take past the pool returned (%v) before its wait ended", err)
		default:
		}
		cancel()
		if err := <-gaveUp; !errors.Is(err, errTakeWait) {
			t.Errorf("a take whose wait ended: %v, want errTakeWait", err)
		}
		_ = held[0].Body.Close()
		r := <-next
		if r.err != nil || r.taken.Body == nil {
			t.Fatalf("the take given a slot back: %v", r.err)
		}
		_ = r.taken.Body.Close()
		for _, h := range held[1:] {
			_ = h.Body.Close()
		}
		if n := len(sessionTakes); n != 0 {
			t.Errorf("once every take was closed, %d slots are held, want none", n)
		}
	})
}

// WHAT A TAKE HOLDS AT ITS LARGE TIER, the figure the package doc's "A
// collection's read of a thread" states beside the collection's pass: a kept
// archive whose main thread holds a line of [threadLineMax] bytes, whose
// `.json` member holds a string as long, each of bytes no UTF-8 holds, which
// the decode reads as U+FFFD, three bytes a byte, and whose text file is as
// long; scrubbed, both readings, with its meter fed as [Runner.TakeSession]
// feeds it, under one budget. What is held at most is read off the runtime's
// GC trace as BenchmarkAThreadChainAtItsBounds says:
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkATakeAtItsLargeTier$' -benchtime 1x -benchmem
func BenchmarkATakeAtItsLargeTier(b *testing.B) {
	benchmarkATake(b, takeOfPieces(b, threadLineMax))
}

// WHAT A TAKE'S SCRUB HOLDS AT ITS BASE: as [BenchmarkATakeAtItsLargeTier],
// its line and its string at [scrubPieceBaseBytes], beside a text file at it.
// The base tier counts a take at this and [BenchmarkAMeterAtItsBase] summed:
// the meter's worst line is past the scrub's base, so the two never meet on
// one line, but the scrub may read on at its base while the meter is still on
// that line. Read off its held-MiB, in live bytes ([atLiveBytes]), which the
// pool is sized by:
//
//	GODEBUG=gcstoptheworld=1 go test -run '^$' -bench '^BenchmarkATakeAtItsBase$' -benchtime 1x -benchmem
func BenchmarkATakeAtItsBase(b *testing.B) {
	atLiveBytes(b)
	benchmarkATake(b, takeOfPieces(b, scrubPieceBaseBytes))
}

// WHAT A TAKE'S METER HOLDS AT ITS BASE: the meter alone, fed what the scrub
// keeps of a take whose main thread holds [meterAtItsBase]'s line, so the
// figure is the meter's, not the scrub's hold of that line past its own base,
// which the large tier counts. Read off its held-MiB, as that is:
//
//	GODEBUG=gcstoptheworld=1 go test -run '^$' -bench '^BenchmarkAMeterAtItsBase$' -benchtime 1x -benchmem
func BenchmarkAMeterAtItsBase(b *testing.B) {
	atLiveBytes(b)
	archive := takeOfPieces(b, 1<<10, meterAtItsBase(b))
	var kept bytes.Buffer
	if err := scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), &kept, newScrubber(nil),
		testBounds); err != nil {
		b.Fatalf("the take: %v", err)
	}
	takeWindow(b, func() {
		meter := newContextMeter(b.Context(), ClaudeCode{}, ctSession, "2.1.322", new(passBudget))
		_, _ = meter.Write(kept.Bytes())
		meter.finish(nil, 0)
		meter.count()
	})
}

// meterAtItsBase is a main-thread line under u1 holding what a take's meter
// holds at its base at most (the package doc's THE BASE), as the scrub hands
// it on: every scratch buffer grown and the type's text tripled
// ([grownScratch]), results whose facts charge [collectionBaseLineCharge],
// then a tool_use whose id and name are each [threadValueMax] bytes no UTF-8
// holds, decoded at three bytes a byte before their charge passes the base,
// then a byte past the value, so the scrub keeps the line as text, verbatim.
// tb fails where the scrub keeps it otherwise or the meter charges otherwise.
func meterAtItsBase(tb testing.TB) string {
	bad := benchInvalid(threadValueMax - 2)
	head, block, result := grownScratch(`"u1"`, bad)
	calls := 2 * 3 * len(bad)
	// Read as a record, its head's three strings decode at three bytes a byte.
	record := resultsCharging(tb, `{`+head+result+`"message":{"role":"user","content":[`+block+`,`,
		`,{"type":"tool_use","id":"`+bad+`","name":"`+bad+`","input":{}}]},"type":"`+bad+`","subtype":"`+
			bad+`","agentId":"`+bad+`","uuid":"u2"}`, collectionBaseLineCharge+calls+3*3*len(bad)+len("u1"))
	scratchGrown(tb, record, 3*len(bad))
	line := record + "x"
	if rewriteLine(tb, line, newScrubber(nil)) != line {
		tb.Fatal("the scrub keeps the meter's line otherwise than it is built")
	}
	r := newThreadReader(strings.NewReader(line+"\n"), threadPlainMax)
	var refused *threadLineError
	if _, err := r.Next(tb.Context()); !errors.As(err, &refused) {
		tb.Fatalf("the meter's line reads %v, want it refused as no record", err)
	}
	if facts := r.meterMax - r.dec.meter.left - calls; facts > collectionBaseLineCharge ||
		facts < collectionBaseLineCharge-1<<10 {
		tb.Fatalf("the meter's line charges %d bytes before its call, want the base, %d", facts,
			collectionBaseLineCharge)
	}
	return line
}

// benchmarkATake scrubs archive as [Runner.TakeSession] does, its meter fed
// beside it under one budget.
func benchmarkATake(b *testing.B, archive []byte) {
	var kept countedWriter
	takeWindow(b, func() {
		large := new(passBudget)
		meter := newContextMeter(b.Context(), ClaudeCode{}, ctSession, "2.1.322", large)
		err := scrubArchive(archiveTar, bytes.NewReader(archive), reopenOf(archive), io.MultiWriter(&kept, meter),
			newScrubber(nil), testBounds.gatedBy(b.Context(), large))
		meter.finish(err, 0)
		meter.count()
		if err != nil {
			b.Fatalf("the take: %v", err)
		}
	})
	b.ReportMetric(float64(kept.n.Load())/float64(b.N)/(1<<20), "kept-MiB")
}

// atLiveBytes has b read what a take holds in live bytes: each GC stops the
// world to mark, so nothing allocated meanwhile is counted live, and runs at
// every 1% the heap grows (GOGC=1), so a buffer a call allocates and drops
// inside one read is caught within 1% of the heap. A concurrent mark counts
// what is allocated during it as live: on a slot where a mark ran long, main's
// take read 13.85 MiB in 6 runs of 30, against at most 8.02 here. GODEBUG is
// read as the process starts, so b refuses to run without gcstoptheworld=1.
func atLiveBytes(b *testing.B) {
	b.Helper()
	if !strings.Contains(os.Getenv("GODEBUG"), "gcstoptheworld=1") {
		b.Fatal("a take's base is read in live bytes: run it with GODEBUG=gcstoptheworld=1")
	}
	gogc := debug.SetGCPercent(1)
	b.Cleanup(func() { debug.SetGCPercent(gogc) })
}

// takeWindow runs op b.N times between the markers a take's GC trace is read
// between, reporting what it left live and, as held-MiB, the most live heap a
// GC marked meanwhile ([liveHeapMost]); b fails where it left a large slot
// held.
func takeWindow(b *testing.B, op func()) {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	held := watchLiveHeap()
	fmt.Fprintf(os.Stderr, "take-bench: the take starts\n")
	b.ResetTimer()
	for range b.N {
		op()
	}
	b.StopTimer()
	b.ReportMetric(float64(held.stop())/(1<<20), "held-MiB")
	runtime.GC()
	runtime.ReadMemStats(&after)
	fmt.Fprintf(os.Stderr, "take-bench: the take ends\n")
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), "live-MiB")
	if len(collectionLargeReads) != 0 {
		b.Fatalf("the take ended holding %d large slots, want none", len(collectionLargeReads))
	}
}

// liveHeapMost is the most live heap a GC marked while it is watched, read in
// bytes as each cycle ends. The GC trace prints the same figure in whole MiB,
// cut down, which leaves a pool sized by it ([SessionTakesAtOnce]) up to a MiB
// a take short.
type liveHeapMost struct {
	most    atomic.Uint64
	stopped atomic.Bool
}

// watchLiveHeap watches the live heap from now until [liveHeapMost.stop].
func watchLiveHeap() *liveHeapMost {
	l := new(liveHeapMost)
	l.arm()
	return l
}

// arm reads the live heap once the next GC has found a fresh object dead, and
// arms again until stopped: one read a cycle, after the cycle's mark.
func (l *liveHeapMost) arm() {
	runtime.AddCleanup(new([64]byte), func(w *liveHeapMost) {
		w.read()
		if !w.stopped.Load() {
			w.arm()
		}
	}, l)
}

func (l *liveHeapMost) read() {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	v := s[0].Value.Uint64()
	for {
		old := l.most.Load()
		if v <= old || l.most.CompareAndSwap(old, v) {
			return
		}
	}
}

// stop ends the watch and is the most it read.
func (l *liveHeapMost) stop() uint64 {
	l.stopped.Store(true)
	l.read()
	return l.most.Load()
}

// takeOfPieces is a take's archive whose main thread holds a line of n bytes,
// then lines, and whose `.json` member a string within n, each of bytes no
// UTF-8 holds, and a text file of n bytes, built where nothing the read is
// measured by keeps its pieces. Each piece ends in a key, so both readings
// redact it, as the take of a session it has not seen unchanged does.
func takeOfPieces(b *testing.B, n int, lines ...string) []byte {
	thread, _ := claudeMainThread(ctSession)
	bare := cntUser("", "u1", `""`)
	key := " " + shapedKey
	members := []struct{ name, body string }{
		{thread, cntUser("", "u1", `"`+benchInvalid(n-len(bare)-1-len(key))+key+`"`) + "\n" +
			strings.Join(append(lines, ""), "\n")},
		{"tasks/s/1.json", `{"text":"` + benchInvalid(n-64-len(key)) + key + `"}`},
		{"projects/p/s/tool-results/out.txt", strings.Repeat(keyLine+"\n", n/(len(keyLine)+1)-1) + key[1:] + "\n"},
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, m := range members {
		if err := tw.WriteHeader(&tar.Header{Name: m.name, Mode: 0o600, Size: int64(len(m.body)),
			Typeflag: tar.TypeReg}); err != nil {
			b.Fatal(err)
		}
		if _, err := io.WriteString(tw, m.body); err != nil {
			b.Fatal(err)
		}
	}
	if err := errors.Join(tw.Close(), zw.Close()); err != nil {
		b.Fatal(err)
	}
	return buf.Bytes()
}

// logBuffer is the process's log, written from any goroutine.
type logBuffer struct {
	mu  sync.Mutex
	out bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.out.Write(p)
}

func (b *logBuffer) lines(event string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var found []string
	for line := range strings.Lines(b.out.String()) {
		if strings.Contains(line, event) {
			found = append(found, line)
		}
	}
	return found
}

// A WAIT FOR THE LARGE SLOT IS LOGGED AS IT ENDS, and a slot taken free is
// not: a pass that found the slot held logs executor_large_read_waited with
// whether it took it, so a read cut while it waited is told from one past its
// own wait.
//
// Mutation: log no wait, or every take of the slot, or `took` whatever the
// wait's end, and a case goes red.
func TestAWaitForTheLargeSlotIsLoggedAsItEnds(t *testing.T) {
	var logs logBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	free := new(passBudget)
	if err := free.hold(t.Context()); err != nil {
		t.Fatal(err)
	}
	free.drop()
	if got := logs.lines("executor_large_read_waited"); len(got) != 0 {
		t.Errorf("a slot taken free was logged as a wait: %q", got)
	}
	for _, leaves := range []bool{true, false} {
		release := holdLargeSlot(t)
		leaving, leave := context.WithCancel(t.Context())
		ctx := newSlotWait(leaving)
		large := new(passBudget)
		done := make(chan error, 1)
		go func() { done <- large.hold(ctx) }()
		<-ctx.waited
		if leaves {
			leave()
		} else {
			release()
		}
		<-done
		large.release()
		release()
		leave()
		got := logs.lines("executor_large_read_waited")
		if want := fmt.Sprintf("took=%t", !leaves); len(got) != 1 || !strings.Contains(got[0], want) {
			t.Errorf("a wait that took the slot %t logged %q, want one line saying %s", !leaves, got, want)
		}
		logs.mu.Lock()
		logs.out.Reset()
		logs.mu.Unlock()
	}
}
