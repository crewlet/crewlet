//go:build unix

package sandbox_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// A LOCAL BOX READS ONLY A REGULAR FILE, AND NEVER WAITS TO FIND OUT WHAT IT
// IS. The other side of a container box's mount is the agent's, and a plain
// open of a named pipe it made blocks until a writer appears, uncancellably: a
// pipe at the done marker wedged the completion poll, and every other box's
// poll and keepalive behind it. Every read — whole, streamed, from the end —
// answers at once with an error saying what the path is, never with an empty
// file, which is the reading of a marker not written yet.
//
// Mutation: open with a plain os.Open, and every read here blocks for good.
func TestALocalBoxRefusesWhatIsNotARegularFileAtOnce(t *testing.T) {
	t.Parallel()
	boxes := map[string]func(*testing.T) (sandbox.Sandbox, string){
		"direct": func(t *testing.T) (sandbox.Sandbox, string) {
			box := directBox(t)
			return box, box.Home()
		},
		"container": func(t *testing.T) (sandbox.Sandbox, string) {
			return sandbox.NewContainerBoxAt(t.TempDir())
		},
	}
	kinds := map[string]func(t *testing.T, host string){
		"named pipe": func(t *testing.T, host string) {
			if err := syscall.Mkfifo(host, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
		},
		"directory": func(t *testing.T, host string) {
			if err := os.Mkdir(host, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
		// A link that leads nowhere passes the escape check as itself, and
		// is a link, not a marker not written yet.
		"symbolic link": func(t *testing.T, host string) {
			if err := os.Symlink(host+".nowhere", host); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		},
	}
	if runtime.GOOS == "linux" {
		// An open of a socket fails rather than blocking, so there is no
		// descriptor to ask what it is: it was reported as a box that could
		// not be read, which a collection retries until the run is lost.
		// (Made with mknod, which Linux lets anybody use for a socket.)
		kinds["socket"] = func(t *testing.T, host string) {
			if err := syscall.Mknod(host, syscall.S_IFSOCK|0o600, 0); err != nil {
				t.Fatalf("mknod: %v", err)
			}
		}
	}
	for boxName, fresh := range boxes {
		for kind, create := range kinds {
			t.Run(boxName+"/"+kind, func(t *testing.T) {
				t.Parallel()
				box, hostHome := fresh(t)
				ctx := t.Context()
				if err := box.WriteFile(ctx, box.Home()+"/.crewlet/err.log", []byte("x")); err != nil {
					t.Fatalf("prepare the box: %v", err)
				}
				create(t, filepath.Join(hostHome, ".crewlet", "done"))
				path := box.Home() + "/.crewlet/done"
				reads := map[string]func() error{
					"ReadFile": func() error { _, err := box.ReadFile(ctx, path); return err },
					"ReadTail": func() error { _, err := box.ReadTail(ctx, path, 64); return err },
					"OpenFile": func() error {
						r, err := box.OpenFile(ctx, path)
						if err == nil {
							_, err = io.ReadAll(r)
							_ = r.Close()
						}
						return err
					},
				}
				for read, fn := range reads {
					answered := make(chan error, 1)
					go func() { answered <- fn() }()
					select {
					case err := <-answered:
						// ErrNotRegularFile, so a collection can tell it
						// from a box it could not read and degrade only
						// the piece.
						if !errors.Is(err, sandbox.ErrNotRegularFile) || !strings.Contains(err.Error(), kind) {
							t.Errorf("%s of a %s = %v; want ErrNotRegularFile saying what it is", read, kind, err)
						}
					case <-time.After(10 * time.Second):
						// A blocked open is never released, so the test
						// binary is what ends it; the red case is the point.
						t.Fatalf("%s of a %s has not answered in 10s", read, kind)
					}
				}
			})
		}
	}
}

// A LOCAL BOX REPLACES ONLY A REGULAR FILE, AND NEVER WAITS TO FIND OUT WHAT IS
// THERE, whether a write hands it the content whole or streams it: the write
// contract of a box the engine writes from its own host
// ([sandboxtest.HostWrites], which says why and what a mutation turns red),
// on both cells that are one.
func TestALocalBoxWritesOnlyARegularFileAtOnce(t *testing.T) {
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
			sandboxtest.HostWrites(t, fresh)
		})
	}
}

// A WRITE REPLACES THE FILE, NEVER REWRITES IT. The file at the path is
// swapped for a new one in a single step, so nothing that holds the old one is
// written: another name the box gave the same file — a hard link — keeps the
// old content, where a write in place changed both names at once and left
// each empty for the moment between. And the new file has the mode a box's
// files are written with, whatever the box made of the old one's, so new
// content is never handed a mode the box widened. A write over a file, and as
// a new one, still lands where the box asked, and no staged file is left
// beside it.
//
// Mutation: open the file with O_TRUNC and write it in place, and the hard
// link reads the new content and the widened mode is kept.
func TestALocalBoxReplacesAFileWhole(t *testing.T) {
	t.Parallel()
	boxes := map[string]func(*testing.T) (sandbox.Sandbox, string){
		"direct": func(t *testing.T) (sandbox.Sandbox, string) {
			box := directBox(t)
			return box, box.Home()
		},
		"container": func(t *testing.T) (sandbox.Sandbox, string) {
			return sandbox.NewContainerBoxAt(t.TempDir())
		},
	}
	writes := map[string]func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error{
		"WriteFile": func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error {
			return box.WriteFile(ctx, path, content)
		},
		"WriteStream": func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error {
			return box.WriteStream(ctx, path, bytes.NewReader(content))
		},
	}
	for boxName, fresh := range boxes {
		for shape, write := range writes {
			t.Run(boxName+"/"+shape, func(t *testing.T) {
				t.Parallel()
				replacesWhole(t, fresh, write)
			})
		}
	}
}

// replacesWhole is one cell's and one write's case of
// [TestALocalBoxReplacesAFileWhole].
func replacesWhole(t *testing.T, fresh func(*testing.T) (sandbox.Sandbox, string),
	write func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error,
) {
	box, hostHome := fresh(t)
	ctx := t.Context()
	path := box.Home() + "/.crewlet/notes.md"
	host := filepath.Join(hostHome, ".crewlet", "notes.md")
	for _, content := range []string{"a longer first version", "short"} {
		if err := write(ctx, box, path, []byte(content)); err != nil {
			t.Fatalf("write: %v", err)
		}
		got, err := box.ReadFile(ctx, path)
		if err != nil || string(got) != content {
			t.Fatalf("read back %q, %v; want %q", got, err, content)
		}
	}

	kept := filepath.Join(hostHome, ".crewlet", "kept.md")
	if err := os.Link(host, kept); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := os.Chmod(host, 0o644); err != nil { //nolint:gosec // the box widening its own file
		t.Fatalf("chmod: %v", err)
	}
	if err := write(ctx, box, path, []byte("the new version")); err != nil {
		t.Fatalf("a write over a linked file: %v", err)
	}
	if got := readHost(t, host); got != "the new version" {
		t.Errorf("the path holds %q, want the new version", got)
	}
	if got := readHost(t, kept); got != "short" {
		t.Errorf("the box's other name for the old file holds %q, want the old content: "+
			"the write rewrote the file rather than replacing it", got)
	}
	if info, err := os.Stat(host); err != nil || info.Mode().Perm() != hostbox.FileMode {
		t.Errorf("the new file is %v, %v; want mode %v, whatever the old one had", info.Mode(), err,
			hostbox.FileMode)
	}
	noneStaged(t, filepath.Dir(host))
}

// noneStaged fails the test where dir holds a file a write staged and never
// moved over its path ([hostbox.ReplaceFileIn]).
func noneStaged(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".crewlet-tmp-") {
			t.Errorf("%s holds %s, a file a write staged and left behind", dir, entry.Name())
		}
	}
}

// readHost is a host file's content, failing the test where it cannot be read.
func readHost(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(raw)
}
