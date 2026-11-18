package cache

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE INSTALL (package doc, "The byte path is shaped for S3"): one GET a pack
// BUNDLE, split into its .pack, .idx and .rev as it streams, held to its row
// (objstore's SHA-256, then gitlet.CheckBundle) and renamed in. No index-pack:
// the receiving node's already indexed these bytes, and an .idx is a pure
// function of its pack, so re-indexing would make a cold sync CPU-bound.
// The round hands packs out LARGEST FIRST, staging each bundle's files in its
// order (stageBundle); each worker waits for its TURN, then the round's share
// and a node slot, downloads, and places the pack (placePack). The slot, the
// share and the slot's copy buffer are held for the GET alone; the disk work
// after it is bounded by the packs IN HAND (installsPerFetch).

// copyBuffer is the buffer one download is copied through: 256 KiB, so a
// 150 KB bundle is one read. One a node slot ([fetchSlots]), so NodeFetches
// of them (8 MiB) is all the node's download memory.
const copyBuffer = 256 << 10

// fetchSlots is the node's downloads: NodeFetches slots, each handed out with
// its copy buffer, and the packs the node holds IN HAND, installsPerFetch a
// slot, each taken before its GET and given back once the pack is placed or
// abandoned.
type fetchSlots struct {
	sem *semaphore.Weighted
	// free holds the buffers no slot holds, capped at the slots, so buffers
	// never outnumber them.
	free chan []byte
	// out is the buffers handed out now, most the most at once, and waiting
	// the downloads waiting for a slot: what the suite holds to the bound.
	out, most, waiting atomic.Int64
	// hands bounds the node's packs in hand; held, mostHeld and handWaiting
	// count them as out, most and waiting count the buffers.
	hands                       *semaphore.Weighted
	held, mostHeld, handWaiting atomic.Int64
}

func newFetchSlots(n int64) *fetchSlots {
	return &fetchSlots{
		sem: semaphore.NewWeighted(n), free: make(chan []byte, n),
		hands: semaphore.NewWeighted(installsPerFetch * n),
	}
}

// hold waits for a pack in hand: one the node may be downloading or have
// downloaded and not yet placed.
func (s *fetchSlots) hold(ctx context.Context) error {
	s.handWaiting.Add(1)
	err := s.hands.Acquire(ctx, 1)
	s.handWaiting.Add(-1)
	if err != nil {
		return err
	}
	raise(&s.mostHeld, s.held.Add(1))
	return nil
}

// letGo gives a pack in hand back.
func (s *fetchSlots) letGo() {
	s.held.Add(-1)
	s.hands.Release(1)
}

// raise sets most to n when n is larger.
func raise(most *atomic.Int64, n int64) {
	for {
		m := most.Load()
		if n <= m || most.CompareAndSwap(m, n) {
			return
		}
	}
}

// take waits for a slot, then hands it out with a buffer: the slot FIRST, so a
// download waiting for one holds no memory.
func (s *fetchSlots) take(ctx context.Context) ([]byte, error) {
	s.waiting.Add(1)
	err := s.sem.Acquire(ctx, 1)
	s.waiting.Add(-1)
	if err != nil {
		return nil, err
	}
	var buf []byte
	select {
	case buf = <-s.free:
	default:
		buf = make([]byte, copyBuffer)
	}
	raise(&s.most, s.out.Add(1))
	return buf, nil
}

// give takes a slot and its buffer back.
func (s *fetchSlots) give(buf []byte) {
	s.out.Add(-1)
	select {
	case s.free <- buf:
	default:
	}
	s.sem.Release(1)
}

// installsPerFetch is how many packs a round, and the node, hold IN HAND per
// GET they may run at once: from being handed to a worker until renamed into
// place or abandoned.
//
// TWO, so a GET never waits on this node's disk: while one share's bundles
// are read, the share before is synced and renamed in. A share brings a
// bundle every 2 ms from the S3 fake's 30 ms first byte, 6 to 12 ms from S3's
// own, against 1.1 to 1.5 ms of this node's work a bundle (package doc,
// "Measured"); a disk slower than its link holds the GETs back rather than
// growing a backlog. It bounds tmp/ to 2 × FetchConcurrency bundles a round
// and 2 × NodeFetches a node, and the syncs in flight to three a pack in
// hand, however many rounds a joining node starts while its disk stalls.
const installsPerFetch = 2

// The modes an installed pack's files take: the .pack read-only to the node
// (nothing writes a pack in place), the indexes as git leaves the ones it
// writes.
const (
	packMode  os.FileMode = 0o400
	indexMode os.FileMode = 0o444
)

// install brings into inc every pack snap names that the directory does not
// hold whole, and stages every layer of chain.want, answering what it did.
// A pack is held once its three files exist and, on a FULL round, each is at
// its row's size, so a file a disk fault truncated is caught here, not by a
// client. A pack held damaged is installed again by renaming over its files,
// never removed first, dropping the derived files only where the damage may
// have built them (reinstallWhy); a reinstall whose download fails is logged
// and backed off (repairMark.fetchFailed), never the round's failure (package
// doc, "Damage is repaired once, in place where nothing is lost"). Any other
// failure removes the round's temporary files; packs placed stay, named by
// rows. Each pack goes where its rows place it once the round has applied
// (placesAfter; package doc, "The dropping view").
func (c *Cache) install(ctx context.Context, e *entry, inc *incarnation, snap snapshot, ck checked,
	chain chainPlan) (installs, error) {

	var out installs
	reinstall, _ := ck.still(c.image, inc.dir)
	var todo []gitlet.PackRow
	// over is every pack placed over files present, by checksum, and why
	// (reinstallWhy). absent is every pack placed that the directory LOST
	// (lost): a REPAIR whichever round puts it back, since whatever puts it
	// back ends the failure its loss caused (incarnation.repairWrite); were it
	// counted only by a forced round, a request whose git began before another
	// round restored it got a forced round, a scrub and a rebuild for nothing. A
	// pack the copy never held is a push not yet read, and repairs nothing.
	over := map[string]reinstallWhy{}
	absent := map[string]bool{}
	now := c.now()
	e.mu.Lock()
	before := inc.state
	heldOff := inc.repair.heldOff(now)
	// Where the live set places each pack once the round has applied snap.
	places := placesAfter(inc, snap)
	e.mu.Unlock()
	for _, p := range snap.packs {
		// Rows sharing a checksum name one set of files, placed together.
		place := places[p.GitChecksum]
		have, err := installed(filepath.Join(inc.dir, place.rel()), p, snap.full)
		f, failed := reinstall[p.GitChecksum]
		switch {
		case err != nil:
			return out, fmt.Errorf("cache: sync %s: install: %w", e.id, err)
		case have == packWhole && !failed:
			continue
		case have != packAbsent && heldOff[p.GitChecksum]:
			// Its last reinstall's download failed and its backoff has not
			// passed: left as it stands, its finding on the mark.
			continue
		case have == packWhole && f.hinted:
			over[p.GitChecksum] = reinstallWhy{found: "hint", trigger: f.trigger,
				drops: f.index || !c.indexVouched(e, inc, place, p.GitChecksum)}
		case have == packWhole:
			over[p.GitChecksum] = reinstallWhy{found: "trailer", trigger: f.trigger, drops: f.index}
		case have == packWrongSize:
			over[p.GitChecksum] = reinstallWhy{found: "size", trigger: ck.trigger, drops: true}
		case lost(before, snap, p) && !heldAt(inc.dir, place.other(), p):
			// Lost, not MOVED: a pack its rows moved between objects/pack
			// and the dropping view is installed at its new place, no repair.
			absent[p.GitChecksum] = true
		}
		todo = append(todo, p)
	}
	if len(todo) == 0 && len(chain.want) == 0 {
		return out, nil
	}
	// dropOnce drops what the declarations drop for EventPackRemoved before
	// the first placement over a present pack, since what named its old files
	// names bytes gone; addOnce does so for EventPackAdded before the first pack
	// added. No declaration drops on that today, but what an event drops is
	// the declaration's to decide, never a call site's.
	var droppedOver atomic.Bool
	var dropped derivedSet
	dropOnce := sync.OnceValue(func() error {
		s, err := c.dropFor(e, inc, EventPackRemoved, eventDetail{})
		if err != nil {
			return fmt.Errorf("cache: sync %s: install: %w", e.id, err)
		}
		dropped = s
		droppedOver.Store(true)
		return nil
	})
	addOnce := sync.OnceValue(func() error {
		if _, err := c.dropFor(e, inc, EventPackAdded, eventDetail{}); err != nil {
			return fmt.Errorf("cache: sync %s: install: %w", e.id, err)
		}
		return nil
	})
	slices.SortFunc(todo, func(a, b gitlet.PackRow) int {
		return cmp.Or(cmp.Compare(b.Size, a.Size), cmp.Compare(a.PackID, b.PackID), cmp.Compare(a.Part, b.Part))
	})

	// Per pack: the round's pack in hand, then the node's; the round stages
	// its files (stageBundle) and starts a worker, which waits for its TURN,
	// then the round's share and a node slot, held for the GET alone
	// (download), then places the pack. The packs in hand bound goroutines
	// and files and go back once it is placed or abandoned (letGo). The first
	// failure ends rctx, so the round hands out nothing more.
	//
	// THE ROUND KEEPS ITS ORDER, as the scheduler runs the newest goroutine
	// first: a worker asks only once the one before holds its share and slot,
	// costing no GET (both are first come, first served), and the round makes
	// the files, or its first GET waits for files a directory made last, one
	// at a time (BenchmarkColdSync: no slower).
	rctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var failOnce sync.Once
	var failed error
	fail := func(err error) {
		failOnce.Do(func() {
			failed = err
			cancel(err)
		})
	}
	var added, replaced, restored atomic.Int64
	var failedMu sync.Mutex
	share := semaphore.NewWeighted(int64(c.fetchConcurrency))
	inHand := semaphore.NewWeighted(int64(installsPerFetch * c.fetchConcurrency))
	letGo := func() {
		c.fetches.letGo()
		inHand.Release(1)
	}
	getSlot := func(ctx context.Context, turn <-chan struct{}, pass func()) ([]byte, func(), error) {
		select {
		case <-turn:
		case <-ctx.Done():
			return nil, nil, context.Cause(ctx)
		}
		if err := share.Acquire(ctx, 1); err != nil {
			return nil, nil, err
		}
		buf, err := c.fetches.take(ctx)
		if err != nil {
			share.Release(1)
			return nil, nil, err
		}
		pass()
		return buf, func() {
			c.fetches.give(buf)
			share.Release(1)
		}, nil
	}
	var workers sync.WaitGroup
	var waited error
	// placedIn is every place a pack landed in, each synced once at the end.
	var placedIn [placeDropping + 1]atomic.Bool
	viewMade := false
	turn := make(chan struct{})
	close(turn) // the first worker's turn is now
	for _, p := range todo {
		if err := rctx.Err(); err != nil {
			waited = context.Cause(rctx)
			break
		}
		place := places[p.GitChecksum]
		packDir := filepath.Join(inc.dir, place.rel())
		if place == placeDropping && !viewMade {
			// The dropping view, made durable before any pack goes in it.
			if err := c.ensureView(inc.dir); err != nil {
				fail(fmt.Errorf("cache: sync %s: install: %w", e.id, err))
				break
			}
			viewMade = true
		}
		// A pack this node's own push wrote, handed over by its adoption
		// (adopt.go), is linked from those files, no GET: still a pack ADDED,
		// so what a declaration says of one comes first (dropFor's order rule).
		if st := c.claimHanded(e, p); st != nil {
			if _, present := over[p.GitChecksum]; !present {
				if err := addOnce(); err != nil {
					c.dropStaging(st)
					fail(err)
					break
				}
			}
			if c.installHanded(e, inc, place, p, st) {
				placedIn[place].Store(true)
				added.Add(1)
				continue
			}
		}
		if err := inHand.Acquire(rctx, 1); err != nil {
			waited = context.Cause(rctx)
			break
		}
		if err := c.fetches.hold(rctx); err != nil {
			inHand.Release(1)
			waited = context.Cause(rctx)
			break
		}
		staged, err := c.stageBundle(e.id, p)
		if err != nil {
			letGo()
			fail(err)
			break
		}
		mine, next := turn, make(chan struct{})
		turn = next
		workers.Go(func() {
			// Passed on once this worker holds its share and slot, or as it
			// stops without them, when the next worker finds rctx ended.
			pass := sync.OnceFunc(func() { close(next) })
			defer pass()
			defer letGo()
			err := c.download(rctx, e.id, staged, func(ctx context.Context) ([]byte, func(), error) {
				return getSlot(ctx, mine, pass)
			})
			why, present := over[staged.p.GitChecksum]
			switch {
			case err != nil && present && rctx.Err() == nil:
				// A REPAIR THAT COULD NOT DOWNLOAD: the pack stays as it is,
				// and the round goes on.
				failedMu.Lock()
				if out.failed == nil {
					out.failed = map[string]error{}
				}
				out.failed[staged.p.GitChecksum] = err
				failedMu.Unlock()
				c.log.Warn("code_cache_pack_reinstall_failed", "repo", e.id, "incarnation", inc.n,
					"pack_id", staged.p.PackID, "found", why.found, "error", err)
				return
			case err != nil:
				fail(err)
				return
			}
			c.hookInstall(installFetched, packDir)
			if present && why.drops {
				if err := dropOnce(); err != nil {
					fail(err)
					return
				}
			}
			if !present {
				// A pack added (dropFor's order rule).
				if err := addOnce(); err != nil {
					fail(err)
					return
				}
			}
			if err := c.placePack(e, inc, place, staged, present, absent[staged.p.GitChecksum]); err != nil {
				fail(err)
				return
			}
			placedIn[place].Store(true)
			if !present {
				added.Add(1)
				if absent[staged.p.GitChecksum] {
					restored.Add(1)
					c.log.Warn("code_cache_pack_reinstalled", "repo", e.id, "incarnation", inc.n,
						"pack_id", staged.p.PackID, "found", "absent", "trigger", string(ck.trigger))
				}
				return
			}
			replaced.Add(1)
			c.log.Warn("code_cache_pack_reinstalled", "repo", e.id, "incarnation", inc.n,
				"pack_id", staged.p.PackID, "found", why.found, "trigger", string(why.trigger))
		})
	}
	// THE LAYERS (layers.go) queue behind the packs in the one turn chain, so
	// a layer's GET starts as the packs' tail frees a slot, and none waits for
	// the one below (CheckLayer reads the base's graph ids from the rows).
	// Each is staged and checked for the round to place, never placed here
	// and never the round's failure: one not fetched leaves the copy serving
	// the prefix below it. One adopted here (AdoptLayer) needs no GET. What
	// the round does not place leaves tmp/ with it.
	out.layers = make([]layerFetch, len(chain.want))
	for i, row := range chain.want {
		out.layers[i] = layerFetch{row: row}
		if waited != nil || rctx.Err() != nil {
			out.layers[i].err, out.layers[i].outcome = context.Cause(rctx), layerFailed
			continue
		}
		base := chain.base(i)
		if st := c.claimStagedLayer(e.id, row, base); st != nil {
			out.layers[i].staged = st
			continue
		}
		if err := inHand.Acquire(rctx, 1); err != nil {
			waited = context.Cause(rctx)
			out.layers[i].err, out.layers[i].outcome = waited, layerFailed
			continue
		}
		if err := c.fetches.hold(rctx); err != nil {
			inHand.Release(1)
			waited = context.Cause(rctx)
			out.layers[i].err, out.layers[i].outcome = waited, layerFailed
			continue
		}
		st, files, err := c.stageLayer(row)
		if err != nil {
			letGo()
			out.layers[i].err, out.layers[i].outcome = err, layerFailed
			continue
		}
		// A layer's own end: a part's failure ends no other work of the round.
		lctx, lcancel := context.WithCancelCause(rctx)
		parts := make([]error, len(row.Layer.Parts))
		var partsDone sync.WaitGroup
		var writers [3]io.WriterAt
		for j, f := range files {
			if f != nil {
				writers[j] = f
			}
		}
		for j := range row.Layer.Parts {
			mine, next := turn, make(chan struct{})
			turn = next
			partsDone.Go(func() {
				pass := sync.OnceFunc(func() { close(next) })
				defer pass()
				buf, give, err := getSlot(lctx, mine, pass)
				if err != nil {
					parts[j] = err
					lcancel(err)
					return
				}
				defer give()
				if err = gitlet.FetchLayerPart(lctx, c.objects, row.Layer, j, writers, buf); err != nil {
					parts[j] = err
					lcancel(err)
				}
			})
		}
		f := &out.layers[i]
		workers.Go(func() {
			defer letGo()
			defer lcancel(nil)
			partsDone.Wait()
			var err error
			if slices.ContainsFunc(parts, func(err error) bool { return err != nil }) {
				// The first part's failure, which ended the rest: a part the
				// store lost, bytes not its row's, or the round's own end.
				closeAll(files)
				err = context.Cause(lctx)
			} else {
				err = c.finishLayer(rctx, st, files, base, c.git.HoldRead)
			}
			if err != nil {
				st.remove(c)
				f.err, f.outcome, f.took = err, layerFailure(err), time.Since(st.began)
				return
			}
			f.staged = st
		})
	}
	workers.Wait()
	err := failed
	if err == nil && waited != nil {
		err = fmt.Errorf("cache: sync %s: install: %w", e.id, waited)
	}
	out.added, out.replaced, out.restored = int(added.Load()), int(replaced.Load()), int(restored.Load())
	if droppedOver.Load() {
		out.dropped = dropped
	}
	// The renames are durable before any ref names what they installed.
	for _, place := range []packPlace{placeLive, placeDropping} {
		if !placedIn[place].Load() {
			continue
		}
		if serr := syncDir(filepath.Join(inc.dir, place.rel())); serr != nil && err == nil {
			err = fmt.Errorf("cache: sync %s: install: %w", e.id, serr)
		}
	}
	return out, err
}

// ensureView makes the dropping view of the incarnation at dir where it is
// missing, gitproc.DroppingDir and its pack directory, each new entry durable
// before any pack goes in (durable.go's rule). Something else at either name
// fails it, for the next full round's sweep to move aside (sweep.go's image).
func (c *Cache) ensureView(dir string) error {
	view := filepath.Join(dir, droppingRel)
	if info, err := os.Lstat(view); err == nil && info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(view, hostbox.DirMode); err != nil {
		return fmt.Errorf("making the dropping view %s: %w", view, err)
	}
	for _, d := range []string{filepath.Join(dir, gitproc.DroppingDir), dir} {
		if err := syncDir(d); err != nil {
			return err
		}
	}
	return nil
}

// heldAt reports whether the incarnation at dir holds p whole at place: what
// tells a pack its rows MOVED between objects/pack and the dropping view from
// one the copy lost. A file that cannot be read says it does not.
func heldAt(dir string, place packPlace, p gitlet.PackRow) bool {
	if place == placeNone {
		return false
	}
	have, err := installed(filepath.Join(dir, place.rel()), p, false)
	return err == nil && have == packWhole
}

// reinstallWhy is why an install places a pack over files the directory
// holds, logged with code_cache_pack_reinstalled beside the trigger of the
// check that looked: found is `trailer` (a check or scrub held a file to its
// checksum), `hint` (ConfirmHint held its bytes to its row) or `size` (a full
// round); a lost pack logs as `absent`. drops: the derived files may have
// been built from the damage (a failed .idx, a wrong size, or a hint, which
// names the bundle, over an .idx not vouched), so they go before it is
// placed; a .pack or .rev alone comes back byte for byte, at the same offsets.
type reinstallWhy struct {
	found   string
	trigger checkTrigger
	drops   bool
}

// installs is what one install did: the packs it added, the packs it
// installed again over their damaged files, and the derived files it dropped
// before the first of those.
type installs struct {
	added, replaced int
	// restored is how many of the added were packs the copy had lost (lost),
	// a repair; the rest are pushes it had not yet read.
	restored int
	dropped  derivedSet
	// failed is the reinstalls, by checksum, whose download failed, each with
	// its failure: left as they stand.
	failed map[string]error
	// layers is what became of each layer of chainPlan.want, in chain order:
	// staged and checked, or the failure that stopped it.
	layers []layerFetch
}

// indexVouched reports whether the .idx of inc's pack cs at place is one this
// process wrote or held to its trailer, at the identity it has now: a hint's
// damage, which the hint places only in the pack's bundle, is then not the
// index's.
func (c *Cache) indexVouched(e *entry, inc *incarnation, place packPlace, cs string) bool {
	rel := packFileRel(place, cs, ".idx")
	id := lstatID(filepath.Join(inc.dir, rel))
	e.mu.Lock()
	defer e.mu.Unlock()
	return vouchedAt(inc, rel, id)
}

// lost reports whether p, a pack the directory does not hold, is one the
// incarnation held before the round (st, its state then): its row added on
// st's stream at or before st's position. A row added since is a push not yet
// read; no state, or another stream's, says nothing. Nor does a snapshot
// BELOW st's position: only a log rewound under the copy gives one
// (converge's rewound read), and it hands positions the copy had read to
// other records, so a push there was never held.
func lost(st *state, snap snapshot, p gitlet.PackRow) bool {
	if st == nil || !st.StreamCreated.Equal(snap.streamCreated) || p.Added.IsZero() ||
		!reached(snap.position, st.Position) {
		return false
	}
	newer, err := st.Position.Before(p.Added)
	return err == nil && !newer
}

// packHeld is how much of a pack a directory holds.
type packHeld int

const (
	packAbsent packHeld = iota
	packWhole
	packWrongSize
)

// installed reports whether packDir holds p: its three files present, and —
// when sized — each at the size p's row says.
func installed(packDir string, p gitlet.PackRow, sized bool) (packHeld, error) {
	ref := p.Ref()
	want := map[string]int64{".idx": ref.IndexBytes, ".pack": ref.PackBytes(), ".rev": ref.ReverseBytes}
	held := packWhole
	for _, ext := range []string{".idx", ".pack", ".rev"} {
		info, err := os.Lstat(filepath.Join(packDir, "pack-"+p.GitChecksum+ext))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if held == packWhole {
				held = packAbsent
			}
		case err != nil:
			return packAbsent, err
		case sized && (!info.Mode().IsRegular() || info.Size() != want[ext]):
			held = packWrongSize
		}
	}
	return held, nil
}

// placePack makes s durable and renames it into place (packPlace), counting
// it into inc: its three files synced at once in tmp/, then renamed .pack,
// .rev and the .idx LAST, since git counts a pack once its .idx exists, also
// over damaged files. Whatever of s is not placed is removed. What it placed
// is vouched (check.go); placed over damage, or a pack the copy had lost, it
// is a REPAIR (incarnation.repairWrite).
//
// THE SYNCS ARE ISSUED TOGETHER, so the filesystem groups them, and every
// other worker's, into one journal commit: three in turn halved the node
// side's bundles a second. A per-round placer syncing every bundle together
// was slower (BenchmarkColdSync), its one goroutine becoming the round's pace.
func (c *Cache) placePack(e *entry, inc *incarnation, place packPlace, s *stagedPack, over, repairs bool) error {
	defer s.remove(c)
	if err := c.syncStaged(s); err != nil {
		return fmt.Errorf("cache: sync %s: install pack %s: %w", e.id, s.p.PackID, err)
	}
	packDir := filepath.Join(inc.dir, place.rel())
	var was int64
	for _, ext := range []string{".pack", ".rev", ".idx"} {
		dst := filepath.Join(packDir, "pack-"+s.p.GitChecksum+ext)
		if over {
			if info, err := os.Lstat(dst); err == nil {
				was += info.Size()
			}
		}
		if err := os.Rename(s.path(ext), dst); err != nil {
			return fmt.Errorf("cache: sync %s: install pack %s: renaming %s over %s: %w",
				e.id, s.p.PackID, s.path(ext), dst, err)
		}
		s.placed(ext)
		c.hookInstall(strings.TrimPrefix(ext, "."), packDir)
	}
	e.mu.Lock()
	c.counted(inc, place, s.p.Objects, !over, s.p.Size-was)
	if over || repairs {
		repaired(inc)
	}
	// In place: any backoff its failed download earned is over
	// (repairMark.fetchFailed).
	delete(inc.repair.fetchFailed, s.p.GitChecksum)
	e.mu.Unlock()
	c.vouchPack(e, inc, place, s.p.GitChecksum)
	return nil
}

// counted counts a pack of objects objects placed at place into inc, added or
// over files it held (its bytes then move by grew alone): the bytes on disk,
// the dropping view's among them, and the packs and objects an index covers,
// which never covers a pack in the view. e.mu is held.
func (c *Cache) counted(inc *incarnation, place packPlace, objects int64, added bool, grew int64) {
	if added && place != placeDropping {
		inc.packs++
		inc.objects += objects
	}
	c.setSize(inc, inc.diskBytes+grew)
	if place == placeDropping {
		c.setDropping(inc, inc.droppingBytes+grew)
	}
}

// installHanded installs p from the staging an adoption handed the round (the
// receiving node's own files, sealed and held to the row) by linking them in
// the install's order, and reports whether it did. One it could not link is
// logged and left to the download; the staging is dropped either way, and the
// directory is synced with the round's other installs.
func (c *Cache) installHanded(e *entry, inc *incarnation, place packPlace, p gitlet.PackRow, st *staging) bool {
	defer c.dropStaging(st)
	packDir := filepath.Join(inc.dir, place.rel())
	linked, err := linkBundle(nil, st.stem, packDir, p.GitChecksum, func(ext string) {
		c.hookInstall(strings.TrimPrefix(ext, "."), packDir)
	})
	if err != nil {
		c.log.Warn("code_cache_adopt_failed", "repo", e.id, "pack_id", p.PackID, "error", err)
	}
	if !linked {
		return false
	}
	e.mu.Lock()
	c.counted(inc, place, p.Objects, true, p.Size)
	e.mu.Unlock()
	c.vouchPack(e, inc, place, p.GitChecksum)
	return true
}

// installFetched is the install hook's stage once a bundle is split and
// checked, before any of its renames.
const installFetched = "fetched"

// hookInstall calls the suite's install hook, when one is set.
func (c *Cache) hookInstall(name, packDir string) {
	if c.installHook != nil {
		c.installHook(name, packDir)
	}
}

// stagedPack is one bundle's three files in tmp/, made by its round and
// written, checked and closed by its download, until each is renamed into
// place; whatever is left when the install ends is removed and counted out of
// the bytes in flight.
type stagedPack struct {
	p     gitlet.PackRow
	stem  string
	sizes map[string]int64
	left  map[string]bool
	// files is the .pack, .idx and .rev, open for the GET to write, until
	// the download closes them.
	files []*os.File
}

func (s *stagedPack) path(ext string) string { return s.stem + ext }

// placed records that ext was renamed into place: no longer this pack's to
// remove, and no longer bytes in tmp/.
func (s *stagedPack) placed(ext string) { delete(s.left, ext) }

// remove takes away every file of s not placed, and counts all of s out of
// the bytes in flight (a placed file now counts as the incarnation's).
func (s *stagedPack) remove(c *Cache) {
	for ext := range s.left {
		if err := os.Remove(s.path(ext)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_trash_failed", "path", s.path(ext), "error", err)
		}
	}
	var total int64
	for _, n := range s.sizes {
		total += n
	}
	c.tmpBytes.Add(-total)
}

// abandon closes whatever of s is still open and removes it: a bundle whose
// install failed before it was placed.
func (s *stagedPack) abandon(c *Cache) {
	for _, f := range s.files {
		// Removed next; an already-closed file's error says nothing.
		_ = f.Close()
	}
	s.files = nil
	s.remove(c)
}

// stageBundle makes p's three files in tmp/, open for its GET to write, and
// counts p into the bytes in flight. The round calls it on its own goroutine,
// in its order (install says why); whatever is not placed is removed
// (abandon, placePack).
func (c *Cache) stageBundle(id string, p gitlet.PackRow) (*stagedPack, error) {
	ref := p.Ref()
	f, err := os.CreateTemp(filepath.Join(c.dir, tmpDir), "pack-*.pack")
	if err != nil {
		return nil, fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err)
	}
	s := &stagedPack{
		p:     p,
		stem:  strings.TrimSuffix(f.Name(), ".pack"),
		sizes: map[string]int64{".pack": ref.PackBytes(), ".idx": ref.IndexBytes, ".rev": ref.ReverseBytes},
		left:  map[string]bool{".pack": true},
		files: []*os.File{f},
	}
	c.tmpBytes.Add(p.Size)
	for _, ext := range []string{".idx", ".rev"} {
		g, err := os.OpenFile(s.path(ext), os.O_CREATE|os.O_EXCL|os.O_WRONLY, indexMode)
		if err != nil {
			s.abandon(c)
			return nil, fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err)
		}
		s.left[ext] = true
		s.files = append(s.files, g)
	}
	if c.knobs != nil && c.knobs.staged != nil {
		c.knobs.staged(p.Object)
	}
	return s, nil
}

// download streams s's bundle into its three files, split as its row says and
// teed through gitlet's seal, checks the seal against the row, and closes the
// files; syncing them is placePack's, off the GET slot. getSlot waits for the
// GET's turn, share and node slot, and answers the copy buffer and what gives
// both back, called the moment the store's reader closes. Any failure
// abandons s.
func (c *Cache) download(ctx context.Context, id string, s *stagedPack,
	getSlot func(context.Context) ([]byte, func(), error)) error {

	p := s.p
	fail := func(err error) error {
		s.abandon(c)
		return err
	}
	if c.faults != nil {
		if err := c.faults.Hit(ctx, gitletfault.SyncDownload, 0); err != nil {
			return fail(fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err))
		}
	}
	if c.knobs != nil && c.knobs.asking != nil {
		c.knobs.asking(p.Object)
	}
	buf, end, err := getSlot(ctx)
	if err != nil {
		return fail(err)
	}
	giveBack := sync.OnceFunc(end)
	defer giveBack()
	ref := p.Ref()
	rc, err := c.objects.Open(ctx, ref.Content())
	if err != nil {
		return fail(c.storeFailure(id, p, err))
	}
	seal := gitlet.NewSealWriter(ref)
	files := []io.Writer{s.files[0], s.files[1], s.files[2]}
	if c.knobs != nil && c.knobs.stagedWrite != nil {
		for i := range files {
			files[i] = failingWriter{err: c.knobs.stagedWrite(p.Object), w: files[i]}
		}
	}
	split := &splitter{seal: seal, parts: []part{
		{w: files[0], left: ref.PackBytes()},
		{w: files[1], left: ref.IndexBytes},
		{w: files[2], left: ref.ReverseBytes},
	}}
	// WHOSE FAILURE A COPY'S IS, by the side that failed, never by
	// elimination: the reader's is the store's (fromStore), a write's is this
	// node's disk, which told as the store's sends an operator to the bucket
	// for a full local volume.
	_, err = io.CopyBuffer(split, fromStore{rc}, buf)
	if cerr := rc.Close(); err == nil && cerr != nil {
		err = storeErr{cerr}
	}
	// The GET is over: what follows is local disk, and the slot another's.
	giveBack()
	var store storeErr
	switch {
	case errors.Is(err, errBundleTooLong):
		return fail(c.rejected(id, p, fmt.Errorf("%w: %w", gitlet.ErrBadBundle, err)))
	case errors.As(err, &store):
		return fail(c.storeFailure(id, p, store.err))
	case err != nil:
		return fail(fmt.Errorf("cache: sync %s: install pack %s: writing its files on this node: %w", id, p.PackID,
			err))
	}
	sealed, err := seal.Seal()
	if err == nil {
		err = gitlet.CheckBundle(ref, sealed)
	}
	if err != nil {
		return fail(c.rejected(id, p, err))
	}
	if err = s.files[0].Chmod(packMode); err != nil {
		return fail(fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err))
	}
	for _, f := range s.files {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		return fail(fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err))
	}
	s.files = nil
	return nil
}

// syncStaged makes s's three files durable AT ONCE — each file's sync in
// flight together, answered once all have ended (placePack says why).
func (c *Cache) syncStaged(s *stagedPack) error {
	syncFile := (*os.File).Sync
	if c.knobs != nil && c.knobs.fileSync != nil {
		syncFile = c.knobs.fileSync
	}
	exts := []string{".pack", ".idx", ".rev"}
	errs := make([]error, len(exts))
	var wg sync.WaitGroup
	for i, ext := range exts {
		wg.Go(func() { errs[i] = syncPath(s.path(ext), syncFile) })
	}
	wg.Wait()
	return errors.Join(errs...)
}

// syncPath makes the file at path durable: opened to be synced, since a
// downloaded file is closed once it is written.
func syncPath(path string, syncFile func(*os.File) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	err = syncFile(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// fromStore is the object store's reader, its failures marked as the store's
// (storeErr) — every one but the io.EOF that ends a whole read, which
// io.CopyBuffer compares by identity.
type fromStore struct{ r io.Reader }

func (f fromStore) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && err != io.EOF {
		err = storeErr{err}
	}
	return n, err
}

// storeErr is a failure of the object store's reader, as a download met it.
type storeErr struct{ err error }

func (e storeErr) Error() string { return e.err.Error() }
func (e storeErr) Unwrap() error { return e.err }

// failingWriter is a staged file whose every write answers err: the suite's
// stand-in for this node's disk failing under a download (knobs.stagedWrite).
type failingWriter struct {
	err error
	w   io.Writer
}

func (f failingWriter) Write(p []byte) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	return f.w.Write(p)
}

// storeFailure is what the object store's failure to hand p over means:
// absent or damaged is DEFINITE, an *ErrPackMissing the round logs once final
// (damaged at once, absent once a read at the log's head made after this
// answer still names it); anything else wraps ErrStoreRead.
func (c *Cache) storeFailure(id string, p gitlet.PackRow, err error) error {
	var damaged bool
	switch {
	case errors.Is(err, objstore.ErrNotFound):
	case errors.Is(err, objstore.ErrCorrupt):
		damaged = true
	default:
		return fmt.Errorf("%w: sync %s: install: download pack %s (object %s): %w", ErrStoreRead, id, p.PackID,
			p.Object, err)
	}
	return &ErrPackMissing{Repo: id, Pack: p, Damaged: damaged, Err: err}
}

// rejected is a bundle whose verified bytes are not its row's, logged as
// code_pack_checksum_mismatch, the one name the node gives that fact (Gitlet
// logs it again for each git request the failed sync answers). Not
// code_pack_rejected: that is the receiving node refusing a push's own pack,
// a different fault with a different remedy.
func (c *Cache) rejected(id string, p gitlet.PackRow, err error) error {
	c.log.Error("code_pack_checksum_mismatch", "repo", id, "pack_id", p.PackID, "object", p.Object.String(),
		"error", err)
	return &ErrCorruptPack{Repo: id, Pack: p, Err: err}
}

// errBundleTooLong is a stream longer than the bundle its row lays out.
var errBundleTooLong = errors.New("the stream is longer than the bundle its row lays out")

// splitter writes a bundle's bytes to its three files in turn — each part
// takes exactly its row's size — teeing every byte through the seal.
type splitter struct {
	seal  *gitlet.SealWriter
	parts []part
	at    int
}

// part is one file of a bundle and how many of its bytes are still to come.
type part struct {
	w    io.Writer
	left int64
}

func (s *splitter) Write(p []byte) (int, error) {
	_, _ = s.seal.Write(p) // gathers and never fails
	n := 0
	for len(p) > 0 {
		for s.at < len(s.parts) && s.parts[s.at].left == 0 {
			s.at++
		}
		if s.at == len(s.parts) {
			return n, errBundleTooLong
		}
		cur := &s.parts[s.at]
		chunk := p[:min(int64(len(p)), cur.left)]
		w, err := cur.w.Write(chunk)
		n += w
		cur.left -= int64(w)
		if err != nil {
			return n, err
		}
		p = p[w:]
	}
	return n, nil
}
