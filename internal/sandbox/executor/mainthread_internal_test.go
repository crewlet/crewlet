package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
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
	parent := ""
	if n > 1 {
		parent = fmt.Sprintf("u%d", n-1)
	}
	return cntUser(parent, fmt.Sprintf("u%d", n), `"`+strings.Repeat("a", pad)+`"`)
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

// A TYPICAL COLLECTION READ TAKES NO SLOT: the probe's two threads, and a
// thread AT both bases, [collectionBaseRecords] records kept beside a line of
// [collectionBaseLineBytes], are each walked with the large slot free
// throughout, as seen from inside the walk at every line.
//
// Mutation: escalate on every pass, or at either base (`>=` for `>`), and a
// line is read with the slot held.
func TestATypicalCollectionReadTakesNoSlot(t *testing.T) {
	threads := map[string]string{
		"at both bases": slotChain(collectionBaseRecords-1, slotUserOf(collectionBaseRecords, collectionBaseLineBytes)),
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
	if got := len(threads["at both bases"]); got <= collectionBaseLineBytes {
		t.Fatalf("the thread at both bases is %d bytes", got)
	}
}

// A LINE OR A CHAIN PAST THE BASE WAITS FOR THE LARGE SLOT: with the slot held
// elsewhere, a walk meeting a line one byte past [collectionBaseLineBytes]
// waits before that line is decoded, and one whose reading keeps one record
// past [collectionBaseRecords] waits before the next line: a reading sees no
// later line. Freed, the walk takes the slot and reads on with it held; and a
// walk whose context ends while it waits ends with that context's error, its
// count unread, holding nothing.
//
// Mutation: `>=` for `>` at either base, or escalate after the line past the
// base is decoded, and the walk waits at another line.
func TestALineOrAChainPastTheBaseWaitsForTheLargeSlot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		before int
	}{
		{"a line", cntThread(slotUser(1, 0), slotUserOf(2, collectionBaseLineBytes+1), slotUser(3, 0)), 1},
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
		if !errors.Is(cut.ended, context.Canceled) || cut.ends != 1 || len(cut.lines) != tc.before {
			t.Errorf("%s: a walk whose caller left while it waited read %d lines and ended %v (%d ends), want %d "+
				"and the caller's own error once", tc.name, len(cut.lines), cut.ended, cut.ends, tc.before)
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
// triggers, a chain past [collectionBaseRecords] and then a line past
// [collectionBaseLineBytes], holds one slot, never waiting for the one it
// holds; a collection's read past the base holds the slot as it reads and none
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
	body := slotChain(collectionBaseRecords+1, slotUserOf(collectionBaseRecords+2, collectionBaseLineBytes+1),
		slotUser(collectionBaseRecords+3, 0))
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
	long := slotUserOf(2, 2*collectionBaseLineBytes)
	// AFTER IT, more than the reader's buffer holds, so the read is still
	// asking for lines once its context ends.
	after := make([]string, 0, 254)
	for k := 3; k <= 256; k++ {
		after = append(after, slotUser(k, 1<<10))
	}
	for _, tc := range []struct {
		name  string
		body  string
		cutAt int
		mark  sandbox.CountMark
	}{
		{"at the thread's end", cntThread(slotUser(1, 0), long, cntAnchor("u2", "u3", "m1", 100, 0, 0, 50)), 0, ""},
		{"at a bound", cntThread(slotUser(1, 0), long, slotUserOf(3, threadLineMax+1)), 0, sandbox.CountUnread},
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
		meter := newContextMeter(leaving, archiveTar, ctSession, "2.1.322", took)
		_, _ = meter.Write(tc.written)
		meter.finish(tc.failed)
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
		*err = scrubDocument(bytes.NewReader(export), io.Discard, newScrubber(nil), testBounds.gatedBy(ctx, large))
	}()
	return done
}

// A TYPICAL TAKE'S SCRUB TAKES NO SLOT: with the large slot held elsewhere,
// the scrub of an archive holding the probe's two threads, a transcript's line
// at [collectionBaseLineBytes], a document's string at it and a text that
// holds a key's stretch below it reads to its end without waiting for it.
//
// Mutation: gate a piece at the base (`>=` for `>`), or every piece, and the
// scrub waits.
func TestATypicalTakesScrubTakesNoSlot(t *testing.T) {
	files := map[string]string{
		"projects/p/s.jsonl":                slotUserOf(1, collectionBaseLineBytes) + "\n",
		"tasks/s/1.json":                    `{"text":"` + strings.Repeat("a", collectionBaseLineBytes-11) + `"}`,
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
	export := gzipOf(t, `{"text":"`+strings.Repeat("a", collectionBaseLineBytes-1)+`"}`)
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
// a transcript's line, a document's string and a stretch of short lines a
// redaction holds (a key with no END), each twice [collectionBaseLineBytes],
// wait for the slot held elsewhere having read at most the base and one fill
// past it; freed, the rewrite reads to its end and holds nothing once done;
// with its context ended while it waits, it ends with [errLargeWait].
//
// Mutation: hold a line only once it is read whole, drop a piece's gate, or
// keep a hold past the rewrite, and a case goes red.
func TestATakesPiecePastTheBaseWaitsForTheLargeSlot(t *testing.T) {
	long := strings.Repeat("a", 2*collectionBaseLineBytes)
	key := "-----BEGIN RSA PRIVATE KEY-----\n" +
		strings.Repeat(keyLine+"\n", 2*collectionBaseLineBytes/(len(keyLine)+1))
	type rewrite func(io.Writer, io.Reader, scrubber, scrubBounds) error
	pieces := []struct {
		name    string
		body    string
		rewrite rewrite
	}{
		{"a transcript's line", `{"text":"` + long + `"}` + "\n", rewriteLines},
		{"a document's string", `{"text":"` + long + `"}`, rewriteDocument},
		{"a held stretch", key, rewriteText},
	}
	most := collectionBaseLineBytes + 64<<10 + len(keyLine) + 1
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
					"one fill", tc.name, ctx.read, most)
			}
			if leaves {
				leave()
			} else {
				free()
			}
			<-done
			switch {
			case leaves && !(errors.Is(err, errLargeWait) && errors.Is(err, context.Canceled)):
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

// heldReading is a reading that keeps no records and notes the large slots
// held as it is handed each line.
type heldReading struct{ held []int }

func (r *heldReading) line(threadLine)          { r.held = append(r.held, len(collectionLargeReads)) }
func (r *heldReading) refused(*threadLineError) { r.held = append(r.held, len(collectionLargeReads)) }
func (r *heldReading) end(bool, error)          {}

// A WALK THAT KEEPS NO RECORDS HOLDS THE SLOT FOR ITS LINE ALONE, as a take's
// meter and a collection's count walk: a line past [collectionBaseLineBytes]
// is handed on with the slot held, and the lines more than a buffer after it
// with none, and nothing is held once the walk ends.
//
// Mutation: keep every hold to the pass's end, and a later line is read with
// the slot held.
func TestAWalkThatKeepsNoRecordsHoldsTheSlotForItsLineAlone(t *testing.T) {
	lines := []string{slotUser(1, 0), slotUserOf(2, collectionBaseLineBytes+1)}
	for k := 3; k <= 256; k++ {
		lines = append(lines, slotUser(k, 1<<10))
	}
	var r heldReading
	large := new(passBudget)
	defer large.release()
	walkThread(t.Context(), strings.NewReader(cntThread(lines...)), threadPlainMax, large, &r)
	if len(r.held) != len(lines) {
		t.Fatalf("the walk handed %d lines, want %d", len(r.held), len(lines))
	}
	if r.held[1] != 1 || r.held[len(r.held)-1] != 0 {
		t.Errorf("the walk handed its long line with %d large slots held and its last with %d, want one and none",
			r.held[1], r.held[len(r.held)-1])
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

// WHAT A COLLECTION'S PASS COSTS AT ITS BASE, the figures
// [collectionBaseLineBytes] and [collectionBaseRecords] are stated by (the
// package doc's "A collection's read of a thread"): a main thread of
// [collectionBaseRecords] records, the chain filing each, whose last is a
// reply at [collectionBaseLineBytes] whose id is bytes no UTF-8 holds, which
// the decode of a string reads as U+FFFD, three bytes a byte; read by the
// chain and the count in the one walk, as a collection reads it, under a pass
// that must take no slot. What is held at most is read off the runtime's GC
// trace as BenchmarkAThreadChainAtItsBounds says:
//
//	GOGC=10 GODEBUG=gctrace=1 go test -run '^$' -bench '^BenchmarkACollectionPassAtItsBase$' -benchtime 1x -benchmem
func BenchmarkACollectionPassAtItsBase(b *testing.B) {
	t := benchWrite(b, filepath.Join(b.TempDir(), "thread.jsonl"), func(t *benchThread) {
		n := collectionBaseRecords
		for k := 1; k < n; k++ {
			parent := benchRoot
			if k > 1 {
				parent = benchOf(k - 1)
			}
			t.record(benchRecord(parent, benchMain, "assistant", benchReply(fmt.Sprintf("msg_%08d", k),
				benchCall(fmt.Sprintf("toolu_%08d", k))), k, k))
		}
		head, tail, _ := strings.Cut(benchRecord(benchOf(n-1), benchMain, "assistant", `"message":{"id":"@",`+
			`"model":"claude-sonnet-4-5","usage":{`+benchUsage+`},"type":"message","role":"assistant","content":`+
			benchCall(fmt.Sprintf("toolu_%08d", n))+`,"stop_reason":"tool_use"},`, n, n), "@")
		t.record(head + benchInvalid(collectionBaseLineBytes-len(head)-len(tail)) + tail)
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
	b.ReportMetric(float64(read.c.handles), "records")
	b.ReportMetric(float64(int64(after.HeapAlloc)-int64(before.HeapAlloc))/(1<<20), "live-MiB")
	benchCountRead(b, read)
	if read.c.handles != collectionBaseRecords || read.c.past || read.count.tokens != 2 || read.escalated {
		b.Fatalf("the chain filed %d records, past its bound %t, the count anchors at %d and the pass took the "+
			"large slot %t; want %d, false, 2 and false", read.c.handles, read.c.past, read.count.tokens,
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
		outer := collectionLargeReads
		collectionLargeReads = make(chan struct{}, CollectionLargeReadsAtOnce)
		t.Cleanup(func() { collectionLargeReads = outer })
		f(t)
	})
}

// slotTake is a take of a box whose capture is archive, of ctSession's
// session, written by Claude Code 2.1.322.
func slotTake(t *testing.T, archive []byte) sandbox.TakenSession {
	t.Helper()
	box := sandbox.NewFakeSandbox("box-1")
	p := PathsFor(box)
	box.Put(p.SessionRecord(), "id "+ctSession+"\ncontinued no\nrelease 2.1.322 (Claude Code)\n")
	box.Put(p.SessionCapture(), string(archive))
	taken, err := NewClaudeCode().TakeSession(t.Context(), box, sandbox.SessionTake{MaxBytes: 32 << 20})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take: %v", err)
	}
	return taken
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
// [collectionBaseLineBytes]. With the slot held by the case, the body yields
// no byte and its Close returns, ending everything the take started, the case
// still holding the slot; freed, the body reads to its end, the count reads
// the whole thread, and nothing of the take holds the slot afterwards.
//
// Mutation: leave the take's scrub ungated, keep its hold past a piece, or
// let Close leave the scrub's wait, and a case goes red or deadlocks.
func TestATakeHoldsTheLargeSlotOnlyWhileItHoldsAPiecePastTheBase(t *testing.T) {
	long := `"` + strings.Repeat("a", 2*collectionBaseLineBytes) + `"`
	later := tarMember{h: tar.Header{Name: "projects/p/s/tool-results/out.txt", Mode: 0o600,
		Typeflag: tar.TypeReg}, body: strings.Repeat("b", 2*collectionBaseLineBytes) + "\n"}
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
// within the base whose kept form is past it (bytes no UTF-8 holds, each kept
// as U+FFFD), so the meter alone waits for the slot held by the case, and the
// scrub waits on the meter. Closed before its end, the take returns, its
// meter ended with the count unread, the case still holding the slot.
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
	lines := []string{cntUser("", "u1", `"`+strings.Repeat("\xff", 100<<10)+`"`)}
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
