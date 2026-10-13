package cache

import (
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SyncKind is what handing out a cache's directory cost: a closed set
// ([SyncKinds], [SyncKind.Valid]), and the host's fetch.duration{sync}
// attribute.
type SyncKind string

const (
	// SyncNone is an answer from what the cache holds, with no estate call.
	SyncNone SyncKind = "none"
	// SyncDelta is the rows that moved since the cache's position.
	SyncDelta SyncKind = "delta"
	// SyncFull is the repository's whole answer.
	SyncFull SyncKind = "full"
)

// SyncKinds is every [SyncKind], in the order of what each costs.
var SyncKinds = []SyncKind{SyncNone, SyncDelta, SyncFull}

// Valid reports whether k is one of [SyncKinds].
func (k SyncKind) Valid() bool { return slices.Contains(SyncKinds, k) }

// Synced is a repository's directory, handed to a reader PINNED: Dir is valid
// until Release, and THERE IS NO OTHER WAY TO A REPOSITORY'S DIRECTORY — no
// path accessor that skips the pin, so no reader can hold a directory a
// rebuild or a purge is about to remove. Every holder defers Release.
type Synced struct {
	// Dir is the incarnation's directory, absolute and clean.
	Dir string
	// Repo is the repository row the cache last read.
	Repo code.RepoRef
	// Position: every row up to here is reflected in Dir.
	Position statelog.Position
	// Kind is what this hand-out cost.
	Kind SyncKind
	// Release unpins Dir. Idempotent: a second call does nothing.
	Release func()

	// inc is which incarnation Dir is.
	inc *incarnation
}

// entry is one repository's place in the cache. Lock order is ALWAYS
// Cache.mu, then entry.mu, and nothing holds entry.mu across a git process,
// an estate call or a file copy.
type entry struct {
	id string

	// mu guards everything below but stateMu, and every incarnation's
	// fields.
	mu sync.Mutex
	// current is the incarnation readers are handed; nil when there is no
	// directory yet.
	current *incarnation
	// retiring is every incarnation replaced or purged while a reader still
	// held it, removed as its last pin is released.
	retiring []*incarnation

	// stateMu serialises persisting the state.
	stateMu sync.Mutex
}

// incarnation is one directory of a repository: repos/<id>.<n>.git. A
// rebuild builds the next number BESIDE the one readers hold rather than
// renaming a path under them — a path is a NAME, and a git process whose
// GIT_DIR is that name would otherwise open the new directory's files half
// way through its run.
type incarnation struct {
	n   int
	dir string

	// pins counts the readers holding the directory.
	pins int
	// retired: replaced or purged; the directory goes as the last pin is
	// released.
	retired bool

	// state is the state as last persisted; nil when there is none.
	state *state
	// at is the in-memory position, at or past state's: a round whose delta
	// was empty moves it without rewriting the state, since nothing between
	// the two changed the directory.
	at statelog.Position
	// stream is the StreamCreated of the answer at came from.
	stream     time.Time
	repo       code.RepoRef
	refsDigest string
	refs       int
	packs      int
	// diskBytes is the pack files and packed-refs; metaBytes the
	// maintainer's files.
	diskBytes, metaBytes int64
	midx                 midxState
	// lastUsed is when a reader last took or released it.
	lastUsed time.Time
	// stamped is the last use the disk records (lastUsedOn): read at boot,
	// moved forward by the janitor's recordUse.
	stamped time.Time
}

// fromState sets inc's in-memory fields from a state read at boot.
func (inc *incarnation) fromState(st *state) {
	inc.state = st
	inc.at = st.Position
	inc.stream = st.StreamCreated
	inc.repo = st.Repo
	inc.refsDigest = st.RefsDigest
	inc.refs = st.Refs
	inc.packs = st.Packs
	inc.diskBytes = st.DiskBytes
	inc.metaBytes = st.Midx.Bytes
	inc.midx = st.Midx
}

// stateValue is the state inc's in-memory fields describe; the entry's mu is
// held.
func (inc *incarnation) stateValue(id string) state {
	midx := inc.midx
	midx.Bytes = inc.metaBytes
	return state{
		Format:        stateFormat,
		RepoID:        id,
		StreamCreated: inc.stream,
		Position:      inc.at,
		Repo:          inc.repo,
		RefsDigest:    inc.refsDigest,
		Refs:          inc.refs,
		Packs:         inc.packs,
		DiskBytes:     inc.diskBytes,
		Midx:          midx,
	}
}

// pin hands inc to a reader; the entry's mu is held.
func (c *Cache) pin(e *entry, inc *incarnation, kind SyncKind) Synced {
	inc.pins++
	inc.lastUsed = c.now()
	return Synced{
		Dir:      inc.dir,
		Repo:     inc.repo,
		Position: inc.at,
		Kind:     kind,
		Release:  sync.OnceFunc(func() { c.release(e, inc) }),
		inc:      inc,
	}
}

// release is a reader letting inc go. The last release of a RETIRED
// incarnation moves its directory into trash/ and kicks the janitor; the
// removal itself is the janitor's, so a request that lets go of a 20 GiB
// directory returns at once. Safe after Stop.
func (c *Cache) release(e *entry, inc *incarnation) {
	e.mu.Lock()
	defer e.mu.Unlock()
	inc.pins--
	inc.lastUsed = c.now()
	if inc.pins == 0 && inc.retired {
		e.retiring = slices.DeleteFunc(e.retiring, func(r *incarnation) bool { return r == inc })
		c.discard(inc)
	}
}

// retire takes inc out of service — a rebuild replaced it, or the repository
// was purged; the entry's mu is held. Unpinned, its directory goes into
// trash/ at once; pinned, it waits in retiring for its last reader.
func (c *Cache) retire(e *entry, inc *incarnation) {
	if inc.retired {
		return
	}
	inc.retired = true
	if e.current == inc {
		e.current = nil
	}
	if inc.pins > 0 {
		e.retiring = append(e.retiring, inc)
		return
	}
	c.discard(inc)
}

// discard moves a retired incarnation nobody holds into trash/ and kicks the
// janitor. A rename that fails leaves the directory where it is: no entry
// names it any more, and the next boot's reading trashes it as an older
// incarnation or converges it as the newest — so it is logged, never fatal.
// Once Stop has begun it moves nothing, for the same reason: the directory
// is the next Open's, which may be another process's already. A move that
// began before Stop is work Stop waits for (enter).
func (c *Cache) discard(inc *incarnation) {
	if !c.enter() {
		return
	}
	defer c.leave()
	if err := toTrash(c.dir, inc.dir, newV7()+"-"+filepath.Base(inc.dir)); err != nil {
		c.log.Warn("code_cache_trash_failed", "path", inc.dir, "error", err)
		return
	}
	c.kick()
}
