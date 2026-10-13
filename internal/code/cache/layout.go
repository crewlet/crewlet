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
	"regexp"
	"strconv"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/filelock"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/logging"
)

// log is the package's logger, which a Cache uses when Options.Logger is nil
// and which Prepare, running before any Cache exists, always uses.
var log = logging.Get("code.cache")

// The names under a cache's root.
const (
	// tagName is the Cache Directory Tagging standard's file
	// (https://bford.info/cachedir/): tar --exclude-caches, borg and restic
	// skip a directory holding it, so a cache that sits under the store's
	// directory is never in a backup of it.
	tagName = "CACHEDIR.TAG"
	// lockName is the process claim's sidecar (internal/filelock).
	lockName = "lock"
	// reposDir holds one incarnation per repository.
	reposDir = "repos"
	// scratchDir holds a receive-pack's scratch repositories and the
	// advertisement views; every one belongs to a request of the process
	// that made it.
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

// tagContent is the whole of the tag this build writes.
const tagContent = tagSignature + "\n" + tagMarker + ": disposable copies of a company's " +
	"repositories, rebuilt from the object store whenever they are missing. It is never " +
	"backed up; see https://bford.info/cachedir/\n"

// maxTagBytes bounds the read of a tag: ours is a couple of hundred bytes,
// and the file sits in a directory an operator chose, so whatever is there is
// never read whole into memory to be judged.
const maxTagBytes = 4 << 10

// incarnationName matches a repository's directory under repos/: a canonical
// lowercase UUID, then the incarnation's number with no leading zero.
var incarnationName = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.([1-9][0-9]*)\.git$`)

// parseIncarnation reads a directory name under repos/: the repository's id
// and the incarnation's number, or false for anything else — a name that is
// not the grammar, an id that is not a canonical UUID, or a number past what
// an int holds.
func parseIncarnation(name string) (string, int, bool) {
	m := incarnationName.FindStringSubmatch(name)
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
	// next one's for no change in the disk.
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
//  4. The directory is CLAIMED for this process (internal/filelock); one
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
//     from the reading: an incarnation with a state of this build counts what
//     its state says it holds, so a warm node's boot reads one small file a
//     repository and stats none of its packs. Only what has no state to
//     count — an incarnation whose state is missing, unreadable or another
//     build's, and an entry that is not an incarnation — is walked, and so
//     are trash/, scratch/ and tmp/, which the janitor keeps small. An entry
//     a walk cannot read is skipped and logged (code_cache_held_unmeasured),
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
func claimDir(root string) (*filelock.Claim, error) {
	claim, err := filelock.Take(filepath.Join(root, lockName))
	var held *filelock.HeldError
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

// sizeUnder sums the sizes of the regular files and symbolic links at and
// under root, each as itself — a link is never followed, so a link out of the
// cache never counts what it points at. A missing root is zero; an entry
// that cannot be read is handed to skipped and left out. An ended ctx stops
// the walk and is its error.
func sizeUnder(ctx context.Context, root string, skipped func(path string, err error)) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if path != root || !errors.Is(err, fs.ErrNotExist) {
				skipped(path, err)
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				skipped(path, err)
			}
			return nil
		}
		if info.Mode().IsRegular() || info.Mode()&fs.ModeSymlink != 0 {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cache: measuring %s: %w", root, err)
	}
	return total, nil
}
