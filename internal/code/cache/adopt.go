package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE RECEIVING NODE'S OWN PACK. A push's receive-pack wrote its pack, its
// .idx and its .rev into the push's scratch — the bundle the host stored — so
// once the push's row has applied, the node that took the push links those
// files into the cache rather than download them back: Adopt.
//
// AN ADOPTION NEVER WAITS AND IS NEVER LOST. What it must not do is write
// beside a ROUND — a full one sweeps what its snapshot does not name and
// counts what it finds, so a pack linked in beside it could be swept as a
// stray or counted twice — nor beside Verify's listing, which must see no
// step half done. So the entry's write lock is SHARED by adoptions, each
// linking its own pack's files, and taken WHOLE (writeExclusive) by a round
// and by a listing: adoptions never exclude one another — group commit
// answers up to MaxPushesPerRecord pushes of one repository with one record,
// so they arrive together — and an adoption that finds the lock held whole,
// or a round or a listing waiting for it, does not wait: it HANDS its pack to
// that holder. The pack's files are staged in tmp/ under second names first —
// they must outlive the scratch, which the host removes as soon as the push
// is answered — and registered on the entry by checksum; the round that is
// writing installs a pack it is about to download from the staging instead
// (three links, no GET), and whoever holds the lock installs every staging
// still registered before it lets the lock go (releaseWrite). So the node
// that took a push downloads it only when a round that read the push's row
// had already begun that pack's GET before the adoption handed it the
// staging — a round learns of the pack from the row, so the window runs from
// the row's apply on this node to the adoption's registration, and the
// staging is then found held and dropped — or when the links fail; never
// because a busy repository always has a round in flight, which is when an
// adoption saves the most.
//
// Which of the two happens is decided under the entry's mu, in one step with
// the registration (a try of the write lock, never a wait), and a holder
// takes the registrations and lets the lock go under the same mu: no staging
// is registered after its holder's last look.

// writeExclusive is the weight a round and Verify's listing take of an
// entry's write lock — all of it — where an adoption takes one: any number of
// adoptions hold it at once, and nothing holds it beside a round or a
// listing. The largest weight there is, so no count of adoptions in flight
// can reach it.
const writeExclusive = math.MaxInt64

// adoptOutcome is what became of one adoption: the outcome attribute of
// crewlet.code.cache.adoptions.
type adoptOutcome string

const (
	// adoptAdopted: the adoption linked the pack in itself.
	adoptAdopted adoptOutcome = "adopted"
	// adoptHanded: a round or a listing held the directory, and the pack was
	// staged for it to install before it lets go.
	adoptHanded adoptOutcome = "handed"
	// adoptHeld: the cache already held the pack, or one of its files.
	adoptHeld adoptOutcome = "held"
	// adoptUncached: the repository's cache was gone from the node.
	adoptUncached adoptOutcome = "uncached"
	// adoptFailed: a link failed; the next sync downloads the pack.
	adoptFailed adoptOutcome = "failed"
)

// bundleOrder is the order a pack's files are placed in: the .pack, then the
// .rev, then the .idx LAST — git counts a pack present once its .idx exists.
var bundleOrder = []string{".pack", ".rev", ".idx"}

// staging is a pushed pack's three files under second names in tmp/,
// tmp/adopt-<uuidv7>.{pack,rev,idx}: links of the scratch's own files, so
// they hold no bytes of their own while the scratch stands and outlive it
// when it goes. Counted in the cache's tmp/ bytes until dropped.
type staging struct {
	pack code.PackRef
	stem string
}

// Adopt installs a push's pack from its scratch into the repository's
// CURRENT incarnation by hard link, so the node that took a push does not
// download it back — unless a round that read the push's row had already
// begun that pack's GET before the adoption handed it the staging, or a link
// fails (see above): called by the host once the push's row has applied, and
// before it tells the client ok — a client fetching from this node at once
// would otherwise race the adoption and download its own push.
//
// The row having applied is the one thing Adopt cannot check, and the
// caller's whole obligation: a pack linked in before its row would be an
// object no row names, fetchable by id from this node alone until the next
// full round swept it.
//
// pack is the row's: its checksum must be the scratch's one pack's, and each
// section's size the scratch's file's (a .rev missing included) — the files
// linked ARE the bundle the host uploaded and sealed, so the sizes are the
// whole check; a disagreement would install bytes no row names, and is
// refused ([ErrInvalid]), nothing linked.
//
// It NEVER WAITS: adoptions share the directory, and one that meets a round
// or a listing writing it hands the pack to that writer, which installs it
// before it lets the directory go (see above). Otherwise a nil error with
// nothing linked is a cache that already holds the pack, or no cache of the
// repository on this node. Every adoption is counted under its outcome
// (crewlet.code.cache.adoptions).
//
// The CURRENT incarnation, not the scratch's: a rebuild between the push's
// sync and its adoption leaves the pack's row applied either way, and the
// current incarnation is the one the next reader is handed. Its state is not
// written — the next round that moves its refs writes it, and a boot's first
// round recounts the directory.
func (c *Cache) Adopt(ctx context.Context, repoID string, sc *Scratch, pack code.PackRef) error {
	switch {
	case sc == nil || sc.c != c:
		return fmt.Errorf("%w: an adoption names a scratch this cache made", ErrInvalid)
	case sc.view:
		return fmt.Errorf("%w: %s is an advertisement view, which no push writes into", ErrInvalid, sc.Dir)
	case sc.e.id != repoID:
		return fmt.Errorf("%w: %s is a scratch of %s, not of %s", ErrInvalid, sc.Dir, sc.e.id, repoID)
	}
	if !c.enter() {
		return ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cache: adopting into %s: %w", repoID, context.Cause(ctx))
	}
	if err := checkAdoption(sc, pack); err != nil {
		return err
	}

	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		c.adoptCounted(repoID, adoptUncached)
		return nil
	}
	st, err := c.stageAdoption(sc, pack)
	if err != nil {
		c.adoptCounted(repoID, adoptFailed)
		return fmt.Errorf("cache: adopting pack %s into %s: %w", pack.PackID, repoID, err)
	}

	e.mu.Lock()
	var inc *incarnation
	switch {
	case e.dead || e.current == nil:
		e.mu.Unlock()
		c.dropStaging(st)
		c.adoptCounted(repoID, adoptUncached)
		return nil
	case e.write.TryAcquire(1):
		inc = e.current
		inc.pins++
	default:
		// A round or a listing holds the directory, or waits for it: it
		// installs the pack before it lets go. A staging of the same pack
		// already handed is the same bytes.
		dup := e.handed[pack.GitChecksum] != nil
		if !dup {
			if e.handed == nil {
				e.handed = map[string]*staging{}
			}
			e.handed[pack.GitChecksum] = st
		}
		e.mu.Unlock()
		if dup {
			c.dropStaging(st)
		}
		c.log.Debug("code_cache_adopt_handed", "repo", repoID, "pack_id", pack.PackID)
		c.adoptCounted(repoID, adoptHanded)
		return nil
	}
	e.mu.Unlock()
	defer c.releaseWrite(e, 1)
	if c.knobs != nil && c.knobs.adopting != nil {
		c.knobs.adopting(repoID)
	}
	outcome, err := c.placeStaging(e, inc, st)
	e.mu.Lock()
	gone := c.unpin(e, inc)
	e.mu.Unlock()
	c.trash(gone)
	c.adoptCounted(repoID, outcome)
	if err != nil {
		return fmt.Errorf("cache: adopting pack %s into %s: %w", pack.PackID, repoID, err)
	}
	return nil
}

// adoptCounted records one adoption's outcome: counted, and — when nothing
// was linked — logged at debug, since the next sync then pays for the pack.
func (c *Cache) adoptCounted(repoID string, outcome adoptOutcome) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheAdoptions, 1, metrics.Attrs{"outcome": string(outcome)})
	}
	switch outcome {
	case adoptHeld:
		c.log.Debug("code_cache_adopt_skipped", "repo", repoID, "reason", "the cache holds the pack")
	case adoptUncached:
		c.log.Debug("code_cache_adopt_skipped", "repo", repoID, "reason", "the repository has no cache on this node")
	}
}

// checkAdoption holds the scratch's one pack to the row's: its checksum, and
// each section's size.
func checkAdoption(sc *Scratch, pack code.PackRef) error {
	if err := pack.Check(); err != nil {
		return fmt.Errorf("%w: adopting pack %s: %w", ErrInvalid, pack.PackID, err)
	}
	_, cs, ok, err := sc.Pack()
	switch {
	case err != nil:
		return fmt.Errorf("%w: adopting pack %s: %w", ErrInvalid, pack.PackID, err)
	case !ok:
		return fmt.Errorf("%w: adopting pack %s: %s holds no pack", ErrInvalid, pack.PackID, sc.Dir)
	case cs != pack.GitChecksum:
		return fmt.Errorf("%w: adopting pack %s: its row names pack-%s and %s holds pack-%s",
			ErrInvalid, pack.PackID, pack.GitChecksum, sc.Dir, cs)
	}
	stem := filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs)
	for _, section := range []struct {
		ext  string
		want int64
	}{{".pack", pack.PackBytes()}, {".idx", pack.IndexBytes}, {".rev", pack.ReverseBytes}} {
		info, err := os.Lstat(stem + section.ext)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return fmt.Errorf("%w: adopting pack %s: %s has no %s", ErrInvalid, pack.PackID, sc.Dir, section.ext)
		case err != nil:
			return fmt.Errorf("cache: adopting pack %s: %w", pack.PackID, err)
		case !info.Mode().IsRegular() || info.Size() != section.want:
			return fmt.Errorf("%w: adopting pack %s: the scratch's %s is %d bytes and its row says %d",
				ErrInvalid, pack.PackID, section.ext, info.Size(), section.want)
		}
	}
	return nil
}

// stageAdoption links the scratch's pack files into tmp/ under second names,
// and counts them as tmp/'s until they are dropped. A link that fails takes
// back the ones before it.
func (c *Cache) stageAdoption(sc *Scratch, pack code.PackRef) (*staging, error) {
	st := &staging{pack: pack, stem: filepath.Join(c.dir, tmpDir, "adopt-"+newV7())}
	from := filepath.Join(sc.Dir, "objects", "pack", "pack-"+pack.GitChecksum)
	for i, ext := range bundleOrder {
		if err := os.Link(from+ext, st.stem+ext); err != nil {
			return nil, errors.Join(fmt.Errorf("staging %s: %w", from+ext, err), removeAll(st.stem, bundleOrder[:i]))
		}
	}
	c.tmpBytes.Add(pack.Size)
	return st, nil
}

// dropStaging removes a staging's names from tmp/ — never its bytes, which
// the scratch's names or an incarnation's still hold — and takes it out of
// tmp/'s count. A removal that fails is logged: a boot empties tmp/. Once
// Stop has begun it removes nothing, since tmp/ is the next Open's to empty.
func (c *Cache) dropStaging(st *staging) {
	c.tmpBytes.Add(-st.pack.Size)
	if !c.enter() {
		return
	}
	defer c.leave()
	if err := removeAll(st.stem, bundleOrder); err != nil {
		c.log.Warn("code_cache_trash_failed", "path", st.stem, "error", err)
	}
}

// removeAll removes stem+ext for each ext, a name already gone being no
// failure.
func removeAll(stem string, exts []string) error {
	var errs []error
	for _, ext := range exts {
		if err := os.Remove(stem + ext); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// linkBundle links a pack's three files from stem into the incarnation's
// objects/pack as pack-<cs>.*, in bundleOrder, calling placed after each, and
// reports whether it linked them. A name already there — a file of the same
// pack, which two pushes of the same content share, or one an install a
// failed round left — is the pack HELD: whatever this call linked before it
// is taken back, and nothing else is touched. Any other failure takes back
// what was linked and is returned. The directory is not synced: its caller
// makes the step durable.
func linkBundle(stem, packDir, checksum string, placed func(ext string)) (bool, error) {
	to := filepath.Join(packDir, "pack-"+checksum)
	for i, ext := range bundleOrder {
		err := os.Link(stem+ext, to+ext)
		if err == nil {
			if placed != nil {
				placed(ext)
			}
			continue
		}
		undo := removeAll(to, bundleOrder[:i])
		if errors.Is(err, fs.ErrExist) {
			return false, undo
		}
		return false, errors.Join(fmt.Errorf("linking %s: %w", to+ext, err), undo)
	}
	return true, nil
}

// placeStaging installs a staging into inc — the write lock held, inc pinned
// — and drops it, whatever the outcome: the pack's files linked into
// objects/pack (unless the incarnation holds one of them already), counted,
// and the directory synced.
//
// A DIRECTORY SYNC THAT FAILS LEAVES THE PACK INSTALLED AND COUNTED, and is
// returned, exactly as a round's install answers one: the names are there and
// every reader of the directory sees them; what the failed sync costs is their
// surviving a crash, and a boot's first round is a full one, which installs
// whatever a crash lost. Taking the links back would cost the next sync a GET
// to repair nothing.
func (c *Cache) placeStaging(e *entry, inc *incarnation, st *staging) (adoptOutcome, error) {
	defer c.dropStaging(st)
	packDir := filepath.Join(inc.dir, "objects", "pack")
	for _, ext := range bundleOrder {
		_, err := os.Lstat(filepath.Join(packDir, "pack-"+st.pack.GitChecksum+ext))
		switch {
		case err == nil:
			// Installed — or begun by an install a failed round left,
			// which the next round finishes.
			return adoptHeld, nil
		case !errors.Is(err, fs.ErrNotExist):
			return adoptFailed, err
		}
	}
	linked, err := linkBundle(st.stem, packDir, st.pack.GitChecksum, nil)
	switch {
	case err != nil:
		return adoptFailed, err
	case !linked:
		return adoptHeld, nil
	}

	c.vouchPack(e, inc, st.pack.GitChecksum)
	now := c.now()
	e.mu.Lock()
	inc.packs++
	inc.objects += st.pack.Objects
	c.setSize(inc, inc.diskBytes+st.pack.Size, inc.metaBytes)
	inc.unmoved = false
	// No repair (incarnation.repairWrite): a push's pack adds what no
	// failed request needed, and leaves whatever failed it as it was.
	// The maintainer's debounce and ceiling, exactly as a round that adds a
	// pack sets them; and like such a round's, no kick: the pack is due only
	// once the debounce has passed, which is the maintainer's own tick.
	e.lastAdded = now
	if e.uncoveredSince.IsZero() {
		e.uncoveredSince = now
	}
	e.mu.Unlock()
	if c.total() > c.maxBytes {
		c.kick()
	}
	c.setGauges()
	sync := syncDir
	if c.knobs != nil && c.knobs.adoptSync != nil {
		sync = c.knobs.adoptSync
	}
	if err := sync(packDir); err != nil {
		return adoptAdopted, fmt.Errorf("the pack is installed, and its directory is not yet durable: %w", err)
	}
	return adoptAdopted, nil
}

// claimHanded takes the staging handed to e for p, if one was and it is p's —
// its sizes the row's — for a round about to download p: three links instead
// of a GET. A staging that is not p's is dropped and the pack downloaded.
func (c *Cache) claimHanded(e *entry, p code.PackRow) *staging {
	e.mu.Lock()
	st := e.handed[p.GitChecksum]
	delete(e.handed, p.GitChecksum)
	e.mu.Unlock()
	if st == nil {
		return nil
	}
	ref := p.Ref()
	if st.pack.Size != ref.Size || st.pack.IndexBytes != ref.IndexBytes || st.pack.ReverseBytes != ref.ReverseBytes {
		c.dropStaging(st)
		return nil
	}
	return st
}

// releaseWrite lets go of n of e's write lock — taken by a round, a listing
// or an adoption — after installing every staging handed to e meanwhile into
// its current incarnation (placeStaging). The registrations are taken, and
// the lock let go, under e.mu: an adoption that fails to try the lock under
// the same mu registers before this last look, or finds the lock free.
// Taken back after a release, a staging is dropped where nothing is left to
// install it into — the repository purged or evicted — and, once Stop has
// begun, left to the next boot's tmp/.
func (c *Cache) releaseWrite(e *entry, n int64) {
	for {
		e.mu.Lock()
		handed := e.handed
		e.handed = nil
		if len(handed) == 0 {
			e.write.Release(n)
			e.mu.Unlock()
			return
		}
		inc := e.current
		if e.dead {
			inc = nil
		}
		if inc != nil {
			inc.pins++
		}
		e.mu.Unlock()
		for _, st := range handed {
			if inc == nil || !c.enter() {
				c.dropStaging(st)
				continue
			}
			if _, err := c.placeStaging(e, inc, st); err != nil {
				c.log.Warn("code_cache_adopt_failed", "repo", e.id, "pack_id", st.pack.PackID, "error", err)
			}
			c.leave()
		}
		if inc != nil {
			e.mu.Lock()
			gone := c.unpin(e, inc)
			e.mu.Unlock()
			c.trash(gone)
		}
	}
}

// acquireWrite takes all of e's write lock — a round's or a listing's —
// waiting under ctx. A wait that fails may have been what an adoption
// handed its pack to; if nothing else holds the lock to install it, this
// does, as an adoption would.
func (c *Cache) acquireWrite(ctx context.Context, e *entry) error {
	err := e.write.Acquire(ctx, writeExclusive)
	if err == nil {
		return nil
	}
	e.mu.Lock()
	settle := len(e.handed) > 0 && e.write.TryAcquire(1)
	e.mu.Unlock()
	if settle {
		c.releaseWrite(e, 1)
	}
	return err
}
