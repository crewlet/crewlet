package sandbox

import (
	"io"
	"math"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// A FILE THAT CANNOT BE OPENED IS AN ERROR, never an empty one — the open
// half of what TestALocalBoxAnswersAFileItCannotReadAsAnError holds for a
// read. A Unix socket stands in for a file the engine may not open, because
// opening one fails on every host and for every user, the root this suite may
// run as included (permissions would not: root reads past them).
func TestALocalFileThatCannotBeOpenedIsAnError(t *testing.T) {
	t.Parallel()
	// NOT UNDER t.TempDir, which is named for this test: a socket's path is
	// bounded (104 bytes on macOS, 108 on Linux), and macOS's per-user temporary
	// directory plus the test's name runs past it. A short directory of its own
	// and a one-letter name fit both.
	dir, err := os.MkdirTemp("", "sock") //nolint:usetesting // t.TempDir is named for the test, past a socket path's bound
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	box := boxLayout{id: "b", root: dir}
	if err := os.MkdirAll(box.home(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	l, err := net.Listen("unix", filepath.Join(box.home(), "s"))
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	if got, err := readHomeFile(box, "s", "findings.md", MaxFileBytes); err == nil {
		t.Errorf("readHomeFile = %q, nil; want the open's failure, not an empty file", got)
	}
	if r, err := openHomeFile(box, "s", "stream.jsonl"); err == nil {
		_ = r.Close()
		t.Error("openHomeFile = a reader, nil; want the open's failure, not an empty stream")
	}
	if tail, err := readHomeTail(box, "s", "err.log", 64); err == nil {
		t.Errorf("readHomeTail = %+v, nil; want the open's failure", tail)
	}
	// And absence is still empty, for all three.
	if got, err := readHomeFile(box, "absent", "done", MaxFileBytes); err != nil || got != nil {
		t.Errorf("a missing file = %q, %v; want empty", got, err)
	}
	if tail, err := readHomeTail(box, "absent", "done", 64); err != nil || tail.Size != 0 {
		t.Errorf("a missing tail = %+v, %v; want empty", tail, err)
	}
}

// A HOST READ IS SIZED AND CHECKED BY ONE STAT OF ITS OPEN, on every open: a
// file the job's writer grows the moment that stat is taken reads, where the
// open is a section, to the size the stat found, and tells an append
// ([ChangeOf]). Two stats, one sizing the read and a later one for its check
// to compare with, would let growth between them pass a cut read as the file.
//
// Mutation: stat the open a second time, for its size or for its check.
func TestAHostReadIsSizedAndCheckedByOneStatOfItsOpen(t *testing.T) {
	t.Parallel()
	const before, after = "the first line\n", "a line written after\n"
	for name, c := range map[string]struct {
		open    func(l boxLayout) (io.ReadCloser, error)
		section bool
	}{
		// A read to the end reads what was written after too.
		"OpenFile": {func(l boxLayout) (io.ReadCloser, error) { return openHomeFile(l, "s.log", "s.log") }, false},
		"OpenTail": {func(l boxLayout) (io.ReadCloser, error) {
			r, _, err := openHomeTail(l, "s.log", "s.log", math.MaxInt64)
			return r, err
		}, true},
		"OpenRange": {func(l boxLayout) (io.ReadCloser, error) {
			r, _, err := openHomeRange(l, "s.log", "s.log", 0, math.MaxInt64)
			return r, err
		}, true},
		"OpenWhole": {func(l boxLayout) (io.ReadCloser, error) {
			r, _, err := openHomeWhole(l, "s.log", "s.log", MaxFileBytes)
			return r, err
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			l := boxLayout{id: "b", root: t.TempDir()}
			path := filepath.Join(l.home(), "s.log")
			if err := os.MkdirAll(l.home(), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
				t.Fatal(err)
			}
			l.statTaken = func(*os.File) {
				w, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = w.Close() }()
				if _, err := w.WriteString(after); err != nil {
					t.Fatal(err)
				}
			}
			r, err := c.open(l)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = r.Close() }()
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if c.section && string(got) != before {
				t.Errorf("a section read %q, want the %d bytes its open's stat found", got, len(before))
			}
			if change, err := ChangeOf(r); err != nil || change != FileAppended {
				t.Errorf("a read of a file grown after its open's stat tells %q (%v), want %q", change, err,
					FileAppended)
			}
		})
	}
}
