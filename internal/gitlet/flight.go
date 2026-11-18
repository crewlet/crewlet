package gitlet

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// A SINGLE FLIGHT A KEY, shared by the change writer and the checks writer:
// each writes one key at a time — a change's conversation ([ChangeWriter]), a
// repository's checks ([ChecksWriter]) — and keys side by side, a write holding
// its key's turn from its decide to its answer. A publish runs under the
// writer's lifetime (flights.life), never its asker's, so an asker that hangs up
// never leaves the next write deciding beside a record still in flight.
//
// A write holding or awaiting a turn holds its decoded request and an answer
// slot of the node's proposal lane, which the committer's pushes are answered on
// too: [flights.join] refuses it busy, undecided, past its writer's bounds
// (flightBounds; the node count is a share of internal/estate's
// proposalAnswers), so a burst on one key never starves a push of a slot.

// flightBounds is what one writer's single flights admit.
type flightBounds struct {
	// key is the most writes of one key holding or waiting for its turn at
	// once; node the most of every key together; bytes the most those may
	// weigh together.
	key, node, bytes int
	// keyBound, nodeBound and bytesBound name the constants each is, as a
	// refusal's sentence names them.
	keyBound, nodeBound, bytesBound string
	// noun names a key in a sentence (a "change"), and writes its writes
	// ("writes", "check writes").
	noun, writes string
	// closed is the writer's refusal of a write it decides nothing of once
	// it is closed — its not-serving error.
	closed error
}

// flights is one writer's single flights.
type flights struct {
	bounds flightBounds

	// clock is the wait's clock — the wall clock's monotonic reading, never a
	// decide's clock, which a caller may pin — and a test's own.
	clock func() time.Time

	// life is the publishes' context, the writer's lifetime rather than an
	// asker's; stop ends it once its wait is over.
	life context.Context
	end  context.CancelFunc

	mu sync.Mutex
	// turns is each key with a write holding or waiting for its turn; a key
	// leaves it with its last such write. held and weighed are those writes'
	// count and bytes across every key.
	turns   map[string]*flightTurn
	held    int
	weighed int
	closed  bool
	closing chan struct{}
	// wg is the publishes in flight.
	wg sync.WaitGroup
}

// flightTurn is one key's single flight: the one write of it in flight holds
// token, and every other one waits to.
type flightTurn struct {
	token chan struct{}
	// holders is the writes holding or waiting for token; changed only under
	// the flights' mu.
	holders int
}

// newFlights is a writer's single flights, admitting what b bounds.
func newFlights(b flightBounds) *flights {
	f := &flights{bounds: b, clock: time.Now, turns: map[string]*flightTurn{}, closing: make(chan struct{})}
	f.life, f.end = context.WithCancel(context.Background())
	return f
}

// flightHold is one write's hold of its key's turn, given back by release.
type flightHold struct {
	f      *flights
	key    string
	turn   *flightTurn
	weight int
}

// release gives the turn to the key's next write and the write's place back.
func (h *flightHold) release() {
	<-h.turn.token
	h.f.leave(h.key, h.turn, h.weight)
}

// take waits for key's turn for a write weighing weight and answers its hold;
// or a busy sentence and no hold, past a bound ([flights.join]) or past the room
// its asker's deadline leaves for a publish and its answer ([CommitBudget],
// [ReplyMargin]) — the committer's rule for a push's wait, since past it the
// asker hands the operation id to the key's runner-up, which would contend on
// the key's subject. An asker with no deadline (in-process, no runner-up) waits
// at most a CommitBudget. Or an error and no hold: ctx ending, or the writer
// closed (flightBounds.closed). Nothing is decided either way; ctx bounds only
// this caller's wait.
func (f *flights) take(ctx context.Context, key string, weight int) (*flightHold, string, error) {
	arrived := f.clock()
	deadline, hasDeadline := ctx.Deadline()
	wait := CommitBudget
	if hasDeadline {
		if wait = deadline.Sub(arrived) - CommitBudget - ReplyMargin; wait <= 0 {
			return nil, f.shortOfTime(deadline.Sub(arrived)), nil
		}
	}
	turn, busy, err := f.join(key, weight)
	if err != nil || busy != "" {
		return nil, busy, err
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case turn.token <- struct{}{}:
		return &flightHold{f: f, key: key, turn: turn, weight: weight}, "", nil
	case <-timer.C:
		f.leave(key, turn, weight)
		if hasDeadline {
			return nil, f.shortOfTime(deadline.Sub(f.clock())), nil
		}
		return nil, fmt.Sprintf("the write waited %s behind this %s's other %s on this node, and a write "+
			"waits at most %s for its turn; %s", f.clock().Sub(arrived).Round(time.Millisecond), f.bounds.noun,
			f.bounds.writes, CommitBudget, flightAgain), nil
	case <-ctx.Done():
		f.leave(key, turn, weight)
		return nil, "", ctx.Err()
	case <-f.closing:
		f.leave(key, turn, weight)
		return nil, "", f.bounds.closed
	}
}

// flightAgain is what a write refused busy is told to do.
const flightAgain = "send the write again"

// shortOfTime is the refusal of a write whose asker stops waiting in left —
// no more than a publish and its answer may take.
func (f *flights) shortOfTime(left time.Duration) string {
	left = min(max(left, 0), CommitBudget+ReplyMargin)
	return fmt.Sprintf("the write's asker stops waiting in %s, and a publish started now may take %s and its "+
		"answer %s more; %s", left.Round(time.Millisecond), CommitBudget, ReplyMargin, flightAgain)
}

// join counts a write weighing weight holding or waiting for key's turn,
// answering its single flight — or, refused at one of the three bounds, the
// busy sentence that says which; or the writer's closed refusal.
func (f *flights) join(key string, weight int) (*flightTurn, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil, "", f.bounds.closed
	}
	b := f.bounds
	turn := f.turns[key]
	switch {
	case turn != nil && turn.holders >= b.key:
		return nil, fmt.Sprintf("%d %s of this %s hold or wait for its turn on this node, the most one %s "+
			"takes at once (%s); %s", turn.holders, b.writes, b.noun, b.noun, b.keyBound, flightAgain), nil
	case f.held >= b.node:
		return nil, fmt.Sprintf("%d %s hold or wait for their %s's turn on this node, the most it takes at "+
			"once (%s); %s", f.held, b.writes, b.noun, b.nodeBound, flightAgain), nil
	case f.weighed+weight > b.bytes:
		return nil, fmt.Sprintf("%d bytes of %s are held on this node, and %d more would pass the %d it holds "+
			"(%s); %s", f.weighed, b.writes, weight, b.bytes, b.bytesBound, flightAgain), nil
	}
	if turn == nil {
		turn = &flightTurn{token: make(chan struct{}, 1)}
		f.turns[key] = turn
	}
	turn.holders++
	f.held++
	f.weighed += weight
	return turn, "", nil
}

// leave counts a write of key gone, forgetting the key with its last.
func (f *flights) leave(key string, turn *flightTurn, weight int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held--
	f.weighed -= weight
	if turn.holders--; turn.holders == 0 {
		delete(f.turns, key)
	}
}

// begin counts a publish in flight — false once the writer is closed, whose
// publish is then never started.
func (f *flights) begin() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.wg.Add(1)
	return true
}

// done counts a publish begin started ended.
func (f *flights) done() { f.wg.Done() }

// close refuses every new write and every one waiting for its turn with the
// writer's not-serving refusal, so the router asks the runner-up, and leaves the
// publishes in flight to run. The engine closes every writer of a node before
// draining them all under one bound. Safe to call more than once.
func (f *flights) close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.closing)
	}
}

// stop closes the flights and waits for the publishes in flight, which are
// FINISHED rather than abandoned so their askers get their outcome; if ctx
// ends first, they are cancelled and answered from what their publish
// returned. Safe to call more than once.
func (f *flights) stop(ctx context.Context) {
	f.close()
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	f.end()
	<-done
}

// holders is how many writes of key hold or wait for its turn.
func (f *flights) holders(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t := f.turns[key]; t != nil {
		return t.holders
	}
	return 0
}
