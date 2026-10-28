package cache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/flock"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
)

// log is the package's logger, which a Cache uses when Options.Logger is nil
// and which Prepare, running before any Cache exists, always uses.
var log = logging.Get("code.cache")

// The names under a cache's root.
const (
	// tagName is the Cache Directory Tagging standard's file
	// (https://bford.info/cachedir/): a backup run with --exclude-caches
	// (tar, borg and restic all take it) skips a directory holding it, so a
	// cache that sits under the store's directory is left out of an
	// operator's backup of it that asks for that. Without the option those
	// tools copy it like any other directory; the engine's own backup never
	// reads it at all.
	tagName = "CACHEDIR.TAG"
	// lockName is the process claim's sidecar (internal/flock).
	lockName = "lock"
	// reposDir holds one incarnation per repository.
	reposDir = "repos"
	// scratchDir holds a receive-pack's scratch repositories, the
	// advertisement views and — unnamed — the requests' spools (Spool),
	// every one a request's of the process that made it; a merge's scratch
	// and the store of exactly the packs it merges (NewMergeScratch), a
	// compaction's of that process; and a swap's next incarnation while it
	// is staged (swap.go), a round's of that process, renamed into repos/
	// whole — so a crash before the rename leaves the incarnation it replaces
	// the highest in repos/, and the boot moves the staging aside with the
	// rest.
	scratchDir = "scratch"
	// tmpDir holds every file before its rename into place, and downloads
	// in progress.
	tmpDir = "tmp"
	// trashDir holds what is being removed: renamed in at once, removed by
	// the janitor in the background.
	trashDir = "trash"
	// hooksDir is the hooks directory every push's receive-pack runs with.
	hooksDir = "hooks"
	// hookName is the one hook in it.
	hookName = "proc-receive"
	// homeDir is HOME for every git process — gitproc's, which creates it
	// and keeps it empty.
	homeDir = "home"
	// lostFound is the directory mkfs makes at the root of every ext
	// filesystem. Prepare reads it as part of the filesystem rather than as
	// content, and nothing in the cache ever opens it.
	lostFound = "lost+found"
)

// tagSignature is the standard's first line, byte for byte: what every
// backup tool looks for.
const tagSignature = "Signature: 8a477f597d28d172789f06886806bc55"

// tagMarker begins the tag's second line, and is what says the tag is THIS
// cache's rather than another tool's: every tool that follows the standard
// writes the same signature, and a directory another tool tagged must not
// have its tmp/ emptied at our boot. A later build may reword what follows
// the marker; it keeps the marker.
const tagMarker = "# This file marks a crewlet git cache"

// tagContent is the whole of the tag this build writes. What follows the
// marker is for the person who opens the file in the directory, so it claims
// only what holds of every backup: the engine's own never reads the cache,
// but tar, borg and restic leave a tagged directory out only when run with
// --exclude-caches, and copy it otherwise.
const tagContent = tagSignature + "\n" + tagMarker + ": disposable copies of a company's " +
	"repositories, rebuilt from the object store whenever they are missing. Crewlet's own " +
	"backup never copies this directory, and a backup run with --exclude-caches leaves it " +
	"out; see https://bford.info/cachedir/\n"

// maxTagBytes bounds the read of a tag: ours is a couple of hundred bytes,
// and the file sits in a directory an operator chose, so whatever is there is
// never read whole into memory to be judged.
const maxTagBytes = 4 << 10

// incarnationName matches a repository's directory under repos/: a canonical
// lowercase UUID, then the incarnation's number with no leading zero —
// compiled at its first use, as every file-name pattern of this package is
// (lazyPattern).
var incarnationName = lazyPattern(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.([1-9][0-9]*)\.git$`)

// parseIncarnation reads a directory name under repos/: the repository's id
// and the incarnation's number, or false for anything else — a name that is
// not the grammar, an id that is not a canonical UUID, or a number past what
// an int holds.
func parseIncarnation(name string) (string, int, bool) {
	m := incarnationName().FindStringSubmatch(name)
	if m == nil || !canonicalID(m[1]) {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return "", 0, false
	}
	return m[1], n, true
}

// canonicalID reports whether s is a UUID in its one canonical spelling. A
// repository's id becomes a path component, so a spelling the parser
// accepts and the printer would not write (upper case, braces, a urn: prefix)
// would be two directories for one repository.
func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// Layout is a prepared cache: where it is, what it already holds, and — not
// exported — this process's claim on it and what Prepare read of repos/, both
// of which [Open] takes over.
type Layout struct {
	// Dir is the cache's root: absolute, clean, every symbolic link in it
	// resolved.
	Dir string

	// Home is Dir/home: gitproc.Options.Home. Prepare does not create it;
	// gitproc.New does, and refuses one that is not empty.
	Home string

	// Held is every byte the cache owns under Dir as Prepare found it — what
	// its repositories hold, and what the last process left in trash/,
	// scratch/ and tmp/ — which the engine adds to the volume's free space
	// before it derives a budget. Free space EXCLUDES the cache's own files,
	// so a budget derived from free space alone would shrink at every restart
	// of a warm node and the first janitor pass would evict the difference.
	// The leftovers count for the same reason: the janitor gives them back
	// seconds after the boot, so the NEXT boot finds them as free space, and
	// left out here they would make this boot's budget smaller than the
	// next one's for no change in the disk. EACH FILE COUNTS ONCE, however
	// many names it has: the cache hard-links one file into many places
	// (givenBack), and a byte counted twice here is room the budget claims
	// that the volume does not have.
	Held int64

	// boot is the claim and the reading Open takes over; nil in a Layout
	// Prepare did not return.
	boot *boot
}

// Release gives back the claim of a Layout no [Open] took. A boot that fails
// between Prepare and Open must release it, or the directory stays held by a
// process that will never serve from it — so `defer layout.Release()` right
// after Prepare, which does nothing once Open has taken the claim: the cache
// then holds it until [Cache.Stop]. Idempotent, and safe on a Layout Prepare
// did not return.
func (l Layout) Release() {
	if l.boot != nil && l.boot.taken.CompareAndSwap(false, true) {
		l.boot.claim.Release()
	}
}

// Prepare makes dir a cache directory, CLAIMS it for this process, and reads
// what it holds. It runs BEFORE gitproc.New, whose Home it returns, and
// before [Open], which takes over its claim and its reading:
//
//  1. dir must be absolute; it is created when absent, and its symbolic
//     links are resolved once.
//  2. A non-empty directory without this cache's CACHEDIR.TAG is REFUSED
//     ([ErrNotACache]), and nothing in it is touched: Open empties tmp/ and
//     scratch/ at every boot, so git.cache_dir pointed at /var would
//     otherwise empty /var/tmp. A tag another tool wrote is refused the same
//     way. A lost+found DIRECTORY is not content: mkfs makes one at the root
//     of every ext filesystem, so the root of a volume of the cache's own —
//     the home this refusal recommends — holds one before anything has been
//     written to it, and nothing in the cache ever reads, writes or removes
//     it.
//  3. The tag is written when absent — first, before anything else, so a
//     crash part way through leaves a directory the next Prepare recognises.
//  4. The directory is CLAIMED for this process (internal/flock); one
//     another process holds is refused ([ErrCacheInUse]) before anything
//     else changes in it, because the next step moves a live HOME. The claim
//     is held from here to [Cache.Stop] — Open takes it over rather than
//     claiming again — so no other process can take the directory between
//     this reading of it and the cache that acts on the reading. A second
//     Prepare of a held directory is refused the same way, in this process
//     too, until the first Layout is released or its cache stopped.
//  5. The layout's directories are created; a home/ that EXISTS is moved
//     into trash/. gitproc keeps HOME empty while it runs, so anything in it
//     is what a killed git left behind, and a populated HOME fails
//     gitproc.New at every boot until a person removes it. This is the one
//     place that can be done: it runs before gitproc.New, so no pool ever
//     sees its HOME move.
//  6. repos/ is READ — which incarnation of each repository Open keeps, with
//     its state, and which entries Open moves aside — and Held is summed
//     from the reading: the incarnation kept, with a state of this build,
//     counts what its state says it holds, so a warm node's boot reads one
//     small file a repository and stats none of its packs; one kept with no
//     state to count — missing or unreadable — is walked. Everything the boot
//     GIVES BACK is walked too and counted each file once, only where every
//     name of the file lies in it (givenBack): each entry of repos/ Open
//     moves aside — an older incarnation a crash left beside the one kept,
//     whose shared files the kept one links and its state counts; one whose
//     state is another build's; an entry that is not an incarnation — and
//     trash/, scratch/ and tmp/, which the janitor keeps small. An entry a
//     walk cannot read is skipped and logged (code_cache_held_unmeasured),
//     never a boot failure: the budget then errs small. The cost is
//     O(repositories), whatever they hold (the package doc's Measured
//     section). ctx bounds all of it.
//
// Any failure releases the claim.
func Prepare(ctx context.Context, dir string) (Layout, error) {
	if !filepath.IsAbs(dir) {
		return Layout{}, fmt.Errorf("%w: git.cache_dir %q is not an absolute path", ErrOptions, dir)
	}
	if err := os.MkdirAll(dir, hostbox.DirMode); err != nil {
		return Layout{}, fmt.Errorf("cache: making git.cache_dir %s: %w", dir, err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Layout{}, fmt.Errorf("cache: resolving git.cache_dir %s: %w", dir, err)
	}
	if err = adoptDirectory(root); err != nil {
		return Layout{}, err
	}

	claim, err := claimDir(root)
	if err != nil {
		return Layout{}, err
	}
	handed := false
	defer func() {
		if !handed {
			claim.Release()
		}
	}()

	for _, name := range []string{reposDir, scratchDir, tmpDir, trashDir, hooksDir} {
		if err = os.MkdirAll(filepath.Join(root, name), hostbox.DirMode); err != nil {
			return Layout{}, fmt.Errorf("cache: making %s: %w", filepath.Join(root, name), err)
		}
	}
	home := filepath.Join(root, homeDir)
	if _, err = os.Lstat(home); err == nil {
		if err = toTrash(root, home, bootLabel(homeDir)); err != nil {
			return Layout{}, fmt.Errorf("cache: moving a leftover %s aside: %w", home, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Layout{}, fmt.Errorf("cache: reading %s: %w", home, err)
	}

	b, held, err := readBoot(ctx, root)
	if err != nil {
		return Layout{}, err
	}
	b.claim = claim
	handed = true
	return Layout{Dir: root, Home: home, Held: held, boot: b}, nil
}

// adoptDirectory is Prepare's steps 2 and 3: root is empty but for a
// lost+found directory, or already this cache's — its tag completed when a
// crash cut the first write of it short.
func adoptDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("cache: listing git.cache_dir %s: %w", root, err)
	}
	content := 0
	for _, d := range entries {
		// The filesystem's own, never the cache's: a directory, as mkfs
		// makes it (ReadDir does not follow a link, so a link named
		// lost+found is content like any other).
		if d.Name() != lostFound || !d.IsDir() {
			content++
		}
	}
	tag, err := readTag(root)
	if err != nil {
		return err
	}
	switch {
	case tag == tagOurs:
		return nil
	case content == 0, tag == tagPartial && content == 1:
		// Empty, or holding nothing but the tag this build began to write
		// when it was cut short: no other tool writes a prefix of this
		// text, and nothing else is ever written before the tag.
		return writeTag(root)
	case tag == tagAbsent:
		return fmt.Errorf("%w: %s is not empty and is not a crewlet git cache (it has no %s): "+
			"point git.cache_dir at an empty directory or a volume of its own — the cache "+
			"removes what it finds under tmp/, scratch/ and trash/ at every boot",
			ErrNotACache, root, tagName)
	default:
		return fmt.Errorf("%w: %s holds a %s this cache did not write (another tool's cache): "+
			"point git.cache_dir at an empty directory or a volume of its own — the cache "+
			"removes what it finds under tmp/, scratch/ and trash/ at every boot",
			ErrNotACache, root, tagName)
	}
}

// tagState is what a directory's CACHEDIR.TAG says about it.
type tagState int

const (
	// tagAbsent: there is none.
	tagAbsent tagState = iota
	// tagOurs: the standard's signature, then this cache's marker.
	tagOurs
	// tagPartial: a strict prefix of what this build writes — the first
	// write of it, cut short.
	tagPartial
	// tagForeign: anything else, another tool's included.
	tagForeign
)

// readTag judges root's tag. A read that FAILED is an error, never an
// answer: a directory whose tag cannot be read is not thereby somebody
// else's.
func readTag(root string) (tagState, error) {
	f, err := os.Open(filepath.Join(root, tagName))
	if errors.Is(err, fs.ErrNotExist) {
		return tagAbsent, nil
	}
	if err != nil {
		return tagAbsent, fmt.Errorf("cache: reading %s: %w", filepath.Join(root, tagName), err)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTagBytes))
	_ = f.Close() // read-only: what the read returned is the answer
	if err != nil {
		return tagAbsent, fmt.Errorf("cache: reading %s: %w", filepath.Join(root, tagName), err)
	}
	ours := []byte(tagSignature + "\n" + tagMarker)
	switch {
	case bytes.HasPrefix(raw, ours):
		return tagOurs, nil
	case len(raw) < len(tagContent) && bytes.HasPrefix([]byte(tagContent), raw):
		return tagPartial, nil
	default:
		return tagForeign, nil
	}
}

// writeTag writes the tag IN PLACE — the one cache file not staged in tmp/
// and renamed — and makes it and its name durable. There is no tmp/ yet, and
// creating one first would leave a crash a non-empty directory with no tag,
// which every later Prepare refuses for good; whereas a tag cut short is
// recognised (tagPartial) and completed.
func writeTag(root string) error {
	path := filepath.Join(root, tagName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, hostbox.FileMode)
	if err != nil {
		return fmt.Errorf("cache: writing %s: %w", path, err)
	}
	_, err = f.WriteString(tagContent)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("cache: writing %s: %w", path, err)
	}
	return syncDir(root)
}

// claimDir takes this process's claim on the cache at root, answering a
// claim held elsewhere as [ErrCacheInUse] naming the holder.
func claimDir(root string) (*flock.Lock, error) {
	claim, err := flock.Try(filepath.Join(root, lockName))
	var held *flock.HeldError
	switch {
	case errors.As(err, &held):
		return nil, fmt.Errorf("%w: %s is in use by %s — two nodes are pointed at one "+
			"git.cache_dir; give each its own", ErrCacheInUse, root, held.Holder)
	case err != nil:
		return nil, fmt.Errorf("cache: claiming %s: %w", root, err)
	}
	return claim, nil
}

// bootLabel names what a boot moves into trash/: boot-<uuidv7>-<name>.
func bootLabel(name string) string { return "boot-" + newV7() + "-" + name }

// newV7 is a fresh UUIDv7's text. uuid.NewV7 fails only when the system's
// randomness does, and a node without randomness cannot mint a single key
// the object store would accept either — so it panics rather than name two
// trash entries alike.
func newV7() string {
	id, err := uuid.NewV7()
	if err != nil {
		panic(fmt.Sprintf("cache: minting a UUIDv7: %v", err))
	}
	return id.String()
}

// toTrash renames path into root's trash/ as label: an instant at any size,
// on one filesystem, leaving the removal to the janitor.
func toTrash(root, path, label string) error {
	return os.Rename(path, filepath.Join(root, trashDir, label))
}

// nameRule is which files a measure of a tree counts.
type nameRule int

const (
	// allNames counts every regular file and symbolic link, each as itself:
	// a kept incarnation a boot finds with no state (measureIncarnation),
	// every file of which is its own — one it shares with what the boot gives
	// back is counted here, and never there (givenBack).
	allNames nameRule = iota
	// soleNames counts a regular file only when it has no other name:
	// scratch/ as the janitor measures it (measureScratch), where a file
	// with a second name is one of an incarnation's own linked in — an
	// advertisement view's packed-refs, an adopted push's pack before its
	// scratch goes, a merge's store of the packs it merges, a swap's staged
	// links — and its bytes are counted where that incarnation is. A
	// symbolic link counts as itself.
	soleNames
)

// sizeUnder sums the sizes of the regular files and symbolic links at and
// under root that rule counts, each as itself (eachFile). An ended ctx stops
// the walk and is its error.
func sizeUnder(ctx context.Context, root string, rule nameRule, skipped func(path string, err error),
	listing func(dir string)) (int64, error) {
	var total int64
	err := eachFile(ctx, root, skipped, listing, func(info fs.FileInfo) {
		if rule == soleNames && info.Mode().IsRegular() && linksOf(info) > 1 {
			return
		}
		total += info.Size()
	})
	if err != nil {
		return 0, err
	}
	return total, nil
}

// eachFile hands visit what Lstat says of every regular file and symbolic
// link at and under root — a link is never followed, so a link out of the
// cache never counts what it points at, and anything else that is not a
// directory is no file the cache measures. An entry that is there but cannot
// be read is handed to skipped and left out; an ended ctx stops the walk and
// is its error. listing is nil but in a test (knobs.measuring): it is told
// each directory as the walk is about to list it, root first.
//
// AN ENTRY THAT IS GONE IS NOTHING TO COUNT, WHEREVER THE WALK MEETS ITS
// ABSENCE — the root; a directory a listing named, gone when the walk opens
// it; a file gone when the walk reads its size — and none is handed to
// skipped. The cache's own directories are written while they are measured:
// the janitor walks scratch/ while the host moves advertisement views and
// ended pushes' scratches out of it whole, and a push's receive-pack removes
// its object quarantine there, so the walk finds gone, at any depth, what a
// listing named a moment before. What the directory holds is then what the
// walk counted, and nothing is wrong. Handed to skipped, the absence would be
// logged as a file that could not be measured, with a disk fault's remedy,
// at random on any node that pushes — burying the line that means one. Every
// other error leaves bytes on the disk the walk did not count, and goes to
// skipped. The boot's walks lose nothing by the rule: nothing of this process
// writes the directory then, and an entry something else removed holds no
// bytes either.
func eachFile(ctx context.Context, root string, skipped func(path string, err error), listing func(dir string),
	visit func(info fs.FileInfo)) error {
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				skipped(path, err)
			}
			return nil
		}
		if d.IsDir() {
			if listing != nil {
				listing(path)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				skipped(path, err)
			}
			return nil
		}
		if info.Mode()&fs.ModeSymlink != 0 || info.Mode().IsRegular() {
			visit(info)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("cache: measuring %s: %w", root, err)
	}
	return nil
}

// linksOf is how many names the file info describes has: its link count,
// which every platform this builds for answers in its stat — one that
// answers none is taken to have only its own.
func linksOf(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Nlink)
	}
	return 1
}

// givenBack measures what a boot GIVES BACK to the volume: every tree Open
// moves into trash/ — each entry of repos/ but the incarnations it keeps —
// and trash/, scratch/ and tmp/ themselves, which the janitor empties within
// seconds of the boot. It counts EACH FILE ONCE, by its device and inode,
// and only a file EVERY ONE OF WHOSE NAMES lies in those trees: removing
// them gives back exactly those bytes, and a file with a name anywhere else —
// in an incarnation the boot keeps, whose state counts it — gives back
// nothing.
//
// Hard links put one file under many names here, and every one of them the
// cache's own: an incarnation a swap or a rebuild replaced shares every file
// the next one linked from it (a swap every live pack, HEAD, config,
// packed-refs and the derived files it carries; a rebuild every pack a check
// found whole), whether it is still in repos/ for a reader a crash outlived,
// or in trash/, where its last reader's release, the promote, or the boot
// that found it older put it, the janitor's removal of it cut short by a stop
// or a crash; a failed swap's staging holds a link of every live pack, and a
// crashed one's sits in scratch/ — which the boot then moves into trash/,
// where the next boot finds it if this one dies before its janitor's first
// pass; and a purged or evicted repository's incarnations share each other's
// files in trash/. Counted by every name, a boot counted a kept incarnation's
// linked files once by its state and once more for each name in those trees,
// and the budget the engine derives from Held claimed room the volume does
// not have, by those bytes, for the whole life of the process. Counted only
// where a file has a single name, a file two of those trees share would count
// nowhere, though removing both gives it back.
//
// One walk a tree, a map entry for each file with more than one name. A
// name a walk cannot read leaves its file uncounted, as an unreadable entry
// is: Held errs small, never claiming room the volume lacks.
type givenBack struct {
	ctx     context.Context
	skipped func(path string, err error)
	// single is the bytes of the files with one name, each counted as the
	// walk meets it; linked is every file with more, by its identity.
	single int64
	linked map[fileKey]*linkedFile
}

// fileKey is what every name of one file shares: its device and inode.
type fileKey struct{ dev, ino uint64 }

// linkedFile is one file with more than one name: its size, its link count,
// and how many of its names the walks met.
type linkedFile struct {
	size         int64
	links, found int
}

// newGivenBack is an empty measure whose walks run under ctx and hand an
// entry they cannot read to skipped.
func newGivenBack(ctx context.Context, skipped func(path string, err error)) *givenBack {
	return &givenBack{ctx: ctx, skipped: skipped, linked: map[fileKey]*linkedFile{}}
}

// walk adds every file at and under root to the measure.
func (g *givenBack) walk(root string) error {
	return eachFile(g.ctx, root, g.skipped, nil, func(info fs.FileInfo) {
		links := linksOf(info)
		if links <= 1 {
			g.single += info.Size()
			return
		}
		id := idOf(info)
		key := fileKey{dev: id.dev, ino: id.ino}
		f := g.linked[key]
		if f == nil {
			f = &linkedFile{size: info.Size(), links: links}
			g.linked[key] = f
		}
		f.found++
	})
}

// bytes is what the walks gave back: every file with one name, and every
// file with more whose every name they met, each once.
func (g *givenBack) bytes() int64 {
	total := g.single
	for _, f := range g.linked {
		if f.found >= f.links {
			total += f.size
		}
	}
	return total
}
