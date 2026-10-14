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

	"golang.org/x/sync/semaphore"

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

	// release unpins Dir, once: what [Synced.Release] calls.
	release func()
	// inc is which incarnation Dir is, and entry the repository it is one
	// of: what Rebuild reads to tell whether that incarnation is still the
	// one readers are handed. owner is the cache that handed it out — the
	// one whose entry and incarnation these are.
	inc   *incarnation
	entry *entry
	owner *Cache
}

// Release unpins Dir. Idempotent — a second call does nothing — and A NO-OP
// ON THE ZERO Synced, which every failed Sync answers: a holder that defers
// the release of a variable a later Sync may overwrite (a fetch asking once
// more at the log's head) releases whatever the variable holds when it
// returns, and a failed second Sync must leave it something safe to release
// rather than a nil function that panics on the request's goroutine.
func (s Synced) Release() {
	if s.release != nil {
		s.release()
	}
}

// handedOut refuses a Synced this cache did not hand out — a zero one, or
// another cache's — by name: every method that takes one acts on its
// incarnation, and only the cache that pinned it may.
func (c *Cache) handedOut(s Synced, what string) error {
	if s.inc == nil || s.entry == nil || s.owner != c {
		return fmt.Errorf("%w: %s names the Synced a sync of this cache handed out", ErrInvalid, what)
	}
	return nil
}

// entry is one repository's place in the cache. Lock order is ALWAYS write,
// then Cache.mu, then entry.mu; nothing holds Cache.mu or entry.mu across a
// git process, an estate call or a file copy, and nothing that holds either
// WAITS for write — an adoption's try of it and a holder's release are taken
// under entry.mu, and neither waits (adopt.go).
type entry struct {
	id string

	// write is what may change what the directory HOLDS, or must see it
	// whole: held WHOLE (writeExclusive) by every round for its whole body
	// and by Verify for its listing, and ONE of it by each adoption for its
	// links. A round writes the directory by steps that assume nothing else
	// does — a full one sweeps what its snapshot does not name and counts
	// what it finds, so a pack linked in beside it could be swept as a stray
	// or counted twice — and Verify's listing must see no step half done;
	// adoptions link distinct files, so they share it. Taken whole under a
	// context, so a Verify waiting behind a round's downloads gives up with
	// its caller; and an adoption only TRIES it: one that finds a round or a
	// listing holding it or waiting for it hands its pack to that holder
	// (handed) rather than make a push wait behind somebody else's
	// downloads — a weighted semaphore's try fails while a whole taker
	// waits, so adoptions never starve a round.
	write *semaphore.Weighted
	// handed is the pushed packs adoptions handed to whoever holds write
	// whole, by git checksum: staged in tmp/ (adopt.go), claimed by a round
	// about to download one, and installed by the holder before it lets go
	// (releaseWrite).
	handed map[string]*staging

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
	// dead: the repository was purged or evicted and the entry left the
	// cache; a caller that finds it so looks the repository up again.
	dead bool
	// maint is the maintenance running on an incarnation of the repository,
	// if any (maintain.go): one at a time a repository.
	maint *maintRun
	// verifying counts the Verify calls checking the repository: no
	// maintenance run starts while one does (verify.go), from its listing
	// to its fsck — which a run in the lane's last free slot would refuse.
	// A run already in flight is never stopped for one: its own files are
	// admitted instead (maintenanceWriting).
	verifying int
	// lastAdded is when a round last added packs: the maintainer's debounce.
	lastAdded time.Time
	// uncoveredSince is when the oldest pack the maintainer's index does not
	// cover arrived — set by the round that adds packs while none wait, and
	// cleared by a run whose index covers every pack — and zero when none
	// waits: the debounce's ceiling (MidxMaxWait).
	uncoveredSince time.Time
	// lastRebuild is when the last rebuild ATTEMPT started; lastAsked when a
	// rebuild of the current incarnation was last ASKED for, whether it was
	// refused, joined or started — the quiet that ends a line of attempts
	// (RebuildQuiet) is measured from it; and refusedLogged is when a
	// refusal was last logged: a repository failing on every request logs a
	// refusal once a RebuildCooldown, never once a request.
	lastRebuild, lastAsked, refusedLogged time.Time

	// stateMu serialises persisting the state.
	stateMu sync.Mutex
}

// newEntry is repository id's place in the cache, with no incarnation yet.
func newEntry(id string) *entry {
	return &entry{id: id, write: semaphore.NewWeighted(writeExclusive)}
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
	reread RebuildReason
	// rebuilds is how many rebuild attempts in a row stand behind this
	// incarnation without having ended the failure that asked for them: none
	// for one no rebuild made, one more than its predecessor's for a
	// rebuild's product, and one more for each rebuild of it that failed —
	// IN A ROW: a rebuild asked for after RebuildQuiet with none asked
	// starts the line again from none.
	// unmoved: what it holds is what the last of those attempts read — it IS
	// that rebuild's product, or that rebuild failed definitively on its
	// rows — and nothing has written into it since: no round moved its packs
	// or its refs, wrote its index or swept a file out of it, and the
	// maintainer wrote no index and no graph into it. Cleared, too, with the
	// line. Both are what Rebuild refuses by (rebuild.go).
	rebuilds int
	unmoved  bool

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
	// objects is how many objects its packs hold, as their rows count them:
	// set whole by every full round and raised by each pack a delta installs,
	// so it is exact on every incarnation this process has converged — the
	// only ones the maintainer runs on, whose memory it estimates by it.
	// Never persisted: a boot's first round is a full one.
	objects int64
	// diskBytes is the pack files and packed-refs; metaBytes the
	// maintainer's files. Set only through setSize, which keeps the cache's
	// total in step.
	diskBytes, metaBytes int64
	midx                 midxState
	// memoryRefused is the steps the node's memory has refused it — each
	// one whose charge alone is more than the node can give its background
	// work — and said so: each logged and counted once an incarnation,
	// however many passes, rounds and checks find it again.
	memoryRefused maintStep
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
		release:  sync.OnceFunc(func() { c.release(e, inc) }),
		inc:      inc,
		entry:    e,
		owner:    c,
	}
}

// release is a reader letting inc go. The last release of a RETIRED
// incarnation moves its directory into trash/ and kicks the janitor; the
// removal itself is the janitor's, so a request that lets go of a 20 GiB
// directory returns at once. The last release of a current incarnation while
// the cache is over its budget kicks the janitor too: a repository too large
// to keep is served while a request holds it, and evicted as soon as none
// does rather than at the next tick. Safe after Stop.
func (c *Cache) release(e *entry, inc *incarnation) {
	e.mu.Lock()
	inc.lastUsed = c.now()
	gone := c.unpin(e, inc)
	e.mu.Unlock()
	c.trash(gone)
}

// unpin drops one pin on inc; the entry's mu is held. With the last one, a
// retired incarnation is discarded, and unpin answers the directory its
// caller moves into trash/ once it has let go of the entry (Cache.trash);
// and a current one while the cache is over its budget kicks the janitor,
// since it was not evictable while held. What a reader's release and the
// maintainer's share — a maintenance run's pin keeps a repository from the
// budget exactly as a reader's does, so its end must not leave the eviction
// to the tick — but the maintainer's pin is no USE, so it leaves lastUsed as
// it was and the budget's order is the readers' alone.
func (c *Cache) unpin(e *entry, inc *incarnation) string {
	inc.pins--
	switch {
	case inc.pins > 0:
	case inc.retired:
		e.retiring = slices.DeleteFunc(e.retiring, func(r *incarnation) bool { return r == inc })
		return c.discard(inc)
	case c.total() > c.maxBytes:
		c.kick()
	}
	return ""
}

// retire takes inc out of service — a rebuild replaced it, or the repository
// was purged or evicted; the entry's mu is held. A maintenance run on it is
// cancelled — what it writes is about to be thrown away — without waiting for
// it, since its pin keeps the directory until it has ended. Unpinned, it is
// discarded at once, and retire answers the directory its caller moves into
// trash/ once it holds no lock (Cache.trash); pinned, it waits in retiring for
// its last reader, and retire answers "".
func (c *Cache) retire(e *entry, inc *incarnation) string {
	if inc.retired {
		return ""
	}
	inc.retired = true
	if m := e.maint; m != nil && m.inc == inc {
		m.cancel()
	}
	if e.current == inc {
		e.current = nil
		c.heldRepos.Add(-1)
	}
	if inc.pins > 0 {
		e.retiring = append(e.retiring, inc)
		return ""
	}
	return c.discard(inc)
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

// discard takes a retired incarnation nobody holds out of the cache — its
// bytes out of the total, whether or not its directory is ever moved: nothing
// names the directory any more, and one a failed move left is the next boot's
// to count and clear — and answers the directory, which its caller moves into
// trash/ (Cache.trash) once it holds NO LOCK: a rename can wait on the
// filesystem's journal, and the locks a discard is decided under are every
// request's to the repository (entry.mu) and, for an eviction or a purge,
// every request's to any repository on the node (Cache.mu). A repository's
// next incarnation never waits for the move either: it takes the next free
// number while the old directory is still there (newIncarnation). The
// entry's mu is held; "" when inc was already discarded.
func (c *Cache) discard(inc *incarnation) string {
	if inc.discarded {
		return ""
	}
	inc.discarded = true
	c.heldBytes.Add(-(inc.diskBytes + inc.metaBytes))
	return inc.dir
}

// trash moves a directory a discard answered into trash/, kicks the janitor
// and sets the gauges; "" does nothing. A rename that fails leaves the
// directory where it is: no entry names it any more, and the next boot's
// reading trashes it as an older incarnation or converges it as the newest —
// so it is logged, never fatal. Once Stop has begun it moves nothing, for the
// same reason: the directory is the next Open's, which may be another
// process's already. A move that began before Stop is work Stop waits for
// (enter). Called holding no lock.
func (c *Cache) trash(dir string) {
	if dir == "" {
		return
	}
	c.setGauges()
	if !c.enter() {
		return
	}
	defer c.leave()
	if err := toTrash(c.dir, dir, newV7()+"-"+filepath.Base(dir)); err != nil {
		c.log.Warn("code_cache_trash_failed", "path", dir, "error", err)
		return
	}
	c.kick()
}

// newIncarnation makes e's next incarnation — one past the highest it has
// had. A FIRST incarnation (current: the entry has none) is e's current one
// from the moment it is made: there is nothing else to serve, and no reader
// is handed it before a round has converged it (a reader is answered at once
// only from an incarnation this process has converged). A REBUILD's is not:
// it is built beside the current one, which serves until the rebuild has
// converged its replacement (promote).
//
// Its skeleton is a bare repository git can run in: HEAD naming main (the
// round's refs step names the repository's own default branch before any
// reader sees it), config gitproc.RepoConfig, a packed-refs of the header
// alone, and the empty directories refs/heads, refs/tags, objects/pack and
// objects/info. The number is taken by os.Mkdir, never by a rename over a
// name: a directory already there under it — a purged repository's
// incarnation a reader still holds — keeps its name, and the next number is
// taken. A skeleton that cannot be written is moved into trash/.
func (c *Cache) newIncarnation(e *entry, current bool) (*incarnation, error) {
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
	if current {
		e.mu.Lock()
		if e.current == nil {
			c.heldRepos.Add(1)
		}
		e.current = inc
		e.mu.Unlock()
	}
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
