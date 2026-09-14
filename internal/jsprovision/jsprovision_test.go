package jsprovision

import (
	"errors"
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

// PEERS ARE WHAT MAKES A NODE CLUSTERED, and one replica is not peers. Both
// callers ask this of their own config, so a disagreement about where the
// boundary sits would give one subsystem the solo budget and the other the
// clustered one on the same node.
func TestOnlyMoreThanOneReplicaIsClustered(t *testing.T) {
	t.Parallel()
	for _, replicas := range []int{0, 1} {
		if Clustered(replicas) {
			t.Errorf("%d replicas reported as clustered", replicas)
		}
	}
	for _, replicas := range []int{2, 3, 5} {
		if !Clustered(replicas) {
			t.Errorf("%d replicas reported as solo", replicas)
		}
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
