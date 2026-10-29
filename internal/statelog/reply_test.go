package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// replyMargin is the margin these cases mark a request with, and askerSlack
// what each request's asker leaves the serving node beyond it — so a wait the
// margin bounds ends askerSlack in, and one it does not runs on to the deadline
// a whole margin later.
const (
	replyMargin = time.Second
	askerSlack  = 400 * time.Millisecond
)

// asked is a request its asker waits for across a wire, until margin and slack
// from now ([statelog.WithReplyMargin]), and the deadline it stops listening
// at.
func asked(t *testing.T) (context.Context, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), replyMargin+askerSlack)
	t.Cleanup(cancel)
	deadline, _ := ctx.Deadline()
	return statelog.WithReplyMargin(ctx, replyMargin), deadline
}

// heard fails a case whose answer came with less than half the margin left
// before its asker stopped listening: an answer sent then is one the asker
// may already have given up on.
func heard(t *testing.T, what string, deadline time.Time) {
	t.Helper()
	if left := time.Until(deadline); left < replyMargin/2 {
		t.Fatalf("%s was answered %v before its asker stops listening, want about the %v "+
			"margin — a wait that runs to the deadline answers nobody", what,
			left.Round(time.Millisecond), replyMargin)
	}
}

// unreachedWaiter is a node that never reaches the position a read waits for,
// and waits until it is told to stop.
type unreachedWaiter struct{ at statelog.Position }

func (w unreachedWaiter) Reached() statelog.Position { return w.at }

func (w unreachedWaiter) WaitReached(ctx context.Context, _ statelog.Position) error {
	<-ctx.Done()
	return ctx.Err()
}

func (w unreachedWaiter) WaitApplied(ctx context.Context, _ statelog.ScopeSet, _ statelog.Position) error {
	<-ctx.Done()
	return ctx.Err()
}

// heldAppender takes a barrier's append and holds it until the case ends, a
// broker that does not answer; began is closed as the first append arrives.
type heldAppender struct {
	release, began chan struct{}
	once           *sync.Once
}

func newHeldAppender(t *testing.T) heldAppender {
	t.Helper()
	a := heldAppender{release: make(chan struct{}), began: make(chan struct{}), once: &sync.Once{}}
	t.Cleanup(func() { close(a.release) })
	return a
}

func (a heldAppender) Append(ctx context.Context, _, _ string, _ *uint64, _ []byte) (uint64, bool, error) {
	a.once.Do(func() { close(a.began) })
	select {
	case <-a.release:
		return 0, false, errors.New("the case ended")
	case <-ctx.Done():
		return 0, false, ctx.Err()
	}
}

func (heldAppender) LastSeq(context.Context, string) (uint64, bool, error) { return 0, false, nil }

// heldIndex is a read index whose every barrier is held ([heldAppender]).
func heldIndex(t *testing.T, held heldAppender) *statelog.ReadIndex {
	t.Helper()
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), held,
		testSigner(t, probeDomain{}), noCeiling(t), probeEncode, func() uint32 { return 1 }, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	return index
}

// A READ ASKED ACROSS A WIRE SAYS IT IS BEHIND WHILE ITS ASKER IS STILL
// LISTENING. The node never reaches the position the read waits for, and the
// asker stops listening long before the read's own budget (statelog.ReadBudget)
// would end the wait: run to the deadline, the wait ended with the caller's
// context and nobody heard the node say anything, which an asker reads as a
// node that is gone. It ends a margin short of the deadline instead, refused
// `behind` and saying it ran out of what the asker left.
func TestAServedReadSaysItIsBehindWhileItsAskerListens(t *testing.T) {
	t.Parallel()
	if replyMargin+askerSlack >= statelog.ReadBudget {
		t.Fatalf("the premise: the asker's %v must end before the read's own %v wait would",
			replyMargin+askerSlack, statelog.ReadBudget)
	}
	r := newReader(t, newStubStore(t), healthy, unreachedWaiter{at: healthy().Position}, nil)
	q := pointQuery(statelog.ReadSession)
	q.Session = statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}

	ctx, deadline := asked(t)
	_, err := r.Read(ctx, q, func(*sql.Tx) error { return nil })
	heard(t, "a read behind its position", deadline)
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseBehind {
		t.Fatalf("a read that never reaches its position answered %v, want refused %q", err,
			statelog.RefuseBehind)
	}
	if !strings.Contains(refusal.Detail, "all its asker's attempt left") {
		t.Errorf("the refusal reads %q, which does not say the asker's time is what ran out",
			refusal.Detail)
	}
}

// A LINEARIZABLE READ'S BARRIER ENDS WHERE ITS ASKER'S ATTEMPT DOES, less the
// margin. The barrier's append is held — a broker that does not answer — and
// the barrier's own budget (statelog.DefaultBarrierBudget) outlasts the
// asker: waited out, the read was refused after its asker had stopped
// listening. It is refused `no_quorum` a margin short of the deadline instead,
// saying the asker's time is what ran out.
func TestAServedReadsBarrierEndsWhileItsAskerListens(t *testing.T) {
	t.Parallel()
	if replyMargin+askerSlack >= statelog.DefaultBarrierBudget {
		t.Fatalf("the premise: the asker's %v must end before the barrier's own %v would",
			replyMargin+askerSlack, statelog.DefaultBarrierBudget)
	}
	index := heldIndex(t, newHeldAppender(t))
	r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, index)

	ctx, deadline := asked(t)
	_, err := r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
	heard(t, "a read whose barrier is held", deadline)
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseNoQuorum {
		t.Fatalf("a read whose barrier never committed answered %v, want refused %q", err,
			statelog.RefuseNoQuorum)
	}
	if !strings.Contains(refusal.Detail, "all its asker's attempt left") {
		t.Errorf("the refusal reads %q, which does not say the asker's time is what ran out",
			refusal.Detail)
	}
}

// A SERVED READ ON A FULL LOG IS STILL TOLD THE LOG IS FULL: only a barrier
// whose wait the asker's attempt ended is refused as having run out of it. A
// barrier the broker refused is the broker's answer whatever request it was
// for, and `log_full` names the ceiling an operator has to raise where
// `no_quorum` sends them to look for an election.
func TestAServedReadOnAFullLogIsToldTheLogIsFull(t *testing.T) {
	t.Parallel()
	index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), fullLog(),
		testSigner(t, probeDomain{}), noCeiling(t), probeEncode, func() uint32 { return 1 }, nil)
	if err != nil {
		t.Fatalf("NewReadIndex: %v", err)
	}
	r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, index)

	ctx, _ := asked(t)
	_, err = r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseLogFull {
		t.Fatalf("a served read whose barrier the full log refused answered %v, want refused %q",
			err, statelog.RefuseLogFull)
	}
}

// A READER THAT HANGS UP DURING ITS BARRIER IS TOLD ITS OWN ENDING, never a
// refusal: a caller walking away says nothing about the broker, and refused
// `no_quorum` it was counted on every graph an operator reads as a majority
// that failed to agree.
func TestAReaderThatHangsUpDuringItsBarrierIsToldItsOwnEnding(t *testing.T) {
	t.Parallel()
	held := newHeldAppender(t)
	r := newReader(t, newStubStore(t), healthy, &stubWaiter{at: healthy().Position}, heldIndex(t, held))

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-held.began
		cancel()
	}()
	_, err := r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
	var refusal *statelog.Refused
	if !errors.Is(err, context.Canceled) || errors.As(err, &refusal) {
		t.Fatalf("a reader that hung up during its barrier was answered %v, want its own ending", err)
	}
}

// A WRITE ASKED ACROSS A WIRE ANSWERS WHILE ITS ASKER IS STILL LISTENING, in
// each of the three waits it may make for this node's applier: its own record's
// resolution answers `pending`, and its wait for the caller's session and for a
// peer's record ahead of it refuse `behind`. Every one of them is given a
// budget of the publisher's that outlasts the asker (the default five seconds),
// so what ends each is the asker's attempt, a margin short of its deadline —
// and each answer that names its bound says so.
func TestAServedWriteAnswersWhileItsAskerListens(t *testing.T) {
	t.Parallel()
	if replyMargin+askerSlack >= statelog.DefaultResolveBudget {
		t.Fatalf("the premise: the asker's %v must end before the write's own %v wait would",
			replyMargin+askerSlack, statelog.DefaultResolveBudget)
	}
	// served is a write's request, given the publisher's default budget so
	// the asker's attempt, not the harness's short one, is what ends a wait.
	served := func(t *testing.T) (context.Context, time.Time) {
		ctx, deadline := asked(t)
		return statelog.WithResolveBudget(ctx, statelog.DefaultResolveBudget), deadline
	}
	waitedWithin := func(t *testing.T, h *harness) {
		t.Helper()
		waits := h.applier.waitBudgets()
		if len(waits) == 0 {
			t.Fatal("the write never waited for this node's applier")
		}
		for _, w := range waits {
			if w > askerSlack {
				t.Errorf("the write waited on a budget of %v, past the %v its asker left", w, askerSlack)
			}
		}
	}

	t.Run("resolution", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.applier.mu.Lock()
		h.applier.auto = false // this node never reaches its own record
		h.applier.mu.Unlock()

		ctx, deadline := served(t)
		res, err := h.pub.Publish(ctx, sessionWrite(probeSubject("a"), "op-1",
			statelog.Position{}, func() {}))
		heard(t, "a write that never resolves here", deadline)
		if err != nil || res.Outcome != statelog.OutcomePending {
			t.Fatalf("the write answered %q (%v), want pending", res.Outcome, err)
		}
		waitedWithin(t, h)
	})

	for _, tc := range []struct {
		name string
		// arrange puts this node behind the position the write waits for,
		// returning the session the write carries.
		arrange func(t *testing.T, h *harness) statelog.Position
	}{
		{name: "session", arrange: func(_ *testing.T, h *harness) statelog.Position {
			h.applier.mu.Lock()
			h.applier.stalled = true // the caller's own write is never reached
			h.applier.mu.Unlock()
			return statelog.Position{Stream: probeStream, Generation: 1, Seq: 9_000}
		}},
		{name: "behind", arrange: func(t *testing.T, h *harness) statelog.Position {
			peer, _, err := h.log.Append(t.Context(), probePrefix+".object.a", "peer-op", nil,
				signedByPeer(t, h, []byte("peer")))
			if err != nil {
				t.Fatalf("the peer's write: %v", err)
			}
			h.anchorAt(probeSubject("a"), peer-1) // behind a peer's record...
			h.applier.mu.Lock()
			h.applier.stalled = true // ...that this node never reaches
			h.applier.mu.Unlock()
			return statelog.Position{}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			session := tc.arrange(t, h)

			ctx, deadline := served(t)
			_, err := h.pub.Publish(ctx, sessionWrite(probeSubject("a"), "op-1", session, func() {}))
			heard(t, "a write behind its position", deadline)
			var refusal *statelog.Unavailable
			if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonBehind {
				t.Fatalf("the write answered %v, want refused behind", err)
			}
			if !strings.Contains(refusal.Detail, "all its asker's attempt left") {
				t.Errorf("the refusal reads %q, which does not say the asker's time is what "+
					"ran out", refusal.Detail)
			}
			waitedWithin(t, h)
		})
	}
}

// A WAIT IS BOUNDED BY THE REQUEST IT BELONGS TO AND BY NO OTHER. On a request
// asked across a wire a wait ends at the margin when that comes before its own
// budget, and at its budget when that comes first; a context DETACHED from the
// request — a rollback, a teardown, which outlives it on purpose — carries its
// values and keeps its own bounds, with no deadline or with a later one of its
// own; and nothing is marked that has no deadline, or no positive margin.
func TestAWaitIsBoundedByTheRequestItBelongsToAndNoOther(t *testing.T) {
	t.Parallel()
	const budget = 2 * time.Second
	request, cancel := context.WithTimeout(t.Context(), replyMargin+askerSlack)
	defer cancel()
	served := statelog.WithReplyMargin(request, replyMargin)
	passed, cancelPassed := context.WithTimeout(t.Context(), replyMargin/2)
	defer cancelPassed()
	later, cancelLater := context.WithTimeout(context.WithoutCancel(served), time.Minute)
	defer cancelLater()

	for _, tc := range []struct {
		name   string
		ctx    context.Context
		budget time.Duration
		asker  bool
		// within is the bound's most; an asker's bound is at least none.
		within time.Duration
	}{
		{name: "the asker's attempt comes first", ctx: served, budget: budget, asker: true,
			within: askerSlack},
		{name: "the wait's own budget comes first", ctx: served, budget: askerSlack / 4,
			within: askerSlack / 4},
		{name: "a request whose margin has already passed", budget: budget, asker: true,
			ctx: statelog.WithReplyMargin(passed, replyMargin)},
		{name: "detached from the request", ctx: context.WithoutCancel(served), budget: budget,
			within: budget},
		{name: "detached, with a later deadline of its own", ctx: later, budget: budget,
			within: budget},
		{name: "no request at all", ctx: t.Context(), budget: budget, within: budget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			waitCtx, cancel, bound := statelog.WaitWithin(tc.ctx, tc.budget)
			defer cancel()
			if bound.Asker != tc.asker || bound.Within > tc.within || bound.Within < 0 ||
				(!tc.asker && bound.Within != tc.within) {
				t.Fatalf("the wait is bounded %+v, want asker=%v within at most %v", bound,
					tc.asker, tc.within)
			}
			deadline, ok := waitCtx.Deadline()
			if !ok || time.Until(deadline) > bound.Within {
				t.Fatalf("the wait's context ends in %v, past its %v bound", time.Until(deadline),
					bound.Within)
			}
		})
	}

	for _, unmarked := range []struct {
		name   string
		ctx    context.Context
		margin time.Duration
	}{
		{name: "no deadline", ctx: context.WithoutCancel(request), margin: replyMargin},
		{name: "no margin", ctx: request},
		{name: "a negative margin", ctx: request, margin: -time.Second},
	} {
		if got := statelog.WithReplyMargin(unmarked.ctx, unmarked.margin); got != unmarked.ctx {
			t.Errorf("a context with %s was marked a served request", unmarked.name)
		}
	}
}
