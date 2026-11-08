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

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/codeindex"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE CODE INDEX (DerivedCodeIndex, M6-D12): the trigram index of a
// `search: indexed` repository's default branch, kept on the Gitlet nodes its
// search is placed on — the first code.IndexHolders of the repository's
// rendezvous order over the live Gitlet nodes — INSIDE the incarnation, under
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
// (compact.Poll), so a Gitlet node whose nudges were lost catches up in the
// time a compaction would.
const IndexPoll = time.Minute

// IndexPlacementGrace is how long this node's place among a repository's
// index holders must stand, once a look has seen it change, before the
// indexer acts on it: a node that BECOMES a holder fills and builds nothing
// until then, and one the order moved PAST keeps its index until then.
//
// The order is over the LIVE Gitlet nodes, and a node gives up its presence at
// the first step of its drain, so every restart of a Gitlet node made the next
// node of each of its repositories' orders a holder — which filled a copy and
// built an index from every blob of the tip — and, once the node was back,
// dropped them again: a rolling upgrade of the Gitlet nodes cost some two full
// fills and builds for every indexed repository, all thrown away. Ten
// IndexPolls cover a restart: the drain waits for the node's turns and git
// requests (a multi-tool turn takes two to five minutes; the drain's own
// stop up to about a minute more, agent-runtime.md's grace rule) and the
// boot that follows it, while a holder truly gone costs its repositories one
// index for ten minutes more — searched the first holder's way, or with git
// grep — rather than none. A JUDGMENT, from those figures rather than a
// measured restart.
const IndexPlacementGrace = 10 * IndexPoll

// CodeIndexShare is the share of the cache's budget the node's code indexes
// may hold — a third, trimmed before any repository is evicted, the
// repository searched least recently first: one dropped is built again from
// the blobs of one tip, at 78 seconds of a core a gigabyte of source, once
// the share has room or its repository is searched again (indexTaken), where a
// repository evicted is fetched again whole. A
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
	// code.IndexHolders Gitlet nodes (estate.Router.GitOrder), whose indexer
	// keeps an index of it. False where the order cannot be read now: a grep
	// answers the same hits.
	Holder bool
	// PassesOn is whether this node is the FIRST of those holders and the
	// order names a second: what lets a search of the default branch that
	// this node's index does not answer for yet send the walk on to the
	// second holder (code.UnreadyIndexBuilding) rather than read it with git
	// grep. Never the second holder's, nor the only one's: no holder follows
	// either in the walk, so a refusal there would leave a search unanswered
	// until the indexer caught up — on a fleet of one Gitlet node, after every
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
// node that holds none, or s not handed out. It is what every search of one
// repository asks first, so it is what notes the repository SEARCHED here
// (entry.searched), whether or not the copy keeps an index: what the trim of
// the code indexes keeps first, and what lets a build displace an index
// searched less recently (Cache.mayBuildIndex).
func (c *Cache) IndexTip(s Synced) (IndexedTip, bool) {
	if c.handedOut(s, "a read of the code index") != nil {
		return IndexedTip{}, false
	}
	var t IndexedTip
	t.Holder, t.PassesOn = c.indexHolder(s.Repo.ID)
	s.entry.mu.Lock()
	defer s.entry.mu.Unlock()
	s.entry.searched = c.now()
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
// code.IndexHolders Gitlet nodes, and whether it is the first of them with
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

// CodeIndex is the code index of s's copy, mapped and opened, as a SEARCH
// reads it — VALID WHILE s IS HELD: its files stay mapped for s's reader
// until s is released (openedCodeIndex). False where the copy keeps none,
// where the node's bound on mappings is held whole by readers, where a file
// of it cannot be read — which also drops the index for the indexer to build
// again — and where a file of it is not yet one this process wrote or held to
// its trailer: a search answers each by reading the tip with git grep.
//
// A SEARCH NEVER HASHES AN INDEX. After a restart nothing has vouched for an
// index's files, and the first open of one hashes every byte of every file —
// a cold read of an index of a gigabyte or more and seconds of a core — which
// on a search's path ran outside its budget (code.SearchBudget), and once for
// each of the concurrent first searches that each found it unopened. A
// search that finds the index unverified greps and NUDGES THE INDEXER, whose
// look verifies it (Index), one look at a time, off every request's path;
// and an opening in progress is waited on by every other reader of the
// incarnation (incarnation.codeIndexOpening), so the mappings it reserves are
// made once — but for the indexer's verifying one, which no search waits
// for.
func (c *Cache) CodeIndex(ctx context.Context, s Synced) (*CodeIndexView, bool) {
	return c.codeIndex(ctx, s, false)
}

// indexOpening is an opening of an incarnation's code index in progress:
// done closes once it is installed or has failed, and verifying says it is
// the indexer's, holding files to their trailers.
type indexOpening struct {
	done      chan struct{}
	verifying bool
}

// errUnverified is an opening of a code index a file of which this process
// has neither written nor held to its trailer, by a reader that does not
// verify (CodeIndex).
var errUnverified = errors.New("cache: a file of the code index is not yet held to its trailer")

// codeIndex is CodeIndex, and with verify — the indexer's — the opening that
// holds every file nothing has vouched for to its trailer.
func (c *Cache) codeIndex(ctx context.Context, s Synced, verify bool) (*CodeIndexView, bool) {
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
		if opening := inc.codeIndexOpening; opening != nil {
			e.mu.Unlock()
			if opening.verifying && !verify {
				// The indexer is holding the files to their trailers: a
				// search waits for none of that, and greps.
				return nil, false
			}
			// Another reader is mapping it: this one takes that opening.
			select {
			case <-opening.done:
				continue
			case <-ctx.Done():
				return nil, false
			}
		}
		opening := &indexOpening{done: make(chan struct{}), verifying: verify}
		inc.codeIndexOpening = opening
		e.mu.Unlock()
		o, err := c.openCodeIndex(e, inc, rec, verify)
		e.mu.Lock()
		inc.codeIndexOpening = nil
		close(opening.done)
		switch {
		case errors.Is(err, errUnverified):
			e.mu.Unlock()
			c.nudgeIndex(e.id)
			return nil, false
		case errors.Is(err, errMapsFull):
			// Every mapping the node may make is a reader's: this search
			// reads the tip with git grep, and the index stays.
			e.mu.Unlock()
			return nil, false
		case err != nil:
			e.mu.Unlock()
			c.log.Warn("code_cache_codeindex_unreadable", "repo", e.id, "incarnation", inc.n, "error", err)
			c.dropCodeIndex(ctx, e, inc, rec)
			return nil, false
		}
		if inc.codeIndexRecord() != rec || inc.discarded || inc.codeIndex != nil {
			// The record moved, or was dropped, while this one mapped: this
			// opening is nobody's.
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
// and opens them — or, without verify, answers errUnverified for one nothing
// has vouched for, before it maps it. errMapsFull where the node's bound on
// mappings has no room.
func (c *Cache) openCodeIndex(e *entry, inc *incarnation, rec *codeIndexState, verify bool) (*openedCodeIndex,
	error) {

	o := &openedCodeIndex{rec: rec, view: &CodeIndexView{}}
	mapFile := func(rel string, size int64) ([]byte, error) {
		path := filepath.Join(inc.dir, rel)
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		// Read-only, and mapped by the time it closes: a close's error has
		// nothing left to lose.
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return nil, err
		}
		if info.Size() != size || size == 0 {
			return nil, fmt.Errorf("%s is %d bytes, and the index names it at %d", rel, info.Size(), size)
		}
		id := idOf(info)
		e.mu.Lock()
		vouched := vouchedAt(inc, rel, id)
		e.mu.Unlock()
		if !vouched && !verify {
			return nil, fmt.Errorf("%w: %s", errUnverified, rel)
		}
		if err = c.maps.reserve(); err != nil {
			return nil, err
		}
		data, err := unix.Mmap(int(f.Fd()), 0, int(size), unix.PROT_READ, unix.MAP_SHARED)
		if err != nil {
			c.maps.unreserve()
			return nil, fmt.Errorf("mapping %s: %w", rel, err)
		}
		o.maps = append(o.maps, &mappedRun{data: data})
		if !vouched {
			if k := c.knobs; k != nil && k.hashing != nil {
				k.hashing(path)
			}
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

// codeIndexVouched reports whether every file rec names is one this process
// wrote or held to its trailer at the identity it has now (vouch): an index
// a search may open (CodeIndex).
func (c *Cache) codeIndexVouched(e *entry, inc *incarnation, rec *codeIndexState) bool {
	rels := rec.rels()
	ids := make([]fileID, len(rels))
	for i, rel := range rels {
		ids[i] = lstatID(filepath.Join(inc.dir, rel))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, rel := range rels {
		if !vouchedAt(inc, rel, ids[i]) {
			return false
		}
	}
	return true
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
// state that names it no more — under ctx's values but never its
// cancellation: the drop is already made in memory, so a write the caller's
// ending cut would leave the state naming an index nothing keeps.
func (c *Cache) dropCodeIndex(ctx context.Context, e *entry, inc *incarnation, rec *codeIndexState) {
	e.mu.Lock()
	stale := inc.codeIndexRecord() != rec || inc.discarded
	if !stale {
		c.setCodeIndex(inc, nil)
	}
	e.mu.Unlock()
	if !stale {
		if err := c.persist(context.WithoutCancel(ctx), e, inc, false); err != nil {
			c.log.Warn("code_cache_codeindex_drop_failed", "repo", e.id, "error", err)
		}
	}
}

// IndexOutcome is what one Index call did: the `outcome` of
// crewlet.code.index.builds.
type IndexOutcome string

const (
	// IndexCurrent means the index already answers for the branch's tip.
	IndexCurrent IndexOutcome = "current"
	// IndexBuilt means the index now answers for the branch's tip.
	IndexBuilt IndexOutcome = "built"
	// IndexNone means the repository keeps no index here — its search is grep,
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
// keeps is dropped. An index nothing in this process has held to its
// trailers yet — a boot's — is verified and opened here first, so no search
// pays for it (CodeIndex).
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
			c.dropCodeIndex(ctx, e, inc, rec)
		}
		return IndexNone, nil
	}
	var view *CodeIndexView
	if rec != nil && !c.codeIndexVouched(e, inc, rec) {
		if view, _ = c.codeIndex(ctx, s, true); view == nil {
			e.mu.Lock()
			rec = inc.codeIndexRecord()
			e.mu.Unlock()
		}
	}
	// The refs as the copy holds them now, read BEFORE the tip: refs that
	// move while it is read are another digest, which the next look reads
	// the tip under again.
	e.mu.Lock()
	refs := indexRefs{rec: rec, digest: inc.refsDigest, branch: s.Repo.DefaultBranch}
	unmoved := rec != nil && refs.digest != "" && inc.codeIndexRefs == refs
	e.mu.Unlock()
	if unmoved {
		return IndexCurrent, nil
	}
	tip, err := c.indexTip(ctx, s)
	if err != nil || tip == "" {
		return IndexNone, err
	}
	if rec != nil && rec.Tip == tip {
		e.mu.Lock()
		if inc.codeIndexRecord() == rec {
			inc.codeIndexRefs = refs
		}
		e.mu.Unlock()
		return IndexCurrent, nil
	}
	files, err := c.listTip(ctx, s, tip)
	if err != nil {
		return "", err
	}
	if rec != nil && view == nil {
		if view, _ = c.codeIndex(ctx, s, true); view == nil {
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
	if err := b.durable(); err != nil {
		return "", err
	}
	if !b.commit(tip, refs) {
		return IndexCurrent, nil
	}
	if err := c.persist(ctx, e, inc, false); err != nil {
		return "", err
	}
	c.gaveBack(e.id, false, true)
	return IndexBuilt, nil
}

// indexRefs is the refs a code index's tip was read under: the record it
// answers for, the copy's packed-refs as its digest names it, and the
// default branch the tip was read of.
//
// THE INDEXER'S LOOK READS NO TIP WHILE THEY STAND (Index): it looks at every
// repository it keeps an index of every IndexPoll, and reading the tip is a
// git process — two thousand held repositories were some 33 a second around
// the clock on a quiet company — while what moves the tip is a round that
// writes the copy's refs, which moves the digest and nudges the indexer.
type indexRefs struct {
	rec    *codeIndexState
	digest string
	branch string
}

// indexTip is the commit s's default branch names, or "" where it names
// none.
func (c *Cache) indexTip(ctx context.Context, s Synced) (string, error) {
	if k := c.knobs; k != nil && k.indexTipReading != nil {
		k.indexTipReading(s.Dir)
	}
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
	// dirs is every directory whose entries the build's renames changed —
	// the index's own, and the parent of each directory it made — which
	// durable syncs once, before the record names any file in them.
	dirs   []string
	next   uint64
	stored bool
}

// builtSegment is one segment a record names, opened: m is the mapping of a
// segment THIS BUILD wrote, nil for one of the prior index, which the view
// the build read holds.
type builtSegment struct {
	part codeIndexPart
	seg  *codeindex.Segment
	m    *mappedRun
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

// place writes a file of the index through write STREAMED INTO ITS STAGED
// FILE in tmp/ — never into a buffer: a segment is up to the index's whole
// size once merged, and every format's writer buffers and hashes as it goes
// (codeindex.WriteSegment, MergeSegments, WriteTipMap) — renamed in at rel,
// and answers its size.
func (b *indexBuild) place(rel string, write func(io.Writer) error) (int64, error) {
	var size int64
	staged, err := b.c.stage(hostbox.FileMode, func(f *os.File) error {
		if err := write(f); err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			return err
		}
		size = info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := b.mkdir(); err != nil {
		return 0, removeStaged(staged, err)
	}
	if err := b.c.place(staged, filepath.Join(b.inc.dir, rel)); err != nil {
		return 0, err
	}
	b.wrote = append(b.wrote, rel)
	return size, nil
}

// mkdir makes the index's directory, the first time the build places a file,
// and notes the directories durable syncs: the index's own, and the parent of
// each directory it made, whose new entry is a rename of its own.
func (b *indexBuild) mkdir() error {
	if b.dirs != nil {
		return nil
	}
	dir := filepath.Join(b.inc.dir, filepath.FromSlash(codeIndexDir))
	b.dirs = []string{dir}
	for d := dir; d != b.inc.dir; d = filepath.Dir(d) {
		_, err := os.Lstat(d)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		b.dirs = append(b.dirs, filepath.Dir(d))
	}
	return os.MkdirAll(dir, hostbox.DirMode)
}

// durable syncs every directory the build's renames changed, ONCE each,
// before its commit stores the record that names them — the cache's rule of
// one sync a directory a step writes ([Cache.place]): nothing names a file the
// build placed until then, and each staged file was synced as it was written
// (stage), so a sync after each placement, as the build once made, bought
// nothing but a second fsync a file — hundreds of them for a first build.
func (b *indexBuild) durable() error {
	for _, dir := range b.dirs {
		if err := b.c.durableDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// placeSegment places a segment through write at a number of its own and
// opens it again by MAPPING the file it placed (openBuilt), never from the
// bytes it wrote: a build reads back every segment it writes — its blobs to
// know what the next tip brings, both inputs of every merge — and a first
// build of a large repository writes many, so held in the heap as written
// they made the build's peak the repository's whole index, twice over in its
// last merge, where the declaration's charge is one segment's postings.
func (b *indexBuild) placeSegment(write func(io.Writer) error) (builtSegment, error) {
	n := b.alloc()
	rel := codeIndexSegRel(n)
	size, err := b.place(rel, write)
	if err != nil {
		return builtSegment{}, err
	}
	return b.openBuilt(rel, codeIndexPart{N: n, Bytes: size})
}

// openBuilt maps the segment this build placed at rel, counted against the
// node's bound on mappings as a reader's is (runmaps.go) — errMapsFull, where
// readers hold the bound whole, fails the build, which the indexer's next look
// asks again — and opens it. Its trailer is not checked: this build wrote it.
func (b *indexBuild) openBuilt(rel string, part codeIndexPart) (builtSegment, error) {
	path := filepath.Join(b.inc.dir, rel)
	f, err := os.Open(path)
	if err != nil {
		return builtSegment{}, err
	}
	// Read-only, and mapped by the time it closes: a close's error has
	// nothing left to lose.
	defer func() { _ = f.Close() }()
	if err = b.c.maps.reserve(); err != nil {
		return builtSegment{}, err
	}
	data, err := unix.Mmap(int(f.Fd()), 0, int(part.Bytes), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		b.c.maps.unreserve()
		return builtSegment{}, fmt.Errorf("mapping %s: %w", rel, err)
	}
	m := &mappedRun{data: data}
	if k := b.c.knobs; k != nil && k.indexMapped != nil {
		k.indexMapped(path, data)
	}
	seg, err := codeindex.OpenSegment(data)
	if err != nil {
		b.c.maps.drop(m)
		return builtSegment{}, err
	}
	return builtSegment{part: part, seg: seg, m: m}, nil
}

// unmapBuilt unmaps the segments of gs this build mapped (openBuilt); a prior
// index's are its view's.
func (b *indexBuild) unmapBuilt(gs ...builtSegment) {
	for _, g := range gs {
		if g.m != nil {
			b.c.maps.drop(g.m)
		}
	}
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
		g, err := b.placeSegment(func(w io.Writer) error {
			return codeindex.WriteSegment(w, builder)
		})
		if err != nil {
			return err
		}
		b.segs = append(b.segs, g)
		builder = &codeindex.Builder{}
		if err := b.merge(); err != nil {
			return err
		}
		if k := b.c.knobs; k != nil && k.indexPlaced != nil {
			k.indexPlaced(b.c.maps.count())
		}
		return nil
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
	// done closes as the process ends: its error reaches the blob reads
	// through the pipe it closes the output with, and nothing else needs it.
	done := make(chan struct{})
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
		close(done)
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
		g, err := b.placeSegment(func(w io.Writer) error {
			return codeindex.MergeSegments(w, []*codeindex.Segment{older.seg, newer.seg},
				func(oid codeindex.OID) bool { return b.tip[oid] })
		})
		if err != nil {
			return err
		}
		b.segs = append(b.segs[:n-2], g)
		// The inputs this build wrote are read no more: their mappings go
		// now, not at the build's end, so what a build maps at once is the
		// few segments a geometric index keeps.
		b.unmapBuilt(older, newer)
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
	size, err := b.place(codeIndexMapRel(n), func(w io.Writer) error {
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
// for as this process's own, and the record is noted as read under refs,
// those the tip was read under.
func (b *indexBuild) commit(tip string, refs indexRefs) bool {
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
	refs.rec = rec
	inc.codeIndexRefs = refs
	b.stored = true
	return true
}

// abandon unmaps every segment the build mapped and still held, and moves
// every file the build wrote into trash/ where no record names it: a build
// that failed, or whose commit found the index moved. Files the stored record
// does not name — a segment a later merge of the same build replaced — go
// too.
func (b *indexBuild) abandon() {
	b.unmapBuilt(b.segs...)
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

// IndexFailed means the copy could not be synced or its index built, and the
// indexer's next look tries again.
const IndexFailed IndexOutcome = "failed"

const (
	// IndexOverShare means the copy keeps no index and the node's code
	// indexes hold their share of the budget (CodeIndexShare), so none is
	// built for it — nor a copy filled — until there is room or a search of
	// it is newer than that of an index kept (Cache.mayBuildIndex): its
	// searches grep meanwhile.
	IndexOverShare IndexOutcome = "over_share"
	// IndexNoRoom means the budget evicted the copy this node kept an index
	// in, and the indexer fills it again only once the budget has room for
	// what it held (Cache.roomToFill): its searches go to the next node.
	IndexNoRoom IndexOutcome = "no_room"
	// IndexPlacing means this node became one of the repository's index
	// holders within IndexPlacementGrace and its copy keeps no index: it
	// fills and builds nothing until its place has stood that long.
	IndexPlacing IndexOutcome = "placing"
)

// indexPlace is the place among a repository's index holders the indexer
// last saw this node in: whether a holder, since when, and whether since is
// a CHANGE a look saw rather than the first look since the boot.
type indexPlace struct {
	holder, moved bool
	since         time.Time
}

// settled notes holder as this node's place among repoID's index holders now,
// and reports whether the indexer acts on it (IndexPlacementGrace): a holder
// at once where it has been one since the boot's first look, and otherwise —
// a holder it became, or a place past the holders, whose index stays until
// then — once the place has stood the grace.
func (c *Cache) settled(repoID string, holder bool) bool {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	now := c.now()
	p, seen := c.indexSeen[repoID]
	if !seen || p.holder != holder {
		p = indexPlace{holder: holder, moved: seen, since: now}
		if c.indexSeen == nil {
			c.indexSeen = map[string]indexPlace{}
		}
		c.indexSeen[repoID] = p
	}
	if holder && !p.moved {
		return true
	}
	return now.Sub(p.since) >= IndexPlacementGrace
}

// indexTaken is what the budget took of a repository this node keeps a code
// index of, by the bytes each held: copyBytes, the copy its eviction took,
// and indexBytes, the index its trim took — zero for what it took nothing
// of.
//
// WHAT THE BUDGET TOOK IS NOT TAKEN STRAIGHT BACK. Built again at the next
// look, an index the trim dropped put the indexes past their share again,
// and the trim dropped the next one: while a node's indexes exceeded the
// share, it rebuilt an index from every blob of its tip every minute, for
// ever. Filled again at the next look, a copy the budget evicted evicted the
// next least used, a GET a pack each minute. So the indexer builds a trimmed
// index again only into room for what it held, or for a repository searched
// more recently than an index it would displace (mayBuildIndex), and fills
// an evicted copy only into room for what it held (roomToFill): an
// over-committed node keeps the indexes searched most recently and greps
// the rest, rather than churning.
type indexTaken struct{ copyBytes, indexBytes int64 }

// tookBack notes what the budget took of repoID: an eviction's copy bytes,
// or a trim's index bytes.
func (c *Cache) tookBack(repoID string, copyBytes, indexBytes int64) {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	if c.indexBack == nil {
		c.indexBack = map[string]indexTaken{}
	}
	hb := c.indexBack[repoID]
	if copyBytes > 0 {
		hb.copyBytes = copyBytes
	}
	if indexBytes > 0 {
		hb.indexBytes = indexBytes
	}
	c.indexBack[repoID] = hb
}

// gaveBack forgets what the budget took of repoID's copy (copyBytes) or its
// index (indexBytes), each it is told: the indexer has it again, or keeps it
// no more.
func (c *Cache) gaveBack(repoID string, copyBytes, indexBytes bool) {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	hb, ok := c.indexBack[repoID]
	if !ok {
		return
	}
	if copyBytes {
		hb.copyBytes = 0
	}
	if indexBytes {
		hb.indexBytes = 0
	}
	if hb == (indexTaken{}) {
		delete(c.indexBack, repoID)
		return
	}
	c.indexBack[repoID] = hb
}

// indexTakenOf is what the budget took of repoID (indexTaken).
func (c *Cache) indexTakenOf(repoID string) indexTaken {
	c.indexMu.Lock()
	defer c.indexMu.Unlock()
	return c.indexBack[repoID]
}

// roomToFill reports whether the indexer may fill a copy of repoID this node
// does not hold: unless the budget evicted the one it held, at once, and
// otherwise once the budget has room for the bytes that one held (indexTaken).
func (c *Cache) roomToFill(repoID string) bool {
	need := c.indexTakenOf(repoID).copyBytes
	return need == 0 || -c.overBudget() >= need
}

// mayBuildIndex reports whether a build may give repoID's copy, which keeps
// no index, one: while the node's indexes have room within their share — for
// what this one held when the trim took it (indexTaken), or for anything at
// all — or, with none, where the repository was SEARCHED here more recently
// than an index the node keeps, which the trim then drops in its stead
// (trimCodeIndexes keeps the most recently searched).
func (c *Cache) mayBuildIndex(repoID string) bool {
	need := max(c.indexTakenOf(repoID).indexBytes, 1)
	if c.codeIndexBytes.Load()+need <= c.codeIndexShare() {
		return true
	}
	var searched, oldest time.Time
	kept := false
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.entries {
		e.mu.Lock()
		if id == repoID {
			searched = e.searched
		} else if inc := e.current; inc != nil && !e.dead && inc.codeIndexRecord() != nil {
			if !kept || e.searched.Before(oldest) {
				oldest, kept = e.searched, true
			}
		}
		e.mu.Unlock()
	}
	return kept && searched.After(oldest)
}

// keepsCodeIndex reports whether this node holds a copy of repoID (Holds),
// and whether that copy keeps a code index.
func (c *Cache) keepsCodeIndex(repoID string) (held, keeps bool) {
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return false, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dead {
		return false, false
	}
	return e.current != nil || e.running != nil, e.current != nil && e.current.codeIndexRecord() != nil
}

// THE INDEXER (Start runs it on a cache with a Placement): every IndexPoll it
// lists the repositories searched by index (code.ReadIndexed, at STALE: a
// repository a stale page misses is listed at the next look) and, for each one
// this node is among the first code.IndexHolders of — its git order read
// again at every look, so a node that joins or leaves moves the index with the
// order, once the move has stood IndexPlacementGrace (settled), so a node's
// restart moves none — syncs its copy, FILLING ONE THIS NODE DOES NOT HOLD (M6-D12: the
// index's holder holds a copy deliberately, where a read fills one only on
// the home), and brings the index to the default branch's tip — as NO USE
// of the copy (indexSync), and never straight back into what the budget
// just took (indexTaken: a copy it evicted, an index it trimmed). Between looks,
// a round that moves a repository's refs NUDGES it (nudgeIndex), and only
// that repository is looked at. A repository this node has been no holder of
// for the grace has its index dropped where its copy keeps one: an index nobody's
// search reads is budget a held one needs, and a placement back finds the
// tip from nothing, which is what a new holder pays anyway.
func (c *Cache) indexer(ctx context.Context) {
	if c.placement == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	//nolint:contextcheck // c.life only cancels ctx, which stays Start's: Stop must end the indexer however long Start's ctx lives.
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
	listed := map[string]bool{}
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
			listed[r.ID] = true
			c.indexOne(ctx, r.ID)
		}
		if page.Next == "" {
			break
		}
		q.Cursor = page.Next
	}
	// A whole listing read: what it no longer names is searched by grep, or
	// gone, and the indexer keeps nothing of it.
	c.indexMu.Lock()
	maps.DeleteFunc(c.indexBack, func(id string, _ indexTaken) bool { return !listed[id] })
	maps.DeleteFunc(c.indexSeen, func(id string, _ indexPlace) bool { return !listed[id] })
	c.indexMu.Unlock()
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
	holder := slices.Contains(order[:min(len(order), code.IndexHolders)], c.placement.Node)
	settled := c.settled(repoID, holder)
	if !holder {
		if settled {
			c.dropIndexOf(ctx, repoID)
			c.gaveBack(repoID, true, true)
		}
		return IndexNone, nil
	}
	// Before any sync: a copy filled only for a place a restart lent this
	// node, for an index the share has no room for, or into a budget that
	// evicted it, would be dropped, trimmed or evicted again.
	held, keeps := c.keepsCodeIndex(repoID)
	if !keeps && !settled {
		return IndexPlacing, nil
	}
	if !keeps && !c.mayBuildIndex(repoID) {
		return IndexOverShare, nil
	}
	if !held && !c.roomToFill(repoID) {
		return IndexNoRoom, nil
	}
	s, err := c.indexSync(ctx, repoID)
	if err != nil {
		return "", err
	}
	defer s.Release()
	c.gaveBack(repoID, true, false)
	return c.Index(ctx, s)
}

// dropIndexOf drops the code index of repoID's current copy, where this node
// holds one that keeps an index; it syncs nothing.
func (c *Cache) dropIndexOf(ctx context.Context, repoID string) {
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
		c.dropCodeIndex(ctx, e, inc, rec)
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
// hold more, the index of the repository SEARCHED least recently here
// (entry.searched; then the copy used least recently, by its readers) is
// dropped WHOLE — a part of one answers no search — and noted, so it is not
// built straight back (indexTaken); it answers whether it dropped any. A
// search already reading a dropped index answers from the view it opened.
//
// Never by the copy's last use alone: the indexer looks at every copy it
// keeps an index of every minute, which once counted as a use, so the trim
// took whichever index the last look happened to reach first.
func (c *Cache) trimCodeIndexes(ctx context.Context) bool {
	if c.codeIndexExcess() <= 0 {
		return false
	}
	type candidate struct {
		e                  *entry
		inc                *incarnation
		rec                *codeIndexState
		searched, lastUsed time.Time
	}
	var candidates []candidate
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && !e.dead {
			if rec := inc.codeIndexRecord(); rec != nil {
				candidates = append(candidates, candidate{e: e, inc: inc, rec: rec, searched: e.searched,
					lastUsed: inc.lastUsed})
			}
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	slices.SortFunc(candidates, func(a, b candidate) int {
		return cmp.Or(a.searched.Compare(b.searched), a.lastUsed.Compare(b.lastUsed), strings.Compare(a.e.id, b.e.id))
	})
	dropped := false
	for _, cand := range candidates {
		if c.codeIndexExcess() <= 0 || ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		c.log.Info("code_cache_index_trimmed", "repo", cand.e.id, "bytes", cand.rec.bytes(),
			"share", c.codeIndexShare())
		c.tookBack(cand.e.id, 0, cand.rec.bytes())
		c.dropCodeIndex(ctx, cand.e, cand.inc, cand.rec)
		dropped = true
	}
	return dropped
}
