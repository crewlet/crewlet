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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The swap (package doc, "A pack leaves a copy a reader may hold by a swap";
// ADR-0090): the next incarnation is staged under scratch/ by hard links, made
// durable as ONE step of durable.go's rule (syncStaged), and renamed in. A
// derived file whose declaration drops on an event of what leaves is never
// linked, so the drop precedes the change (dropFor's order rule); a round that
// crosses an expunge's begin and commit takes the purged packs out as the
// commit's (sync.go's crossedACommit), never as a retirement. What is learnt
// of the old copy's files afterwards lands on the copy serving them (serving,
// observe, handOn, swapAt, swappedAnswer). The rename is the one irreversible
// step; gitletfault.SyncSwapBeforeRename is the moment before it.

// The graph attribute of crewlet.code.cache.swaps: what became of the left
// copy's commit-graph (swapped), a next incarnation serving a chain, or a
// failed swap. CHAIN is its own value: a chained copy holds no whole graph and
// the indexing writes none beside the chain (layers.go), so "dropped" would
// read as a graph owed, and "none" would hide that every placement on a held
// copy is a swap.
const (
	swapGraphKept    = "kept"
	swapGraphDropped = "dropped"
	swapGraphNone    = "none"
	swapGraphChain   = "chain"
	swapFailed       = "failed"
)

// swapLeaving swaps inc, the entry's current incarnation, into its next when
// packs are leaving it or step places its chain (layers.go), and answers the
// incarnation current on return: the next, or inc after a failed swap, its
// leaving kept for the next round. The caller holds the entry's write lock for
// its round or placement. snap is the snapshot the round APPLIED (a full one's
// index is written over the next incarnation), or zero where it applied none.
//
// A placement whose canary finds the chain unsound is abandoned, its added
// layers refused as corrupt, and the swap made again for the packs leaving
// with the held chain as it stands, so one bad layer undoes none of the round.
// A canary that read nothing is no verdict and fails the swap (layers.go's
// canary). What the placement fetched and no swap placed goes from tmp/.
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
			if slices.ContainsFunc(bad.added, func(r gitlet.LayerRow) bool { return sameLayer(r, st.row) }) {
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
	added []gitlet.LayerRow
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
	// Every layer the swap was to place failed with it, counted once a try as a
	// failed fetch is: it may pass next time (the installer retries). A
	// canary's verdict is no try: its refusal counts it (swapLeaving).
	var bad *canaryFailed
	if s.layers != nil && !errors.As(err, &bad) {
		for _, st := range s.layers.fetched {
			c.observeInstall(layerFailed, time.Since(st.began))
		}
		c.countLayers(layerFailed, len(s.layers.fetched))
	}
	if errors.Is(err, fs.ErrNotExist) && (s.step == stepPacks || s.step == stepRoot) {
		// A file the swap links is gone from the copy's own directory, which
		// no retry ends: the next round reads the copy whole, putting a live
		// pack back with one GET (install's lost) or a root file anew (tidy,
		// writeRefs), and swaps.
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

// stagedName is where under scratch/ a swap of repository id stages:
// swap.<id>.<v7>.git, the id so a git process's report there finds the
// repository (swapAt), a fresh v7 so no two stagings, a crashed one's among
// them, share a name.
func stagedName(id string) string { return "swap." + id + "." + newV7() + ".git" }

// swapAt is the entry whose swap stages at dir and the incarnation it leaves,
// or nil. A git process in the staged directory (the round's index, step 5)
// reads the very inodes the leaving copy serves, so its report is about that
// copy, and the promote carries what that copy makes of it.
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

// swappedOut reports whether a swap replaced inc and the entry's current
// incarnation serves its files now (serving), the same inodes for each kept.
// One a rebuild replaced is not (its successor was read whole and checked),
// nor one of a purged or evicted repository. e.mu is held.
func swappedOut(e *entry, inc *incarnation) bool {
	return !e.dead && e.current != nil && inc != e.current && serving(inc) == e.current
}

// swapping is one swap in flight: the incarnation it leaves, the one it
// builds, and what it keeps and has done, which the promote's carries
// (swapFields) read.
type swapping struct {
	c    *Cache
	e    *entry
	inc  *incarnation
	snap snapshot
	// layers is the placement the swap makes of the installed chain
	// (layers.go), or nil to carry inc's chain whole (the chain holds its
	// packs). chain is the chain next holds once the layer step ran, added its
	// layers inc lacked (which the canary reads), and chainShared the bytes of
	// each chain declaration's files both incarnations hold, linked.
	layers      *layerStep
	chain       []gitlet.LayerRow
	added       []gitlet.LayerRow
	chainShared map[DerivedName]int64
	// tip is the node's tip graph layer next carries, linked, with the tail its
	// last write covered (tipOver, tipWritten): inc's where the whole chain
	// beneath it crosses and every pack leaving is a retirement (tipGoes),
	// else none, as for a placement, which moves the chain beneath it.
	tip        tipLayer
	tipOver    tailDigest
	tipWritten bool
	// placedLayers is what the layer step renamed in, keptLayers how many it
	// linked, cutLayers how many of inc's layers next carries no file of;
	// counted once the swap is made, as cutChain counts a cut in place.
	placedLayers []*stagedLayer
	keptLayers   int
	cutLayers    int
	// wrote is every file the layer step wrote or renamed in rather than
	// linked, relative to the staged root, whose directories syncStaged syncs.
	wrote []string

	// next is the incarnation being built, at staged until the rename numbers
	// it. stagedAt is the directory as step 1 named it, never moved, which
	// swapAt matches. step is the step in flight, which a failure names.
	next     *incarnation
	staged   string
	stagedAt string
	step     string

	// packs is the live set's packs by checksum (what next holds), places
	// where each lies (packPlace: objects/pack, or the dropping view for a pack
	// only dropping rows name), keys the same checksums as a vouch keys them.
	// keep is the declarations no event of what leaves drops; carried those it
	// linked files of, plus the memo once moved (carryMemo). linked is every
	// derived file it linked, relative to the incarnation.
	packs   map[string]livePack
	places  map[string]packPlace
	keys    map[[hashTrailer]byte]bool
	keep    derivedSet
	carried derivedSet
	linked  map[string]bool
	// events is every event that took a pack file out, refsBytes the linked
	// packed-refs' size, shared the bytes both hold (live packs, packed-refs),
	// sharedDropping the dropping view's part. heldGraph: the left copy held a
	// commit-graph (own, or its chain's graph layers) once nothing could write
	// one, which tells a dropped graph from none to keep (swapped).
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
	// 0. THE CHAIN next holds, decided before anything is staged (chainOf):
	// the placement's, or inc's cut where a pack leaving names a layer
	// (prefixOf); every pack it names joins what next holds, live or not.
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

	// 1. STAGE under scratch/: a crash before the rename must leave the old
	// incarnation the highest in repos/ (Open empties scratch/), and the
	// janitor counts scratch/ by sole names, so the staged links count only
	// what they alone hold. From here the swap is the entry's (entry.swap)
	// until swapLeaving ends it.
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
	// Staged, next stands outside the cache's total (discarded), so no step
	// moves the total and a failed swap takes nothing back: the promote counts
	// it in.
	s.next = &incarnation{dir: s.staged, discarded: true}

	// 2. THE PACKS, largest first: .pack, .rev, then .idx, so git sees a pack
	// only once whole; each linked into its place, a dropping one into next's
	// dropping view, made for the first (a next holding none has none).
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

	// 3. THE ROOT'S FILES, LINKED: none is ever written in place (tidy and
	// writeRefs stage and rename), so a link is a snapshot; written afresh,
	// HEAD and config would cost two files staged and synced every swap.
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

	// 4. THE DERIVED FILES kept, once nothing writes one in the left copy: its
	// maintenance run stopped (stopRun), its index of pushes taking no commit
	// while the swap is the entry's (commitIndex).
	s.step = stepDerived
	s.stopRun()
	if rel := c.image.mainRel(DerivedGraph); rel != "" {
		_, gerr := os.Lstat(filepath.Join(inc.dir, rel))
		s.heldGraph = gerr == nil
	}
	if _, graph := chainIDs(installed); len(graph) > 0 || hadTip {
		// A chain's graph layers and the tip are the copy's commit-graph too:
		// a next with no chain and no graph dropped them.
		s.heldGraph = true
	}
	if err = s.carryDerived(); err != nil {
		return nil, err
	}

	// 4b. THE INSTALLED CHAIN (placeLayers): kept layers linked, fetched ones
	// renamed in, both chain files after the layers' so neither names a file
	// not there; no directory synced here (step 6).
	s.step = stepLayers
	if err = s.placeLayers(); err != nil {
		return nil, err
	}

	// 5. A FULL ROUND'S INDEX over exactly the live packs, in the staged
	// directory and the round's read lane, vouched as written; its git's
	// reports reach the copy it reads (swapAt). None for a delta or beside a
	// chain (indexRound's indexed). Never a swap failure: a round answers
	// without its index, slower and never wrong.
	if s.snap.full {
		c.indexRound(ctx, e, s.next, s.snap)
	}

	// 5b. THE CANARY (layers.go), where the placement added a layer: one
	// bitmapped walk of the staging before the rename makes the chain visible,
	// since a layer over the wrong base passes its own files' checks. An
	// unsound verdict abandons the placement (swapLeaving) before the staging
	// is synced; a canary that read nothing (no slot within SlotWait, its
	// deadline) fails the swap as a disk write would, to be tried again. The
	// installer's canary walks in its share of the read lane (takeInstallRead),
	// never behind its checks' hashing, since this swap holds the write lock
	// the repository's rounds wait for; a round's in the lane as its others.
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
		if err = c.faults.Hit(ctx, gitletfault.SyncSwapBeforeRename, 0); err != nil {
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

// stopRun stops a maintenance run on the left incarnation and waits for it (as
// stopMaintenance does), so nothing writes a derived file the swap links. The
// stop counts as a run of the duty cycle where the run counts (maintRun.duty):
// a run ended from outside records no end (maintain), and a repository
// compacted more often than its index run lasts would otherwise have every
// run stopped and the next due at once, a slot held for ever writing nothing.
// A run that ended by itself meanwhile keeps its own end. Carried by
// swapFields, and kept by a failed swap: the run was stopped either way.
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

// carryDerived links into the staging the files of every declaration the swap
// keeps, with each one's record: by its patterns (carryFiles), or by its
// record for the index of pushes (carryIndex). A tree a declaration owns (the
// memo's) is never linked but MOVES at the promote, one rename (carryMemo,
// SeedMove), where links would cost a stat and a link an answer while the
// round holds the repository. syncStaged syncs the directories.
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
			// Placed layer by layer by placeLayers, never by its patterns,
			// which would carry a layer the chain leaves and the file naming it.
		case d.Name == DerivedCodeIndex:
			carried, err = s.carryCodeIndex(d)
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

// syncStaged syncs ONCE each directory of the staging a link or write landed
// in (objects/pack, each linked derived file's, the chain's), before the
// state; the state's write syncs the root last (writeState). Nothing reads the
// staging before the rename, and a crash leaves it in scratch/ for the boot to
// move aside, so one sync at the end suffices: each sync more is a journal
// commit while the round holds the repository, readers waiting on it.
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

// chainOf decides the chain next holds (the placement's rows, or inc's cut to
// the prefix the leaving packs leave valid: prefixOf) and the layers of it inc
// lacks, which the canary reads; and adds every pack that chain names to
// s.packs at inc's figures, live row or not (the chain holds its packs). A
// named pack inc holds neither live nor for its chain fails the swap: a layer
// is valid only over every pack it names. The tip crosses only where the whole
// chain beneath it does and every pack leaving is a retirement (tipGoes).
// installed, held and leaving are inc's chain, chain packs and leaving files,
// read under e.mu.
func (s *swapping) chainOf(installed []gitlet.LayerRow, held map[string]livePack, leaving map[string]bool) error {
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
	// CUT: every layer of inc whose files next does not carry (outside its
	// chain, or fetched again over failed files), counted as cutChain counts.
	refetched := map[string]bool{}
	if s.layers != nil {
		for _, st := range s.layers.fetched {
			refetched[st.row.Layer.LayerID] = true
		}
	}
	s.cutLayers = 0
	for _, r := range installed {
		if refetched[r.Layer.LayerID] ||
			!slices.ContainsFunc(s.chain, func(n gitlet.LayerRow) bool { return sameLayer(n, r) }) {
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

// placeLayers is the layer step: each layer of next's chain renamed in from
// tmp/ where the placement fetched it, else linked from inc; the tip linked
// where it crosses (chainOf); both chain files written, the graph's ending in
// the tip; next's record set (recordInstalled). Nothing is synced here:
// syncStaged syncs the directories once, sparing a chained copy four
// directory syncs a swap. A written file is vouched (this process wrote it
// from files held to their rows); a linked one carries inc's (carryVouches).
// Bytes both hold, linked, count by declaration in chainShared, which the
// promote takes out of inc's total.
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
		// THE TIP, linked; gone from inc (a run ended before stopRun wrote
		// another), carried as none, which the maintainer writes at once.
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

// carryFiles links every regular file d owns in inc into the same place in the
// staging and records d there as inc does, at the bytes linked; a part linked
// beside no main file is counted, never read as made (recordDerived's rule).
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

// carryIndex links every run of inc's index of pushes into the staging and
// carries its record, which takes no commit while the swap is the entry's
// (commitIndex), so every run it names is on disk; backfill work on inc from
// then on is next's to redo. An index whose log is gone (historyGone), or one
// missing a run, carries nothing and the swap goes on: next's backfill reads
// it again, where failing would fail every swap of the copy until a full
// round, keeping retired packs for as long.
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

// writeState writes the staged incarnation's state from inc's records and
// what the swap counted of next. It carries the copy's LAST USE (writeState's
// stampUse): a swap is no use, and a state stamped at its write would bring a
// copy nobody had read in a week back from a restart as the copy read last.
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
	// THE EPOCH ITS OBJECTS/ HOLDS (Synced.DropEpoch): the rows', since it
	// holds every pack file where they place it, once its chain holds no pack
	// for a row not live (liveChain); inc's otherwise.
	next.dropEpoch = inc.dropEpoch
	if liveChain(inc, s.chain) {
		next.dropEpoch = inc.repo.DropEpoch
	}
	s.e.mu.Unlock()
	return s.c.persist(ctx, s.e, next, false)
}

// rename numbers the staging one past every incarnation the entry has and
// every name repos/ holds, and renames it in. It Lstats first, because a
// rename onto an EMPTY directory succeeds silently on both supported
// platforms, and tries the next number where the name is taken. Race-free:
// only the entry's round and the boot create incarnation names, and the
// caller holds the round. repos/ is synced before any promote; a failed sync
// fails the swap, s.staged naming the renamed directory to take back out.
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

// promote makes next the entry's current incarnation in three steps. Under
// e.mu, next is counted into the cache's total and inc left counting only what
// it holds alone. Then the memo's tree moves into next whole (carryMemo) where
// nothing leaving drops it: outside e.mu, since the memo's lock is taken
// before e.mu, and before inc retires, since a retired inc nobody holds goes
// into trash/ with its tree. Then, under e.mu, every field is carried by its
// class (swapFields), next made current and inc retired.
func (s *swapping) promote() {
	c, e, inc, next := s.c, s.e, s.inc, s.next
	e.mu.Lock()
	// THE TOTAL: every file both share counted once, by next, so the budget
	// sees what the swap gives back when inc goes. next holds no memo yet:
	// the move below counts it (setMemo).
	next.discarded = false
	c.heldBytes.Add(next.diskBytes + next.metaBytes)
	// The linked code index joins the indexes' own total too (setDerived
	// counted nothing while next was discarded), as inc's count goes to zero
	// below, so the janitor's share sees it once.
	c.codeIndexBytes.Add(next.derived[DerivedCodeIndex].Bytes)
	c.droppingBytes.Add(next.droppingBytes)
	c.setSize(inc, max(inc.diskBytes-s.shared, 0))
	c.setDropping(inc, max(inc.droppingBytes-s.sharedDropping, 0))
	c.image.each(s.carried, func(_ int, d *Derived) {
		if st, ok := inc.derived[d.Name]; ok {
			// Shared only where a layer was kept: inc holds alone its chain
			// files and every layer next does not.
			if d.chained() {
				st.Bytes = max(st.Bytes-s.chainShared[d.Name], 0)
			} else {
				st.Bytes = 0
			}
			c.setDerived(inc, d.Name, st, true)
		}
	})
	e.mu.Unlock()
	// THE MEMO moves unless an event of what leaves drops it (s.keep): an
	// expunge's commit, whose answers may quote what it purged. None moves
	// from a memo shut by its round either (apply). Else next opens its own.
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
	// THE TAIL, recounted now the live set is next's (recordInstalled counted
	// it before). The carried tip keeps its tail digest: it stands where
	// next's tail is that tail (retirements take no commit, X04), and is
	// rewritten at the ordinary cadence where a compaction's output joined it,
	// since the digest cannot tell that from a push: once too often for a
	// merge, never once too few for a push.
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

// swapped records a swap made (code_cache_swapped, crewlet.code.cache.swaps)
// by its graph: chain where next serves an installed chain; kept where next
// linked the graph; dropped where the copy held one (own or chain graph
// layers) and next holds none, which the maintainer writes again at once;
// none where the copy held none to keep (unwritten, or past the node's
// memory). Counted as dropped, every compaction of a repository too large for
// the node would count among the drops that say whole reads miss their
// retirements. The packs dropped are every pack inc held that next does not:
// what was leaving, and the packs a replaced chain held alone.
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

// swapFields is EVERY FIELD OF incarnation, each in one class, in the
// struct's order; TestEveryIncarnationFieldIsClassifiedForTheSwap holds it to
// the struct both ways, so a new field is classified before a swap can lose it.
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
	{name: "tagsFailed", class: carried, why: "a tag step that failed waits its retry whichever directory it " +
		"would write in", carry: func(s *swapping) { s.next.tagsFailed = s.inc.tagsFailed }},
	{name: "tagsBad", class: carried, why: "the file is linked, the same inode, logged once",
		carry: func(s *swapping) { s.next.tagsBad = s.inc.tagsBad }},
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
	{name: "codeIndex", class: notCarried, why: "inc's code index as its readers opened it; next opens the " +
		"record carryCodeIndex links in at its first reader"},
	{name: "codeIndexOpening", class: notCarried, why: "a reader's opening of inc's files, which ends as " +
		"inc's; next's readers open next's"},
	{name: "codeIndexRefs", class: carried, why: "packed-refs is the same file, linked, and carryCodeIndex " +
		"carries the record the refs name; one it does not carry names no record of next's",
		carry: func(s *swapping) { s.next.codeIndexRefs = s.inc.codeIndexRefs }},
	{name: "memoMu", class: own, why: "the memo's lock of next's own"},
	{name: "memoListed", class: own, why: "false, or inc's as carryMemo moves the memo's tree in whole as next " +
		"is promoted: the cohorts on next's disk are those inc's record named, listed or not"},
	{name: "memoUnlisted", class: own, why: "false, or inc's as carryMemo moves the memo's tree in whole beside " +
		"memoListed: a tree whose last listing failed is the same tree on the same disk in next, so next is " +
		"listed again at the janitor's tick alone, as inc was"},
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
	{name: "publishRefused", class: carried, why: "the publication of the repository the node's memory " +
		"refused, which its objects decide whichever directory serves them",
		carry: func(s *swapping) { s.next.publishRefused = s.inc.publishRefused }},
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
	{name: "droppingObjects", class: carried, why: "the objects of those packs, which next holds there: what its " +
		"connectivity check is charged for beside its objects (verifyCharge)",
		carry: func(s *swapping) { s.next.droppingObjects = s.inc.droppingObjects }},
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
