package cache

import (
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
)

// The node maps at most a bound of runs at once (package doc, "The index of
// pushes"): an idle mapping is kept for its copy's next reader only while the
// bound has room, unmapped let-go-longest-ago first past it, and a held one is
// never taken from under its readers. A reader finding the bound held whole is
// answered without the index (errMapsFull): an index decorates an answer. A
// code index's tip map and segments (codeindex.go) count against the same
// bound, since the sysctl it guards is one count of the process's; a search
// finding it full reads the tip with git grep.
//
// runMaps.mu is a LEAF: taken under an entry's mu, nothing taken under it. A
// mapping is unmapped only once gone (decided under the lock, so nobody holds
// or takes it again), and the munmap itself runs holding no lock.

// errMapsFull is a mapping the bound has no room for: readers hold every
// mapped run.
var errMapsFull = errors.New("cache: the node maps as many files of its indexes as it may, " +
	"and its readers hold every one")

// runMaps is the node's mapped runs, counted against its bound.
type runMaps struct {
	mu sync.Mutex
	// most is the bound: the runs the node may hold mapped at once.
	most int
	// mapped is the runs it holds mapped, or has reserved a mapping for:
	// held by a reader, idle, or being made.
	mapped int
	// idle is every mapping nobody holds, the most recently let go at its
	// head: what a mapping past the bound unmaps first, from its tail.
	idle idleRuns
}

// newRunMaps is a node's mapped runs, bounded at most.
func newRunMaps(most int) *runMaps { return &runMaps{most: max(most, 1)} }

// mappedRunsOf is the bound on mapped runs a process whose map count is
// count holds itself to.
func mappedRunsOf(count int) int { return max(count/MappedRunShare, 1) }

// reserve takes the room for one mapping about to be made, unmapping idle
// mappings let go longest ago past the bound, or answers errMapsFull. A
// reservation is given back by unreserve if no mapping is made of it, and by
// the mapping's unmap if one is.
func (rm *runMaps) reserve() error {
	var victims []*mappedRun
	rm.mu.Lock()
	for rm.mapped >= rm.most && rm.idle.tail != nil {
		m := rm.idle.tail
		rm.idle.remove(m)
		m.gone = true
		rm.mapped--
		victims = append(victims, m)
	}
	full := rm.mapped >= rm.most
	if !full {
		rm.mapped++
	}
	rm.mu.Unlock()
	for _, m := range victims {
		m.unmap()
	}
	if full {
		return fmt.Errorf("%w: %d of %d", errMapsFull, rm.most, rm.most)
	}
	return nil
}

// unreserve gives back a reservation no mapping was made of.
func (rm *runMaps) unreserve() {
	rm.mu.Lock()
	rm.mapped--
	rm.mu.Unlock()
}

// count is how many runs the node holds mapped now.
func (rm *runMaps) count() int {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	return rm.mapped
}

// hold takes one more hold of m for a reader, and answers false where m can
// be held no more: unmapped, or a run its index no longer names.
func (rm *runMaps) hold(m *mappedRun) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if m.gone || m.stale {
		return false
	}
	if m.holds == 0 && m.idling {
		rm.idle.remove(m)
	}
	m.holds++
	return true
}

// let gives one hold of m back. The last hold of a run its index still names
// leaves it idle for the next reader; of one it no longer names, let answers
// true and the caller unmaps it, holding no lock.
func (rm *runMaps) let(m *mappedRun) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	m.holds--
	switch {
	case m.holds > 0 || m.gone:
		return false
	case m.stale:
		m.gone = true
		rm.mapped--
		return true
	}
	rm.idle.push(m)
	return false
}

// forget marks m a run its index (or its incarnation) no longer names, so it
// is never held again, and answers true where nobody holds it now: the caller
// unmaps it. A held one is unmapped at its last release (let).
func (rm *runMaps) forget(m *mappedRun) bool {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	if m.gone {
		return false
	}
	m.stale = true
	if m.holds > 0 {
		return false
	}
	if m.idling {
		rm.idle.remove(m)
	}
	m.gone = true
	rm.mapped--
	return true
}

// drop unmaps m, a mapping made for one use and held by nobody else (a
// merge's input, or a reader's made needless by another reader's mapping of
// the same run), and gives back its room.
func (rm *runMaps) drop(m *mappedRun) {
	rm.mu.Lock()
	gone := m.gone
	m.gone = true
	if !gone {
		rm.mapped--
	}
	rm.mu.Unlock()
	if !gone {
		m.unmap()
	}
}

// idleRuns is the mappings nobody holds, in the order they were let go,
// linked through the mappings themselves so a let and a hold allocate
// nothing.
type idleRuns struct {
	head, tail *mappedRun
}

// push puts m at the head: the most recently let go.
func (l *idleRuns) push(m *mappedRun) {
	m.prev, m.next, m.idling = nil, l.head, true
	if l.head != nil {
		l.head.prev = m
	}
	l.head = m
	if l.tail == nil {
		l.tail = m
	}
}

// remove takes m out.
func (l *idleRuns) remove(m *mappedRun) {
	if m.prev != nil {
		m.prev.next = m.next
	} else {
		l.head = m.next
	}
	if m.next != nil {
		m.next.prev = m.prev
	} else {
		l.tail = m.prev
	}
	m.prev, m.next, m.idling = nil, nil, false
}

// mappedRun is one run mapped into this process's memory.
type mappedRun struct {
	n    uint64
	data []byte
	file runFile
	// Under runMaps.mu: holds counts its readers; stale, that its index (or
	// incarnation) no longer names it, so its last release unmaps it rather
	// than idling it; gone, that it is unmapped or about to be, so nobody
	// takes it again; prev, next and idling, its place among the idle.
	holds      int
	stale      bool
	gone       bool
	prev, next *mappedRun
	idling     bool
}

// unmap gives m's memory back; called once, by whoever decided it gone.
func (m *mappedRun) unmap() {
	if m.data != nil {
		_ = unix.Munmap(m.data) // what fails here is no mapping of ours, and nothing reads it again
		m.data, m.file = nil, runFile{}
	}
}
