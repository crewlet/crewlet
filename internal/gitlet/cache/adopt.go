package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// Adoption links what this node wrote into the cache in place of its GET: a
// push's pack, a compaction's outputs, an expunge's packs (dropping ones by
// AdoptDropping) and rescue, a maintainer's layer (AdoptLayer); package doc,
// "A push never runs in a cache". Adoptions share an entry's write lock
// (group commit lands a repository's pushes together); a round and Verify's
// listing take it whole, since a full round sweeps and counts what it finds
// and a listing must see no step half done. An adoption finding it held or
// awaited never waits: it stages its files in tmp/ (outliving the scratch,
// removed once the push is answered) and registers them in entry.handed,
// which the holder installs before it lets go (releaseWrite). The try and
// the holder's last look both run under the entry's mu.

// writeExclusive is the weight a round and Verify's listing take of an
// entry's write lock, where an adoption takes one: the largest there is, so
// no count of adoptions in flight reaches it.
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

// staging is a pushed pack's three files linked into tmp/ as
// tmp/adopt-<uuidv7>.{pack,rev,idx}: no bytes of their own while the scratch
// stands, and they outlive it. Counted in tmp/'s bytes until dropped.
type staging struct {
	pack gitlet.PackRef
	stem string
	// dropping: an expunge's begin left the row dropping
	// ([Cache.AdoptDropping]); placed in the dropping view, never objects/pack.
	dropping bool
}

// Adopt hard-links live packs from sc (a push's, a compaction's outputs, an
// expunge's kept packs or rescue) into objects/pack of the repository's
// CURRENT incarnation, the next reader's, so the node that wrote them does not
// GET them back. Call it once their rows have applied, which it cannot check:
// a pack linked first is an object no row names, served by this node alone. A
// push calls it before answering, lest its client race it to a GET. A pack not
// sc's by checksum and every section's size (.rev too) refuses the call whole
// ([ErrInvalid]); nil with nothing linked is a cache already holding the
// packs, or none of the repository here. It takes one share of the write
// lock, never waits (it hands its packs to the writer), syncs once, counts
// each pack in crewlet.code.cache.adoptions and leaves the state file to the
// next round. An expunge's dropping pack is [Cache.AdoptDropping]'s.
func (c *Cache) Adopt(ctx context.Context, repoID string, sc *Scratch, packs ...gitlet.PackRef) error {
	return c.adopt(ctx, repoID, sc, false, packs)
}

// AdoptDropping is [Cache.Adopt], obligation included, for the packs an
// expunge's begin left DROPPING, built in its maintainer's scratch: called
// once the begin's record has applied, it spares that node up to
// internal/gitlet/compact's ExpungeInputByteCap of GETs a cycle for packs the
// commit retires minutes later. Each pack's files go into the incarnation's
// objects-dropping/pack (made durably where missing), never objects/pack, and
// its row is recorded dropping, where a round's install would place it
// (placeOf), so only a process serving the copy reads them and no push relies
// on one (packPlace). A pack whose checksum a live row already names stays in
// objects/pack with that row's files.
func (c *Cache) AdoptDropping(ctx context.Context, repoID string, sc *Scratch, packs ...gitlet.PackRef) error {
	return c.adopt(ctx, repoID, sc, true, packs)
}

// adopt is [Cache.Adopt] — and, dropping, [Cache.AdoptDropping] — of packs.
func (c *Cache) adopt(ctx context.Context, repoID string, sc *Scratch, dropping bool, packs []gitlet.PackRef) error {
	switch {
	case sc == nil || sc.c != c:
		return fmt.Errorf("%w: an adoption names a scratch this cache made", ErrInvalid)
	case sc.view:
		return fmt.Errorf("%w: %s is an advertisement view, which no push writes into", ErrInvalid, sc.Dir)
	case sc.e.id != repoID:
		return fmt.Errorf("%w: %s is a scratch of %s, not of %s", ErrInvalid, sc.Dir, sc.e.id, repoID)
	case len(packs) == 0:
		return fmt.Errorf("%w: an adoption names the packs it installs, and this one names none", ErrInvalid)
	}
	if !c.enter() {
		return ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cache: adopting into %s: %w", repoID, context.Cause(ctx))
	}
	if err := checkAdoption(sc, packs); err != nil {
		return err
	}
	counted := func(outcome adoptOutcome) {
		for range packs {
			c.adoptCounted(repoID, outcome)
		}
	}

	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		counted(adoptUncached)
		return nil
	}
	stagings := make([]*staging, 0, len(packs))
	for _, pack := range packs {
		st, err := c.stageAdoption(sc, pack, dropping)
		if err != nil {
			for _, st := range stagings {
				c.dropStaging(st)
			}
			counted(adoptFailed)
			return fmt.Errorf("cache: adopting pack %s into %s: %w", pack.PackID, repoID, err)
		}
		stagings = append(stagings, st)
	}

	e.mu.Lock()
	var inc *incarnation
	switch {
	case e.dead || e.current == nil:
		e.mu.Unlock()
		for _, st := range stagings {
			c.dropStaging(st)
		}
		counted(adoptUncached)
		return nil
	case e.write.TryAcquire(1):
		e.adopting++
		inc = e.current
		inc.pins++
	default:
		// A round or a listing holds the directory, or waits for it: it
		// installs the packs before it lets go. A staging of the same pack
		// already handed is the same bytes.
		var dups []*staging
		for _, st := range stagings {
			if e.handed[st.pack.GitChecksum] != nil {
				dups = append(dups, st)
				continue
			}
			if e.handed == nil {
				e.handed = map[string]*staging{}
			}
			e.handed[st.pack.GitChecksum] = st
		}
		e.mu.Unlock()
		for _, st := range dups {
			c.dropStaging(st)
		}
		for _, pack := range packs {
			c.log.Debug("code_cache_adopt_handed", "repo", repoID, "pack_id", pack.PackID)
		}
		counted(adoptHanded)
		return nil
	}
	e.mu.Unlock()
	defer c.releaseWrite(e, 1)
	if c.knobs != nil && c.knobs.adopting != nil {
		c.knobs.adopting(repoID)
	}
	outcomes, err := c.placeStagings(e, inc, stagings)
	e.mu.Lock()
	gone := c.unpin(e, inc)
	e.mu.Unlock()
	c.trash(gone)
	for _, outcome := range outcomes {
		c.adoptCounted(repoID, outcome)
	}
	if err != nil {
		return fmt.Errorf("cache: adopting %s into %s: %w", packIDs(packs), repoID, err)
	}
	return nil
}

// packIDs names packs for a line or an error: their ids, in order.
func packIDs(packs []gitlet.PackRef) string {
	ids := make([]string, len(packs))
	for i, p := range packs {
		ids[i] = p.PackID
	}
	return "pack " + strings.Join(ids, ", ")
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

// AdoptLayer hands the installer a published layer its own node, the
// repository's maintainer, built in sc, so it is placed with no GET. Like
// Adopt, call it only once the publication's record has applied: placed
// before its row, the layer is a chain no row names. row is the layer as the
// record names it; sc holds its files where git wrote them, over a base whose
// graph layers its commit-graph-chain lists, root first, before the layer's
// own. They are linked into tmp/layer-<id>-<v7>/, which outlives sc, held to
// row (gitlet.CheckLayer, hashed in the read lane) and registered for the
// next round or install over that base (crewlet.code.cache.layers{outcome=adopted});
// one no chain names within objstore.RecordWithin is discarded (expireStagedLayers).
// Files that are not row (gitlet.ErrBadLayer) or a foreign scratch are refused
// (ErrInvalid); any other failure costs the installer a GET, never the publication.
func (c *Cache) AdoptLayer(ctx context.Context, repoID string, sc *Scratch, row gitlet.LayerRef) error {
	switch {
	case sc == nil || sc.c != c:
		return fmt.Errorf("%w: a layer's adoption names a scratch this cache made", ErrInvalid)
	case sc.view:
		return fmt.Errorf("%w: %s is an advertisement view, which no publication writes into", ErrInvalid, sc.Dir)
	case sc.e.id != repoID:
		return fmt.Errorf("%w: %s is a scratch of %s, not of %s", ErrInvalid, sc.Dir, sc.e.id, repoID)
	}
	if err := row.Check(); err != nil {
		return fmt.Errorf("%w: adopting layer %s into %s: %w", ErrInvalid, row.LayerID, repoID, err)
	}
	if !c.enter() {
		return ErrStopped
	}
	defer c.leave()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("cache: adopting layer %s into %s: %w", row.LayerID, repoID, context.Cause(ctx))
	}
	base, err := scratchGraphBase(sc.Dir, row)
	if err != nil {
		return fmt.Errorf("cache: adopting layer %s into %s: %w", row.LayerID, repoID, err)
	}
	st, err := c.stageAdoptedLayer(sc, gitlet.LayerRow{Layer: row})
	if err != nil {
		return fmt.Errorf("cache: adopting layer %s into %s: %w", row.LayerID, repoID, err)
	}
	// The read lane: the publisher released its maintenance slot when its
	// build's last git step ended.
	if err = checkStaged(ctx, st, base, c.pieces(c.git.HoldRead).layer()); err != nil {
		st.remove(c)
		if errors.Is(err, gitlet.ErrBadLayer) {
			return fmt.Errorf("%w: adopting layer %s into %s: %w", ErrInvalid, row.LayerID, repoID, err)
		}
		// Says nothing of the files; the installer fetches the layer instead.
		return fmt.Errorf("cache: adopting layer %s into %s: %w", row.LayerID, repoID, err)
	}
	st.base = base
	in := c.installer
	in.mu.Lock()
	if in.staged[repoID] == nil {
		in.staged[repoID] = map[string]*stagedLayer{}
	}
	old := in.staged[repoID][row.LayerID]
	in.staged[repoID][row.LayerID] = st
	in.mu.Unlock()
	if old != nil {
		old.remove(c)
	}
	c.kickInstall(repoID, false)
	return nil
}

// scratchGraphBase is the commit-graph layers the scratch at dir wrote row's
// graph layer over, root first: its commit-graph-chain less the layer's own
// last line, or all of it where the layer carries no graph. A chain not
// ending in the layer's graph is refused: the files are no layer row names.
func scratchGraphBase(dir string, row gitlet.LayerRef) ([]string, error) {
	text, err := os.ReadFile(filepath.Join(dir, graphChainRel))
	if errors.Is(err, fs.ErrNotExist) {
		text, err = nil, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Fields(string(text))
	if row.GraphID == "" {
		return lines, nil
	}
	if len(lines) == 0 || lines[len(lines)-1] != row.GraphID {
		return nil, fmt.Errorf("%w: %s's commit-graph-chain does not end in the layer's graph-%s",
			ErrInvalid, dir, row.GraphID)
	}
	return lines[:len(lines)-1], nil
}

// stageAdoptedLayer links the scratch's files of row's layer into
// tmp/layer-<id>-<v7>/, as a fetch stages them (stageLayer), and counts their
// bytes into tmp/'s. A link that fails takes back the staging.
func (c *Cache) stageAdoptedLayer(sc *Scratch, row gitlet.LayerRow) (*stagedLayer, error) {
	dir := filepath.Join(c.dir, tmpDir, "layer-"+row.Layer.LayerID+"-"+newV7())
	if err := os.Mkdir(dir, hostbox.DirMode); err != nil {
		return nil, err
	}
	st := &stagedLayer{row: row, dir: dir, adopted: true, at: c.now(), began: time.Now()}
	c.tmpBytes.Add(row.Layer.Size())
	for i, rel := range layerRels(row.Layer) {
		if rel == "" {
			continue
		}
		st.paths[i] = filepath.Join(dir, []string{"midx", "bitmap", "graph"}[i])
		if err := os.Link(filepath.Join(sc.Dir, rel), st.paths[i]); err != nil {
			st.remove(c)
			return nil, fmt.Errorf("staging %s: %w", filepath.Join(sc.Dir, rel), err)
		}
	}
	return st, nil
}

// checkAdoption holds each of packs to one of the scratch's packs: its
// checksum, and each section's size. The scratch is listed once for them all.
func checkAdoption(sc *Scratch, packs []gitlet.PackRef) error {
	held, err := sc.Packs()
	if err != nil {
		return fmt.Errorf("%w: adopting %s: %w", ErrInvalid, packIDs(packs), err)
	}
	in := make(map[string]bool, len(held))
	for _, p := range held {
		in[p.Checksum] = true
	}
	for _, pack := range packs {
		if err := pack.Check(); err != nil {
			return fmt.Errorf("%w: adopting pack %s: %w", ErrInvalid, pack.PackID, err)
		}
		if !in[pack.GitChecksum] {
			return fmt.Errorf("%w: adopting pack %s: its row names pack-%s and %s holds no such pack",
				ErrInvalid, pack.PackID, pack.GitChecksum, sc.Dir)
		}
		stem := filepath.Join(sc.Dir, "objects", "pack", "pack-"+pack.GitChecksum)
		for _, section := range []struct {
			ext  string
			want int64
		}{{".pack", pack.PackBytes()}, {".idx", pack.IndexBytes}, {".rev", pack.ReverseBytes}} {
			info, err := os.Lstat(stem + section.ext)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				return fmt.Errorf("%w: adopting pack %s: %s has no %s", ErrInvalid, pack.PackID, sc.Dir,
					section.ext)
			case err != nil:
				return fmt.Errorf("cache: adopting pack %s: %w", pack.PackID, err)
			case !info.Mode().IsRegular() || info.Size() != section.want:
				return fmt.Errorf("%w: adopting pack %s: the scratch's %s is %d bytes and its row says %d",
					ErrInvalid, pack.PackID, section.ext, info.Size(), section.want)
			}
		}
	}
	return nil
}

// stageAdoption links the scratch's pack files into tmp/ under second names,
// and counts them as tmp/'s until they are dropped. A link that fails takes
// back the ones before it.
func (c *Cache) stageAdoption(sc *Scratch, pack gitlet.PackRef, dropping bool) (*staging, error) {
	st := &staging{pack: pack, stem: filepath.Join(c.dir, tmpDir, "adopt-"+newV7()), dropping: dropping}
	from := filepath.Join(sc.Dir, "objects", "pack", "pack-"+pack.GitChecksum)
	for i, ext := range bundleOrder {
		if err := os.Link(from+ext, st.stem+ext); err != nil {
			return nil, errors.Join(fmt.Errorf("staging %s: %w", from+ext, err), removeAll(st.stem, bundleOrder[:i]))
		}
	}
	c.tmpBytes.Add(pack.Size)
	return st, nil
}

// dropStaging removes a staging's names from tmp/ (its bytes stay with the
// scratch's or an incarnation's names) and takes it out of tmp/'s count. A
// failed removal is logged, and after Stop nothing is removed: the next
// Open empties tmp/.
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

// linkBundle links a pack's three files from stem into packDir as
// pack-<cs>.* in bundleOrder through link (os.Link where nil; a swap passes
// Cache.link, which a case can make fail), calling placed after each, and
// reports whether it linked them. A name already there (two pushes of the
// same content share a pack, or a failed round's install left it) is the
// pack HELD: what this call linked is taken back, nothing else touched. Any
// other failure is taken back and returned. The caller syncs the directory.
func linkBundle(link func(oldname, newname string) error, stem, packDir, checksum string,
	placed func(ext string)) (bool, error) {
	if link == nil {
		link = os.Link
	}
	to := filepath.Join(packDir, "pack-"+checksum)
	for i, ext := range bundleOrder {
		err := link(stem+ext, to+ext)
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

// liveAdopted records st's pack as a live row of inc; e.mu is held. Where its
// checksum was in the DROPPING VIEW (a rescue byte for byte the dropping pack
// it was cut from, as a commit's rescue of a pack a ref reaches whole is), its
// view files are marked LEAVING for the next round's swap, as applyDelta
// marks a delta's move: no later delta moves it, so unmarked they would stay
// beside their live copy until a full round swept them.
func liveAdopted(inc *incarnation, st *staging) {
	cs := st.pack.GitChecksum
	was := placeOf(inc, cs)
	setLive(inc, st.pack.PackID, liveOf(st.pack, gitlet.PackLive))
	if was != placeDropping || placeOf(inc, cs) != placeLive {
		return
	}
	if inc.leaving == nil {
		inc.leaving = map[leaveKey]Event{}
	}
	inc.leaving[leaveKey{placeDropping, cs}] = EventPackRetired
}

// adopted records st's pack as a row of inc's live set: live (liveAdopted),
// or for an expunge's dropping pack ([Cache.AdoptDropping]) the dropping row
// its begin wrote. e.mu is held.
func adopted(inc *incarnation, st *staging) {
	if st.dropping {
		setLive(inc, st.pack.PackID, liveOf(st.pack, gitlet.PackDropping))
		return
	}
	liveAdopted(inc, st)
}

// adoptedPlace is where inc keeps st's pack's files once its row is recorded
// (placeOf): objects/pack, or the dropping view for a dropping row whose
// checksum no live row names. e.mu is held.
func adoptedPlace(inc *incarnation, st *staging) packPlace {
	if st.dropping && placeOf(inc, st.pack.GitChecksum) != placeLive {
		return placeDropping
	}
	return placeLive
}

// placeStagings installs stagings into inc (write lock held, inc pinned) and
// drops them whatever happens: each pack's files are linked where its row
// places them (adoptedPlace; the dropping view made first where missing)
// unless already there, and counted, and each directory is synced ONCE. It
// answers each staging's outcome in order, every failure joined.
//
// A FAILED DIRECTORY SYNC LEAVES THE PACKS INSTALLED AND COUNTED, and is
// returned, as a round's install does: readers see the names, only their
// surviving a crash is lost, and a boot's first round is full and installs
// what a crash lost. Taking the links back would cost a GET to repair nothing.
func (c *Cache) placeStagings(e *entry, inc *incarnation, stagings []*staging) ([]adoptOutcome, error) {
	defer func() {
		for _, st := range stagings {
			c.dropStaging(st)
		}
	}()
	// The row enters the incarnation whether the pack is linked or held: it
	// has applied, and a compaction retiring it takes it out by this id
	// (swap.go's live set).
	held := func(st *staging) {
		e.mu.Lock()
		adopted(inc, st)
		e.mu.Unlock()
	}
	places := make([]packPlace, len(stagings))
	e.mu.Lock()
	for i, st := range stagings {
		places[i] = adoptedPlace(inc, st)
	}
	e.mu.Unlock()
	outcomes := make([]adoptOutcome, len(stagings))
	var (
		errs   []error
		toLink []int
		view   bool
	)
	for i, st := range stagings {
		state, err := placedAlready(filepath.Join(inc.dir, places[i].rel()), st.pack.GitChecksum)
		switch {
		case err != nil:
			outcomes[i] = adoptFailed
			errs = append(errs, fmt.Errorf("pack %s: %w", st.pack.PackID, err))
		case state:
			// Installed, or begun by a failed round, which the next finishes.
			held(st)
			outcomes[i] = adoptHeld
		default:
			toLink = append(toLink, i)
			view = view || places[i] == placeDropping
		}
	}
	if len(toLink) == 0 {
		return outcomes, errors.Join(errs...)
	}
	// Once for the call, before any pack is linked (dropFor's order rule).
	if _, err := c.dropFor(e, inc, EventPackAdded, eventDetail{}); err != nil {
		for _, i := range toLink {
			outcomes[i] = adoptFailed
		}
		return outcomes, errors.Join(append(errs, err)...)
	}
	// The dropping view is made durable before any pack is placed in it.
	var viewErr error
	if view {
		viewErr = c.ensureView(inc.dir)
	}
	var (
		linked   []int
		placedIn [placeDropping + 1]bool
	)
	for _, i := range toLink {
		st, place := stagings[i], places[i]
		if place == placeDropping && viewErr != nil {
			outcomes[i] = adoptFailed
			errs = append(errs, fmt.Errorf("pack %s: %w", st.pack.PackID, viewErr))
			continue
		}
		ok, err := linkBundle(nil, st.stem, filepath.Join(inc.dir, place.rel()), st.pack.GitChecksum, nil)
		switch {
		case err != nil:
			outcomes[i] = adoptFailed
			errs = append(errs, fmt.Errorf("pack %s: %w", st.pack.PackID, err))
		case !ok:
			held(st)
			outcomes[i] = adoptHeld
		default:
			c.vouchPack(e, inc, place, st.pack.GitChecksum)
			placedIn[place] = true
			linked = append(linked, i)
		}
	}
	if len(linked) == 0 {
		return outcomes, errors.Join(errs...)
	}

	now := c.now()
	e.mu.Lock()
	addedLive := false
	for _, i := range linked {
		st := stagings[i]
		adopted(inc, st)
		c.counted(inc, places[i], st.pack.Objects, true, st.pack.Size)
		addedLive = addedLive || places[i] == placeLive
		outcomes[i] = adoptAdopted
	}
	inc.unmoved = false
	// No repair (incarnation.repairWrite): an adopted pack leaves whatever
	// failed a request as it was. The maintainer's debounce and ceiling are
	// set as a round adding a pack sets them, for objects/pack only (the one
	// directory the index covers), and with no kick: the pack is due once the
	// debounce passes, on the maintainer's own tick.
	if addedLive {
		e.lastAdded = now
		if e.uncoveredSince.IsZero() {
			e.uncoveredSince = now
		}
	}
	e.mu.Unlock()
	if c.overBudget() > 0 {
		c.kick()
	}
	c.setGauges()
	sync := syncDir
	if c.knobs != nil && c.knobs.adoptSync != nil {
		sync = c.knobs.adoptSync
	}
	for _, place := range []packPlace{placeLive, placeDropping} {
		if !placedIn[place] {
			continue
		}
		if err := sync(filepath.Join(inc.dir, place.rel())); err != nil {
			errs = append(errs, fmt.Errorf("the packs are installed, and their directory is not yet durable: %w", err))
		}
	}
	return outcomes, errors.Join(errs...)
}

// placedAlready reports whether packDir holds any of the files of the pack
// named checksum — installed, or begun by an install a failed round left —
// or an error that is no file's absence.
func placedAlready(packDir, checksum string) (bool, error) {
	for _, ext := range bundleOrder {
		_, err := os.Lstat(filepath.Join(packDir, "pack-"+checksum+ext))
		switch {
		case err == nil:
			return true, nil
		case !errors.Is(err, fs.ErrNotExist):
			return false, err
		}
	}
	return false, nil
}

// claimHanded takes the staging handed to e for p, if one was and it is p's —
// its sizes the row's — for a round about to download p: three links instead
// of a GET. A staging that is not p's is dropped and the pack downloaded.
func (c *Cache) claimHanded(e *entry, p gitlet.PackRow) *staging {
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

// releaseWrite lets go of n of e's write lock (a round's, a listing's or an
// adoption's) after installing every staging handed to e meanwhile into its
// current incarnation (placeStagings). Taking the registrations and the
// release both run under e.mu, so an adoption whose try fails under it has
// registered before this last look, or finds the lock free. A staging with no
// incarnation left (purged or evicted) is dropped, and after Stop left to the
// next boot's tmp/. The last adoption to leave (n one) wakes the maintainer
// for a pass it held back (owedWake).
func (c *Cache) releaseWrite(e *entry, n int64) {
	for {
		e.mu.Lock()
		handed := e.handed
		e.handed = nil
		if len(handed) == 0 {
			e.write.Release(n)
			wake := false
			if n != writeExclusive {
				e.adopting--
				wake = c.owedWake(e)
			}
			e.mu.Unlock()
			if wake {
				c.kickMaintainer()
			}
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
		stagings := make([]*staging, 0, len(handed))
		for _, st := range handed {
			stagings = append(stagings, st)
		}
		if inc == nil || !c.enter() {
			for _, st := range stagings {
				c.dropStaging(st)
			}
		} else {
			if _, err := c.placeStagings(e, inc, stagings); err != nil {
				c.log.Warn("code_cache_adopt_failed", "repo", e.id, "error", err)
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
	if settle {
		e.adopting++
	}
	e.mu.Unlock()
	if settle {
		c.releaseWrite(e, 1)
	}
	return err
}
