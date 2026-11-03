package code

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// A SINGLE FLIGHT A KEY, AND WHAT IT ADMITS: the rule the change writer and the
// checks writer share. Each takes ONE KEY'S WRITES ONE AT A TIME — a change's
// conversation ([ChangeWriter]), a repository's checks ([ChecksWriter]) — and
// keys side by side, each write holding its key's turn from its decide to its
// answer; and each publish runs under the writer's own lifetime, never its
// asker's, so an asker that hangs up mid-publish never leaves the key's next
// write deciding beside a record still in flight.
//
// # Refused at once past any of three bounds
//
// A write that holds or waits for its key's turn holds the estate's answer
// slot it was asked through — a slot of the node's proposal lane, which the
// committer's pushes are answered on too — and its decoded request. So before
// it takes a place it is held, as the committer holds a proposal, to three
// bounds, and refused busy AT ONCE, undecided, past any of them: the writes of
// its own KEY holding or waiting for the turn, the writes of EVERY key the
// writer holds, and the BYTES those weigh together. Each writer's three are its
// own constants (flightBounds), and the node-wide count is its share of the
// proposal lane (internal/estate's proposalAnswers, which sums the shares): a
// burst on one key — written faster than its single flight lands them, or
// queued behind a publish an election or a lagging applier holds — meets its
// writer's refusal and never holds the slots a push routed to the node needs.
//
// # And refused in time
//
// A write waits for its turn only while its asker's attempt leaves room for a
// publish and its answer ([CommitBudget], [ReplyMargin]), and is refused busy,
// undecided, the moment it does not — the committer's rule for a push's wait
// in its queue, and for its reason: past it the asker hands the same operation
// id to the key's runner-up, which would then contend with this node on the
// key's subject. One whose asker has no deadline — the in-process path, where
// no runner-up is asked while it runs — waits at most a CommitBudget.
//
// # Closed before it stops
//
// [flights.close] refuses every new write and every one still waiting for its
// turn with the writer's own not-serving refusal, so the router asks the
// runner-up; [flights.stop] then waits for the publishes in flight, FINISHED
// rather than abandoned so their askers get their outcome, and cancels them only
// once its context ends. The engine closes every writer of a node first and
// drains them all at once, under one bound.

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

	// life is the publishes' context: the writer's own lifetime, never an
	// asker's — a publish runs on whether or not its asker still waits.
	// Ended by stop once its wait is over.
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

// take waits for key's turn for a write weighing weight, and answers its hold
// — or, refused, a busy sentence and no hold: past one of the three bounds at
// once, or past the room its asker's deadline leaves (or a whole CommitBudget
// for an asker with none) while it waits — or an error and no hold: the
// asker's context ending, or the writer closed (flightBounds.closed). Nothing
// is decided either way. ctx bounds only this caller's wait.
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

// close refuses every new write and every one waiting for its turn the
// writer's closed refusal, leaving the publishes in flight to run. Safe to
// call more than once.
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
