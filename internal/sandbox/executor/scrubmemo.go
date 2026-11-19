package executor

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sync"
)

// THE MEMO OF TEXTS THE SHAPE PASS LEFT UNCHANGED, one a process: the package
// doc's "A text a take found unchanged is not redacted again" says what it
// keeps, why it is sound and what it costs.

// The memo's bounds, each a judgment call the take's log reads back
// (executor_session_scrub_memo: its hits, misses and evictions).
const (
	// scrubMemoEntries is the most texts the memo holds: about 250 bytes
	// each, so a mebibyte a process. A whole-PDF read is three texts
	// (its line read as text, and its two strings), so 1,365 reads.
	scrubMemoEntries = 4096
	// scrubMemoScopeEntries is the most one scope (an agent) holds, so a
	// session of hundreds of large texts, which a take inserts at once,
	// evicts its own and never every other seat's on the node: 21 whole-PDF
	// reads, more than a seat's session has been seen to hold.
	scrubMemoScopeEntries = 64
	// scrubMemoWays is the most texts one first chunk names: two PDFs of one
	// template, or a text and a later copy that grew, share a first chunk.
	scrubMemoWays = 4
)

// scrubMemo is the texts of at least [scrubPieceBaseBytes] this process's own
// full scrub found the shape pass leaves unchanged, each by its first chunk's
// digest under its scope ([memoKeyOf]) and kept as its length and the digest
// of its chunks' digests ([pieceWhole]): never the text. Least recently used
// first out, within [scrubMemoEntries], [scrubMemoScopeEntries] a scope and
// [scrubMemoWays] a first chunk. Safe for concurrent use; every method
// unlocks on its way out, so a panic the take contains leaves it usable.
type scrubMemo struct {
	mu sync.Mutex
	// byKey is the texts under each key, oldest kept first; all and each
	// scope's own list order them least recently used first.
	byKey   map[memoKey][]*memoEntry
	all     list.List
	scopes  map[string]*memoScope
	evicted uint64
}

// memoKey is a text's place in the memo: its scope and its first chunk's
// digest, hashed together.
type memoKey [sha256.Size]byte

// memoEntry is one text the memo holds.
type memoEntry struct {
	key            memoKey
	scope          *memoScope
	whole          pieceWhole
	inAll, inScope *list.Element
}

// memoScope is the texts one scope holds, least recently used first.
type memoScope struct {
	name    string
	entries list.List
}

// pieceWhole is a text as the memo knows it: its length, and the digest of
// its chunks' digests in order ([pieceDigests]).
type pieceWhole struct {
	length int64
	digest [sha256.Size]byte
}

// processScrubMemo is the memo every runner of this process scrubs with
// ([New]): a restart, a deploy or a seat's move to another node starts empty.
var processScrubMemo = newScrubMemo()

// newScrubMemo is an empty memo.
func newScrubMemo() *scrubMemo {
	return &scrubMemo{byKey: map[memoKey][]*memoEntry{}, scopes: map[string]*memoScope{}}
}

// memoKeyOf is the key of a text whose first chunk's digest is first, in
// scope: its length first, so no scope's name runs into the digest.
func memoKeyOf(scope string, first [sha256.Size]byte) memoKey {
	h := sha256.New()
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(scope)))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(scope))
	_, _ = h.Write(first[:])
	var k memoKey
	h.Sum(k[:0])
	return k
}

// candidates is every text the memo holds under key, copied out.
func (m *scrubMemo) candidates(key memoKey) []pieceWhole {
	m.mu.Lock()
	defer m.mu.Unlock()
	var found []pieceWhole
	for _, e := range m.byKey[key] {
		found = append(found, e.whole)
	}
	return found
}

// keep records whole under key in scope as most recently used, held already
// or not, dropping what a bound asks first.
func (m *scrubMemo) keep(scope string, key memoKey, whole pieceWhole) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := slices.IndexFunc(m.byKey[key], func(e *memoEntry) bool { return e.whole == whole }); i >= 0 {
		e := m.byKey[key][i]
		m.all.MoveToBack(e.inAll)
		e.scope.entries.MoveToBack(e.inScope)
		return
	}
	if bucket := m.byKey[key]; len(bucket) >= scrubMemoWays {
		m.evict(bucket[0])
	}
	if sc := m.scopes[scope]; sc != nil && sc.entries.Len() >= scrubMemoScopeEntries {
		m.evict(sc.entries.Front().Value.(*memoEntry))
	}
	if m.all.Len() >= scrubMemoEntries {
		m.evict(m.all.Front().Value.(*memoEntry))
	}
	sc := m.scopes[scope]
	if sc == nil {
		sc = &memoScope{name: scope}
		m.scopes[scope] = sc
	}
	e := &memoEntry{key: key, scope: sc, whole: whole}
	e.inAll = m.all.PushBack(e)
	e.inScope = sc.entries.PushBack(e)
	m.byKey[key] = append(m.byKey[key], e)
}

// evict drops e, and its scope where e was its last; m.mu is held.
func (m *scrubMemo) evict(e *memoEntry) {
	m.evicted++
	m.all.Remove(e.inAll)
	e.scope.entries.Remove(e.inScope)
	if e.scope.entries.Len() == 0 {
		delete(m.scopes, e.scope.name)
	}
	bucket := slices.DeleteFunc(m.byKey[e.key], func(o *memoEntry) bool { return o == e })
	if len(bucket) == 0 {
		delete(m.byKey, e.key)
		return
	}
	m.byKey[e.key] = bucket
}

// size is how many texts the memo holds, and how many it has evicted.
func (m *scrubMemo) size() (entries int, evicted uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.all.Len(), m.evicted
}
