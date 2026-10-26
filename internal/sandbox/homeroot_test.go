//go:build unix

package sandbox

import (
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The cases here hold the engine's host-side access to a local box's files to
// the box's home: every read, write, directory made and file removed goes
// through the home opened as an os.Root ([boxLayout.openHome]), so a directory
// on the way that the box made a link — to anywhere outside it, absolute or
// climbing out — leads the access nowhere. Each plants such a link where the
// engine is about to reach and asserts the access was refused and nothing
// outside the box was read, written or made.

// outsideTheBox is a directory on the engine host beside the box, holding one
// file the box must never be able to have the engine read, and what it holds.
func outsideTheBox(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secret"), "an engine host file")
	return dir
}

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
// than through the home's root, and the write lands outside the box and the
// read hands back the host file.
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
				got, err := box.ReadFile(ctx, in("secret"))
				refused("ReadFile", err)
				if strings.Contains(string(got), "engine host") {
					t.Errorf("ReadFile handed back the host file: %q", got)
				}
				r, err := box.OpenFile(ctx, in("secret"))
				if err == nil {
					raw, _ := io.ReadAll(r)
					_ = r.Close()
					t.Errorf("OpenFile through the link was not refused, and read %q", raw)
				}
				refused("OpenFile", err)
				tail, err := box.ReadTail(ctx, in("secret"), 64)
				refused("ReadTail", err)
				if strings.Contains(string(tail.Data), "engine host") {
					t.Errorf("ReadTail handed back the host file: %q", tail.Data)
				}

				if after := outsideHolds(t, outside); !slices.Equal(after, before) {
					t.Errorf("the engine host beside the box now holds %v, want %v: an access went through the link",
						after, before)
				}
				if got := readFile(t, filepath.Join(outside, "secret")); got != "an engine host file" {
					t.Errorf("the host file was rewritten to %q", got)
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
		t.Errorf("the engine host beside the box now holds %v, want %v", after, before)
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
// login the run left after the run had the home to itself; with the login's
// directory made a link to files on the engine host, those files must not be
// read as the run's refresh and copied over the shared login.
//
// Mutation: read the box's copy by its joined path, and the host's files
// become the shared login.
func TestATeardownWritesBackNoLoginReadFromOutsideTheBox(t *testing.T) {
	t.Parallel()
	dir, files := twoFileBox(t)
	putLogin(t, dir, signedIn("seed"))
	box, err := newDirect(t).Create(t.Context(), Spec{CredentialFiles: files})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	outside := t.TempDir()
	putLogin(t, outside, signedIn("an engine host file"))
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
	if got := sharedNow(t, outside); !maps.Equal(got, signedIn("an engine host file")) {
		t.Errorf("the engine host's files were changed: %v", got)
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
		t.Errorf("the engine host beside the box now holds %v, want %v", after, before)
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
		t.Errorf("the engine host beside the box now holds %v, want %v", after, before)
	}
}
