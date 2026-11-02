package cache

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"golang.org/x/sys/unix"
)

// THE CODE INDEX (DerivedCodeIndex, M6-D12): the trigram index of a
// `search: indexed` repository's default branch, kept on the git nodes its
// search is placed on — the first code.IndexHolders of the repository's
// rendezvous order over the live git nodes — INSIDE the incarnation, under
// crewlet/codeindex, so it is counted by the budget, leaves with the
// repository and crosses a compaction's swap and a rebuild by hard link
// (S18): its postings are keyed by BLOB, which no pack moving changes.
//
// What the index holds is its RECORD (codeIndexState): the tip it was read
// at, the TIP MAP naming that tip's every file and its blob, and the
// SEGMENTS, each the trigrams of a batch of blobs. A new tip indexes ONLY
// THE BLOBS IT BROUGHT — every blob a segment already holds is known,
// skipped ones included — into new segments of at most
// codeindex.SegmentSourceBytes of source each, read through ONE cat-file in
// the maintenance lane; a merge of the newest segments, keeping only the
// blobs the tip still names, keeps them geometric (codeindex.MaxSegments).
// The record changes as one step under the entry's lock, and the files the
// new record no longer names wait for the next state written (retiredRuns),
// as a retired run of the index of pushes does.

// codeIndexFile is the file that writes the code index: its declaration's
// Writer.File, which the writers gate holds to its row.
const codeIndexFile = "internal/code/cache/codeindex.go"

// codeIndexDir is the index's directory, relative to its incarnation.
const codeIndexDir = engineRoot + "/codeindex"

// IndexPoll is how often the indexer looks at every repository it keeps an
// index of, whatever nudged it meanwhile: the compactor's poll
// (compact.Poll), so a git node whose nudges were lost catches up in the
// time a compaction would.
const IndexPoll = time.Minute

// CodeIndexShare is the share of the cache's budget the node's code indexes
// may hold — a third, trimmed before any repository is evicted: one dropped
// is built again from the blobs at the next tip, at 78 seconds of a core a
// gigabyte of source, where a repository evicted is fetched again whole. A
// third holds every index a full cache can carry: BenchmarkCodeIndex
// measured an index at 0.147 bytes a source byte and the source deflated as
// a pack holds it at 0.327, so an index is at most 0.45 of its copy — of a
// copy holding its tip and no history — and the indexes of a cache full of
// such copies 31% of its budget. History only makes the copies larger.
// Re-derived from the PROVISIONAL 4 (M3-O6), which rested on P8's estimate of
// a sixth of the working set and trimmed the indexes of young repositories.
const CodeIndexShare = 3

// codeIndexState is the code index's record, which only its declaration's
// entry carries (derivedState.CodeIndex), and only with its Step: a set of
// files with no main one, so the record is what says it is there.
// IMMUTABLE once stored, as the index of pushes' is.
type codeIndexState struct {
	// Tip is the commit the tip map was read at, forty hex digits.
	Tip string `json:"tip"`
	// Map is the tip map's file.
	Map codeIndexPart `json:"map"`
	// Segments is every segment, oldest first.
	Segments []codeIndexPart `json:"segments,omitempty"`
	// Next is the number the next file of the index takes: above every one
	// it names.
	Next uint64 `json:"next"`
}

// codeIndexPart is one file of the index: its number and its bytes.
type codeIndexPart struct {
	N     uint64 `json:"n"`
	Bytes int64  `json:"bytes"`
}

// bytes is every file the record names.
func (s *codeIndexState) bytes() int64 {
	n := s.Map.Bytes
	for _, g := range s.Segments {
		n += g.Bytes
	}
	return n
}

// rels is every file the record names, relative to its incarnation.
func (s *codeIndexState) rels() []string {
	out := []string{codeIndexMapRel(s.Map.N)}
	for _, g := range s.Segments {
		out = append(out, codeIndexSegRel(g.N))
	}
	return out
}

// check refuses a record no write of this build leaves: a tip that is no
// commit id, a file numbered at or above Next or twice, a segment count past
// codeindex.MaxSegments, or bytes other than want, the entry's.
func (s *codeIndexState) check(want int64) error {
	if _, err := codeindex.ParseOID(s.Tip); err != nil {
		return fmt.Errorf("a code index read at %q, which is no commit id", s.Tip)
	}
	if len(s.Segments) > codeindex.MaxSegments {
		return fmt.Errorf("a code index of %d segments, past the %d one keeps", len(s.Segments),
			codeindex.MaxSegments)
	}
	seen := map[uint64]bool{}
	for _, p := range append([]codeIndexPart{s.Map}, s.Segments...) {
		if p.N == 0 || p.N >= s.Next || seen[p.N] || p.Bytes <= 0 {
			return fmt.Errorf("a code index naming file %d at %d bytes under next %d", p.N, p.Bytes, s.Next)
		}
		seen[p.N] = true
	}
	if s.bytes() != want {
		return fmt.Errorf("a code index of %d bytes, recorded at %d", s.bytes(), want)
	}
	return nil
}

// codeIndexSegRel and codeIndexMapRel are a segment's and a tip map's paths,
// relative to their incarnation.
func codeIndexSegRel(n uint64) string {
	return filepath.Join(filepath.FromSlash(codeIndexDir), "seg."+strconv.FormatUint(n, 10))
}

func codeIndexMapRel(n uint64) string {
	return filepath.Join(filepath.FromSlash(codeIndexDir), "tip."+strconv.FormatUint(n, 10))
}

// codeIndexRecord is inc's code index, nil for none; e.mu is held.
func (inc *incarnation) codeIndexRecord() *codeIndexState {
	return inc.derived[DerivedCodeIndex].CodeIndex
}

// IndexedTip is the commit a repository's code index answers for, and how many
// files that commit holds, on this node.
type IndexedTip struct {
	// Commit is the commit the copy's index answers for; "" where the copy
	// keeps none.
	Commit string
	// Files is how many files Commit holds; -1 until a reader opened the
	// index.
	Files int
	// Holder is whether this node is one of the repository's first
	// code.IndexHolders git nodes (estate.Router.GitOrder), whose indexer
	// keeps an index of it. False where the order cannot be read now: a grep
	// answers the same hits.
	Holder bool
	// PassesOn is whether this node is the FIRST of those holders and the
	// order names a second: what lets a search of the default branch that
	// this node's index does not answer for yet send the walk on to the
	// second holder (code.UnreadyIndexBuilding) rather than read it with git
	// grep. Never the second holder's, nor the only one's: no holder follows
	// either in the walk, so a refusal there would leave a search unanswered
	// until the indexer caught up — on a fleet of one git node, after every
	// push.
	PassesOn bool
}

// CodeIndexView is a code index as a search reads it: the tip map and every
// segment, held to their trailers and opened. Immutable, so any number of
// searches share one.
type CodeIndexView struct {
	Map      *codeindex.TipMap
	Segments []*codeindex.Segment
}

// IndexTip is the tip s's copy keeps an index of, and whether this node is
// one of the repository's index holders — false for a copy keeping none on a
// node that holds none, or s not handed out.
func (c *Cache) IndexTip(s Synced) (IndexedTip, bool) {
	if c.handedOut(s, "a read of the code index") != nil {
		return IndexedTip{}, false
	}
	var t IndexedTip
	t.Holder, t.PassesOn = c.indexHolder(s.Repo.ID)
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	rec := s.inc.codeIndexRecord()
	switch {
	case rec == nil:
		return t, t.Holder
	case s.inc.codeIndex != nil && s.inc.codeIndex.rec == rec:
		t.Commit, t.Files = rec.Tip, s.inc.codeIndex.view.Map.Len()
	default:
		t.Commit, t.Files = rec.Tip, -1
	}
	return t, true
}

// indexHolder reports whether this node is one of repoID's first
// code.IndexHolders git nodes, and whether it is the first of them with
// another after it (IndexedTip.PassesOn) — both false where the cache keeps
// no index or the order cannot be read.
func (c *Cache) indexHolder(repoID string) (holder, passesOn bool) {
	if c.placement == nil {
		return false, false
	}
	order, err := c.placement.Order(repoID)
	if err != nil {
		return false, false
	}
	holders := order[:min(len(order), code.IndexHolders)]
	return slices.Contains(holders, c.placement.Node), len(holders) > 1 && holders[0] == c.placement.Node
}

// CodeIndex is the code index of s's copy, mapped and opened — its files
// held to their trailers once each, the first time a reader of this process
// opens them — and VALID WHILE s IS HELD: its files stay mapped for s's
// reader until s is released (openedCodeIndex). False where the copy keeps
// none, where the node's bound on mappings is held whole by readers, or where
// a file of it cannot be read — which also drops the index for the indexer to
// build again; a search answers each by reading the tip with git grep.
func (c *Cache) CodeIndex(s Synced) (*CodeIndexView, bool) {
	if c.handedOut(s, "a read of the code index") != nil {
		return nil, false
	}
	e, inc := s.entry, s.inc
	for range 3 {
		e.mu.Lock()
		rec := inc.codeIndexRecord()
		if rec == nil {
			e.mu.Unlock()
			return nil, false
		}
		if o := inc.codeIndex; o != nil {
			if o.rec == rec && c.holdCodeIndex(inc, s.Repairer, o) {
				e.mu.Unlock()
				return o.view, true
			}
			// An opening of an older record, or one the bound unmapped a
			// file of while nobody held it: opened again.
			c.forgetCodeIndex(inc)
		}
		e.mu.Unlock()
		o, err := c.openCodeIndex(e, inc, rec)
		switch {
		case errors.Is(err, errMapsFull):
			// Every mapping the node may make is a reader's: this search
			// reads the tip with git grep, and the index stays.
			return nil, false
		case err != nil:
			c.log.Warn("code_cache_codeindex_unreadable", "repo", e.id, "incarnation", inc.n, "error", err)
			c.dropCodeIndex(e, inc, rec)
			return nil, false
		}
		e.mu.Lock()
		if inc.codeIndexRecord() != rec || inc.discarded || inc.codeIndex != nil {
			// The record moved, or another reader installed its own
			// opening, while this one mapped: this one is nobody's.
			e.mu.Unlock()
			c.unmapCodeIndex(o)
			continue
		}
		inc.codeIndex = o
		c.holdCodeIndex(inc, s.Repairer, o) // made now and held by nobody: the bound took nothing of it
		e.mu.Unlock()
		return o.view, true
	}
	return nil, false
}

// openedCodeIndex is a record's files MAPPED and opened, kept on the
// incarnation for the next reader while the record is the incarnation's.
//
// MAPPED, NEVER READ INTO THE HEAP: a code index is up to a third of the
// cache's budget on disk (CodeIndexShare), and a node's searches touch a few
// of its pages — the lists a plan names, the tip map's entries of its
// candidates. Read whole and kept for the next search, every index a node's
// searches had opened stayed in its heap (a 100 GiB cache's 33 GiB of
// indexes, in the worst case); mapped, the kernel keeps the pages a search
// reads in its page cache and gives them back under pressure. Each file is
// one mapping of the process, counted against the node's bound on mappings
// with the runs of the indexes of pushes (runmaps.go), held by a reader for
// as long as its Synced is (Cache.release lets it go) and unmapped past the
// bound only while nobody holds it.
type openedCodeIndex struct {
	rec  *codeIndexState
	view *CodeIndexView
	maps []*mappedRun
}

// holdCodeIndex holds every mapping of o for the reader by, which its
// release lets go (releaseRuns), or answers false having held none, where
// one of them can be held no more; e.mu is held.
func (c *Cache) holdCodeIndex(inc *incarnation, by gitproc.Repairer, o *openedCodeIndex) bool {
	for i, m := range o.maps {
		if !c.maps.hold(m) {
			for _, h := range o.maps[:i] {
				if c.maps.let(h) {
					h.unmap()
				}
			}
			return false
		}
	}
	if inc.runHolds == nil {
		inc.runHolds = map[gitproc.Repairer][]*mappedRun{}
	}
	inc.runHolds[by] = append(inc.runHolds[by], o.maps...)
	return true
}

// forgetCodeIndex lets go of inc's opened code index, where it is not the
// record's or inc is discarded — or at once, where the record is about to
// change (setCodeIndex) — unmapping every file nobody holds now and leaving
// a held one to its last release (runMaps.forget); e.mu is held.
func (c *Cache) forgetCodeIndex(inc *incarnation) {
	o := inc.codeIndex
	if o == nil {
		return
	}
	inc.codeIndex = nil
	for _, m := range o.maps {
		if c.maps.forget(m) {
			m.unmap()
		}
	}
}

// unmapCodeIndex unmaps an opening nobody holds or ever held: one made for a
// record that moved while it was made.
func (c *Cache) unmapCodeIndex(o *openedCodeIndex) {
	for _, m := range o.maps {
		c.maps.drop(m)
	}
}

// openCodeIndex maps every file rec names, each held to its SHA-256 trailer
// unless this process wrote or checked it at the identity it has (vouch),
// and opens them. errMapsFull where the node's bound on mappings has no room.
func (c *Cache) openCodeIndex(e *entry, inc *incarnation, rec *codeIndexState) (*openedCodeIndex, error) {
	o := &openedCodeIndex{rec: rec, view: &CodeIndexView{}}
	mapFile := func(rel string, size int64) ([]byte, error) {
		path := filepath.Join(inc.dir, rel)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() != size || size == 0 {
			return nil, fmt.Errorf("%s is %d bytes, and the index names it at %d", rel, info.Size(), size)
		}
		if err := c.maps.reserve(); err != nil {
			return nil, err
		}
		data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			c.maps.unreserve()
			return nil, fmt.Errorf("mapping %s: %w", rel, err)
		}
		o.maps = append(o.maps, &mappedRun{data: data})
		id := idOf(info)
		e.mu.Lock()
		vouched := vouchedAt(inc, rel, id)
		e.mu.Unlock()
		if !vouched {
			if err := runTrailerHolds(data); err != nil {
				return nil, fmt.Errorf("%s: %w", rel, err)
			}
			e.mu.Lock()
			vouch(inc, rel, id)
			e.mu.Unlock()
		}
		return data, nil
	}
	fail := func(err error) (*openedCodeIndex, error) {
		c.unmapCodeIndex(o)
		return nil, err
	}
	data, err := mapFile(codeIndexMapRel(rec.Map.N), rec.Map.Bytes)
	if err != nil {
		return fail(err)
	}
	if o.view.Map, err = codeindex.OpenTipMap(data); err != nil {
		return fail(err)
	}
	for _, g := range rec.Segments {
		data, err := mapFile(codeIndexSegRel(g.N), g.Bytes)
		if err != nil {
			return fail(err)
		}
		seg, err := codeindex.OpenSegment(data)
		if err != nil {
			return fail(err)
		}
		o.view.Segments = append(o.view.Segments, seg)
	}
	return o, nil
}

// runTrailerHolds reports whether data ends in the SHA-256 of every byte
// before it: the run-trailer check (CheckRunTrailer).
func runTrailerHolds(data []byte) error {
	if len(data) < sha256.Size {
		return errors.New("shorter than its trailer")
	}
	body, trailer := data[:len(data)-sha256.Size], data[len(data)-sha256.Size:]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], trailer) {
		return errors.New("its trailer is not the SHA-256 of its bytes")
	}
	return nil
}

// setCodeIndex stores rec as inc's code index — nil drops it — its files
// the new record no longer names among inc's retired runs, gone with the
// next state written; e.mu is held.
func (c *Cache) setCodeIndex(inc *incarnation, rec *codeIndexState) {
	if old := inc.codeIndexRecord(); old != nil {
		for _, rel := range old.rels() {
			if rec == nil || !slices.Contains(rec.rels(), rel) {
				inc.retiredRuns = append(inc.retiredRuns, filepath.Join(inc.dir, rel))
			}
		}
	}
	c.forgetCodeIndex(inc)
	if rec == nil {
		c.setDerived(inc, DerivedCodeIndex, derivedState{}, false)
		return
	}
	c.setDerived(inc, DerivedCodeIndex, derivedState{Bytes: rec.bytes(), Step: StepCodeIndex, CodeIndex: rec}, true)
}

// dropCodeIndex drops inc's code index where it is still rec, and writes the
// state that names it no more.
func (c *Cache) dropCodeIndex(e *entry, inc *incarnation, rec *codeIndexState) {
	e.mu.Lock()
	stale := inc.codeIndexRecord() != rec || inc.discarded
	if !stale {
		c.setCodeIndex(inc, nil)
	}
	e.mu.Unlock()
	if !stale {
		if err := c.persist(context.WithoutCancel(context.Background()), e, inc, false); err != nil {
			c.log.Warn("code_cache_codeindex_drop_failed", "repo", e.id, "error", err)
		}
	}
}

// IndexOutcome is what one Index call did: the `outcome` of
// crewlet.code.index.builds.
type IndexOutcome string

const (
	// IndexCurrent: the index already answers for the branch's tip.
	IndexCurrent IndexOutcome = "current"
	// IndexBuilt: the index now answers for the branch's tip.
	IndexBuilt IndexOutcome = "built"
	// IndexNone: the repository keeps no index here — its search is grep,
	// or its default branch names no commit.
	IndexNone IndexOutcome = "none"
)

// treeFile is one file of a tip, as ls-tree -r -l names it.
type treeFile struct {
	path string
	blob codeindex.OID
	size int64
}

// Index brings the code index of s's copy to its default branch's tip: the
// tip's files listed, the blobs no segment holds read through one cat-file
// and indexed into new segments, the newest segments merged while they are
// geometric, and the tip map written — then the record stored as one step,
// where the copy is still the entry's and its index still the one the build
// read. A copy whose repository is searched with grep keeps none, and one it
// keeps is dropped.
func (c *Cache) Index(ctx context.Context, s Synced) (IndexOutcome, error) {
	if err := c.handedOut(s, "a build of the code index"); err != nil {
		return "", err
	}
	e, inc := s.entry, s.inc
	e.mu.Lock()
	rec := inc.codeIndexRecord()
	e.mu.Unlock()
	if s.Repo.Search != code.SearchIndexed {
		if rec != nil {
			c.dropCodeIndex(e, inc, rec)
		}
		return IndexNone, nil
	}
	tip, err := c.indexTip(ctx, s)
	if err != nil || tip == "" {
		return IndexNone, err
	}
	if rec != nil && rec.Tip == tip {
		return IndexCurrent, nil
	}
	files, err := c.listTip(ctx, s, tip)
	if err != nil {
		return "", err
	}
	var view *CodeIndexView
	if rec != nil {
		if view, _ = c.CodeIndex(s); view == nil {
			e.mu.Lock()
			rec = inc.codeIndexRecord()
			e.mu.Unlock()
		}
	}
	b := &indexBuild{c: c, e: e, inc: inc, prior: rec, view: view}
	defer b.abandon()
	if err := b.segments(ctx, s, files); err != nil {
		return "", err
	}
	if err := b.merge(); err != nil {
		return "", err
	}
	if err := b.tipMap(tip, files); err != nil {
		return "", err
	}
	if !b.commit(tip) {
		return IndexCurrent, nil
	}
	if err := c.persist(ctx, e, inc, false); err != nil {
		return "", err
	}
	return IndexBuilt, nil
}

// indexTip is the commit s's default branch names, or "" where it names
// none.
func (c *Cache) indexTip(ctx context.Context, s Synced) (string, error) {
	res, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: s.Dir, Repairer: s.Repairer, Config: c.ServeConfig(),
		Args:  []string{"rev-parse", "--verify", "--quiet", "--end-of-options", "refs/heads/" + s.Repo.DefaultBranch + "^{commit}"},
		Class: gitproc.ClassMaintenance, Deadline: gitproc.PackWorkDeadline(0),
	})
	if err != nil {
		if res.Exit == 1 {
			return "", nil
		}
		return "", fmt.Errorf("cache: %s's default branch: %w", s.entry.id, err)
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// listTip is every regular file of tip, by ls-tree -r -l: its path, its
// blob and its size. A submodule's commit and a symbolic link are no file a
// search reads.
func (c *Cache) listTip(ctx context.Context, s Synced, tip string) ([]treeFile, error) {
	var out bytes.Buffer
	_, err := c.git.Run(ctx, gitproc.Cmd{
		Dir: s.Dir, Repairer: s.Repairer, Config: c.ServeConfig(), Stdout: &out,
		Args:  []string{"ls-tree", "-r", "-l", "-z", "--full-tree", tip},
		Class: gitproc.ClassMaintenance, Deadline: gitproc.PackWorkDeadline(0),
	})
	if err != nil {
		return nil, fmt.Errorf("cache: listing %s at %s: %w", s.entry.id, tip, err)
	}
	var files []treeFile
	for _, rec := range bytes.Split(out.Bytes(), []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 4 {
			return nil, fmt.Errorf("cache: ls-tree of %s answered %q", s.entry.id, rec)
		}
		if fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") {
			continue
		}
		oid, err := codeindex.ParseOID(fields[2])
		if err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			return nil, err
		}
		files = append(files, treeFile{path: string(path), blob: oid, size: size})
	}
	return files, nil
}

// indexBuild is one Index call's files: those it wrote, which go into
// trash/ unless its commit stores a record naming them.
type indexBuild struct {
	c     *Cache
	e     *entry
	inc   *incarnation
	prior *codeIndexState
	view  *CodeIndexView
	// segs is the segments the next record names, oldest first, with each
	// opened; skipped is every blob the index skips, by id.
	segs    []builtSegment
	skipped map[codeindex.OID]bool
	// tip is every blob the tip names, by id: what a merge keeps.
	tip     map[codeindex.OID]bool
	tipPart codeIndexPart
	wrote   []string
	next    uint64
	stored  bool
}

type builtSegment struct {
	part codeIndexPart
	seg  *codeindex.Segment
}

// alloc is the number of the next file this build writes.
func (b *indexBuild) alloc() uint64 {
	if b.next == 0 {
		b.next = 1
		if b.prior != nil {
			b.next = b.prior.Next
		}
	}
	n := b.next
	b.next++
	return n
}

// place writes a file of the index through write, staged in tmp/ and
// renamed in at rel, and answers its bytes.
func (b *indexBuild) place(rel string, write func(io.Writer) error) (int64, []byte, error) {
	var buf bytes.Buffer
	if err := write(&buf); err != nil {
		return 0, nil, err
	}
	data := buf.Bytes()
	staged, err := b.c.stage(hostbox.FileMode, func(f *os.File) error {
		_, werr := f.Write(data)
		return werr
	})
	if err != nil {
		return 0, nil, err
	}
	dir := filepath.Join(b.inc.dir, filepath.FromSlash(codeIndexDir))
	if err := os.MkdirAll(dir, hostbox.DirMode); err != nil {
		return 0, nil, removeStaged(staged, err)
	}
	if err := b.c.place(staged, filepath.Join(b.inc.dir, rel)); err != nil {
		return 0, nil, err
	}
	b.wrote = append(b.wrote, rel)
	if err := syncDir(dir); err != nil {
		return 0, nil, err
	}
	return int64(len(data)), data, nil
}

// segments indexes every blob of files no segment of the prior index holds:
// those past codeindex.MaxFileBytes recorded unread, the rest read in id
// order through one cat-file, a segment written each time the builder has
// read codeindex.SegmentSourceBytes.
func (b *indexBuild) segments(ctx context.Context, s Synced, files []treeFile) error {
	b.skipped = map[codeindex.OID]bool{}
	b.tip = make(map[codeindex.OID]bool, len(files))
	for _, f := range files {
		b.tip[f.blob] = true
	}
	if b.view != nil {
		for i, seg := range b.view.Segments {
			b.segs = append(b.segs, builtSegment{part: b.prior.Segments[i], seg: seg})
			for j := range seg.Len() {
				if seg.Skipped(j) {
					b.skipped[seg.OID(j)] = true
				}
			}
		}
	}
	known := func(oid codeindex.OID) bool {
		for _, g := range b.segs {
			if _, ok := g.seg.Find(oid); ok {
				return true
			}
		}
		return false
	}
	sizes := map[codeindex.OID]int64{}
	for _, f := range files {
		if !known(f.blob) {
			sizes[f.blob] = f.size
		}
	}
	if len(sizes) == 0 {
		return nil
	}
	fresh := make([]codeindex.OID, 0, len(sizes))
	var read []codeindex.OID
	var readBytes int64
	for oid := range sizes {
		fresh = append(fresh, oid)
	}
	slices.SortFunc(fresh, func(a, b codeindex.OID) int { return bytes.Compare(a[:], b[:]) })
	for _, oid := range fresh {
		if sizes[oid] <= codeindex.MaxFileBytes {
			read = append(read, oid)
			readBytes += sizes[oid]
		}
	}
	if k := b.c.knobs; k != nil && k.indexReading != nil {
		k.indexReading(len(read))
	}
	blobs, finish := b.c.catBlobs(ctx, s, read, readBytes)
	defer finish()
	segmentBytes := int64(codeindex.SegmentSourceBytes)
	if k := b.c.knobs; k != nil && k.indexSegmentBytes > 0 {
		segmentBytes = k.indexSegmentBytes
	}
	builder := &codeindex.Builder{}
	flush := func() error {
		if builder.Len() == 0 {
			return nil
		}
		n := b.alloc()
		size, data, err := b.place(codeIndexSegRel(n), func(w io.Writer) error {
			return codeindex.WriteSegment(w, builder)
		})
		if err != nil {
			return err
		}
		seg, err := codeindex.OpenSegment(data)
		if err != nil {
			return err
		}
		b.segs = append(b.segs, builtSegment{part: codeIndexPart{N: n, Bytes: size}, seg: seg})
		builder = &codeindex.Builder{}
		return b.merge()
	}
	for _, oid := range fresh {
		if err := ctx.Err(); err != nil {
			return err
		}
		size := sizes[oid]
		if size > codeindex.MaxFileBytes {
			if err := builder.AddUnread(oid, size); err != nil {
				return err
			}
			b.skipped[oid] = true
			continue
		}
		data, err := blobs(oid)
		if err != nil {
			return err
		}
		if codeindex.Skip(int64(len(data)), data[:min(len(data), codeindex.BinaryProbeBytes)]) {
			b.skipped[oid] = true
		}
		if err := builder.Add(oid, data); err != nil {
			return err
		}
		if builder.SourceBytes() >= segmentBytes {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	return flush()
}

// catBlobs runs one `cat-file --batch` over oids, in their order, in the
// maintenance lane: blob answers the next one's bytes, each once, and
// finish ends the process, waiting for it.
func (c *Cache) catBlobs(ctx context.Context, s Synced, oids []codeindex.OID, total int64) (
	blob func(codeindex.OID) ([]byte, error), finish func()) {

	if len(oids) == 0 {
		return func(oid codeindex.OID) ([]byte, error) {
			return nil, fmt.Errorf("cache: blob %s was asked of no cat-file", oid)
		}, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		for _, oid := range oids {
			if _, err := io.WriteString(inW, oid.String()+"\n"); err != nil {
				return
			}
		}
		_ = inW.Close()
	}()
	go func() {
		_, err := c.git.Run(ctx, gitproc.Cmd{
			Dir: s.Dir, Repairer: s.Repairer, Config: c.ServeConfig(), Stdin: inR, Stdout: outW,
			Args:  []string{"cat-file", "--batch"},
			Class: gitproc.ClassMaintenance, Deadline: gitproc.PackWorkDeadline(total),
		})
		_ = outW.CloseWithError(errors.Join(err, io.EOF))
		_ = inR.Close()
		done <- err
	}()
	r := bufio.NewReader(outR)
	blob = func(want codeindex.OID) ([]byte, error) {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("cache: cat-file of %s: %w", s.entry.id, err)
		}
		f := strings.Fields(header)
		if len(f) != 3 || f[0] != want.String() || f[1] != "blob" {
			return nil, fmt.Errorf("cache: cat-file of %s answered %q for blob %s", s.entry.id, header, want)
		}
		size, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil || size > codeindex.MaxFileBytes {
			return nil, fmt.Errorf("cache: cat-file of %s answered %q for blob %s", s.entry.id, header, want)
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, fmt.Errorf("cache: cat-file of %s: %w", s.entry.id, err)
		}
		return data[:size], nil
	}
	finish = func() {
		cancel()
		_ = outR.Close()
		<-done
	}
	return blob, finish
}

// merge keeps the index's segments few and geometric: while
// codeindex.MergeNewest says so, the two newest are merged into one, keeping
// only the blobs the tip names — so a blob is rewritten a logarithmic number
// of times, and a blob the branch no longer holds leaves at its next merge.
// It is called after EACH segment the build writes (flush), as MergeNewest
// asks: called once after a build of many, it folded them into one from the
// end, rewriting what it had merged at every step.
func (b *indexBuild) merge() error {
	for n := len(b.segs); n >= 2; n = len(b.segs) {
		sizes := make([]int64, n)
		for i, g := range b.segs {
			sizes[i] = g.part.Bytes
		}
		if !codeindex.MergeNewest(sizes) {
			return nil
		}
		older, newer := b.segs[n-2], b.segs[n-1]
		num := b.alloc()
		size, data, err := b.place(codeIndexSegRel(num), func(w io.Writer) error {
			return codeindex.MergeSegments(w, []*codeindex.Segment{older.seg, newer.seg},
				func(oid codeindex.OID) bool { return b.tip[oid] })
		})
		if err != nil {
			return err
		}
		seg, err := codeindex.OpenSegment(data)
		if err != nil {
			return err
		}
		b.segs = append(b.segs[:n-2], builtSegment{part: codeIndexPart{N: num, Bytes: size}, seg: seg})
	}
	return nil
}

// tipMap writes the tip map of tip.
func (b *indexBuild) tipMap(tip string, files []treeFile) error {
	commit, err := codeindex.ParseOID(tip)
	if err != nil {
		return err
	}
	entries := make([]codeindex.TipEntry, len(files))
	for i, f := range files {
		entries[i] = codeindex.TipEntry{Path: f.path, Blob: f.blob, Skipped: b.skipped[f.blob]}
	}
	n := b.alloc()
	size, _, err := b.place(codeIndexMapRel(n), func(w io.Writer) error {
		return codeindex.WriteTipMap(w, commit, entries)
	})
	if err != nil {
		return err
	}
	b.tipPart = codeIndexPart{N: n, Bytes: size}
	return nil
}

// commit stores the record the build made, where the copy is still the
// entry's, no swap is leaving it, and its index is still the one the build
// read — and answers whether it did. Every file the build wrote is vouched
// for as this process's own.
func (b *indexBuild) commit(tip string) bool {
	b.c.vouchFiles(b.e, b.inc, b.wrote...)
	rec := &codeIndexState{Tip: tip, Map: b.tipPart, Next: b.next}
	for _, g := range b.segs {
		rec.Segments = append(rec.Segments, g.part)
	}
	e, inc := b.e, b.inc
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.codeIndexRecord() != b.prior || inc.discarded || inc.retired || e.swap != nil && e.swap.inc == inc {
		return false
	}
	b.c.setCodeIndex(inc, rec)
	b.stored = true
	return true
}

// abandon moves every file the build wrote into trash/ where no record
// names it: a build that failed, or whose commit found the index moved.
// Files the stored record does not name — a segment a later merge of the
// same build replaced — go too.
func (b *indexBuild) abandon() {
	var named []string
	if b.stored {
		b.e.mu.Lock()
		if rec := b.inc.codeIndexRecord(); rec != nil {
			named = rec.rels()
		}
		b.e.mu.Unlock()
	}
	var gone []string
	for _, rel := range b.wrote {
		if !slices.Contains(named, rel) {
			gone = append(gone, filepath.Join(b.inc.dir, rel))
		}
	}
	if len(gone) > 0 {
		b.c.trashRuns(b.e, b.inc, gone)
	}
}

// carryCodeIndex links every file of the old incarnation's code index into
// the staged one and carries its record (S18: no re-index after a
// compaction). An index a file of which is gone carries nothing: the next
// copy starts with none, which the indexer builds again.
func (s *swapping) carryCodeIndex(d *Derived) (bool, error) {
	s.e.mu.Lock()
	rec, st, gone := s.inc.codeIndexRecord(), s.inc.derived[d.Name], s.inc.historyGone
	s.e.mu.Unlock()
	if rec == nil || gone {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Join(s.staged, filepath.FromSlash(codeIndexDir)), hostbox.DirMode); err != nil {
		return false, err
	}
	var linked []string
	for _, rel := range rec.rels() {
		err := s.c.link(filepath.Join(s.inc.dir, rel), filepath.Join(s.staged, rel))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			for _, l := range linked {
				if rerr := os.Remove(filepath.Join(s.staged, l)); rerr != nil {
					return false, rerr
				}
			}
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

// seedCodeIndex is a rebuild's seeding of its code index (SeedLink): every
// file the replaced incarnation's record names hard-linked into the new one
// and the record carried, each file's vouch with it. A file that cannot be
// linked seeds nothing, and the indexer builds the index again.
func (c *Cache) seedCodeIndex(e *entry, replaced, inc *incarnation) {
	if d := c.image.decl(DerivedCodeIndex); d == nil || d.Seed != SeedLink {
		return
	}
	e.mu.Lock()
	rec, gone := replaced.codeIndexRecord(), replaced.historyGone
	e.mu.Unlock()
	if rec == nil || gone {
		return
	}
	dir := filepath.Join(inc.dir, filepath.FromSlash(codeIndexDir))
	if err := os.MkdirAll(dir, hostbox.DirMode); err != nil {
		c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", codeIndexDir, "error", err)
		return
	}
	ids := map[string]fileID{}
	for _, rel := range rec.rels() {
		if err := os.Link(filepath.Join(replaced.dir, rel), filepath.Join(inc.dir, rel)); err != nil {
			c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", rel, "error", err)
			return
		}
		ids[rel] = lstatID(filepath.Join(inc.dir, rel))
	}
	for _, d := range []string{dir, filepath.Dir(dir), inc.dir} {
		if err := syncDir(d); err != nil {
			c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", codeIndexDir, "error", err)
			return
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if replaced.codeIndexRecord() != rec {
		return
	}
	for rel, id := range ids {
		if vouchedAt(replaced, rel, id) {
			vouch(inc, rel, id)
		}
	}
	c.setCodeIndex(inc, rec)
}

// IndexFailed: the copy could not be synced or its index built, and the
// indexer's next look tries again.
const IndexFailed IndexOutcome = "failed"

// THE INDEXER (Start runs it on a cache with a Placement): every IndexPoll it
// lists the repositories searched by index (code.ReadIndexed, at STALE: a
// repository a stale page misses is listed at the next look) and, for each one
// this node is among the first code.IndexHolders of — its git order read
// again at every look, so a node that joins or leaves moves the index with the
// order — syncs its copy, FILLING ONE THIS NODE DOES NOT HOLD (M6-D12: the
// index's holder holds a copy deliberately, where a read fills one only on
// the home), and brings the index to the default branch's tip. Between looks,
// a round that moves a repository's refs NUDGES it (nudgeIndex), and only
// that repository is looked at. A repository this node is no longer a holder
// of has its index dropped where its copy keeps one: an index nobody's
// search reads is budget a held one needs, and a placement back finds the
// tip from nothing, which is what a new holder pays anyway.
func (c *Cache) indexer(ctx context.Context) {
	if c.placement == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(c.life, cancel)()
	var ticks <-chan time.Time
	if c.knobs != nil && c.knobs.indexTicks != nil {
		ticks = c.knobs.indexTicks
	} else {
		tick := time.NewTicker(IndexPoll)
		defer tick.Stop()
		ticks = tick.C
	}
	full := true
	for {
		if !c.enter() {
			return
		}
		if full {
			c.indexPass(ctx)
		} else {
			c.indexWantedPass(ctx)
		}
		c.leave()
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			full = true
		case <-c.indexKicks:
			full = false
		}
	}
}

// nudgeIndex asks the indexer to look at repoID before its next poll: a
// round moved its refs. Never blocks — a nudge the indexer has not taken yet
// carries this one — and costs nothing on a node that keeps no index.
func (c *Cache) nudgeIndex(repoID string) {
	if c.placement == nil {
		return
	}
	c.indexMu.Lock()
	if c.indexWanted == nil {
		c.indexWanted = map[string]bool{}
	}
	c.indexWanted[repoID] = true
	c.indexMu.Unlock()
	select {
	case c.indexKicks <- struct{}{}:
	default:
	}
}

// indexPass is one look at every repository searched by index. A listing
// that fails is logged and looked at again at the next poll.
func (c *Cache) indexPass(ctx context.Context) {
	c.indexMu.Lock()
	c.indexWanted = nil
	c.indexMu.Unlock()
	q := code.ReadQuery{Kind: code.ReadIndexed, Fresh: statelog.Freshness{Level: statelog.ReadStale}}
	for {
		page, err := c.fetch.Read(ctx, q)
		if err != nil {
			if ctx.Err() == nil {
				c.log.Warn("code_cache_index_listing_failed", "error", err)
			}
			return
		}
		for _, r := range page.Indexed {
			if ctx.Err() != nil {
				return
			}
			c.indexOne(ctx, r.ID)
		}
		if page.Next == "" {
			return
		}
		q.Cursor = page.Next
	}
}

// indexWantedPass looks at the repositories nudges named since the last
// look: each one's mode is read from its synced copy, so a repository
// searched with grep costs a sync this node already made and nothing more.
func (c *Cache) indexWantedPass(ctx context.Context) {
	c.indexMu.Lock()
	wanted := c.indexWanted
	c.indexWanted = nil
	c.indexMu.Unlock()
	for _, id := range slices.Sorted(maps.Keys(wanted)) {
		if ctx.Err() != nil {
			return
		}
		c.indexOne(ctx, id)
	}
}

// indexOne brings repoID's code index here to its tip where this node is
// among its holders, and drops it where it is not.
func (c *Cache) indexOne(ctx context.Context, repoID string) {
	outcome, err := c.indexHeld(ctx, repoID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		outcome = IndexFailed
		c.log.Warn("code_cache_index_failed", "repo", repoID, "error", err)
	}
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeIndexBuilds, 1, metrics.Attrs{"outcome": string(outcome)})
	}
}

// indexHeld is indexOne's decision and build.
func (c *Cache) indexHeld(ctx context.Context, repoID string) (IndexOutcome, error) {
	order, err := c.placement.Order(repoID)
	if err != nil {
		return "", err
	}
	if !slices.Contains(order[:min(len(order), code.IndexHolders)], c.placement.Node) {
		c.dropIndexOf(repoID)
		return IndexNone, nil
	}
	s, err := c.Sync(ctx, repoID, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		return "", err
	}
	defer s.Release()
	return c.Index(ctx, s)
}

// dropIndexOf drops the code index of repoID's current copy, where this node
// holds one that keeps an index; it syncs nothing.
func (c *Cache) dropIndexOf(repoID string) {
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	inc := e.current
	var rec *codeIndexState
	if inc != nil && !e.dead {
		rec = inc.codeIndexRecord()
	}
	e.mu.Unlock()
	if rec != nil {
		c.dropCodeIndex(e, inc, rec)
	}
}

// codeIndexShare is the most the node's code indexes may hold of the budget:
// the budget divided by CodeIndexShare.
func (c *Cache) codeIndexShare() int64 { return c.maxBytes / CodeIndexShare }

// codeIndexExcess is what the code indexes hold past their share, or zero:
// what their own trim gives back (trimCodeIndexes), never a repository's
// eviction — one evicted for an index's bytes is a cold sync of every pack it
// holds, where the index is rebuilt from the blobs of one tip.
func (c *Cache) codeIndexExcess() int64 {
	return max(c.codeIndexBytes.Load()-c.codeIndexShare(), 0)
}

// trimCodeIndexes holds the node's code indexes to their share: while they
// hold more, the index of the copy used least recently is dropped WHOLE — a
// part of one answers no search — and answers whether it dropped any. A
// search already reading a dropped index answers from the view it opened.
func (c *Cache) trimCodeIndexes(ctx context.Context) bool {
	if c.codeIndexExcess() <= 0 {
		return false
	}
	type candidate struct {
		e        *entry
		inc      *incarnation
		rec      *codeIndexState
		lastUsed time.Time
	}
	var candidates []candidate
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && !e.dead {
			if rec := inc.codeIndexRecord(); rec != nil {
				candidates = append(candidates, candidate{e: e, inc: inc, rec: rec, lastUsed: inc.lastUsed})
			}
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(a.lastUsed.Compare(b.lastUsed), strings.Compare(a.e.id, b.e.id))
	})
	dropped := false
	for _, cand := range candidates {
		if c.codeIndexExcess() <= 0 || ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		c.log.Info("code_cache_index_trimmed", "repo", cand.e.id, "bytes", cand.rec.bytes(),
			"share", c.codeIndexShare())
		c.dropCodeIndex(cand.e, cand.inc, cand.rec)
		dropped = true
	}
	return dropped
}
