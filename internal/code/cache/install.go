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

	"golang.org/x/sync/errgroup"
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
// exists only once a node slot is in hand, and a worker installs the one pack
// it downloaded: an install is three renames, so tmp/ holds at most one pack a
// worker.
//
// A NODE SLOT IS A DOWNLOAD AND ITS BUFFER, and nothing else. Its copy buffer
// is handed out with it and back with it, so the node's download memory is
// NodeFetches buffers however many rounds wait — a node joining a fleet asked
// for 500 cold repositories at once holds 8 MiB of them, not a buffer for
// every round's every worker. And it goes back the moment the store's reader
// closes: the split's seal check, the chmod and the three fsyncs that follow
// are local disk work, and a GET slot spent on them is a GET the node does not
// make.

// copyBuffer is the buffer one download is copied through: 256 KiB, so a
// 150 KB bundle is one read of the store's stream and a large one a few
// thousand. One a node slot ([fetchSlots]), so NodeFetches of them — 8 MiB —
// is every download buffer the node holds at once.
const copyBuffer = 256 << 10

// fetchSlots is the node's downloads: NodeFetches slots, each handed out with
// the copy buffer its download runs through and taken back with it.
type fetchSlots struct {
	sem *semaphore.Weighted
	// free holds the buffers no slot holds: never more than the slots, so
	// buffers made and buffers kept together never exceed them.
	free chan []byte
	// out is the buffers handed out now, most the most at once, and waiting
	// the downloads waiting for a slot — what the suite holds to the node's
	// bound.
	out, most, waiting atomic.Int64
}

func newFetchSlots(n int64) *fetchSlots {
	return &fetchSlots{sem: semaphore.NewWeighted(n), free: make(chan []byte, n)}
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
	n := s.out.Add(1)
	for {
		most := s.most.Load()
		if n <= most || s.most.CompareAndSwap(most, n) {
			break
		}
	}
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

	// The round's share, then a node slot, then a worker: a round waiting
	// for either holds one goroutine — this one — and no buffer. A worker
	// that FAILS keeps its share, so the round hands out nothing more; its
	// failure ends gctx, which ends the wait below.
	var added atomic.Int64
	share := semaphore.NewWeighted(int64(c.fetchConcurrency))
	g, gctx := errgroup.WithContext(ctx)
	var waited error
	for _, p := range todo {
		if err := gctx.Err(); err != nil {
			waited = context.Cause(gctx)
			break
		}
		// A pack this node's own push wrote, handed to the round by its
		// adoption (adopt.go), is installed from those files — three links
		// rather than a GET — once the round reaches it.
		if st := c.claimHanded(e, p); st != nil && c.installHanded(e, inc, packDir, p, st) {
			added.Add(1)
			continue
		}
		if err := share.Acquire(gctx, 1); err != nil {
			waited = context.Cause(gctx)
			break
		}
		buf, err := c.fetches.take(gctx)
		if err != nil {
			waited = context.Cause(gctx)
			break
		}
		g.Go(func() error {
			if err := c.installOne(gctx, e, inc, packDir, p, buf); err != nil {
				return err
			}
			added.Add(1)
			share.Release(1)
			return nil
		})
	}
	err := g.Wait()
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

// installOne downloads one pack bundle into three files in tmp/ through buf —
// the node slot's, handed back once the store's reader closes — checks it,
// and renames it into place: the .pack, then the .rev, then the .idx — the
// .idx LAST, because git counts a pack present once its .idx exists.
func (c *Cache) installOne(ctx context.Context, e *entry, inc *incarnation, packDir string,
	p code.PackRow, buf []byte) error {

	giveBack := sync.OnceFunc(func() { c.fetches.give(buf) })
	defer giveBack()
	staged, err := c.download(ctx, e.id, p, buf, giveBack)
	if err != nil {
		return err
	}
	c.hookInstall(installFetched, packDir)
	defer staged.remove(c)
	for _, ext := range []string{".pack", ".rev", ".idx"} {
		dst := filepath.Join(packDir, "pack-"+p.GitChecksum+ext)
		if err := os.Rename(staged.path(ext), dst); err != nil {
			return fmt.Errorf("cache: sync %s: install pack %s: renaming %s over %s: %w",
				e.id, p.PackID, staged.path(ext), dst, err)
		}
		staged.placed(ext)
		c.hookInstall(strings.TrimPrefix(ext, "."), packDir)
	}
	e.mu.Lock()
	inc.packs++
	inc.objects += p.Objects
	c.setSize(inc, inc.diskBytes+p.Size, inc.metaBytes)
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

// stagedPack is one bundle's three files in tmp/, until each is renamed into
// place: whatever is still here when the install ends is removed, and
// counted out of the bytes in flight.
type stagedPack struct {
	stem  string
	sizes map[string]int64
	left  map[string]bool
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

// download streams p's bundle from the object store into three files in
// tmp/, split as its row says and teed through code's seal, and checks the
// seal against the row. Every file is made durable before it is returned.
// giveBack returns the node slot buf came with, and is called the moment the
// store's reader is closed.
func (c *Cache) download(ctx context.Context, id string, p code.PackRow, buf []byte,
	giveBack func()) (*stagedPack, error) {

	if c.faults != nil {
		if err := c.faults.Hit(ctx, codefault.SyncDownload, 0); err != nil {
			return nil, fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err)
		}
	}
	ref := p.Ref()
	f, err := os.CreateTemp(filepath.Join(c.dir, tmpDir), "pack-*.pack")
	if err != nil {
		return nil, fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err)
	}
	stem := strings.TrimSuffix(f.Name(), ".pack")
	s := &stagedPack{
		stem:  stem,
		sizes: map[string]int64{".pack": ref.PackBytes(), ".idx": ref.IndexBytes, ".rev": ref.ReverseBytes},
		left:  map[string]bool{".pack": true},
	}
	c.tmpBytes.Add(p.Size)
	files := []*os.File{f}
	closeAll := func() {
		for _, f := range files {
			// A failed install: the files are removed next, and one already
			// closed answers an error that says nothing.
			_ = f.Close()
		}
	}
	for _, ext := range []string{".idx", ".rev"} {
		g, oerr := os.OpenFile(stem+ext, os.O_CREATE|os.O_EXCL|os.O_WRONLY, indexMode)
		if oerr != nil {
			closeAll()
			s.remove(c)
			return nil, fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, oerr)
		}
		s.left[ext] = true
		files = append(files, g)
	}
	fail := func(err error) (*stagedPack, error) {
		closeAll()
		s.remove(c)
		return nil, err
	}

	rc, err := c.objects.Open(ctx, ref.Content())
	if err != nil {
		return fail(c.storeFailure(id, p, err))
	}
	seal := code.NewSealWriter(ref)
	split := &splitter{seal: seal, parts: []part{
		{w: files[0], left: ref.PackBytes()},
		{w: files[1], left: ref.IndexBytes},
		{w: files[2], left: ref.ReverseBytes},
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
	if err = f.Chmod(packMode); err != nil {
		return fail(fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err))
	}
	for _, f := range files {
		err = f.Sync()
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(fmt.Errorf("cache: sync %s: install pack %s: %w", id, p.PackID, err))
		}
	}
	return s, nil
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
