package cache

import (
	"cmp"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE SWAP: A ROUND THAT TAKES A PACK OUT OF A COPY A READER MAY HOLD BUILDS
// THE COPY'S NEXT INCARNATION BESIDE IT (ADR-0060). A compaction retires packs — the
// code domain moves their rows into code_packs_retired, the outputs holding
// every object of them — and each git node's cache learns it from a round: a
// delta's retired phase, or the retirements a full answer serves above the
// caller's position. Its pack files then leave the copy. They cannot leave IN
// PLACE: git opens a pack lazily, part way through a run, so a clone reading
// the copy's old refs may open a retired pack's files a second after the
// round, and no pin of a file could say which a running process will touch —
// a scratch push's pin holds its incarnation whole for the same reason. So
// the copy is SWAPPED, as a rebuild replaces one: the round applies to its
// end on the current incarnation — every pack installed or adopted, the refs
// written, the state renamed in, a truthful superset of the rows at the
// round's position — and then the next incarnation, repos/<id>.<n+1>.git, is
// built BY HARD LINKS of every pack the live rows still name, its HEAD,
// config and packed-refs and every derived file a retirement keeps but the
// memo, renamed into repos/ whole, and made current, the memo's tree MOVED
// into it whole as it is — one rename however many answers it holds
// (carryMemo), as a rebuild's promote moves it; the old one is retired, and
// its readers keep it whole until the last of them lets go (entry.go's
// retired-incarnation rule). The cost follows the pack COUNT — three links a
// live pack, one each for HEAD, config and packed-refs, the carried files'
// links and the memo's one rename — never the bytes: no GET and no git
// process, but a full round's own index, which it writes over the next
// incarnation rather than the one it leaves. Its durability is ONE SYNC A
// DIRECTORY, made once the staging is complete and before its rename
// (syncStaged): objects/pack, objects/info where it carries the commit-graph,
// crewlet/ where it carries the index of pushes, the installed chain's two
// directories where it holds one, then the state — its own file and the
// staged root — and repos/ after the rename: four to six fsyncs a swap —
// six to nine for a copy serving from a published chain, whose two chain
// files the layer step writes from the rows, each made durable as it is
// staged. The staging is ONE step of durable.go's rule, never several:
// nothing in it is read before the rename makes it an incarnation, and a
// crash before the rename leaves it in scratch/, which the boot moves aside.
// Synced step by step, a swap made up to eleven, every one a journal commit
// while the round holds the repository, on every git node at every
// compaction.
//
// WHAT LEAVES, AND WHAT IT TAKES WITH IT. The live set (entry.go's live) is
// the rows the copy last converged to hold, by pack id: set whole by a full
// round, a delta's added rows set and its retired rows taken out, an adopted
// pack set. A pack's files leave once no live row names their checksum
// (incarnation.leaving), each with the event that took it out — a
// retirement, an expunge's commit taking a dropping pack out of the
// repository, or a stray no retirement accounts for (a log gone back or
// recreated, a full answer that served no retirements) — and every derived
// file whose declaration DROPS on one of those events is simply not linked
// into the next incarnation, which never holds it, so the drop precedes the
// change the moment any reader can see it (dropFor's order rule). A merge
// drops no commit, so a retirement keeps the commit-graph (X04): the next
// incarnation holds the very same inode; a stray drops it, and so does an
// expunge, whose commits are gone from the repository — the memo with it, its
// answers perhaps about what was purged.
//
// WHAT CARRIES OVER. A linked file is the same inode, its bytes those this
// process wrote, checked or scrubbed, so the promote carries every per-file
// verdict of the files it links — their vouches, the hint hashes of their
// packs, the scrub's schedule and the packs it found whole, what a repair
// still owes them — and nothing for a file it dropped; and the repository's
// own records — its position, its refs, its rebuild line, its maintenance
// cadence, its last use. Every field of an incarnation is classified in
// swapFields, which TestEveryIncarnationFieldIsClassifiedForTheSwap holds to
// the struct both ways, so a field a later part adds is classified before a
// swap can lose it.
//
// NOTHING WRITES A FILE THE SWAP LINKS while it links it: a run of the
// maintainer in flight on the copy the swap leaves is stopped, and waited
// for, first — counted as a RUN by the duty cycle, the stop its end and what
// it had spent its length, since a repository compacted more often than its
// index run lasts would otherwise have every run stopped and every next one
// due at once, holding a maintenance slot for ever and writing nothing — and
// that copy's index of pushes takes no commit from then on (entry.swap).
//
// THE OLD COPY'S FILES ARE THE NEXT ONE'S where it kept them, so what is
// learnt of them after the swap lands on the incarnation serving them now
// (serving): a verdict reached over them — a scrub that was reading them, a
// forced check of the round that swapped — is recorded there; what a git
// process over the old copy reports is that copy's scrub's evidence (observe,
// handOn), as is what the round's own index reports from the staged directory
// (swapAt); and a request whose git failed over the old copy runs once more
// over the one serving, its repair unspent, so damage in a file the two share
// is repaired there and damage in one that left is simply gone
// (swappedAnswer). A copy a rebuild replaced is none of these: its successor
// was read whole and checked.
//
// A SWAP THAT FAILS leaves the copy as it was: the staged directory goes into
// trash/, the current incarnation keeps serving — it holds every live pack and
// the round's refs — what is leaving stays so, and the round answers from it.
// The repository's next round, delta or full, tries the swap again. A copy
// whose live pack, HEAD, config or packed-refs is gone from its own
// directory cannot be linked from; it is marked to be read whole, so its
// next round — full — puts the pack back with one GET, or the file anew, and
// swaps.
//
// THE INSTALLED CHAIN CROSSES A SWAP BY ITS ROWS (layers.go). Every pack an
// installed layer names joins the packs the next incarnation holds, live row
// or none — the chain holds its packs — and a swap that PLACES a chain, the
// installer's or a full round's on a copy a reader may hold, links the layers
// it keeps, renames in the ones it fetched, writes both chain files — their
// directories synced once with the rest of the staging, as every other one
// is — and carries no own-mode index and no graph (EventLayersMoved drops
// them: git reads either before a chain); the packs the chain it leaves held
// alone go with the old copy. Before the rename a CANARY reads through the
// staged chain's bitmap wherever the placement added a layer, and a canary
// that fails abandons the placement — its added layers refused corrupt —
// while the swap is made again for any packs still leaving.
//
// A CRASH before the rename leaves the old incarnation the highest in repos/
// with its state and the staged one in scratch/, which every boot empties;
// the state names the floor the leaving files' retirements are above
// (state.LeavingSince), so the boot's first round reads them as retirements
// and keeps the commit-graph. The rename is the swap's one irreversible step,
// and its fault point (codefault.SyncSwapBeforeRename) is the moment before
// it.

// swapGraph is the graph attribute of crewlet.code.cache.swaps: what became
// of the commit-graph of the copy a swap left — kept by the next
// incarnation, dropped, or none there to keep (swapped) — that the next
// incarnation serves from an installed chain, or that the swap failed. CHAIN
// is its own value because own mode's answers do not hold there: a chained
// copy holds no whole graph to keep, and the background indexing writes none
// beside the chain (layers.go), so counting its swaps as dropped would read
// as a graph to be written again, and as none would hide that a chain's
// every placement on a held copy is a swap.
const (
	swapGraphKept    = "kept"
	swapGraphDropped = "dropped"
	swapGraphNone    = "none"
	swapGraphChain   = "chain"
	swapFailed       = "failed"
)

// swapLeaving swaps inc, the entry's current incarnation, into its next when
// packs are leaving it or step places its chain (layers.go: a full round's
// placement on a copy a reader may hold, or the installer's), and answers the
// incarnation that is current once it returns: the next one, or inc after a
// swap that failed — which leaves inc serving, its leaving kept for the next
// round. A copy whose live pack or root file is gone from its directory is
// marked to be read whole. The caller holds the entry's write lock whole, for
// its round or its placement. snap is the snapshot the round APPLIED — a full
// one's index is written over the next incarnation, which holds exactly its
// packs — or the zero snapshot where it applied none.
//
// A PLACEMENT WHOSE CANARY FINDS THE CHAIN UNSOUND IS ABANDONED: every layer
// it added is refused as corrupt, and the swap is made again for what else it
// was for — packs leaving — with the chain the copy holds carried as it
// stands, so one bad layer undoes none of the round's own work. A canary that
// read nothing of the chain is no verdict, and fails the swap at its step as
// a write the disk failed does (layers.go's canary). Whatever the placement
// fetched and no swap placed goes from tmp/.
func (c *Cache) swapLeaving(ctx context.Context, e *entry, inc *incarnation, snap snapshot,
	step *layerStep) *incarnation {

	if step != nil {
		defer step.discard(c)
	}
	e.mu.Lock()
	leaving := (len(inc.leaving) > 0 || step != nil) && e.current == inc && !inc.retired
	e.mu.Unlock()
	if !leaving {
		return inc
	}
	for {
		next, err := c.swapOnce(ctx, e, inc, snap, step)
		var bad *canaryFailed
		if err == nil || !errors.As(err, &bad) {
			return next
		}
		e.mu.Lock()
		for _, row := range bad.added {
			c.refuse(e, inc, row, layerCorrupt, bad.err.Error())
		}
		again := len(inc.leaving) > 0
		e.mu.Unlock()
		for _, st := range step.fetched {
			if slices.ContainsFunc(bad.added, func(r code.LayerRow) bool { return sameLayer(r, st.row) }) {
				c.observeInstall(layerCorrupt, time.Since(st.began))
			}
		}
		if !again {
			return inc
		}
		step = nil
	}
}

// canaryFailed is a placement's canary finding the chain staged in the next
// incarnation unsound (errChainUnsound): the layers the placement added, and
// what the canary met.
type canaryFailed struct {
	added []code.LayerRow
	err   error
}

func (f *canaryFailed) Error() string { return f.err.Error() }
func (f *canaryFailed) Unwrap() error { return f.err }

// swapOnce is one swap of inc into its next incarnation, with the placement
// step when one is asked: the next incarnation made current, or inc and the
// failure that left it serving.
func (c *Cache) swapOnce(ctx context.Context, e *entry, inc *incarnation, snap snapshot,
	step *layerStep) (*incarnation, error) {

	began := time.Now()
	s := &swapping{c: c, e: e, inc: inc, snap: snap, layers: step}
	next, err := s.run(ctx)
	e.mu.Lock()
	e.swap = nil
	e.mu.Unlock()
	if err == nil {
		c.swapped(s, next, time.Since(began))
		if s.layers != nil {
			c.installedLayers(e.id, s.placedLayers)
			c.countLayers(layerKept, s.keptLayers)
		}
		c.countLayers(layerCut, s.cutLayers)
		return next, nil
	}
	if s.staged != "" {
		if terr := toTrash(c.dir, s.staged, newV7()+"-"+filepath.Base(s.staged)); terr != nil &&
			!errors.Is(terr, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", s.staged, "error", terr)
		}
	}
	c.log.Warn("code_cache_swap_failed", "repo", e.id, "incarnation", inc.n, "step", s.step, "error", err)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheSwaps, 1, metrics.Attrs{"graph": swapFailed})
	}
	// EVERY LAYER THE SWAP WAS TO PLACE FAILED WITH IT, a try that may pass —
	// a full round's left to the installer, which the round's end kicks, and
	// the installer's tried again behind its wait — counted once a try, as a
	// fetch that failed is, whoever's the placement was. A canary's verdict is
	// no such try: its refusal counts it (swapLeaving).
	var bad *canaryFailed
	if s.layers != nil && !errors.As(err, &bad) {
		for _, st := range s.layers.fetched {
			c.observeInstall(layerFailed, time.Since(st.began))
		}
		c.countLayers(layerFailed, len(s.layers.fetched))
	}
	if errors.Is(err, fs.ErrNotExist) && (s.step == stepPacks || s.step == stepRoot) {
		// A file the swap links is gone from the copy's own directory —
		// damage no retry ends, since every swap of the copy links it
		// again: the next round reads the copy whole and swaps. It puts a
		// live pack back with one GET (install's lost), counted as the
		// stored files' repair, and HEAD, config or packed-refs anew (tidy,
		// writeRefs), as the shape its sweep puts right.
		reason := RebuildStored
		if s.step == stepRoot {
			reason = RebuildShape
		}
		e.mu.Lock()
		c.markWhole(e, inc, Failure{Reason: reason}, dropRound)
		e.mu.Unlock()
	}
	return inc, err
}

// The steps of a swap, as code_cache_swap_failed names the one it failed at.
// The round's index between the layers and the canary is none: it fails
// nothing (indexRound).
const (
	stepStage   = "stage"
	stepPacks   = "packs"
	stepRoot    = "root"
	stepDerived = "derived"
	stepLayers  = "layers"
	stepCanary  = "canary"
	stepSync    = "sync"
	stepState   = "state"
	stepRename  = "rename"
)

// stagedName is the name under scratch/ a swap of repository id stages its
// next incarnation at: swap.<id>.<v7>.git — the id so a report of a git
// process run there finds the repository (swapAt), a fresh v7 so no two
// swaps' stagings, a crashed one's among them, share a name.
func stagedName(id string) string { return "swap." + id + "." + newV7() + ".git" }

// swapAt is the entry whose swap stages its next incarnation at dir, and the
// incarnation that swap leaves — the entry's current one until the promote —
// or nil: a git process run in the staged directory, the round's own index
// (step 5), reads the live packs' files, which the next incarnation holds as
// the very inodes the one it leaves does, so what it reports is about the copy
// serving them now, and the promote carries what that copy makes of it.
func (c *Cache) swapAt(dir string) (*entry, *incarnation) {
	if dir == "" || filepath.Dir(dir) != filepath.Join(c.dir, scratchDir) {
		return nil, nil
	}
	parts := strings.Split(filepath.Base(dir), ".")
	if len(parts) != 4 || parts[0] != "swap" || parts[3] != "git" {
		return nil, nil
	}
	c.mu.Lock()
	e := c.entries[parts[1]]
	c.mu.Unlock()
	if e == nil {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if s := e.swap; s != nil && s.stagedAt == dir && e.current == s.inc && !e.dead {
		return e, s.inc
	}
	return nil, nil
}

// swappedOut reports whether inc is an incarnation a swap replaced whose
// files the entry's current incarnation serves now (serving), as the very
// inodes for every one it kept: the copy a reader still holds was swapped
// under it. One a rebuild replaced is not — its successor was read whole and
// checked, and holds none of its files unchecked — nor one of a purged or
// evicted repository. e.mu is held.
func swappedOut(e *entry, inc *incarnation) bool {
	return !e.dead && e.current != nil && inc != e.current && serving(inc) == e.current
}

// swapping is one swap in flight: the incarnation it leaves and the one it
// builds, what it keeps and what it has done — what the promote's carries
// (swapFields) read.
type swapping struct {
	c    *Cache
	e    *entry
	inc  *incarnation
	snap snapshot
	// layers is a PLACEMENT the swap makes of the installed chain
	// (layers.go): the chain the next incarnation holds, the layers it keeps
	// of inc linked and those fetched renamed in — or nil, inc's installed
	// chain carried whole, every pack it names being in inc (the chain holds
	// its packs). chain is the chain the next incarnation holds once the
	// layer step has run, added the layers of it inc did not hold, which the
	// canary reads, and chainShared the bytes of each chain declaration's
	// files the two incarnations hold alike, linked.
	layers      *layerStep
	chain       []code.LayerRow
	added       []code.LayerRow
	chainShared map[DerivedName]int64
	// tip is the node's tip commit-graph layer the next incarnation carries,
	// linked, with the tail the last write of it covered (tipOver,
	// tipWritten): inc's where the swap keeps the whole chain beneath it and
	// every pack leaving is a retirement (tipGoes) — none otherwise, and none
	// for a placement, which moves the chain beneath it.
	tip        tipLayer
	tipOver    tailDigest
	tipWritten bool
	// placedLayers is the staged layers the layer step renamed in,
	// keptLayers how many it linked, and cutLayers how many layers of inc's
	// installed chain the next incarnation carries no file of — replaced by
	// a publication, stale, above a pack leaving, or fetched again over
	// files that failed: what the swap counts once it is made, as cutChain
	// counts what it cuts in place.
	placedLayers []*stagedLayer
	keptLayers   int
	cutLayers    int
	// wrote is every file the layer step placed by a write or a rename
	// rather than a link — the layers it fetched, the two chain files —
	// relative to the staged root: syncStaged syncs their directories, as
	// it syncs those of what the swap linked.
	wrote []string

	// next is the incarnation being built, staged at staged until the rename
	// gives it its number. stagedAt is the staged directory as step 1 named
	// it, never moved: what a git process run there names (swapAt). step is
	// the step in flight, which a failure names.
	next     *incarnation
	staged   string
	stagedAt string
	step     string

	// packs is the live set's packs by checksum — what the next incarnation
	// holds — places where it holds each (packPlace: objects/pack, or the
	// dropping view for a pack only dropping rows name), and keys the same
	// checksums as the twenty bytes a vouch keys a pack by. keep is the
	// declarations a swap may carry: those no event of what is leaving
	// drops; carried, the ones it linked files of, and the memo once the
	// promote has moved its tree (carryMemo). linked is every derived file it
	// linked, by its path relative to the incarnation.
	packs   map[string]livePack
	places  map[string]packPlace
	keys    map[[hashTrailer]byte]bool
	keep    derivedSet
	carried derivedSet
	linked  map[string]bool
	// events is every event that took a pack file out, refsBytes the
	// packed-refs it linked, and shared the bytes the two incarnations hold
	// alike: the live packs' and the packed-refs' — of which sharedDropping
	// the dropping view's. heldGraph: the copy the swap leaves held a
	// commit-graph once nothing could write one in it — its own whole graph,
	// or its installed chain's graph layers — what tells a swap that dropped
	// the graph from one of a copy with none to keep (swapped).
	events         map[Event]bool
	refsBytes      int64
	shared         int64
	sharedDropping int64
	heldGraph      bool
}

// keepsPack reports whether the next incarnation holds the pack checksum cs.
func (s *swapping) keepsPack(cs string) bool {
	_, ok := s.packs[cs]
	return ok
}

// keepsDecl reports whether the swap carried the declaration name's files
// into the next incarnation.
func (s *swapping) keepsDecl(name DerivedName) bool {
	return s.carried&s.c.image.set(name) != 0
}

// holds reports whether the next incarnation holds inc's file at rel, as
// the same inode: one of a pack's files it keeps at the place rel lies in, or
// a derived file it linked.
func (s *swapping) holds(rel string) bool {
	if cs, ok := packOfRel(rel); ok {
		return s.keepsPack(cs) && s.places[cs].rel() == filepath.Dir(rel)
	}
	return s.linked[rel]
}

// run is the swap's steps, in order, answering the next incarnation made
// current or the failure that left inc as it was. Each step is named in
// s.step as it runs.
func (s *swapping) run(ctx context.Context) (*incarnation, error) {
	c, e, inc := s.c, s.e, s.inc
	e.mu.Lock()
	s.packs = make(map[string]livePack, len(inc.byChecksum))
	s.places = make(map[string]packPlace, len(inc.byChecksum))
	for _, p := range inc.live {
		s.packs[p.checksum] = p
		s.places[p.checksum] = placeOf(inc, p.checksum)
	}
	s.events = map[Event]bool{}
	// leaving is the checksums whose files leave objects/pack — what a layer
	// of the chain may name; one leaving the dropping view names none.
	leaving := map[string]bool{}
	for key, ev := range inc.leaving {
		s.events[ev] = true
		if key.place != placeDropping {
			leaving[key.name] = true
		}
	}
	installed, chainPacks := slices.Clone(inc.installed), maps.Clone(inc.installedPacks)
	s.tip, s.tipOver, s.tipWritten = inc.tip, inc.tipOver, inc.tipWritten
	hadTip := inc.tip.id != ""
	e.mu.Unlock()
	// 0. THE CHAIN the next incarnation holds, decided before anything is
	// staged: the placement's, or inc's own carried — cut where a pack
	// leaving names a layer of it, which the chain's holding its packs means
	// none does (prefixOf, the one cut) — and every pack it names joins what
	// the next incarnation holds, live or not: the chain holds its packs; and
	// whether the node's tip crosses over it.
	if err := s.chainOf(installed, chainPacks, leaving); err != nil {
		s.step = stepLayers
		return nil, err
	}
	var drop derivedSet
	for ev := range s.events {
		drop |= c.image.dropping(ev)
	}
	s.keep = c.image.all() &^ drop
	s.keys = make(map[[hashTrailer]byte]bool, len(s.packs))
	for cs := range s.packs {
		var key [hashTrailer]byte
		if _, err := hex.Decode(key[:], []byte(cs)); err == nil {
			s.keys[key] = true
		}
	}
	s.linked = map[string]bool{}

	// 1. STAGE scratch/swap.<id>.<v7>.git: scratch/, because a crash before
	// the rename must leave the old incarnation the highest in repos/ — and
	// Open empties scratch/ into trash/ — and because the janitor counts
	// scratch/ by sole names, so the staged links count only what they alone
	// hold. Named by the repository, so what a git process run in it reports
	// reaches the copy whose files it reads (swapAt). Its directories are
	// every incarnation's (requiredDirs); its files are all linked but its
	// state. From here the swap is the entry's (entry.swap), until
	// swapLeaving ends it.
	s.step = stepStage
	s.staged = filepath.Join(c.dir, scratchDir, stagedName(e.id))
	if err := os.Mkdir(s.staged, hostbox.DirMode); err != nil {
		s.staged = ""
		return nil, err
	}
	s.stagedAt = s.staged
	e.mu.Lock()
	e.swap = s
	e.mu.Unlock()
	for _, d := range requiredDirs {
		if err := os.MkdirAll(filepath.Join(s.staged, filepath.FromSlash(d)), hostbox.DirMode); err != nil {
			return nil, err
		}
	}
	// THE NEXT INCARNATION stands outside the cache's total while it is
	// staged — discarded, as an incarnation whose bytes the total does not
	// count is — so nothing a step records of it moves the total, and a
	// swap that fails takes nothing back out: the promote counts it in.
	s.next = &incarnation{dir: s.staged, discarded: true}

	// 2. THE PACKS, every one the live set names, largest first: .pack, .rev,
	// then .idx last, so a pack is present to git only once whole — each
	// where the live set places it, linked from where inc holds it there: a
	// dropping pack into the next incarnation's dropping view, made for its
	// first (a next incarnation holding none has none), every other into
	// objects/pack.
	s.step = stepPacks
	order := slices.SortedFunc(maps.Keys(s.packs), func(a, b string) int {
		return cmp.Or(cmp.Compare(s.packs[b].size, s.packs[a].size), cmp.Compare(a, b))
	})
	viewMade := false
	for _, cs := range order {
		place := s.places[cs]
		into := filepath.Join(s.staged, place.rel())
		if place == placeDropping && !viewMade {
			if err := os.MkdirAll(into, hostbox.DirMode); err != nil {
				return nil, err
			}
			viewMade = true
		}
		linked, err := linkBundle(c.link, filepath.Join(inc.dir, place.rel(), "pack-"+cs), into, cs, nil)
		switch {
		case err != nil:
			return nil, err
		case !linked:
			return nil, fmt.Errorf("pack-%s is in %s already", cs, into)
		}
		s.shared += s.packs[cs].size
		if place == placeDropping {
			s.sharedDropping += s.packs[cs].size
		}
	}

	// 3. THE ROOT'S FILES — HEAD, config and packed-refs — the old
	// incarnation's own, LINKED: none is ever written in place (a rewrite of
	// any stages and renames, as tidy and writeRefs do), so a link is a
	// snapshot of each, the packed-refs the round's own; written afresh,
	// HEAD and config cost two files staged and synced on every swap.
	s.step = stepRoot
	for _, name := range []string{"HEAD", "config", "packed-refs"} {
		if err := c.link(filepath.Join(inc.dir, name), filepath.Join(s.staged, name)); err != nil {
			return nil, err
		}
	}
	info, err := os.Lstat(filepath.Join(s.staged, "packed-refs"))
	if err != nil {
		return nil, err
	}
	s.refsBytes = info.Size()
	s.shared += s.refsBytes

	// 4. THE DERIVED FILES a retirement keeps, once nothing writes one in the
	// copy the swap leaves: a maintenance run on it stopped and waited for
	// (stopRun), and its index of pushes taking no commit while the swap is
	// the entry's (commitIndex), so what the next incarnation records of a
	// file is what the swap linked of it.
	s.step = stepDerived
	s.stopRun()
	if rel := c.image.mainRel(DerivedGraph); rel != "" {
		_, gerr := os.Lstat(filepath.Join(inc.dir, rel))
		s.heldGraph = gerr == nil
	}
	if _, graph := chainIDs(installed); len(graph) > 0 || hadTip {
		// A chain's graph layers and the node's tip over them are the copy's
		// commit-graph as much as an own one is: a swap whose next incarnation
		// holds no chain and no graph took them, and the maintainer writes a
		// whole graph again.
		s.heldGraph = true
	}
	if err = s.carryDerived(); err != nil {
		return nil, err
	}

	// 4b. THE INSTALLED CHAIN (layers.go): every layer of it the copy kept
	// LINKED, every one the placement fetched RENAMED IN from tmp/, and both
	// chain files written over the chain — staged and renamed in, after the
	// layers' files, so neither names a file not there — with no directory
	// synced at the step: the staging's every directory is, once, at step 6.
	s.step = stepLayers
	if err = s.placeLayers(); err != nil {
		return nil, err
	}

	// 5. A FULL ROUND'S INDEX over exactly the live packs, written into the
	// staged directory as it would have been into the old one in place —
	// part of the round's answer, in the round's read lane, vouched for as
	// written; what its git reports reaches the copy it reads (swapAt). A
	// delta writes none, and nor does a round beside a chain the next
	// incarnation holds (indexRound's indexed). Never a failure of the swap:
	// a round answers without its index, slower and never wrong.
	if s.snap.full {
		c.indexRound(ctx, e, s.next, s.snap)
	}

	// 5b. THE CANARY (layers.go), where the placement added a layer: one
	// bitmapped walk in the staged directory before the rename that makes
	// the chain visible — a layer over the wrong base passes every check of
	// its own files and fails the first read through its bitmap. Its verdict
	// that the chain is unsound abandons the placement (swapLeaving), its
	// added layers refused — before the staging is synced, which a placement
	// abandoned would have spent; a canary that read nothing of the chain (no
	// read slot within SlotWait, its deadline) is no verdict, and fails the
	// swap here, as a write the disk failed would, for the layers to be tried
	// again. The installer's placement walks in the installer's share of the
	// read lane for canaries (takeInstallRead), behind the serving — never
	// behind its checks' hashing, since this swap holds the repository's
	// write lock, which the repository's rounds wait for; a round's, which a
	// reader waits on, in the lane as the round's other reads do.
	if len(s.added) > 0 {
		s.step = stepCanary
		objects := make(map[string]int64, len(s.packs))
		for cs, p := range s.packs {
			if s.places[cs] != placeDropping {
				objects[cs] = p.objects
			}
		}
		share := func() {}
		if s.layers.installer {
			if share, err = c.takeInstallRead(ctx, true); err != nil {
				return nil, err
			}
		}
		err = c.canary(ctx, e, inc, s.staged, tailObjects(s.chain, objects), nil)
		share()
		switch {
		case errors.Is(err, errChainUnsound):
			return nil, &canaryFailed{added: s.added, err: err}
		case err != nil:
			return nil, err
		}
	}

	// 6. THE STAGING MADE DURABLE, each directory a link or a write landed
	// in synced once (syncStaged); then THE STATE, which syncs the root
	// last; then the rename's fault point, the NUMBER and the rename.
	s.step = stepSync
	if err = s.syncStaged(); err != nil {
		return nil, err
	}
	s.step = stepState
	if err = s.writeState(ctx); err != nil {
		return nil, err
	}
	s.step = stepRename
	if c.faults != nil {
		if err = c.faults.Hit(ctx, codefault.SyncSwapBeforeRename, 0); err != nil {
			return nil, err
		}
	}
	if err = s.rename(); err != nil {
		return nil, err
	}
	s.staged = ""

	// 7. THE PROMOTE.
	s.promote()
	return s.next, nil
}

// stopRun stops a maintenance run on the incarnation the swap leaves and waits
// for it to end, as a full round's sweep stops one (stopMaintenance): nothing
// then writes a derived file of that copy while the swap links it, so what
// the next incarnation records of a file is what it holds. The stop counts as
// a RUN of the duty cycle where the run is one it counts (maintRun.duty) —
// the repository's last run ended now, after what the run had spent — since
// a run ended from outside records no end of its own (maintain), and a
// repository compacted more often than its index run lasts would otherwise
// have every run stopped and every next one due at once: a maintenance slot
// held for ever, and nothing written. A run that ended by itself meanwhile
// recorded its own end, which stands. Recorded on the incarnation the swap
// leaves, it is carried with the rest of its cadence (swapFields), and kept
// by a swap that fails: the run was stopped either way.
func (s *swapping) stopRun() {
	s.e.mu.Lock()
	m, before := s.e.maint, s.inc.maintained
	s.e.mu.Unlock()
	if m == nil || m.inc != s.inc {
		return
	}
	m.cancel()
	<-m.done
	if !m.duty {
		return
	}
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	if s.inc.maintained == before {
		s.inc.maintained = maintainedState{LastRun: s.c.now(), Took: time.Since(m.began)}
	}
}

// carryDerived links into the staged incarnation the files of every
// declaration the swap keeps, and gives it what each records: the files a
// git process writes by the declaration's own patterns (carryFiles) and the
// index of pushes by its record (carryIndex). A directory a declaration owns
// whole — the memo's tree — is never linked: it MOVES into the next
// incarnation as that is promoted, one rename (promote's carryMemo, SeedMove),
// where linked an entry at a time it would cost a stat and a link an answer
// inside the round that holds the repository, on every git node holding it at
// every compaction. The directories its links land in are synced with the
// rest of the staging, once each (syncStaged).
func (s *swapping) carryDerived() error {
	var err error
	s.c.image.each(s.keep, func(i int, d *Derived) {
		if err != nil {
			return
		}
		var carried bool
		switch {
		case d.tree():
			// Moved at the promote (carryMemo), never linked here.
		case d.chained():
			// The installed chain: placed by the layer step (placeLayers),
			// layer by layer as the chain the next incarnation holds says —
			// never by its patterns, which would carry a layer the chain
			// leaves and the chain file naming it.
		case d.inProcess() && !d.hasMain():
			carried, err = s.carryIndex(d)
		default:
			carried, err = s.carryFiles(d)
		}
		if carried {
			s.carried |= 1 << i
		}
	})
	return err
}

// syncStaged makes durable what the staging holds below its root, before its
// state is written: each directory a link or a write landed in, synced ONCE —
// objects/pack for the live packs and a full round's index, which is written
// only over them, the directory of each derived file it linked
// (objects/info for the commit-graph, crewlet/ for the index of pushes), and
// those of the installed chain the layer step linked, renamed in and wrote
// (multi-pack-index.d and commit-graphs, a chain file in each). The
// root — HEAD, config, packed-refs and the state — is synced by the state's
// write, last (writeState). Nothing in the staging is read before the rename
// makes it an incarnation, and a crash before the rename leaves it in
// scratch/, which every boot moves aside, so the staging needs its files
// durable once, at the end, and never step by step: each sync more is a
// journal commit while the round holds the repository, on every git node at
// every compaction, which can wait out the data other rounds are writing, the
// readers waiting on the round with it.
func (s *swapping) syncStaged() error {
	dirs := map[string]bool{}
	for _, place := range s.places {
		dirs[place.rel()] = true
		if place == placeDropping {
			// The view's own directory, which names its pack directory.
			dirs[gitproc.DroppingDir] = true
		}
	}
	for rel := range s.linked {
		if dir := filepath.Dir(rel); dir != "." {
			dirs[dir] = true
		}
	}
	for _, rel := range s.wrote {
		if dir := filepath.Dir(rel); dir != "." {
			dirs[dir] = true
		}
	}
	for _, rel := range slices.Sorted(maps.Keys(dirs)) {
		if err := s.c.durableDir(filepath.Join(s.staged, rel)); err != nil {
			return err
		}
	}
	return nil
}

// unsynced is the layer step's sync of a directory of the staging
// (writeChains): none at the step, since nothing reads the staging before its
// rename, and syncStaged syncs every directory the step wrote in, once.
func unsynced(string) error { return nil }

// chainOf decides the chain the next incarnation holds — the placement's
// rows (layers), or inc's installed chain cut to the prefix the packs
// leaving leave valid (prefixOf) — and the layers of it inc does not hold,
// which the canary reads; and adds to the packs the next incarnation holds
// every pack that chain names, with the figures inc recorded of it: a pack
// an installed layer names stays while the layer does, whether or not a live
// row still names it (layers.go, the chain holds its packs). A pack the chain
// names that inc holds neither as a live row nor for its chain fails the swap:
// a layer is valid only over every pack it names. The node's tip — s.tip and
// the tail its last write covered, inc's as step 0 read them — crosses only
// where the chain beneath it does, whole, and every pack leaving is a
// retirement (tipGoes). installed, held and leaving are inc's, read under
// e.mu: its chain, the packs its chain holds and the pack files leaving.
func (s *swapping) chainOf(installed []code.LayerRow, held map[string]livePack, leaving map[string]bool) error {
	if s.layers != nil {
		s.chain = slices.Clone(s.layers.rows)
		s.added = s.layers.adds(installed)
		s.events[EventLayersMoved] = true
	} else {
		k := prefixOf(installed, EventPackRetired, eventDetail{packs: leaving})
		s.chain = installed[:k]
		if k < len(installed) {
			s.events[EventLayersMoved] = true
		}
	}
	for ev := range s.events {
		if tipGoes(installed, len(s.chain), ev) {
			s.tip, s.tipOver, s.tipWritten = tipLayer{}, tailDigest{}, false
		}
	}
	// CUT is every layer of inc whose files the next incarnation does not
	// carry: one its chain does not hold, and one fetched again over files
	// that failed their check — counted as the cut in place counts them
	// (cutChain), so the outcome reads alike whichever way it was placed.
	refetched := map[string]bool{}
	if s.layers != nil {
		for _, st := range s.layers.fetched {
			refetched[st.row.Layer.LayerID] = true
		}
	}
	s.cutLayers = 0
	for _, r := range installed {
		if refetched[r.Layer.LayerID] ||
			!slices.ContainsFunc(s.chain, func(n code.LayerRow) bool { return sameLayer(n, r) }) {
			s.cutLayers++
		}
	}
	for _, r := range s.chain {
		for _, cs := range r.Layer.Packs {
			if _, ok := s.packs[cs]; ok {
				continue
			}
			p, ok := held[cs]
			if !ok {
				return fmt.Errorf("layer %s names pack-%s, which the copy holds for no live row and no "+
					"installed layer", r.Layer.LayerID, cs)
			}
			// In objects/pack, where every pack a layer names lies.
			s.packs[cs], s.places[cs] = p, placeLive
		}
	}
	return nil
}

// placeLayers is the swap's layer step: every layer of the chain the next
// incarnation holds placed in the staged directory — renamed in from tmp/
// where the placement fetched it (a layer new to the copy, or one fetched
// again over files that failed their check), linked from inc otherwise, the
// same inodes — then the node's tip linked where it crosses (chainOf), both
// chain files written over the chain, the commit-graph's ending in that tip,
// and the next incarnation's record of it set (recordInstalled) at the bytes
// the rows count. Nothing is synced here: the directories of what it wrote and linked
// are synced once with the rest of the staging (syncStaged), since nothing
// reads the staging before its rename, where synced as an incarnation's are
// in place its two chain files would cost four more directory syncs at every
// swap of a chained copy. Every file it wrote is vouched, as written by this
// process from files held to their rows; a linked one carries inc's vouch
// (carryVouches).
// The bytes the two incarnations hold alike, linked, are counted by
// declaration (chainShared), which the promote takes out of inc's total.
func (s *swapping) placeLayers() error {
	c := s.c
	fetched := map[string]*stagedLayer{}
	if s.layers != nil {
		for _, st := range s.layers.fetched {
			fetched[st.row.Layer.LayerID] = st
		}
	}
	s.chainShared = map[DerivedName]int64{}
	var wrote []string
	for _, r := range s.chain {
		if st := fetched[r.Layer.LayerID]; st != nil && st.dir != "" && sameLayer(st.row, r) {
			moved, err := c.moveLayerIn(st, s.staged)
			wrote = append(wrote, moved...)
			if err != nil {
				return err
			}
			s.placedLayers = append(s.placedLayers, st)
			continue
		}
		linked, err := c.linkLayerIn(r, s.inc.dir, s.staged)
		for _, rel := range linked {
			s.linked[rel] = true
			if i, _ := c.image.owner(rel); i >= 0 {
				s.carried |= 1 << i
			}
		}
		if err != nil {
			return err
		}
		s.keptLayers++
		s.chainShared[DerivedLayers] += r.Layer.MidxBytes + r.Layer.BitmapBytes
		s.chainShared[DerivedGraphLayers] += r.Layer.GraphBytes
	}
	if s.tip.id != "" {
		// THE NODE'S TIP, linked over the whole chain it was written over —
		// or, gone from inc's directory (a run that ended before the swap
		// stopped it wrote another), carried as none, and as no write of one:
		// the maintainer writes one at once over the next incarnation's tail.
		rel := tipRel(s.tip.id)
		err := os.MkdirAll(filepath.Join(s.staged, filepath.Dir(rel)), hostbox.DirMode)
		if err == nil {
			err = c.link(filepath.Join(s.inc.dir, rel), filepath.Join(s.staged, rel))
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			s.tip, s.tipOver, s.tipWritten = tipLayer{}, tailDigest{}, false
		case err != nil:
			return err
		default:
			s.linked[rel] = true
			s.carried |= c.image.set(DerivedGraphLayers)
			s.chainShared[DerivedGraphLayers] += s.tip.bytes
		}
	}
	if err := c.writeChains(s.staged, s.chain, s.tip.id, unsynced); err != nil {
		return err
	}
	if len(s.chain) > 0 {
		wrote = append(wrote, layersChainRel)
		if _, graph := chainIDs(s.chain); len(graph) > 0 || s.tip.id != "" {
			wrote = append(wrote, graphChainRel)
		}
	}
	s.wrote = wrote
	ids := make([]fileID, len(wrote))
	for i, rel := range wrote {
		ids[i] = lstatID(filepath.Join(s.staged, rel))
	}
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	s.next.chainGen = s.inc.chainGen + 1
	s.next.tip, s.next.tipOver, s.next.tipWritten = s.tip, s.tipOver, s.tipWritten
	c.recordInstalled(s.next, s.chain, s.packs)
	for i, rel := range wrote {
		vouch(s.next, rel, ids[i])
	}
	if len(wrote) > 0 {
		c.wrote(s.next, c.image.set(DerivedLayers, DerivedGraphLayers))
	}
	return nil
}

// carryFiles links every regular file of the old incarnation d owns into the
// same place in the staged one, and records d there as the old one records
// it — at the bytes it linked; a part linked beside no main file is counted
// and never read as made (recordDerived's rule).
func (s *swapping) carryFiles(d *Derived) (bool, error) {
	var bytes int64
	present, main := false, false
	for _, o := range d.Owns {
		rel := filepath.FromSlash(o.Dir)
		list, err := s.c.image.listed(s.inc.dir, rel)
		if err != nil {
			return false, err
		}
		for _, entry := range list {
			if !entry.Type().IsRegular() || !o.Name.MatchString(entry.Name()) {
				continue
			}
			to := filepath.Join(s.staged, rel, entry.Name())
			if err = os.MkdirAll(filepath.Dir(to), hostbox.DirMode); err != nil {
				return false, err
			}
			if err = s.c.link(filepath.Join(s.inc.dir, rel, entry.Name()), to); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue // gone since the listing: nothing to carry
				}
				return false, err
			}
			info, err := os.Lstat(to)
			if err != nil {
				return false, err
			}
			s.linked[filepath.Join(rel, entry.Name())] = true
			bytes += info.Size()
			present, main = true, main || o.Main
		}
	}
	if !present {
		return false, nil
	}
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	st := s.inc.derived[d.Name]
	if !main {
		st = derivedState{}
	}
	st.Bytes = bytes
	s.c.setDerived(s.next, d.Name, st, true)
	return true, nil
}

// carryIndex links every run of the old incarnation's index of pushes into
// the staged one and carries its record — the one it read, which takes no
// commit while the swap is the entry's (commitIndex), so every run it names
// is on disk to link; the backfill's work on the old copy from then on is the
// next copy's to redo. An index whose log a round marked gone carries
// nothing, and nor does one a run of which is gone from the disk: the next
// copy starts with no index, which its backfill reads again, rather than a
// record naming a file it does not hold — and the swap goes on, where failing
// it would fail every swap of the copy until a full round, keeping retired
// packs in it for as long.
func (s *swapping) carryIndex(d *Derived) (bool, error) {
	s.e.mu.Lock()
	rec, st, gone := s.inc.record(), s.inc.derived[d.Name], s.inc.historyGone
	s.e.mu.Unlock()
	if rec == nil || gone {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Join(s.staged, engineRoot), hostbox.DirMode); err != nil {
		return false, err
	}
	var linked []string
	for _, r := range rec.Runs {
		rel := runRel(r.N)
		err := s.c.link(filepath.Join(s.inc.dir, rel), filepath.Join(s.staged, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			for _, l := range linked {
				if rerr := os.Remove(filepath.Join(s.staged, l)); rerr != nil {
					return false, rerr
				}
			}
			s.c.log.Warn("code_cache_provenance_lost", "repo", s.e.id, "incarnation", s.inc.n,
				"why", "a run the index names is not on disk to link into the next incarnation: it starts with "+
					"none, made anew by its next full round")
			return false, nil
		case err != nil:
			return false, err
		}
		linked = append(linked, rel)
	}
	for _, rel := range linked {
		s.linked[rel] = true
	}
	s.e.mu.Lock()
	defer s.e.mu.Unlock()
	s.c.setDerived(s.next, d.Name, st, true)
	return true, nil
}

// writeState writes the staged incarnation's state — staged in tmp/ and
// renamed in, the directory synced, as every state is — from the records the
// old incarnation holds now and what the swap counted of the next: its packs,
// their bytes and the packed-refs', its objects, and its derived files'. It
// carries the copy's LAST USE as every state written does (writeState's
// stampUse): a swap is work, no use, and a state stamped with the moment of
// its write would bring a copy nobody had read in a week back from a restart
// as the copy read last.
func (s *swapping) writeState(ctx context.Context) error {
	s.e.mu.Lock()
	next, inc := s.next, s.inc
	next.at, next.stream, next.repo = inc.at, inc.stream, inc.repo
	next.refsDigest, next.refs = inc.refsDigest, inc.refs
	next.maintained = inc.maintained
	next.lastUsed = inc.lastUsed
	// The packs and objects an index covers are objects/pack's: the
	// dropping view's are on the disk the budget counts, and in neither.
	next.packs, next.objects = 0, 0
	for cs, p := range s.packs {
		if s.places[cs] != placeDropping {
			next.packs++
			next.objects += p.objects
		}
	}
	s.c.setSize(next, s.shared)
	s.c.setDropping(next, s.sharedDropping)
	// THE EPOCH ITS OBJECTS/ HOLDS (Synced.DropEpoch): the rows' at the
	// position it reflects, every pack file of which it holds where they
	// place it — a begin's inputs gone with inc — once its chain holds no
	// pack for a row that is not live (liveChain), and inc's otherwise.
	next.dropEpoch = inc.dropEpoch
	if liveChain(inc, s.chain) {
		next.dropEpoch = inc.repo.DropEpoch
	}
	s.e.mu.Unlock()
	return s.c.persist(ctx, s.e, next, false)
}

// rename gives the staged incarnation its number — one past the highest the
// entry has, current or retiring, past every name repos/ already holds — and
// renames it into repos/, the one step that makes it the repository's
// newest: os.Lstat first, because a rename onto an EMPTY directory succeeds
// silently on both supported platforms, and the next number again where the
// rename finds the name taken. Race-free: only the entry's round and the
// boot create incarnation names, and the caller holds the round. repos/ is
// synced before anything is promoted; a sync that fails fails the swap, and
// the directory — renamed already — is taken back out of repos/ with the
// staging (s.staged names where it is).
func (s *swapping) rename() error {
	s.e.mu.Lock()
	n := 1
	for _, inc := range append([]*incarnation{s.e.current}, s.e.retiring...) {
		if inc != nil && inc.n >= n {
			n = inc.n + 1
		}
	}
	s.e.mu.Unlock()
	repos := filepath.Join(s.c.dir, reposDir)
	for ; ; n++ {
		dir := filepath.Join(repos, s.e.id+"."+strconv.Itoa(n)+".git")
		if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
			if err != nil {
				return err
			}
			continue
		}
		err := os.Rename(s.staged, dir)
		if errors.Is(err, fs.ErrExist) || errors.Is(err, syscall.ENOTEMPTY) {
			continue
		}
		if err != nil {
			return err
		}
		s.staged = dir
		if err = s.c.durableDir(repos); err != nil {
			return err
		}
		s.e.mu.Lock()
		s.next.n, s.next.dir = n, dir
		s.e.mu.Unlock()
		return nil
	}
}

// promote makes the next incarnation the entry's current one in place of
// inc, in three steps. Under e.mu, next is counted into the cache's total and
// inc left counting only the files it holds alone — the leaving packs' and the
// derived files a retirement drops. Then the memo's tree MOVES into next
// whole, one rename however many answers it holds (carryMemo), as a rebuild's
// promote moves it — where no event of what leaves drops it, as an expunge's
// commit does: between the two locked steps, since the memo's lock is
// taken before e.mu, and before inc is retired, since an inc nobody holds goes
// into trash/ at its retirement, its tree with it. Then, under e.mu again,
// every field is carried by its class (swapFields), next is made current, and
// inc is retired, which stops a maintenance run on it (its cadence carried as
// a run), and goes into trash/ with its last reader.
func (s *swapping) promote() {
	c, e, inc, next := s.c, s.e, s.inc, s.next
	e.mu.Lock()
	// THE TOTAL: next counted in — the live packs, the packed-refs and the
	// derived files it linked — and inc from here on only what it holds
	// ALONE: the files of the leaving packs and of the declarations a
	// retirement drops. Every file the two share is counted once, by next,
	// so the budget sees what the swap gives back when inc goes. next holds
	// no memo yet: the move below counts it as it moves it (setMemo).
	next.discarded = false
	c.heldBytes.Add(next.diskBytes + next.metaBytes)
	c.droppingBytes.Add(next.droppingBytes)
	c.setSize(inc, max(inc.diskBytes-s.shared, 0))
	c.setDropping(inc, max(inc.droppingBytes-s.sharedDropping, 0))
	c.image.each(s.carried, func(_ int, d *Derived) {
		if st, ok := inc.derived[d.Name]; ok {
			// A chain's files are shared only where a layer was kept, linked:
			// inc holds its chain files, and every layer the next one does
			// not, alone.
			if d.chained() {
				st.Bytes = max(st.Bytes-s.chainShared[d.Name], 0)
			} else {
				st.Bytes = 0
			}
			c.setDerived(inc, d.Name, st, true)
		}
	})
	e.mu.Unlock()
	// THE MEMO, served from inc until here and from next from here on —
	// unless an event of what leaves drops it (s.keep): an expunge's commit,
	// one of whose answers may quote what it purged, leaves the tree with inc
	// to go with it, as a swap leaves every file a dropping declaration owns.
	// None moves from a memo shut either — a round that dropped it with its
	// history leaves it shut over the packs that history left (apply). Either
	// way next opens a memo of its own.
	if s.keep&c.image.set(DerivedMemo) != 0 {
		c.carryMemo(e, inc, next)
	}
	e.mu.Lock()
	if _, moved := next.derived[DerivedMemo]; moved {
		s.carried |= c.image.set(DerivedMemo)
	}
	for _, f := range swapFields {
		if f.carry != nil {
			f.carry(s)
		}
	}
	// THE TAIL of next, counted afresh from the live set just carried into it
	// and the chain its layer step placed (recordInstalled counted it before
	// the live set was next's). The tip the swap carried keeps the digest of
	// the tail its last write covered, so it stands where next's tail is that
	// tail — the packs leaving are retirements, which take no commit with
	// them (X04) — and is wanted again where it is not: a compaction's own
	// output is a pack the tail gains, whose commits the digest cannot tell
	// from a push's, so the tip is written again at the ordinary cadence over
	// the tail's commits alone — once too often for a merge, never once too
	// few for a push.
	recountTail(next)
	inc.successor = next
	e.current = next
	c.noteChained(e)
	// The backfill's work is the new copy's now (schedule.go), as at a
	// rebuild's promote.
	c.backfillDirty(e)
	gone := c.retire(e, inc)
	e.mu.Unlock()
	c.trash(gone)
}

// swapped records a swap made: code_cache_swapped and
// crewlet.code.cache.swaps, by whether the next incarnation serves from an
// installed chain (layers.go), and otherwise by what became of the
// commit-graph — kept, where the next incarnation linked it; dropped, where
// the copy held one, its own or its chain's graph layers, and the next holds
// none — a stray no retirement accounts for or an expunge took it, or the
// chain was cut whole — which the maintainer writes again at once; none,
// where the copy held none to keep, its graph not yet written or past what
// the node's memory holds, so nothing was lost and nothing is written again.
// Counted as dropped, a node too small for a large repository's graph
// counted every compaction of it among the drops that say whole reads cannot
// see their retirements. The packs dropped are every pack the old
// incarnation held that the next does not: what was leaving, and the packs a
// chain the placement replaced held alone, whose rows had gone.
func (c *Cache) swapped(s *swapping, next *incarnation, took time.Duration) {
	graph := swapGraphNone
	switch rel := c.image.mainRel(DerivedGraph); {
	case len(s.chain) > 0:
		graph = swapGraphChain
	case rel != "" && s.linked[rel]:
		graph = swapGraphKept
	case s.heldGraph:
		graph = swapGraphDropped
	}
	dropped := 0
	s.e.mu.Lock()
	for key := range s.inc.leaving {
		if packChecksum().MatchString(key.name) {
			dropped++
		}
	}
	for cs := range s.inc.installedPacks {
		_, kept := s.packs[cs]
		_, counted := s.inc.leaving[leaveKey{placeLive, cs}]
		if !kept && !counted {
			dropped++
		}
	}
	s.e.mu.Unlock()
	c.log.Info("code_cache_swapped", "repo", s.e.id, "from", s.inc.n, "to", next.n, "packs_kept", len(s.packs),
		"packs_dropped", dropped, "layers", len(s.chain), "graph", graph, "took", took)
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheSwaps, 1, metrics.Attrs{"graph": graph})
	}
	c.setGauges()
}

// link makes newname a hard link of oldname: os.Link, or the suite's link
// (knobs.link) — a case makes one fail.
func (c *Cache) link(oldname, newname string) error {
	if c.knobs != nil && c.knobs.link != nil {
		return c.knobs.link(oldname, newname)
	}
	return os.Link(oldname, newname)
}

// leavingRetired reports whether a retirement took out any of leaving — a
// compaction's, or an expunge's, whose commit retires the packs it takes out
// into code_packs_retired too: what a state's LeavingSince is written for.
func leavingRetired(leaving map[leaveKey]Event) bool {
	for _, ev := range leaving {
		if ev == EventPackRetired || ev == EventPackExpunged {
			return true
		}
	}
	return false
}

// lowest is the lower of two positions on the code log, a zero one being
// none; zero where both are.
func lowest(a, b statelog.Position) statelog.Position {
	switch {
	case a.IsZero():
		return b
	case b.IsZero():
		return a
	}
	if before, err := b.Before(a); err == nil && before {
		return b
	}
	return a
}

// swapClass is how a swap's promote treats one field of an incarnation.
type swapClass string

const (
	// carried: next takes inc's value as it stands at the promote — the
	// repository's, or a record of files next holds as the same inodes.
	carried swapClass = "carried"
	// carriedFiltered: next takes inc's value for the files it holds alone —
	// the live packs, the derived files it linked — and nothing for a file
	// the swap dropped.
	carriedFiltered swapClass = "carried-filtered"
	// notCarried: inc's own — a reader's or a process's of inc's directory,
	// or what goes with it — and next starts from the zero value.
	notCarried swapClass = "not-carried"
	// own: next's own, set by the swap's steps: its number, its directory,
	// the state it wrote.
	own swapClass = "own"
	// recomputed: worked out afresh for next from the live rows and what the
	// swap linked and wrote.
	recomputed swapClass = "recomputed"
)

// swapClasses is every swapClass.
var swapClasses = []swapClass{carried, carriedFiltered, notCarried, own, recomputed}

// swapField is one field of an incarnation as the swap treats it: its class,
// why, and — for a carried one alone — the carry the promote makes, under
// e.mu.
type swapField struct {
	name  string
	class swapClass
	why   string
	carry func(s *swapping)
}

// swapFields is EVERY FIELD OF incarnation, each in exactly one class, in the
// struct's order: the promote carries each carried field by its own carry,
// and TestEveryIncarnationFieldIsClassifiedForTheSwap holds the table to the
// struct both ways, so a field a later part adds is classified before a swap
// can lose it.
var swapFields = []swapField{
	{name: "n", class: own, why: "the number the rename gave it"},
	{name: "dir", class: own, why: "repos/<id>.<n>.git, its own directory"},
	{name: "pins", class: notCarried, why: "each reader's pin is of the directory it holds; next is pinned as " +
		"it is handed out"},
	{name: "retired", class: notCarried, why: "inc's: retired by the promote"},
	{name: "discarded", class: own, why: "true while staged, outside the cache's total; the promote counts it in"},
	{name: "converged", class: carried, why: "inc's own verdict for the round that swapped: next holds what inc " +
		"converged to, less what left",
		carry: func(s *swapping) { s.next.converged = s.inc.converged }},
	{name: "reread", class: carried, why: "a whole read owed, which the next full round answers on next",
		carry: func(s *swapping) { s.next.reread = s.inc.reread }},
	{name: "marks", class: carried, why: "the in-place marks a round converges against",
		carry: func(s *swapping) { s.next.marks = s.inc.marks }},
	{name: "drop", class: carriedFiltered, why: "the drops reports asked of the declarations next carries",
		carry: func(s *swapping) { s.next.drop = s.filterDrops(s.inc.drop) }},
	{name: "packChecks", class: carriedFiltered, why: "a hint's hash of a live pack is of the same inodes",
		carry: func(s *swapping) {
			for cs, check := range s.inc.packChecks {
				if s.keepsPack(cs) {
					if s.next.packChecks == nil {
						s.next.packChecks = map[string]*packCheck{}
					}
					s.next.packChecks[cs] = check
				}
			}
		}},
	{name: "rebuilds", class: carried, why: "the line of rebuild attempts is the repository's, not the " +
		"directory's",
		carry: func(s *swapping) { s.next.rebuilds = s.inc.rebuilds }},
	{name: "unmoved", class: own, why: "false: the swap moved the copy's packs, so no rebuild's read is what " +
		"it holds"},
	{name: "state", class: own, why: "the state the swap wrote into the staged directory"},
	{name: "at", class: carried, why: "the position the rows were read at",
		carry: func(s *swapping) { s.next.at = s.inc.at }},
	{name: "stream", class: carried, why: "the code log the position is on",
		carry: func(s *swapping) { s.next.stream = s.inc.stream }},
	{name: "repo", class: carried, why: "the repository's row as the round read it",
		carry: func(s *swapping) { s.next.repo = s.inc.repo }},
	{name: "refsDigest", class: carried, why: "packed-refs is the same file, linked",
		carry: func(s *swapping) { s.next.refsDigest = s.inc.refsDigest }},
	{name: "refs", class: carried, why: "packed-refs is the same file, linked",
		carry: func(s *swapping) { s.next.refs = s.inc.refs }},
	{name: "packs", class: recomputed, why: "the live packs it linked"},
	{name: "objects", class: recomputed, why: "the live rows' objects"},
	{name: "diskBytes", class: recomputed, why: "the live packs' bundles and the packed-refs it linked (setSize)"},
	{name: "metaBytes", class: recomputed, why: "its derived files' bytes, as setDerived keeps them"},
	{name: "droppingBytes", class: recomputed, why: "the dropping packs' bundles it linked into its dropping view " +
		"(setDropping)"},
	{name: "dropEpoch", class: recomputed, why: "the rows' epoch at the position it reflects once its chain " +
		"holds no pack for a row that is not live, and inc's otherwise (writeState): it holds every pack file " +
		"where they place it, a begin's inputs gone with inc"},
	{name: "derived", class: recomputed, why: "what the swap linked of the declarations it carries, the " +
		"round's index it wrote, and the memo's record, moved with its tree (carryMemo)"},
	{name: "maintained", class: carried, why: "the duty cycle's clock is the repository's: copied whatever the " +
		"declarations drop, a run the swap stopped counted on it as a run (stopRun)",
		carry: func(s *swapping) { s.next.maintained = s.inc.maintained }},
	{name: "graphFailed", class: carried, why: "a graph-first run that failed waits for the ordinary rules " +
		"whichever directory it would write in",
		carry: func(s *swapping) { s.next.graphFailed = s.inc.graphFailed }},
	{name: "vouched", class: carriedFiltered, why: "a link is the same inode: inc's vouch for a live pack's " +
		"index files and a linked derived file stands, beside next's own for what the swap wrote",
		carry: func(s *swapping) { s.carryVouches() }},
	{name: "checkedOnce", class: carried, why: "every file next holds was held to its trailer by a full round " +
		"of this process, or written by it",
		carry: func(s *swapping) { s.next.checkedOnce = s.inc.checkedOnce }},
	{name: "indexRepair", class: carriedFiltered, why: "an index repair is the multi-pack-index's: owed only " +
		"where the swap carries it",
		carry: func(s *swapping) { s.next.indexRepair = s.inc.indexRepair && s.keepsDecl(DerivedMidx) }},
	{name: "repair", class: carriedFiltered, why: "what the repair still owes a file next holds",
		carry: func(s *swapping) { s.next.repair = s.filterRepair(s.inc.repair) }},
	{name: "held", class: notCarried, why: "each reader's held reports are that reader's, keyed on the inc it " +
		"holds: its repair consumes them, and its release hands what is left to the copy serving inc's files " +
		"(handOn) — next, whose scrub answers them"},
	{name: "mapped", class: notCarried, why: "inc's mappings of its runs, held by inc's readers; next maps its " +
		"own"},
	{name: "runHolds", class: notCarried, why: "inc's readers' holds of its mappings"},
	{name: "historyGone", class: carried, why: "a log gone back the next full round drops the index for",
		carry: func(s *swapping) { s.next.historyGone = s.inc.historyGone }},
	{name: "provGen", class: carried, why: "the index next carries is the same index",
		carry: func(s *swapping) { s.next.provGen = s.inc.provGen }},
	{name: "provNext", class: carried, why: "no run of the index takes a number one before it took",
		carry: func(s *swapping) { s.next.provNext = max(s.next.provNext, s.inc.provNext) }},
	{name: "retiredRuns", class: notCarried, why: "inc's runs its index stopped naming, on inc's disk and gone " +
		"with inc's directory: next links only the runs its record names (carryIndex)"},
	{name: "memoMu", class: own, why: "the memo's lock of next's own"},
	{name: "memoListed", class: own, why: "false, or inc's as carryMemo moves the memo's tree in whole as next " +
		"is promoted: the cohorts on next's disk are those inc's record named, listed or not"},
	{name: "memoVersion", class: own, why: "moved by next's own record as carryMemo moves the memo in " +
		"(setMemo), past what the swap's state records, so the janitor writes next's state at its tick"},
	{name: "memoStated", class: own, why: "the memo's version the state the swap wrote records"},
	{name: "memoGen", class: own, why: "zero, as a rebuild's new incarnation starts: a batch is begun on next " +
		"only once it serves, and carryMemo moves a memo only into a copy whose own round has dropped none"},
	{name: "memoShut", class: own, why: "false, open: a memo is shut until the round that dropped it has made " +
		"its change visible (reopenMemo), and next holds exactly the live rows' packs — the change is visible " +
		"in it, whichever round swapped — while a memo shut on inc, which holds what left until its last " +
		"reader lets go, or carried away from it, stays shut there"},
	{name: "written", class: carriedFiltered, why: "when this process last wrote a carried declaration's files, " +
		"the same inodes; next's own write of what the swap wrote stands",
		carry: func(s *swapping) { s.next.written = s.filterTimes(s.inc.written, s.next.written) }},
	{name: "damagedAt", class: carriedFiltered, why: "when a carried declaration's files were last dropped for " +
		"damage",
		carry: func(s *swapping) { s.next.damagedAt = s.filterTimes(s.inc.damagedAt, s.next.damagedAt) }},
	{name: "againLogged", class: carriedFiltered, why: "the carried declarations logged damaged again",
		carry: func(s *swapping) { s.next.againLogged = s.inc.againLogged & s.carried }},
	{name: "memoryRefused", class: carried, why: "the steps the node's memory refuses the repository, which " +
		"its objects decide — beside any the round's index the swap wrote was refused",
		carry: func(s *swapping) { s.next.memoryRefused |= s.inc.memoryRefused }},
	{name: "inherited", class: carried, why: "whether a previous process left the bytes next holds",
		carry: func(s *swapping) { s.next.inherited = s.inc.inherited }},
	{name: "scrubbing", class: notCarried, why: "a scrub running reads inc's files; its verdict lands on the " +
		"incarnation serving them when it ends (serving)"},
	{name: "scrubRunning", class: notCarried, why: "the priority of the scrub running over inc"},
	{name: "scrubDue", class: carried, why: "a scrub due at once is due for the same bytes",
		carry: func(s *swapping) { s.next.scrubDue = s.inc.scrubDue }},
	{name: "scrubAsked", class: carried, why: "as old as it was",
		carry: func(s *swapping) { s.next.scrubAsked = s.inc.scrubAsked }},
	{name: "scrubBase", class: carried, why: "every byte next holds was held to its checksum then, or placed " +
		"since by this process",
		carry: func(s *swapping) { s.next.scrubBase = s.inc.scrubBase }},
	{name: "scrubbedAt", class: carried, why: "the cooldown runs from the scrub of the same bytes",
		carry: func(s *swapping) { s.next.scrubbedAt = s.inc.scrubbedAt }},
	{name: "scrubFound", class: carried, why: "whether that scrub found anything",
		carry: func(s *swapping) { s.next.scrubFound = s.inc.scrubFound }},
	{name: "scrubBegan", class: carried, why: "when that scrub began, on the monotonic clock",
		carry: func(s *swapping) { s.next.scrubBegan = s.inc.scrubBegan }},
	{name: "scrubEnded", class: carried, why: "when that scrub ended, on the monotonic clock",
		carry: func(s *swapping) { s.next.scrubEnded = s.inc.scrubEnded }},
	{name: "scrubWhole", class: carriedFiltered, why: "the live packs that scrub found whole",
		carry: func(s *swapping) {
			s.next.scrubWhole = slices.DeleteFunc(slices.Clone(s.inc.scrubWhole), func(cs string) bool {
				return !s.keepsPack(cs)
			})
		}},
	{name: "repairWrite", class: carried, why: "a repair written into a file next holds, the guard of a request " +
		"that may have failed on what it replaced",
		carry: func(s *swapping) { s.next.repairWrite = s.inc.repairWrite }},
	{name: "forcedAt", class: carried, why: "a forced check's verdict on the same inodes",
		carry: func(s *swapping) { s.next.forcedAt = s.inc.forcedAt }},
	{name: "forcedEnded", class: carried, why: "the same, on the monotonic clock",
		carry: func(s *swapping) { s.next.forcedEnded = s.inc.forcedEnded }},
	{name: "lastUsed", class: carried, why: "the repository's last use orders the budget: the state the swap " +
		"wrote carries it, and a use of inc since is carried at the promote",
		carry: func(s *swapping) { s.next.lastUsed = s.inc.lastUsed }},
	{name: "stamped", class: own, why: "the last use the state the swap wrote was stamped with (writeState), " +
		"never the moment of the write: a later use the janitor stamps at its tick"},
	{name: "live", class: carried, why: "the live rows are what next holds",
		carry: func(s *swapping) { s.next.live, s.inc.live = s.inc.live, nil }},
	{name: "byChecksum", class: carried, why: "the live rows' checksums are next's files",
		carry: func(s *swapping) { s.next.byChecksum, s.inc.byChecksum = s.inc.byChecksum, nil }},
	{name: "droppingPacks", class: carried, why: "the packs the live set carried places in the dropping view, " +
		"which next holds there: what its readers are handed the view by (droppingView)",
		carry: func(s *swapping) { s.next.droppingPacks = s.inc.droppingPacks }},
	{name: "leaving", class: recomputed, why: "empty: the swap took out every file that was leaving"},
	{name: "leavingSince", class: recomputed, why: "zero: nothing is leaving next"},
	{name: "successor", class: notCarried, why: "inc's: the promote points it at next"},
	{name: "chain", class: carried, why: "the rows' published chain as the round read it, which no placement moves",
		carry: func(s *swapping) { s.next.chain = s.inc.chain }},
	{name: "chainRead", class: carried, why: "whether a round of this process served the rows' chain",
		carry: func(s *swapping) { s.next.chainRead = s.inc.chainRead }},
	{name: "chainGen", class: recomputed, why: "one past inc's (placeLayers): a placement planned over inc finds " +
		"the copy moved"},
	{name: "installed", class: recomputed, why: "the chain the layer step placed or carried (recordInstalled)"},
	{name: "installedPacks", class: recomputed, why: "the packs that chain names, at the figures the swap linked " +
		"them by (recordInstalled)"},
	{name: "refused", class: carried, why: "a layer refused while the rows name it at its record is refused for " +
		"the repository: the next copy would meet the same bytes — a canary that abandoned the swap's own " +
		"placement among them",
		carry: func(s *swapping) { s.next.refused = maps.Clone(s.inc.refused) }},
	{name: "tip", class: recomputed, why: "the layer step's: inc's tip linked where the whole chain beneath it " +
		"crosses and every pack leaving is a retirement (chainOf), else none"},
	{name: "tipOver", class: recomputed, why: "the layer step's: the digest of the tail inc's last tip write " +
		"covered, with the tip it describes, where that tip crosses; else none, which the maintainer's next " +
		"pass answers"},
	{name: "tipWritten", class: recomputed, why: "the layer step's, with tipOver"},
	{name: "tipFailed", class: carried, why: "a tip-first run that failed waits for the ordinary rules whichever " +
		"directory it would write in",
		carry: func(s *swapping) { s.next.tipFailed = s.inc.tipFailed }},
	{name: "tail", class: recomputed, why: "counted afresh at the promote from the live set it carries and the " +
		"chain the layer step placed (recountTail)"},
	{name: "chainSeq", class: own, why: "next's chain files are the swap's, written before anybody read them in " +
		"place: no reader of next began its read before them"},
	{name: "chainWriting", class: own, why: "none: no write of next's chain files in place is in flight as it " +
		"is promoted"},
}

// filterDrops is drops for the declarations the swap carries.
func (s *swapping) filterDrops(drops derivedDrops) derivedDrops {
	var out derivedDrops
	for name, ask := range drops {
		if s.keepsDecl(name) {
			out.add(name, ask.began, ask.by)
		}
	}
	return out
}

// filterTimes is the times of the declarations the swap carries, beside
// next's own, which stand.
func (s *swapping) filterTimes(from, mine map[DerivedName]time.Time) map[DerivedName]time.Time {
	out := maps.Clone(mine)
	for name, at := range from {
		if _, own := out[name]; own || !s.keepsDecl(name) {
			continue
		}
		if out == nil {
			out = map[DerivedName]time.Time{}
		}
		out[name] = at
	}
	return out
}

// filterRepair is what inc's repair mark still owes a file next holds: the
// drops of the declarations carried, the findings and the reinstalls of the
// files and the live packs it holds, the downloads held off of the live
// packs, and the forced checks owed, which hold every index file next holds.
func (s *swapping) filterRepair(m repairMark) repairMark {
	out := repairMark{drops: s.filterDrops(m.drops), forces: m.forces}
	for rel, f := range m.found {
		if s.holds(rel) {
			if out.found == nil {
				out.found = map[string]checkedFile{}
			}
			out.found[rel] = f
		}
	}
	for cs, f := range m.reinstall {
		if s.keepsPack(cs) {
			if out.reinstall == nil {
				out.reinstall = map[string]checkedFile{}
			}
			out.reinstall[cs] = f
		}
	}
	for cs, b := range m.fetchFailed {
		if s.keepsPack(cs) {
			if out.fetchFailed == nil {
				out.fetchFailed = map[string]fetchBackoff{}
			}
			out.fetchFailed[cs] = b
		}
	}
	return out
}

// carryVouches carries inc's vouches for the files next holds as the same
// inodes — a live pack's .idx and .rev, a linked derived file — beside next's
// own, for what the swap wrote, which stand.
func (s *swapping) carryVouches() {
	for key, v := range s.inc.vouched.packs {
		if !s.keys[key] {
			continue
		}
		if s.next.vouched.packs == nil {
			s.next.vouched.packs = map[[hashTrailer]byte]packVouch{}
		}
		s.next.vouched.packs[key] = v
	}
	for rel, stamp := range s.inc.vouched.derived {
		if _, own := s.next.vouched.derived[rel]; own || !s.linked[rel] {
			continue
		}
		if s.next.vouched.derived == nil {
			s.next.vouched.derived = map[string]fileStamp{}
		}
		s.next.vouched.derived[rel] = stamp
	}
}
