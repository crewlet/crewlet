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
var log = logging.Get("gitlet.cache")

// The names under a cache's root.
const (
	// tagName is the Cache Directory Tagging standard's file
	// (https://bford.info/cachedir/): tar, borg and restic run with
	// --exclude-caches skip a directory holding it. The engine's own backup never
	// reads the cache.
	tagName = "CACHEDIR.TAG"
	// lockName is the process claim's sidecar (internal/flock).
	lockName = "lock"
	// reposDir holds one incarnation per repository.
	reposDir = "repos"
	// scratchDir holds each process's own work: receive-pack scratch
	// repositories, advertisement views, request spools (Spool), merge scratches
	// (NewMergeScratch) and a swap's staged next incarnation (swap.go), renamed
	// into repos/ whole, so a crash before the rename leaves the incarnation it
	// replaces the highest and the boot moves the staging aside.
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
	// cohortsName is the newest memo cohort any process of this cache may have
	// answered in; a boot opens its own past it. It is durable before the node's
	// newest cohort passes it (memo.go's reserveCohort).
	cohortsName = "memo-cohorts"
	// lostFound is the directory mkfs makes at an ext filesystem's root: Prepare
	// counts it as the filesystem's, not content, and the cache never opens it.
	lostFound = "lost+found"
)

// tagSignature is the standard's first line, byte for byte: what every
// backup tool looks for.
const tagSignature = "Signature: 8a477f597d28d172789f06886806bc55"

// tagMarker begins the tag's second line and says the tag is THIS cache's:
// every tool following the standard writes the same signature, and another
// tool's tagged directory must not have its tmp/ emptied at our boot. A later
// build may reword what follows it, never the marker.
const tagMarker = "# This file marks a crewlet git cache"

// tagContent is the whole tag this build writes. Its prose claims only what
// holds of every backup: tar, borg and restic skip the directory only when run
// with --exclude-caches.
const tagContent = tagSignature + "\n" + tagMarker + ": disposable copies of a company's " +
	"repositories, rebuilt from the object store whenever they are missing. Crewlet's own " +
	"backup never copies this directory, and a backup run with --exclude-caches leaves it " +
	"out; see https://bford.info/cachedir/\n"

// maxTagBytes bounds the read of a tag: ours is a couple of hundred bytes, and
// the file sits in a directory an operator chose, so it is never read whole.
const maxTagBytes = 4 << 10

// incarnationName matches a directory under repos/: a canonical lowercase
// UUID, then the incarnation's number with no leading zero. Compiled at first
// use (lazyPattern).
var incarnationName = lazyPattern(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.([1-9][0-9]*)\.git$`)

// parseIncarnation reads a directory name under repos/ as the repository's id
// and the incarnation's number, or false for a name off the grammar, a
// non-canonical id or a number past an int.
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

// canonicalID reports whether s is a UUID in its one canonical spelling. The
// id becomes a path component, so a spelling the parser accepts and the printer
// would not write (upper case, braces, urn:) would be a second directory.
func canonicalID(s string) bool {
	id, err := uuid.Parse(s)
	return err == nil && id.String() == s
}

// Layout is a prepared cache: where it is, what it holds, and, unexported,
// this process's claim and Prepare's reading of repos/, which [Open] takes
// over.
type Layout struct {
	// Dir is the cache's root: absolute, clean, every symbolic link in it
	// resolved.
	Dir string

	// Home is Dir/home: gitproc.Options.Home. Prepare does not create it;
	// gitproc.New does, and refuses one that is not empty.
	Home string

	// Held is every byte the cache owns under Dir as Prepare found it, its
	// repositories' and the leftovers in trash/, scratch/ and tmp/, which the
	// engine adds to the volume's free space to derive a budget. Free space
	// excludes the cache's files, so without Held a warm node's budget would
	// shrink at each restart and the janitor evict the difference; the leftovers
	// count because the janitor frees them and the next boot finds them as free
	// space. EACH FILE COUNTS ONCE however many names it has (givenBack): a byte
	// counted twice is room the volume lacks.
	Held int64

	// boot is the claim and reading Open takes over; nil in a Layout
	// Prepare did not return.
	boot *boot
}

// Release gives back the claim of a Layout no [Open] took: a boot failing
// between them must, so `defer layout.Release()` right after Prepare. It does
// nothing once Open has taken the claim, which the cache holds until
// [Cache.Stop]; idempotent, and safe on a Layout Prepare did not return.
func (l Layout) Release() {
	if l.boot != nil && l.boot.taken.CompareAndSwap(false, true) {
		l.boot.claim.Release()
	}
}

// Prepare makes dir (absolute; created if absent, links resolved once) a
// cache directory, CLAIMS it and reads it, before gitproc.New, whose Home it
// returns, and [Open], which takes over the claim and the reading (package
// doc, "The directory"). In order: a non-empty dir without this cache's tag
// is refused untouched ([ErrNotACache]; mkfs's lost+found is not content);
// the tag is written first, so a crash leaves a directory the next Prepare
// knows; the claim is taken before anything changes, as the next step moves a
// live HOME ([ErrCacheInUse], in this process too until released); the
// directories are made and a leftover home/, which gitproc.New refuses, goes
// to trash/; repos/ is read and Held summed, an unreadable entry logged
// (code_cache_held_unmeasured), not fatal (package doc, "A boot starts from
// the disk, once"), bounded by ctx. Any failure releases the claim.
func Prepare(ctx context.Context, dir string) (Layout, error) {
	if !filepath.IsAbs(dir) {
		return Layout{}, fmt.Errorf("%w: gitlet.cache_dir %q is not an absolute path", ErrOptions, dir)
	}
	if err := os.MkdirAll(dir, hostbox.DirMode); err != nil {
		return Layout{}, fmt.Errorf("cache: making gitlet.cache_dir %s: %w", dir, err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return Layout{}, fmt.Errorf("cache: resolving gitlet.cache_dir %s: %w", dir, err)
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

// adoptDirectory is Prepare's tag check and first write: root is empty but
// for a lost+found directory, or already this cache's, its tag completed when
// a crash cut the first write short.
func adoptDirectory(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("cache: listing gitlet.cache_dir %s: %w", root, err)
	}
	content := 0
	for _, d := range entries {
		// mkfs's directory is not content; a link named lost+found is (ReadDir
		// does not follow it).
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
		// A tag cut short is this build's first write: no other tool writes a
		// prefix of this text, and nothing is written before the tag.
		return writeTag(root)
	case tag == tagAbsent:
		return fmt.Errorf("%w: %s is not empty and is not a crewlet git cache (it has no %s): "+
			"point gitlet.cache_dir at an empty directory or a volume of its own — the cache "+
			"removes what it finds under tmp/, scratch/ and trash/ at every boot",
			ErrNotACache, root, tagName)
	default:
		return fmt.Errorf("%w: %s holds a %s this cache did not write (another tool's cache): "+
			"point gitlet.cache_dir at an empty directory or a volume of its own — the cache "+
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

// readTag judges root's tag. A FAILED read is an error, never an answer: an
// unreadable tag is not thereby somebody else's.
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

// writeTag writes the tag IN PLACE, the one cache file not staged in tmp/, and
// makes it and its name durable. There is no tmp/ yet, and making one first
// would leave a crash a non-empty untagged directory every later Prepare
// refuses; a tag cut short is recognised (tagPartial) and completed.
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
			"gitlet.cache_dir; give each its own", ErrCacheInUse, root, held.Holder)
	case err != nil:
		return nil, fmt.Errorf("cache: claiming %s: %w", root, err)
	}
	return claim, nil
}

// bootLabel names what a boot moves into trash/: boot-<uuidv7>-<name>.
func bootLabel(name string) string { return "boot-" + newV7() + "-" + name }

// newV7 is a fresh UUIDv7's text. uuid.NewV7 fails only when the system's
// randomness does, when no object store key could be minted either, so it
// panics rather than name two trash entries alike.
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
	// allNames counts every regular file and symbolic link: a stateless kept
	// incarnation (measureIncarnation), whose every file is its own; one it
	// shares with what the boot gives back counts here, never in givenBack.
	allNames nameRule = iota
	// soleNames counts a regular file only when it has one name: scratch/ as the
	// janitor measures it (measureScratch), where a second name is an
	// incarnation's file linked in (a view's packed-refs, an adopted push's pack,
	// a merge's store, a swap's staged links), counted with that incarnation. A
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

// eachFile hands visit the Lstat of every regular file and symbolic link at
// and under root, never following a link, so a link out of the cache never
// counts its target. An unreadable entry goes to skipped; an ended ctx stops
// the walk and is its error. listing is nil but in a test (knobs.measuring):
// it is told each directory as the walk lists it, root first.
//
// AN ENTRY THAT IS GONE IS NOTHING TO COUNT, at any depth, and never goes to
// skipped: the janitor walks scratch/ while views, ended pushes' scratches and
// receive-pack's quarantines leave it, and logging those as unmeasured, with a
// disk fault's remedy, would bury the line that means one. Every other error
// leaves bytes uncounted and goes to skipped.
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

// linksOf is the file's link count; a platform whose stat gives none is taken
// to have one name.
func linksOf(info fs.FileInfo) int {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Nlink)
	}
	return 1
}

// givenBack measures what a boot GIVES BACK to the volume: every tree Open
// moves into trash/ (each entry of repos/ but the kept incarnations) and
// trash/, scratch/ and tmp/, which the janitor empties within seconds. It
// counts EACH FILE ONCE, by device and inode, and only a file ALL OF WHOSE
// NAMES lie in those trees: one named in a kept incarnation, whose state
// counts it, gives back nothing. Swaps and rebuilds hard-link files across
// incarnations, so older copies, failed or crashed swaps' stagings and a
// purged repository's copies share files (package doc, "A boot starts from
// the disk, once"). Counting every name makes Held claim room the volume
// lacks; counting sole names misses a file two given-back trees share. An
// unreadable name leaves its file uncounted: Held errs small.
type givenBack struct {
	ctx     context.Context
	skipped func(path string, err error)
	// single is the bytes of one-name files; linked is every file with more,
	// by identity.
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
