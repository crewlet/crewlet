package cache

import (
	"container/heap"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE BACKFILL'S SCHEDULE: which copies have a step of the backfill due, and
// in what order the loop takes them (backfill.go), KEPT AS THE STEPS FALL DUE
// rather than found by a walk. Picked by a walk of every copy the cache holds,
// under the cache-wide lock every request takes to find its copy, each step
// cost every copy — its runs' paths built, its merge rule judged twice — and
// steps come with every push: each fold kicks the backfill, and a merge goes
// on to the next step at once. Measured at 10 000 copies (code.MaxRepos), one
// walk took 56 to 127 ms and a request waited up to 280 ms behind walks run
// back to back; after a restart every copy's first round opens a gap, so the
// warm-up was quadratic in the copies.
//
// So every change that can make a copy's work due — or no longer due — MARKS
// IT (backfillDirty): a change to its index's record, which every change goes
// through (setRecord), and its files' drop (dropFor); its runs' vouches as a
// check, a scrub or a reader's mapping changes them; its history wanted; the
// copy converged, read whole, made current, retired or rebuilt; and each step
// the backfill takes of it. The next pick looks again at the marked copies
// alone (backfillDue), each under its own lock, and places each in ONE queue:
// a due queue of the kind of step it has first due, the most recently used
// on top — every use of a queued copy moves it (touch) — or the later queue,
// soonest on top, keyed by when its next step falls due by time alone (its
// horizon). A pick then takes the top of the first due queue that has one,
// O(log n) in the copies, holding the schedule's lock alone, and looks once
// more at that copy under its lock before it pins it: the look is the
// authority, the queues only an order, so a copy whose look says otherwise is
// placed again and the pick goes on.
//
// crewlet.code.cache.provenance.behind is the schedule's own count of the
// copies it has placed as behind, moved as each is placed.
//
// LOCK ORDER: the schedule's mu is a LEAF — taken under an entry's mu (a mark,
// a touch), and nothing is taken under it — and pick serialises picks, taken
// before any entry's mu, so a placement made from one look never lands over
// the placement made from a later one.

// schedule is a cache's backfill schedule.
type schedule struct {
	// pick is held by a pick, from its placings through its pin.
	pick sync.Mutex

	mu sync.Mutex
	// due is, by kind of step — workHorizon the last — every copy whose
	// first step due is that kind, most recently used on top; later every
	// copy with no step due now whose next falls due by time, soonest on
	// top.
	due   [workHorizon]dueQueue
	later laterQueue
	// dirty is every copy marked since it was last placed, and spare the
	// buffer the next marks go into while a pick places these.
	dirty, spare []*entry
	// behind is how many copies are placed as behind: a gap, or a history
	// wanted and not complete.
	behind int
}

// slot is a copy's place in the schedule, under the schedule's mu.
type slot struct {
	// kind is the due queue it is in, and used its key there; when, its key
	// in later; at its index in whichever it is in, -1 for neither.
	kind backfillWork
	used time.Time
	when time.Time
	at   int
	// dirty: on the schedule's dirty list. behind: counted in its behind.
	dirty, behind bool
}

// backfillDirty marks e: its backfill work may have changed, so the next pick
// looks at it again, and the backfill is woken for it. Taken under e.mu or
// not, and never under the schedule's mu.
func (c *Cache) backfillDirty(e *entry) {
	s := &c.sched
	s.mu.Lock()
	if !e.bf.dirty {
		e.bf.dirty = true
		s.dirty = append(s.dirty, e)
	}
	s.mu.Unlock()
	c.kickBackfill()
}

// touch moves e in its due queue to its new last use; e.mu is held, and e is
// queued (entry.bfQueued). A copy used while its backfill is due is the one
// people are asking about.
func (c *Cache) touch(e *entry, used time.Time) {
	s := &c.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.bf.kind != 0 && e.bf.at >= 0 {
		e.bf.used = used
		heap.Fix(&s.due[e.bf.kind-1], e.bf.at)
	}
}

// placeDue looks again at every copy marked since the last pick, and at every
// copy whose next step's time has come, and places each as its look says;
// then it sets crewlet.code.cache.provenance.behind. Held under s.pick.
func (c *Cache) placeDue(now time.Time) {
	s := &c.sched
	s.mu.Lock()
	batch := s.dirty
	s.dirty, s.spare = s.spare[:0], nil
	for _, e := range batch {
		e.bf.dirty = false
	}
	for len(s.later) > 0 && !s.later[0].bf.when.After(now) {
		e := heap.Pop(&s.later).(*entry)
		e.bf.when = time.Time{}
		batch = append(batch, e)
	}
	s.mu.Unlock()
	for _, e := range batch {
		// Placed under the lock it was looked at under, so no use of the
		// copy falls between the look and the queue it keys.
		e.mu.Lock()
		d := c.backfillDue(e, now)
		s.mu.Lock()
		s.placeLooked(e, d)
		s.mu.Unlock()
		e.mu.Unlock()
	}
	s.mu.Lock()
	clear(batch)
	if s.spare == nil {
		s.spare = batch[:0]
	}
	behind := s.behind
	s.mu.Unlock()
	if c.metrics != nil {
		c.metrics.Set(metrics.CodeCacheProvenanceBehind, float64(behind), nil)
	}
}

// placeLooked places e as d, a look at it, says — unless e was marked again
// since that look, when it leaves every queue for the next pick to look at
// it again: a placement from a look older than the mark is no placement.
// s.mu is held.
func (s *schedule) placeLooked(e *entry, d dueWork) {
	if e.bf.dirty {
		s.remove(e)
		return
	}
	s.place(e, d)
}

// place puts e where d says: the due queue of its first step, the later
// queue, or neither; s.mu is held.
func (s *schedule) place(e *entry, d dueWork) {
	s.remove(e)
	switch {
	case d.behind && !e.bf.behind:
		s.behind++
	case !d.behind && e.bf.behind:
		s.behind--
	}
	e.bf.behind = d.behind
	switch {
	case d.work != 0:
		e.bf.kind, e.bf.used = d.work, d.used
		heap.Push(&s.due[d.work-1], e)
		e.bfQueued.Store(true)
	case !d.when.IsZero():
		e.bf.when = d.when
		heap.Push(&s.later, e)
	}
}

// remove takes e out of whichever queue it is in; s.mu is held.
func (s *schedule) remove(e *entry) {
	switch {
	case e.bf.at < 0:
	case e.bf.kind != 0:
		heap.Remove(&s.due[e.bf.kind-1], e.bf.at)
	default:
		heap.Remove(&s.later, e.bf.at)
	}
	e.bf.kind, e.bf.used, e.bf.when, e.bf.at = 0, time.Time{}, time.Time{}, -1
	e.bfQueued.Store(false)
}

// top is the copy the next step is taken of — the top of the first due queue
// that holds one — and its kind of step; or nil, and when the soonest copy of
// the later queue falls due (zero for none). s.mu is held.
func (s *schedule) top() (*entry, backfillWork, time.Time) {
	for k := range s.due {
		if len(s.due[k]) > 0 {
			e := s.due[k][0]
			return e, e.bf.kind, time.Time{}
		}
	}
	if len(s.later) > 0 {
		return nil, 0, s.later[0].bf.when
	}
	return nil, 0, time.Time{}
}

// dueQueue is the copies with one kind of step due, most recently used first,
// a copy's id breaking a tie: a heap.Interface keeping each copy's index in
// its slot.
type dueQueue []*entry

func (q dueQueue) Len() int { return len(q) }

func (q dueQueue) Less(i, j int) bool {
	a, b := q[i], q[j]
	if !a.bf.used.Equal(b.bf.used) {
		return a.bf.used.After(b.bf.used)
	}
	return a.id < b.id
}

func (q dueQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].bf.at, q[j].bf.at = i, j
}

func (q *dueQueue) Push(x any) {
	e := x.(*entry)
	e.bf.at = len(*q)
	*q = append(*q, e)
}

func (q *dueQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	e.bf.at = -1
	return e
}

// laterQueue is the copies whose next step falls due by time, soonest first,
// a copy's id breaking a tie.
type laterQueue []*entry

func (q laterQueue) Len() int { return len(q) }

func (q laterQueue) Less(i, j int) bool {
	a, b := q[i], q[j]
	if !a.bf.when.Equal(b.bf.when) {
		return a.bf.when.Before(b.bf.when)
	}
	return a.id < b.id
}

func (q laterQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].bf.at, q[j].bf.at = i, j
}

func (q *laterQueue) Push(x any) {
	e := x.(*entry)
	e.bf.at = len(*q)
	*q = append(*q, e)
}

func (q *laterQueue) Pop() any {
	old := *q
	e := old[len(old)-1]
	old[len(old)-1] = nil
	*q = old[:len(old)-1]
	e.bf.at = -1
	return e
}
