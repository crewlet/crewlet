package stream

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MaxInFlightQueries bounds how many queries ONE SOCKET may have waiting at, or
// running past, the node's ceiling at once.
//
// # Four, and per socket
//
// FOUR is what one dashboard tab needs running at once, and it is the number
// internal/store's reader floor is sized from: eight, so that one full tab's
// four fit inside the socket surface's half of the pool (see [queryCeiling])
// and the engine's own reads keep the other four — the two numbers are one
// decision stated twice, so a change to either is a change to both. A larger
// allowance would not run a fifth query any sooner on a small host: it would
// park it at the node's ceiling or in `database/sql`'s wait for a connection
// rather than here, holding a goroutine against everybody else's queries
// instead of against this tab's.
//
// PER SOCKET, because a socket's questions are what its own goroutines run:
// four of them at most, whatever the frames say. Which PERSON gets the next
// free slot at the node's ceiling is not this number's question — the ceiling
// shares itself out between principals in turn ([queryGate]).
//
// It is NOT sized to a screen's burst. The shell keeps five reads standing on
// every page — the viewer, the Inbox count, My work's count, the projects and
// the pinned views — two asked the moment a tab opens and three the moment the
// viewer answers, so a screen's first reads queue behind them for the length
// of a tracker read, and after the first paint the five poll on independent
// 60 s to 5 min timers and rarely coincide. That queue is the design working:
// a burst waits in its own socket's backlog ([MaxQueuedQueries]), and the
// read loop goes on reading — a ping, a watch — while it does.
const MaxInFlightQueries = 4

// MaxQueuedQueries bounds how many of ONE SOCKET's questions may wait behind
// its [MaxInFlightQueries], in the order they arrived; a question past it is
// answered `unavailable` at once, with a [queryBacklogRetry] hint.
//
// # Why a backlog, and why not the read loop
//
// The bound used to be taken ON THE READ LOOP: a fifth question stopped the
// reader until one of the four finished. That was backpressure while the four
// ran on this socket's own reads; once the four wait at the node's ceiling,
// what they wait for is OTHER sockets' questions, so a tab's fifth frame held
// its ping and its watch behind somebody else's burst — the keepalive the read
// loop exists to answer. So the read loop never waits: a question beyond the
// four joins this backlog, and one of the socket's four goroutines takes it
// when its own question ends.
//
// # Thirty-two
//
// A backlog bounds what a client that sends faster than it is answered can
// make this socket hold, and nothing else: past it the client is told to come
// back. So it is sized to be out of reach of an honest tab — the shell's five
// standing reads and the busiest screen's ten (a work item's activity) at
// once, past the four running, is eleven waiting; thirty-two is that with room
// for every panel of a screen mounting together — and small next to what it
// costs, a frame's decoded request each.
const MaxQueuedQueries = 32

// queryBacklogRetry is what a question refused for a full backlog is told to
// wait before asking again: the backlog frees as soon as one of the socket's
// own questions is answered — a store read, milliseconds to a second — so the
// soonest a retry could be admitted is within a second, the smallest hint a
// frame's whole seconds can say.
const queryBacklogRetry = time.Second

// queryCeiling is how many queries EVERY SOCKET ON A NODE may have running at
// once, together, given how many connections ordinary reads may hold on its
// store (internal/store's DB.Readers): half of them, and never fewer than
// one.
//
// # Why there is a node-wide ceiling at all
//
// [MaxInFlightQueries] bounds one socket and nothing bounds how many sockets
// there are: any caller holding `state:read` can open as many as it likes, and
// N sockets at four queries each take every reader the store has. What queues
// behind them is the ENGINE's own reads — a seat's tool lookups mid-turn, the
// coverage probes, the /health body — in the same pool, first come first
// served. The store's reserved connection keeps exactly one read out of that
// queue, resolving who is acting, and nothing else; so without this ceiling
// one account and a script that opens sockets could starve every seat turn
// and health probe on a node.
//
// # Why half
//
// The socket surface is the one reader population an outside caller drives,
// and the engine's own reads are what run the company; neither may starve the
// other, and the store sizes its pool for both — its floor of eight is one
// full tab in the sockets' half and the same again for the engine's. Half
// moves with the host as the pool does (max(8, GOMAXPROCS)), because a larger
// node runs more seats whose reads need the other half.
//
// # Shared out between principals in turn
//
// The ceiling is one more thing a caller who opens sockets could take whole: a
// queue served first come first served puts every question of somebody's
// tenth tab ahead of a colleague's first. So it is a [queryGate], whose next
// free slot goes to the principal holding the FEWEST — see there.
//
// # Where a query waits for it
//
// In its own goroutine, with the socket's context, never on the read loop:
// the read loop answers the keepalive, and a ceiling some OTHER socket's burst
// is holding must not stop this one's pings.
func queryCeiling(readers int) int {
	return max(1, readers/2)
}

// queryGate is the node's ceiling on every socket's queries together, shared
// out between the PRINCIPALS asking. SAFE FOR CONCURRENT USE.
//
// # The next free slot goes to whoever holds the fewest
//
// Every question waits in its principal's own lane, in arrival order, and a
// slot that frees goes to the waiting principal holding the FEWEST slots — of
// those tied, the one granted a slot LONGEST AGO, a principal never granted
// one first. So however many sockets one
// principal opens and however many questions they queue, a second principal's
// question is admitted on the next slot that frees, rather than behind every
// question the first one queued: one person's tabs cannot keep the ceiling
// against anybody else's. It is the old per-principal budget's promise —
// "one person opening six tabs would have been a denial of service against
// everybody else's dashboard" — kept at the one place it is about: the
// ceiling every principal shares.
//
// # Work-conserving, deliberately
//
// A slot free while only one principal is asking goes to that principal's
// next question. The verify cap a sign-in takes turns at is the opposite
// (internal/iam/credential's turns), and for a reason that does not reach
// here: there a free slot handed to a source already holding one is a timing
// oracle about which names exist. A dashboard question discloses nothing by
// when it runs, and a slot held back from the one person asking is a read
// nobody runs.
//
// # Keyed on the principal's ID, never its login
//
// A login is a name, and a name is renamed. The id is what every row keys a
// principal on: never empty for a resolved one and never shared between two.
// A machine token acts as its owner, so a person's tabs and their scripts are
// one lane.
type queryGate struct {
	mu sync.Mutex

	// free is how many of the ceiling's slots are not held.
	free int

	// lanes are the principals with a slot held or a question waiting, and
	// nobody else: a principal is forgotten the moment it has neither, so
	// the map is bounded by the questions in flight rather than by every
	// principal that ever opened a tab.
	lanes map[uuid.UUID]*queryLane

	// ready are the lanes with a question waiting, in the order they became
	// so — the last tie-break, between principals never granted a slot.
	ready list.List // of *queryLane

	// grants counts every slot granted, so a lane can say how long ago it
	// was last served.
	grants uint64
}

// queryLane is one principal's questions at the ceiling.
type queryLane struct {
	who     uuid.UUID
	waiting list.List // of *queryWaiter, in arrival order
	running int
	queued  *list.Element // this lane's place in ready, or nil

	// granted is the gate's grant count when this lane was last granted a
	// slot, zero for never.
	granted uint64
}

// before reports whether l's principal is owed the next free slot ahead of
// o's: it holds fewer, or as many and was served longer ago. Ordered on the
// slot it was last GRANTED rather than on when it last joined the line,
// because a principal with questions queued never leaves the line — ordered on
// that, the principal whose slot just ended was ahead of one who arrived while
// it ran, and the newcomer waited a second slot.
func (l *queryLane) before(o *queryLane) bool {
	if l.running != o.running {
		return l.running < o.running
	}
	return l.granted < o.granted
}

// queryWaiter is one question waiting for a slot; granted is closed, under the
// gate's lock, when the slot is its.
type queryWaiter struct {
	granted chan struct{}
	el      *list.Element
}

func newQueryGate(slots int) *queryGate {
	return &queryGate{free: slots, lanes: map[uuid.UUID]*queryLane{}}
}

// take waits for a slot on who's behalf and answers what gives it back — a
// function its caller calls once, when the question is answered — or the
// context's error when the wait ended first, in which case the question has
// left its lane and nothing is held.
func (g *queryGate) take(ctx context.Context, who uuid.UUID) (func(), error) {
	g.mu.Lock()
	l := g.lanes[who]
	if l == nil {
		l = &queryLane{who: who}
		g.lanes[who] = l
	}
	w := &queryWaiter{granted: make(chan struct{})}
	w.el = l.waiting.PushBack(w)
	if l.queued == nil {
		l.queued = g.ready.PushBack(l)
	}
	g.dispatchLocked()
	g.mu.Unlock()

	select {
	case <-w.granted:
		return sync.OnceFunc(func() { g.end(l) }), nil
	case <-ctx.Done():
	}

	g.mu.Lock()
	select {
	case <-w.granted:
		// GRANTED AS THE SOCKET WENT AWAY: the slot is this question's,
		// and it is handed straight back rather than spent on nobody.
		g.mu.Unlock()
		g.end(l)
		return nil, ctx.Err()
	default:
	}
	l.waiting.Remove(w.el)
	if l.waiting.Len() == 0 && l.queued != nil {
		g.ready.Remove(l.queued)
		l.queued = nil
	}
	g.forgetLocked(l)
	g.mu.Unlock()
	return nil, ctx.Err()
}

// end gives back a slot l's principal held, and hands it on.
func (g *queryGate) end(l *queryLane) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.free++
	l.running--
	g.forgetLocked(l)
	g.dispatchLocked()
}

// forgetLocked drops l's lane once it holds nothing and waits for nothing.
// Held under the lock.
func (g *queryGate) forgetLocked(l *queryLane) {
	if l.running == 0 && l.waiting.Len() == 0 {
		delete(g.lanes, l.who)
	}
}

// dispatchLocked hands every free slot to the waiting principal owed it
// ([queryLane.before]), and takes a principal out of the line once nothing of
// theirs is waiting. Held under the lock.
//
// A WALK OF THE LINE PER GRANT, because the line is the principals with a
// question waiting at this instant — a handful on any node — and a heap keyed
// on two values that move with every grant and every end would be more code
// than the walk for nothing a node could measure.
func (g *queryGate) dispatchLocked() {
	for g.free > 0 && g.ready.Len() > 0 {
		owed := g.ready.Front()
		for e := owed.Next(); e != nil; e = e.Next() {
			if e.Value.(*queryLane).before(owed.Value.(*queryLane)) {
				owed = e
			}
		}
		l := owed.Value.(*queryLane)
		w := l.waiting.Remove(l.waiting.Front()).(*queryWaiter)
		if l.waiting.Len() == 0 {
			g.ready.Remove(owed)
			l.queued = nil
		}
		l.running++
		g.grants++
		l.granted = g.grants
		g.free--
		close(w.granted)
	}
}

// held is how many slots are held and how many principals have a lane, for the
// suite that asserts nothing is left behind.
func (g *queryGate) held() (slots, lanes int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	running := 0
	for _, l := range g.lanes {
		running += l.running
	}
	return running, len(g.lanes)
}

// socketQueries is one socket's questions: the ones its goroutines are
// running or waiting at the node's ceiling for, and the backlog behind them.
// SAFE FOR CONCURRENT USE — the read loop admits, the goroutines take.
type socketQueries struct {
	mu      sync.Mutex
	running int
	backlog []request
}

// admit takes one question: true and start when one of the socket's four
// goroutines is free to run it (the caller starts that goroutine), true alone
// when it joined the backlog for one of them to take, and false when the
// backlog is full.
func (q *socketQueries) admit(req request) (admitted, start bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case q.running < MaxInFlightQueries:
		q.running++
		return true, true
	case len(q.backlog) < MaxQueuedQueries:
		q.backlog = append(q.backlog, req)
		return true, false
	}
	return false, false
}

// next is the question a goroutine whose own question just ended runs next,
// or false when there is none — or the socket has gone, which drops the
// backlog with it — in which case the goroutine ends.
func (q *socketQueries) next(ctx context.Context) (request, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.backlog) == 0 || ctx.Err() != nil {
		q.backlog = nil
		q.running--
		return request{}, false
	}
	req := q.backlog[0]
	q.backlog = q.backlog[1:]
	return req, true
}
