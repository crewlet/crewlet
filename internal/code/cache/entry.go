package cache

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
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
	// running is the round in flight, and next the one queued behind it:
	// at most one of each (sync.go).
	running, next *round
	// dead: the repository was purged and the entry left the cache; a
	// caller that finds it so looks the repository up again.
	dead bool

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
	// discarded: moved into trash/ (or given up on) — its bytes have left the
	// cache's total, and nothing it does moves the total again.
	discarded bool
	// converged: a full round ran on it in this process, so every later
	// round may be a delta — nothing but this process has written the
	// directory since — and a caller it already meets is answered from it
	// at once.
	converged bool
	// reread is why its next full round reads it whole again — a log
	// recreated or gone back under it (Cache.reread) — counted when that
	// round completes; "" for none.
	reread string

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
	// maintainer's files. Set only through setSize, which keeps the cache's
	// total in step.
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
		c.heldRepos.Add(-1)
	}
	if inc.pins > 0 {
		e.retiring = append(e.retiring, inc)
		return
	}
	c.discard(inc)
}

// setSize sets what inc's files hold, keeping the cache's running total
// (Cache.heldBytes) in step; the entry's mu is held. Every change to an
// incarnation's sizes goes through here, so the total is never recomputed.
func (c *Cache) setSize(inc *incarnation, disk, meta int64) {
	if !inc.discarded {
		c.heldBytes.Add(disk + meta - inc.diskBytes - inc.metaBytes)
	}
	inc.diskBytes, inc.metaBytes = disk, meta
}

// discard moves a retired incarnation nobody holds into trash/ and kicks the
// janitor. A rename that fails leaves the directory where it is: no entry
// names it any more, and the next boot's reading trashes it as an older
// incarnation or converges it as the newest — so it is logged, never fatal.
// Once Stop has begun it moves nothing, for the same reason: the directory
// is the next Open's, which may be another process's already. A move that
// began before Stop is work Stop waits for (enter).
//
// Its bytes leave the cache's total here, whether or not the move is made:
// nothing names the directory any more, and a directory a failed move left is
// the next boot's to count and clear.
func (c *Cache) discard(inc *incarnation) {
	if !inc.discarded {
		inc.discarded = true
		c.heldBytes.Add(-(inc.diskBytes + inc.metaBytes))
		c.setGauges()
	}
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

// newIncarnation makes e's next incarnation — one past the highest it has
// had — and makes it e's current one: there is nothing else to serve, and no
// reader is handed it before a round has converged it (a reader is answered
// at once only from an incarnation this process has converged).
//
// Its skeleton is a bare repository git can run in: HEAD naming main (the
// round's refs step names the repository's own default branch before any
// reader sees it), config gitproc.RepoConfig, a packed-refs of the header
// alone, and the empty directories refs/heads, refs/tags, objects/pack and
// objects/info. The number is taken by os.Mkdir, never by a rename over a
// name: a directory already there under it — a purged repository's
// incarnation a reader still holds — keeps its name, and the next number is
// taken. A skeleton that cannot be written is moved into trash/.
func (c *Cache) newIncarnation(e *entry) (*incarnation, error) {
	e.mu.Lock()
	n := 1
	for _, inc := range append([]*incarnation{e.current}, e.retiring...) {
		if inc != nil && inc.n >= n {
			n = inc.n + 1
		}
	}
	e.mu.Unlock()
	repos := filepath.Join(c.dir, reposDir)
	var dir string
	for ; ; n++ {
		dir = filepath.Join(repos, e.id+"."+strconv.Itoa(n)+".git")
		err := os.Mkdir(dir, hostbox.DirMode)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("making %s: %w", dir, err)
		}
	}
	if err := c.skeleton(dir); err != nil {
		if terr := toTrash(c.dir, dir, newV7()+"-"+filepath.Base(dir)); terr != nil {
			err = errors.Join(err, terr)
		}
		return nil, err
	}
	inc := &incarnation{n: n, dir: dir, lastUsed: c.now()}
	e.mu.Lock()
	if e.current == nil {
		c.heldRepos.Add(1)
	}
	e.current = inc
	e.mu.Unlock()
	return inc, nil
}

// skeleton writes an empty bare repository's files into dir.
func (c *Cache) skeleton(dir string) error {
	for _, d := range []string{"refs/heads", "refs/tags", "objects/pack", "objects/info"} {
		if err := os.MkdirAll(filepath.Join(dir, d), hostbox.DirMode); err != nil {
			return fmt.Errorf("making %s: %w", filepath.Join(dir, d), err)
		}
	}
	for _, f := range []struct{ name, content string }{
		{"HEAD", string(headFor("main"))},
		{"config", gitproc.RepoConfig},
		{"packed-refs", packedRefsHeader},
	} {
		staged, err := c.stage(hostbox.FileMode, func(w *os.File) error {
			_, werr := w.WriteString(f.content)
			return werr
		})
		if err != nil {
			return err
		}
		if err = c.place(staged, filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	for _, d := range []string{dir, filepath.Dir(dir)} {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}
