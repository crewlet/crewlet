//go:build unix

package harness

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// The capture's own guarantees, held one at a time on captures a case makes
// and a child with no process around them: the reader and the watcher are the
// case's to drive, and the end's deadline and ticks are the case's to give.

// testSetting is what a case's captures name as the setting that moves them.
const testSetting = "providers.llm.test.state_dir"

// testCapture makes a capture of stream in a directory of the case's, and
// returns it with the child's write description.
func testCapture(t *testing.T, stream string, spec streamSpec, hooks *captureHooks) (*capture, *os.File) {
	t.Helper()
	c, w, err := newCapture(t.TempDir(), testSetting, stream, false, spec, hooks)
	if err != nil {
		t.Fatalf("newCapture: %v", err)
	}
	t.Cleanup(func() {
		_ = w.Close()
		_ = c.r.Close()
	})
	return c, w
}

// handChild is a child with no process around caps: foreground, so no group is
// asked or signalled, with its reader and watcher never started.
func handChild(hooks *captureHooks, caps ...*capture) *childRun {
	done := make(chan struct{})
	close(done)
	return &childRun{
		logCtx: context.Background(), foreground: true, caps: caps, hooks: hooks, janitor: &janitor{},
		stop: make(chan struct{}), watchStop: make(chan struct{}), readDone: done, watchDone: done,
		send: procgroup.Kill,
	}
}

// now is a deadline the end meets at its first look.
func now() *captureHooks {
	return &captureHooks{deadline: func(time.Time) <-chan time.Time {
		at := make(chan time.Time)
		close(at)
		return at
	}}
}

// NOTHING WRITTEN AFTER THE END IS READ. The end measures each capture's size
// at one instant, and the last read and what the stream keeps are bounded by
// it: a line written after the end reaches no reader, and is not in what was
// kept, however soon it came.
//
// Mutation: collect reads to the end of the file rather than to the size the
// end measured.
func TestNothingWrittenAfterTheEndIsRead(t *testing.T) {
	t.Parallel()
	var lines []string
	hooks := now()
	c, w := testCapture(t, "stdout", streamSpec{kind: readToEnd, keep: maxOutput,
		onLine: func(l string) { lines = append(lines, l) }}, hooks)
	ch := handChild(hooks, c)
	if _, err := w.WriteString("A\n"); err != nil {
		t.Fatal(err)
	}
	ended := ch.end(time.Now())
	if _, err := w.WriteString("B\n"); err != nil {
		t.Fatal(err)
	}
	got := ch.collect(ended)
	if got.out != "A\n" || strings.Join(lines, "|") != "A" {
		t.Errorf("kept %q and handed on %q, want only what was written before the end", got.out, lines)
	}
}

// A PIN LASTS WHILE ANY HOLDER HOLDS THE DESCRIPTION. A copy of the child's
// write description kept open, as a helper that inherited it keeps one, holds
// the capture pinned after the original is closed; closing every copy lets it
// go — the instant a pipe's reader would have seen its end.
//
// Sequential: a child that any other case starts holds a copy of every
// description open in this process from its fork until its exec, and a fork
// landing before the last close would still pin the capture when the case
// asks. While a top-level case runs sequentially no other case's body runs, so
// no fork can land.
//
// Mutation: open the engine's description as a dup of the child's (the same
// description, as darwin's /dev/fd gives): the probe then converts the pin
// instead of being refused by it, and the capture is refused as one on a
// filesystem without per-description locks.
func TestAPinLastsWhileAnyHolderHoldsTheDescription(t *testing.T) {
	c, w := testCapture(t, "stdout", streamSpec{kind: oneAnswer}, nil)
	fd, err := unix.Dup(int(w.Fd())) //nolint:gosec // a descriptor fits an int
	if err != nil {
		t.Fatal(err)
	}
	holder := os.NewFile(uintptr(fd), "holder") //nolint:gosec // a descriptor the kernel just returned
	_ = w.Close()
	if pinned, err := c.pinned(); err != nil || !pinned {
		t.Errorf("pinned = %v (%v) while a copy of the description is held, want true", pinned, err)
	}
	_ = holder.Close()
	if pinned, err := c.pinned(); err != nil || pinned {
		t.Errorf("pinned = %v (%v) once every copy is closed, want false", pinned, err)
	}
}

// WHAT WAS READ PAST THE BOUND IS GIVEN BACK, AND ROOM IS KEPT AHEAD OF THE
// READING, through the platform's own calls: fallocate on linux, and on darwin
// F_PUNCHHOLE, which capture_darwin.go calls itself, and F_PREALLOCATE, which
// counts from the file's physical end. A stream read to its end is written 8
// MiB past maxOutput and read as it goes, then measured once: the one punch
// runs from the kept head's end to the reading, in whole blocks, and the
// file's blocks drop by all of it; and the room ahead is reserved, so the
// blocks cover the kept head and maxOutput past the reading, and writing that
// maxOutput takes no block more (a mebibyte of slack for the filesystem's
// own).
//
// Mutation: give nothing back, and nothing is punched; reserve no room ahead
// mid-run, and the blocks fall short of it.
func TestWhatWasReadPastTheBoundIsGivenBackAndRoomKeptAhead(t *testing.T) {
	t.Parallel()
	type punch struct{ off, n, dropped int64 }
	var punches []punch
	hooks := &captureHooks{punch: func(f *os.File, off, n int64) error {
		before := allocated(f)
		err := punchHole(f, off, n)
		punches = append(punches, punch{off, n, before - allocated(f)})
		return err
	}}
	c, w := testCapture(t, "stdout", streamSpec{kind: readToEnd, keep: maxOutput}, hooks)
	ch := handChild(hooks, c)
	buf := make([]byte, readChunk)
	line := append(bytes.Repeat([]byte("x"), 1<<10-1), '\n')
	mebibyte := bytes.Repeat(line, 1<<10)
	for range maxOutput>>20 + 8 {
		if _, err := w.Write(mebibyte); err != nil {
			t.Fatal(err)
		}
		for ch.readSome(c, buf, -1) {
		}
	}
	delivered := c.delivered.Load()
	if cut := c.check(); cut != nil {
		t.Fatalf("the watcher cut a stream read as it was written: %v", cut)
	}
	from, to := roundUp(maxOutput, c.block), delivered/c.block*c.block
	if len(punches) != 1 || punches[0].off != from || punches[0].n != to-from {
		t.Fatalf("punched %+v, want once from %d for %d bytes: the kept head's end to the reading", punches,
			from, to-from)
	}
	if punches[0].dropped < to-from {
		t.Errorf("the punch gave %d bytes back to the volume, want the %d it named", punches[0].dropped, to-from)
	}
	want := int64(maxOutput) + delivered + maxOutput - to
	if got := allocated(c.r); got < want {
		t.Errorf("the capture's blocks take %d bytes, want at least %d: the kept head and maxOutput "+
			"past the reading", got, want)
	}
	for range maxOutput >> 20 {
		if _, err := w.Write(mebibyte); err != nil {
			t.Fatal(err)
		}
	}
	if got := allocated(c.r); got > want+1<<20 {
		t.Errorf("writing the room ahead took the capture's blocks to %d bytes, past the %d reserved: "+
			"the room was not where the writer writes", got, want)
	}
}

// A CUT RUN'S CAPTURES ARE STILL BOUNDED UNTIL THE END. A one answer passes
// its bound and is cut at the watcher's first tick, which ends the run; but a
// writer the kill does not reach (one that escaped the group) writes on, and
// at the next tick what landed past the bound is given back too. The watcher
// runs until the end stops it, and ends the run once.
//
// Mutation: stop the watcher at the first cut, and nothing is given back
// after it; give nothing back past a one answer's bound, and nothing is
// given back at all.
func TestACutRunsCapturesAreStillBoundedUntilTheEnd(t *testing.T) {
	t.Parallel()
	ticks := make(chan time.Time, 1)
	punched := make(chan int64, 4)
	hooks := &captureHooks{
		ticker: func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} },
		punch: func(f *os.File, off, n int64) error {
			punched <- off
			return punchHole(f, off, n)
		},
	}
	c, w := testCapture(t, "stdout", streamSpec{kind: oneAnswer}, hooks)
	ch := handChild(hooks, c)
	ch.foreground = false
	ch.group = harnessGroup{leader: procgroup.Leader{PID: 4242}, current: func() (bool, error) { return true, nil }}
	var sent sentTo
	ch.send = sent.send
	var cancels atomic.Int32
	ch.cancel = func(error) { cancels.Add(1) }
	ch.watchDone = make(chan struct{})
	go ch.watch()
	t.Cleanup(func() {
		close(ch.watchStop)
		<-ch.watchDone
	})
	next := func(mib int, what string) int64 {
		t.Helper()
		if _, err := w.Write(make([]byte, mib<<20)); err != nil {
			t.Fatal(err)
		}
		ticks <- time.Now()
		select {
		case off := <-punched:
			return off
		case <-time.After(30 * time.Second): //fixedwait:liveness
			t.Fatalf("nothing was given back %s", what)
			return 0
		}
	}
	if off := next(maxOutput>>20+1, "when the answer passed its bound"); off != roundUp(maxOutput, c.block) {
		t.Errorf("the first punch was at %d, want the bound, %d", off, maxOutput)
	}
	if off := next(2, "once the run was cut"); off != maxOutput+1<<20 {
		t.Errorf("the punch after the cut was at %d, want where the first one ended, %d", off, maxOutput+1<<20)
	}
	if n, to := cancels.Load(), sent.list(); n != 1 || len(to) != 1 {
		t.Errorf("the run was cancelled %d times and its group killed %d times, want once each", n, len(to))
	}
}

// A FILE THAT SHRANK UNDER THE READER IS REFUSED. Something truncated what the
// harness had written — a reopen of its stdout with O_TRUNC — and nothing read
// of it can be shown to be whole: the reader names the stream, and unfinished
// refuses it.
//
// Mutation: ignore a size below the reader's offset.
func TestAFileThatShrankUnderTheReaderIsRefused(t *testing.T) {
	t.Parallel()
	c, w := testCapture(t, "stdout", streamSpec{kind: readToEnd, keep: maxOutput}, nil)
	ch := handChild(nil, c)
	buf := make([]byte, readChunk)
	if _, err := w.WriteString(strings.Repeat("x", 100)); err != nil {
		t.Fatal(err)
	}
	if !ch.readSome(c, buf, -1) {
		t.Fatal("the reader read nothing of what was written")
	}
	if err := w.Truncate(0); err != nil {
		t.Fatal(err)
	}
	ch.readSome(c, buf, -1)
	shrunk := ch.shrunk.Load()
	if shrunk == nil || *shrunk != "stdout" {
		t.Fatalf("shrunk = %v, want stdout", shrunk)
	}
	if why := (&rawResult{shrunk: *shrunk}).unfinished(); !strings.Contains(why, "stdout file shrank") {
		t.Errorf("unfinished = %q, want it to refuse the shrunk stdout", why)
	}
}

// A CAPTURE ON A FILESYSTEM WITHOUT PER-DESCRIPTION LOCKS IS REFUSED. The end
// sees who still holds the output only through a pin, and a state_dir whose
// filesystem keeps no lock per open file description would read every holder
// as gone: the capture is refused, naming state_dir, and leaves nothing behind.
//
// Mutation: drop the check that the pin holds.
func TestACaptureOnAFilesystemWithoutPerDescriptionLocksIsRefused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hooks := &captureHooks{pinned: func(*os.File) (bool, error) { return false, nil }}
	c, w, err := newCapture(dir, testSetting, "stdout", false, streamSpec{}, hooks)
	if err == nil {
		_ = w.Close()
		_ = c.r.Close()
		t.Fatal("a capture whose pin the filesystem does not keep was made")
	}
	for _, want := range []string{testSetting, "lock per open file"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the refused capture left %d files in its directory", len(entries))
	}
}

// A HELD CAPTURE IS EMPTIED AT THE END. A copy of the write description stays
// open with a mebibyte written: the capture goes to the janitor, whose first
// tick gives back its size and every block — the room reserved for it
// included — while the holder still has the description; and once the holder
// lets go, the next tick closes it.
//
// Sequential: a child that any other case starts holds a copy of every
// description open in this process from its fork until its exec, this
// capture's write description among them, and the janitor rightly keeps a
// capture something still holds. A fork landing on the second tick would hold
// the capture past it, and no tick the case could count would be the last.
// While a top-level case runs sequentially no other case's body runs, so no
// fork can land.
//
// Mutation: open the engine's description read-only again: it can then neither
// reserve nor truncate.
func TestAHeldCaptureIsEmptiedAtTheEnd(t *testing.T) {
	hooks := now()
	c, w := testCapture(t, "stdout", streamSpec{kind: readToEnd, keep: maxOutput}, hooks)
	ticks, observed := make(chan time.Time), make(chan int64)
	ch := handChild(hooks, c)
	ch.janitor = &janitor{
		tick:    func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} },
		observe: func(_ string, allocated int64) { observed <- allocated },
	}
	if _, err := w.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	ch.collect(ch.end(time.Now()))
	ch.close()
	ticks <- time.Now()
	left := <-observed
	if size, err := c.size(); err != nil || size != 0 || left != 0 {
		t.Errorf("the held capture's size is %d (%v) and its blocks %d bytes, want both emptied", size, err, left)
	}
	_ = w.Close()
	ticks <- time.Now()
	<-observed
	if _, err := c.r.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Errorf("the janitor kept the capture open once its holder let go (%v)", err)
	}
}

// A CAPTURE PATH REPLACED BEFORE ITS REOPEN IS NAMED AS SUCH. The engine opens
// its description of the file a second time: on darwin by path, so a file
// renamed onto the path in between would hand it another file, which the
// comparison of the two refuses in its own words, never as a filesystem
// without locks; on linux through /proc/self/fd, so the swap hands it nothing
// and the engine reads the file the child writes.
//
// Mutation: skip the comparison, and two files pass as one; reopen by path on
// linux, and the engine reads the swapped file.
func TestACapturePathReplacedBeforeItsReopenIsNamedAsSuch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := os.Create(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := os.Create(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := sameFile(a, b); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Errorf("two files were taken for one capture's (%v)", err)
	}

	hooks := &captureHooks{betweenOpens: func(path string) {
		other := path + ".other"
		_ = os.WriteFile(other, []byte("SWAPPED!"), 0o600)
		_ = os.Rename(other, path)
	}}
	c, w, err := newCapture(t.TempDir(), testSetting, "stdout", false, streamSpec{kind: oneAnswer}, hooks)
	if runtime.GOOS != "linux" {
		if err == nil || !strings.Contains(err.Error(), "replaced") {
			t.Errorf("a path swapped between the opens was not refused as one (%v)", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("newCapture: %v", err)
	}
	defer c.r.Close()
	if _, err := w.WriteString("ORIGINAL"); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if got := c.contents(8); got != "ORIGINAL" {
		t.Errorf("the engine reads %q, want what the child wrote into the file it was handed", got)
	}
}

// A GROUP THAT CANNOT BE READ HOLDS THE END, AND IS NOT SIGNALLED. Whether the
// group still has a living member is an error under a /proc that hides other
// users' records: the end reads the error as a member alive, so it asks again
// at every tick until its deadline rather than end at the first; and at the
// deadline the kill is withheld and logged, since the group cannot be shown to
// be the one recorded.
//
// Sequential: the log is the process's.
//
// Mutation: read the error as no living member, and the end returns at its
// first ask; or kill without asking whether the group is current.
func TestAGroupThatCannotBeReadHoldsTheEndAndIsNotSignalled(t *testing.T) {
	logs := captureLogs(t)
	ticks, deadline := make(chan time.Time), make(chan time.Time)
	ch := handChild(&captureHooks{
		ticker:   func(time.Duration) (<-chan time.Time, func()) { return ticks, func() {} },
		deadline: func(time.Time) <-chan time.Time { return deadline },
	})
	ch.foreground = false
	ch.group = harnessGroup{leader: procgroup.Leader{PID: 4242},
		current: func() (bool, error) { return false, errors.New("the process table could not be read") }}
	var sent sentTo
	ch.send = sent.send
	done := make(chan ending, 1)
	go func() { done <- ch.end(time.Now()) }()
	for range 3 {
		select {
		case ticks <- time.Now():
		case <-done:
			t.Fatal("the end returned before its deadline though its group could not be read")
		}
	}
	close(deadline)
	<-done
	if got := sent.list(); len(got) != 0 {
		t.Errorf("a group the kernel would not describe was signalled at %v", got)
	}
	if !strings.Contains(logs.String(), "harness_group_signal_withheld") {
		t.Errorf("the withheld kill was not logged: %s", logs.String())
	}
}
