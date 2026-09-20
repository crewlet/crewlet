package jsprovision

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
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
	const fleetBuckets = 14

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
	err := Settle(t.Context(), func(context.Context) error {
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
	if err := Settle(t.Context(), func(context.Context) error {
		calls++
		return placement
	}); !errors.Is(err, placement) {
		t.Errorf("a placement failure came back as %v", err)
	}
	if calls != 1 {
		t.Errorf("a terminal error was re-asked %d times", calls)
	}

	// AND AN OBJECT THAT NEVER APPEARS reports the broker's own not-found,
	// which names it, rather than a bare deadline.
	err = Settle(t.Context(), func(context.Context) error { return jetstream.ErrStreamNotFound })
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

// A CANCELLED CALLER STOPS THE RE-ASKING AT ONCE.
//
// Which is also exactly why a caller must hand this the context that bounds
// its BOOT and never the one that bounds its create: given a deadline that has
// already expired — which is the state the timeout half of a peer race leaves
// it in — this asks once, finds the window closed and gives up, so the
// re-asking is dead on the very paths it was written for.
func TestCancellingTheCallerStopsTheReAsking(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	calls := 0
	start := time.Now()
	err := Settle(ctx, func(context.Context) error {
		calls++
		return jetstream.ErrStreamNotFound
	})
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

// THE WINDOW BOUNDS THE LOOKUPS, NOT ONLY THE GAPS BETWEEN THEM — and what
// comes back at the end of it still names the object.
//
// # Why both halves are one case
//
// Because the second is what the first costs if nobody arranges it. A window
// the CALLER built separately left each attempt free to block past it: the one
// metadata read that hangs is exactly what is slow on a group that has not
// settled, so the function documented as bounded by [ReadBack] would sit there
// until the boot's own context expired.
//
// Bounding the attempts fixes that and introduces the other half: the attempt
// the window interrupts comes back with a DEADLINE, and a deadline says
// nothing about the object. `stream not found` is what tells an operator which
// stream; `context deadline exceeded` sends them to look at the cluster.
func TestTheWindowBoundsEachLookupAndTheAnswerStillNamesTheObject(t *testing.T) {
	t.Parallel()

	// EVERY ATTEMPT CARRIES A DEADLINE, and it is this function's window
	// rather than the caller's: the caller here has none at all.
	var seen time.Time
	if err := Settle(t.Context(), func(ctx context.Context) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("an attempt ran with no deadline of its own, so one " +
				"blocked lookup outlives the whole window")
		}
		seen = deadline
		return nil
	}); err != nil {
		t.Fatalf("a visible object reported %v", err)
	}
	if got := time.Until(seen); got > ReadBack {
		t.Errorf("an attempt got %v, more than the %v window it runs inside", got, ReadBack)
	}

	// AND THE ANSWER IS THE OBJECT'S. This ask says not-found, then
	// blocks until the window closes and reports what its context says —
	// which is the shape a real metadata read takes when the group is
	// slow. The not-found is the last thing anything said about the
	// stream, so it is what comes back.
	calls := 0
	err := Settle(t.Context(), func(ctx context.Context) error {
		calls++
		if calls == 1 {
			return jetstream.ErrStreamNotFound
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an absent object reported this function's own patience (%v) "+
			"rather than the not-found that names it", err)
	}
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("reported %v, want the broker's own not-found", err)
	}
}

// A CALLER'S OWN DEADLINE STILL WINS when it is shorter than the window.
//
// [context.WithTimeout] only ever shortens, and this is the property that
// makes a sequence ceiling mean anything: a read-back that could extend past
// the budget its caller is working inside would be a hole in every aggregate
// bound above it.
func TestTheCallersDeadlineWinsWhenItIsShorter(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := Settle(ctx, func(context.Context) error { return jetstream.ErrStreamNotFound })
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("reported %v, want the ask's own error", err)
	}
	if waited := time.Since(start); waited >= ReadBack {
		t.Errorf("a caller with a %v deadline was held for %v, the whole %v window",
			20*time.Millisecond, waited, ReadBack)
	}
}

// THE TWO PREDICATES ARE DISJOINT, which is what lets a create failure be
// routed to exactly one of waiting for peers and waiting for propagation.
//
// # Why that matters at the create sites
//
// A create that came back [Unplaceable] was REFUSED by the metadata leader, so
// nothing was placed and nothing can become visible: a read-back there spends
// the whole window asking after an object nobody made, and then appends a
// not-found to an error that already said what was wrong. Every create site
// therefore returns an unplaceable error before the read-back — and if these
// predicates overlapped, that gate would swallow the propagation case the
// read-back exists for.
func TestPlacementAndPropagationAreDisjoint(t *testing.T) {
	t.Parallel()

	placement := &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400}
	if !Unplaceable(placement) {
		t.Fatal("a placement refusal is not recognised as one")
	}
	if NotYetVisible(placement) {
		t.Error("a placement refusal reads as a propagation delay, so a create " +
			"the cluster refused would be waited out as if the object existed")
	}

	for _, absent := range []error{
		jetstream.ErrStreamNotFound,
		jetstream.ErrBucketNotFound,
		jetstream.ErrConsumerNotFound,
	} {
		if !NotYetVisible(absent) {
			t.Errorf("%v is not recognised as a propagation delay, so its "+
				"read-back gives up on the first 404", absent)
		}
		if Unplaceable(absent) {
			t.Errorf("%v reads as a placement refusal, so the gate before the "+
				"read-back would return it instead of waiting it out", absent)
		}
	}
}

// A PLACEMENT REFUSAL IS WAITED OUT, and the answer at the end names it.
//
// # Why this is one helper and not four loops
//
// Because three of the four creates had the loop and the fourth did not, and
// the fourth was a durable consumer — placed by the same metadata group, on
// the same stream, at the same moment of the same boot as the stream that had
// it. So the clustered budget bought the consumer creates nothing: the single
// condition the budget exists to wait out was the condition they returned on
// immediately, and a node could fail its boot on "no suitable peers" while the
// group it was asking was still forming.
func TestAPlacementRefusalIsWaitedOut(t *testing.T) {
	t.Parallel()

	placement := &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400}

	// IT CLEARS: the members arrive and the create succeeds.
	calls, announced := 0, 0
	err := Place(t.Context(), time.Minute, func(context.Context) error {
		calls++
		if calls < 3 {
			return placement
		}
		return nil
	}, func() { announced++ })
	if err != nil {
		t.Errorf("a create that succeeded on the third attempt reported %v", err)
	}
	if calls != 3 {
		t.Errorf("tried %d times, want 3", calls)
	}
	// ANNOUNCED ONCE, not per poll: the useful facts are that this
	// started and whether it ended.
	if announced != 1 {
		t.Errorf("said it was waiting %d times, want exactly one line", announced)
	}

	// ANYTHING ELSE IS TERMINAL AT ONCE. Waiting out a bad configuration
	// turns a mistake into a two-minute hang with the same message at the
	// end.
	calls = 0
	bad := errors.New("invalid consumer config")
	if err := Place(t.Context(), time.Minute, func(context.Context) error {
		calls++
		return bad
	}, nil); !errors.Is(err, bad) {
		t.Errorf("a terminal create error came back as %v", err)
	}
	if calls != 1 {
		t.Errorf("a terminal error was retried %d times", calls)
	}

	// AND A BUDGET THAT RUNS OUT REPORTS THE REFUSAL, not its own
	// deadline: "no suitable peers" names the condition to go and look
	// at, and "deadline exceeded" names nothing.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = Place(ctx, time.Minute, func(context.Context) error { return placement }, nil)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("an exhausted budget reported its own deadline (%v) rather "+
			"than the refusal that explains it", err)
	}
	if !errors.Is(err, placement) {
		t.Errorf("reported %v, want the placement refusal", err)
	}
}

// SILENCE IS NOT AN ANSWER, and telling it from one is what stops a slow
// metadata group failing a boot.
//
// This is [internal/coord]'s three-valued rule at the broker: "it is there",
// "it is not there" and "nobody said" are three facts. Every provisioning path
// here used to read the last two as one, so a member whose existence probe
// went unanswered reported `ensure stream CREWLET_CONFIG: context deadline
// exceeded` and refused to start — on a cluster where nothing was wrong.
func TestSilenceIsNotAnAnswer(t *testing.T) {
	t.Parallel()

	for _, err := range []error{
		context.DeadlineExceeded,
		nats.ErrTimeout,
		nats.ErrNoResponders,
		// WRAPPED, because that is how it arrives: every caller adds
		// the object's name before anything sees it.
		fmt.Errorf("ensure stream CREWLET_CONFIG: %w", context.DeadlineExceeded),
		errors.Join(errors.New("open crewlet_channels"), nats.ErrNoResponders),
	} {
		if !Unanswered(context.Background(), err) {
			t.Errorf("%v was read as an answer: a broker that did not reply "+
				"has not said the object is absent", err)
		}
	}

	for _, err := range []error{
		nil,
		// A CANCELLED CONTEXT IS THE CALLER GIVING UP, not the broker
		// staying quiet. Falling through to a create on it would spend
		// a doomed round trip arguing with a decision already made.
		context.Canceled,
		fmt.Errorf("shutting down: %w", context.Canceled),
		// THESE ARE ANSWERS. "Not found" is the broker saying so, and a
		// placement refusal is it saying why it will not act.
		jetstream.ErrStreamNotFound,
		jetstream.ErrBucketNotFound,
		jetstream.ErrConsumerNotFound,
		&jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400,
			Description: "no suitable peers for placement"},
		errors.New("nats: authorization violation"),
	} {
		if Unanswered(context.Background(), err) {
			t.Errorf("%v was read as silence: it is an answer, and carrying on "+
				"to a create would turn it into a second round trip ending in "+
				"a worse message", err)
		}
	}
}

// THE THREE PREDICATES NAME THREE DIFFERENT FACTS, and no error is two of
// them.
//
// Each has its own remedy: [Unplaceable] is waited out by [Place] because more
// members may arrive, [NotYetVisible] is re-asked by [Settle] because the
// object exists and this member has not seen it yet, and [Unanswered] falls
// through to the create because nothing was learned at all. An error matching
// two would take whichever branch was written first, which is how a rule with
// three cases decays into one with two.
func TestTheProvisioningPredicatesAreDisjoint(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want [3]bool // Unplaceable, NotYetVisible, Unanswered
	}{
		{"placement refusal", &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400},
			[3]bool{true, false, false}},
		// A STORAGE CLAUSE INSIDE THE SAME CODE IS STILL PLACEMENT —
		// see [TestAPlacementRefusalIsWaitedOutWhateverItsReason] for
		// why the prose does not reclassify it.
		{"placement refusal naming storage", &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400,
			Description: "no suitable peers for placement, insufficient storage"},
			[3]bool{true, false, false}},
		// AND A RESERVATION THE BROKER REFUSED IS NONE OF THE THREE: it
		// is terminal, and [OutOfCapacity] is the predicate for it.
		{"refused reservation", &jetstream.APIError{ErrorCode: errCodeOutOfStore, Code: 500,
			Description: "insufficient storage resources available"},
			[3]bool{false, false, false}},
		{"stream not found", jetstream.ErrStreamNotFound, [3]bool{false, true, false}},
		{"bucket not found", jetstream.ErrBucketNotFound, [3]bool{false, true, false}},
		{"consumer not found", jetstream.ErrConsumerNotFound, [3]bool{false, true, false}},
		{"deadline", context.DeadlineExceeded, [3]bool{false, false, true}},
		{"nats timeout", nats.ErrTimeout, [3]bool{false, false, true}},
		{"no responders", nats.ErrNoResponders, [3]bool{false, false, true}},
		{"a real refusal", errors.New("nats: authorization violation"),
			[3]bool{false, false, false}},
	} {
		got := [3]bool{Unplaceable(tc.err), NotYetVisible(tc.err), Unanswered(context.Background(), tc.err)}
		if got != tc.want {
			t.Errorf("%s: Unplaceable/NotYetVisible/Unanswered = %v, want %v",
				tc.name, got, tc.want)
		}
	}
}

// A LOOKUP IS SIZED BY THE SERVER'S OWN HOLD WINDOW, not by the create it
// precedes.
//
// The two shared one deadline, so a lookup nobody answered spent the whole
// clustered [Budget] and left none of it for the create that would have
// settled the question. The anchor is nats-server's own raft timing: an
// election runs to maxElectionTimeout, a group notices lost quorum after
// lostQuorumInterval and re-checks on lostQuorumCheckInterval. Past their sum
// the server has itself decided, so a request still unanswered is one that
// waiting will not answer.
//
// The three constants are spelled here for the reason clusterReadyTimeout is
// spelled above: a Go test cannot reach a vendored package's unexported
// constants, and a change to them must be reflected here.
func TestALookupIsSizedByTheServersHoldWindow(t *testing.T) {
	t.Parallel()

	const (
		maxElectionTimeout      = 9 * time.Second
		lostQuorumInterval      = 10 * time.Second
		lostQuorumCheckInterval = 10 * time.Second
	)
	hold := maxElectionTimeout + lostQuorumInterval + lostQuorumCheckInterval

	if LookupBudget < hold {
		t.Errorf("a lookup gets %v, which is less than the %v the server may "+
			"hold a metadata request for — so a probe can be abandoned while "+
			"the thing that would have answered it is still running",
			LookupBudget, hold)
	}

	// AND IT IS SHORTER THAN A CREATE'S, which is the whole point: a read
	// that waits as long as a raft round trip is a read nobody sized.
	if LookupBudget >= Budget(true) {
		t.Errorf("a lookup gets %v and a clustered create %v — the lookup is "+
			"not cheaper, so splitting them bought nothing", LookupBudget, Budget(true))
	}

	// AND A WHOLE BRING-UP STILL OUTLASTS ONE OF EACH. A boot that could
	// not afford a slow lookup followed by its create would fail on the
	// first object that needed both.
	for _, clustered := range []bool{false, true} {
		if got := SequenceBudget(clustered); got <= LookupBudget+Budget(clustered) {
			t.Errorf("clustered=%v: the sequence gets %v, which is not more "+
				"than one lookup (%v) plus one create (%v)",
				clustered, got, LookupBudget, Budget(clustered))
		}
	}
}

// A REQUEST NOBODY ANSWERED IS SENT AGAIN, which is the only remedy that
// works when the reply does not exist.
//
// nats-server drops a routed metadata request outright in more than one place
// — a non-leader with no assignment returns without replying, and apiDispatch
// drains its whole queue at the request limit — so the caller is waiting on a
// reply nobody will send. Waiting longer was measured buying exactly nothing,
// at two minutes a time.
func TestAnUnansweredRequestIsAskedAgain(t *testing.T) {
	t.Parallel()

	var asks atomic.Int64
	// SILENT TWICE, then answered: the shape of a group that elects a
	// leader while the caller is asking.
	err := Ask(t.Context(), 20*time.Millisecond, func(ctx context.Context) error {
		if asks.Add(1) <= 2 {
			<-ctx.Done() // the reply never comes; the term expires
			return ctx.Err()
		}
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("Ask returned %v, want the answer the third request got", err)
	}
	if got := asks.Load(); got != 3 {
		t.Errorf("the request was sent %d times, want 3 — a request that was "+
			"destroyed has to be re-issued, not waited on", got)
	}
}

// AND ANY ANSWER ENDS IT, including one the caller will not like.
//
// [Unanswered] is the only condition re-asked. An auth failure, a bad subject
// or a placement refusal is the broker having spoken, and re-sending it would
// turn a configuration mistake into a loop — and would hide the refusal
// [Place] exists to wait out.
func TestAnAnsweredRequestIsNotAskedAgain(t *testing.T) {
	t.Parallel()

	for _, answer := range []error{
		nil,
		jetstream.ErrStreamNotFound,
		errors.New("nats: authorization violation"),
		&jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400},
	} {
		var asks atomic.Int64
		got := Ask(t.Context(), time.Minute, func(context.Context) error {
			asks.Add(1)
			return answer
		}, nil)
		if !errors.Is(got, answer) {
			t.Errorf("Ask returned %v, want the answer %v", got, answer)
		}
		if n := asks.Load(); n != 1 {
			t.Errorf("answer %v was asked %d times, want 1", answer, n)
		}
	}
}

// A CALLER'S OWN DEADLINE ENDS IT TOO, rather than being read as silence.
//
// The two deadlines produce the identical error value and mean opposite
// things: a term this package set means the request is gone and the next will
// be answered; the caller's means nobody is waiting for the answer any more.
// Re-asking on the second spends requests on a context that is already done.
func TestAskStopsWhenTheCallersOwnDeadlineExpires(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()

	var asks atomic.Int64
	start := time.Now()
	err := Ask(ctx, time.Hour, func(ctx context.Context) error {
		asks.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Ask returned %v, want the deadline that actually expired", err)
	}
	// ONE REQUEST. The term is an hour, so the only thing that can have
	// ended the attempt is the caller's own ceiling — and that must not
	// look like a destroyed request.
	if got := asks.Load(); got != 1 {
		t.Errorf("the request was sent %d times after the caller's deadline "+
			"expired, want 1", got)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Ask took %v to notice the caller's deadline", elapsed)
	}
}

// AND A CREATE THE BROKER NEVER ANSWERED IS RE-ISSUED THROUGH [Place].
//
// This is the path the end-to-end failure actually died on: the create sat on
// one call for the whole clustered budget, so the placement refusal this loop
// waits for never arrived to be waited for. Routing it through [Ask] is what
// makes the budget buy attempts instead of one long silence.
func TestAPlacementLoopReissuesADestroyedCreate(t *testing.T) {
	t.Parallel()

	// THE OUTER CONTEXT IS BOUNDED TOO, so a Place that stopped giving each
	// attempt its own term fails here in seconds rather than hanging until
	// the package timeout — a mutation that hangs is one nobody diagnoses.
	//
	// Five seconds because the run below really costs about 1.3: one
	// destroyed attempt, a [ReAsk] second, a refusal, a [PlacementRetry]
	// quarter-second, then the answer.
	outer, cancelOuter := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancelOuter()

	var creates atomic.Int64
	err := Place(outer, 20*time.Millisecond, func(ctx context.Context) error {
		switch creates.Add(1) {
		case 1: // destroyed: no reply at all
			<-ctx.Done()
			return ctx.Err()
		case 2: // answered, and refusing: the condition Place waits out
			return &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400}
		default:
			return nil
		}
	}, nil)
	if err != nil {
		t.Fatalf("Place returned %v, want the create that finally landed", err)
	}
	if got := creates.Load(); got != 3 {
		t.Errorf("the create was attempted %d times, want 3 — one destroyed, "+
			"one refused, one answered", got)
	}
}

// THE SAME ERROR VALUE FROM TWO DEADLINES MEANS OPPOSITE THINGS, and only the
// parent can tell them apart.
//
// [context.DeadlineExceeded] from a term this package set means the request
// was destroyed and the next one will be answered. The identical value from
// the CALLER's own deadline — the boot's sequence ceiling, an operator's
// Ctrl-C — means time is up and nobody is waiting for the answer any more.
// Reading the second as the first sends a doomed request on a context that is
// already done, and reports whatever that produces instead of the ceiling that
// actually expired.
//
// This is the guard [Ask]'s own cases cannot provide: its select on ctx.Done()
// ends the loop either way, so the predicate has to be exercised directly.
func TestUnansweredTellsThisPackagesDeadlineFromTheCallers(t *testing.T) {
	t.Parallel()

	// A LIVE PARENT: our own term expired, so the request is gone and the
	// next one is worth sending.
	if !Unanswered(t.Context(), context.DeadlineExceeded) {
		t.Error("a deadline under a live caller was not read as a destroyed " +
			"request — this is the whole condition the re-ask exists for")
	}

	// AN EXPIRED PARENT: the caller's ceiling is what ran out. There is
	// nobody to answer to any more.
	spent, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	if Unanswered(spent, context.DeadlineExceeded) {
		t.Error("a deadline under a caller whose own deadline had passed was " +
			"read as a destroyed request: falling through on that spends a " +
			"second doomed round trip on a context that is already done")
	}

	// AND nats.go's OWN TWO ARE UNCONDITIONAL, because neither can be
	// produced by a caller's deadline — the parent has nothing to say.
	for _, err := range []error{nats.ErrTimeout, nats.ErrNoResponders} {
		if !Unanswered(spent, err) {
			t.Errorf("%v under an expired caller was not read as silence: it "+
				"is the broker's own report of no reply, not a deadline", err)
		}
	}
}

// A READ-BACK NOBODY ANSWERED IS ASKED AGAIN, and it is asked at the cadence a
// destroyed request gets rather than the one a refusal the server ANSWERED
// gets.
//
// [Settle] serves two conditions now. A not-found is an ANSWER and clears on
// this member's next metadata update, which is worth polling four times a
// second. A request nobody replied to was DESTROYED, and what has to change is
// which member holds the group — [ReAsk] is that interval, and the two must not
// share one cadence: `nats.ErrNoResponders` comes back in microseconds, so at
// the placement cadence a broker whose JetStream is not serving yet is asked
// four times a second, for each of the objects a boot provisions, which is the
// load ReAsk exists to remove.
func TestAnUnansweredReadBackIsReAskedAtItsOwnCadence(t *testing.T) {
	t.Parallel()

	// A REPLY THAT NEVER CAME, then the object. The answer is the later
	// one: silence is not "absent".
	calls := 0
	err := Settle(t.Context(), func(context.Context) error {
		calls++
		if calls < 3 {
			return nats.ErrNoResponders
		}
		return nil
	})
	if err != nil {
		t.Errorf("an object found after two unanswered asks reported %v", err)
	}
	if calls != 3 {
		t.Errorf("asked %d times, want 3", calls)
	}

	// AND THE GAP IS [ReAsk], NOT [PlacementRetry]. Two unanswered asks
	// that each return at once must still be at least one ReAsk apart:
	// measured against the elapsed time rather than the constant, so
	// swapping the cadence back is what turns this red.
	calls = 0
	start := time.Now()
	_ = Settle(t.Context(), func(context.Context) error {
		calls++
		if calls < 2 {
			return nats.ErrNoResponders
		}
		return nil
	})
	if waited := time.Since(start); waited < ReAsk {
		t.Errorf("an unanswered ask was re-issued after %v, inside the %v a "+
			"destroyed request gets; the placement cadence is for a refusal "+
			"the server answered", waited, ReAsk)
	}
}

// AND WHEN NOBODY EVER ANSWERS, THE CALLER IS TOLD THAT rather than handed
// this function's own patience.
//
// The last ask is the one the window interrupts, so it comes back as a bare
// [context.DeadlineExceeded] — which is what [Settle] promises never to
// return, and the undiagnosable shape [LookupBudget] records having produced.
// What the object last said is the honest answer, and when it never said
// anything, the silence is.
func TestAReadBackNobodyEverAnsweredNamesTheSilence(t *testing.T) {
	t.Parallel()

	// EVERY ASK UNANSWERED, and the LAST one cut off mid-flight by the
	// window rather than returning: that attempt comes back as the
	// window's own deadline, which is not an answer and must not be
	// reported as one.
	calls := 0
	err := Settle(t.Context(), func(ctx context.Context) error {
		calls++
		if calls <= 2 {
			return nats.ErrNoResponders
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, nats.ErrNoResponders) {
		t.Errorf("a read-back nobody ever answered reported %v, want the "+
			"no-responders that says the broker is not serving yet", err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the caller was handed this function's own deadline: %v", err)
	}

	// A NOT-FOUND STILL OUTRANKS IT: the object DID answer once, and what
	// it said names it.
	err = Settle(t.Context(), func(ctx context.Context) error {
		return jetstream.ErrStreamNotFound
	})
	if !errors.Is(err, jetstream.ErrStreamNotFound) {
		t.Errorf("an absent object reported %v, want its own not-found", err)
	}
}

// THE READ-BACK'S OWN ARITHMETIC, held against the window rather than restated
// in a comment.
//
// [SettleAsk] bounds ONE attempt so a request that hangs cannot spend the whole
// window, and [ReadBack] bounds the run of them. A term at or above the window
// makes the first hung ask the entire read-back, which is the single lookup
// this re-asking replaced.
func TestAReadBacksAttemptIsBoundedWellInsideItsWindow(t *testing.T) {
	t.Parallel()
	if SettleAsk >= ReadBack {
		t.Errorf("one attempt gets %v of a %v window, so a hung ask is the "+
			"whole read-back", SettleAsk, ReadBack)
	}
	// AT LEAST TWO UNANSWERED ASKS FIT, or the re-asking never happens on
	// the condition it was written for: an ask that hangs costs SettleAsk
	// and then waits ReAsk before the next one.
	if SettleAsk+ReAsk >= ReadBack {
		t.Errorf("a hung ask plus its %v pause is %v of a %v window, leaving "+
			"room for no second ask", ReAsk, SettleAsk+ReAsk, ReadBack)
	}
	// AND AN ANSWERED NOT-FOUND IS POLLED MANY TIMES OVER, which is the
	// propagation delay this window was sized for.
	if PlacementRetry*10 > ReadBack {
		t.Errorf("a %v read-back holds fewer than ten %v polls",
			ReadBack, PlacementRetry)
	}
}

// A CEILING THAT DOES NOT FIT IS TERMINAL AND IS NOT A CLUSTER STILL FORMING.
//
// The two are both create refusals from the same call, and waiting is the
// answer to exactly one of them: a forming cluster gains members, and a
// storage limit gains nothing by anybody waiting. Confusing them costs the
// whole provisioning budget and ends with the same message — and a read-back
// after it appends a not-found for an object nobody made, which is how the
// measured failure came to name `CREWLET_PAGES_LOG` twice for a limit the
// other two logs had already spent.
//
// BOTH CODES, because which one a broker answers with is decided by where its
// streams live rather than by what went wrong, so an assertion naming one
// certifies half the rule.
func TestACeilingThatDoesNotFitIsTerminal(t *testing.T) {
	t.Parallel()
	for name, refusal := range map[string]*jetstream.APIError{
		"a file-backed broker": {
			ErrorCode: errCodeOutOfStore, Code: 500,
			Description: "insufficient storage resources available",
		},
		"a memory-backed broker": {
			ErrorCode: errCodeOutOfMemory, Code: 500,
			Description: "insufficient memory resources available",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !OutOfCapacity(refusal) {
				t.Fatal("the refusal is not recognised, so a limit that will " +
					"never change is waited out for the whole provisioning " +
					"budget and then reported as a cluster that never formed")
			}
			// WRAPPED, because that is how it arrives: the caller adds
			// the stream or bucket name before anything sees it.
			if !OutOfCapacity(errors.Join(
				errors.New("ensure stream CREWLET_PAGES_LOG"), refusal)) {
				t.Fatal("a wrapped refusal is not recognised")
			}
			if Unplaceable(refusal) {
				t.Fatal("it was also read as a cluster still forming; the two " +
					"predicates are disjoint and no caller orders them")
			}
		})
	}
}

// AND A PLACEMENT REFUSAL IS NOT, WHATEVER REASON IT CARRIES.
//
// [errCodeNoPeers] is one code over every reason the metadata leader's peer
// selection accumulated, flattened into prose across the peers the group HAS
// (never a member that has not joined, which is what makes a bring-up's
// storage-only verdict one peer speaking for the rest) — and the
// room it weighed is another member's disk, which this node cannot read. So a
// storage clause there is not this node's ceiling refused: an offline peer may
// still arrive, a member whose tags do not match may still be starting, and a
// terminal message could only quote a budget that is not the one that refused.
// The unknown direction is the one that matters — a new upstream reason read
// as terminal would fail a boot that used to succeed on the retry.
func TestAPlacementRefusalIsWaitedOutWhateverItsReason(t *testing.T) {
	t.Parallel()
	for name, description := range map[string]string{
		"nothing but the prefix":  "no suitable peers for placement",
		"a peer with no room":     "no suitable peers for placement, insufficient storage",
		"a peer that may return":  "no suitable peers for placement, peer offline, insufficient storage",
		"tags that may be fixed":  "no suitable peers for placement, insufficient storage, tags not matched ['eu']",
		"a reason nobody has met": "no suitable peers for placement, some future reason",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refusal := &jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400,
				Description: description}
			if OutOfCapacity(refusal) {
				t.Errorf("%q was declared terminal, so a condition that clears "+
					"by waiting fails the boot instead", description)
			}
			if !Unplaceable(refusal) {
				t.Errorf("%q is no longer waited out", description)
			}
		})
	}
}

// AND NEITHER PREDICATE FIRES ON AN ERROR THAT IS NOT THE BROKER'S ANSWER.
func TestNeitherRefusalPredicateFiresOnAnOrdinaryError(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errors.New("insufficient storage resources available"),
		context.DeadlineExceeded,
		nats.ErrNoResponders,
		&jetstream.APIError{ErrorCode: 10023, Code: 503,
			Description: "insufficient resources"},
	} {
		if OutOfCapacity(err) {
			t.Errorf("%v was read as a ceiling the broker refused", err)
		}
		if Unplaceable(err) {
			t.Errorf("%v was read as a cluster still forming", err)
		}
	}
}

// A CREATE REFUSED FOR HAVING NO APPLICABLE LIMIT IS ITS OWN FACT.
//
// # What it was, unclassified
//
// 10120 matched neither [OutOfCapacity] nor [Unplaceable], so every create path
// fell through to the read-back it keeps for a peer that won the race: it spent
// [ReadBack] asking after an object the broker had just refused to make, found
// nothing, and appended `(and it is not there: stream not found)` to the one
// sentence that said what was actually wrong. An operator reading that goes
// looking for a missing stream on a cluster whose account never carried a limit
// for it.
//
// # And why it is not a third arm of OutOfCapacity
//
// Because the remedy differs. OutOfCapacity means the ceiling asked for does
// not fit inside a limit that exists, and its sentence names that limit, what
// is already reserved against it and `stream.store_max_bytes`. Here there is no
// limit at all: the account's are tiered and it carries none for this node's
// replica class, so those three numbers do not exist and the lever is
// `stream.replicas` or the account's own tier declarations. Folded in, the
// refusal would offer a field that changes nothing.
func TestACreateWithNoApplicableLimitIsClassifiedOnItsOwn(t *testing.T) {
	t.Parallel()
	refusal := &jetstream.APIError{ErrorCode: errCodeNoLimits, Code: 400,
		Description: "no JetStream default or applicable tiered limit present"}

	if !NoApplicableLimit(refusal) {
		t.Fatal("the refusal is not recognised, so it reaches a caller's " +
			"read-back and is reported as an object that is not there")
	}
	// WRAPPED, because that is how it arrives: the caller adds the object's
	// name before anything sees it.
	if !NoApplicableLimit(errors.Join(
		errors.New("ensure stream CREWLET_PAGES_LOG"), refusal)) {
		t.Fatal("a wrapped refusal is not recognised")
	}

	// DISJOINT FROM EVERY OTHER ANSWER THIS PATH CLASSIFIES. Each has its
	// own remedy, and an error matching two takes whichever branch was
	// written first.
	for name, matched := range map[string]bool{
		"OutOfCapacity": OutOfCapacity(refusal),
		"Unplaceable":   Unplaceable(refusal),
		"NotYetVisible": NotYetVisible(refusal),
		"Unanswered":    Unanswered(context.Background(), refusal),
	} {
		if matched {
			t.Errorf("the refusal is also %s, so it is answered with that "+
				"condition's remedy — and a limit table is not freed by "+
				"waiting, not fitted into by a smaller ceiling, and not "+
				"about to become visible", name)
		}
	}

	// AND NOTHING ELSE IS IT.
	for _, err := range []error{
		nil,
		errors.New("connection refused"),
		&jetstream.APIError{ErrorCode: errCodeNoPeers, Code: 400,
			Description: "no suitable peers for placement, insufficient storage"},
		&jetstream.APIError{ErrorCode: errCodeOutOfStore, Code: 500,
			Description: "insufficient storage resources available"},
		&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound, Code: 404},
	} {
		if NoApplicableLimit(err) {
			t.Errorf("%v was read as an account with no applicable limit, "+
				"which sends an operator to `stream.replicas` over something "+
				"else entirely", err)
		}
	}
}

// AND THE CLAUSE NAMES THE CLASS AND THE TWO LEVERS, AND NOT THE ONE THAT WOULD
// BE WRONG.
//
// The broker's own words are `no JetStream default or applicable tiered limit
// present`: no class, no field, and nothing to do about it. What an operator
// needs is which replica class was asked for — the account's limits are
// per-class and this node's number is `stream.replicas` — and that the two
// things that move are that field and the account's declarations. What they
// must NOT be offered is `stream.store_max_bytes`, which bounds an embedded
// broker and is read by nobody on the topology this refusal comes from.
func TestTheNoApplicableLimitClauseNamesTheClassAndNotTheCapacityLever(t *testing.T) {
	t.Parallel()
	got := NoApplicableLimitDetail(3)
	for _, needle := range []string{"R3", "stream.replicas", "TIERED"} {
		if !strings.Contains(got, needle) {
			t.Errorf("the clause does not mention %q:\n%s", needle, got)
		}
	}
	if strings.Contains(got, "store_max_bytes") {
		t.Errorf("the clause offers the capacity lever, which bounds an "+
			"embedded broker and is read by nobody on the external cluster "+
			"this refusal comes from:\n%s", got)
	}
	// A ZERO IS ONE REPLICA, the way the server reads it when it picks the
	// tier (server/jetstream.go, tierName) — so the class named here is the
	// class the refusal was decided against.
	if zero := NoApplicableLimitDetail(0); !strings.Contains(zero, "R1") {
		t.Errorf("an unset replica count named a class the server never "+
			"looks up:\n%s", zero)
	}
}
