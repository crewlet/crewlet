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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/objstore"
)

// THE INSTALL: one GET a pack, split into its three files as it streams,
// checked, and three renames — no git process. A push stores ONE object, its
// pack BUNDLE: the pack, then the .idx (version 2) and the .rev (version 1)
// the receiving node's receive-pack wrote, back to back. Every byte is
// SHA-256 verified against the row on the way in (objstore holds back the
// byte that completes the object until the whole of it is verified), and the
// three sections are held to the row by code.CheckBundle: their heads, their
// counts and their pack checksums. A bundle that is not its row's is refused
// and never retried — the row and the bytes disagree, and asking again fixes
// neither.
//
// WHY NO index-pack: the receiving node's receive-pack ran index-pack
// --strict --fix-thin over exactly these bytes against the repository, and
// an .idx is a pure function of its pack; re-indexing would spend 0.13–0.18 s
// of a core a mebibyte, on every git node, recomputing a file the row
// already names — and make a cold sync bound by CPU rather than by the
// object store.
//
// THE BYTE PATH IS SHAPED FOR S3, whose GETs are latency-bound and want
// width: up to fetchConcurrency downloads a round, at most NodeFetches across
// every round on the node, LARGEST FIRST — the round ends when its last
// download does, so the longest start first and the small ones fill the gaps.
// A round hands out its packs in that order, each to a worker of its own that
// exists only once the pack is IN HAND, the round's and the node's
// (installsPerFetch). The round makes the bundle's three files as it hands
// the pack out (stageBundle), so in its order; the worker waits for its TURN
// — the worker before it holding its GET, so the round's GETs are granted in
// its order however the scheduler runs its workers — then for the round's
// share and a node slot, downloads and checks the one pack it was given, and
// places it (placePack): its three files made durable at once, then renamed
// in.
//
// A GET'S SLOT AND SHARE ARE THE GET, and nothing else. A node slot's copy
// buffer is handed out with it and back with it, so the node's download
// memory is NodeFetches buffers however many rounds wait — a node joining a
// fleet asked for 500 cold repositories at once holds 8 MiB of them, not a
// buffer for every round's every worker. And the slot and the round's share
// go back the moment the store's reader closes: the split's seal check, the
// syncs and the renames that follow are local disk work, and a GET slot spent
// on them is a GET the node does not make. What bounds that disk work instead
// is the packs IN HAND (installsPerFetch), the round's and the node's.

// copyBuffer is the buffer one download is copied through: 256 KiB, so a
// 150 KB bundle is one read of the store's stream and a large one a few
// thousand. One a node slot ([fetchSlots]), so NodeFetches of them — 8 MiB —
// is every download buffer the node holds at once.
const copyBuffer = 256 << 10

// fetchSlots is the node's downloads: NodeFetches slots, each handed out with
// the copy buffer its download runs through and taken back with it — and the
// packs the node holds IN HAND, installsPerFetch for every slot, each taken
// before its GET and given back once the pack is in place or abandoned.
type fetchSlots struct {
	sem *semaphore.Weighted
	// free holds the buffers no slot holds: never more than the slots, so
	// buffers made and buffers kept together never exceed them.
	free chan []byte
	// out is the buffers handed out now, most the most at once, and waiting
	// the downloads waiting for a slot — what the suite holds to the node's
	// bound.
	out, most, waiting atomic.Int64
	// hands bounds the node's packs in hand, and held, mostHeld and
	// handWaiting count them as out, most and waiting count the buffers.
	hands                       *semaphore.Weighted
	held, mostHeld, handWaiting atomic.Int64
}

func newFetchSlots(n int64) *fetchSlots {
	return &fetchSlots{
		sem: semaphore.NewWeighted(n), free: make(chan []byte, n),
		hands: semaphore.NewWeighted(installsPerFetch * n),
	}
}

// hold waits for a pack in hand: one the node's rounds may be downloading or
// have downloaded and not yet placed.
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

// installsPerFetch is how many packs a round, and the node, hold IN HAND for
// every GET they may run at once: a pack is in hand from the moment it is
// given to a worker until it is renamed into place or abandoned — its files'
// creation, its GET, then its checks, its syncs and its renames.
//
// TWO, so a GET never waits on this node's disk: the round's share and the
// node's slot go back the moment a GET ends, and while one share of bundles
// is being read the one before it is made durable and renamed in. A share's
// GETs bring a bundle every 2 ms at sixteen and the S3 fake's 30 ms first
// byte — every 6 to 12 ms at S3's own 100 to 200 ms — while this node's own
// side took 1.1 to 1.5 ms a bundle from memory (Measured) — so a second share
// covers a share's disk work, by more at a real bucket's first byte, and a
// disk slower than its link holds the GETs back rather than growing a
// backlog. It is what bounds
// that disk work: tmp/ holds at most a round's 2 × FetchConcurrency and the
// node's 2 × NodeFetches largest bundles, and the syncs a node has in flight
// at once are three files a pack of them — never a backlog, nor a thread a
// sync, that grows with the rounds a joining node starts at once while its
// disk stalls.
const installsPerFetch = 2

// The modes an installed pack's files take: the .pack read-only to the node
// (nothing writes a pack in place), the indexes as git leaves the ones it
// writes.
const (
	packMode  os.FileMode = 0o400
	indexMode os.FileMode = 0o444
)

// install brings into inc every pack snap names that the directory does not
// hold, and answers how many it added and how many it installed again over
// the files of a pack it held damaged.
//
// A pack is installed when its .idx, .pack and .rev all exist — git counts a
// pack once its .idx does — and, on a FULL round, each at the size its row
// says: three stats a pack, once per repository per process, so a file a disk
// fault truncated is caught here rather than by the next client.
//
// A PACK HELD DAMAGED IS INSTALLED AGAIN BY RENAMING OVER ITS FILES, NEVER
// REMOVED FIRST: one at the wrong size, and one whose .idx, .rev or .pack a
// check, a scrub or a confirmed hint found failing and that is still the
// file found so (checked.still). Removed before its download, its objects
// were missing for the whole of that download to every git process over the
// copy that opened the pack meanwhile; renamed over, a process holding the
// old files reads the inodes it opened, and one that opens the path
// afterwards reads the new ones. Such a reinstall adds no pack to the counts;
// the incarnation's size moves by the difference.
//
// THE DERIVED FILES GO ONLY WHERE THEY MAY HAVE BEEN BUILT FROM THE DAMAGE —
// once, before the first such placement: a pack whose .idx failed, a pack at
// the wrong size, or one a confirmed hint named whose .idx this process has
// not held to its trailer (the hint names the bundle, not the file). The
// multi-pack-index and its bitmap record each object's offset as the pack's
// .idx placed it, and the commit-graph was built reading commits through it;
// a pack whose .pack or .rev alone failed is installed again byte for byte
// — its name is its checksum, its bundle held to its row — so every offset
// the index and the bitmap record, and every commit the graph does, is the
// same over the new files. Dropped for a .pack's body, a copy lost its
// bitmap for the whole rewrite over its history — and for good where the
// node's memory no longer holds the bitmap's charge.
//
// A REINSTALL IS A REPAIR, NEVER THE ROUND'S PRECONDITION: one whose download
// fails — the object store unreachable, or a bundle it no longer holds — is
// logged, its pack left as it stands, and the round goes on and converges,
// installing every other pack, the pushes it was asked for among them. The
// finding stays on the repair mark, held off for a backoff that doubles from
// RebuildCooldown to RebuildBackoffCap (repairMark.fetchFailed), during which
// no round tries that pack again; failing the round instead failed every
// later one of the repository — each one full, since the mark asks for a
// full round — and every advertisement of it with them, for as long as the
// store could not hand the pack over.
//
// A failure of anything else removes every temporary file the round made; the
// packs installed before it stay, named by rows, and the next round skips
// them.
//
// EACH PACK GOES WHERE ITS ROWS PLACE IT once the round has applied
// (placesAfter): objects/pack, or — for a pack only rows an expunge's begin
// left dropping name — the incarnation's DROPPING VIEW, made with the first
// pack placed in it (ensureView), which only a process serving the copy reads
// (packPlace). A pack its rows moved from one to the other is installed at
// its new place, one GET, its files at the old leaving with the swap.
//
// A FULL ROUND'S LAYERS JOIN THE SAME QUEUE (layers.go): every part of every
// layer of chain.want — the published chain above what the copy keeps, root
// first — is handed out BEHIND the round's packs in the one turn chain, in
// the round's share and the node's slots, so a layer's GET begins the moment
// a slot the packs' tail no longer needs frees, and no layer waits for the
// one below it: CheckLayer reads a layer's own files and the base's graph ids
// from the rows. Each layer is staged in tmp/ and checked, and answered in
// installs.layers for the round to place — never placed here, and never the
// round's failure: a layer that cannot be fetched leaves the copy serving
// from the prefix below it. A layer the repository's maintainer adopted on
// this node (AdoptLayer) is taken from its files, no GET. What the round
// does not place goes from tmp/ with it.
func (c *Cache) install(ctx context.Context, e *entry, inc *incarnation, snap snapshot, ck checked,
	chain chainPlan) (installs, error) {

	var out installs
	reinstall, _ := ck.still(c.image, inc.dir)
	var todo []code.PackRow
	// over is every pack this install places over files present, by its
	// checksum, with what found it damaged and why that looked; absent, every
	// pack it installs that the directory LOST — held before this round (lost)
	// and gone from it since — a REPAIR as much as one installed again,
	// WHICHEVER ROUND PUTS IT BACK: a request's forced round, a round's own
	// repair, or any round reading the copy whole (a shape's, a report's, a
	// boot's). A failure a lost pack caused is ended by whatever puts the pack
	// back, so the guard reads that write as it reads any repair
	// (incarnation.repairWrite); counted only by a forced round, a request
	// whose git began before another round put the pack back was handed a
	// forced round that found nothing, the scrub and a whole rebuild of a copy
	// nothing was wrong with any more. A pack the rows name that the copy never
	// held is a push it has not read yet, which repairs nothing.
	over := map[string]reinstallWhy{}
	absent := map[string]bool{}
	now := c.now()
	e.mu.Lock()
	before := inc.state
	heldOff := inc.repair.heldOff(now)
	// WHERE EACH PACK GOES: where the live set places it once the round has
	// applied the snapshot — the dropping view for a pack only rows an
	// expunge's begin left dropping name, objects/pack for any other.
	places := placesAfter(inc, snap)
	e.mu.Unlock()
	for _, p := range snap.packs {
		// Two rows of one checksum name one set of files, which one place
		// holds: their rows place it together.
		place := places[p.GitChecksum]
		have, err := installed(filepath.Join(inc.dir, place.rel()), p, snap.full)
		f, failed := reinstall[p.GitChecksum]
		switch {
		case err != nil:
			return out, fmt.Errorf("cache: sync %s: install: %w", e.id, err)
		case have == packWhole && !failed:
			continue
		case have != packAbsent && heldOff[p.GitChecksum]:
			// Its last download to install it again failed, and its backoff
			// has not passed: left as it stands, its finding on the mark.
			continue
		case have == packWhole && f.hinted:
			over[p.GitChecksum] = reinstallWhy{found: "hint", trigger: f.trigger,
				drops: f.index || !c.indexVouched(e, inc, place, p.GitChecksum)}
		case have == packWhole:
			over[p.GitChecksum] = reinstallWhy{found: "trailer", trigger: f.trigger, drops: f.index}
		case have == packWrongSize:
			over[p.GitChecksum] = reinstallWhy{found: "size", trigger: ck.trigger, drops: true}
		case lost(before, snap, p) && !heldAt(inc.dir, place.other(), p):
			// Lost, not MOVED: a pack its rows place anew — out of
			// objects/pack into the dropping view, or back — is installed at
			// its new place, and is no repair.
			absent[p.GitChecksum] = true
		}
		todo = append(todo, p)
	}
	if len(todo) == 0 && len(chain.want) == 0 {
		return out, nil
	}
	// dropOnce drops what the declarations drop when a pack is removed
	// (EventPackRemoved) before the first placement over a present pack —
	// its old files go, and whatever named them was naming bytes no longer
	// there — and answers every later one with its outcome. addOnce is the
	// same for the first pack ADDED (EventPackAdded), which no declaration
	// drops today, so it moves nothing; it is here because what a
	// declaration says of an event is the declaration's to change, never a
	// call site's.
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
	slices.SortFunc(todo, func(a, b code.PackRow) int {
		return cmp.Or(cmp.Compare(b.Size, a.Size), cmp.Compare(a.PackID, b.PackID), cmp.Compare(a.Part, b.Part))
	})

	// The round's pack in hand, then the node's; then the round makes the
	// bundle's files (stageBundle) and starts a worker, which waits for its
	// TURN, then for the round's share and a node slot — its GET's alone,
	// both given back when the GET ends (download) — and then places the
	// pack. Goroutines and files are bounded by the packs in hand, and no
	// buffer is held but a slot's. The packs in hand go back once the pack is
	// in place or abandoned (letGo). The first failure — a worker's, the
	// round's own staging or its context — ends rctx, which ends the waits
	// below, so the round hands out nothing more.
	//
	// THE ROUND'S ORDER HOLDS AT BOTH STEPS, because the scheduler keeps
	// none: it runs a newly started goroutine next, so of the workers in
	// hand the one started LAST usually runs first. Workers that asked for a
	// GET as soon as they could granted a round's first shares to the packs
	// it handed out last, scrambling its order across every window of packs
	// in hand; so each worker asks only once the worker before it holds its
	// share and its node slot, and passes its own turn on once it holds both
	// (or as it stops without them). The turn costs no GET: a worker waiting
	// for it would wait behind the one before it for the share and the
	// node's slots anyway, each granted first come, first served. And workers
	// that made their own bundles' files made them in that scrambled order
	// too, so under the turn the round's first GET waited for whichever
	// pack's files were made last — a directory makes its files one at a
	// time, however many goroutines ask, so up to a hand of bundles' files,
	// at every round's start. The round makes them itself, in its order,
	// which costs no file a second for the same reason (BenchmarkColdSync's
	// 200 pushes from the S3 fake, medians of five interleaved runs: 390
	// and 507 bundles a second at sixteen and thirty-two GETs with the
	// workers making them, 400 and 530 with the round — and 401 and 527
	// with neither the turn nor the order).
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
	// placedIn is every directory a pack was placed in, by its place: each
	// synced once the install ends, before any ref names what it holds.
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
			// The dropping view, made durable before any pack is placed in
			// it — the copy's first dropping pack makes it.
			if err := c.ensureView(inc.dir); err != nil {
				fail(fmt.Errorf("cache: sync %s: install: %w", e.id, err))
				break
			}
			viewMade = true
		}
		// A pack this node's own push wrote, handed to the round by its
		// adoption (adopt.go), is installed from those files — three links
		// rather than a GET — once the round reaches it: a pack ADDED like
		// any other, so what a declaration says of one comes first
		// (dropFor's order rule), as it does before a download's placement.
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
			// Passed on once this worker holds its GET's share and slot, or
			// as it stops without them — the round has failed then, and the
			// next worker finds rctx ended.
			pass := sync.OnceFunc(func() { close(next) })
			defer pass()
			defer letGo()
			err := c.download(rctx, e.id, staged, func(ctx context.Context) ([]byte, func(), error) {
				return getSlot(ctx, mine, pass)
			})
			why, present := over[staged.p.GitChecksum]
			switch {
			case err != nil && present && rctx.Err() == nil:
				// A REPAIR THAT COULD NOT DOWNLOAD: the pack stays as it
				// stands, and the round goes on without it.
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
				// A pack added: what a declaration says of one, before
				// the first is placed (dropFor's order rule).
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
	// THE LAYERS, behind the packs in the same turn chain (above).
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
		// A layer's own end, which a part's failure brings about and which
		// ends none of the round's other work.
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
				if err = code.FetchLayerPart(lctx, c.objects, row.Layer, j, writers, buf); err != nil {
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
	// The renames are durable before any ref names what they installed: each
	// directory a pack landed in synced once.
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
// not there yet — its two directories, gitproc.DroppingDir and its pack
// directory — each new entry durable before any pack is placed in it, as
// durable.go's rule asks of what a file names. Something else at either name
// fails it: the next full round's sweep moves that aside (sweep.go's image).
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
// tells a pack its rows MOVED — out of objects/pack into the dropping view, or
// back — from one the copy lost. A file that cannot be read says it does not.
func heldAt(dir string, place packPlace, p code.PackRow) bool {
	if place == placeNone {
		return false
	}
	have, err := installed(filepath.Join(dir, place.rel()), p, false)
	return err == nil && have == packWhole
}

// reinstallWhy is why an install places a pack over files the directory
// holds: what found them damaged — `trailer` (a check or a scrub held one to
// its checksum), `hint` (a confirmed hint named it: ConfirmHint held its
// bytes to its row) or `size` (a full round found one at another size than
// its row's) — and the trigger of the check or scrub that looked, both logged
// with code_cache_pack_reinstalled; any round logs a pack the directory lost
// (lost) as `absent`, with its own check's trigger. drops: the derived files
// may have been built from the damage, so they go before it is placed
// (install's doc).
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
	// restored is how many of the added were packs the copy had lost (lost):
	// a repair, whichever round put them back, where the rest are pushes it
	// had not yet read.
	restored int
	dropped  derivedSet
	// failed is the packs, by checksum, it was to install again over their
	// files and could not download, each with the download's failure: left as
	// they stand.
	failed map[string]error
	// layers is what became of each layer of the chain the round fetches
	// (chainPlan.want), in chain order: staged and checked, or the failure
	// that stopped it.
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
// incarnation held before the round began (st, its state then): its row added
// on the log st was read from, at or before st's position — so the copy had
// it and has lost it. A row added since is a push the copy has not yet read,
// and no state (or another stream's) says nothing either way — nor does an
// answer BELOW st's position: only a log gone back under the copy gives one to
// a round (converge's rewound read), and such a log hands the positions past
// the copy it was restored from to other records, so a push it takes there
// sits at a position the copy had read without the copy ever having held it.
func lost(st *state, snap snapshot, p code.PackRow) bool {
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
func installed(packDir string, p code.PackRow, sized bool) (packHeld, error) {
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

// placePack makes s durable and renames it into the directory place puts it in
// — objects/pack, or the dropping view (packPlace) — counting the pack into
// inc: its three files synced at once while still in tmp/, then renamed
// .pack, .rev, and the .idx LAST, because git counts a pack present once its
// .idx exists. Whatever of s is not placed is removed, failure or not. A pack
// in the dropping view is counted in the bytes inc holds (setDropping beside
// setSize) and in none of the packs or objects an index covers (inc.packs,
// inc.objects), which never covers the view.
//
// THE SYNCS ARE ISSUED TOGETHER: a sync is a journal commit, and a bundle's
// three made one after another were three commits a bundle — half of a cold
// sync's node side one bundle at a time, when every bundle paid them in turn
// with its GET's share held (321 bundles a second from memory, against 600
// with them issued together). In flight at once they wait for one commit,
// and so do those of every other worker placing a bundle meanwhile: the
// filesystem groups concurrent syncs into one commit. A per-round placer
// that took every bundle downloaded meanwhile, synced them together and
// renamed them one after another was built and measured as well
// (BenchmarkColdSync): no faster from the S3 fake, and slower from memory at
// sixteen (597 bundles a second against 662), its one goroutine's
// sync-then-rename becoming the round's pace.
//
// OVER a pack the directory holds damaged, the renames replace its files in
// the same order, so the .idx that names the new .pack is the last to land;
// the pack is counted already, and the incarnation's size moves by what the
// files it replaced held. What it placed is vouched either way: this process
// wrote it, from a bundle held to its row (check.go). A placement over damaged
// files, or of a pack the copy had lost, is a REPAIR (incarnation.repairWrite):
// a request that failed before it may have failed on what it replaced.
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
	c.counted(inc, place, s.p, !over, s.p.Size-was)
	if over || repairs {
		repaired(inc)
	}
	// In place: a backoff its download earned while it could not be fetched
	// is over (repairMark.fetchFailed).
	delete(inc.repair.fetchFailed, s.p.GitChecksum)
	e.mu.Unlock()
	c.vouchPack(e, inc, place, s.p.GitChecksum)
	return nil
}

// counted counts a pack placed at place into inc — added, a pack it did not
// hold there before, or placed over files it held, which moves its bytes by
// grew alone: the bytes on its disk, of them the dropping view's, and the
// packs and objects an index covers, which a pack in the view is none of.
// e.mu is held.
func (c *Cache) counted(inc *incarnation, place packPlace, p code.PackRow, added bool, grew int64) {
	if added && place != placeDropping {
		inc.packs++
		inc.objects += p.Objects
	}
	c.setSize(inc, inc.diskBytes+grew)
	if place == placeDropping {
		c.setDropping(inc, inc.droppingBytes+grew)
	}
}

// installHanded installs p from the staging an adoption handed the round —
// the receiving node's own files, the bundle the host sealed and held to the
// row before it was handed — by linking them into place in the install's
// order, and reports whether it did. One it could not link is logged and
// left to the download, which needs nothing from it: the staging is dropped
// either way, and the directory is synced with the round's other installs.
func (c *Cache) installHanded(e *entry, inc *incarnation, place packPlace, p code.PackRow, st *staging) bool {
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
	c.counted(inc, place, p, true, p.Size)
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

// stagedPack is one bundle's three files in tmp/ — made by its round, then
// written, checked and closed by its download — until each is renamed into
// place: whatever is still here when the install ends is removed, and counted
// out of the bytes in flight.
type stagedPack struct {
	p     code.PackRow
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
		// The files are removed next, and one already closed answers an
		// error that says nothing.
		_ = f.Close()
	}
	s.files = nil
	s.remove(c)
}

// stageBundle makes p's three files in tmp/, open for its GET to write, and
// counts p into the bytes in flight. The round calls it as it hands p out — on
// its own goroutine, in its order (install says why) — and whatever of them is
// not placed is removed (abandon, placePack).
func (c *Cache) stageBundle(id string, p code.PackRow) (*stagedPack, error) {
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

// download streams s's bundle from the object store into its three files,
// split as its row says and teed through code's seal, checks the seal against
// the row, and closes the files: making them durable is placePack's, on no GET
// slot. getSlot waits for the GET's turn, share and node slot — the files
// already made — and answers the slot's copy buffer and what gives the share
// and the slot back, called the moment the store's reader is closed. Whatever
// fails abandons s.
func (c *Cache) download(ctx context.Context, id string, s *stagedPack,
	getSlot func(context.Context) ([]byte, func(), error)) error {

	p := s.p
	fail := func(err error) error {
		s.abandon(c)
		return err
	}
	if c.faults != nil {
		if err := c.faults.Hit(ctx, codefault.SyncDownload, 0); err != nil {
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
	seal := code.NewSealWriter(ref)
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
	// WHOSE FAILURE A COPY'S IS, by which side of it failed — never by
	// elimination: the store's reader is the object store's (fromStore), and
	// a write into the staged files is this node's own disk, which told as
	// the store's sent an operator to the bucket for a full volume here.
	_, err = io.CopyBuffer(split, fromStore{rc}, buf)
	if cerr := rc.Close(); err == nil && cerr != nil {
		err = storeErr{cerr}
	}
	// The GET is over: what follows is this node's disk, and the slot is
	// another download's.
	giveBack()
	var store storeErr
	switch {
	case errors.Is(err, errBundleTooLong):
		return fail(c.rejected(id, p, fmt.Errorf("%w: %w", code.ErrBadBundle, err)))
	case errors.As(err, &store):
		return fail(c.storeFailure(id, p, store.err))
	case err != nil:
		return fail(fmt.Errorf("cache: sync %s: install pack %s: writing its files on this node: %w", id, p.PackID,
			err))
	}
	sealed, err := seal.Seal()
	if err == nil {
		err = code.CheckBundle(ref, sealed)
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
// absent and damaged are DEFINITE (an *ErrPackMissing — logged by the round
// once it is final: a damaged pack at once, and an absent one only once a
// read at the log's head made after this answer, whatever level the round
// read at, still names it), anything else the store failing, wrapped as
// ErrStoreRead.
func (c *Cache) storeFailure(id string, p code.PackRow, err error) error {
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

// rejected is a bundle whose verified bytes are not its row's, logged under
// the one name the node gives that fact: the code host logs it again, by the
// same name, for each git request the failed sync answers, and names it to the
// client — code_pack_rejected is the receiving node refusing a push's own
// pack, a different fault with a different remedy.
func (c *Cache) rejected(id string, p code.PackRow, err error) error {
	c.log.Error("code_pack_checksum_mismatch", "repo", id, "pack_id", p.PackID, "object", p.Object.String(),
		"error", err)
	return &ErrCorruptPack{Repo: id, Pack: p, Err: err}
}

// errBundleTooLong is a stream longer than the bundle its row lays out.
var errBundleTooLong = errors.New("the stream is longer than the bundle its row lays out")

// splitter writes a bundle's bytes to its three files in turn — each part
// takes exactly its row's size — teeing every byte through the seal.
type splitter struct {
	seal  *code.SealWriter
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
