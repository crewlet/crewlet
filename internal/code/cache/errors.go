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

	// ErrLogRead is the code log failing a sync's read of a repository's
	// rows — no quorum for a linearizable read, a copy out of service or
	// behind, no data node answering — wrapped with the estate's own
	// error. The one failure of a sync that is the code log's: the host
	// answers it as that, and nothing else as that. (A repository the
	// rows do not hold is code.ErrRepoNotFound, an answer, which a sync
	// reads at the log's head before concluding anything from.)
	ErrLogRead = errors.New("cache: the code log could not be read")

	// ErrStoreRead is the object store failing to hand a pack over for a
	// reason that is no answer about the pack — a timeout, a throttle past
	// its retries, the store unreachable — wrapped naming the pack and the
	// store's own error. A definite answer about the pack is an
	// *ErrPackMissing instead.
	ErrStoreRead = errors.New("cache: the object store could not be read")

	// ErrRebuildRefused is a rebuild that could not end the failure it was
	// asked for: of an incarnation holding exactly what a rebuild that did
	// not end it read, less than a day ago, or within the window after the
	// repository's last attempt (Cache.Rebuild). Wrapped naming the
	// repository and `crewlet code verify`, which says what a rebuild could
	// not repair. The host answers it 503.
	ErrRebuildRefused = errors.New("cache: a rebuild would not end the failure")

	// ErrNoMemory is a git process a caller asked for — Verify's
	// connectivity check of a whole repository, a person's, or a compaction's
	// build over the packs it merges (Cache.AdmitCompaction) — whose charge
	// alone is more than this node can give its background work: the node's
	// memory, less what it keeps for its git lanes, the store, the broker and
	// the engine (memory.go). Definite
	// for this node AT THIS MEMORY: a node with more memory can run it, and so
	// can this one once its memory is raised in place, which the budget reads
	// again at every maintainer pass — so a caller asks again later rather
	// than remembering the refusal. Wrapped naming the repository, the charge
	// and the budget. The maintainer's own steps are never an error: they are
	// refused and said so (the code_maintenance_refused alarm).
	ErrNoMemory = errors.New("cache: this node's memory cannot hold it")

	// ErrNotHeld is a check asked of a repository this node holds no copy
	// of: it never served it here, or evicted it. Nothing was read or
	// downloaded, and nothing is: a check builds no copy, since building one
	// is a whole download of the repository from the object store into a
	// cache that would then keep it and evict a warm repository to do so
	// (verify.go). Definite for this node now; a git node that serves the
	// repository holds one. The API answers it 404.
	ErrNotHeld = errors.New("cache: this node holds no copy of the repository")

	// ErrPastBound is a check of a repository's copy that ran past the bound
	// what the copy holds gives it (CheckBudget) and was ended there: a disk
	// or a node far slower than the bound allows for, or a check wedged on
	// one. Never a finding about the copy — what the check found before it
	// was ended is in its report — and the check is had again by asking
	// again. Wrapped naming the bound and the bytes it was over.
	ErrPastBound = errors.New("cache: the check ran past its bound")
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
