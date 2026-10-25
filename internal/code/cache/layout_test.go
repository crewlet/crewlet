package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
)

// tagSignature is the Cache Directory Tagging standard's first line, which
// every backup tool reads.
const tagSignature = "Signature: 8a477f597d28d172789f06886806bc55"

// prepare prepares dir and releases its claim when the case ends.
func prepare(t *testing.T, dir string) cache.Layout {
	t.Helper()
	layout, err := cache.Prepare(t.Context(), dir)
	if err != nil {
		t.Fatalf("Prepare %s: %v", dir, err)
	}
	t.Cleanup(layout.Release)
	return layout
}

// PREPARE TAKES ONLY WHAT IS ITS OWN: an empty directory or one that already
// carries this cache's tag. Open empties tmp/ and scratch/ at every boot, so
// git.cache_dir pointed at a populated directory — /var, a home — would
// otherwise empty /var/tmp; and a directory another tool tagged under the
// same standard is that tool's.
func TestPrepareRefusesADirectoryThatIsNotACache(t *testing.T) {
	t.Parallel()

	refused := map[string]func(t *testing.T, dir string){
		"a populated directory with no tag": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "notes.txt"), "an operator's file")
		},
		// The filesystem's own directory is not content; a file beside it
		// still is.
		"a file beside lost+found": func(t *testing.T, dir string) {
			if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "notes.txt"), "an operator's file")
		},
		// Only a DIRECTORY of that name is mkfs's.
		"a file named lost+found": func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "lost+found"), "an operator's file")
		},
	}
	for name, setup := range refused {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			setup(t, dir)
			before := names(t, dir)
			_, err := cache.Prepare(t.Context(), dir)
			if !errors.Is(err, cache.ErrNotACache) {
				t.Fatalf("Prepare of %s = %v, want ErrNotACache", name, err)
			}
			for _, want := range []string{dir, "git.cache_dir"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not name %q: %v", want, err)
				}
			}
			if got := names(t, dir); !slices.Equal(got, before) {
				t.Errorf("a refused directory was changed: it holds %v, held %v", got, before)
			}
		})
	}

	for name, tag := range map[string]string{
		"another signature": "Signature: 00000000000000000000000000000000\n",
		// Every tool that follows the standard writes the same signature;
		// what says the tag is this cache's is the line after it.
		"another tool's tag": tagSignature + "\n# This file is a cache directory tag created by ccache.\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, filepath.Join(dir, "CACHEDIR.TAG"), tag)
			write(t, filepath.Join(dir, "tmp", "theirs"), "another tool's file")
			if _, err := cache.Prepare(t.Context(), dir); !errors.Is(err, cache.ErrNotACache) {
				t.Fatalf("Prepare of a directory with %s = %v, want ErrNotACache", name, err)
			}
			if !exists(filepath.Join(dir, "tmp", "theirs")) {
				t.Error("a refused directory lost a file")
			}
		})
	}

	t.Run("a relative path", func(t *testing.T) {
		t.Parallel()
		_, err := cache.Prepare(t.Context(), "relative/c")
		if !errors.Is(err, cache.ErrOptions) || !strings.Contains(err.Error(), "git.cache_dir") {
			t.Fatalf("Prepare of a relative path = %v, want ErrOptions naming git.cache_dir", err)
		}
	})

	for name, setup := range map[string]func(t *testing.T) string{
		"absent": func(t *testing.T) string { return filepath.Join(t.TempDir(), "a", "c") },
		"empty":  func(t *testing.T) string { return t.TempDir() },
		// A first Prepare cut short while it wrote the tag — the one write
		// it makes before anything else — is recognised and completed.
		"holding a tag cut short": func(t *testing.T) string {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "CACHEDIR.TAG"), tagSignature+"\n# This file mar")
			return dir
		},
		"holding an empty tag": func(t *testing.T) string {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "CACHEDIR.TAG"), "")
			return dir
		},
		// The root of a freshly made ext volume — the volume of its own the
		// refusal recommends — holds mkfs's lost+found and nothing else.
		"the root of a fresh volume": func(t *testing.T) string {
			dir := t.TempDir()
			write(t, filepath.Join(dir, "lost+found", "#1234"), "what fsck recovered")
			return dir
		},
		"a fresh volume holding a tag cut short": func(t *testing.T) string {
			dir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dir, "lost+found"), 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "CACHEDIR.TAG"), tagSignature+"\n")
			return dir
		},
		"reached through a symbolic link": func(t *testing.T) string {
			base := t.TempDir()
			if err := os.Mkdir(filepath.Join(base, "real"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(base, "real"), filepath.Join(base, "link")); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(base, "link")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := setup(t)
			layout := prepare(t, dir)
			resolved, err := filepath.EvalSymlinks(dir)
			if err != nil {
				t.Fatal(err)
			}
			if layout.Dir != resolved {
				t.Errorf("Layout.Dir = %s, want %s (absolute, every link resolved)", layout.Dir, resolved)
			}
			if layout.Home != filepath.Join(resolved, "home") {
				t.Errorf("Layout.Home = %s, want %s", layout.Home, filepath.Join(resolved, "home"))
			}
			if exists(layout.Home) {
				t.Error("Prepare made home/: it is gitproc's to make, and New refuses one it did not")
			}
			for _, sub := range []string{"repos", "scratch", "tmp", "trash", "hooks"} {
				if info, err := os.Stat(filepath.Join(resolved, sub)); err != nil || !info.IsDir() {
					t.Errorf("%s is not a directory after Prepare: %v", sub, err)
				}
			}
			tag := read(t, filepath.Join(resolved, "CACHEDIR.TAG"))
			if first, _, _ := strings.Cut(tag, "\n"); first != tagSignature {
				t.Errorf("the tag's first line is %q, want the standard's signature", first)
			}
			if lf := filepath.Join(resolved, "lost+found"); exists(lf) {
				if got := names(t, lf); len(got) > 0 && read(t, filepath.Join(lf, got[0])) != "what fsck recovered" {
					t.Error("Prepare touched lost+found")
				}
			}

			// A second Prepare finds it prepared and rewrites nothing — once
			// the first has given its claim back.
			before, err := os.Stat(filepath.Join(resolved, "CACHEDIR.TAG"))
			if err != nil {
				t.Fatal(err)
			}
			layout.Release()
			again := prepare(t, dir)
			if again.Dir != layout.Dir || again.Home != layout.Home || again.Held != layout.Held {
				t.Errorf("a second Prepare = %+v, want %+v", again, layout)
			}
			after, err := os.Stat(filepath.Join(resolved, "CACHEDIR.TAG"))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) || read(t, filepath.Join(resolved, "CACHEDIR.TAG")) != tag {
				t.Error("a second Prepare rewrote the tag")
			}
		})
	}
}

// THE CLAIM RUNS FROM PREPARE TO STOP. What Prepare read of repos/ is what Open
// acts on, so nothing may take the directory between them — not another
// process, and not a second Prepare in this one (whose boot would move the
// first one's HOME). A boot that fails before its Open gives the claim back
// with Layout.Release; an Open takes it over, and its Stop gives it back.
func TestAPreparedDirectoryIsHeldUntilItsCacheStops(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	refused := func(when string) {
		t.Helper()
		_, err := cache.Prepare(t.Context(), w.layout.Dir)
		if !errors.Is(err, cache.ErrCacheInUse) || !strings.Contains(err.Error(), fmt.Sprintf("pid %d", os.Getpid())) {
			t.Fatalf("a second Prepare %s = %v, want ErrCacheInUse naming this process", when, err)
		}
	}
	first := w.layout
	refused("while the first Layout is unopened")
	first.Release()
	first.Release() // idempotent
	if _, err := cache.Open(t.Context(), w.options(t, func(o *cache.Options) { o.Layout = first })); !errors.Is(err, cache.ErrOptions) ||
		!strings.Contains(err.Error(), "cache.Prepare") {
		t.Fatalf("Open of a released Layout = %v, want ErrOptions naming cache.Prepare", err)
	}

	w.reboot(t)
	c, err := cache.Open(t.Context(), w.options(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	w.layout.Release() // taken over by Open: nothing to give back
	refused("while its cache is open")
	c.Stop(context.Background())
	prepare(t, w.layout.Dir)
}

// HELD IS EVERY BYTE THE CACHE OWNS, counted the cheap way: an incarnation
// with a state of this build counts what its state says it holds — one
// small read a repository, never a stat of its packs, which at one pack a
// push is a walk of millions of files at every restart — and only what has
// no state is walked. The leftovers count too: trash/, scratch/, tmp/ and a
// home/ a killed git populated are given back by the janitor seconds after
// the boot, so the next boot finds them as free space, and left out here
// they would make this boot's budget smaller than the next one's. A link is
// counted as itself, never as what it points at.
func TestPrepareMeasuresWhatTheCacheHolds(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "c")
	prepare(t, dir).Release()

	// A: its state says 9 100 + 400 + 120 — the packs, and each derived
	// file's entry; its files are not what is counted.
	a := aState(repoA)
	writeState(t, filepath.Join(dir, "repos", repoA+".3.git"), a)
	write(t, filepath.Join(dir, "repos", repoA+".3.git", "objects", "pack", "pack-1.pack"), strings.Repeat("p", 1000))
	// B's kept incarnation has no state: its objects and packed-refs are
	// walked (HEAD is not what a state counts either).
	b := filepath.Join(dir, "repos", repoB+".2.git")
	write(t, filepath.Join(b, "objects", "pack", "pack-2.pack"), strings.Repeat("q", 2000))
	write(t, filepath.Join(b, "packed-refs"), strings.Repeat("r", 100))
	write(t, filepath.Join(b, "HEAD"), "ref: refs/heads/main\n")
	outside := filepath.Join(t.TempDir(), "elsewhere")
	write(t, outside, strings.Repeat("x", 100000))
	link := filepath.Join(b, "objects", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	// B's older incarnation, which Open moves aside, counts by its state.
	older := aState(repoB)
	older.DiskBytes = 50
	older.Derived = map[string]derivedDoc{
		string(cache.DerivedMidx):  {Bytes: 5, Covers: 1, Step: string(cache.StepIndex)},
		string(cache.DerivedGraph): {Bytes: 3, Covers: 1, Step: string(cache.StepGraph)},
	}
	writeState(t, filepath.Join(dir, "repos", repoB+".1.git"), older)
	// A stray is walked whole.
	write(t, filepath.Join(dir, "repos", "junk", "file"), strings.Repeat("j", 300))
	// The leftovers.
	write(t, filepath.Join(dir, "trash", "gone", "big"), strings.Repeat("t", 5000))
	write(t, filepath.Join(dir, "tmp", "download"), strings.Repeat("d", 7000))
	write(t, filepath.Join(dir, "scratch", "0199a1d2-push.git", "f"), strings.Repeat("s", 600))
	write(t, filepath.Join(dir, "home", "left-behind"), strings.Repeat("h", 40))

	layout := prepare(t, dir)
	want := (9100 + 400 + 120) + (2000 + 100 + linkInfo.Size()) + (50 + 5 + 3) + 300 + 5000 + 7000 + 600 + 40
	if layout.Held != want {
		t.Errorf("Layout.Held = %d, want %d: what each state counts, the stateless walked, and the leftovers", layout.Held, want)
	}
}

// PREPARE ENDS WITH ITS CONTEXT, and gives its claim back as it does: a boot
// that is cancelled part way through reading a large cache leaves a
// directory the next boot can take.
func TestPrepareIsBoundedByItsContext(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "c")
	prepare(t, dir).Release()
	writeState(t, filepath.Join(dir, "repos", repoA+".1.git"), aState(repoA))

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.Prepare(ended, dir); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare under an ended context = %v, want context.Canceled", err)
	}
	prepare(t, dir) // the claim is free again
}

// A HOME A KILLED GIT LEFT POPULATED IS MOVED ASIDE, never refused at every
// boot: gitproc keeps HOME empty while it runs and refuses a populated one,
// so without this a crash part way through a git process would keep the node
// down until a person emptied it.
func TestPrepareMovesALeftoverHomeAside(t *testing.T) {
	t.Parallel()
	gittest.RequireNode(t)
	dir := filepath.Join(t.TempDir(), "c")
	prepare(t, dir).Release()
	write(t, filepath.Join(dir, "home", "left-behind"), "a killed git's file")

	layout := prepare(t, dir)
	if exists(layout.Home) {
		t.Fatal("home/ is still there: gitproc.New would refuse it")
	}
	matches, err := filepath.Glob(filepath.Join(layout.Dir, "trash", "boot-*-home", "left-behind"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("the leftover is not under trash/boot-*-home/: %v %v", matches, err)
	}
	if _, err := gitproc.New(t.Context(), gitproc.Options{Home: layout.Home, MaxProcesses: 1}); err != nil {
		t.Fatalf("gitproc.New after Prepare: %v", err)
	}
}
