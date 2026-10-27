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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE RECEIVING NODE'S OWN PACK. A push's receive-pack wrote its pack, its
// .idx and its .rev into the push's scratch — the bundle the host stored — so
// once the push's row has applied, the node that took the push links those
// files into the cache rather than download them back: Adopt. A compaction's
// outputs are adopted the same way by the git node that built them
// (internal/code/compact), every output of one scratch in one call: what the
// one node that built them would otherwise download back, a GET an output.
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

// Adopt installs packs from a scratch into the repository's CURRENT
// incarnation by hard link — a push's one pack, or every output of a
// compaction — so the node that wrote them does not download them back —
// unless a round that read their rows had already begun a pack's GET before
// the adoption handed it the staging, or a link fails (see above): called by
// the host once the push's row has applied, and before it tells the client ok
// — a client fetching from this node at once would otherwise race the
// adoption and download its own push — and by the compactor once its
// compaction's record has applied.
//
// The rows having applied is the one thing Adopt cannot check, and the
// caller's whole obligation: a pack linked in before its row would be an
// object no row names, fetchable by id from this node alone until the next
// full round swept it.
//
// Each of packs is its row's: its checksum must be one of the scratch's
// packs', and each section's size that pack's file's (a .rev missing
// included) — the files linked ARE the bundle the caller uploaded and sealed,
// so the sizes are the whole check; a disagreement would install bytes no row
// names, and refuses the whole call ([ErrInvalid]), nothing linked.
//
// ONE CALL, ONE ADOPTION, however many packs: one share of the directory's
// write lock (an adoption's weight, never writeExclusive — adoptions never
// exclude one another), one dropFor of a pack added, one sync of the
// directory, and every pack counted under its own outcome. It NEVER WAITS:
// adoptions share the directory, and one that meets a round or a listing
// writing it hands every pack to that writer, which installs them before it
// lets the directory go (see above). Otherwise a nil error with nothing
// linked is a cache that already holds the packs, or no cache of the
// repository on this node. Every pack is counted under its outcome
// (crewlet.code.cache.adoptions).
//
// The CURRENT incarnation, not the scratch's: a rebuild — or a swap — between
// the caller's sync and its adoption leaves the rows applied either way, and
// the current incarnation is the one the next reader is handed. Its state is
// not written — the next round that moves its refs writes it, and a boot's
// first round recounts the directory.
func (c *Cache) Adopt(ctx context.Context, repoID string, sc *Scratch, packs ...code.PackRef) error {
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
		st, err := c.stageAdoption(sc, pack)
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
func packIDs(packs []code.PackRef) string {
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

// AdoptLayer hands the installer a published layer its own node wrote — the
// repository's MAINTAINER, which built it in sc — so that node installs it
// with no GET: called once the publication's record has applied, as Adopt is
// for a pack, and for the same reason only then (a layer placed before its
// row would be a chain no row names). row is the layer as the record names it.
//
// sc holds the layer's files under the names git gives them — its
// multi-pack-index layer and bitmap in objects/pack/multi-pack-index.d, and
// its commit-graph layer, where it carries one, in objects/info/commit-graphs
// — and its commit-graph-chain lists the base's graph layers, root first, and
// then the layer's own: what git leaves where it wrote a split graph over a
// named base. They are hard-linked into tmp/layer-<id>-<v7>/, which outlives
// the scratch, held to row over that base (code.CheckLayer: the rule every
// install meets, hashed a piece at a time in a slot of the read lane), and
// registered for the installer: the round or the installer that next reaches
// the layer over that very base places them in place of a GET
// (crewlet.code.cache.layers{outcome=adopted}). One no round's chain names
// within objstore.RecordWithin is a function of a record that never applied,
// and the janitor discards it (expireStagedLayers).
//
// A row the files are not (code.ErrBadLayer), or a scratch of another cache
// or repository, is refused (ErrInvalid) with nothing staged; a link or a
// read that fails, no read slot in time and an ended ctx are returned as
// they came, and cost the installer a GET, never the publication.
func (c *Cache) AdoptLayer(ctx context.Context, repoID string, sc *Scratch, row code.LayerRef) error {
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
	st, err := c.stageAdoptedLayer(sc, code.LayerRow{Layer: row})
	if err != nil {
		return fmt.Errorf("cache: adopting layer %s into %s: %w", row.LayerID, repoID, err)
	}
	// Hashed in the read lane a piece at a time: the publisher holds no
	// maintenance slot by now, having released its admission once its build's
	// last git step ended.
	if err = checkStaged(ctx, st, base, c.pieces(c.git.HoldRead).layer()); err != nil {
		st.remove(c)
		if errors.Is(err, code.ErrBadLayer) {
			return fmt.Errorf("%w: adopting layer %s into %s: %w", ErrInvalid, row.LayerID, repoID, err)
		}
		// A read that failed, no read slot in time or ctx ended: nothing
		// said of the files, whose layer the installer fetches instead.
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
// graph layer over, root first: its commit-graph-chain's lines, less the
// layer's own last line — or all of them where the layer carries no graph.
// A chain that does not end in the layer's own graph layer is refused: the
// files are no layer the row names.
func scratchGraphBase(dir string, row code.LayerRef) ([]string, error) {
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
func (c *Cache) stageAdoptedLayer(sc *Scratch, row code.LayerRow) (*stagedLayer, error) {
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
func checkAdoption(sc *Scratch, packs []code.PackRef) error {
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
// objects/pack as pack-<cs>.*, through link — os.Link where it is nil; a
// swap's link, which a case can make fail (Cache.link) — in bundleOrder,
// calling placed after each, and reports whether it linked them. A name
// already there — a file of the same pack, which two pushes of the same
// content share, or one an install a failed round left — is the pack HELD:
// whatever this call linked before it is taken back, and nothing else is
// touched. Any other failure takes back what was linked and is returned. The
// directory is not synced: its caller makes the step durable.
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

// placeStagings installs stagings into inc — the write lock held, inc pinned
// — and drops them, whatever the outcome: each pack's files linked into
// objects/pack (unless the incarnation holds one of them already) and
// counted, then the directory synced ONCE for them all; and answers each
// staging's outcome, in order, with every failure joined.
//
// A DIRECTORY SYNC THAT FAILS LEAVES THE PACKS INSTALLED AND COUNTED, and is
// returned, exactly as a round's install answers one: the names are there and
// every reader of the directory sees them; what the failed sync costs is their
// surviving a crash, and a boot's first round is a full one, which installs
// whatever a crash lost. Taking the links back would cost the next sync a GET
// a pack to repair nothing.
func (c *Cache) placeStagings(e *entry, inc *incarnation, stagings []*staging) ([]adoptOutcome, error) {
	defer func() {
		for _, st := range stagings {
			c.dropStaging(st)
		}
	}()
	packDir := filepath.Join(inc.dir, "objects", "pack")
	// THE ROW IS LIVE in the incarnation once this ends, whatever it links:
	// its row has applied, and the pack is either linked here or held
	// already (swap.go's live set — a compaction's retirement of it takes it
	// out by this id, before any delta has served it).
	held := func(st *staging) {
		e.mu.Lock()
		setLive(inc, st.pack.PackID, liveOf(st.pack))
		e.mu.Unlock()
	}
	outcomes := make([]adoptOutcome, len(stagings))
	var (
		errs   []error
		toLink []int
	)
	for i, st := range stagings {
		state, err := placedAlready(packDir, st.pack.GitChecksum)
		switch {
		case err != nil:
			outcomes[i] = adoptFailed
			errs = append(errs, fmt.Errorf("pack %s: %w", st.pack.PackID, err))
		case state:
			// Installed — or begun by an install a failed round left, which
			// the next round finishes.
			held(st)
			outcomes[i] = adoptHeld
		default:
			toLink = append(toLink, i)
		}
	}
	if len(toLink) == 0 {
		return outcomes, errors.Join(errs...)
	}
	// Packs added: what a declaration says of one, once for the call and
	// before any is linked in (dropFor's order rule). No declaration drops
	// for an added pack today.
	if _, err := c.dropFor(e, inc, EventPackAdded, eventDetail{}); err != nil {
		for _, i := range toLink {
			outcomes[i] = adoptFailed
		}
		return outcomes, errors.Join(append(errs, err)...)
	}
	var linked []int
	for _, i := range toLink {
		st := stagings[i]
		ok, err := linkBundle(nil, st.stem, packDir, st.pack.GitChecksum, nil)
		switch {
		case err != nil:
			outcomes[i] = adoptFailed
			errs = append(errs, fmt.Errorf("pack %s: %w", st.pack.PackID, err))
		case !ok:
			held(st)
			outcomes[i] = adoptHeld
		default:
			c.vouchPack(e, inc, st.pack.GitChecksum)
			linked = append(linked, i)
		}
	}
	if len(linked) == 0 {
		return outcomes, errors.Join(errs...)
	}

	now := c.now()
	e.mu.Lock()
	for _, i := range linked {
		st := stagings[i]
		setLive(inc, st.pack.PackID, liveOf(st.pack))
		inc.packs++
		inc.objects += st.pack.Objects
		c.setSize(inc, inc.diskBytes+st.pack.Size)
		outcomes[i] = adoptAdopted
	}
	inc.unmoved = false
	// No repair (incarnation.repairWrite): an adopted pack adds what no
	// failed request needed, and leaves whatever failed it as it was.
	// The maintainer's debounce and ceiling, exactly as a round that adds a
	// pack sets them; and like such a round's, no kick: the pack is due only
	// once the debounce has passed, which is the maintainer's own tick.
	e.lastAdded = now
	if e.uncoveredSince.IsZero() {
		e.uncoveredSince = now
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
	if err := sync(packDir); err != nil {
		errs = append(errs, fmt.Errorf("the packs are installed, and their directory is not yet durable: %w", err))
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
// its current incarnation (placeStagings). The registrations are taken, and
// the lock let go, under e.mu: an adoption that fails to try the lock under
// the same mu registers before this last look, or finds the lock free.
// Taken back after a release, a staging is dropped where nothing is left to
// install it into — the repository purged or evicted — and, once Stop has
// begun, left to the next boot's tmp/. An adoption's share (n one, never
// writeExclusive) leaves the adoptions holding the repository, and the last
// to leave wakes the maintainer a pass it held back is owed (owedWake).
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
