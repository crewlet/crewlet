package kv

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// How a token counter is written.
//
// THE HOTTEST KEY THE FLEET HAS. The company's counter is written by every
// round of every seat the company runs, by every auxiliary call and by every
// collected executor launch, and a seat's by each of its own rounds, its
// workers' and its executor's launches. So the number of writers racing one
// record is the number of rounds in flight across the company, which grows
// with the company and which nothing in the engine bounds. Two rules follow,
// one for each kind of contender: a node's own writers are combined, and a
// race lost to another node is retried by the fleet store's rule for every
// race (race.go), held to each write.
//
// # A node's own writers never race each other
//
// A node's concurrent writes to one counter are COMBINED. They queue on the
// key's lane ([counterLane]), and whichever caller holds the lane's turn sends
// every write queued there as ONE compare-and-swap: each change is applied in
// the order it queued, to the counter as the changes before it left it, and
// each caller is answered what its own change made of it — exactly what
// sending the writes one after another would have decided, admitted and
// refused alike, so a ceiling still admits what it holds and nothing more. A
// node therefore has at most one write to a key in flight, and the racers at
// the broker are the fleet's NODES rather than its rounds.
//
// Racing them instead was the failure this replaced. N writers on one key, each
// a read and a write per attempt, make one winner a round and N-1 losers that
// read and write again: the broker serves on the order of N² requests to land
// N — 64 charges from one node lost 585 races between them on an embedded
// broker — and every round grows with N. Measured under -race through a leaf
// link, 32 concurrent charges from one node took up to a second on two idle
// CPUs and failed in 12 runs of 15 on one busy CPU; combined, the slowest took
// 25 ms on the idle CPUs and 102 ms on the busy one, and none failed in 45 runs.
//
// THE TURN IS HANDED ON, IN ORDER. A caller that finds nobody sending takes
// the turn; one that is done with it hands it to the caller of the write
// queued first ([counterLane.handOn]), never to whichever waiter the runtime
// wakes. So the order a batch is sent in is the order its writes queued, and
// every write still unanswered belongs to a caller that either holds the turn
// or is waiting to be handed it — which is what lets a holder leave early
// (below) without stranding a write nobody will send.
//
// # A race lost to another node is retried for a time, per write
//
// Between nodes a write still races, and a lost race is retried by the fleet
// store's rule (race.go): after a jittered wait ([raceRetryDelay]), for as long
// as the write's PATIENCE allows ([FleetStore.patience]), never for a number of
// attempts. The sixteen attempts this replaced reported a healthy, busy
// counter as an outage once enough writes overlapped one: 64 charges from eight
// nodes, eight each, failed in 18 runs of 30 on idle CPUs and in every run on a
// busy one, where nearly half of them failed. Combined and bounded by the
// patience, none failed in 45 runs, and the slowest was answered in 1.3
// seconds. And the old answer was not harmless. A charge that fails stops its
// round ([coord.Budgets.Charge] fails closed), and when the write that failed
// is the company's, the round the vendor has already billed is recorded
// nowhere: the company's counter is short of it for the rest of every window,
// which is the fail-OPEN direction for every seat judged against it. A round's
// charge runs on a context that outlives its caller, so the patience is also
// all that bounds how long it waits on a counter that keeps answering; running
// out of it is [coord.ErrUnavailable], never a refusal.
//
// IT IS EACH WRITE'S, AND IT DECIDES ONLY WHETHER TO START AN ATTEMPT. A
// write's patience runs from its first attempt, and a write that has run out
// of it leaves its batch at the next attempt boundary — where every attempt it
// was in has COMPLETED and lost, so nothing of it has landed and it is told so
// ([outwaited]). An attempt already sent is never cut short by it: every
// request goes out on a context nothing cancels and no deadline bounds, so the
// client bounds it by its own timeout, exactly as it bounds a request made
// for one caller, and its outcome is the answer. A deadline over the whole
// batch would cut the request in flight when it fell, and a compare-and-swap
// the broker applied but had not yet acknowledged would be answered as one
// that never landed — the company counting a round whose seat did not, and
// the error saying neither had.
//
// # What a caller waits for, and what hanging up does
//
// A write queued behind a batch in flight waits for that batch and then for its
// own, so a caller is answered within twice the patience, plus the attempt each
// batch has in flight when its patience runs out. A caller that hangs up while
// its write is queued is told so at once, and its write is withdrawn. One that
// hangs up while a batch holds its write is told so at that batch's next
// attempt boundary: every attempt before it lost, so nothing of the write has
// landed, and it is withdrawn there. Or the attempt in flight lands, and the
// caller is told what happened — a write that landed is spend recorded whoever
// is still listening, and the caller is the one that has to know it.
//
// AND A HOLDER THAT HANGS UP STOPS SENDING. The caller holding the lane's turn
// sends its batch on its own goroutine, so when ITS write leaves the batch it
// gives the rest back to the front of the queue and hands the turn on, rather
// than racing on for writes whose callers are still waiting to be answered
// and are just as able to send them. Otherwise a caller that hung up was held,
// by another node's contention, for the rest of somebody else's patience.

// counterLane is this node's queue of writes to one counter key, and the right
// to send them.
type counterLane struct {
	mu sync.Mutex

	// queue holds the writes no batch has taken, in the order they are to
	// be sent: a batch that gives writes back puts them at the front.
	queue []*counterWrite

	// sending is whether a caller holds the lane's turn or has been handed
	// it. While the queue holds a write, one does.
	sending bool

	// members counts the callers using the lane, under the store's
	// lanesMu: the lane is dropped when the last one leaves, so a seat that
	// stops running leaves nothing behind on the node.
	members int
}

// counterWrite is one caller's change to a counter, and the answer to it.
type counterWrite struct {
	// hungUp is the caller's context's Err: a write whose caller has hung
	// up is withdrawn while nothing of it has landed. That alone, never the
	// context itself, because the batch is sent on a context of its own.
	hungUp func() error

	// verb names the write in an error, as the caller described it.
	verb string

	// change is handed the counter as the writes queued before this one
	// leave it — the zero Tally and false where there is none yet — and
	// answers the counter to write and whether to write it at all.
	change func(stored coord.Tally, found bool) (coord.Tally, bool)

	// turn is closed when the lane's turn is handed to this write's caller;
	// handed records that it was, under the lane's mu.
	turn   chan struct{}
	handed bool

	// since is when the write's first attempt was sent, the zero time until
	// then: its patience runs from it. Only the caller holding the lane's
	// turn reads or sets it, and a write passes between holders under the
	// lane's mu.
	since time.Time

	// done is closed once tally and err hold the answer.
	done  chan struct{}
	tally coord.Tally
	err   error
}

func (w *counterWrite) answer(tally coord.Tally, err error) {
	w.tally, w.err = tally, err
	close(w.done)
}

func (w *counterWrite) answered() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// joinLane is the lane of one counter key, made if no caller is using one.
func (f *FleetStore) joinLane(key string) *counterLane {
	f.lanesMu.Lock()
	defer f.lanesMu.Unlock()
	if f.lanes == nil {
		f.lanes = map[string]*counterLane{}
	}
	lane, ok := f.lanes[key]
	if !ok {
		lane = &counterLane{}
		f.lanes[key] = lane
	}
	lane.members++
	return lane
}

// leaveLane gives up one caller's use of a lane, dropping it with the last.
func (f *FleetStore) leaveLane(key string, lane *counterLane) {
	f.lanesMu.Lock()
	defer f.lanesMu.Unlock()
	if lane.members--; lane.members == 0 {
		delete(f.lanes, key)
	}
}

// enqueue queues w, handing its caller the turn when nobody holds it.
func (l *counterLane) enqueue(w *counterWrite) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = append(l.queue, w)
	if !l.sending {
		l.sending = true
		l.handOn()
	}
}

// take empties the queue into the batch the turn's holder sends.
func (l *counterLane) take() []*counterWrite {
	l.mu.Lock()
	defer l.mu.Unlock()
	batch := l.queue
	l.queue = nil
	return batch
}

// giveBack puts the writes a batch left unanswered back at the front of the
// queue, in their order, and hands the turn on.
func (l *counterLane) giveBack(rest []*counterWrite) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.queue = slices.Concat(rest, l.queue)
	l.handOn()
}

// withdraw takes w out of the queue, handing the turn on if it had been handed
// to w, and reports false when a batch already holds w.
func (l *counterLane) withdraw(w *counterWrite) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	i := slices.Index(l.queue, w)
	if i < 0 {
		return false
	}
	l.queue = slices.Delete(l.queue, i, i+1)
	if w.handed {
		l.handOn()
	}
	return true
}

// handOn hands the turn to the caller of the first write queued, or lets it go
// when nothing is queued. Under l.mu, by a caller giving the turn up.
func (l *counterLane) handOn() {
	if len(l.queue) == 0 {
		l.sending = false
		return
	}
	next := l.queue[0]
	next.handed = true
	close(next.turn)
}

// casTally is one read-modify-write of a scope's counter under a
// compare-and-swap, combined with this node's other writes to the same counter
// and retried while another node's write lands first. See the top of this file.
//
// change is handed the stored counter — the zero Tally and false when the
// scope has none — and answers the counter to write and whether to write it at
// all. casTally answers what change last answered, written or not.
//
// Running out of patience is reported as an ERROR, never as a refusal: a
// charge's caller then fails the round rather than telling an agent it is out
// of budget, which is the fail-closed direction the contract requires.
func (f *FleetStore) casTally(ctx context.Context, scope, verb string,
	change func(stored coord.Tally, found bool) (coord.Tally, bool)) (coord.Tally, error) {

	if err := ctx.Err(); err != nil {
		return coord.Tally{}, unavailable(verb, err)
	}
	key := encodeKey(scope)
	lane := f.joinLane(key)
	defer f.leaveLane(key, lane)

	w := &counterWrite{hungUp: ctx.Err, verb: verb, change: change,
		turn: make(chan struct{}), done: make(chan struct{})}
	lane.enqueue(w)
	hangUp := ctx.Done()
	for {
		select {
		case <-w.done:
			return w.tally, w.err
		case <-w.turn:
			// The lane's turn, handed to the first write queued: the
			// batch taken now carries w, and is sent until w is answered.
			lane.giveBack(f.writeCounter(ctx, scope, key, lane.take(), w))
			return w.tally, w.err
		case <-hangUp:
			if lane.withdraw(w) {
				return coord.Tally{}, unavailable(verb, ctx.Err())
			}
			// A batch holds w. It answers w at its next attempt
			// boundary, or lands it — or gives it back unanswered, when
			// that batch's own holder hangs up, and then w's caller is
			// handed the turn and its batch withdraws w first thing.
			hangUp = nil
		}
	}
}

// writeCounter sends one batch of a lane's writes for the caller holding the
// lane's turn, whose own write is own, until the batch lands or fails —
// answering every write in it — or until own leaves it. Then it answers
// nothing more, and returns the writes still unanswered for the lane to send
// next.
//
// EVERY REQUEST ON A CONTEXT NOTHING ENDS: the batch is sent for every caller
// in it, so no one caller hanging up may cut a request short, and neither may
// a write's patience — see the top of this file. The client bounds each
// request by its own timeout, and the caller's context lends it only its
// values.
func (f *FleetStore) writeCounter(ctx context.Context, scope, key string,
	batch []*counterWrite, own *counterWrite) []*counterWrite {

	send := context.WithoutCancel(ctx)
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			// Cut short when own's caller hangs up — the boundary below
			// answers it — so the turn passes at once rather than after
			// the wait.
			awaitRace(ctx, attempt)
		}
		// An attempt boundary: every attempt before it lost, so a write
		// whose caller has hung up, or whose patience has run out, has had
		// nothing of it land — it leaves the batch here, told so.
		now := time.Now()
		batch = slices.DeleteFunc(batch, func(w *counterWrite) bool {
			switch err := w.hungUp(); {
			case err != nil:
				w.answer(coord.Tally{}, unavailable(w.verb, err))
			case !w.since.IsZero() && now.Sub(w.since) >= f.patience:
				w.answer(coord.Tally{}, outwaited(w.verb, scope, f.patience))
			default:
				return false
			}
			return true
		})
		if own.answered() {
			return batch
		}
		for _, w := range batch {
			if w.since.IsZero() {
				w.since = now
			}
		}
		answers, lost, failure := f.sendCounter(send, key, batch)
		switch {
		case failure != nil:
			answerAll(batch, failure)
			return nil
		case lost:
			continue
		}
		for i, w := range batch {
			w.answer(answers[i], nil)
		}
		return nil
	}
}

// answerAll answers every write in a batch with the error failure names for it.
func answerAll(batch []*counterWrite, failure func(verb string) error) {
	for _, w := range batch {
		w.answer(coord.Tally{}, failure(w.verb))
	}
}

// sendCounter makes one attempt at a batch: it reads the counter, applies every
// write's change to it in order, and writes the result if any change asked to.
//
// It answers each write's counter when the attempt landed or had nothing to
// write; that it LOST its race, which leaves the batch for the next attempt;
// or the failure, as the error it is for each write's verb.
func (f *FleetStore) sendCounter(ctx context.Context, key string,
	batch []*counterWrite) (answers []coord.Tally, lost bool, failure func(verb string) error) {

	fail := func(err error) func(string) error {
		return func(string) error { return err }
	}
	var (
		stored   coord.Tally
		found    bool
		revision uint64
	)
	entry, err := f.get(ctx, f.budgets, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
	case err != nil:
		return nil, false, fail(unavailable("read the token counter", err))
	default:
		if stored, err = decodeTally(entry.Value()); err != nil {
			return nil, false, fail(unavailable("decode the token counter", err))
		}
		found, revision = true, entry.Revision()
	}

	// Each change sees the counter as the ones queued before it leave it,
	// and a counter an earlier change writes exists for every later one.
	next, exists, write := stored, found, false
	answers = make([]coord.Tally, len(batch))
	for i, w := range batch {
		changed, writes := w.change(next, exists)
		answers[i] = changed
		if writes {
			next, exists, write = changed, true, true
		}
	}
	if !write {
		return answers, false, nil
	}
	raw, err := encodeTally(next)
	if err != nil {
		return nil, false, fail(err)
	}
	if found {
		_, err = f.budgets.Update(ctx, key, raw, revision)
	} else {
		_, err = f.create(ctx, f.budgets, key, raw)
	}
	switch {
	case err == nil:
		return answers, false, nil
	case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyExists):
		return nil, true, nil
	default:
		return nil, false, func(verb string) error { return unavailable(verb, err) }
	}
}
