//go:build unix

package sandboxtest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// HostWrites drives the WRITE CONTRACT of a box the engine writes from its own
// host — a `direct` box, and a `container` box's host side of its mount — for
// both writes ([sandbox.Sandbox.WriteFile], [sandbox.Sandbox.WriteStream]): a
// write replaces only a regular file, or puts one where nothing is, and refuses
// anything else at the path AT ONCE — a named pipe, a directory, a socket, and
// a link wherever it leads — naming what it is, leaving it as the box made it,
// writing nothing past the box and staging nothing beside it; and a streamed
// write whose source fails part way stages nothing either.
//
// ONLY SUCH A BOX REFUSES THEM. A remote box is a machine of its own, so what
// is at a path there is replaced, a link included ([sandbox.Sandbox.WriteFile]);
// and the suite puts what it refuses at the path from the engine host's side,
// which no box reached through the interface alone can be made to hold — so
// [Box] certifies what every backend writes, and this what the engine host's
// boxes refuse.
//
// WHY. The engine writes into a box after something has run in it — a coding
// harness's configuration after the setup steps' commands, a token file under a
// running job, a carried session's archive into a box a previous job used —
// and what the box put at the path is the box's own: a plain write followed a
// link there (one that leads nowhere passes the escape check as itself, so the
// write CREATED a file wherever the link pointed on the engine host), and a
// named pipe nobody reads blocked the write, and the launch behind it, for
// good.
//
// fresh returns an empty box and the host directory its home is.
//
// Mutation: write with os.WriteFile, and the dangling link creates the host
// file and the pipe never answers; or drop the look before the replacement,
// and the link, the pipe and the socket are replaced by a file.
func HostWrites(t *testing.T, fresh func(t *testing.T) (sandbox.Sandbox, string)) {
	t.Helper()
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
	writes := map[string]func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error{
		"WriteFile": func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error {
			return box.WriteFile(ctx, path, content)
		},
		"WriteStream": func(ctx context.Context, box sandbox.Sandbox, path string, content []byte) error {
			return box.WriteStream(ctx, path, &pieces{rest: content, size: streamPiece})
		},
	}
	for shape, write := range writes {
		for kind, create := range kinds {
			t.Run(shape+"/"+kind, func(t *testing.T) {
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
					answered <- write(ctx, box, box.Home()+"/.crewlet/mcp.json", []byte(`{"mcp":{}}`))
				}()
				select {
				case err := <-answered:
					if !errors.Is(err, sandbox.ErrNotRegularFile) || !strings.Contains(err.Error(), kind) ||
						!strings.Contains(err.Error(), "not written") {
						t.Errorf("%s over a %s = %v; want ErrNotRegularFile saying what it is", shape, kind, err)
					}
				case <-time.After(10 * time.Second):
					t.Fatalf("%s over a %s has not answered in 10s", shape, kind)
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
				noneStagedIn(t, filepath.Dir(host))
			})
		}
	}

	// A STREAM THAT FAILS PART WAY STAGES NOTHING: what its source had yielded
	// is dropped with the file staged for it, beside the one the box held,
	// which is left as it was.
	//
	// Mutation: leave the staged file when the copy into it fails, and the
	// box keeps half an archive under a name nothing reads.
	t.Run("AStreamThatFailsStagesNothing", func(t *testing.T) {
		t.Parallel()
		box, hostHome := fresh(t)
		path := box.Home() + "/.crewlet/session-carried"
		before := []byte("the archive the box held")
		if err := box.WriteFile(t.Context(), path, before); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := box.WriteStream(t.Context(), path, &brokenSource{
			head: pieces{rest: pattern(wholeReadBytes), size: streamPiece}}); err == nil {
			t.Fatal("a streamed write whose source broke off succeeded")
		}
		host := filepath.Join(hostHome, ".crewlet", "session-carried")
		if got, err := os.ReadFile(host); err != nil || !bytes.Equal(got, before) { //nolint:gosec // the box's own file
			t.Errorf("the file holds %q, %v; want the %q it held", got, err, before)
		}
		noneStagedIn(t, filepath.Dir(host))
	})
}

// noneStagedIn fails the test where dir holds a file a write staged and never
// moved over its path (hostbox.ReplaceFileIn's `.crewlet-tmp-` names).
func noneStagedIn(t *testing.T, dir string) {
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
