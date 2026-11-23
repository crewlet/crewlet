//go:build unix

package sandbox

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/shellword"
)

// keepingRuntime is a stand-in container runtime holding the containers its
// listed file names. `rm` exits with the status its rm file holds, emptying
// the list at 0 and saying the daemon's words otherwise; `ps -a` lists the
// containers by name and state, or fails with the status its ps file holds.
// Every call is logged to its calls file.
type keepingRuntime struct{ dir, path string }

// newKeepingRuntime is a runtime holding container, whose `rm` the daemon
// refuses and whose listing answers.
func newKeepingRuntime(t *testing.T, container string) keepingRuntime {
	t.Helper()
	r := keepingRuntime{dir: t.TempDir()}
	r.path = filepath.Join(r.dir, "docker")
	writeExecutable(t, r.path, `#!/bin/sh
d=`+shellword.Quote(r.dir)+`
echo "$1" >> "$d/calls"
case "$1" in
rm)
	code=$(cat "$d/rm")
	[ "$code" = 0 ] && { : > "$d/listed"; exit 0; }
	echo "Error response from daemon: cannot remove container \"/$3\": could not kill: device or resource busy" >&2
	exit "$code";;
ps)
	code=$(cat "$d/ps")
	[ "$code" = 0 ] || { echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock" >&2; exit "$code"; }
	while read -r name; do printf '%s\trunning\n' "$name"; done < "$d/listed";;
esac
`)
	r.set(t, "rm", "1")
	r.set(t, "ps", "0")
	r.set(t, "listed", container+"\n")
	return r
}

func (r keepingRuntime) set(t *testing.T, file, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// asked is how many times the runtime was handed verb.
func (r keepingRuntime) asked(t *testing.T, verb string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(r.dir, "calls"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	n := 0
	for _, call := range strings.Fields(string(raw)) {
		if call == verb {
			n++
		}
	}
	return n
}

// keptBox is a container cell driven through runtime, and its box box1 on the
// engine host, recording the run t-gone as a launch records one.
func keptBox(t *testing.T, runtime keepingRuntime) (*Local, boxLayout) {
	t.Helper()
	local := &Local{opts: LocalOptions{Placement: Container}, root: t.TempDir(), cli: containerCLI{path: runtime.path}}
	layout, err := local.layout("box1")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{layout.home(), layout.records()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := recordRun(layout, boxRun{TurnID: "t-gone", Placement: Container}); err != nil {
		t.Fatalf("recordRun: %v", err)
	}
	return local, layout
}

// listedBy reports whether local lists box as a run's box, as its host's sweep
// finds one to reclaim.
func listedBy(t *testing.T, local *Local, box string) bool {
	t.Helper()
	boxes, err := local.RunBoxes()
	if err != nil {
		t.Fatalf("RunBoxes: %v", err)
	}
	return slices.ContainsFunc(boxes, func(b HostBox) bool { return b.SandboxID == box })
}

// A CONTAINER BOX IS DELETED ONLY WITH ITS CONTAINER. Its directory holds the
// record every reclaimer lists it by, and the orphan reaper judges a container
// by its existence, so a box deleted while its container stays is a container
// nothing finds again. A kill or a close whose `rm -f` the runtime refuses is
// an error with the runtime's words while the runtime still lists the
// container, or cannot say: the box is kept, still listed, for the next
// reclaim. One refused for a container already gone (Podman's `rm -f` of a
// missing one) was harmless, and the box goes as after a removal.
//
// Mutations: delete the box whatever `rm -f` answered, and a kept box is
// gone; take a failed listing as the container gone, and that box is gone;
// take any refusal as fatal, and the box of a container already gone stays.
func TestAContainerBoxIsDeletedOnlyWithItsContainer(t *testing.T) {
	t.Parallel()
	ends := map[string]func(ctx context.Context, local *Local) error{
		"kill": func(ctx context.Context, local *Local) error { return local.Kill(ctx, "box1") },
		"close": func(ctx context.Context, local *Local) error {
			box, err := local.reattach("box1")
			if err != nil {
				return err
			}
			return box.Close(ctx)
		},
	}
	for _, c := range []struct {
		name       string
		rm, ps     string
		gone, kept bool
	}{
		{name: "removed", rm: "0", ps: "0"},
		{name: "refused while still listed", rm: "1", ps: "0", kept: true},
		{name: "refused and not listed", rm: "1", ps: "1", kept: true},
		{name: "refused for one already gone", rm: "1", ps: "0", gone: true},
	} {
		for end, ending := range ends {
			t.Run(end+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				runtime := newKeepingRuntime(t, ContainerPrefix+"box1")
				runtime.set(t, "rm", c.rm)
				runtime.set(t, "ps", c.ps)
				if c.gone {
					runtime.set(t, "listed", "")
				}
				local, layout := keptBox(t, runtime)

				err := ending(t.Context(), local)
				_, statErr := os.Stat(layout.root)
				if !c.kept {
					if err != nil || !errors.Is(statErr, fs.ErrNotExist) {
						t.Fatalf("a %s whose container is gone = %v, and its box: %v; want nil, and the box gone",
							end, err, statErr)
					}
					return
				}
				if err == nil {
					t.Fatalf("a %s whose container the runtime kept answered no error", end)
				}
				for _, want := range []string{ContainerPrefix + "box1", "device or resource busy"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("the %s's error does not say %q: %v", end, want, err)
					}
				}
				if statErr != nil || !listedBy(t, local, "box1") {
					t.Fatalf("a %s whose container the runtime kept deleted its box (%v): nothing lists it again",
						end, statErr)
				}
			})
		}
	}
}

// A REMOVAL THE RUNTIME DID NOT ANSWER IN TIME IS NOT ASKED AFTER. Asking
// whether the container went would wait as long again on the runtime that let
// it time out, which a teardown or a check runner's walk would sit through
// twice; the box is kept as for any other refusal. The stand-in answers 124,
// the status runHost reads a timeout as, rather than holding the case for
// ControlTimeout.
//
// Mutation: list the containers after a timed-out removal, red.
func TestARemovalThatTimedOutIsNotAskedAfter(t *testing.T) {
	t.Parallel()
	runtime := newKeepingRuntime(t, ContainerPrefix+"box1")
	runtime.set(t, "rm", "124")
	local, layout := keptBox(t, runtime)

	if err := local.Kill(t.Context(), "box1"); err == nil {
		t.Fatal("a kill whose removal timed out answered no error")
	}
	if n := runtime.asked(t, "ps"); n != 0 {
		t.Errorf("a kill whose removal timed out asked the runtime for its containers %d times", n)
	}
	if _, err := os.Stat(layout.root); err != nil {
		t.Fatalf("a kill whose removal timed out deleted its box: %v", err)
	}
}

// THE HOST'S SWEEP TRIES AGAIN A BOX WHOSE CONTAINER STAYED. A box no run
// names is killed by its host's sweep; where its runtime refuses the removal,
// the box is kept and listed, so the next pass kills it again, and once the
// runtime removes the container the box goes.
//
// Mutation: delete the box on a refused removal (Kill's error dropped), and the
// first pass leaves the container running with nothing to find it, red.
func TestTheHostsSweepTriesAgainABoxWhoseContainerStayed(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	runtime := newKeepingRuntime(t, ContainerPrefix+"box1")
	local, layout := keptBox(t, runtime)
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Container: local},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.manager = manager
	rig.current.Store(manager)

	for pass := 1; pass <= 2; pass++ {
		rig.tick()
		if n := runtime.asked(t, "rm"); n != pass {
			t.Fatalf("after %d passes the runtime was asked to remove the container %d times", pass, n)
		}
		if _, err := os.Stat(layout.root); err != nil || !listedBy(t, local, "box1") {
			t.Fatalf("pass %d deleted a box whose container the runtime kept (%v): nothing lists it again",
				pass, err)
		}
	}

	runtime.set(t, "rm", "0")
	rig.tick()
	if _, err := os.Stat(layout.root); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the box outlived its container's removal: %v", err)
	}
}
