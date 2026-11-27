package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// THE FILE CONTRACT, ON EVERY BACKEND. The twin first: every runner and
// coordinator test reads a box through it, so a twin answering what no real
// box would makes each of them a claim about nothing.
//
// The twin and the two local boxes run at [sandboxtest.ReadCap]; their
// production reads by [sandbox.MaxFileBytes] are
// [TestEveryBoxReadsWholeUpToMaxFileBytes]'s.
func TestTheFakeKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, func(*testing.T) (sandbox.Sandbox, sandboxtest.InPlace) {
		box := sandbox.NewFakeSandbox("box-1")
		return box.CapReads(sandboxtest.ReadCap), sandboxtest.InPlace{
			Append:   func(_ *testing.T, path string, more []byte) { box.AppendInPlace(path, more) },
			Truncate: func(_ *testing.T, path string, n int64) { box.TruncateInPlace(path, int(n)) },
		}
	})
}

func TestADirectBoxKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, func(t *testing.T) (sandbox.Sandbox, sandboxtest.InPlace) {
		box := directBox(t)
		return sandbox.CapReads(box, sandboxtest.ReadCap), hostEdits(box.Home(), box.Home())
	})
}

// A container box reads and writes on the host side of its mount, so its
// contract is certified there; what the container itself does is local_test's.
func TestAContainerBoxKeepsTheFileContract(t *testing.T) {
	t.Parallel()
	sandboxtest.Box(t, func(t *testing.T) (sandbox.Sandbox, sandboxtest.InPlace) {
		box, home := sandbox.NewContainerBoxAt(t.TempDir())
		return sandbox.CapReads(box, sandboxtest.ReadCap), hostEdits(box.Home(), home)
	})
}

// hostEdits edits a local box's files in place on the engine host, a box path
// under boxHome being the host path under hostHome.
func hostEdits(boxHome, hostHome string) sandboxtest.InPlace {
	at := func(path string) string { return hostHome + strings.TrimPrefix(path, boxHome) }
	return sandboxtest.InPlace{
		Append: func(t *testing.T, path string, more []byte) {
			f, err := os.OpenFile(at(path), os.O_WRONLY|os.O_APPEND, 0)
			if err == nil {
				_, err = f.Write(more)
				err = errors.Join(err, f.Close())
			}
			if err != nil {
				t.Fatalf("appending to %s: %v", path, err)
			}
		},
		Truncate: func(t *testing.T, path string, n int64) {
			if err := os.Truncate(at(path), n); err != nil {
				t.Fatalf("truncating %s: %v", path, err)
			}
		},
	}
}

// A LOCAL BOX'S RANGE ENDS AT THE SIZE ITS OPEN ANSWERED, though the file grows
// in place under it, as a run's event stream does while it is read: bytes past
// that size are the next read's, which starts from the size this one counted.
// Mutation: section the file for n rather than to the size, and the range
// carries the appended bytes.
func TestALocalBoxsRangeEndsAtTheSizeItsOpenAnswered(t *testing.T) {
	t.Parallel()
	for name, fresh := range map[string]func(*testing.T) (sandbox.Sandbox, string){
		"direct": func(t *testing.T) (sandbox.Sandbox, string) {
			box := directBox(t)
			return box, box.Home()
		},
		"container": func(t *testing.T) (sandbox.Sandbox, string) {
			return sandbox.NewContainerBoxAt(t.TempDir())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box, home := fresh(t)
			content := bytes.Repeat([]byte("0123456789"), 100)
			if err := box.WriteFile(t.Context(), box.Home()+"/.crewlet/events.jsonl", content); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			r, size, err := box.OpenRange(t.Context(), box.Home()+"/.crewlet/events.jsonl", 100, math.MaxInt64)
			if err != nil {
				t.Fatalf("OpenRange: %v", err)
			}
			defer func() { _ = r.Close() }()
			f, err := os.OpenFile(filepath.Join(home, ".crewlet", "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("open the host file to append: %v", err)
			}
			_, err = f.WriteString("appended after the open\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, content[100:]) || size != int64(len(content)) {
				t.Errorf("the range = %d bytes of %d, %v; want the %d from 100 of the %d the open found",
					len(got), size, err, len(content)-100, len(content))
			}
		})
	}
}

// A LOCAL BOX'S WHOLE OPEN IS THE FILE AS ITS OPEN SIZED IT, though the file
// grows in place under it before the read, so a collection reads no more than
// the size it weighed: the size comes from the open file, never the path.
//
// Mutation: section the file to the cap rather than to the size, and the read
// carries the appended bytes.
func TestALocalBoxsWholeOpenIsTheFileAsItsOpenSizedIt(t *testing.T) {
	t.Parallel()
	for name, fresh := range map[string]func(*testing.T) (sandbox.Sandbox, string){
		"direct": func(t *testing.T) (sandbox.Sandbox, string) {
			box := directBox(t)
			return box, box.Home()
		},
		"container": func(t *testing.T) (sandbox.Sandbox, string) {
			return sandbox.NewContainerBoxAt(t.TempDir())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box, home := fresh(t)
			content := bytes.Repeat([]byte("0123456789"), 100)
			if err := box.WriteFile(t.Context(), box.Home()+"/.crewlet/findings.md", content); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			r, size, err := box.OpenWhole(t.Context(), box.Home()+"/.crewlet/findings.md")
			if err != nil {
				t.Fatalf("OpenWhole: %v", err)
			}
			defer func() { _ = r.Close() }()
			f, err := os.OpenFile(filepath.Join(home, ".crewlet", "findings.md"), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatalf("open the host file to append: %v", err)
			}
			_, err = f.WriteString("appended after the open\n")
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				t.Fatalf("append: %v", err)
			}
			got, err := io.ReadAll(r)
			if err != nil || !bytes.Equal(got, content) || size != int64(len(content)) {
				t.Errorf("the whole open = %d bytes of %d, %v; want the %d the open found",
					len(got), size, err, len(content))
			}
		})
	}
}

// THE FAKE'S STREAMS HOOK WRAPS EVERY STREAM IT OPENS, and what it hands out,
// not what it reads beneath, is what the box counts as delivered: a case that
// fails a stream part way or slows it holds every open, not only the one it
// thought of. The hook here hands out five bytes of its own before ten of the
// file's.
//
// Mutation: count the file's bytes beneath the hook, and Delivered is 10 an
// open rather than 15.
func TestTheFakesStreamsHookWrapsEveryStreamItOpens(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box-1")
	const path = "/home/user/.crewlet/events.jsonl"
	if err := box.WriteFile(t.Context(), path, bytes.Repeat([]byte("x"), 100)); err != nil {
		t.Fatal(err)
	}
	cut := errors.New("the stream was cut")
	var wrapped []string
	box.Streams = func(p string, r io.Reader) io.Reader {
		wrapped = append(wrapped, p)
		return io.MultiReader(strings.NewReader("hook:"), io.LimitReader(r, 10), iotest.ErrReader(cut))
	}
	opens := map[string]func() (io.ReadCloser, error){
		"OpenFile": func() (io.ReadCloser, error) { return box.OpenFile(t.Context(), path) },
		"OpenTail": func() (io.ReadCloser, error) {
			r, _, err := box.OpenTail(t.Context(), path, 50)
			return r, err
		},
		"OpenRange": func() (io.ReadCloser, error) {
			r, _, err := box.OpenRange(t.Context(), path, 20, 50)
			return r, err
		},
	}
	for name, open := range opens {
		r, err := open()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got, err := io.ReadAll(r)
		_ = r.Close()
		if len(got) != 15 || !errors.Is(err, cut) {
			t.Errorf("%s through the hook read %d bytes, %v; want 15 and then the hook's failure", name, len(got), err)
		}
	}
	if len(wrapped) != len(opens) || box.Delivered() != int64(15*len(opens)) {
		t.Errorf("the hook wrapped %v and the box delivered %d; want each of the %d opens, 15 bytes each",
			wrapped, box.Delivered(), len(opens))
	}
}

// ONLY ABSENCE IS EMPTY ON THE ENGINE HOST. A local box answers a file it
// cannot open or read with an error, which a collection retries and a person
// can act on; an empty answer would collect as
// a run that wrote no report. A path under a FILE cannot be resolved, and says
// so rather than claiming it lies outside the box.
//
// A DIRECTORY stands in for an unreadable file: it is one on every host and for
// every user, root included.
func TestALocalBoxAnswersAFileItCannotReadAsAnError(t *testing.T) {
	t.Parallel()
	for name, fresh := range map[string]func(*testing.T) sandbox.Sandbox{
		"direct": directBox, "container": containerBox,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := fresh(t)
			ctx := t.Context()
			unreadable := box.Home() + "/.crewlet/findings.md"
			if err := box.WriteFile(ctx, unreadable+"/inside", []byte("x")); err != nil {
				t.Fatalf("make the path a directory: %v", err)
			}
			if got, err := box.ReadFile(ctx, unreadable); err == nil {
				t.Errorf("ReadFile = %q, nil; want the read's failure, not an empty file", got)
			}
			if tail, err := box.ReadTail(ctx, unreadable, 64); err == nil {
				t.Errorf("ReadTail = %+v, nil; want the read's failure", tail)
			}
			stream, err := box.OpenFile(ctx, unreadable)
			if err == nil {
				_, err = io.ReadAll(stream)
				_ = stream.Close()
			}
			if err == nil {
				t.Error("OpenFile read to its end without an error; want the read's failure")
			}
			for name, err := range map[string]error{
				"OpenTail":  readThrough(box.OpenTail(ctx, unreadable, 64)),
				"OpenRange": readThrough(box.OpenRange(ctx, unreadable, 0, 64)),
			} {
				if err == nil {
					t.Errorf("%s read to its end without an error; want the read's failure", name)
				}
			}

			marker := box.Home() + "/.crewlet/done"
			if err := box.WriteFile(ctx, marker, []byte("0")); err != nil {
				t.Fatalf("write: %v", err)
			}
			_, err = box.ReadFile(ctx, marker+"/below")
			if err == nil || strings.Contains(err.Error(), "outside") {
				t.Errorf("a path under a file = %v; want it unresolvable, and not an escape", err)
			}
		})
	}
}

// readThrough is the failure of a stream's open, or else of reading it to its
// end.
func readThrough(r io.ReadCloser, _ int64, err error) error {
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	_, err = io.ReadAll(r)
	return err
}

func directBox(t *testing.T) sandbox.Sandbox {
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{Placement: sandbox.Direct})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = box.Close(context.Background()) })
	return box
}

func containerBox(t *testing.T) sandbox.Sandbox {
	box, _ := sandbox.NewContainerBoxAt(t.TempDir())
	return box
}

// A LOCAL BOX TELLS A FILE WRITTEN OVER IN PLACE AT ITS SIZE: the same inode,
// the same length, new bytes and a new modification time, as a writer that
// cut it and wrote it again leaves it, is a change, not the file as opened.
//
// Mutation: compare only the file's size to the open's.
func TestALocalBoxTellsAFileWrittenOverInPlace(t *testing.T) {
	t.Parallel()
	for name, fresh := range map[string]func(*testing.T) (sandbox.Sandbox, string){
		"direct": func(t *testing.T) (sandbox.Sandbox, string) {
			box := directBox(t)
			return box, box.Home()
		},
		"container": func(t *testing.T) (sandbox.Sandbox, string) {
			return sandbox.NewContainerBoxAt(t.TempDir())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box, home := fresh(t)
			path := box.Home() + "/.crewlet/over.log"
			if err := box.WriteFile(t.Context(), path, bytes.Repeat([]byte("a"), 1000)); err != nil {
				t.Fatal(err)
			}
			r, _, err := box.OpenRange(t.Context(), path, 0, math.MaxInt64)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			if _, err = io.ReadAll(r); err != nil {
				t.Fatal(err)
			}
			host := home + strings.TrimPrefix(path, box.Home())
			f, err := os.OpenFile(host, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteAt(bytes.Repeat([]byte("b"), 1000), 0)
			if err = errors.Join(err, f.Close()); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(host)
			if err != nil {
				t.Fatal(err)
			}
			later := info.ModTime().Add(time.Second)
			if err := os.Chtimes(host, later, later); err != nil {
				t.Fatal(err)
			}
			if got, err := sandbox.ChangeOf(r); err != nil || got != sandbox.FileChanged {
				t.Errorf("a file written over in place at its size tells %q (%v), want %q", got, err,
					sandbox.FileChanged)
			}
		})
	}
}
