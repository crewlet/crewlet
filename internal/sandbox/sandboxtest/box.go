package sandboxtest

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Box drives the FILE CONTRACT every [sandbox.Sandbox] backend keeps: the
// three reads a coding-agent runner is built on, and the write the suite
// seeds them through.
//
// ONE SUITE FOR EVERY BACKEND, the in-memory twin included, because the twin
// is what every runner and coordinator test reads a box through. It answered
// a 33 MiB file whole while every real backend refused it, so a runner test
// that "collected" a run past the cap certified a read no real box would make
// — the failure this suite exists to stop recurring.
//
// What it does NOT cover is a backend's own geometry (where a path may point,
// how a box is torn down): those differ on purpose and each backend's own
// tests hold them.
//
// fresh returns an empty box per case. Every path is under the box's own
// [sandbox.Sandbox.Home], because that is the only place every backend can
// write.
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
		{"AFilePastTheCapIsRefusedWholeAndReadAsAStream", testAFilePastTheCapIsRefusedWholeAndReadAsAStream},
		{"AFileAtTheCapIsReadWhole", testAFileAtTheCapIsReadWhole},
		{"AStreamClosedEarlyIsReleased", testAStreamClosedEarlyIsReleased},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.fn(t, fresh(t))
		})
	}
}

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
}

func testAWholeReadReturnsTheFileByteForByte(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "report.md")
	content := pattern(70_000)
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

// THE REFUSAL IS FOR A FILE MEANT TO BE READ WHOLE, and only for that. A
// report past the cap is refused rather than clipped, because a clipped report
// reads as a finished one; the same bytes read as a stream or from their end
// are what a run's event log and its stderr are, and refusing those refused a
// run for the size of its own log.
func testAFilePastTheCapIsRefusedWholeAndReadAsAStream(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "stream.jsonl")
	content := pattern(sandbox.MaxFileBytes + 1)
	write(t, box, path, content)

	if got, err := box.ReadFile(t.Context(), path); !errors.Is(err, sandbox.ErrFileTooLarge) {
		t.Errorf("ReadFile past the cap = %d bytes, %v; want ErrFileTooLarge", len(got), err)
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
}

// The cap is inclusive: a file of exactly it is read, so the refusal above is
// the +1 and not an off-by-one that refuses honest output.
func testAFileAtTheCapIsReadWhole(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "findings.md")
	content := pattern(sandbox.MaxFileBytes)
	write(t, box, path, content)
	got, err := box.ReadFile(t.Context(), path)
	if err != nil || len(got) != len(content) {
		t.Errorf("ReadFile of exactly the cap = %d bytes, %v; want all %d", len(got), err, len(content))
	}
}

// A reader that stops early — a decoder that found what it needed — closes
// the stream without draining it, and the close neither fails nor hangs.
func testAStreamClosedEarlyIsReleased(t *testing.T, box sandbox.Sandbox) {
	path := pathIn(box, "stream.jsonl")
	write(t, box, path, pattern(1<<20))
	r, err := box.OpenFile(t.Context(), path)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	head := make([]byte, 10)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("reading the first bytes: %v", err)
	}
	if !bytes.Equal(head, pattern(10)) {
		t.Errorf("the stream began %v; want the file's first bytes", head)
	}
	if err := r.Close(); err != nil {
		t.Errorf("closing a stream early: %v", err)
	}
}
