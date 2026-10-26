//go:build unix

package sandbox

import (
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The cases here hold the engine's host-side access to a local box's files to
// the box's home: every read, write, directory made and file removed goes
// through the home opened as an os.Root ([boxLayout.openHome]), so a directory
// on the way that the box made a link — to anywhere outside it, absolute or
// climbing out — leads the access nowhere. Each plants such a link where the
// engine is about to reach and asserts the access was refused and nothing
// outside the box was read, written or made.

// outsideTheBox is a directory on the engine host beside the box, holding one
// file, `beside`, which a case holds every access to leave unread and unchanged.
func outsideTheBox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "beside"), besideTheBox)
	return dir
}

// besideTheBox is what the file beside the box holds.
const besideTheBox = "a file beside the box"

// outsideHolds is every name under dir, so a case can tell that an access
// through a link made nothing there.
func outsideHolds(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, _ os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err == nil && rel != "." {
			names = append(names, rel)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(names)
	return names
}

// links are the two ways a link leads out of a box: by an absolute path, and
// by a relative one that climbs past the home.
var links = map[string]func(t *testing.T, home, outside string) string{
	"absolute": func(_ *testing.T, _, outside string) string { return outside },
	"climbing out": func(t *testing.T, home, outside string) string {
		t.Helper()
		rel, err := filepath.Rel(home, outside)
		if err != nil {
			t.Fatal(err)
		}
		return rel
	},
}

// localBoxes are the two cells' boxes, each with the wording its escapes
// have always been refused in.
var localBoxes = map[string]struct {
	fresh  func(t *testing.T) Sandbox
	escape string
}{
	"direct": {
		fresh:  func(t *testing.T) Sandbox { return mustCreate(t, newDirect(t), Spec{}) },
		escape: "refuses to touch",
	},
	"container": {
		fresh: func(t *testing.T) Sandbox {
			box, _ := NewContainerBoxAt(t.TempDir())
			return box
		},
		escape: "resolves outside the sandbox home mount",
	},
}

// A LOCAL BOX'S FILES ARE REACHED ONLY BENEATH ITS HOME, whatever the box made
// of a directory on the way: a link in place of .crewlet/ — absolute, or
// climbing out — refuses every read, write and directory made through it,
// in the words an escape has always been refused in, and the engine host
// beside the box is neither read nor written.
//
// Mutation: open the file or make the directory by its joined path rather
// than through the home's root, and the accesses reach the directory beside
// the box rather than being refused.
func TestALocalBoxReachesItsFilesOnlyBeneathItsHome(t *testing.T) {
	t.Parallel()
	for cell, kind := range localBoxes {
		for how, target := range links {
			t.Run(cell+"/"+how, func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				box := kind.fresh(t)
				home := layoutOf(t, box).home()
				outside := outsideTheBox(t)
				if err := os.Symlink(target(t, home, outside), filepath.Join(home, ".crewlet")); err != nil {
					t.Fatal(err)
				}
				before := outsideHolds(t, outside)
				in := func(name string) string { return box.Home() + "/.crewlet/" + name }

				refused := func(op string, err error) {
					t.Helper()
					if err == nil {
						t.Errorf("%s through the link was not refused", op)
						return
					}
					if !strings.Contains(err.Error(), kind.escape) {
						t.Errorf("%s = %v; want it refused as the escape it is (%q)", op, err, kind.escape)
					}
				}
				refused("WriteFile", box.WriteFile(ctx, in("brief.md"), []byte("the brief")))
				refused("WriteFile into a directory it makes", box.WriteFile(ctx, in("tools/shim"), []byte("#!/bin/sh")))
				got, err := box.ReadFile(ctx, in("beside"))
				refused("ReadFile", err)
				if strings.Contains(string(got), besideTheBox) {
					t.Errorf("ReadFile answered the content beside the box, not the refusal: %q", got)
				}
				r, err := box.OpenFile(ctx, in("beside"))
				if err == nil {
					raw, _ := io.ReadAll(r)
					_ = r.Close()
					t.Errorf("OpenFile through the link was not refused, and read %q", raw)
				}
				refused("OpenFile", err)
				tail, err := box.ReadTail(ctx, in("beside"), 64)
				refused("ReadTail", err)
				if strings.Contains(string(tail.Data), besideTheBox) {
					t.Errorf("ReadTail answered the content beside the box, not the refusal: %q", tail.Data)
				}

				if after := outsideHolds(t, outside); !slices.Equal(after, before) {
					t.Errorf("the directory beside the box now holds %v, want %v: nothing outside the "+
						"home is made", after, before)
				}
				if got := readFile(t, filepath.Join(outside, "beside")); got != besideTheBox {
					t.Errorf("the file beside the box now holds %q: nothing outside the home is written", got)
				}
			})
		}
	}
}

// A COMMAND'S WORKING DIRECTORY IS MADE BENEATH THE HOME TOO: a cwd under a
// link the box put in place of a directory is refused before the command is
// started, and nothing is made where the link leads.
//
// Mutation: make the working directory with os.MkdirAll on its joined path,
// and the directory appears beside the box.
func TestADirectBoxMakesACommandsDirectoryOnlyBeneathItsHome(t *testing.T) {
	t.Parallel()
	box := mustCreate(t, newDirect(t), Spec{})
	home := box.Home()
	outside := outsideTheBox(t)
	if err := os.Symlink(outside, filepath.Join(home, "repo")); err != nil {
		t.Fatal(err)
	}
	before := outsideHolds(t, outside)
	_, err := box.Exec(t.Context(), "true", ExecOptions{Cwd: home + "/repo/build"})
	if err == nil || !strings.Contains(err.Error(), "refuses to touch") {
		t.Fatalf("Exec in a directory under the link = %v; want it refused as an escape", err)
	}
	if after := outsideHolds(t, outside); !slices.Equal(after, before) {
		t.Errorf("the directory beside the box now holds %v, want %v", after, before)
	}
}

// A COMMAND STARTS IN ITS BOX'S HOME AND ENTERS ITS WORKING DIRECTORY ITSELF.
// The engine makes the directory through the home's root and never walks into
// it by name: the command's own shell does, relative to the home, so where it
// starts is the box's own walk, made in the box's own process — and every
// confinement starts it where it was asked to, bubblewrap included, which
// enters the home whatever directory it was handed. A name the shell must
// quote, a quote in it included, is entered whole.
//
// Mutation: hand the joined directory to the process rather than to its
// shell, and the command starts in it with no home behind it to have left;
// quote the name loosely, and the shell enters another directory or none.
func TestADirectBoxCommandEntersItsWorkingDirectoryFromItsHome(t *testing.T) {
	t.Parallel()
	box := mustCreate(t, newDirect(t), Spec{})
	home := box.Home()
	cwd := home + "/it's a dir"
	want := func(t *testing.T, got string) {
		t.Helper()
		resolvedHome, err := filepath.EvalSymlinks(home)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(got), "\n")
		if len(lines) != 2 || lines[0] != home || lines[1] != filepath.Join(resolvedHome, "it's a dir") {
			t.Fatalf("the command started in and ran in %q; want it started in the home %s and run in "+
				"its working directory beneath it", lines, home)
		}
	}
	report := `printf '%s\n%s\n' "$OLDPWD" "$(pwd -P)"`

	res, err := box.Exec(t.Context(), report, ExecOptions{Cwd: cwd})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Exec = %+v, %v", res, err)
	}
	want(t, res.Stdout)

	out := filepath.Join(home, "started")
	if _, err := box.StartBackground(t.Context(), report+" > "+out+".tmp && mv "+out+".tmp "+out,
		ExecOptions{Cwd: cwd}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if raw, err := os.ReadFile(out); err == nil {
			want(t, string(raw))
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the background job wrote nothing in 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A LINK THAT STAYS IN THE HOME IS FOLLOWED: a box may arrange its own home as
// it likes, and the root refuses only what leads out of it. A write through a
// link to a sibling directory lands there, and reads back.
func TestALinkThatStaysInTheHomeIsFollowedWhereItLeads(t *testing.T) {
	t.Parallel()
	for cell, kind := range localBoxes {
		t.Run(cell, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			box := kind.fresh(t)
			home := layoutOf(t, box).home()
			if err := os.MkdirAll(filepath.Join(home, "meta"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("meta", filepath.Join(home, ".crewlet")); err != nil {
				t.Fatal(err)
			}
			path := box.Home() + "/.crewlet/brief.md"
			if err := box.WriteFile(ctx, path, []byte("the brief")); err != nil {
				t.Fatalf("WriteFile through a link inside the home: %v", err)
			}
			if got := readFile(t, filepath.Join(home, "meta", "brief.md")); got != "the brief" {
				t.Fatalf("the write landed as %q, want it where the link leads", got)
			}
			if got, err := box.ReadFile(ctx, path); err != nil || string(got) != "the brief" {
				t.Fatalf("ReadFile = %q, %v; want the brief", got, err)
			}
		})
	}
}

// A LOGIN IS WRITTEN BACK ONLY FROM INSIDE THE BOX. The teardown reads the
// login the run left after the run had the home to itself, and only beneath the
// home: a login directory that is a link out of it holds no login of the run's,
// so the shared login keeps its seed and the files the link leads to are left
// as they are.
//
// Mutation: read the box's copy by its joined path, and the shared login takes
// the files beside the box.
func TestATeardownWritesBackNoLoginReadFromOutsideTheBox(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("seed"))
	box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	outside := t.TempDir()
	putLogin(t, outside, signedIn("beside the box"))
	seededDir := filepath.Join(box.Home(), ".fake")
	if err := os.RemoveAll(seededDir); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, seededDir); err != nil {
		t.Fatal(err)
	}
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, want := sharedNow(t, dir), signedIn("seed"); !maps.Equal(got, want) {
		t.Errorf("the shared login is %v, want the seed %v untouched", got, want)
	}
	if got := sharedNow(t, outside); !maps.Equal(got, signedIn("beside the box")) {
		t.Errorf("the files beside the box were changed: %v", got)
	}
}

// A SEEDED LOGIN FILE IS WRITTEN ONLY BENEATH THE HOME: a directory on its
// path that is a link out of the box refuses the write, the temporary file
// and the rename both, and leaves nothing where the link leads.
//
// Mutation: write the seed with [hostbox.ReplaceFile] on its joined path, and
// the login lands beside the box.
func TestASeededLoginFileIsWrittenOnlyBeneathTheHome(t *testing.T) {
	t.Parallel()
	layout := boxLayout{id: "seed", root: t.TempDir()}
	if err := os.MkdirAll(layout.home(), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := outsideTheBox(t)
	if err := os.Symlink(outside, filepath.Join(layout.home(), ".claude")); err != nil {
		t.Fatal(err)
	}
	before := outsideHolds(t, outside)
	home, err := layout.openHome()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = home.Close() }()
	if err := writeBoxFile(home, filepath.Join(".claude", ".credentials.json"), []byte(`{"token":"t"}`)); err == nil {
		t.Fatal("a login file was written through a link out of the box")
	}
	if after := outsideHolds(t, outside); !slices.Equal(after, before) {
		t.Errorf("the directory beside the box now holds %v, want %v", after, before)
	}
}

// THE MOUNT PROOF WRITES INTO WHAT THE CONTAINER MADE ONLY BENEATH THE HOME.
// The container is running when the engine writes into the directory it
// made; a container that made it a link to the engine host gets the box
// refused, and nothing is written, or removed, where the link leads.
//
// Mutation: write the proof's file by its joined path, and it lands beside the
// box.
func TestTheMountProofWritesNothingThroughALinkTheContainerMade(t *testing.T) {
	t.Parallel()
	root := madeBox(t)
	outside := outsideTheBox(t)
	before := outsideHolds(t, outside)
	err := verifyMount(t.Context(), &stubBox{root: homeOf(root), linkTo: outside}, boxLayout{id: "b", root: root})
	if err == nil {
		t.Fatal("verifyMount accepted a directory the container made a link out of the box")
	}
	if !strings.Contains(err.Error(), "cannot manage") {
		t.Errorf("error = %v; want the box refused as one the engine cannot manage", err)
	}
	if after := outsideHolds(t, outside); !slices.Equal(after, before) {
		t.Errorf("the directory beside the box now holds %v, want %v", after, before)
	}
}

// A DIRECTORY ON THE WAY THAT IS NOT ONE IS REFUSED AT ONCE. The root opens
// every directory on a path as a directory, the one the file is in too, so
// anything else the box put in its place — a named pipe where `.crewlet` was, or
// a file — is refused before anything is opened, saying what it is: an open of
// a pipe there waits for a writer, uncancellably, and every read, write and
// teardown behind it waits too. Every access answers at once — the reads with
// [ErrNotRegularFile] naming the path and what it is — and so does the
// teardown's write-back with a pipe in place of the login's directory, leaving
// the shared login as it was.
//
// Mutation: open the directory the file is in as root.Open does — as it is,
// without O_DIRECTORY — and every access to a file under the pipe waits.
func TestALocalBoxRefusesWhatIsNotADirectoryOnTheWayAtOnce(t *testing.T) {
	t.Parallel()
	kinds := map[string]func(t *testing.T, host string){
		"a named pipe": func(t *testing.T, host string) {
			if err := syscall.Mkfifo(host, 0o600); err != nil {
				t.Fatalf("mkfifo: %v", err)
			}
		},
		"a regular file": func(t *testing.T, host string) { writeFile(t, host, "not a directory") },
	}
	within := func(t *testing.T, what string, fn func() error) error {
		t.Helper()
		answered := make(chan error, 1)
		go func() { answered <- fn() }()
		select {
		case err := <-answered:
			return err
		case <-time.After(10 * time.Second):
			// A blocked open is never released, so the test binary is what
			// ends it; the red case is the point.
			t.Fatalf("%s has not answered in 10s", what)
			return nil
		}
	}
	for cell, kind := range localBoxes {
		for what, create := range kinds {
			t.Run(cell+"/"+what, func(t *testing.T) {
				t.Parallel()
				ctx := t.Context()
				box := kind.fresh(t)
				home := layoutOf(t, box).home()
				if err := os.RemoveAll(filepath.Join(home, ".crewlet")); err != nil {
					t.Fatal(err)
				}
				create(t, filepath.Join(home, ".crewlet"))
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
					err := within(t, read, fn)
					if !errors.Is(err, ErrNotRegularFile) || !strings.Contains(err.Error(), what) ||
						!strings.Contains(err.Error(), "not a directory") {
						t.Errorf("%s under %s = %v; want ErrNotRegularFile saying what is on the way", read, what, err)
					}
				}
				if err := within(t, "WriteFile", func() error {
					return box.WriteFile(ctx, path, []byte("x"))
				}); err == nil {
					t.Errorf("WriteFile under %s was not refused", what)
				}
			})
		}
	}

	t.Run("the teardown's write-back", func(t *testing.T) {
		t.Parallel()
		dir, files := twoFileBox(t)
		putLogin(t, dir, signedIn("seed"))
		box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		seededDir := filepath.Join(box.Home(), ".fake")
		if err := os.RemoveAll(seededDir); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(seededDir, 0o600); err != nil {
			t.Fatalf("mkfifo: %v", err)
		}
		_ = within(t, "Close", func() error { return box.Close(t.Context()) })
		if got, want := sharedNow(t, dir), signedIn("seed"); !maps.Equal(got, want) {
			t.Errorf("the shared login is %v, want the seed %v untouched", got, want)
		}
	})
}
