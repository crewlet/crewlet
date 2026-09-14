package jsprovision

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// A CLUSTER STILL FORMING is the one error worth waiting out, and telling it
// from the rest is what keeps a config mistake from becoming a two-minute hang
// with the same message at the end.
func TestOnlyAPlacementFailureIsWaitedOut(t *testing.T) {
	t.Parallel()
	placement := &jetstream.APIError{
		ErrorCode: errCodeNoPeers, Code: 400,
		Description: "no suitable peers for placement",
	}
	if !Unplaceable(placement) {
		t.Fatal("a placement failure is not recognised")
	}
	// WRAPPED, because that is how it arrives: the caller adds the stream
	// or bucket name before anything sees it.
	if !Unplaceable(errors.Join(errors.New("ensure stream CREWLET_AGENT"), placement)) {
		t.Fatal("a wrapped placement failure is not recognised")
	}

	for _, err := range []error{
		nil,
		errors.New("connection refused"),
		&jetstream.APIError{ErrorCode: jetstream.JSErrCodeBadRequest, Code: 400,
			Description: "subject overlaps with an existing stream"},
		&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound, Code: 404},
	} {
		if Unplaceable(err) {
			t.Fatalf("%v was treated as a forming cluster", err)
		}
	}
}

// A CREATE THAT LANDED BUT IS NOT VISIBLE YET is a different question from a
// create the leader refused, and the two must not be confused: one is waited
// out by a lookup and the other by a create, and each waiting on the other's
// error waits for something nobody is going to do.
func TestNotYetVisibleIsNotAPlacementFailure(t *testing.T) {
	t.Parallel()

	// THE SHAPE THAT BROKE A CLUSTERED BOOT: the create of this very
	// stream had just succeeded, and the lookup that followed it said
	// 404.
	notFound := &jetstream.APIError{
		ErrorCode: jetstream.JSErrCodeStreamNotFound, Code: 404,
		Description: "stream not found",
	}
	if !NotYetVisible(notFound) {
		t.Error("a stream that has not propagated yet is not recognised")
	}
	if !NotYetVisible(jetstream.ErrBucketNotFound) {
		t.Error("a bucket that has not propagated yet is not recognised")
	}
	// WRAPPED, because that is how it reaches the caller.
	if !NotYetVisible(errors.Join(
		errors.New(`open the log "CREWLET_TRACKER_VECTORS"`), notFound)) {
		t.Error("a wrapped not-found is not recognised")
	}

	// AND THE TWO PREDICATES ARE DISJOINT. A lookup must not wait out a
	// placement failure, and a create must not wait out a not-found.
	placement := &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400}
	if NotYetVisible(placement) {
		t.Error("a placement failure was treated as a propagation delay")
	}
	if Unplaceable(notFound) {
		t.Error("a propagation delay was treated as a placement failure")
	}

	for _, err := range []error{
		nil,
		errors.New("connection refused"),
		&jetstream.APIError{ErrorCode: jetstream.JSErrCodeBadRequest, Code: 400},
	} {
		if NotYetVisible(err) {
			t.Errorf("%v was treated as a propagation delay", err)
		}
	}
}

// A CLUSTERED CREATE GETS MORE THAN A SOLO ONE, which is the whole reason this
// package exists: one flat number was measured failing a clustered boot while
// being enormous for a solo one.
func TestAClusteredCreateIsGivenLongerThanASoloOne(t *testing.T) {
	t.Parallel()
	if Budget(true) <= Budget(false) {
		t.Errorf("clustered gets %v and solo %v — the asymmetry this package "+
			"exists for is gone", Budget(true), Budget(false))
	}

	// AT LEAST AS PATIENT AS THE WAIT IT SITS BEHIND. A create runs after
	// the readiness wait has already spent up to a minute on the same
	// cluster for the same reason, so a create that gave up sooner would
	// be declaring wedged a group the layer below was still willing to
	// wait for. The number is jetstream's clusterReadyTimeout; it is
	// spelled here because a Go test cannot reach another package's
	// unexported constant, and a change to it must be reflected here.
	const clusterReadyTimeout = 60 * time.Second
	if Budget(true) < clusterReadyTimeout {
		t.Errorf("a clustered create gets %v, which is less patient than the "+
			"%v readiness wait it runs after", Budget(true), clusterReadyTimeout)
	}
}

// THE SEQUENCE IS BOUNDED, AND BY MORE THAN ONE CREATE — because a bring-up is
// many creates in a row and the per-create budget bounds only the term. This
// is the pairing that stopped the real worst case being the product: without
// it, raising the term multiplied a limit nobody had declared.
func TestASequenceIsBoundedAboveOneCreateAndBelowTheirSum(t *testing.T) {
	t.Parallel()

	// fleetBuckets is how many buckets OpenFleet opens in a row. Spelled
	// here for the same reason clusterReadyTimeout is above: it is the
	// count this ceiling was sized against.
	const fleetBuckets = 13

	for _, clustered := range []bool{false, true} {
		seq, one := SequenceBudget(clustered), Budget(clustered)
		if seq <= one {
			t.Errorf("clustered=%v: a whole sequence gets %v and one create "+
				"%v — a sequence that cannot outlast a single slow create "+
				"fails a boot the create itself would have survived",
				clustered, seq, one)
		}
		// AND IT IS A REAL CEILING. The point is to be BELOW the sum of
		// the terms; a sequence budget at or above it bounds nothing
		// that the per-create budgets did not already bound.
		if seq >= one*fleetBuckets {
			t.Errorf("clustered=%v: a whole sequence gets %v, which is not "+
				"less than %d creates at %v each — it bounds nothing",
				clustered, seq, fleetBuckets, one)
		}
	}
}

// THE TWO BUDGETS FOLLOW THE TOPOLOGY FACT, and a caller that holds one reads
// the same numbers the package functions give.
//
// It used to be inferred from the replica count, which reported a member
// naming peers at `replicas: 1` — a configuration this repository permits — as
// solo, handing a create that waits on a real metadata group the local
// file-store budget.
func TestTheBudgetsFollowWhetherTheBrokerHasPeers(t *testing.T) {
	t.Parallel()
	for _, clustered := range []bool{false, true} {
		c := Clustered(clustered)
		if got, want := c.Budget(), Budget(clustered); got != want {
			t.Errorf("Clustered(%v).Budget() = %v, want %v", clustered, got, want)
		}
		if got, want := c.SequenceBudget(), SequenceBudget(clustered); got != want {
			t.Errorf("Clustered(%v).SequenceBudget() = %v, want %v", clustered, got, want)
		}
	}
	if Clustered(true).Budget() <= Clustered(false).Budget() {
		t.Error("a clustered create is no more patient than a solo one")
	}
}

// A READ-BACK RE-ASKS while the object is not visible yet, and gives back the
// ASK'S error rather than a deadline of its own.
//
// Four read-backs on the provisioning path had the one-shot form and all four
// sit inside the window by construction — each is asking whether a create just
// landed. A single lookup answers at one arbitrary instant inside it.
func TestAReadBackReAsksWhileTheObjectIsNotVisibleYet(t *testing.T) {
	t.Parallel()

	// NOT VISIBLE, THEN VISIBLE: the answer is the later one.
	calls := 0
	err := Settle(t.Context(), func() error {
		calls++
		if calls < 3 {
			return jetstream.ErrStreamNotFound
		}
		return nil
	})
	if err != nil {
		t.Errorf("an object that appeared on the third look reported %v", err)
	}
	if calls != 3 {
		t.Errorf("asked %d times, want 3", calls)
	}

	// ANYTHING ELSE IS TERMINAL AT ONCE — waiting out a placement failure
	// would be waiting for something nobody is going to do.
	calls = 0
	placement := &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400}
	if err := Settle(t.Context(), func() error { calls++; return placement }); !errors.Is(err, placement) {
		t.Errorf("a placement failure came back as %v", err)
	}
	if calls != 1 {
		t.Errorf("a terminal error was re-asked %d times", calls)
	}

	// AND AN OBJECT THAT NEVER APPEARS reports the broker's own not-found,
	// which names it, rather than a bare deadline.
	err = Settle(t.Context(), func() error { return jetstream.ErrStreamNotFound })
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("an absent object reported %v, want the not-found that names it", err)
	}
}

// A READ-BACK STAYS SHORT. It is an ordinary metadata read against a group
// that has just proven it works, and inheriting the provisioning budget would
// multiply a failing boot's time to say so.
func TestAReadBackIsFarShorterThanACreate(t *testing.T) {
	t.Parallel()
	if ReadBack >= Budget(false) {
		t.Errorf("a read-back gets %v against a solo create's %v", ReadBack, Budget(false))
	}
	// AND THE RETRY CADENCE FITS INSIDE IT MANY TIMES OVER, or the wait is
	// one poll long and the tolerance is decoration.
	if PlacementRetry*10 > ReadBack {
		t.Errorf("a %v read-back holds fewer than ten %v polls",
			ReadBack, PlacementRetry)
	}
}

// A STALLED CREATE SAYS SO, because the failure this closes was a member that
// emitted nothing at all for 28 seconds and then died: the log could not name
// the object it was on, how many it had already done, or whether it was moving.
func TestSlowWorkIsReportedOnceAndFastWorkIsSilent(t *testing.T) {
	t.Parallel()

	// THE FIRING HALF, at a threshold short enough to assert. It is the
	// half that only runs when something is wrong, so it is the half that
	// would otherwise be a claim.
	fired := make(chan time.Duration, 1)
	stopSlow := whenSlowAfter(t.Context(), time.Millisecond,
		func(after time.Duration) { fired <- after })
	select {
	case got := <-fired:
		if got != time.Millisecond {
			t.Errorf("reported a wait of %v, want the threshold it crossed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("work that outlived its threshold produced no line at all, " +
			"which is the silence this exists to remove")
	}
	stopSlow()

	// AND THE FAST HALF IS SILENT, which is what runs on every healthy
	// create on every boot.
	var reports atomic.Int32
	stop := WhenSlow(t.Context(), func(time.Duration) { reports.Add(1) })
	stop()
	if got := reports.Load(); got != 0 {
		t.Errorf("work that finished at once produced %d line(s); every healthy "+
			"create on every boot goes through here", got)
	}

	// AND STOP IS A BARRIER. A report arriving after the call it described
	// has returned is a line attributed to the wrong object — and in a test
	// sink, one arriving after the case that owned it ended.
	//
	// ASSERTED BY THE RACE DETECTOR rather than by a poll, because that is
	// what the guarantee actually is: `written` is a PLAIN int, the
	// watcher writes it and this goroutine reads it after stop returns. If
	// stop does not wait for the watcher those two are unordered, and -race
	// says so. A threshold of zero makes the watcher fire at once, so the
	// two really do run together.
	started, proceed := make(chan struct{}), make(chan struct{})
	written := 0
	stop = whenSlowAfter(t.Context(), 0, func(time.Duration) {
		close(started)
		<-proceed
		written++
	})
	<-started // the watcher is inside report, so stop really has to wait
	close(proceed)
	stop()
	if written != 1 {
		t.Errorf("one slow call produced %d lines, want exactly one", written)
	}
}

// A CANCELLED CALLER IS NOT REPORTED ON either: the work stopped because
// somebody asked it to, which is not the silence this exists to describe.
func TestCancelledWorkIsNotReportedAsSlow(t *testing.T) {
	t.Parallel()
	var reports atomic.Int32
	ctx, cancel := context.WithCancel(t.Context())
	stop := WhenSlow(ctx, func(time.Duration) { reports.Add(1) })
	cancel()
	stop()
	if got := reports.Load(); got != 0 {
		t.Errorf("a cancelled wait produced %d line(s)", got)
	}
}

// THE THRESHOLD SITS INSIDE THE BUDGET IT DESCRIBES, or the line it exists to
// emit arrives after the failure it was meant to explain.
func TestTheSlowThresholdFitsInsideEveryBudget(t *testing.T) {
	t.Parallel()
	for _, clustered := range []bool{false, true} {
		if SlowAfter >= Budget(clustered) {
			t.Errorf("clustered=%v: a create is called slow at %v and gives up "+
				"at %v, so the breadcrumb never lands before the error does",
				clustered, SlowAfter, Budget(clustered))
		}
	}
}

// A CONSUMER THAT HAS NOT PROPAGATED is the same propagation delay a stream or
// a bucket is, and leaving it out of the predicate defeated the retry on the
// one object a seat's mailbox is made of.
func TestAConsumerNotYetVisibleIsAPropagationDelay(t *testing.T) {
	t.Parallel()
	if !NotYetVisible(jetstream.ErrConsumerNotFound) {
		t.Error("a durable consumer that has not propagated is not recognised, " +
			"so its read-back gives up on the first 404")
	}
	// AND IT IS STILL NOT A PLACEMENT FAILURE: the two predicates stay
	// disjoint, or a lookup would wait for peers nobody is bringing.
	if Unplaceable(jetstream.ErrConsumerNotFound) {
		t.Error("a consumer propagation delay was treated as a placement failure")
	}
}

// A CANCELLED CALLER STOPS THE RE-ASKING AT ONCE, even though the lookups
// themselves run on a context detached from that cancellation.
//
// The read-backs deliberately survive the deadline that just expired — that is
// what they exist for — but detaching the LOOP as well meant an operator who
// cancelled a boot waited out the whole window for an answer nobody wanted.
func TestCancellingTheCallerStopsTheReAsking(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	calls := 0
	start := time.Now()
	err := Settle(ctx, func() error { calls++; return jetstream.ErrStreamNotFound })
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("reported %v, want the ask's own error", err)
	}
	if calls != 1 {
		t.Errorf("a cancelled caller was asked %d times, want 1", calls)
	}
	if waited := time.Since(start); waited > ReadBack/2 {
		t.Errorf("a cancelled caller waited %v, close to the whole %v window",
			waited, ReadBack)
	}
}
