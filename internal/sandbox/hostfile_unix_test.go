//go:build unix

package sandbox_test

import (
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
// THERE. The engine writes into a box after something has run in it — a coding
// harness's configuration after the setup steps' commands, a token file under a
// running job — and what the box put at the path is the box's own: a plain
// write followed a link there (one that leads nowhere passes the escape check
// as itself, so the write CREATED a file wherever the link pointed on the
// engine host), and a named pipe nobody reads blocked the write, and the launch
// behind it, for good. A write replaces a regular file whole or puts one where
// nothing is; anything else is refused at once, saying what it is, and left as
// the box made it, with no staged file beside it.
//
// Mutation: write with os.WriteFile, and the dangling link creates the host
// file and the pipe never answers; or drop the look before the replacement,
// and the link, the pipe and the socket are replaced by a file.
func TestALocalBoxWritesOnlyARegularFileAtOnce(t *testing.T) {
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
	kinds := map[string]func(t *testing.T, host, outside string){
		"named pipe": func(t *testing.T, host, _ string) {
			if err := syscall.Mkfifo(host, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
		},
		"directory": func(t *testing.T, host, _ string) {
			if err := os.Mkdir(host, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		},
		"symbolic link": func(t *testing.T, host, outside string) {
			if err := os.Symlink(outside, host); err != nil {
				t.Fatalf("symlink: %v", err)
			}
		},
	}
	if runtime.GOOS == "linux" {
		// Made with mknod, which Linux lets anybody use for a socket: a
		// socket something in the box listens on is the box's, and a write
		// that replaced it with a file would cut whatever was listening off.
		kinds["socket"] = func(t *testing.T, host, _ string) {
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
				if err := box.WriteFile(ctx, box.Home()+"/.crewlet/brief.md", []byte("x")); err != nil {
					t.Fatalf("prepare the box: %v", err)
				}
				// Somewhere on the engine host that is not in the box and
				// does not exist yet: what a dangling link names.
				outside := filepath.Join(t.TempDir(), "engine-host-file")
				host := filepath.Join(hostHome, ".crewlet", "mcp.json")
				create(t, host, outside)
				before, err := os.Lstat(host)
				if err != nil {
					t.Fatal(err)
				}

				answered := make(chan error, 1)
				go func() {
					answered <- box.WriteFile(ctx, box.Home()+"/.crewlet/mcp.json", []byte(`{"mcp":{}}`))
				}()
				select {
				case err := <-answered:
					if !errors.Is(err, sandbox.ErrNotRegularFile) || !strings.Contains(err.Error(), kind) ||
						!strings.Contains(err.Error(), "not written") {
						t.Errorf("a write over a %s = %v; want ErrNotRegularFile saying what it is", kind, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("a write over a %s has not answered in 10s", kind)
				}
				if _, err := os.Lstat(outside); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("the write reached the engine host past the box: %v", err)
				}
				if after, err := os.Lstat(host); err != nil || !os.SameFile(before, after) ||
					after.Mode().Type() != before.Mode().Type() {
					now := "gone"
					if after != nil {
						now = "a new " + after.Mode().String()
					}
					t.Errorf("the %s at the path is now %s (%v): a refused write leaves what the box made",
						kind, now, err)
				}
				noneStaged(t, filepath.Dir(host))
			})
		}
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
	for boxName, fresh := range boxes {
		t.Run(boxName, func(t *testing.T) {
			t.Parallel()
			box, hostHome := fresh(t)
			ctx := t.Context()
			path := box.Home() + "/.crewlet/notes.md"
			host := filepath.Join(hostHome, ".crewlet", "notes.md")
			for _, content := range []string{"a longer first version", "short"} {
				if err := box.WriteFile(ctx, path, []byte(content)); err != nil {
					t.Fatalf("WriteFile: %v", err)
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
			if err := box.WriteFile(ctx, path, []byte("the new version")); err != nil {
				t.Fatalf("WriteFile over a linked file: %v", err)
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
		})
	}
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
