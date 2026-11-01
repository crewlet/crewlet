package harness

import (
	"context"
	"fmt"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A SLOT IS ONE LIVE CLI PROCESS, and max_concurrent is how many an entry may
// have at once: the memory bound an operator sizes a host by (a coding CLI is a
// Node or Bun runtime of a few hundred megabytes). A run holds its slot for as
// long as its process lives, through every tool call it makes.
//
// # Except for the calls made from inside those tool calls
//
// A tool can make a model call of its own — a condensing pass, a judge, an
// answer drawn from the knowledge base — and the seat's chain usually puts it
// on the same entry. With the entry's slots all held by runs blocked in such
// calls, the nested call would wait for a slot only those runs can free, and
// they free it only when it answers: at max_concurrent 1, every such call is a
// deadlock until somebody's deadline.
//
// So a run LENDS its slot to the calls made inside its own tool calls, and to
// nothing else: the run puts a [lease] on the context its calls run under
// ([llm.ToolHost.Serve]), and a model call on the same entry carrying one takes
// a free slot if there is one and the run's otherwise. One borrower at a time
// per run — calls nested under one run share its slot in turn — and the slot
// goes back to the pool only when the run and its borrower are both done. The
// run's process is idle while a tool of its runs, so what is alive per slot is
// bounded by how deep the nesting goes, never by how many seats are waiting —
// which lending to the pool would not be: a slot handed to another seat while
// its run's process lives is a process no slot counts.

// token is one of an entry's slots while it is out of the pool, and everyone
// holding it.
type token struct {
	p       *Provider
	mu      sync.Mutex
	holders int
}

// join counts one more holder, unless the token went back to the pool.
func (t *token) join() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.holders == 0 {
		return false
	}
	t.holders++
	return true
}

// leave counts one holder out, and puts the slot back when it was the last.
func (t *token) leave() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.holders--
	if t.holders == 0 {
		<-t.p.slots
	}
}

// lease is what a run lends the calls nested in its tool calls: its slot, one
// borrower at a time.
type lease struct {
	tok  *token
	turn chan struct{}
}

type leaseKey struct{}

// withLease puts a run's lease on the context its tool calls run under.
func withLease(ctx context.Context, l *lease) context.Context {
	return context.WithValue(ctx, leaseKey{}, l)
}

// slot is one call's hold on a slot: its own, or a borrowed one.
type slot struct {
	tok *token
	// turn is the lender's, held while this call borrows.
	turn chan struct{}
	once sync.Once
}

// lend is the lease a run on this slot hands its tool calls.
func (s *slot) lend() *lease {
	return &lease{tok: s.tok, turn: make(chan struct{}, 1)}
}

// release gives the slot up: back to the pool if nobody else holds it.
func (s *slot) release() {
	s.once.Do(func() {
		if s.turn != nil {
			<-s.turn
		}
		s.tok.leave()
	})
}

// acquire takes a slot for one call: a free one from the pool, or — for a
// call made inside a tool call of a run on this entry — the run's, when no
// free one comes first.
//
// Selectable against the caller's context: a seat whose turn was cancelled
// while queueing must not go on to launch a process nobody is waiting for.
func (p *Provider) acquire(ctx context.Context) (*slot, error) {
	var lent *lease
	if l, ok := ctx.Value(leaseKey{}).(*lease); ok && l.tok.p == p {
		lent = l
	}
	for {
		var turn chan struct{}
		if lent != nil {
			turn = lent.turn
			// A FREE SLOT FIRST, so calls nested under one run do not
			// queue for its slot while the entry has another idle.
			select {
			case p.slots <- struct{}{}:
				return &slot{tok: &token{p: p, holders: 1}}, nil
			default:
			}
		}
		select {
		case p.slots <- struct{}{}:
			return &slot{tok: &token{p: p, holders: 1}}, nil
		case turn <- struct{}{}:
			if lent.tok.join() {
				return &slot{tok: lent.tok, turn: turn}, nil
			}
			// The run is over and its slot back in the pool: wait there
			// like anybody else.
			<-turn
			lent = nil
		case <-ctx.Done():
			// Queued behind other seats and the caller gave up. Reported as
			// a timeout rather than fatal: nothing about the request was
			// wrong, and the chain should be free to try another member.
			return nil, p.fail(llm.KindTimeout, fmt.Errorf("waiting for a CLI slot: %w", ctx.Err()))
		}
	}
}
