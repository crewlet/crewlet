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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE MEMO OF ANSWERS BY OBJECT ID: every page a server-side read cut — a
// listing's, a file's, a diff's, a comparison — kept beside the incarnation it
// was read from, one file an answer under crewlet/memo, keyed by everything it
// is a function of (NewMemoKey), so the next read that asks the same is
// answered from a file rather than from git. A derived file of the cache's own
// (derived.go's DerivedMemo), so the budget counts it and it leaves with the
// repository; internal/code/query forms its keys and its bodies, and the cache
// keeps, checks, counts and trims them, knowing nothing of what one says.
//
// # Immutable, so never stale — and answered with no git run
//
// An object id names one object for ever, so an answer that is a function of
// object ids and its read's arguments is that answer for as long as the ids
// name anything — whatever packs hold them, whatever refs point at them — and
// no push, compaction or ref moved ever makes an entry wrong: an entry is only
// ever absent, never stale (FreshImmutable). It is the COPY'S: an entry is in
// an incarnation's memo only where a read of that incarnation computed it from
// objects the incarnation held, so no answer crosses repositories, and a read
// naming whole ids is answered from it before any git runs — the copy is not
// asked again whether it holds them (internal/code/query). That rests on an
// incarnation never losing an object it held while its history stands: packs
// are only added to it, a rebuild holds the same rows' packs (and the memo
// moves into it whole, SeedMove), and an object leaves a copy only with its
// history — a log restored or recreated under the node, another history, which
// the memo cannot tell entry by entry. So a history gone drops the memo whole
// (OnHistoryGone) and SHUTS it until the full round that dropped it has swept
// the packs the history it replaced named (reopenMemo): no answer a read cut
// while both histories' packs were on disk is placed, and no batch begun
// before the drop places anything after it (MemoBatch's generation). A change
// a later part makes that takes an object out of a copy drops the memo the
// same way, shut until that change is visible.
//
// # An entry is whole or it is not served
//
// An entry is one file, "CRMM" | version | its key | its body's length | the
// SHA-256 of its body | its body, staged in tmp/ and renamed in with NO SYNC:
// an entry is a cache of a cache, so a crash may lose one, and EVERY READ holds
// an entry to its key and its digest before it is answered (CheckEntryDigest)
// — a torn or flipped one is a miss, removed, and computed again. So nothing
// behind the serving hashes the memo: the check and the scrub pass over it
// (Check.hashed), which would otherwise read a memo of gigabytes whole for
// nothing a read does not already refuse.
//
// A read's run stages every page it cuts as it cuts it (MemoBatch.Put), and
// places them together — each ending with what the run learned of it after
// it cut it: a stat's page's files' counts, written into its staged entry as
// soon as the stat has counted past the page (Extend), so a run holds the
// counts of one page at a time, and what only the run's end tells, a
// listing's total, a file's line count, a stat's totals (Place's tails) —
// only once its git has answered whole and reported no damage: an answer cut
// from a run that met damage is never memoized.
//
// # Cohorts: the order of use kept by the disk, and nothing an entry in memory
//
// A copy's memo is its COHORTS, a directory each, crewlet/memo/<n>/<key>:
// every entry lives in the cohort that was the node's NEWEST when it was last
// placed or read. The node opens a new cohort once a sixteenth of the memo's
// share has been placed in the newest (memoCohortsPerShare), and a read that
// finds an entry in an older cohort moves it into the newest, one rename
// (memoUse) — so a cohort holds what was last used while it was the newest,
// and the OLDEST cohort is the sixteenth of the memo used least recently,
// whichever copies hold it. The cache keeps, for each copy, its cohorts'
// numbers and the bytes each holds (its record, derivedState.Cohorts) and
// NOTHING AN ENTRY: no index, no last use, no key, whatever the memo holds —
// where a map of every entry this process had touched, 112 bytes an entry
// measured, held some 680 MB at a 400 GB budget's worst case (6.1 million
// entries of a block each) and 3 GB at the 1.75 TB a 2 TB dedicated volume
// derives, memory the disk budget bounded and nothing charged. What a copy
// holds in memory is 16 bytes a cohort, its cohorts some seventeen at the most
// (the share's sixteen and the newest; maxMemoCohorts bounds them whatever
// removals thinned), so 10 000 copies hold under 3 MB. A lookup opens the
// entry's name in the copy's cohorts newest first: one open for a page read
// since the newest opened, one an older cohort for an older one, and one a
// cohort for a miss, which then runs git.
//
// # Its share of the budget, trimmed a cohort at a time, before any repository
//
// The memo holds at most a MemoShare-th of the cache's budget, node-wide,
// counted as a running total (Cache.memoBytes) and each entry at the volume it
// takes (memoBytesOf) — and with it every page a read's run has staged for it
// and not yet placed (Cache.memoStaged), which is the memo's as much: a page
// in flight takes its room from the memo's own answers, never from a
// repository (Cache.overBudget, the eviction's measure, leaves out whatever
// the memo holds past its share), and one that would take the staged pages
// past the share is not staged at all (MemoBatch.Put). The janitor holds it
// there BEFORE it evicts any repository (janitorPass), woken by the placement
// that takes it past its share, by DROPPING THE OLDEST COHORT WHOLE: one
// rename of its directory in each copy holding it, the memo's record of it
// gone with it (trimMemo). A trim gives back a sixteenth of the share, so it
// falls due once a sixteenth of the share of new pages has been placed rather
// than after every miss, and costs one walk of the node's copies and a rename
// for each copy holding the cohort, however many entries the cohort holds:
// BenchmarkMemoTrim, over 1 000 and 10 000 copies of seventeen cohorts each,
// measures it at 55 and 66 µs a copy, 0.66 s for 10 000 (cache doc.go,
// "Measured"). The removal of the entries' files is the janitor's emptying of
// trash/, as it is for every removal the cache makes.
//
// # What a boot and a crash leave
//
// A copy's state records its cohorts and their bytes; the janitor writes it
// again once the memo's record has moved since it was last written — at its
// TICK, so at most once an EvictInterval a copy however often the memo moves,
// since a state is two syncs (janitorPass) — so a boot counts the memo within
// a tick of its writes (Layout.Held) without walking it. A boot opens a
// cohort past every one a state records, so whatever a previous process left
// is older than anything placed or read since, and goes first unless a read
// moves it on. The janitor LISTS each copy's tree once a process, behind the
// serving and never on a request (listMemos): the names of its cohorts, never
// its entries. A cohort no state records — placed after the last state a crash
// kept — is measured then and counted; one recorded and gone is forgotten; and
// a tree holding anything but cohorts is no memo of this build's and goes
// whole, unread. What a crash costs the count is what was placed in a cohort a
// state records after that state was written — a tick's writes at most, given
// back with the cohort when the trim takes it.

// memoFile is this file as the memo's declared write names its writer
// (Writer.File): every placement, move and removal of an entry is made here.
const memoFile = "internal/code/cache/memo.go"

// MemoShare is the share of the cache's budget the memo may hold, node-wide:
// the budget divided by it, one sixteenth. An answer is at most
// code.MaxGitAnswerBytes (56 KiB) of JSON and costs a git process to compute —
// a few milliseconds warm, up to gitproc.ReadDeadline (10 s) — so a sixteenth
// of a 400 GB budget is some 450 000 answers at their largest, while fifteen
// sixteenths stay the repositories the budget exists for: a repository evicted
// costs a cold sync of every pack it holds, an answer evicted one git process.
// The janitor trims the memo to it BEFORE it evicts any repository. A JUDGMENT
// CALL, of what the cache is for: BenchmarkCodeRead's memo arms measure what a
// hit saves (internal/code/query), and crewlet.code.cache.memo.lookups beside
// crewlet.code.cache.rebuilds and the evictions the status counts is what
// re-decides it — misses climbing while the memo sits at its share, with
// repositories rarely evicted, is a share too small for the answers seats ask
// again.
const MemoShare = 16

// memoCohortsPerShare is how many cohorts a memo at its share holds: the
// newest is closed once a sixteenth of the share has been placed in it
// (memoPlaced). It is the trim's GRANULARITY, and so its rate: a trim drops
// the oldest cohort, a sixteenth of the share, and the memo is then that far
// under its share, so the next is due once as much again has been placed —
// where a trim of single entries to the share exactly fell due again at the
// very next miss, and paid a walk of every entry each time. Sixteen, so a
// trim takes the least recently used sixteenth of the memo and leaves fifteen
// sixteenths of it, while a lookup's misses open at most some seventeen names:
// fewer cohorts trim more of the memo at once than its use says to, more cost
// every miss an open more. A JUDGMENT, at the scale it is read at: a cohort of
// a 400 GB budget's memo is 1.6 GB of pages, and of a test's a few blocks.
const memoCohortsPerShare = 16

// maxMemoCohorts bounds the cohorts the node keeps, whatever their bytes: past
// it, the janitor's tick drops the oldest though the memo is within its share.
// A cohort is opened only by a sixteenth of the share's placements and the
// trim drops one once the memo holds its share, so a memo the disk keeps holds
// seventeen; what thins old cohorts without the memo filling — damaged entries
// removed, entries moved on by reads — would otherwise leave ever more of them
// for every miss to open. Twice the share's sixteen: a cohort nobody has read
// from for two shares' worth of placements is dropped.
const maxMemoCohorts = 2 * memoCohortsPerShare

// memoBlock is the volume an entry takes at the least: a file is given whole
// blocks, 4 KiB at ext4's, xfs's and btrfs's defaults and the page size, so an
// answer of a hundred bytes takes 4 KiB of the disk. Counted at its length, a
// memo of small answers would hold several times its share of the volume; each
// entry is counted at its length rounded up to it (memoBytesOf).
const memoBlock = 4 << 10

// The entry format: "CRMM", its version, the key it answers, its body's
// length and its body's SHA-256, then the body. A reshape takes a new version
// and makes every entry of another a miss, removed as any that fails.
const (
	memoMagic   = "CRMM"
	memoVersion = 1
	// memoHeader is the header's bytes: the magic, the version, the key, the
	// length and the digest.
	memoHeader = len(memoMagic) + 1 + sha256.Size + 4 + sha256.Size
	// memoMaxTail bounds what a run appends to a page it staged, by Extend as
	// it learns it and by Place's tail at its end together: what the run
	// learned of the page only after cutting it — a listing's total, a file's
	// line count, a diff stat's counts of every file on the page — which is
	// never more than the page itself, an answer at its bound: a file's counts
	// are a few bytes against the hundred its entry of the page takes at the
	// least.
	memoMaxTail = code.MaxGitAnswerBytes
	// memoMaxBody is the most a body holds: an answer at its bound and its
	// tail at its own.
	memoMaxBody = code.MaxGitAnswerBytes + memoMaxTail
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
	memoHit     = "hit"
	memoMiss    = "miss"
	memoCorrupt = "corrupt"
)

// memoing reports whether the build declares the memo — a case's cache opened
// over declarations without it (WithDerived) keeps none.
func (c *Cache) memoing() bool { return c.image.decl(DerivedMemo) != nil }

// setMemo records cohorts as what inc's memo holds — no memo where present is
// false — its bytes its cohorts' together, and where that is a change moves
// its version, so the janitor writes the state again at its tick
// (stateMemos). EVERY CHANGE to the memo's record goes through here, so no
// move of an entry between cohorts is a change the state misses — and what
// changes nothing, a listing of a copy with no memo among them, writes no
// state. e.mu is held.
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

// MemoGet is the body memoized under k in the incarnation s holds — whole,
// its header naming k and the SHA-256 of what it holds — or false: no entry,
// or one that fails, which is removed (logged code_cache_memo_corrupt) so the
// read that misses writes it again. It opens k's name in the copy's cohorts,
// newest first; a copy with no memo misses with no syscall. A hit is a USE: an
// entry found in an older cohort than the node's newest moves into the newest
// (memoUse), so the trim, which takes the oldest cohort, takes it last.
func (c *Cache) MemoGet(s Synced, k MemoKey) ([]byte, bool) {
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
		c.lookedUp(memoHit)
		return body, true
	}
	c.lookedUp(memoMiss)
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

// memoUse is a hit of inc's entry k in its cohort n, of bytes, as the USE it
// is: the entry moved into the node's newest cohort — one rename, and only
// where n is older — so the cohort it leaves holds what nobody has read since,
// and the trim reaches it last. Under the memo's lock, where the copy still
// serves its memo and the file is still the one the read found (info): one a
// placement renamed over it since is somebody's newer answer, and moves as
// that one's use. A HIT NEVER WAITS: where the lock is held — a run placing
// its pages, the trim, the janitor listing the tree — the entry stays where it
// is this once, and the next read of it moves it; and a move that cannot be
// made leaves it there too. The answer is served either way.
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

// memoStaged is one staged answer: its key, which Put it was (put), its file
// in tmp/ ("" once placed or given back), its body's length so far — as put,
// and as Extend lengthened it — and what Extend appended of it, its body's
// SHA-256 so far, which the run's tail completes, and the bytes its file
// holds — the header, the body and, once completed, the tail.
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

// Put stages body as the answer under k: written to tmp/ with NO SYNC — every
// read holds an entry to its header, so a crash may lose one and never serves
// a torn one — and memoized only by Place. A body past code.MaxGitAnswerBytes
// is refused, and a write that fails leaves the answer unmemoized, logged once
// a batch (code_cache_memo_unwritten): a memo that cannot be written is a
// slower read, never a failed one.
//
// A STAGED PAGE IS THE MEMO'S, counted against its share beside the pages it
// keeps (memoExcess): one that would take what every run has staged past the
// share is not staged at all — the memo could never keep it, and staged it
// would hold room the repositories' budget is for (overBudget) — and the run
// answers it unmemoized, as it does a page whose write failed.
//
// It answers which Put this was — what Extend and Place's tail name the
// answer by — staged or not; -1 for a batch that takes no more.
func (b *MemoBatch) Put(k MemoKey, body []byte) int {
	if b == nil || b.done {
		return -1
	}
	put := b.puts
	b.puts++
	if len(body) > code.MaxGitAnswerBytes {
		b.fail(fmt.Errorf("an answer of %d bytes, past the %d one holds", len(body), code.MaxGitAnswerBytes))
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

// Extend appends data to the answer the put-th Put staged: what the run
// learned of it after it cut it and before its end — a diff stat's page's
// files' counts, once the stat has counted past the page — so the run holds
// none of it until its end, where Place's tail would. It takes its room out of
// the memo's share as its page did (stageMemo), and with the tail Place
// appends it is held to memoMaxTail; refused either way, or a write that
// fails, the answer is given back unmemoized. Before Place; an answer not
// staged takes nothing.
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

// stageMemo takes room for n bytes of staged pages out of the memo's share,
// and reports whether there was room: none where every run's staged pages
// would then hold more than the share. A RESERVATION, added before it is
// judged and given back when refused, so two runs staging at once never both
// take the last of the room; one may then be refused room the other gives
// back a moment later, which costs it one page unmemoized.
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

// Place memoizes every answer staged, the one the i-th Put staged ending with
// tail(i) — what the run learned of it only at its end: a listing's total, a
// file's line count, a stat's counts; i counts every Put, so one that failed
// to stage shifts no other's tail — into the incarnation the batch is of:
// renamed into the node's NEWEST cohort of its memo, the cohort's directory
// and the tree made for its first answer, under the memo's lock, where the
// incarnation still serves, the cache still runs, and the memo is neither
// shut nor dropped since the batch began (its generation). A tail past
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
	// placed is the volume the answers take, and prior that of what they were
	// renamed over — answers of the same keys an earlier placement made in
	// this cohort, whose bytes the record counts already. A cohort this Place
	// made holds nothing else.
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
	defer e.mu.Unlock()
	if inc.discarded {
		return // its bytes left the total with it
	}
	cs := inc.derived[DerivedMemo].Cohorts
	switch {
	case tree:
		// A TREE THIS PLACE MADE holds exactly what it placed: whatever a
		// record said of one before is gone with it, and nothing on the disk
		// is one the record does not know.
		cs = nil
		inc.memoListed = true
	case made:
		// So does a cohort it made.
		cs, _ = withoutCohort(cs, n)
	}
	c.setMemo(inc, withCohort(cs, n, placed-prior), true)
	c.memoPlaced(placed - prior)
	// PAST ITS SHARE, the memo's trim is due at once rather than at the
	// janitor's tick: no eviction is for the excess (overBudget), so nothing
	// else would wake the janitor for it.
	if c.memoExcess() > 0 {
		c.kick()
	}
}

// memoPlaced counts n bytes placed in the node's newest cohort, and opens the
// next once the newest holds a sixteenth of the share (memoCohortsPerShare): a
// cohort is the answers placed and read over a sixteenth of the share's worth
// of placements, which is what a trim of it gives back. Only a placement opens
// a cohort: an entry a read moves on adds nothing to the memo, so a memo read
// over and over and placed in seldom stays in few cohorts. e.mu may be held;
// cohortMu is taken after every other lock.
func (c *Cache) memoPlaced(n int64) {
	c.cohortMu.Lock()
	defer c.cohortMu.Unlock()
	c.memoFill += max(n, 0)
	if c.memoFill >= c.memoShare()/memoCohortsPerShare {
		c.memoNewest.Add(1)
		c.memoFill = 0
	}
}

// openCohortPast makes the node's newest cohort one past n, where it is not
// already: what the trim does before it drops a newest cohort that is also
// the oldest, and the listing for a cohort a previous process opened past
// every one its states record.
func (c *Cache) openCohortPast(n uint64) {
	c.cohortMu.Lock()
	defer c.cohortMu.Unlock()
	if c.memoNewest.Load() <= n {
		c.memoNewest.Store(n + 1)
		c.memoFill = 0
	}
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

// makeCohort makes cohort n's directory in inc's memo tree, and the tree,
// where either is not there, and reports which it made; a name held by
// something that is no directory is an error — the next full round's sweep
// moves aside what holds the tree's name (image.imaged), and the janitor's
// listing a tree holding what no cohort is. Under the memo's lock.
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

// dropMemo moves inc's memo tree into trash/ whole and forgets what inc
// records of it — under the memo's lock, so no answer is placed into the tree
// as it goes and counted for a directory no longer there: the one removal of
// a tree (dropFor). A memo with no tree on disk is forgotten the same. AND IT
// SHUTS THE MEMO: its generation moves, so no batch begun before the drop
// places anything, and none is begun until the round that dropped it reopens
// it (reopenMemo) — the memo drops only where the objects of the copy change
// under it, and an answer a read cuts until that change is visible may be
// read off what it replaces.
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

// listMemos lists, behind the serving, the tree of every copy this process has
// not yet listed (incarnation.memoListed) and that has a state — one a round
// or a boot gave it; a copy with none is a stateless boot's, whose first full
// round measures what it holds, and is listed after. A request never lists a
// tree, and a listing reads a tree's cohorts' names alone.
func (c *Cache) listMemos(ctx context.Context) {
	type due struct {
		e   *entry
		inc *incarnation
	}
	var dues []due
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.state != nil && !inc.memoListed && !inc.discarded {
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

// listMemo lists inc's memo tree — its cohorts' names, never their entries —
// and makes inc's record what it finds: a cohort recorded and gone is
// forgotten, and one no record names, placed after the last state a crash
// kept, is MEASURED and counted, the node's newest opened past it. A tree
// holding anything but cohorts is no memo of this build's — a build of another
// layout's, or something outside the engine's — and goes whole into trash/,
// unread. Under the memo's lock, so nothing is placed, moved or removed in the
// tree while it is read — but the copy itself can go: a purge retires it, as
// does the rebuild that replaced it once its last reader lets go, and its
// directory moves into trash/ whole (Cache.trash), which no memo lock guards.
// A cohort the listing named and then found gone is therefore nothing to
// count and no failure. A listing that fails, or that the pass's end cuts
// short, leaves the copy to the next pass; only the failure is logged.
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
			inc.memoListed = true
		}
		e.mu.Unlock()
		return
	case err != nil:
		c.log.Warn("code_cache_memo_unlisted", "repo", e.id, "incarnation", inc.n, "error", err)
		return
	case foreign != "":
		if terr := toTrash(c.dir, tree, newV7()+"-memo"); terr != nil && !errors.Is(terr, fs.ErrNotExist) {
			c.log.Warn("code_cache_memo_unlisted", "repo", e.id, "incarnation", inc.n, "error", terr)
			return
		}
		e.mu.Lock()
		if !inc.discarded {
			c.setMemo(inc, nil, false)
			inc.memoListed = true
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
	var measuring func(dir string)
	if c.knobs != nil {
		measuring = c.knobs.measuring
	}
	for _, n := range found {
		if holdsCohort(recorded, n) {
			continue
		}
		dir := cohortDir(inc.dir, n)
		if measuring != nil {
			measuring(dir)
		}
		bytes, err := measureCohort(ctx, dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// GONE SINCE THE LISTING NAMED IT: the copy was retired and its
			// directory moved into trash/ while this listing held the memo's
			// lock, or something outside the engine removed the cohort. It
			// holds nothing to count. Logged, it would say this copy could
			// not be listed and is listed again at the next pass — of a copy
			// that is gone, on any node whose boot listing meets a purge. A
			// discarded copy is let go below; a copy still here is recorded
			// without the cohort, as a recorded cohort gone is forgotten.
			continue
		case err != nil && ctx.Err() != nil:
			// THE PASS ENDED, not the listing: the engine stopping ends the
			// context the janitor runs under, and measureCohort answers it.
			// Nothing failed, and the copy stays unlisted for the next pass
			// or process — a stop, as every duty of the cache tells one from
			// a failure (scrub, backfill, maintenance).
			return
		case err != nil:
			c.log.Warn("code_cache_memo_unlisted", "repo", e.id, "incarnation", inc.n, "cohort", n, "error", err)
			return
		}
		cs = withCohort(cs, n, bytes)
		c.openCohortPast(n)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.discarded {
		return
	}
	// A TREE IS RECORDED WHILE IT IS THERE, holding cohorts or none: one with
	// no record is what a round's measure gives a record to be listed
	// (foundMemo), which would list it again at every round.
	c.setMemo(inc, cs, true)
	inc.memoListed = true
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
// it: each regular file at the blocks it takes, read a batch of names at a
// time. Anything else in it is no entry and counts nothing; it goes with the
// cohort.
func measureCohort(ctx context.Context, dir string) (int64, error) {
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
			if info, ierr := d.Info(); ierr == nil {
				total += memoBytesOf(info.Size())
			}
		}
		switch {
		case errors.Is(rerr, io.EOF):
			return total, nil
		case rerr != nil:
			return 0, rerr
		}
	}
}

// trimMemo holds the memo to its share of the budget (MemoShare) — the pages
// it keeps and those runs have staged for it together (memoExcess), so a page
// in flight takes its room from the memo's oldest answers rather than from a
// repository — by dropping its OLDEST COHORT whole, again until the memo is
// within its share, and reports whether it moved anything into trash/ for the
// janitor to empty. On a ticked pass it also drops the oldest while the node
// keeps more than maxMemoCohorts. A newest cohort that is also the oldest has
// the next opened first, so it can go. Before any repository is evicted
// (janitorPass).
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
		if oldest >= newest {
			c.openCohortPast(oldest)
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
	if err := toTrash(c.dir, dir, newV7()+"-memo-"+cohortName(n)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	cs, freed := withoutCohort(inc.derived[DerivedMemo].Cohorts, n)
	c.setMemo(inc, cs, true)
	return freed, true, nil
}

// stateMemos writes again the state of every copy whose memo's record moved
// since its state was last written (incarnation.memoVersion), so a boot counts
// the memo within a tick of its writes (Layout.Held) without walking it — on
// the janitor's ticked pass alone (janitorPass), so a copy's memo costs its
// disk at most one state's two syncs an EvictInterval. Only a copy with a
// state — one a round or a boot gave it — is written: a state is never made
// for a copy that has none, whose first full round writes it.
//
// A COPY IS HELD TO THAT RULE (memoStateDue) AS IT IS PICKED, AGAIN AS ITS
// WRITE BEGINS, AND ONCE MORE IF ITS WRITE FAILS. The writes are made one copy
// after another, two syncs each, holding nothing of the copy's — where every
// other writer of a state holds the repository's write lock (a round, a swap)
// or a pin (the maintainer, the backfill) — so meanwhile a purge, a rebuild's
// promote or a swap may retire a copy and move its directory into trash/
// under the write, the moment nobody holds it (Cache.trash), and a round may
// write its state. Not a pin for the write, deliberately: the cache reads a
// pin as a reader's — a boot's copy has its strays swept in place by its
// first full round only because nothing pins a copy before that round
// (sweep.go's packStrays), and pinned they would leave by a swap, a whole new
// incarnation; the status counts a pin a reader — and a record of the memo,
// a cache of a cache, is no reason to keep a retired copy's directory for two
// syncs nobody reads. So a copy no longer due is not written: a retired
// copy's state is read by nobody again, and one another writer wrote already
// records the memo. And a write that fails for a copy no longer due is NO
// FAILURE, as stampUse takes a copy moved into trash/ since it was chosen:
// nothing is lost and no tick picks the copy again, so nothing is logged,
// where code_cache_memo_unstated tells an operator the node tries again at its
// next tick. A copy moved under its write fails it on its absence (the rename
// into its directory, or the sync of the directory after), and retire marks a
// copy under e.mu before its directory moves, so that failure is always a
// retired copy's. A copy still due whose write fails is logged, and its next
// tick writes it.
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

// carryMemo moves the memo of from, an incarnation a rebuild replaces, into
// to, the incarnation replacing it, as to is promoted (rebuild.go's promote):
// ONE RENAME OF THE TREE, whatever it holds (SeedMove), its record with it —
// so the carry costs the same at ten answers or a million, and the round that
// holds the repository, which a request's repair waits on, pays one syscall
// for it rather than a stat and a link an answer. An answer is a function of
// object ids, which to holds as from did, and every read there holds an entry
// to its key and digest as it did here, so nothing is read on the way. A
// compaction's swap carries a memo the same way.
//
// Under from's memo lock and then to's, so no answer is placed into from's
// tree as it moves, nor counted for a tree no longer there; and from's memo is
// SHUT once its tree is gone — its record taken, its generation moved — so a
// batch begun on from places nothing (Place), none begins (Memo), and a
// reader still holding from misses. Nothing is carried from a memo whose log
// a round has marked gone (incarnation.historyGone), which that round would
// drop, nor from one shut (dropMemo), nor into an incarnation whose own round
// has dropped a memo (its generation moved): what it holds is then not the
// history from's answers were cut from. A tree that cannot be moved is logged
// and left in from, and goes with it: to starts with none.
//
// NO SYNC: a memo is a cache of a cache. A crash that loses the rename leaves
// the tree in from, the lower incarnation, which the boot moves aside; one
// that keeps it before to's state records it leaves a tree to's first full
// round finds (foundMemo) and the janitor counts.
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
	rec, listed := from.derived[DerivedMemo], from.memoListed
	c.setMemo(from, nil, false)
	from.memoListed = true
	from.memoGen++
	from.memoShut = true
	c.setMemo(to, rec.Cohorts, true)
	to.memoListed = listed
	e.mu.Unlock()
	c.log.Info("code_cache_memo_carried", "repo", e.id, "from", from.n, "to", to.n, "bytes", rec.Bytes)
}
