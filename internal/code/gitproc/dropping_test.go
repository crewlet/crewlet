package gitproc_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/hostbox"
)

// THE DROPPING VIEW IS NAMED WHERE IT IS SERVED, AND NOWHERE ELSE: a process
// that names a cache's dropping view (Cmd.Dropping) reads it as
// GIT_ALTERNATE_OBJECT_DIRECTORIES and nothing more; a process leaning on an
// alternate that holds one — a push's scratch — reads no alternate object
// directory and holds every commit its alternate's commit-graph names to the
// objects it can reach (GIT_COMMIT_GRAPH_PARANOIA); and a process leaning on
// an alternate that holds none, or running in a cache without naming its
// view — the maintainer's — is handed neither.
//
// Mutations: set the paranoia only where an alternate is named, whatever it
// holds, and the scratch over a cache with no dropping view carries it; drop
// it, and the scratch over one with a view carries none.
func TestTheDroppingViewIsNamedWhereItIsServed(t *testing.T) {
	t.Parallel()
	p := fakePool(t, "exec env", gitproc.Options{})
	cache, bare := withDropping(t, gittest.Empty(t)), gittest.Empty(t)
	scratchOver := func(alternate string) string {
		s := gittest.Empty(t)
		writeFile(t, filepath.Join(s, "objects", "info", "alternates"), alternate+"/objects\n")
		return s
	}
	view := filepath.Join(cache, gitproc.DroppingDir)
	for _, tc := range []struct {
		name               string
		cmd                gitproc.Cmd
		alternates, strict string
	}{
		{"a serving process naming the view", gitproc.Cmd{Dir: cache, Dropping: view}, view, ""},
		{"a process in the cache naming none", gitproc.Cmd{Dir: cache}, "", ""},
		{"a scratch over a cache with a view", gitproc.Cmd{Dir: scratchOver(cache), Alternate: cache}, "", "1"},
		{"a scratch over a cache with none", gitproc.Cmd{Dir: scratchOver(bare), Alternate: bare}, "", ""},
	} {
		tc.cmd.Args, tc.cmd.Class, tc.cmd.Deadline = []string{"env"}, gitproc.ClassRead, gitproc.ReadDeadline
		res, err := p.Run(t.Context(), tc.cmd)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		env := envOf(res.Stdout)
		if got := env["GIT_ALTERNATE_OBJECT_DIRECTORIES"]; got != tc.alternates {
			t.Errorf("%s: GIT_ALTERNATE_OBJECT_DIRECTORIES = %q, want %q", tc.name, got, tc.alternates)
		}
		if got := env["GIT_COMMIT_GRAPH_PARANOIA"]; got != tc.strict {
			t.Errorf("%s: GIT_COMMIT_GRAPH_PARANOIA = %q, want %q", tc.name, got, tc.strict)
		}
	}
}

// A DROPPING VIEW NO CALL MAY NAME IS REFUSED before any git starts: with no
// repository, beside an alternate, as any other path than the repository's
// own DroppingDir (ErrInvalid) — and one that is not there, a link (wherever
// it leads) or a file is the repository's shape (ShapeDropping), which the
// pool holds every repository to whether or not its call names the view, so
// a planted link refuses every process in the cache, a push's scratch leaning
// on it among them.
//
// Mutations: accept a view beside an alternate, and the scratch runs with it;
// skip checkDropping's link rule, and the link is served.
func TestADroppingViewNoCallMayNameIsRefused(t *testing.T) {
	t.Parallel()
	recorder, calls := gittest.Recording(t)
	p := newPool(t, gitproc.Options{Path: recorder})
	run := func(c gitproc.Cmd) error {
		c.Args, c.Class, c.Deadline = []string{"rev-parse", "--git-dir"}, gitproc.ClassRead, gitproc.ReadDeadline
		_, err := p.Run(t.Context(), c)
		return err
	}
	ranIn := func(dir string) bool {
		for _, call := range calls() {
			if call.GitDir == dir {
				return true
			}
		}
		return false
	}
	cache := withDropping(t, gittest.Empty(t))
	view := filepath.Join(cache, gitproc.DroppingDir)
	scratch := gittest.Empty(t)
	writeFile(t, filepath.Join(scratch, "objects", "info", "alternates"), cache+"/objects\n")
	own := filepath.Join(scratch, gitproc.DroppingDir)
	for name, c := range map[string]gitproc.Cmd{
		"no repository":         {Dropping: view},
		"beside an alternate":   {Dir: scratch, Alternate: cache, Dropping: own},
		"the alternate's view":  {Dir: scratch, Alternate: cache, Dropping: view},
		"another repository's":  {Dir: gittest.Empty(t), Dropping: view},
		"a path below the view": {Dir: cache, Dropping: filepath.Join(view, "pack")},
		"an unclean path":       {Dir: cache, Dropping: cache + "/./" + gitproc.DroppingDir},
	} {
		if err := run(c); !errors.Is(err, gitproc.ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	if ranIn(scratch) || ranIn(cache) {
		t.Fatal("git ran for a call naming a dropping view it may not")
	}

	shaped := func(t *testing.T, err error, dir string) {
		t.Helper()
		var shape *gitproc.ShapeError
		if !errors.As(err, &shape) || shape.Rule != gitproc.ShapeDropping || shape.Dir != dir {
			t.Fatalf("%v, want a refusal under %s naming %s", err, gitproc.ShapeDropping, dir)
		}
		if ranIn(dir) {
			t.Fatal("git ran in the refused repository")
		}
	}
	t.Run("a view the call names that is not there", func(t *testing.T) {
		bare := gittest.Empty(t)
		shaped(t, run(gitproc.Cmd{Dir: bare, Dropping: filepath.Join(bare, gitproc.DroppingDir)}), bare)
	})
	for name, spoil := range map[string]func(t *testing.T, bare string){
		"a link to another repository's objects": func(t *testing.T, bare string) {
			symlink(t, filepath.Join(gittest.Empty(t), "objects"), filepath.Join(bare, gitproc.DroppingDir))
		},
		"a link to a directory of its own": func(t *testing.T, bare string) {
			other := filepath.Join(bare, "elsewhere")
			if err := os.Mkdir(other, hostbox.DirMode); err != nil {
				t.Fatal(err)
			}
			symlink(t, other, filepath.Join(bare, gitproc.DroppingDir))
		},
		"a link to nowhere": func(t *testing.T, bare string) {
			symlink(t, filepath.Join(t.TempDir(), "gone"), filepath.Join(bare, gitproc.DroppingDir))
		},
		"a file": func(t *testing.T, bare string) {
			writeFile(t, filepath.Join(bare, gitproc.DroppingDir), "pack\n")
		},
	} {
		t.Run(name, func(t *testing.T) {
			bare := gittest.Empty(t)
			spoil(t, bare)
			shaped(t, run(gitproc.Cmd{Dir: bare, Dropping: filepath.Join(bare, gitproc.DroppingDir)}), bare)
			shaped(t, run(gitproc.Cmd{Dir: bare}), bare)
			// A scratch leaning on it is refused for the alternate's shape.
			s := gittest.Empty(t)
			writeFile(t, filepath.Join(s, "objects", "info", "alternates"), bare+"/objects\n")
			var shape *gitproc.ShapeError
			if err := run(gitproc.Cmd{Dir: s, Alternate: bare}); !errors.As(err, &shape) ||
				shape.Rule != gitproc.ShapeDropping || !shape.InAlternate || ranIn(s) {
				t.Fatalf("a scratch leaning on it: %v, want a refusal of its alternate under %s", err,
					gitproc.ShapeDropping)
			}
		})
	}
}

// GIT READS A DROPPING OBJECT ONLY THROUGH THE VIEW: an object a cache holds
// in its dropping view alone is read by a process that names the view —
// wherever the cache lies, a directory whose name holds the list's separator
// and a double quote among them — and by none other; and a push's scratch
// leaning on the cache reads neither the object nor the commit the cache's
// commit-graph still names, as git would through the graph had it not been
// told to hold the graph to the objects. On each of the node's gits.
//
// Mutations: write the view's path into the variable unquoted, and git splits
// it at the separator and reads nothing; drop the scratch's paranoia, and
// rev-list answers the dropped commit from the alternate's commit-graph.
func TestGitReadsADroppingObjectOnlyThroughTheView(t *testing.T) {
	t.Parallel()
	onNodeGits(t, func(t *testing.T, git string) {
		p := poolOn(t, git, gitproc.Options{})
		client := gittest.NewClientWith(t, git)
		work := client.Init(t)
		kept := client.Commit(t, work, map[string]string{"a.txt": "a\n"}, "kept")
		// THE DROPPED COMMIT names the kept one's tree: an amended message,
		// so the one object the cache does not serve outside its view is the
		// commit itself, and a walk that takes the commit from a graph finds
		// everything below it.
		dropped := client.Commit(t, work, nil, "dropped")

		parent := t.TempDir()
		cache := filepath.Join(parent, `cache:"one".git`)
		if err := os.Rename(gittest.Empty(t), cache); err != nil {
			t.Fatal(err)
		}
		gittest.Pack(t, client, cache, work, kept)
		cs := gittest.PackObjects(t, client, cache, work, dropped)
		gittest.SetRefs(t, cache, map[string]string{"refs/heads/main": kept, "refs/heads/gone": dropped})
		client.Run(t, cache, "commit-graph", "write", "--reachable")
		view := filepath.Join(cache, gitproc.DroppingDir)
		if err := os.MkdirAll(filepath.Join(view, "pack"), hostbox.DirMode); err != nil {
			t.Fatal(err)
		}
		for _, ext := range []string{".pack", ".idx", ".rev"} {
			name := "pack-" + cs + ext
			from, to := filepath.Join(cache, "objects", "pack", name), filepath.Join(view, "pack", name)
			if err := os.Rename(from, to); err != nil {
				t.Fatal(err)
			}
		}
		gittest.SetRefs(t, cache, map[string]string{"refs/heads/main": kept})

		read := func(c gitproc.Cmd, args ...string) (string, error) {
			c.Args, c.Class, c.Deadline = args, gitproc.ClassRead, gitproc.ReadDeadline
			res, err := p.Run(t.Context(), c)
			return strings.TrimSpace(string(res.Stdout)), err
		}
		if got, err := read(gitproc.Cmd{Dir: cache, Dropping: view}, "cat-file", "-t", dropped); err != nil ||
			got != "commit" {
			t.Errorf("a serving process naming the view: cat-file -t %s = %q, %v; want commit", dropped, got, err)
		}
		if got, err := read(gitproc.Cmd{Dir: cache}, "cat-file", "-t", dropped); err == nil {
			t.Errorf("a process in the cache naming no view read the dropped commit: %q", got)
		}
		scratch := gittest.Empty(t)
		writeFile(t, filepath.Join(scratch, "objects", "info", "alternates"), cache+"/objects\n")
		over := gitproc.Cmd{Dir: scratch, Alternate: cache}
		if got, err := read(over, "cat-file", "-t", kept); err != nil || got != "commit" {
			t.Fatalf("the scratch reads none of its alternate's live objects: cat-file -t %s = %q, %v", kept, got, err)
		}
		if got, err := read(over, "cat-file", "-t", dropped); err == nil {
			t.Errorf("a push's scratch read the dropped commit: %q", got)
		}
		if got, err := read(over, "rev-list", "-1", dropped); err == nil {
			t.Errorf("a push's scratch walked the dropped commit out of its alternate's commit-graph: %q", got)
		}
	})
}

// withDropping gives bare a dropping view — an empty DroppingDir with its
// pack directory — and answers bare.
func withDropping(t testing.TB, bare string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(bare, gitproc.DroppingDir, "pack"), hostbox.DirMode); err != nil {
		t.Fatal(err)
	}
	return bare
}

// writeFile writes body at path, its directory made first.
func writeFile(t testing.TB, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), hostbox.DirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), hostbox.FileMode); err != nil {
		t.Fatal(err)
	}
}

// symlink makes link a symbolic link to target.
func symlink(t testing.TB, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}
