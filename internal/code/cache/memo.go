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
	"strings"

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
// are only added to it, a rebuild holds the same rows' packs (and links the
// memo, SeedLink), and an object leaves a copy only with its history — a log
// restored or recreated under the node, another history, which the memo
// cannot tell entry by entry. So a history gone drops the memo whole
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
// places them together — each ending with what the run learned of it only at
// the run's end, a listing's total, a file's line count, a stat's counts
// (Place's tails) — only once its git has answered whole and reported no
// damage: an answer cut from a run that met damage is never memoized.
//
// # Its share of the budget, trimmed before any repository
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
// that takes it past its share: first every memo of a copy this process has not
// read or written since it opened — nobody asked of it since the boot — WHOLE,
// one rename each, the least recently used repository first; then entries, the
// least recently used first. Each incarnation's memo INDEX — every entry this
// process knows, its bytes and its last use — is made at the first read or
// write of that memo in this process; what a previous process left in it is
// LISTED by the janitor, behind the serving, never by a request (listMemos),
// each such entry counted as used before any this process used. The janitor
// writes a copy's state again once its memo's bytes have moved since it was
// last written, so a boot counts the memo within a tick of its writes
// (Layout.Held) without walking it.

// memoFile is this file as the memo's declared write names its writer
// (Writer.File): every placement, link and removal of an entry is made here.
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
	// memoMaxTail bounds what a run appends to a page it staged (Place): what
	// the run learned of the page only at its end — a listing's total, a
	// file's line count, a diff stat's counts of every file on the page —
	// which is never more than the page itself, an answer at its bound: a
	// file's counts are a few bytes against the hundred its entry of the page
	// takes at the least.
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

// parseMemoKey reads an entry's file name as its key — 64 lowercase hex
// digits, its one spelling — or false.
func parseMemoKey(name string) (MemoKey, bool) {
	var k MemoKey
	if len(name) != 2*len(k) {
		return k, false
	}
	if _, err := hex.Decode(k[:], []byte(name)); err != nil || k.String() != name {
		return MemoKey{}, false
	}
	return k, true
}

// memoBytesOf is the volume an entry of size bytes takes, as the memo counts
// it: its length rounded up to whole blocks (memoBlock).
func memoBytesOf(size int64) int64 { return (size + memoBlock - 1) / memoBlock * memoBlock }

// memoIndex is an incarnation's memo as this process knows it
// (incarnation.memo), under the entry's mu.
type memoIndex struct {
	// entries is every entry this process has placed, read or listed, each at
	// the volume it takes and its last use.
	entries map[MemoKey]memoEntry
	// listed: every entry on disk is in entries — the janitor listed the tree
	// (listMemo), or this process made it (Place) or linked it (seedMemo).
	// Until then the incarnation's record counts what its state did, and an
	// entry no write of this process placed is counted there.
	listed bool
}

// memoEntry is one entry of an index: its volume, and its last use on the
// cache's clock (Options.Now) in nanoseconds — zero for one no read of this
// process has used, which a listing found: older than any use this process
// made, so the first an entry trim takes.
type memoEntry struct {
	bytes int64
	used  int64
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

// MemoGet is the body memoized under k in the incarnation s holds — whole,
// its header naming k and the SHA-256 of what it holds — or false: no entry,
// or one that fails, which is removed (logged code_cache_memo_corrupt) so the
// read that misses writes it again. A copy with no memo misses with no
// syscall. A hit is a use of the entry, which the trims order by.
func (c *Cache) MemoGet(s Synced, k MemoKey) ([]byte, bool) {
	if !c.memoing() || c.handedOut(s, "a memo lookup") != nil {
		return nil, false
	}
	e, inc := s.entry, s.inc
	e.mu.Lock()
	_, recorded := inc.derived[DerivedMemo]
	if !recorded || inc.discarded {
		e.mu.Unlock()
		c.lookedUp(memoMiss)
		return nil, false
	}
	c.touchMemo(inc)
	e.mu.Unlock()

	path := filepath.Join(inc.dir, memoRel, k.String())
	body, bytes, info, err := readMemo(path, k)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		e.mu.Lock()
		c.memoGone(inc, k)
		e.mu.Unlock()
		c.lookedUp(memoMiss)
		return nil, false
	case err != nil:
		c.log.Warn("code_cache_memo_corrupt", "repo", e.id, "incarnation", inc.n, "entry", k.String(),
			"error", err)
		c.removeEntry(e, inc, k, path, info, bytes)
		c.lookedUp(memoCorrupt)
		return nil, false
	}
	e.mu.Lock()
	c.memoUsed(inc, k, bytes)
	e.mu.Unlock()
	c.lookedUp(memoHit)
	return body, true
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

// touchMemo is inc's memo index, made at its first read or write in this
// process — which makes the memo one the janitor no longer trims whole, and
// kicks it to list what a previous process left there. e.mu is held.
func (c *Cache) touchMemo(inc *incarnation) *memoIndex {
	if inc.memo == nil {
		inc.memo = &memoIndex{entries: map[MemoKey]memoEntry{}}
		c.kick()
	}
	return inc.memo
}

// foundMemo gives a tree on disk that no record names — a copy whose state
// named none, a previous process's — a record and an index for the janitor to
// count by listing it (recordDerived). e.mu is held.
func (c *Cache) foundMemo(inc *incarnation) {
	c.setDerived(inc, DerivedMemo, derivedState{Step: StepMemo}, true)
	c.touchMemo(inc)
}

// memoUsed records a hit of inc's entry k, of bytes, as a use. An entry the
// index did not hold is one a previous process left — counted in what the
// state recorded — or, in a listed memo, one placed by nothing this process
// knows, which is counted now. e.mu is held.
func (c *Cache) memoUsed(inc *incarnation, k MemoKey, bytes int64) {
	idx := inc.memo
	if idx == nil || inc.discarded {
		return // dropped while it was read
	}
	now := c.now().UnixNano()
	old, known := idx.entries[k]
	idx.entries[k] = memoEntry{bytes: bytes, used: now}
	switch {
	case known:
		c.addMemo(inc, bytes-old.bytes)
	case idx.listed:
		c.addMemo(inc, bytes)
	}
}

// memoGone forgets inc's entry k, found gone. e.mu is held.
func (c *Cache) memoGone(inc *incarnation, k MemoKey) {
	idx := inc.memo
	if idx == nil || inc.discarded {
		return
	}
	if old, known := idx.entries[k]; known {
		delete(idx.entries, k)
		c.addMemo(inc, -old.bytes)
	}
}

// addMemo moves inc's memo record by delta bytes, never below none. e.mu is
// held, and the record is there.
func (c *Cache) addMemo(inc *incarnation, delta int64) {
	s, ok := inc.derived[DerivedMemo]
	if !ok || delta == 0 {
		return
	}
	s.Bytes = max(s.Bytes+delta, 0)
	c.setDerived(inc, DerivedMemo, s, true)
}

// removeEntry removes inc's entry k at path — the file whose identity info
// is, and no other: one renamed over it since it was read is somebody's new
// answer — and forgets it, its volume bytes where the index did not hold it.
// Under the memo's lock, so no answer is placed at the name as it goes.
func (c *Cache) removeEntry(e *entry, inc *incarnation, k MemoKey, path string, info fs.FileInfo, bytes int64) {
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
		c.log.Warn("code_cache_memo_unremoved", "repo", e.id, "entry", k.String(), "error", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	idx := inc.memo
	if idx == nil || inc.discarded {
		return
	}
	if old, known := idx.entries[k]; known {
		delete(idx.entries, k)
		c.addMemo(inc, -old.bytes)
	} else if !idx.listed {
		c.addMemo(inc, -bytes)
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
// in tmp/ ("" once placed or given back), its body's length as put, its
// body's SHA-256 so far, which the run's tail completes, and the bytes its
// file holds — the header, the body and, once completed, the tail.
type memoStaged struct {
	key    MemoKey
	put    int
	path   string
	length int
	sum    hash.Hash
	size   int64
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
func (b *MemoBatch) Put(k MemoKey, body []byte) {
	if b == nil || b.done {
		return
	}
	put := b.puts
	b.puts++
	if len(body) > code.MaxGitAnswerBytes {
		b.fail(fmt.Errorf("an answer of %d bytes, past the %d one holds", len(body), code.MaxGitAnswerBytes))
		return
	}
	n := int64(memoHeader + len(body))
	if !b.c.stageMemo(n) {
		return
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
		return
	}
	_, err = f.Write(b.buf)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(f.Name()) // what it held is no answer
		b.c.memoStaged.Add(-n)
		b.fail(err)
		return
	}
	sum := sha256.New()
	sum.Write(body)
	b.staged = append(b.staged, memoStaged{key: k, put: put, path: f.Name(), length: len(body), sum: sum, size: n})
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
// renamed into its crewlet/memo, made for its first answer, under the memo's
// lock, where the incarnation still serves, the cache still runs, and the memo
// is neither shut nor dropped since the batch began (its generation). A tail
// past memoMaxTail leaves its answer unmemoized. Every staged file is given
// back either way. Once a batch.
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
		t := tail(b.staged[i].put)
		if len(t) > memoMaxTail {
			b.fail(fmt.Errorf("a tail of %d bytes, past the %d a run appends", len(t), memoMaxTail))
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
	known := map[MemoKey]bool{}
	listed := inc.memo != nil && inc.memo.listed
	if inc.memo != nil {
		for _, st := range b.staged {
			_, known[st.key] = inc.memo.entries[st.key]
		}
	}
	e.mu.Unlock()
	if !serving {
		return
	}
	created, err := c.makeMemoTree(inc)
	if err != nil {
		b.fail(err)
		return
	}
	type placed struct {
		key MemoKey
		// bytes is the volume it takes, and prior the volume of what it was
		// renamed over that the index did not know — -1 for nothing there.
		bytes, prior int64
	}
	var out []placed
	dir := filepath.Join(inc.dir, memoRel)
	for i := range b.staged {
		st := &b.staged[i]
		if st.path == "" {
			continue
		}
		target := filepath.Join(dir, st.key.String())
		prior := int64(-1)
		// AN ENTRY THIS PROCESS DOES NOT KNOW, in a memo it has not listed,
		// may be a previous process's, which the state's bytes count: the
		// rename replaces those bytes, never adds beside them.
		if !created && !listed && !known[st.key] {
			if info, lerr := os.Lstat(target); lerr == nil {
				prior = memoBytesOf(info.Size())
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
		out = append(out, placed{key: st.key, bytes: memoBytesOf(st.size), prior: prior})
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.discarded {
		return // its bytes left the total with it
	}
	idx := c.touchMemo(inc)
	total := inc.derived[DerivedMemo].Bytes
	if created {
		// A TREE THIS PLACE MADE holds exactly what it placed: whatever a
		// record or an index said of one before is gone with it.
		idx = &memoIndex{entries: map[MemoKey]memoEntry{}, listed: true}
		inc.memo, total = idx, 0
	}
	now := c.now().UnixNano()
	for _, p := range out {
		old, ok := idx.entries[p.key]
		switch {
		case ok:
			total += p.bytes - old.bytes
		case !idx.listed && p.prior >= 0:
			total += p.bytes - p.prior
		default:
			total += p.bytes
		}
		idx.entries[p.key] = memoEntry{bytes: p.bytes, used: now}
	}
	c.setDerived(inc, DerivedMemo, derivedState{Bytes: max(total, 0), Step: StepMemo}, true)
	// PAST ITS SHARE, the memo's trim is due at once rather than at the
	// janitor's tick: no eviction is for the excess (overBudget), so nothing
	// else would wake the janitor for it.
	if c.memoExcess() > 0 {
		c.kick()
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

// makeMemoTree makes inc's memo tree where it is not there, and reports
// whether it made it; a tree's name held by something that is no directory is
// an error, which the next full round's sweep moves aside (image.imaged). Under
// the memo's lock.
func (c *Cache) makeMemoTree(inc *incarnation) (bool, error) {
	if err := os.Mkdir(filepath.Join(inc.dir, engineRoot), hostbox.DirMode); err != nil &&
		!errors.Is(err, fs.ErrExist) {
		return false, err
	}
	dir := filepath.Join(inc.dir, memoRel)
	err := os.Mkdir(dir, hostbox.DirMode)
	switch {
	case err == nil:
		return true, nil
	case !errors.Is(err, fs.ErrExist):
		return false, err
	}
	if info, lerr := os.Lstat(dir); lerr != nil || !info.IsDir() {
		return false, fmt.Errorf("cache: %s is no directory", dir)
	}
	return false, nil
}

// dropMemo moves inc's memo tree into trash/ whole and forgets what inc
// records of it, its index with it — under the memo's lock, so no answer is
// placed into the tree as it goes and counted for a directory no longer there:
// the one removal of a tree (dropFor). A memo with no tree on disk is
// forgotten the same. AND IT SHUTS THE MEMO: its generation moves, so no batch
// begun before the drop places anything, and none is begun until the round
// that dropped it reopens it (reopenMemo) — the memo drops only where the
// objects of the copy change under it, and an answer a read cuts until that
// change is visible may be read off what it replaces.
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
	c.setDerived(inc, DerivedMemo, derivedState{}, false)
	inc.memo = nil
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

// listMemos lists, behind the serving, every memo this process has touched
// and not yet listed (memoIndex.listed): each entry a previous process left in
// it is tracked as used before any this process used — the first the entry
// trim takes — and the record's bytes made exactly those of its entries,
// whatever the state said. A request never lists a memo: a tree a previous
// process filled may hold hundreds of thousands of entries.
func (c *Cache) listMemos(ctx context.Context) {
	type due struct {
		e   *entry
		inc *incarnation
		idx *memoIndex
	}
	var dues []due
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.memo != nil && !inc.memo.listed && !inc.discarded {
			dues = append(dues, due{e: e, inc: inc, idx: inc.memo})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	for _, d := range dues {
		if ctx.Err() != nil || c.life.Err() != nil {
			return
		}
		c.listMemo(d.e, d.inc, d.idx)
	}
}

// listMemo lists inc's memo tree into idx, the index it had when it was
// chosen: an index dropped or made anew since takes nothing. An entry no key
// names, or that is no regular file, is nothing a read finds and nothing of
// the memo's, and is moved into trash/.
func (c *Cache) listMemo(e *entry, inc *incarnation, idx *memoIndex) {
	dir := filepath.Join(inc.dir, memoRel)
	list, err := os.ReadDir(dir)
	gone := errors.Is(err, fs.ErrNotExist)
	if err != nil && !gone {
		c.log.Warn("code_cache_memo_unlisted", "repo", e.id, "incarnation", inc.n, "error", err)
		return
	}
	found := make(map[MemoKey]int64, len(list))
	for _, d := range list {
		k, ok := parseMemoKey(d.Name())
		if !ok || !d.Type().IsRegular() {
			path := filepath.Join(dir, d.Name())
			if terr := toTrash(c.dir, path, newV7()+"-"+d.Name()); terr != nil && !errors.Is(terr, fs.ErrNotExist) {
				c.log.Warn("code_cache_memo_unlisted", "repo", e.id, "path", path, "error", terr)
			}
			continue
		}
		info, ierr := d.Info()
		if ierr != nil {
			continue // gone since the listing
		}
		found[k] = memoBytesOf(info.Size())
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if inc.memo != idx || inc.discarded {
		return
	}
	// WHAT THIS PROCESS PLACED IS KEPT AS IT KNOWS IT, the listing's view of
	// it older: an answer placed after the listing read the tree is tracked,
	// and in it, whatever the listing saw.
	for k, n := range found {
		if _, known := idx.entries[k]; !known {
			idx.entries[k] = memoEntry{bytes: n}
		}
	}
	idx.listed = true
	var total int64
	for _, en := range idx.entries {
		total += en.bytes
	}
	if gone && len(idx.entries) == 0 {
		c.setDerived(inc, DerivedMemo, derivedState{}, false)
		return
	}
	c.setDerived(inc, DerivedMemo, derivedState{Bytes: total, Step: StepMemo}, true)
}

// trimMemo holds the memo to its share of the budget (MemoShare) — the pages
// it keeps and those runs have staged for it together (memoExcess), so a page
// in flight takes its room from the memo's oldest answers rather than from a
// repository — and reports whether it moved anything into trash/ for the
// janitor to empty: first every memo of a copy this process has not touched
// since it opened, WHOLE, the least recently used repository first — one
// rename each, and nothing of theirs was read since the boot — then the
// entries of the rest, the least recently used first. Before any repository
// is evicted (janitorPass).
func (c *Cache) trimMemo(ctx context.Context) bool {
	if c.memoExcess() == 0 {
		return false
	}
	moved := c.trimColdMemos(ctx)
	if c.memoExcess() > 0 {
		c.trimMemoEntries(ctx)
	}
	return moved
}

// trimColdMemos moves into trash/ whole, the least recently used repository
// first, the memo of every copy this process has not touched, while the memo
// holds more than its share. It reports whether it moved any.
func (c *Cache) trimColdMemos(ctx context.Context) bool {
	type cold struct {
		e        *entry
		inc      *incarnation
		lastUsed int64
	}
	var colds []cold
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.memo == nil && !inc.discarded &&
			inc.derived[DerivedMemo].Bytes > 0 {
			colds = append(colds, cold{e: e, inc: inc, lastUsed: inc.lastUsed.UnixNano()})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	slices.SortFunc(colds, func(a, b cold) int {
		return cmp.Or(cmp.Compare(a.lastUsed, b.lastUsed), strings.Compare(a.e.id, b.e.id))
	})
	moved := false
	for _, cl := range colds {
		if c.memoExcess() == 0 || ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		moved = c.trimColdMemo(cl.e, cl.inc) || moved
	}
	return moved
}

// trimColdMemo moves inc's memo into trash/ whole where it is still a memo
// this process has not touched, and reports whether it did.
func (c *Cache) trimColdMemo(e *entry, inc *incarnation) bool {
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	e.mu.Lock()
	bytes := inc.derived[DerivedMemo].Bytes
	cold := inc.memo == nil && !inc.discarded && e.current == inc
	e.mu.Unlock()
	if !cold {
		return false
	}
	path := filepath.Join(inc.dir, memoRel)
	err := toTrash(c.dir, path, newV7()+"-"+filepath.Base(path))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.log.Warn("code_cache_memo_untrimmed", "repo", e.id, "path", path, "error", err)
		return false
	}
	e.mu.Lock()
	c.setDerived(inc, DerivedMemo, derivedState{}, false)
	e.mu.Unlock()
	c.log.Info("code_cache_memo_trimmed", "repo", e.id, "incarnation", inc.n, "bytes", bytes, "whole", true)
	return err == nil
}

// trimMemoEntries removes the least recently used entries of every memo this
// process has touched until the memo holds no more than its share. TWO
// PASSES, so a trim of a memo of millions of entries holds no copy of their
// keys: the first reads each entry's last use and volume, and finds the last
// use at and before which enough entries lie (cut); the second takes, a copy
// at a time, the entries at or before it.
func (c *Cache) trimMemoEntries(ctx context.Context) {
	type use struct{ used, bytes int64 }
	var uses []use
	var held []*entry
	c.mu.Lock()
	for _, e := range c.entries {
		held = append(held, e)
	}
	c.mu.Unlock()
	for _, e := range held {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.memo != nil && !inc.discarded {
			for _, en := range inc.memo.entries {
				uses = append(uses, use{used: en.used, bytes: en.bytes})
			}
		}
		e.mu.Unlock()
	}
	slices.SortFunc(uses, func(a, b use) int { return cmp.Compare(a.used, b.used) })
	excess := c.memoExcess()
	var cut int64
	var covered int64
	for _, u := range uses {
		if covered >= excess {
			break
		}
		covered += u.bytes
		cut = u.used
	}
	if covered == 0 {
		return
	}
	type victim struct {
		k    MemoKey
		used int64
	}
	removed, bytes := 0, int64(0)
	for _, e := range held {
		if c.memoExcess() == 0 || ctx.Err() != nil || c.life.Err() != nil {
			break
		}
		e.mu.Lock()
		inc := e.current
		var victims []victim
		if inc != nil && inc.memo != nil && !inc.discarded {
			for k, en := range inc.memo.entries {
				if en.used <= cut {
					victims = append(victims, victim{k: k, used: en.used})
				}
			}
		}
		e.mu.Unlock()
		slices.SortFunc(victims, func(a, b victim) int { return cmp.Compare(a.used, b.used) })
		for _, v := range victims {
			if c.memoExcess() == 0 {
				break
			}
			if n, ok := c.trimEntry(e, inc, v.k, v.used); ok {
				removed++
				bytes += n
			}
		}
	}
	if removed > 0 {
		c.log.Debug("code_cache_memo_trimmed", "entries", removed, "bytes", bytes, "whole", false)
	}
}

// trimEntry removes inc's entry k where its last use is still used — one read
// since it was chosen is no longer the least recently used — and answers the
// volume it took.
func (c *Cache) trimEntry(e *entry, inc *incarnation, k MemoKey, used int64) (int64, bool) {
	inc.memoMu.Lock()
	defer inc.memoMu.Unlock()
	e.mu.Lock()
	still := false
	if idx := inc.memo; idx != nil && !inc.discarded && e.current == inc {
		en, ok := idx.entries[k]
		still = ok && en.used == used
	}
	e.mu.Unlock()
	if !still {
		return 0, false
	}
	err := os.Remove(filepath.Join(inc.dir, memoRel, k.String()))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		c.log.Warn("code_cache_memo_untrimmed", "repo", e.id, "entry", k.String(), "error", err)
		return 0, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if idx := inc.memo; idx != nil && !inc.discarded {
		if en, ok := idx.entries[k]; ok {
			delete(idx.entries, k)
			c.addMemo(inc, -en.bytes)
			return en.bytes, true
		}
	}
	return 0, false
}

// stateMemos writes again the state of every copy whose memo's bytes moved
// since its state was last written, so a boot counts the memo within a tick
// of its writes (Layout.Held) without walking it. Only a copy with a state —
// one a round or a boot gave it — is written: a state is never made for a copy
// that has none, whose first full round writes it.
func (c *Cache) stateMemos(ctx context.Context) {
	type due struct {
		e   *entry
		inc *incarnation
	}
	var dues []due
	c.mu.Lock()
	for _, e := range c.entries {
		e.mu.Lock()
		if inc := e.current; inc != nil && inc.state != nil && !inc.retired && !inc.discarded &&
			inc.derived[DerivedMemo].Bytes != inc.memoStated {
			dues = append(dues, due{e: e, inc: inc})
		}
		e.mu.Unlock()
	}
	c.mu.Unlock()
	for _, d := range dues {
		if ctx.Err() != nil || c.life.Err() != nil {
			return
		}
		if err := c.persist(ctx, d.e, d.inc, false); err != nil {
			c.log.Warn("code_cache_memo_unstated", "repo", d.e.id, "incarnation", d.inc.n, "error", err)
		}
	}
}

// seedMemo is a rebuild's seeding of its memo (seedRebuild): every entry of
// the memo of the incarnation it replaces hard-linked into the new one
// (SeedLink), each with the last use the old index knew — an answer is a
// function of object ids, which the new incarnation holds as the old did, and
// every read there holds an entry to its key and digest as it did here, so
// nothing is hashed on the way. Nothing for a memo whose log a round has
// marked gone (incarnation.historyGone), which that round would drop, nor for
// one shut (dropMemo). An entry that cannot be linked is left behind, logged
// once; the rebuild goes on.
func (c *Cache) seedMemo(e *entry, replaced, inc *incarnation) {
	if d := c.image.decl(DerivedMemo); d == nil || d.Seed != SeedLink {
		return
	}
	e.mu.Lock()
	_, recorded := replaced.derived[DerivedMemo]
	gone := replaced.historyGone || replaced.memoShut
	uses := map[MemoKey]int64{}
	if replaced.memo != nil {
		for k, en := range replaced.memo.entries {
			uses[k] = en.used
		}
	}
	e.mu.Unlock()
	if !recorded || gone {
		return
	}
	from := filepath.Join(replaced.dir, memoRel)
	list, err := os.ReadDir(from)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", memoRel, "error", err)
		}
		return
	}
	if _, err := c.makeMemoTree(inc); err != nil {
		c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", memoRel, "error", err)
		return
	}
	into := filepath.Join(inc.dir, memoRel)
	entries := make(map[MemoKey]memoEntry, len(list))
	logged := false
	for _, d := range list {
		k, ok := parseMemoKey(d.Name())
		if !ok || !d.Type().IsRegular() {
			continue
		}
		info, ierr := d.Info()
		if ierr == nil {
			ierr = os.Link(filepath.Join(from, d.Name()), filepath.Join(into, d.Name()))
		}
		if ierr != nil {
			if !logged && !errors.Is(ierr, fs.ErrNotExist) {
				logged = true
				c.log.Warn("code_cache_rebuild_seed_failed", "repo", e.id, "file", filepath.Join(memoRel, d.Name()),
					"error", ierr)
			}
			continue
		}
		entries[k] = memoEntry{bytes: memoBytesOf(info.Size()), used: uses[k]}
	}
	var total int64
	for _, en := range entries {
		total += en.bytes
	}
	e.mu.Lock()
	inc.memo = &memoIndex{entries: entries, listed: true}
	c.setDerived(inc, DerivedMemo, derivedState{Bytes: total, Step: StepMemo}, true)
	e.mu.Unlock()
	c.log.Info("code_cache_memo_seeded", "repo", e.id, "incarnation", inc.n, "entries", len(entries))
}
