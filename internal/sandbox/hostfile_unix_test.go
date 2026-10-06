//go:build unix

package sandbox_test

import (
	"io"
	"os"
	"path/filepath"
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
			root := t.TempDir()
			return sandbox.NewContainerBoxAt(root), root
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
						if err == nil || !strings.Contains(err.Error(), kind) {
							t.Errorf("%s of a %s = %v; want an error saying what it is", read, kind, err)
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
