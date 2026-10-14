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

	"golang.org/x/sync/semaphore"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
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
// GETs bring a bundle every 2 ms at sixteen and a 30 ms first byte, while
// this node's own side took 1.1 to 1.5 ms a bundle from memory (Measured) —
// so a second share covers a share's disk work, and a disk slower than its
// link holds the GETs back rather than growing a backlog. It is what bounds
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
// hold, and answers how many it added and whether it removed any files.
//
// A pack is installed when its .idx, .pack and .rev all exist — git counts a
// pack once its .idx does — and, on a FULL round, each at the size its row
// says: three stats a pack, once per repository per process, so a file a disk
// fault truncated is caught here rather than by the next client. A pack at the
// wrong size is REMOVED before any download starts — with the
// multi-pack-index and the commit-graph, which may name it — and installed
// again.
//
// A failure removes every temporary file the round made; the packs installed
// before it stay, named by rows, and the next round skips them.
func (c *Cache) install(ctx context.Context, e *entry, inc *incarnation, snap snapshot) (int, bool, error) {
	packDir := filepath.Join(inc.dir, "objects", "pack")
	var todo, damaged []code.PackRow
	for _, p := range snap.packs {
		held, err := installed(packDir, p, snap.full)
		switch {
		case err != nil:
			return 0, false, fmt.Errorf("cache: sync %s: install: %w", e.id, err)
		case held == packWhole:
			continue
		case held == packWrongSize:
			damaged = append(damaged, p)
		}
		todo = append(todo, p)
	}
	removed := false
	if len(damaged) > 0 {
		if err := c.removePacks(inc, damaged); err != nil {
			return 0, false, fmt.Errorf("cache: sync %s: install: %w", e.id, err)
		}
		removed = true
	}
	if len(todo) == 0 {
		return 0, removed, nil
	}
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
	var added atomic.Int64
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
	turn := make(chan struct{})
	close(turn) // the first worker's turn is now
	for _, p := range todo {
		if err := rctx.Err(); err != nil {
			waited = context.Cause(rctx)
			break
		}
		// A pack this node's own push wrote, handed to the round by its
		// adoption (adopt.go), is installed from those files — three links
		// rather than a GET — once the round reaches it.
		if st := c.claimHanded(e, p); st != nil && c.installHanded(e, inc, packDir, p, st) {
			added.Add(1)
			continue
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
			if err != nil {
				fail(err)
				return
			}
			c.hookInstall(installFetched, packDir)
			if err := c.placePack(e, inc, packDir, staged); err != nil {
				fail(err)
				return
			}
			added.Add(1)
		})
	}
	workers.Wait()
	err := failed
	if err == nil && waited != nil {
		err = fmt.Errorf("cache: sync %s: install: %w", e.id, waited)
	}
	if n := added.Load(); n > 0 {
		// The renames are durable before any ref names what they installed.
		if serr := syncDir(packDir); serr != nil && err == nil {
			err = fmt.Errorf("cache: sync %s: install: %w", e.id, serr)
		}
	}
	return int(added.Load()), removed, err
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

// placePack makes s durable and renames it into packDir, counting the pack into
// inc: its three files synced at once while still in tmp/, then renamed
// .pack, .rev, and the .idx LAST, because git counts a pack present once its
// .idx exists. Whatever of s is not placed is removed, failure or not.
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
func (c *Cache) placePack(e *entry, inc *incarnation, packDir string, s *stagedPack) error {
	defer s.remove(c)
	if err := c.syncStaged(s); err != nil {
		return fmt.Errorf("cache: sync %s: install pack %s: %w", e.id, s.p.PackID, err)
	}
	for _, ext := range []string{".pack", ".rev", ".idx"} {
		dst := filepath.Join(packDir, "pack-"+s.p.GitChecksum+ext)
		if err := os.Rename(s.path(ext), dst); err != nil {
			return fmt.Errorf("cache: sync %s: install pack %s: renaming %s over %s: %w",
				e.id, s.p.PackID, s.path(ext), dst, err)
		}
		s.placed(ext)
		c.hookInstall(strings.TrimPrefix(ext, "."), packDir)
	}
	e.mu.Lock()
	inc.packs++
	inc.objects += s.p.Objects
	c.setSize(inc, inc.diskBytes+s.p.Size, inc.metaBytes)
	e.mu.Unlock()
	return nil
}

// installHanded installs p from the staging an adoption handed the round —
// the receiving node's own files, the bundle the host sealed and held to the
// row before it was handed — by linking them into place in the install's
// order, and reports whether it did. One it could not link is logged and
// left to the download, which needs nothing from it: the staging is dropped
// either way, and the directory is synced with the round's other installs.
func (c *Cache) installHanded(e *entry, inc *incarnation, packDir string, p code.PackRow, st *staging) bool {
	defer c.dropStaging(st)
	linked, err := linkBundle(st.stem, packDir, p.GitChecksum, func(ext string) {
		c.hookInstall(strings.TrimPrefix(ext, "."), packDir)
	})
	if err != nil {
		c.log.Warn("code_cache_adopt_failed", "repo", e.id, "pack_id", p.PackID, "error", err)
	}
	if !linked {
		return false
	}
	e.mu.Lock()
	inc.packs++
	inc.objects += p.Objects
	c.setSize(inc, inc.diskBytes+p.Size, inc.metaBytes)
	e.mu.Unlock()
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
	split := &splitter{seal: seal, parts: []part{
		{w: s.files[0], left: ref.PackBytes()},
		{w: s.files[1], left: ref.IndexBytes},
		{w: s.files[2], left: ref.ReverseBytes},
	}}
	_, err = io.CopyBuffer(split, rc, buf)
	if cerr := rc.Close(); err == nil {
		err = cerr
	}
	// The GET is over: what follows is this node's disk, and the slot is
	// another download's.
	giveBack()
	switch {
	case errors.Is(err, errBundleTooLong):
		return fail(c.rejected(id, p, fmt.Errorf("%w: %w", code.ErrBadBundle, err)))
	case err != nil:
		return fail(c.storeFailure(id, p, err))
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

// storeFailure is what the object store's failure to hand p over means:
// absent and damaged are DEFINITE (an *ErrPackMissing — logged by the round
// once it is final, since an absent pack in a round that was not
// linearizable is asked about again first), anything else the store
// failing, wrapped.
func (c *Cache) storeFailure(id string, p code.PackRow, err error) error {
	var damaged bool
	switch {
	case errors.Is(err, objstore.ErrNotFound):
	case errors.Is(err, objstore.ErrCorrupt):
		damaged = true
	default:
		return fmt.Errorf("cache: sync %s: install: download pack %s (object %s): %w", id, p.PackID, p.Object, err)
	}
	return &ErrPackMissing{Repo: id, Pack: p, Damaged: damaged, Err: err}
}

// rejected is a bundle whose verified bytes are not its row's.
func (c *Cache) rejected(id string, p code.PackRow, err error) error {
	c.log.Error("code_pack_rejected", "repo", id, "pack_id", p.PackID, "object", p.Object.String(), "error", err)
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

// removePacks takes packs out of inc before they are installed again: the
// multi-pack-index and the commit-graph first — an index naming a missing
// pack fails every read of the repository — then each pack's three files,
// moved into trash/. Only a full round removes a pack, and it measures the
// incarnation's counts afresh at its end (sweepPacks).
func (c *Cache) removePacks(inc *incarnation, packs []code.PackRow) error {
	if err := c.dropIndexes(inc); err != nil {
		return err
	}
	packDir := filepath.Join(inc.dir, "objects", "pack")
	for _, p := range packs {
		for _, ext := range []string{".idx", ".pack", ".rev"} {
			path := filepath.Join(packDir, "pack-"+p.GitChecksum+ext)
			err := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path))
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	return syncDir(packDir)
}
