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
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
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

// PREPARE TAKES ONLY WHAT IS ITS OWN: an empty directory or one carrying this
// cache's tag. Open empties tmp/ and scratch/ at every boot, so a cache_dir
// of /var would otherwise empty /var/tmp, and a directory another tool
// tagged is that tool's. Prepared again, a directory it took is not rewritten:
// the tag keeps its inode, its bytes and a modification time set long past,
// which any write moves to now, even within one tick of a coarse file clock.
//
// Mutation: a second Prepare rewrites the tag it finds its own, in place
// (writeTag in adoptDirectory's tagOurs arm), on the filesystem's clock or on a
// two-second one.
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
			for _, want := range []string{dir, "gitlet.cache_dir"} {
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
		if !errors.Is(err, cache.ErrOptions) || !strings.Contains(err.Error(), "gitlet.cache_dir") {
			t.Fatalf("Prepare of a relative path = %v, want ErrOptions naming gitlet.cache_dir", err)
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

			// A second Prepare, once the first gave its claim back, rewrites nothing.
			// The tag's modification is first set to an even second, which every file
			// clock holds exactly, so any write moves it however coarse the clock.
			tagPath := filepath.Join(resolved, "CACHEDIR.TAG")
			stated := time.Date(2001, time.February, 3, 4, 5, 6, 0, time.UTC)
			if err := os.Chtimes(tagPath, stated, stated); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(tagPath)
			if err != nil {
				t.Fatal(err)
			}
			layout.Release()
			again := prepare(t, dir)
			if again.Dir != layout.Dir || again.Home != layout.Home || again.Held != layout.Held {
				t.Errorf("a second Prepare = %+v, want %+v", again, layout)
			}
			after, err := os.Stat(tagPath)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) {
				t.Error("a second Prepare replaced the tag with a new file")
			}
			if !after.ModTime().Equal(stated) {
				t.Errorf("a second Prepare rewrote the tag: modified at %v, want the stated %v", after.ModTime(), stated)
			}
			if got := read(t, tagPath); got != tag {
				t.Errorf("a second Prepare changed the tag to %q, want %q", got, tag)
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

// HELD IS EVERY BYTE THE CACHE OWNS, counted the cheap way: a kept
// incarnation with a state of this build counts what its state says, one small
// read a repository, never a walk of its packs; only what has no state and
// what the boot gives back (an older incarnation beside the kept one, trash/,
// scratch/, tmp/ and a populated home/) is walked. Given-back files count once
// each, and never when the kept incarnation holds them
// (TestABootCountsEachFileOnce). A symbolic link counts as itself.
func TestPrepareMeasuresWhatTheCacheHolds(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "c")
	prepare(t, dir).Release()

	// A: its state says what its packs hold — its dropping view's among them
	// — and each derived file's entry; its files are not what is counted.
	a := aState(repoA)
	writeState(t, filepath.Join(dir, "repos", repoA+".3.git"), a)
	write(t, filepath.Join(dir, "repos", repoA+".3.git", "objects", "pack", "pack-1.pack"), strings.Repeat("p", 1000))
	// B's kept incarnation has no state: its objects, its dropping view and
	// its packed-refs are walked (HEAD is not what a state counts either).
	b := filepath.Join(dir, "repos", repoB+".2.git")
	write(t, filepath.Join(b, "objects", "pack", "pack-2.pack"), strings.Repeat("q", 2000))
	write(t, filepath.Join(b, gitproc.DroppingDir, "pack", "pack-4.pack"), strings.Repeat("v", 900))
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
	// B's older incarnation, which Open moves aside, counts the files it holds
	// (its state and a pack of its own), whatever its state says.
	older := aState(repoB)
	older.DiskBytes = 50
	older.Derived = map[string]derivedDoc{
		string(cache.DerivedMidx):  {Bytes: 5, Covers: 1, Step: string(cache.StepIndex)},
		string(cache.DerivedGraph): {Bytes: 3, Covers: 1, Step: string(cache.StepGraph)},
	}
	olderDir := filepath.Join(dir, "repos", repoB+".1.git")
	writeState(t, olderDir, older)
	write(t, filepath.Join(olderDir, "objects", "pack", "pack-3.pack"), strings.Repeat("o", 800))
	olderState, err := os.Lstat(filepath.Join(olderDir, "crewlet-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// A stray is walked whole.
	write(t, filepath.Join(dir, "repos", "junk", "file"), strings.Repeat("j", 300))
	// The leftovers.
	write(t, filepath.Join(dir, "trash", "gone", "big"), strings.Repeat("t", 5000))
	write(t, filepath.Join(dir, "tmp", "download"), strings.Repeat("d", 7000))
	write(t, filepath.Join(dir, "scratch", "0199a1d2-push.git", "f"), strings.Repeat("s", 600))
	write(t, filepath.Join(dir, "home", "left-behind"), strings.Repeat("h", 40))
	// And second names of files counted above, each counted nowhere again:
	// a copy a swap replaced, in trash/ with a link of A's pack, which A's
	// state counts; a failed swap's staging beside it, with a link of the big
	// file trash/ already holds; and a crashed swap's staging in scratch/,
	// with a link of B's pack, which B's walk counts.
	linkAt(t, filepath.Join(dir, "repos", repoA+".3.git", "objects", "pack", "pack-1.pack"),
		filepath.Join(dir, "trash", "swapped", "objects", "pack", "pack-1.pack"))
	linkAt(t, filepath.Join(dir, "trash", "gone", "big"), filepath.Join(dir, "trash", "staged", "big"))
	linkAt(t, filepath.Join(b, "objects", "pack", "pack-2.pack"),
		filepath.Join(dir, "scratch", "swap.staged.git", "objects", "pack", "pack-2.pack"))

	layout := prepare(t, dir)
	want := (a.DiskBytes + a.derivedBytes()) + (2000 + 900 + 100 + linkInfo.Size()) + (olderState.Size() + 800) +
		300 + 5000 + 7000 + 600 + 40
	if layout.Held != want {
		t.Errorf("Layout.Held = %d, want %d: what each kept state counts, the stateless and the older walked, and "+
			"the leftovers, each file once", layout.Held, want)
	}
}

// linkAt makes newname, and the directories above it, a hard link of
// oldname: a second name of one file, as the cache gives them.
func linkAt(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(newname), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(oldname, newname); err != nil {
		t.Fatal(err)
	}
}

// A BOOT COUNTS EACH FILE ONCE, however many names the cache gave it. A
// rebuild (seedRebuild) and a swap (swap.go) hard-link files across
// incarnations, so one file can sit in the kept incarnation, an older one a
// crash left beside it, a replaced copy or failed staging in trash/, or a
// purged repository's two copies. Held counts each given-back file once and
// none the kept one's state counts (by every name the budget overclaims; by
// sole names a shared file counts nowhere); countedOnce expects by inode.
//
// Mutations: count an older incarnation by its state (readBoot), and the
// repos/ arms count its linked files twice; count trash/ by every name, and
// the trash/ arms count shared files twice; by sole names, and the purged
// repository's arm counts them nowhere.
func TestABootCountsEachFileOnce(t *testing.T) {
	t.Parallel()
	t.Run("a rebuild's predecessor in repos/", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		_, damaged := w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/dev", nil)
		c := w.open(t)
		// A reader holds the copy the rebuild replaces, so it stays in repos/
		// past the rebuild, as it does on a node that dies before the reader
		// lets go.
		held := w.sync(t, c, repo)
		damage(t, filepath.Join(held.Dir, "objects", "pack", "pack-"+damaged.GitChecksum+".pack"), 28,
			[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
		if r, err := verify(t.Context(), c, repo, true); err != nil || !r.Rebuilt {
			t.Fatalf("Verify with repair = %+v, %v; want the copy rebuilt", r, err)
		}
		kept := w.dir(t, c, repo)
		if kept == held.Dir {
			t.Fatalf("the rebuild left the copy at %s", kept)
		}
		c.Stop(context.Background())
		requireIn(t, held.Dir)
		countedOnce(t, w, kept)
	})
	t.Run("a swap's predecessor in repos/", func(t *testing.T) {
		t.Parallel()
		// A compaction retires two of the three packs while a reader holds
		// the copy: the next incarnation links the output, the third pack,
		// the packed-refs and the commit-graph — and the old one stays in
		// repos/ until the reader lets go (swap.go).
		k := newCompactable(t, true)
		k.compact(t)
		kept := k.w.sync(t, k.c, k.repo).Dir
		if kept == k.held.Dir {
			t.Fatalf("the round that retired two packs left the copy at %s", kept)
		}
		k.c.Stop(context.Background())
		requireIn(t, k.held.Dir)
		countedOnce(t, k.w, kept)
	})
	t.Run("a swapped-out copy and a failed swap's staging in trash/", func(t *testing.T) {
		t.Parallel()
		// The first swap fails once it has linked every live pack — its
		// staging goes into trash/ — and the next round's swaps; the old
		// copy's last reader then lets it go, into trash/ as well. No
		// janitor runs, as none had when a node stops before its pass.
		k := newCompactable(t, true, cache.LinkWith(refuseOnce(thePackedRefs)))
		w, c, repo := k.w, k.c, k.repo
		k.compact(t)
		s, err := c.Sync(t.Context(), repo, linearizable)
		if err != nil {
			t.Fatal(err)
		}
		if s.Dir != k.held.Dir {
			t.Fatalf("the round whose swap failed answered %s, want the copy it applied to, %s", s.Dir, k.held.Dir)
		}
		s.Release()
		kept := w.sync(t, c, repo).Dir
		if kept == k.held.Dir {
			t.Fatalf("the round after the failed swap left the copy at %s", kept)
		}
		k.held.Release()
		c.Stop(context.Background())
		if statOK(t, k.held.Dir) {
			t.Fatalf("%s is still in repos/ after its last reader let it go", k.held.Dir)
		}
		trash := names(t, w.path("trash"))
		staging := slices.ContainsFunc(trash, func(name string) bool { return strings.Contains(name, "-swap.") })
		replaced := slices.ContainsFunc(trash, func(name string) bool {
			return strings.HasSuffix(name, "-"+filepath.Base(k.held.Dir))
		})
		if !staging || !replaced {
			t.Fatalf("trash/ holds %v, want the failed swap's staging and the copy the swap replaced", trash)
		}
		countedOnce(t, w, kept)
	})
	t.Run("a purged repository's two copies in trash/", func(t *testing.T) {
		t.Parallel()
		// A swap leaves the old copy to its reader, and then the rows purge
		// the repository: the copy serving goes into trash/ at once, the old
		// one with its last reader. The two share every live pack, the
		// packed-refs and the commit-graph, and no incarnation the boot keeps
		// holds any of them.
		k := newCompactable(t, true)
		w, c, repo := k.w, k.c, k.repo
		k.compact(t)
		s, err := c.Sync(t.Context(), repo, linearizable)
		if err != nil {
			t.Fatal(err)
		}
		if s.Dir == k.held.Dir {
			t.Fatalf("the round that retired two packs left the copy at %s", s.Dir)
		}
		s.Release()
		w.log.purge(repo)
		if _, err = c.Sync(t.Context(), repo, linearizable); !errors.Is(err, gitlet.ErrRepoNotFound) {
			t.Fatalf("a sync of the purged repository: %v, want gitlet.ErrRepoNotFound", err)
		}
		k.held.Release()
		c.Stop(context.Background())
		if got := names(t, w.path("repos")); len(got) != 0 {
			t.Fatalf("repos/ holds %v after the purge, want nothing", got)
		}
		countedOnce(t, w, "")
	})
}

// requireIn fails t unless dir is still where the case left it: an older
// incarnation a reader held across the node's stop.
func requireIn(t *testing.T, dir string) {
	t.Helper()
	if !statOK(t, dir) {
		t.Fatalf("%s is gone: the case needs it on the disk at the boot", dir)
	}
}

// countedOnce boots w's directory again and holds Layout.Held to every byte
// there counted ONCE: the incarnation kept at kept ("" for none) as its state
// says, and every other file under repos/, trash/, scratch/, tmp/ and home/
// once an inode, but none the kept one holds. It requires the case's subject:
// a given-back file with a second name, in the kept incarnation or another
// given-back tree.
func countedOnce(t *testing.T, w *syncWorld, kept string) {
	t.Helper()
	keptInodes := map[uint64]bool{}
	var want int64
	if kept != "" {
		for path := range regularFiles(t, kept) {
			keptInodes[inode(t, path)] = true
		}
		st := readStateDoc(t, kept)
		want = st.DiskBytes + st.derivedBytes()
	}
	given := []string{w.path("trash"), w.path("scratch"), w.path("tmp"), w.path("home")}
	repos, err := os.ReadDir(w.path("repos"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range repos {
		if dir := w.path("repos", d.Name()); dir != kept {
			given = append(given, dir)
		}
	}
	// once is what the boot gives back, each file once; again, the bytes a
	// count by every name adds — the kept incarnation's files under another
	// name, and every name of a given-back file past its first.
	var once, again int64
	seen := map[uint64]bool{}
	for _, dir := range given {
		if !statOK(t, dir) {
			continue
		}
		for path, info := range regularFiles(t, dir) {
			ino := inode(t, path)
			switch {
			case keptInodes[ino], seen[ino]:
				again += info.Size()
			default:
				seen[ino] = true
				once += info.Size()
			}
		}
	}
	if again == 0 {
		t.Fatalf("nothing the boot gives back has a second name: the case has nothing to count once")
	}
	want += once
	layout := prepare(t, w.layout.Dir)
	switch held := layout.Held; {
	case held == want+again:
		t.Errorf("Layout.Held = %d: the %d bytes of files with more than one name are counted by every name",
			held, again)
	case held < want:
		t.Errorf("Layout.Held = %d, want %d: %d bytes the boot gives back are counted nowhere", held, want,
			want-held)
	case held != want:
		t.Errorf("Layout.Held = %d, want %d: what the kept incarnation's state counts and the %d bytes the "+
			"boot gives back, each file once", held, want, once)
	}
}

// regularFiles is every regular file under dir, by its path, with what Lstat
// says of it.
func regularFiles(t *testing.T, dir string) map[string]os.FileInfo {
	t.Helper()
	out := map[string]os.FileInfo{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out[path] = info
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", dir, err)
	}
	return out
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
