package sandboxtest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Box drives the FILE CONTRACT every [sandbox.Sandbox] backend keeps: the three
// reads a harness runner is built on, OpenTail, OpenRange, the seeding write,
// and the streamed write ([sandbox.Sandbox.WriteStream]): whole, empty, past
// the read cap, over a file, and from a source that fails part way.
//
// ONE SUITE FOR EVERY BACKEND, the in-memory twin included: a twin answering a
// file past the cap whole lets a runner test certify a read no real box makes.
// A backend's own geometry (where a path may point, teardown) is not covered.
// fresh returns an empty box per case, built with [ReadCap] as its whole-read
// cap; every path is under its [sandbox.Sandbox.Home].
func Box(t *testing.T, fresh func(t *testing.T) sandbox.Sandbox) {
	t.Helper()
	cases := []struct {
		name string
		fn   func(*testing.T, sandbox.Sandbox)
	}{
		{"AMissingFileIsEmptyToEveryRead", testAMissingFileIsEmptyToEveryRead},
		{"AWholeReadReturnsTheFileByteForByte", testAWholeReadReturnsTheFileByteForByte},
		{"AnEmptyFileIsEmptyToEveryRead", testAnEmptyFileIsEmptyToEveryRead},
		{"ATailIsTheFilesEndAndNamesItsWholeSize", testATailIsTheFilesEndAndNamesItsWholeSize},
		{"AStreamedTailIsTheFilesEndAndNamesItsWholeSize", testAStreamedTailIsTheFilesEndAndNamesItsWholeSize},
		{"ARangeIsTheFilesBytesFromItsOffsetAndNamesItsWholeSize",
			testARangeIsTheFilesBytesFromItsOffsetAndNamesItsWholeSize},
		{"AFilePastTheCapIsRefusedWholeAndReadAsAStream", testAFilePastTheCapIsRefusedWholeAndReadAsAStream},
		{"AFileAtTheCapIsReadWhole", testAFileAtTheCapIsReadWhole},
		{"AWholeOpenSizesTheFileAndRefusesOnePastTheCap", testAWholeOpenSizesTheFileAndRefusesOnePastTheCap},
		{"AStreamClosedEarlyIsReleased", testAStreamClosedEarlyIsReleased},
		{"ARewriteIsNeverReadHalfDone", testARewriteIsNeverReadHalfDone},
		{"AStreamedWriteIsReadBackByteForByte", testAStreamedWriteIsReadBackByteForByte},
		{"AnEmptyStreamWritesAnEmptyFile", testAnEmptyStreamWritesAnEmptyFile},
		{"AStreamPastTheReadCapIsWrittenWhole", testAStreamPastTheReadCapIsWrittenWhole},
		{"AStreamReplacesTheFileWhole", testAStreamReplacesTheFileWhole},
		{"AStreamThatFailsLeavesTheFileAsItWas", testAStreamThatFailsLeavesTheFileAsItWas},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.fn(t, fresh(t))
		})
	}
}

// ReadCap is the whole-read cap a backend builds the suite's boxes at: a MiB,
// past the largest file and tail the suite reads, and a thirty-second of what
// [sandbox.MaxFileBytes] would build. Every backend reads memory or the host's
// own files, where nothing could hold a limit of its own, and every cap branch
// is the same at any size; the production cap is held by the sandbox package's
// TestEveryBoxReadsWholeUpToMaxFileBytes.
const ReadCap = 1 << 20

// wholeReadBytes is the file the suite reads whole and byte for byte: more
// than a 64 KiB read buffer, so a read that handed back its first buffer as the
// whole is caught.
const wholeReadBytes = 70_000

// Past the largest file the other cases read whole, or the cap would be what
// failed them: a [ReadCap] at or below [wholeReadBytes] overflows this.
const _ uint = ReadCap - wholeReadBytes - 1

// pathIn is a path under the box's own home.
func pathIn(box sandbox.Sandbox, name string) string {
	return strings.TrimRight(box.Home(), "/") + "/.crewlet/" + name
}

// pattern is n bytes whose every offset is distinguishable from its
// neighbours over any window shorter than the period, so a read that returned
// the right LENGTH from the wrong PLACE is caught.
func pattern(n int) []byte {
	const period = 251 // prime, so no power-of-two window lines up with it
	out := make([]byte, n)
	for i := range out {
		out[i] = byte(i % period)
	}
	return out
}

func write(t *testing.T, box sandbox.Sandbox, path string, content []byte) {
	t.Helper()
	if err := box.WriteFile(t.Context(), path, content); err != nil {
		t.Fatalf("WriteFile %s: %v", path, err)
	}
}

// readTailStream is the stream of path's last n bytes, read to its end, and
// the size the open named.
func readTailStream(t *testing.T, box sandbox.Sandbox, path string, n int64) ([]byte, int64) {
	t.Helper()
	r, size, err := box.OpenTail(t.Context(), path, n)
	if err != nil {
		t.Fatalf("OpenTail %s: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the end of %s: %v", path, err)
	}
	return got, size
}

// readRange is the stream of at most n bytes of path from off, read to its
// end, and the size the open named.
func readRange(t *testing.T, box sandbox.Sandbox, path string, off, n int64) ([]byte, int64) {
	t.Helper()
	r, size, err := box.OpenRange(t.Context(), path, off, n)
	if err != nil {
		t.Fatalf("OpenRange %s from %d for %d: %v", path, off, n, err)
	}
	return readAllOf(t, r), size
}

// readAllOf is what r streams to its end; it closes r.
func readAllOf(t *testing.T, r io.ReadCloser) []byte {
	t.Helper()
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading a range: %v", err)
	}
	return got
}

func readStream(t *testing.T, box sandbox.Sandbox, path string) []byte {
	t.Helper()
	r, err := box.OpenFile(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenFile %s: %v", path, err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the stream of %s: %v", path, err)
	}
	return got
}

// "NOT THERE YET" IS THE ORDINARY ANSWER, on every read: the runner polls for
// markers and streams that do not exist until the job has written them, and a
// poll is not an error.
func testAMissingFileIsEmptyToEveryRead(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "absent")
	if got, err := box.ReadFile(t.Context(), path); err != nil || len(got) != 0 {
		t.Errorf("ReadFile of a missing file = %q, %v; want empty", got, err)
	}
	if got := readStream(t, box, path); len(got) != 0 {
		t.Errorf("OpenFile of a missing file streamed %q; want nothing", got)
	}
	tail, err := box.ReadTail(t.Context(), path, 64)
	if err != nil || len(tail.Data) != 0 || tail.Size != 0 {
		t.Errorf("ReadTail of a missing file = %d bytes of %d, %v; want empty", len(tail.Data), tail.Size, err)
	}
	if got, size := readTailStream(t, box, path, 64); len(got) != 0 || size != 0 {
		t.Errorf("OpenTail of a missing file streamed %d bytes of %d; want nothing", len(got), size)
	}
}

func testAWholeReadReturnsTheFileByteForByte(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "report.md")
	content := pattern(wholeReadBytes)
	write(t, box, path, content)
	if got, err := box.ReadFile(t.Context(), path); err != nil || !bytes.Equal(got, content) {
		t.Errorf("ReadFile = %d bytes, %v; want the %d written", len(got), err, len(content))
	}
	if got := readStream(t, box, path); !bytes.Equal(got, content) {
		t.Errorf("OpenFile streamed %d bytes; want the %d written, in order", len(got), len(content))
	}
}

// A file that exists and is empty is told apart from nothing only by its size,
// and that is what a tail reports: zero, with no data. Over HTTP a suffix range
// on an empty file is the one unsatisfiable range, which a backend must answer
// as an empty end rather than as a failed read.
func testAnEmptyFileIsEmptyToEveryRead(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "err.log")
	write(t, box, path, nil)
	if got, err := box.ReadFile(t.Context(), path); err != nil || len(got) != 0 {
		t.Errorf("ReadFile of an empty file = %q, %v", got, err)
	}
	if got := readStream(t, box, path); len(got) != 0 {
		t.Errorf("OpenFile of an empty file streamed %q", got)
	}
	tail, err := box.ReadTail(t.Context(), path, 64)
	if err != nil || len(tail.Data) != 0 || tail.Size != 0 || !tail.Whole() {
		t.Errorf("ReadTail of an empty file = %d bytes of %d (whole %v), %v",
			len(tail.Data), tail.Size, tail.Whole(), err)
	}
	if got, size := readTailStream(t, box, path, 64); len(got) != 0 || size != 0 {
		t.Errorf("OpenTail of an empty file streamed %d bytes of %d; want nothing", len(got), size)
	}
}

// A STREAMED END IS THE FILE'S LAST BYTES AND ITS WHOLE SIZE: what a failed
// check's log is kept from, which costs the bytes kept and never the file
// before them. Its bytes are the file's own from the right place, its size
// the whole file's, and a stream asked for more than the file is the whole
// file.
func testAStreamedTailIsTheFilesEndAndNamesItsWholeSize(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "output")
	content := pattern(wholeReadBytes)
	write(t, box, path, content)

	got, size := readTailStream(t, box, path, 1000)
	if !bytes.Equal(got, content[len(content)-1000:]) || size != int64(len(content)) {
		t.Errorf("the streamed end is %d bytes of %d; want the last 1000 of %d", len(got), size, len(content))
	}
	whole, size := readTailStream(t, box, path, 2*wholeReadBytes)
	if !bytes.Equal(whole, content) || size != int64(len(content)) {
		t.Errorf("a streamed end longer than the file is %d bytes of %d; want the whole %d", len(whole), size,
			len(content))
	}
}

// THE END, AND HOW MUCH CAME BEFORE IT. A caller reading a stream's end says
// what it did not read by the file's whole size, so a tail that answered the
// right bytes and the wrong size would have it present the end as the whole.
func testATailIsTheFilesEndAndNamesItsWholeSize(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "result.json")
	content := pattern(10_000)
	write(t, box, path, content)

	tail, err := box.ReadTail(t.Context(), path, 1000)
	if err != nil {
		t.Fatalf("ReadTail: %v", err)
	}
	if !bytes.Equal(tail.Data, content[len(content)-1000:]) {
		t.Errorf("the tail is %d bytes not equal to the file's last 1000", len(tail.Data))
	}
	if tail.Size != int64(len(content)) || tail.Whole() || tail.Before() != 9000 {
		t.Errorf("tail size %d (whole %v, before %d); want 10000, not whole, 9000 before",
			tail.Size, tail.Whole(), tail.Before())
	}

	whole, err := box.ReadTail(t.Context(), path, 50_000)
	if err != nil || !bytes.Equal(whole.Data, content) || !whole.Whole() {
		t.Errorf("a tail longer than the file = %d bytes (whole %v), %v; want the whole file",
			len(whole.Data), whole.Whole(), err)
	}
}

// A RANGE IS THE FILE'S BYTES FROM ITS OFFSET, AND ITS WHOLE SIZE: what a
// reading of a run's event stream reads on with from where it stopped. The
// bytes are the file's own from off, never past n nor past the size the open
// found; an offset at or past the end is nothing with the size, which is how a
// reader whose file shrank finds out; a missing file is nothing and 0, and a
// negative range is refused.
func testARangeIsTheFilesBytesFromItsOffsetAndNamesItsWholeSize(t *testing.T, box sandbox.Sandbox) {
	ctx := t.Context()
	path := pathIn(box, "events.jsonl")
	content := pattern(wholeReadBytes)
	write(t, box, path, content)
	size := int64(len(content))
	for _, c := range []struct {
		name   string
		off, n int64
		want   []byte
	}{
		{"from the start", 0, 1000, content[:1000]},
		{"from the middle", 30_000, 1000, content[30_000:31_000]},
		{"from the middle past the end", 60_000, 50_000, content[60_000:]},
		{"to the end of any file", 1234, math.MaxInt64, content[1234:]},
		{"of no bytes", 500, 0, nil},
		{"at the size", size, 64, nil},
		{"past the size, as a file that shrank is read", size + 4096, 64, nil},
	} {
		got, gotSize := readRange(t, box, path, c.off, c.n)
		if !bytes.Equal(got, c.want) || gotSize != size {
			t.Errorf("a range %s (%d for %d) is %d bytes of %d; want the file's %d from there, of %d",
				c.name, c.off, c.n, len(got), gotSize, len(c.want), size)
		}
	}

	for name, at := range map[string]string{"missing": pathIn(box, "absent"), "empty": pathIn(box, "empty")} {
		if name == "empty" {
			write(t, box, at, nil)
		}
		if got, gotSize := readRange(t, box, at, 0, 64); len(got) != 0 || gotSize != 0 {
			t.Errorf("a range of a %s file streamed %d bytes of %d; want nothing of 0", name, len(got), gotSize)
		}
	}

	for _, bad := range [][2]int64{{-1, 64}, {0, -1}} {
		if r, _, err := box.OpenRange(ctx, path, bad[0], bad[1]); !errors.Is(err, sandbox.ErrRangeNegative) {
			if err == nil {
				_ = r.Close()
			}
			t.Errorf("a range %d for %d = %v; want ErrRangeNegative", bad[0], bad[1], err)
		}
	}

	// The file is rewritten longer while the range is open: the range is the
	// file as the open found it.
	r, gotSize, err := box.OpenRange(ctx, path, 1000, math.MaxInt64)
	if err != nil {
		t.Fatalf("OpenRange before the file grew: %v", err)
	}
	write(t, box, path, pattern(2*wholeReadBytes))
	if got := readAllOf(t, r); !bytes.Equal(got, content[1000:]) || gotSize != size {
		t.Errorf("a range opened before the file grew is %d bytes of %d; want the %d from 1000 of %d it had",
			len(got), gotSize, len(content)-1000, size)
	}
}

// THE REFUSAL IS FOR A FILE MEANT TO BE READ WHOLE, and only for that. A
// report past the cap is refused rather than clipped, because a clipped report
// reads as a finished one; the same bytes read as a stream or from their end
// are what a run's event log and its stderr are, and refusing those refused a
// run for the size of its own log.
func testAFilePastTheCapIsRefusedWholeAndReadAsAStream(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "stream.jsonl")
	content := pattern(ReadCap + 1)
	write(t, box, path, content)

	got, err := box.ReadFile(t.Context(), path)
	if !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("ReadFile past the cap = %d bytes, %v; want ErrFileTooLarge", len(got), err)
	}
	// And it names the cap, which is what a collection describes the
	// refused piece by.
	var tooLarge *sandbox.FileTooLargeError
	if errors.As(err, &tooLarge) && (tooLarge.Limit != ReadCap || tooLarge.Path != path) {
		t.Errorf("the refusal names %s at %d bytes; want %s at the box's %d",
			tooLarge.Path, tooLarge.Limit, path, ReadCap)
	} else if err != nil && !errors.As(err, &tooLarge) {
		t.Errorf("the refusal %v does not carry the cap it was refused at", err)
	}

	r, err := box.OpenFile(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenFile past the cap: %v", err)
	}
	sum := sha256.New()
	n, err := io.Copy(sum, r)
	_ = r.Close()
	if err != nil || n != int64(len(content)) {
		t.Fatalf("the stream delivered %d bytes, %v; want all %d", n, err, len(content))
	}
	if want := sha256.Sum256(content); !bytes.Equal(sum.Sum(nil), want[:]) {
		t.Error("the stream delivered the right length and the wrong bytes")
	}

	tail, err := box.ReadTail(t.Context(), path, 4096)
	if err != nil {
		t.Fatalf("ReadTail past the cap: %v", err)
	}
	if !bytes.Equal(tail.Data, content[len(content)-4096:]) || tail.Size != int64(len(content)) {
		t.Errorf("the tail past the cap is %d bytes of %d; want the last 4096 of %d",
			len(tail.Data), tail.Size, len(content))
	}

	end, size := readTailStream(t, box, path, int64(ReadCap))
	if !bytes.Equal(end, content[1:]) || size != int64(len(content)) {
		t.Errorf("the streamed end past the cap is %d bytes of %d; want the last %d of %d", len(end), size,
			ReadCap, len(content))
	}
}

// The cap is inclusive: a file of exactly it is read, so the refusal above is
// the +1 and not an off-by-one that refuses honest output.
func testAFileAtTheCapIsReadWhole(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "findings.md")
	content := pattern(ReadCap)
	write(t, box, path, content)
	got, err := box.ReadFile(t.Context(), path)
	if err != nil || len(got) != len(content) {
		t.Errorf("ReadFile of exactly the cap = %d bytes, %v; want all %d", len(got), err, len(content))
	}
}

// A WHOLE OPEN SIZES THE FILE AND REFUSES ONE PAST THE CAP
// ([sandbox.Sandbox.OpenWhole]): a file at the cap opens with its size and
// reads back byte for byte from that open, a missing one is nothing and 0, and
// one byte past the cap is refused as ReadFile refuses it, naming its size, so
// a collection weighs what it will read before it reads.
//
// Mutations, each red: open a file past the cap; answer the cap for the size;
// read past the size the open gave.
func testAWholeOpenSizesTheFileAndRefusesOnePastTheCap(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "findings.md")
	if r, size, err := box.OpenWhole(t.Context(), path); err != nil || size != 0 || len(readAllOf(t, r)) != 0 {
		t.Errorf("OpenWhole of a missing file = size %d, %v; want nothing and 0", size, err)
	}
	content := pattern(ReadCap)
	write(t, box, path, content)
	r, size, err := box.OpenWhole(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenWhole of a file at the cap: %v", err)
	}
	if got := readAllOf(t, r); size != int64(ReadCap) || !bytes.Equal(got, content) {
		t.Errorf("OpenWhole of a file at the cap = size %d, %d bytes; want %d of each, byte for byte",
			size, len(got), ReadCap)
	}
	write(t, box, path, pattern(ReadCap+1))
	var tooLarge *sandbox.FileTooLargeError
	switch r, _, err := box.OpenWhole(t.Context(), path); {
	case !errors.As(err, &tooLarge):
		if r != nil {
			_ = r.Close()
		}
		t.Errorf("OpenWhole one byte past the cap: %v, want a FileTooLargeError", err)
	case tooLarge.Size != int64(ReadCap+1) || tooLarge.Limit != ReadCap:
		t.Errorf("OpenWhole one byte past the cap refused it at size %d of limit %d, want %d of %d",
			tooLarge.Size, tooLarge.Limit, ReadCap+1, ReadCap)
	}
}

// A reader that stops early (a decoder that found what it needed) closes
// the stream without draining it, and the close neither fails nor hangs, from
// whichever open the stream came.
func testAStreamClosedEarlyIsReleased(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "stream.jsonl")
	content := pattern(1 << 20)
	write(t, box, path, content)
	for _, o := range []struct {
		name string
		from int
		open func() (io.ReadCloser, error)
	}{
		{"OpenFile", 0, func() (io.ReadCloser, error) { return box.OpenFile(t.Context(), path) }},
		{"OpenTail", 1 << 19, func() (io.ReadCloser, error) {
			r, _, err := box.OpenTail(t.Context(), path, 1<<19)
			return r, err
		}},
		{"OpenRange", 100, func() (io.ReadCloser, error) {
			r, _, err := box.OpenRange(t.Context(), path, 100, 1<<19)
			return r, err
		}},
	} {
		r, err := o.open()
		if err != nil {
			t.Fatalf("%s: %v", o.name, err)
		}
		head := make([]byte, 10)
		if _, err := io.ReadFull(r, head); err != nil {
			t.Fatalf("%s: reading the first bytes: %v", o.name, err)
		}
		if !bytes.Equal(head, content[o.from:o.from+10]) {
			t.Errorf("%s began %v; want the file's bytes from %d", o.name, head, o.from)
		}
		if err := r.Close(); err != nil {
			t.Errorf("%s: closing a stream early: %v", o.name, err)
		}
	}
}

// A REWRITE IS NEVER READ HALF DONE. The engine writes into a box a run is
// using: the completion poll rewrites a seat's GitHub App token file under the
// running job, whose git credential helper reads it at every call. A file
// truncated then written in place is empty, then part of the new body, for as
// long as the write takes, and a push that read it then
// authenticated with nothing. So every read made while the file is rewritten
// again and again is one whole body or the other.
//
// The two bodies differ in length and in every byte, so a torn read is neither.
// Each is written whole and streamed in turn ([sandbox.Sandbox.WriteStream]),
// a piece at a time, so a streamed write filling the file in place is caught
// as a written one would be.
func testARewriteIsNeverReadHalfDone(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "github-token")
	long := pattern(wholeReadBytes)
	short := bytes.Repeat([]byte{0xff}, wholeReadBytes/3)
	write(t, box, path, long)

	// The reader reports how it ended rather than failing the test itself:
	// only the test's own goroutine may stop it.
	type ending struct {
		reads int
		torn  []byte
		err   error
	}
	stop := make(chan struct{})
	ended := make(chan ending, 1)
	go func() {
		for reads := 0; ; reads++ {
			select {
			case <-stop:
				ended <- ending{reads: reads}
				return
			default:
			}
			got, err := box.ReadFile(context.WithoutCancel(t.Context()), path)
			if err != nil {
				ended <- ending{reads: reads, err: err}
				return
			}
			if !bytes.Equal(got, long) && !bytes.Equal(got, short) {
				ended <- ending{reads: reads, torn: got}
				return
			}
		}
	}()
	var failed error
	for i := range rewrites {
		body := short
		if i%2 == 1 {
			body = long
		}
		if (i/2)%2 == 0 {
			failed = box.WriteFile(t.Context(), path, body)
		} else {
			failed = box.WriteStream(t.Context(), path, &pieces{rest: body, size: streamPiece})
		}
		if failed != nil {
			break
		}
	}
	close(stop)
	end := <-ended
	if failed != nil {
		t.Fatalf("rewriting %s: %v", path, failed)
	}
	switch {
	case end.err != nil:
		t.Fatalf("a read made while the file was rewritten failed: %v", end.err)
	case end.torn != nil:
		t.Fatalf("a read made while the file was rewritten saw %d bytes that are neither body written "+
			"(%d and %d bytes): a rewrite was read half done", len(end.torn), len(long), len(short))
	}
	if got, err := box.ReadFile(t.Context(), path); err != nil || !bytes.Equal(got, long) {
		t.Errorf("after the rewrites the file holds %d bytes, %v; want the last body written, %d bytes",
			len(got), err, len(long))
	}
}

// rewrites is how many times [testARewriteIsNeverReadHalfDone] rewrites its
// file under the reader. A write made in place was caught half done by two
// rewrites in twenty runs of twenty on both cells; sixteen times that is the
// margin for a runner the race detector and its neighbours slow down.
const rewrites = 32

// streamPiece is the most one read of a streamed write's source hands over in
// these cases: prime, so no piece lines up with a backend's own buffer, and far
// below [wholeReadBytes], so a file is many pieces.
const streamPiece = 997

// pieces is a source that hands rest over at most size bytes a read, as a
// network stream does, never the whole in one.
type pieces struct {
	rest []byte
	size int
}

func (p *pieces) Read(b []byte) (int, error) {
	if len(p.rest) == 0 {
		return 0, io.EOF
	}
	n := copy(b[:min(len(b), p.size)], p.rest)
	p.rest = p.rest[n:]
	return n, nil
}

// patternSource is [pattern]'s first n bytes made as they are read, so a
// source past the read cap is never a buffer of its own beside the file. It
// fills what each read asks for: what is certified with it is the size, which
// at the real cap is 32 MiB, and the pieces are [pieces]' to certify.
type patternSource struct {
	at, n int
}

func (s *patternSource) Read(b []byte) (int, error) {
	if s.at >= s.n {
		return 0, io.EOF
	}
	const period = 251
	k := min(len(b), s.n-s.at)
	for i := range k {
		b[i] = byte((s.at + i) % period)
	}
	s.at += k
	return k, nil
}

// errSourceBroke is a streamed write's source failing part way.
var errSourceBroke = errors.New("sandboxtest: the source broke off part way")

// brokenSource hands over head, a piece at a time, and then fails.
type brokenSource struct{ head pieces }

func (s *brokenSource) Read(b []byte) (int, error) {
	n, err := s.head.Read(b)
	if errors.Is(err, io.EOF) {
		return 0, errSourceBroke
	}
	return n, err
}

func stream(t *testing.T, box sandbox.Sandbox, path string, content io.Reader) {
	t.Helper()
	if err := box.WriteStream(t.Context(), path, content); err != nil {
		t.Fatalf("WriteStream %s: %v", path, err)
	}
}

// A STREAMED WRITE IS THE FILE ITS SOURCE YIELDED, byte for byte, however its
// source hands it over: a piece at a time, as an archive read from the object
// store arrives, never the whole in one read.
//
// Mutation: write only the first piece a source hands over, and the file is
// 997 bytes of the 70,000.
func testAStreamedWriteIsReadBackByteForByte(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "session-carried")
	content := pattern(wholeReadBytes)
	stream(t, box, path, &pieces{rest: content, size: streamPiece})
	if got, err := box.ReadFile(t.Context(), path); err != nil || !bytes.Equal(got, content) {
		t.Errorf("ReadFile after a streamed write = %d bytes, %v; want the %d streamed", len(got), err, len(content))
	}
}

// A SOURCE THAT YIELDS NOTHING WRITES AN EMPTY FILE, and replaces what was at
// the path: an empty archive is the box's to refuse, never a write that left
// the last one in place.
func testAnEmptyStreamWritesAnEmptyFile(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "session-carried")
	write(t, box, path, []byte("the archive before"))
	stream(t, box, path, strings.NewReader(""))
	if got, err := box.ReadFile(t.Context(), path); err != nil || len(got) != 0 {
		t.Errorf("ReadFile after an empty stream = %q, %v; want an empty file", got, err)
	}
	if tail, err := box.ReadTail(t.Context(), path, 64); err != nil || tail.Size != 0 {
		t.Errorf("ReadTail after an empty stream = %d bytes of %d, %v; want nothing", len(tail.Data), tail.Size, err)
	}
}

// A STREAMED WRITE IS NOT BOUNDED BY WHAT A WHOLE READ TAKES. The read cap is a
// refusal of a file meant to be read whole; an archive is streamed in and out,
// and a write that held its content to the cap — or held it in a buffer the
// cap sized — would cut the longest sessions, which are the ones worth
// carrying. Written from a source made as it is read, past the box's own cap,
// and read back as a stream.
//
// Mutation: hold the write to [sandbox.MaxFileBytes], and the file is cut at
// the cap.
func testAStreamPastTheReadCapIsWrittenWhole(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "session-carried")
	n := ReadCap + 1
	stream(t, box, path, &patternSource{n: n})

	if got, err := box.ReadFile(t.Context(), path); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("ReadFile of a streamed file past the cap = %d bytes, %v; want ErrFileTooLarge", len(got), err)
	}
	r, err := box.OpenFile(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	got := sha256.New()
	read, err := io.Copy(got, r)
	_ = r.Close()
	if err != nil || read != int64(n) {
		t.Fatalf("the stream read back %d bytes, %v; want all %d written", read, err, n)
	}
	want := sha256.New()
	if _, err := io.Copy(want, &patternSource{n: n}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Sum(nil), want.Sum(nil)) {
		t.Error("the file holds the right length and the wrong bytes")
	}
}

// A STREAMED WRITE REPLACES THE FILE WHOLE: a shorter body over a longer one
// leaves the shorter alone, never the longer one's end after it, as a write in
// place that did not truncate would.
func testAStreamReplacesTheFileWhole(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "session-carried")
	write(t, box, path, pattern(wholeReadBytes))
	short := bytes.Repeat([]byte{0xab}, wholeReadBytes/7)
	stream(t, box, path, &pieces{rest: short, size: streamPiece})
	if got, err := box.ReadFile(t.Context(), path); err != nil || !bytes.Equal(got, short) {
		t.Errorf("ReadFile after streaming a shorter body = %d bytes, %v; want the %d streamed", len(got), err, len(short))
	}
}

// A SOURCE THAT FAILS PART WAY FAILS THE WRITE, AND THE FILE IS AS IT WAS. An
// archive cut short in transit — the object store's stream broken, the engine's
// context ended — is never a file the box reads as the session: what the source
// had yielded is dropped with the staged file, and the file at the path is the
// one that was there.
//
// Mutation: write the source over the file in place, and the file holds the
// half the source yielded.
func testAStreamThatFailsLeavesTheFileAsItWas(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "session-carried")
	before := []byte("the archive the box held")
	write(t, box, path, before)
	err := box.WriteStream(t.Context(), path, &brokenSource{head: pieces{rest: pattern(wholeReadBytes / 2), size: streamPiece}})
	if err == nil {
		t.Fatal("a streamed write whose source broke off succeeded")
	}
	if got, err := box.ReadFile(t.Context(), path); err != nil || !bytes.Equal(got, before) {
		t.Errorf("after the failed write the file holds %d bytes, %v; want the %q it held", len(got), err, before)
	}
}
