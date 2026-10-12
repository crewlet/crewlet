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

// A LOCAL BOX WRITES ONLY A REGULAR FILE, WHERE IT LIES, AND NEVER WAITS TO
// FIND OUT WHAT IS THERE. The engine writes into a box after something has run
// in it — a coding CLI's configuration after the setup steps' commands, or into
// a box an earlier run had to itself — and a plain write followed a link at the
// path: one that leads nowhere passes the escape check as itself, so the write
// CREATED a file wherever the link pointed on the engine host. A named pipe
// nobody reads blocked the write, and the launch behind it, for good.
//
// Mutation: write with os.WriteFile, and the dangling link creates the host
// file and the pipe never answers.
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
				create(t, filepath.Join(hostHome, ".crewlet", "mcp.json"), outside)

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
			})
		}
	}
}

// And a write still lands where the box asked, in place: over a file it
// holds, and as a new one.
func TestALocalBoxWritesARegularFileInPlace(t *testing.T) {
	t.Parallel()
	box := directBox(t)
	ctx := t.Context()
	path := box.Home() + "/.crewlet/notes.md"
	for _, content := range []string{"a longer first version", "short"} {
		if err := box.WriteFile(ctx, path, []byte(content)); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := box.ReadFile(ctx, path)
		if err != nil || string(got) != content {
			t.Fatalf("read back %q, %v; want %q", got, err, content)
		}
	}
}
