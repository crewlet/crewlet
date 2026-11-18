package cache

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The memo of answers by object id (package doc, "The memo"; DerivedMemo):
// the pages internal/gitlet/query cuts and internal/gitlet/home's decided
// merge previews, one file an answer under crewlet/memo/<cohort>/<key>. The
// callers form keys and bodies; the cache keeps, checks, counts and trims
// them without reading one. An entry is a function of object ids, so only
// ever absent, never stale (FreshImmutable), which rests on a copy never
// losing an object while its history stands: a history gone drops and shuts
// the memo (OnHistoryGone, reopenMemo). Entries are staged with no sync and
// every read holds one to its digest (CheckEntryDigest), so the check and the
// scrub pass over it (Check.hashed). A run that met damage memoizes nothing.
// In memory a copy holds its cohorts' numbers and bytes, never an entry; the
// trim's cost is in the package doc's "Measured".

// memoFile names this file as the memo's declared writer (Writer.File).
const memoFile = "internal/gitlet/cache/memo.go"

// MemoShare divides the cache's budget into the memo's node-wide share: a
// sixteenth. An evicted answer costs one git process to compute again, an
// evicted repository a cold sync of every pack; a sixteenth of 400 GB still
// holds some 450 000 answers at their 56 KiB bound (gitlet.MaxGitAnswerBytes),
// so fifteen sixteenths stay the repositories'; the janitor trims the memo
// before it evicts any. A judgment call: misses climbing
// (crewlet.code.cache.memo.lookups) while the memo sits at its share and
// repositories are rarely evicted say it is too small; BenchmarkCodeRead's
// memo arms measure what a hit saves.
const MemoShare = 16

// memoCohortsPerShare is how many cohorts a memo at its share holds, and so
// the trim's granularity: a trim drops the oldest, a sixteenth of the share,
// and the next falls due once as much has been placed again. Fewer would trim
// more of the memo than its use says to; more cost every miss another open. A
// judgment call.
const memoCohortsPerShare = 16

// maxMemoCohorts bounds the cohorts the node keeps however few bytes they
// hold: removals and reads moving entries on thin old cohorts without filling
// the memo, and every miss opens each one. Past it the janitor's tick drops the
// oldest: a cohort unread for two shares' worth of placements.
const maxMemoCohorts = 2 * memoCohortsPerShare

// memoBlock is the least volume an entry takes: a file gets whole blocks (4 KiB
// at ext4's, xfs's and btrfs's defaults), so counted at its length a memo of
// small answers would hold several times its share (memoBytesOf).
const memoBlock = 4 << 10

// The entry format: "CRMM", its version, its key, its body's length and
// SHA-256, then the body. A reshape takes a new version; an entry of another is
// a miss, removed.
const (
	memoMagic   = "CRMM"
	memoVersion = 1
	// memoHeader is the header's bytes: the magic, the version, the key, the
	// length and the digest.
	memoHeader = len(memoMagic) + 1 + sha256.Size + 4 + sha256.Size
	// memoMaxTail bounds what a run appends to a staged page (Extend and
	// Place's tail together): what it learned only after cutting the page, never
	// more than the page itself.
	memoMaxTail = gitlet.MaxGitAnswerBytes
	// memoMaxBody is the most a body holds: an answer at its bound and its
	// tail at its own.
	memoMaxBody = gitlet.MaxGitAnswerBytes + memoMaxTail
)

// memoRel is the memo's tree, relative to its incarnation: what DerivedMemo
// owns whole (Owned.Tree).
var memoRel = filepath.Join(engineRoot, "memo")

// MemoKey names one memoized answer: the SHA-256 of everything it is a
// function of (NewMemoKey). Its hex is its entry's file name.
type MemoKey [sha256.Size]byte

// NewMemoKey is the key of an answer that is a function of parts alone, in
// their order: each is length-prefixed before it is hashed, so no two lists
// of parts — "ab", "c" and "a", "bc" — name one key. THE ONE WAY A KEY IS
// MADE: a caller names in parts the version of its answers' shape, so an entry
// is never read as an answer of another shape.
func NewMemoKey(parts ...string) MemoKey {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, p := range parts {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
		_, _ = io.WriteString(h, p) // a hash takes every write
	}
	var k MemoKey
	h.Sum(k[:0])
	return k
}

// String is k's hex: its entry's file name.
func (k MemoKey) String() string { return hex.EncodeToString(k[:]) }

// memoBytesOf is the volume an entry of size bytes takes, as the memo counts
// it: its length rounded up to whole blocks (memoBlock).
func memoBytesOf(size int64) int64 { return (size + memoBlock - 1) / memoBlock * memoBlock }

// memoCohort is one cohort of a copy's memo as its record keeps it: its
// number, node-wide, and what its directory holds as the memo counts it.
type memoCohort struct {
	N     uint64 `json:"n"`
	Bytes int64  `json:"bytes"`
}

// cohortName is cohort n's directory name: n in decimal, its one spelling.
func cohortName(n uint64) string { return strconv.FormatUint(n, 10) }

// parseCohort reads a name in a memo's tree as the cohort it names — a
// positive number in its one spelling — or false.
func parseCohort(name string) (uint64, bool) {
	n, err := strconv.ParseUint(name, 10, 64)
	return n, err == nil && n > 0 && cohortName(n) == name
}

// cohortDir is cohort n's directory in the incarnation at dir.
func cohortDir(dir string, n uint64) string { return filepath.Join(dir, memoRel, cohortName(n)) }

// memoPath is the file of entry k in cohort n of the incarnation at dir.
func memoPath(dir string, n uint64, k MemoKey) string {
	return filepath.Join(cohortDir(dir, n), k.String())
}

// withCohort is cs — ordered by cohort, as every record is — with delta added
// to cohort n's bytes, never below none, and n added where cs holds none: a
// new slice, since a record is never changed in place (setMemo).
func withCohort(cs []memoCohort, n uint64, delta int64) []memoCohort {
	i, found := slices.BinarySearchFunc(cs, n, func(c memoCohort, n uint64) int { return cmp.Compare(c.N, n) })
	out := make([]memoCohort, 0, len(cs)+1)
	out = append(out, cs[:i]...)
	if found {
		out = append(out, memoCohort{N: n, Bytes: max(cs[i].Bytes+delta, 0)})
		return append(out, cs[i+1:]...)
	}
	out = append(out, memoCohort{N: n, Bytes: max(delta, 0)})
	return append(out, cs[i:]...)
}

// withoutCohort is cs without cohort n, a new slice, and what n held.
func withoutCohort(cs []memoCohort, n uint64) ([]memoCohort, int64) {
	i, found := slices.BinarySearchFunc(cs, n, func(c memoCohort, n uint64) int { return cmp.Compare(c.N, n) })
	if !found {
		return cs, 0
	}
	return slices.Concat(cs[:i], cs[i+1:]), cs[i].Bytes
}

// holdsCohort reports whether cs holds cohort n.
func holdsCohort(cs []memoCohort, n uint64) bool {
	_, found := slices.BinarySearchFunc(cs, n, func(c memoCohort, n uint64) int { return cmp.Compare(c.N, n) })
	return found
}

// cohortsBytes is what cs holds together.
func cohortsBytes(cs []memoCohort) int64 {
	var n int64
	for _, c := range cs {
		n += c.Bytes
	}
	return n
}

// The outcomes of crewlet.code.cache.memo.lookups.
const (
	memoHit       = "hit"
	memoMiss      = "miss"
	memoCorrupt   = "corrupt"
	memoProbeHit  = "probe_hit"
	memoProbeMiss = "probe_miss"
)

// memoing reports whether the build declares the memo — a case's cache opened
// over declarations without it (WithDerived) keeps none.
func (c *Cache) memoing() bool { return c.image.decl(DerivedMemo) != nil }

// setMemo records cohorts as inc's memo (none where present is false) and,
// where that changes the record, moves inc.memoVersion so the janitor's tick
// writes the state (stateMemos). Every change to the record goes through here,
// so the state misses none; one that changes nothing writes nothing. e.mu is
// held.
func (c *Cache) setMemo(inc *incarnation, cohorts []memoCohort, present bool) {
	d := derivedState{}
	if present {
		d = derivedState{Bytes: cohortsBytes(cohorts), Step: StepMemo, Cohorts: cohorts}
	}
	if was, had := inc.derived[DerivedMemo]; had == present && (!present ||
		(was.Bytes == d.Bytes && was.Step == d.Step && slices.Equal(was.Cohorts, d.Cohorts))) {
		return
	}
	c.setDerived(inc, DerivedMemo, d, present)
	inc.memoVersion++
}

// MemoGet is the body memoized under k in the incarnation s holds, or false:
// none, or an entry that fails its header, which is removed (logged
// code_cache_memo_corrupt) so the read that misses writes it again. It looks in
// the copy's cohorts newest first. A hit is a use: an entry in an older cohort
// moves into the newest (memoUse), so the trim takes it last.
func (c *Cache) MemoGet(s Synced, k MemoKey) ([]byte, bool) {
	return c.memoGet(s, k, memoHit, memoMiss)
}

// MemoProbe is MemoGet for a part of an answer a read looks for under several
// keys (a directory's window of last commits, under each commit its walk
// passes): a miss there computes nothing under its key, so it is counted apart.
func (c *Cache) MemoProbe(s Synced, k MemoKey) ([]byte, bool) {
	return c.memoGet(s, k, memoProbeHit, memoProbeMiss)
}

// memoGet is MemoGet and MemoProbe, counting a hit and a miss as hit and miss.
func (c *Cache) memoGet(s Synced, k MemoKey, hit, miss string) ([]byte, bool) {
	if !c.memoing() || c.handedOut(s, "a memo lookup") != nil {
		return nil, false
	}
	e, inc := s.entry, s.inc
	e.mu.Lock()
	var cohorts []uint64
	if rec, ok := inc.derived[DerivedMemo]; ok && !inc.discarded {
		cohorts = make([]uint64, len(rec.Cohorts))
		for i, ch := range rec.Cohorts {
			cohorts[len(cohorts)-1-i] = ch.N
		}
	}
	e.mu.Unlock()
	for _, n := range cohorts {
		path := memoPath(inc.dir, n, k)
		body, bytes, info, err := readMemo(path, k)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			continue
		case err != nil:
			c.log.Warn("code_cache_memo_corrupt", "repo", e.id, "incarnation", inc.n, "cohort", n,
				"entry", k.String(), "error", err)
			c.removeEntry(e, inc, n, k, path, info, bytes)
			c.lookedUp(memoCorrupt)
			return nil, false
		}
		c.memoUse(e, inc, n, k, info, bytes)
		c.lookedUp(hit)
		return body, true
	}
	c.lookedUp(miss)
	return nil, false
}

// errMemoEntry is an entry that is not whole: what MemoGet removes.
var errMemoEntry = errors.New("cache: a memo entry that is not whole")

// readMemo is the body of the entry at path, which must answer k, and the
// volume it takes; fs.ErrNotExist for none, and an error wrapping errMemoEntry
// — with the file's identity, where it could be read — for one that is not a
// regular file of this format, holds another key, or whose body is not what
// its length and its digest say.
func readMemo(path string, k MemoKey) ([]byte, int64, fs.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, 0, nil, err
		}
		return nil, 0, nil, fmt.Errorf("%w: %w", errMemoEntry, err)
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer
	info, err := f.Stat()
	if err != nil {
		return nil, 0, nil, fmt.Errorf("%w: %w", errMemoEntry, err)
	}
	size := info.Size()
	switch {
	case !info.Mode().IsRegular():
		return nil, 0, info, fmt.Errorf("%w: not a regular file", errMemoEntry)
	case size < int64(memoHeader) || size > int64(memoHeader+memoMaxBody):
		return nil, memoBytesOf(size), info, fmt.Errorf("%w: %d bytes, which no entry holds", errMemoEntry, size)
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, memoBytesOf(size), info, fmt.Errorf("%w: %w", errMemoEntry, err)
	}
	head, body := buf[:memoHeader], buf[memoHeader:]
	at := len(memoMagic)
	switch {
	case string(head[:at]) != memoMagic || head[at] != memoVersion:
		return nil, memoBytesOf(size), info, fmt.Errorf("%w: no entry of format %d", errMemoEntry, memoVersion)
	case MemoKey(head[at+1:at+1+sha256.Size]) != k:
		return nil, memoBytesOf(size), info, fmt.Errorf("%w: it answers another key", errMemoEntry)
	case int(binary.BigEndian.Uint32(head[at+1+sha256.Size:])) != len(body):
		return nil, memoBytesOf(size), info, fmt.Errorf("%w: torn", errMemoEntry)
	case sha256.Sum256(body) != [sha256.Size]byte(head[memoHeader-sha256.Size:]):
		return nil, memoBytesOf(size), info, fmt.Errorf("%w: its body is not what its digest says", errMemoEntry)
	}
	return body, memoBytesOf(size), info, nil
}

// memoUse moves inc's entry k, found in cohort n, into the node's newest
// cohort (one rename, only where n is older), so the trim reaches it last;
// only while the copy still serves its memo and the file is still the one the
// read found (info), since one renamed over it is a newer answer. A hit never
// waits: where the memo's lock is held, or the move fails, the entry stays and
// the next read moves it.
func (c *Cache) memoUse(e *entry, inc *incarnation, n uint64, k MemoKey, info fs.FileInfo, bytes int64) {
	to := c.memoNewest.Load()
	if n >= to || !c.enter() {
		return
	}
	defer c.leave()
	if !inc.memoMu.TryLock() {
		return
	}
	defer inc.memoMu.Unlock()
	e.mu.Lock()
	rec, recorded := inc.derived[DerivedMemo]
	serving := recorded && !inc.discarded && !inc.retired && !inc.memoShut && holdsCohort(rec.Cohorts, n)
	e.mu.Unlock()
	if !serving {
		return
	}
	src := memoPath(inc.dir, n, k)
	if now, err := os.Lstat(src); err != nil || !os.SameFile(now, info) {
		return
	}
	_, made, err := c.makeCohort(inc, to)
	if err != nil {
		return
	}
	dst := memoPath(inc.dir, to, k)
	var prior int64
	if !made {
		if was, err := os.Lstat(dst); err == nil {
			prior = memoBytesOf(was.Size())
		}
	}
	if os.Rename(src, dst) != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.discarded {
		return
	}
	cs := withCohort(inc.derived[DerivedMemo].Cohorts, n, -bytes)
	if made {
		cs, _ = withoutCohort(cs, to)
	}
	c.setMemo(inc, withCohort(cs, to, bytes-prior), true)
}

// removeEntry removes inc's entry k at path in its cohort n — the file whose
// identity info is, and no other: one renamed over it since it was read is
// somebody's new answer — and takes its volume, bytes, out of the cohort.
// Under the memo's lock, so no answer is placed at the name as it goes.
func (c *Cache) removeEntry(e *entry, inc *incarnation, n uint64, k MemoKey, path string, info fs.FileInfo,
	bytes int64) {

	if !c.enter() {
		return
	}
	defer c.leave()
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	now, err := os.Lstat(path)
	if err != nil || (info != nil && !os.SameFile(now, info)) {
		return
	}
	if err := os.Remove(path); err != nil {
		c.log.Warn("code_cache_memo_unremoved", "repo", e.id, "cohort", n, "entry", k.String(), "error", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if rec, recorded := inc.derived[DerivedMemo]; recorded && !inc.discarded && holdsCohort(rec.Cohorts, n) {
		c.setMemo(inc, withCohort(rec.Cohorts, n, -bytes), true)
	}
}

// lookedUp counts one lookup of the memo under outcome.
func (c *Cache) lookedUp(outcome string) {
	if c.metrics != nil {
		c.metrics.Add(metrics.CodeCacheMemoLookups, 1, metrics.Attrs{"outcome": outcome})
	}
}

// MemoBatch is the answers one read's run memoizes into the incarnation it
// read: staged as the run cuts them (Put), placed together once the run has
// answered whole (Place) or given back (Discard). A nil batch memoizes
// nothing; a batch is one goroutine's.
type MemoBatch struct {
	c   *Cache
	e   *entry
	inc *incarnation
	// gen is the incarnation's memo generation the batch was begun at
	// (incarnation.memoGen): a drop since — a history gone under the copy —
	// and the batch places nothing, since what its run read may be what the
	// drop answered.
	gen uint64
	// staged is every answer staged so far, and bytes what their files hold
	// in tmp/, counted in the cache's tmpBytes until each is placed or
	// removed.
	staged []memoStaged
	bytes  int64
	// puts counts every Put, staged or not: what the i-th tail Place asks
	// for is of, so a Put that failed shifts no other answer's tail.
	puts int
	// buf is the one buffer every staging writes through.
	buf []byte
	// failed: a write failed, and was logged (once a batch).
	failed bool
	done   bool
}

// memoStaged is one staged answer: its key, which Put it was, its file in tmp/
// ("" once placed or given back), its body's length and SHA-256 so far (Extend
// grows both, the tail completes them), what Extend appended, and its file's
// bytes.
type memoStaged struct {
	key      MemoKey
	put      int
	path     string
	length   int
	appended int
	sum      hash.Hash
	size     int64
}

// Memo is a batch of answers to memoize into the incarnation s holds, as a
// read's run cuts them — nil where the cache keeps no memo, s is no pin of
// this cache's, or the copy's memo is shut (reopenMemo), each of which
// memoizes nothing.
func (c *Cache) Memo(s Synced) *MemoBatch {
	if !c.memoing() || c.handedOut(s, "a memo batch") != nil {
		return nil
	}
	e, inc := s.entry, s.inc
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.memoShut || inc.discarded {
		return nil
	}
	return &MemoBatch{c: c, e: e, inc: inc, gen: inc.memoGen}
}

// Put stages body as the answer under k in tmp/ with no sync (every read holds
// an entry to its header, so a crash may lose one and never serves it torn); only
// Place memoizes it. A body past gitlet.MaxGitAnswerBytes is refused, and a
// failed write leaves it unmemoized, logged once a batch
// (code_cache_memo_unwritten): a slower read, never a failed one. A staged page
// counts against the memo's share (stageMemo): one past it is not staged, since
// the memo could never keep it and it would hold the repositories' room
// (overBudget). It answers which Put this was, staged or not, for Extend and
// Place's tail to name it by; -1 for a batch that takes no more.
func (b *MemoBatch) Put(k MemoKey, body []byte) int {
	if b == nil || b.done {
		return -1
	}
	put := b.puts
	b.puts++
	if len(body) > gitlet.MaxGitAnswerBytes {
		b.fail(fmt.Errorf("an answer of %d bytes, past the %d one holds", len(body), gitlet.MaxGitAnswerBytes))
		return put
	}
	n := int64(memoHeader + len(body))
	if !b.c.stageMemo(n) {
		return put
	}
	// THE HEADER'S ROOM FIRST: it names the body's length and digest, which
	// the run's tail completes (Place).
	b.buf = slices.Grow(b.buf[:0], memoHeader+len(body))[:memoHeader+len(body)]
	clear(b.buf[:memoHeader])
	copy(b.buf[memoHeader:], body)
	f, err := os.CreateTemp(filepath.Join(b.c.dir, tmpDir), "memo-*")
	if err != nil {
		b.c.memoStaged.Add(-n)
		b.fail(err)
		return put
	}
	_, err = f.Write(b.buf)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name()) // what it held is no answer
		b.c.memoStaged.Add(-n)
		b.fail(err)
		return put
	}
	sum := sha256.New()
	sum.Write(body)
	b.staged = append(b.staged, memoStaged{key: k, put: put, path: f.Name(), length: len(body), sum: sum, size: n})
	b.bytes += n
	b.c.tmpBytes.Add(n)
	return put
}

// Extend appends data to the answer the put-th Put staged: what the run learned
// of it before its end (a diff stat's counts for the page), so the run holds
// none of it until Place. It takes its room from the memo's share (stageMemo)
// and with Place's tail is held to memoMaxTail; refused, or a failed write,
// gives the answer back unmemoized. Before Place; an answer not staged takes
// nothing.
func (b *MemoBatch) Extend(put int, data []byte) {
	if b == nil || b.done || len(data) == 0 {
		return
	}
	i, found := slices.BinarySearchFunc(b.staged, put, func(st memoStaged, put int) int {
		return cmp.Compare(st.put, put)
	})
	if !found || b.staged[i].path == "" {
		return
	}
	st := &b.staged[i]
	if st.appended+len(data) > memoMaxTail {
		b.fail(fmt.Errorf("%d bytes appended to an answer, past the %d a run appends", st.appended+len(data),
			memoMaxTail))
		b.unstage(st)
		return
	}
	n := int64(len(data))
	if !b.c.stageMemo(n) {
		b.unstage(st)
		return
	}
	f, err := os.OpenFile(st.path, os.O_WRONLY, 0)
	if err == nil {
		_, err = f.WriteAt(data, int64(memoHeader+st.length))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		b.c.memoStaged.Add(-n)
		b.fail(err)
		b.unstage(st)
		return
	}
	st.sum.Write(data)
	st.length += len(data)
	st.appended += len(data)
	st.size += n
	b.bytes += n
	b.c.tmpBytes.Add(n)
}

// stageMemo reserves n bytes of staged pages against the memo's share and
// reports whether they fit. Added before it is judged, so two runs never both
// take the last room; one may be refused room the other then gives back,
// costing one page unmemoized.
func (c *Cache) stageMemo(n int64) bool {
	if c.memoStaged.Add(n) > c.memoShare() {
		c.memoStaged.Add(-n)
		return false
	}
	return true
}

// fail logs a write the batch could not make, once a batch.
func (b *MemoBatch) fail(err error) {
	if !b.failed {
		b.failed = true
		b.c.log.Warn("code_cache_memo_unwritten", "repo", b.e.id, "incarnation", b.inc.n, "error", err)
	}
}

// Place memoizes every staged answer, the i-th Put's ending with tail(i), what
// the run learned only at its end (i counts every Put, so a failed one shifts
// no other's tail), renamed into the node's newest cohort under the memo's
// lock: only while the incarnation serves, the cache runs, and the memo is
// neither shut nor dropped since the batch began (its generation). A tail past
// memoMaxTail leaves its answer unmemoized. Every staged file is given back
// either way. Once a batch.
func (b *MemoBatch) Place(tail func(i int) []byte) {
	if b == nil || b.done {
		return
	}
	b.done = true
	defer b.giveBack()
	if len(b.staged) == 0 {
		return
	}
	for i := range b.staged {
		if b.staged[i].path == "" {
			continue // given back already, by an Extend that could not be made
		}
		t := tail(b.staged[i].put)
		if b.staged[i].appended+len(t) > memoMaxTail {
			b.fail(fmt.Errorf("a tail of %d bytes after %d appended, past the %d a run appends", len(t),
				b.staged[i].appended, memoMaxTail))
			b.unstage(&b.staged[i])
			continue
		}
		if err := b.complete(&b.staged[i], t); err != nil {
			if !errors.Is(err, errMemoShare) {
				b.fail(err)
			}
			b.unstage(&b.staged[i])
		}
	}
	c, e, inc := b.c, b.e, b.inc
	if !c.enter() {
		return
	}
	defer c.leave()
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	e.mu.Lock()
	serving := !inc.discarded && !inc.retired && !inc.memoShut && inc.memoGen == b.gen
	e.mu.Unlock()
	if !serving {
		return
	}
	n := c.memoNewest.Load()
	tree, made, err := c.makeCohort(inc, n)
	if err != nil {
		b.fail(err)
		return
	}
	// placed is the volume placed, prior that of same-key answers it renamed
	// over, which the record counts already. A cohort this Place made holds
	// nothing else.
	var placed, prior int64
	for i := range b.staged {
		st := &b.staged[i]
		if st.path == "" {
			continue
		}
		target := memoPath(inc.dir, n, st.key)
		if !made {
			if was, lerr := os.Lstat(target); lerr == nil {
				prior += memoBytesOf(was.Size())
			}
		}
		if err := os.Rename(st.path, target); err != nil {
			b.fail(err)
			b.unstage(st)
			continue
		}
		st.path = ""
		b.bytes -= st.size
		c.tmpBytes.Add(-st.size)
		c.memoStaged.Add(-st.size)
		placed += memoBytesOf(st.size)
	}
	e.mu.Lock()
	if inc.discarded {
		e.mu.Unlock()
		return // its bytes left the total with it
	}
	cs := inc.derived[DerivedMemo].Cohorts
	switch {
	case tree:
		// A tree this Place made holds exactly what it placed, whatever a
		// record said before.
		cs = nil
		inc.memoListed = true
	case made:
		// So does a cohort it made.
		cs, _ = withoutCohort(cs, n)
	}
	c.setMemo(inc, withCohort(cs, n, placed-prior), true)
	// Counted with e.mu let go: memoPlaced may reserve the next cohort (two
	// syncs), and every lookup of this copy takes e.mu.
	e.mu.Unlock()
	c.memoPlaced(placed - prior)
	// Past its share the trim is due now, not at the tick: no eviction is for
	// the excess (overBudget), so nothing else wakes the janitor for it.
	if c.memoExcess() > 0 {
		c.kick()
	}
}

// memoPlaced counts n bytes placed in the node's newest cohort and opens the
// next once the newest holds a sixteenth of the share. Only a placement opens a
// cohort: a read moving an entry adds nothing, so a memo read often and placed
// in seldom stays in few cohorts. The next is reserved first (reserveCohort);
// one that cannot be is logged (code_cache_memo_unreserved) and the newest
// stays open for another sixteenth. e.mu is not held; cohortMu is taken after
// every other lock.
func (c *Cache) memoPlaced(n int64) {
	c.cohortMu.Lock()
	defer c.cohortMu.Unlock()
	c.memoFill += max(n, 0)
	if c.memoFill < c.memoShare()/memoCohortsPerShare {
		return
	}
	c.memoFill = 0
	next := c.memoNewest.Load() + 1
	if err := c.reserveCohort(next); err != nil {
		c.log.Warn("code_cache_memo_unreserved", "cohort", next, "error", err)
		return
	}
	c.memoNewest.Store(next)
}

// openCohortPast makes the node's newest cohort one past n where it is not
// already, and reports whether the newest is past n: before the trim drops a
// newest that is also the oldest, and when the listing finds a cohort at or
// past the newest (a cache root that lost its reservation). A cohort that
// cannot be reserved is not opened, and is logged (code_cache_memo_unreserved).
func (c *Cache) openCohortPast(n uint64) bool {
	c.cohortMu.Lock()
	defer c.cohortMu.Unlock()
	if c.memoNewest.Load() > n {
		return true
	}
	if err := c.reserveCohort(n + 1); err != nil {
		c.log.Warn("code_cache_memo_unreserved", "cohort", n+1, "error", err)
		return false
	}
	c.memoNewest.Store(n + 1)
	c.memoFill = 0
	return true
}

// reserveCohort durably makes n the newest cohort any process of this cache
// may place in, before the node's newest moves to it: staged, synced, renamed
// over the root's cohortsName, the root synced. A number already reserved
// writes nothing. cohortMu is held.
//
// It is what makes a boot's newest new: a crash leaves cohorts no state
// records, and a boot opening past the states alone could reopen one, which
// the listing, passing over recorded cohorts, would never measure whole. One
// cohort a reservation, not a block: a boot opens past a whole block, and
// maxMemoCohorts reads the span of cohort numbers, so numbers skipped would
// drop cohorts that are not old.
func (c *Cache) reserveCohort(n uint64) error {
	if n <= c.memoReserved {
		return nil
	}
	staged, err := c.stage(hostbox.FileMode, func(f *os.File) error {
		_, err := f.WriteString(cohortName(n) + "\n")
		return err
	})
	if err != nil {
		return err
	}
	if err = c.place(staged, filepath.Join(c.dir, cohortsName)); err != nil {
		return err
	}
	if err = syncDir(c.dir); err != nil {
		return err
	}
	c.memoReserved = n
	return nil
}

// openMemo is Open's step 6: the node's newest cohort opened one past both the
// newest any state records and the newest the root reserved (cohortsName), so
// nothing this process places or reads reaches a previous process's cohort. A
// root with copies and no readable reservation is logged
// (code_cache_memo_cohorts_unread) and the boot opens past the states alone. A
// reservation that cannot be written fails the boot.
func (c *Cache) openMemo(recorded uint64) error {
	reserved, err := readReservedCohort(c.dir)
	if err != nil && (!errors.Is(err, fs.ErrNotExist) || len(c.entries) > 0) {
		c.log.Warn("code_cache_memo_cohorts_unread", "path", filepath.Join(c.dir, cohortsName), "error", err)
	}
	newest := max(reserved, recorded) + 1
	c.cohortMu.Lock()
	defer c.cohortMu.Unlock()
	if err = c.reserveCohort(newest); err != nil {
		return fmt.Errorf("cache: reserving the memo's cohort %d in %s — the cache directory must take writes: "+
			"fix the disk or the directory's permissions: %w", newest, filepath.Join(c.dir, cohortsName), err)
	}
	c.memoNewest.Store(newest)
	return nil
}

// maxReservationBytes bounds the read of a reservation: a cohort's number and
// a newline is at most 21 bytes, so anything longer is no reservation of this
// cache's, and is never read whole.
const maxReservationBytes = 32

// readReservedCohort is the cohort the reservation in the cache root at dir
// names (cohortsName): fs.ErrNotExist for none, and an error for one that is
// not a cohort's number and a newline.
func readReservedCohort(dir string) (uint64, error) {
	f, err := os.Open(filepath.Join(dir, cohortsName))
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // read-only: what was read is the answer
	raw, err := io.ReadAll(io.LimitReader(f, maxReservationBytes+1))
	if err != nil {
		return 0, err
	}
	text, whole := strings.CutSuffix(string(raw), "\n")
	n, ok := parseCohort(text)
	if !whole || !ok {
		return 0, fmt.Errorf("cache: %s holds %q, not a cohort's number and a newline",
			filepath.Join(dir, cohortsName), raw)
	}
	return n, nil
}

// errMemoShare is a tail the memo's share has no room for (stageMemo): its
// page is given back unmemoized, and logged as no failed write.
var errMemoShare = errors.New("cache: no room in the memo's share")

// complete ends st's staged file with tail and writes its header: the key, the
// body's length and the SHA-256 of all of it. The tail takes its room out of
// the memo's share as its page did (stageMemo); errMemoShare where there is
// none.
func (b *MemoBatch) complete(st *memoStaged, tail []byte) error {
	n := int64(len(tail))
	if !b.c.stageMemo(n) {
		return errMemoShare
	}
	st.sum.Write(tail)
	length := st.length + len(tail)
	var head [memoHeader]byte
	at := copy(head[:], memoMagic)
	head[at] = memoVersion
	copy(head[at+1:], st.key[:])
	binary.BigEndian.PutUint32(head[at+1+sha256.Size:], uint32(length))
	copy(head[memoHeader-sha256.Size:], st.sum.Sum(nil))
	f, err := os.OpenFile(st.path, os.O_WRONLY, 0)
	if err != nil {
		b.c.memoStaged.Add(-n)
		return err
	}
	_, err = f.WriteAt(tail, int64(memoHeader+st.length))
	if err == nil {
		_, err = f.WriteAt(head[:], 0)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		b.c.memoStaged.Add(-n)
		return err
	}
	st.size += n
	b.bytes += n
	b.c.tmpBytes.Add(n)
	return nil
}

// Discard gives every staged answer back, memoizing none: the run that cut
// them did not answer whole. Once a batch; nothing after Place.
func (b *MemoBatch) Discard() {
	if b == nil || b.done {
		return
	}
	b.done = true
	b.giveBack()
}

// giveBack removes every staged file still in tmp/ and takes its bytes out of
// the total and the memo's share.
func (b *MemoBatch) giveBack() {
	for i := range b.staged {
		b.unstage(&b.staged[i])
	}
	b.staged = nil
	b.c.tmpBytes.Add(-b.bytes)
	b.c.memoStaged.Add(-b.bytes)
	b.bytes = 0
}

// unstage removes st's staged file, if it is still there.
func (b *MemoBatch) unstage(st *memoStaged) {
	if st.path != "" {
		_ = os.Remove(st.path) // a staged answer is nobody's once given back
		st.path = ""
	}
}

// makeCohort makes cohort n's directory in inc's memo tree, and the tree, where
// missing, and reports which it made. A name held by a non-directory is an
// error: the next full round's sweep (image.imaged) or the janitor's listing
// moves it aside. Under the memo's lock.
func (c *Cache) makeCohort(inc *incarnation, n uint64) (tree, cohort bool, err error) {
	if err = os.Mkdir(filepath.Join(inc.dir, engineRoot), hostbox.DirMode); err != nil &&
		!errors.Is(err, fs.ErrExist) {
		return false, false, err
	}
	if tree, err = makeDir(filepath.Join(inc.dir, memoRel)); err != nil {
		return false, false, err
	}
	cohort, err = makeDir(cohortDir(inc.dir, n))
	return tree, cohort, err
}

// makeDir makes the directory path and reports whether it did; a directory
// there already is no error, and anything else at its name is.
func makeDir(path string) (bool, error) {
	err := os.Mkdir(path, hostbox.DirMode)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, fs.ErrExist):
		return false, err
	}
	if info, lerr := os.Lstat(path); lerr != nil || !info.IsDir() {
		return false, fmt.Errorf("cache: %s is no directory", path)
	}
	return false, nil
}

// foundMemo gives a tree on disk that no record names — a copy whose state
// named none, a previous process's — a record of no cohorts and leaves it for
// the janitor to list and count (recordDerived, listMemo), which it is kicked
// to do. e.mu is held.
func (c *Cache) foundMemo(inc *incarnation) {
	c.setMemo(inc, nil, true)
	inc.memoListed = false
	c.kick()
}

// dropMemo moves inc's memo tree into trash/ whole and forgets its record (the
// one removal of a tree, dropFor), under the memo's lock so nothing is placed
// in it as it goes. It also shuts the memo: its generation moves, so no batch
// begun before places anything, and none begins until the dropping round
// reopens it (reopenMemo). The memo drops only where the copy's objects
// change, and until that change is visible a read may cut an answer off what
// it replaces.
func (c *Cache) dropMemo(e *entry, inc *incarnation) error {
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	path := filepath.Join(inc.dir, memoRel)
	err := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path))
	moved := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	e.mu.Lock()
	if moved {
		c.droppedDerived(inc, c.image.set(DerivedMemo))
	}
	c.setMemo(inc, nil, false)
	inc.memoListed = true
	inc.memoGen++
	inc.memoShut = true
	e.mu.Unlock()
	if moved {
		c.kick()
		return syncDir(filepath.Join(inc.dir, engineRoot))
	}
	return nil
}

// reopenMemo opens inc's memo again once the full round that shut it has made
// its change visible — swept the packs the history it replaced named, so
// every object a read can meet is the new history's (sync.go apply). A memo
// not shut is left as it is. The round holds the repository's write lock.
func (c *Cache) reopenMemo(e *entry, inc *incarnation) {
	e.mu.Lock()
	inc.memoShut = false
	e.mu.Unlock()
}

// listMemos lists, behind the serving, the tree of every copy with a state
// that this process has not listed (incarnation.memoListed); a stateless
// boot's copy waits for its first full round. A request never lists a tree,
// and a listing reads only cohorts' names.
//
// A copy whose last listing failed (memoUnlisted) is retried at the tick
// alone, never at a kicked pass: passes run back to back on a busy node, and a
// listing holds the copy's memo lock while it measures, stalling every
// placement behind a failing disk. The tick bounds a fault to one measure and
// one log line a minute.
func (c *Cache) listMemos(ctx context.Context, ticked bool) {
	type due struct {
		e   *entry
		inc *incarnation
	}
	var dues []due
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.state != nil && !inc.memoListed && !inc.discarded &&
			(ticked || !inc.memoUnlisted) {
			dues = append(dues, due{e: e, inc: inc})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	for _, d := range dues {
		if ctx.Err() != nil || c.life.Err() != nil {
			return
		}
		c.listMemo(ctx, d.e, d.inc)
	}
}

// listMemo lists inc's memo tree (cohorts' names, never entries) and makes
// inc's record what it finds: a recorded cohort gone is forgotten, and one no
// record names (placed after the last state a crash kept) is measured and
// counted. Recorded cohorts are passed over, sound because nothing placed or
// read since boot reaches a previous process's cohort (openMemo). A tree
// holding anything but cohorts is another layout's and goes to trash/ unread.
// The memo's lock keeps the tree still but not the copy, whose directory a
// retirement can move into trash/ (Cache.trash), so a cohort found gone counts
// nothing and is no failure. A listing the pass's end cuts short is left to
// the next pass unlogged; one that fails is logged and left to the next tick
// (unlisted).
func (c *Cache) listMemo(ctx context.Context, e *entry, inc *incarnation) {
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	e.mu.Lock()
	skip := inc.memoListed || inc.discarded
	e.mu.Unlock()
	if skip {
		return
	}
	tree := filepath.Join(inc.dir, memoRel)
	found, foreign, err := listCohorts(tree)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e.mu.Lock()
		if !inc.discarded {
			c.setMemo(inc, nil, false)
			inc.memoListed, inc.memoUnlisted = true, false
		}
		e.mu.Unlock()
		return
	case err != nil:
		c.unlisted(e, inc, "error", err)
		return
	case foreign != "":
		if terr := toTrash(c.dir, tree, newV7()+"-memo"); terr != nil && !errors.Is(terr, fs.ErrNotExist) {
			c.unlisted(e, inc, "error", terr)
			return
		}
		e.mu.Lock()
		if !inc.discarded {
			c.setMemo(inc, nil, false)
			inc.memoListed, inc.memoUnlisted = true, false
		}
		e.mu.Unlock()
		c.log.Warn("code_cache_memo_foreign", "repo", e.id, "incarnation", inc.n, "found", foreign)
		return
	}
	e.mu.Lock()
	recorded := inc.derived[DerivedMemo].Cohorts
	e.mu.Unlock()
	var cs []memoCohort
	for _, ch := range recorded {
		if slices.Contains(found, ch.N) {
			cs = append(cs, ch)
		}
	}
	var (
		measuring func(dir string)
		stating   func(path string) error
	)
	if c.knobs != nil {
		measuring, stating = c.knobs.measuring, c.knobs.statingMemo
	}
	for _, n := range found {
		if holdsCohort(recorded, n) {
			continue
		}
		dir := cohortDir(inc.dir, n)
		if measuring != nil {
			measuring(dir)
		}
		bytes, err := measureCohort(ctx, dir, stating)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// Gone since listed: the copy retired, or something outside the
			// engine removed it. Logged, a gone copy would be listed again every
			// tick; a discarded copy is let go below, a live one recorded without
			// the cohort.
			continue
		case err != nil && ctx.Err() != nil:
			// The pass ended, not the listing: a stop, no failure, and the copy
			// stays unlisted for the next pass or process.
			return
		case err != nil:
			// A cohort that could not be counted (measureCohort): the copy stays
			// unlisted, so each tick measures it again and the line repeats until
			// the disk is fixed. Meanwhile nothing reaches the cohort (openMemo),
			// so the copy counts short by it.
			c.unlisted(e, inc, "cohort", n, "error", err)
			return
		}
		cs = withCohort(cs, n, bytes)
		// Already past every reserved cohort unless the root lost its
		// reservation (openMemo). A newest that cannot move is openCohortPast's
		// to log and costs no count: this cohort is recorded whole.
		_ = c.openCohortPast(n)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.discarded {
		return
	}
	// A tree is recorded while it is there, with cohorts or none: one with no
	// record is what foundMemo gives a tree to list, so it would be listed again
	// every round.
	c.setMemo(inc, cs, true)
	inc.memoListed, inc.memoUnlisted = true, false
}

// unlisted logs a listing of inc's tree that failed (code_cache_memo_unlisted,
// with what failed in args) and leaves the copy unlisted for the janitor's
// next tick (listMemos says why not its next pass). The memo's lock is held.
func (c *Cache) unlisted(e *entry, inc *incarnation, args ...any) {
	c.log.Warn("code_cache_memo_unlisted", append([]any{"repo", e.id, "incarnation", inc.n}, args...)...)
	e.mu.Lock()
	inc.memoUnlisted = true
	e.mu.Unlock()
}

// listCohorts is the cohorts the memo tree at dir holds, in order; or the
// first name in it that is no cohort's directory, which makes the tree none
// of this build's — read a batch of names at a time, so a tree a previous
// layout filled with entries is never held whole.
func listCohorts(dir string) (found []uint64, foreign string, err error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = f.Close() }() // read-only: what was listed is the answer
	for {
		list, rerr := f.ReadDir(memoListBatch)
		for _, d := range list {
			n, ok := parseCohort(d.Name())
			if !ok || !d.IsDir() {
				return nil, d.Name(), nil
			}
			found = append(found, n)
		}
		switch {
		case errors.Is(rerr, io.EOF):
			slices.Sort(found)
			return found, "", nil
		case rerr != nil:
			return nil, "", rerr
		}
	}
}

// memoListBatch is how many names a listing of the memo reads at once: a
// cohort's directory, measured, holds as many entries as a sixteenth of the
// share's placements left in it, so the listing holds a batch of names rather
// than all of them.
const memoListBatch = 256

// measureCohort is what the cohort directory at dir holds as the memo counts
// it: each regular file at its blocks, read a batch of names at a time;
// anything else counts nothing and goes with the cohort. stating is nil but in
// a test (knobs.statingMemo): told each entry's path before its stat, and an
// error it answers is that stat's.
//
// An entry gone by its stat counts nothing; every other stat error is the
// measure's, the rule eachFile and image.sizes keep: an entry that cannot be
// stat'ed (EIO, EACCES) still holds bytes, and passed over it would leave the
// budget overrun while every gauge reads within it. listMemo logs it
// (code_cache_memo_unlisted).
func measureCohort(ctx context.Context, dir string, stating func(path string) error) (int64, error) {
	f, err := os.Open(dir)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }() // read-only: what was measured is the answer
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		list, rerr := f.ReadDir(memoListBatch)
		for _, d := range list {
			if !d.Type().IsRegular() {
				continue
			}
			var fault error
			if stating != nil {
				fault = stating(filepath.Join(dir, d.Name()))
			}
			info, ierr := d.Info()
			if fault != nil {
				ierr = fault
			}
			switch {
			case errors.Is(ierr, fs.ErrNotExist):
				continue
			case ierr != nil:
				return 0, ierr
			}
			total += memoBytesOf(info.Size())
		}
		switch {
		case errors.Is(rerr, io.EOF):
			return total, nil
		case rerr != nil:
			return 0, rerr
		}
	}
}

// trimMemo drops the memo's oldest cohort whole until the memo, its staged
// pages included (memoExcess), is within its share (MemoShare), and on a
// ticked pass also while the node keeps more than maxMemoCohorts; it reports
// whether it moved anything into trash/. A newest that is also the oldest has
// the next opened first, and where that cannot be reserved the trim stops.
// Runs before any repository is evicted (janitorPass).
func (c *Cache) trimMemo(ctx context.Context, ticked bool) bool {
	if c.memoExcess() == 0 && !ticked {
		return false
	}
	moved := false
	for ctx.Err() == nil && c.life.Err() == nil {
		oldest, ok := c.oldestCohort()
		if !ok {
			break
		}
		newest := c.memoNewest.Load()
		if c.memoExcess() == 0 && (newest < oldest || newest-oldest < maxMemoCohorts) {
			break
		}
		if oldest >= newest && !c.openCohortPast(oldest) {
			// The newest is never dropped while it is the newest, so the trim
			// stops here, and its next pass tries the reservation again.
			break
		}
		dropped, err := c.trimCohort(ctx, oldest)
		if err != nil {
			break
		}
		moved = moved || dropped
	}
	return moved
}

// oldestCohort is the oldest cohort any copy's memo holds, and false for none:
// one walk of the node's copies, under the lock every sync takes for the
// moment it reads each record's first cohort.
func (c *Cache) oldestCohort() (uint64, bool) {
	var oldest uint64
	found := false
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && !inc.discarded {
			if cs := inc.derived[DerivedMemo].Cohorts; len(cs) > 0 && (!found || cs[0].N < oldest) {
				oldest, found = cs[0].N, true
			}
		}
		e.mu.Unlock()
	}
	return oldest, found
}

// trimCohort moves cohort n of every copy's memo into trash/ — its directory,
// one rename, whatever it holds — and takes it out of each copy's record, and
// reports whether it moved any. A rename that fails is logged and stops the
// trim, which the next pass takes up: the cohort stays recorded, its bytes
// counted, for as long as its files are there.
func (c *Cache) trimCohort(ctx context.Context, n uint64) (bool, error) {
	type holder struct {
		e   *entry
		inc *incarnation
	}
	var holders []holder
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && !inc.discarded && holdsCohort(inc.derived[DerivedMemo].Cohorts, n) {
			holders = append(holders, holder{e: e, inc: inc})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	moved, copies, bytes := false, 0, int64(0)
	defer func() {
		if copies > 0 {
			c.log.Info("code_cache_memo_trimmed", "cohort", n, "copies", copies, "bytes", bytes)
		}
	}()
	for _, h := range holders {
		if ctx.Err() != nil || c.life.Err() != nil {
			return moved, ctx.Err()
		}
		freed, gone, err := c.trimCohortOf(h.e, h.inc, n)
		if err != nil {
			c.log.Warn("code_cache_memo_untrimmed", "repo", h.e.id, "cohort", n, "error", err)
			return moved, err
		}
		if gone {
			copies++
			bytes += freed
			moved = true
		}
	}
	return moved, nil
}

// trimCohortOf moves inc's cohort n into trash/ where inc still holds it, and
// answers what its record counted of it and whether it went. Under the memo's
// lock.
//
// A copy that goes whole under the move gives the trim nothing: the memo's
// lock holds off a rebuild's promote and a swap (carryMemo) but not a purge,
// after which Cache.trash moves the directory, cohort and all, its bytes out
// of the totals already (discard); counted, code_cache_memo_trimmed would
// report the purge's bytes, so a discarded copy is let go uncounted. discard
// marks the copy under e.mu before its directory moves, so the absence a whole
// move makes is always a discarded copy's. A cohort gone from a copy still
// held is forgotten.
func (c *Cache) trimCohortOf(e *entry, inc *incarnation, n uint64) (int64, bool, error) {
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	e.mu.Lock()
	held := e.current == inc && !inc.discarded && holdsCohort(inc.derived[DerivedMemo].Cohorts, n)
	e.mu.Unlock()
	if !held {
		return 0, false, nil
	}
	dir := cohortDir(inc.dir, n)
	if c.knobs != nil && c.knobs.trimming != nil {
		c.knobs.trimming(dir)
	}
	err := toTrash(c.dir, dir, newV7()+"-memo-"+cohortName(n))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil && inc.discarded {
		return 0, false, nil
	}
	cs, freed := withoutCohort(inc.derived[DerivedMemo].Cohorts, n)
	c.setMemo(inc, cs, true)
	return freed, true, nil
}

// stateMemos writes again the state of every copy whose memo's record moved
// since its state was written (incarnation.memoVersion), so a boot counts the
// memo within a tick (Layout.Held) without walking it: on the ticked pass
// alone, at most two syncs a copy an EvictInterval, and never a first state.
//
// It holds nothing of a copy as it writes (not a pin, which the cache reads as
// a reader's and would stop a boot copy's in-place stray sweep, packStrays),
// so a copy may be retired, or written by another, under it. memoStateDue is
// asked as a copy is picked, as its write begins, and if it fails: retire
// marks a copy under e.mu before its directory moves, so a failure for a copy
// no longer due is none, and code_cache_memo_unstated is logged only for one
// still due, which the next tick writes.
func (c *Cache) stateMemos(ctx context.Context) {
	type due struct {
		e   *entry
		inc *incarnation
	}
	var dues []due
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; memoStateDue(inc) {
			dues = append(dues, due{e: e, inc: inc})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	for _, d := range dues {
		if ctx.Err() != nil || c.life.Err() != nil {
			return
		}
		if !d.e.memoStillDue(d.inc) {
			continue
		}
		if err := c.persist(ctx, d.e, d.inc, false); err != nil && d.e.memoStillDue(d.inc) {
			c.log.Warn("code_cache_memo_unstated", "repo", d.e.id, "incarnation", d.inc.n, "error", err)
		}
	}
}

// memoStateDue reports whether inc, its entry's current copy when the janitor
// picked it, is due a state for its memo's moves (stateMemos): in service —
// neither retired nor discarded, and so still its entry's current copy — with
// a state, whose memo's record moved since that state was written. The
// entry's mu is held.
func memoStateDue(inc *incarnation) bool {
	return inc != nil && !inc.retired && !inc.discarded && inc.state != nil && inc.memoVersion != inc.memoStated
}

// memoStillDue is memoStateDue of inc, a copy of e's, under e.mu: what
// stateMemos asks of a copy it picked as the copy's write begins, and again if
// the write fails.
func (e *entry) memoStillDue(inc *incarnation) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return memoStateDue(inc)
}

// carryMemo moves the memo of from, the incarnation a rebuild or a swap
// replaces, into to as to is promoted: one rename of the tree whatever it
// holds (SeedMove), with its record and listing marks (memoListed,
// memoUnlisted), so a failing disk is still measured at the tick alone.
// Nothing is read: to holds from's objects, and every read checks an entry's
// digest. Under from's memo lock, then to's; from's memo is then shut. Nothing
// is carried from a memo whose history is gone or that is shut, nor into a to
// whose round dropped a memo (its generation moved): its answers are of
// another history. A failed move is logged and to starts with none. No sync: a
// lost rename leaves the tree in from, which the boot moves aside; one kept
// before to's state is found by to's first round (foundMemo).
func (c *Cache) carryMemo(e *entry, from, to *incarnation) {
	if d := c.image.decl(DerivedMemo); d == nil || d.Seed != SeedMove {
		return
	}
	from.memoMu.Lock()
	defer from.memoMu.Unlock()
	to.memoMu.Lock()
	defer to.memoMu.Unlock()
	e.mu.Lock()
	_, recorded := from.derived[DerivedMemo]
	carry := recorded && !from.historyGone && !from.memoShut && !from.discarded && !to.discarded &&
		to.memoGen == 0
	e.mu.Unlock()
	if !carry {
		return
	}
	err := os.Mkdir(filepath.Join(to.dir, engineRoot), hostbox.DirMode)
	if err == nil || errors.Is(err, fs.ErrExist) {
		err = os.Rename(filepath.Join(from.dir, memoRel), filepath.Join(to.dir, memoRel))
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_memo_uncarried", "repo", e.id, "from", from.n, "to", to.n, "error", err)
		}
		return
	}
	e.mu.Lock()
	rec, listed, unlisted := from.derived[DerivedMemo], from.memoListed, from.memoUnlisted
	c.setMemo(from, nil, false)
	from.memoListed, from.memoUnlisted = true, false
	from.memoGen++
	from.memoShut = true
	c.setMemo(to, rec.Cohorts, true)
	to.memoListed, to.memoUnlisted = listed, unlisted
	e.mu.Unlock()
	c.log.Info("code_cache_memo_carried", "repo", e.id, "from", from.n, "to", to.n, "bytes", rec.Bytes)
}
