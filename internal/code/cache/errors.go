package cache

import (
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/code"
)

var (
	// ErrOptions is an Options value Open refuses — a Layout already opened
	// or released among them — or a directory Prepare cannot take: wrapped
	// with the field or the setting to change.
	ErrOptions = errors.New("cache: refused options")

	// ErrNotACache is a directory that is not a crewlet git cache: Prepare's
	// refusal of a non-empty directory without the cache's CACHEDIR.TAG, and
	// Open's of a Layout Prepare did not return.
	ErrNotACache = errors.New("cache: not a git cache directory")

	// ErrCacheInUse is a cache directory another process holds — or a
	// Layout or a Cache of this one, until it is released or stopped: two
	// nodes pointed at one git.cache_dir.
	ErrCacheInUse = errors.New("cache: another process holds this cache directory")

	// ErrStopped is work asked of a cache that has been stopped: a sync, a
	// scratch, an adoption, a check of missing objects. What lets go of
	// something — a Release, a scratch's Remove — stays safe after Stop.
	ErrStopped = errors.New("cache: stopped")

	// ErrInvalid is an argument a cache refuses, wrapped with the argument
	// and why: a repository or push id that is not a canonical UUID (each
	// becomes a path component), a floor on another log, a Synced or a
	// scratch another cache handed out, an object id that is not one, a
	// pack whose row and scratch disagree.
	ErrInvalid = errors.New("cache: refused")

	// ErrBadSnapshot is a snapshot row this build cannot install: a ref or
	// a default branch the grammar refuses — a ref is written into a file
	// git parses line by line, so a newline or a space in one would corrupt
	// packed-refs for every reader — an object id that is not one, a pack
	// part this build does not split, a pack whose sizes lay out no bundle.
	// Wrapped naming the page and the row. Never retried: the rows say it,
	// and asking again reads the same rows.
	ErrBadSnapshot = errors.New("cache: a snapshot this build cannot install")

	// ErrRebuildRefused is a rebuild that could not end the failure it was
	// asked for: of an incarnation holding exactly what a rebuild that did
	// not end it read, less than a day ago, or within the window after the
	// repository's last attempt (Cache.Rebuild). Wrapped naming the
	// repository and `crewlet code verify`, which says what a rebuild could
	// not repair. The host answers it 503.
	ErrRebuildRefused = errors.New("cache: a rebuild would not end the failure")

	// ErrNoMemory is a whole-repository git process a person asked for —
	// Verify's connectivity check — whose charge alone is more than this
	// node can give its background work: the node's memory, less what it
	// keeps for its git lanes, the store, the broker and the engine
	// (memory.go). Definite for this node at this memory, and no retry
	// changes it; a node with more memory can run it. Wrapped naming the
	// repository, the charge and the budget. The background work's own steps
	// are never an error: they are refused and said so (the
	// code_maintenance_refused alarm).
	ErrNoMemory = errors.New("cache: this node's memory cannot hold it")
)

// ErrPackMissing is a pack a row names that the object store could not hand
// over, definitively: absent (objstore.ErrNotFound), or Damaged — bytes that
// are not the row's (objstore.ErrCorrupt). The host answers 503 naming the
// pack; the collector's daily audit raises objects_missing for the same
// object, which is the alarm.
type ErrPackMissing struct {
	Repo    string
	Pack    code.PackRow
	Damaged bool
	Err     error
}

func (e *ErrPackMissing) Error() string {
	if e.Damaged {
		return fmt.Sprintf("cache: pack %s of %s (object %s) is damaged in the object store: %v",
			e.Pack.PackID, e.Repo, e.Pack.Object, e.Err)
	}
	return fmt.Sprintf("cache: pack %s of %s (object %s) is missing from the object store",
		e.Pack.PackID, e.Repo, e.Pack.Object)
}

// Unwrap is the store's own answer, so errors.Is sees objstore.ErrNotFound
// or objstore.ErrCorrupt.
func (e *ErrPackMissing) Unwrap() error { return e.Err }

// ErrCorruptPack is a pack whose verified bytes are not the bundle its row
// describes (code.CheckBundle: a section's head, count, size or pack
// checksum disagrees). The store's SHA-256 has just said these ARE the bytes
// the receiver stored, so the row or the uploader is wrong, and no retry
// fixes either.
type ErrCorruptPack struct {
	Repo string
	Pack code.PackRow
	// Err wraps code.ErrBadBundle, naming the field that disagrees.
	Err error
}

func (e *ErrCorruptPack) Error() string {
	return fmt.Sprintf("cache: pack %s of %s (object %s) is not the bundle its row describes: %v",
		e.Pack.PackID, e.Repo, e.Pack.Object, e.Err)
}

// Unwrap is CheckBundle's answer, so errors.Is sees code.ErrBadBundle.
func (e *ErrCorruptPack) Unwrap() error { return e.Err }
