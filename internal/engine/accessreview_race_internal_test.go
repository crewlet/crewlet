package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessreview"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// ANY NODE MAY WRITE A SEAT'S PENDING REVIEW (ADR-0111), so every write is
// conditional on the revision it read and a write that loses re-reads.

// racingReviews is a pending store another node writes into once, just before
// this node's first write of the kind on lands.
type racingReviews struct {
	anyFleet
	on    string
	rival func(ctx context.Context)
	once  sync.Once
}

func (r *racingReviews) race(ctx context.Context, kind string) {
	if kind == r.on {
		r.once.Do(func() { r.rival(ctx) })
	}
}

func (r *racingReviews) CreateAccessReview(ctx context.Context, handle string, value []byte) (bool, error) {
	r.race(ctx, "create")
	return r.anyFleet.CreateAccessReview(ctx, handle, value)
}

func (r *racingReviews) UpdateAccessReview(ctx context.Context, handle string, value []byte,
	revision uint64) (bool, error) {

	r.race(ctx, "update")
	return r.anyFleet.UpdateAccessReview(ctx, handle, value, revision)
}

func (r *racingReviews) DeleteAccessReview(ctx context.Context, handle string, revision uint64) (bool, error) {
	r.race(ctx, "delete")
	return r.anyFleet.DeleteAccessReview(ctx, handle, revision)
}

// putReview writes p as handle's record over whatever it holds.
func putReview(ctx context.Context, t *testing.T, store coord.Fleet, handle string, p accessreview.Pending) {
	t.Helper()
	raw, err := accessreview.EncodePending(p)
	if err != nil {
		t.Fatalf("EncodePending: %v", err)
	}
	_, revision, found, err := store.AccessReview(ctx, handle)
	if err != nil {
		t.Fatalf("AccessReview: %v", err)
	}
	var written bool
	if found {
		written, err = store.UpdateAccessReview(ctx, handle, raw, revision)
	} else {
		written, err = store.CreateAccessReview(ctx, handle, raw)
	}
	if err != nil || !written {
		t.Fatalf("the record for %s was not written (%v)", handle, err)
	}
}

// changedSeat is a running node whose CEO was told the company before the
// change, the change itself, and the change as the node measures it.
func changedSeat(t *testing.T) (*Engine, *Company, accessMeasure) {
	t.Helper()
	e, _ := aRunningNode(t)
	withReviews(t, e)
	first, turnID := dispatchTurn(t, e, capabilityCompany(t, "Ship small changes."))
	launchTurn(t, e, first, turnID)
	after := capabilityCompany(t, "Ship small changes.", "Only the SRE operates production.")
	m, owed, err := e.measureAccess(t.Context(), after, "ceo")
	if err != nil || !owed {
		t.Fatalf("the premise: the changed seat owes no review (%v)", err)
	}
	return e, after, m
}

// pendingFor is the record m measures, first measured at.
func pendingFor(m accessMeasure, at time.Time) accessreview.Pending {
	return accessreview.Pending{Baseline: m.base.Turn, BaselineAt: m.base.At, Digest: m.digest,
		ChangedAt: at, Lines: m.personLines}
}

// TWO FIRST MEASUREMENTS OF ONE CHANGE KEEP ONE: another node records the
// change first, so this node's create loses, re-reads the same change and asks
// under the wake id the other node derived.
//
// Mutation: write the first record unconditionally and the change is recorded
// twice, under two wake ids.
func TestTwoFirstMeasurementsOfAChangeKeepOne(t *testing.T) {
	t.Parallel()
	e, after, m := changedSeat(t)
	rival := pendingFor(m, time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC))
	store := &racingReviews{anyFleet: e.backends.Fleet, on: "create",
		rival: func(ctx context.Context) { putReview(ctx, t, e.backends.Fleet, "ceo", rival) }}
	pub := &captured{}
	seams := seamsOf(e, someWork, pub)
	seams.pending = store
	if err := e.reviewSeat(t.Context(), after, "ceo", seams); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	stored, found, err := e.PendingAccessReview(t.Context(), "ceo")
	if err != nil || !found || !stored.ChangedAt.Equal(rival.ChangedAt) {
		t.Fatalf("the record is %+v (%v, %v), want the change the other node recorded", stored, found, err)
	}
	want := accessreview.WakeID(accessreview.RequestFor("ceo", rival, nil, 0))
	if got := pub.got(); len(got) != 1 || accessreview.WakeID(got[0]) != want {
		t.Errorf("this node asked for %+v, want one review under the other node's wake id", got)
	}
}

// A LATE WRITER LEAVES A TOLD RECORD TOLD: this node read an earlier change,
// and while it measured the newer one another node recorded that one and its
// review turn was told it; this node's update loses, and the record keeps the
// turn that was told.
//
// Mutation: update without the revision read and the told turn is wiped, so the
// record outlives the turn and the Inbox note never ends.
func TestALateWriterLeavesAToldRecordTold(t *testing.T) {
	t.Parallel()
	e, after, m := changedSeat(t)
	earlier := pendingFor(m, time.Date(2026, 11, 8, 7, 0, 0, 0, time.UTC))
	earlier.Digest = "an earlier change"
	putReview(t.Context(), t, e.backends.Fleet, "ceo", earlier)
	told := pendingFor(m, time.Date(2026, 11, 8, 8, 0, 0, 0, time.UTC))
	told.ToldTurn, told.ToldAt = "t-review", told.ChangedAt.Add(time.Minute)
	store := &racingReviews{anyFleet: e.backends.Fleet, on: "update",
		rival: func(ctx context.Context) { putReview(ctx, t, e.backends.Fleet, "ceo", told) }}
	seams := seamsOf(e, someWork, &captured{})
	seams.pending = store
	if err := e.reviewSeat(t.Context(), after, "ceo", seams); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	stored, found, err := e.PendingAccessReview(t.Context(), "ceo")
	if err != nil || !found || stored.ToldTurn != "t-review" || !stored.ChangedAt.Equal(told.ChangedAt) {
		t.Errorf("the record is %+v (%v, %v), want the one told to t-review", stored, found, err)
	}
}

// A TURN'S END LEAVES A NEWER CHANGE: the turn told a change ends while another
// node records a newer one, so the purge of the told record loses, re-reads,
// and leaves the newer change to be reviewed.
//
// Mutation: purge without the revision read and the newer change is lost.
func TestATurnsEndLeavesANewerChange(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	newer := accessreview.Pending{Baseline: "t-review", Digest: "a newer change",
		ChangedAt: time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)}
	store := &racingReviews{anyFleet: fleet, on: "delete",
		rival: func(ctx context.Context) { putReview(ctx, t, fleet, "ceo", newer) }}
	e := &Engine{backends: &Backends{Fleet: store}}
	putReview(t.Context(), t, fleet, "ceo", accessreview.Pending{Baseline: "t-told", Digest: "the change",
		ToldTurn: "t-review", ToldAt: newer.ChangedAt.Add(-time.Hour)})
	e.endReviewOnTurnEnd(t.Context(), "ceo", "t-review")
	stored, found, err := e.PendingAccessReview(t.Context(), "ceo")
	if err != nil || !found || stored.Digest != newer.Digest || stored.ToldTurn != "" {
		t.Errorf("the record is %+v (%v, %v), want the newer change untold", stored, found, err)
	}
}

// A CHANGE REVERTED AND MADE AGAIN WITHIN ONE QUIET WINDOW IS REVIEWED AGAIN:
// the review asked for the change finds it reverted at dispatch and is acked
// without a turn, which drops the untold record, so the redo is measured
// afresh under a wake id of its own.
//
// Mutation: ack the settled review without dropping the record and the redo
// matches it, so no review is asked for it.
func TestASettledReviewDropsItsUntoldRecord(t *testing.T) {
	t.Parallel()
	e, after, _ := changedSeat(t)
	before := capabilityCompany(t, "Ship small changes.")
	pub := &captured{}
	seams := seamsOf(e, someWork, pub)
	if err := e.reviewSeat(t.Context(), after, "ceo", seams); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	if !e.reviewSettled(t.Context(), before, "ceo") {
		t.Fatal("the premise: the review of a reverted change reads as unsettled")
	}
	if err := e.reviewSeat(t.Context(), after, "ceo", seams); err != nil {
		t.Fatalf("reviewSeat: %v", err)
	}
	got := pub.got()
	if len(got) != 2 || accessreview.WakeID(got[0]) == accessreview.WakeID(got[1]) {
		t.Fatalf("a change, its settled review and the change again asked for %d reviews, want 2 under two wakes",
			len(got))
	}
}

// A REMOVED SEAT LEAVES NO REVIEW BEHIND, so a seat added later under its
// handle starts with none; only the fleet's current company clears one.
//
// Mutation: drop the sweep and the removed seat's record is the new seat's.
func TestARemovedSeatLeavesNoReviewBehind(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
	for _, handle := range []string{"ceo", "ghost"} {
		putReview(t.Context(), t, fleet, handle, accessreview.Pending{Baseline: "t-told", Digest: "d"})
	}
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman, Source: "api", CreatedAt: time.Now()}
	earlier, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	current, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	next := companyFor(t, `
name: Acme
roles:
  - name: CEO
    handle: ceo
`)
	kept := func() []string {
		revisions, err := fleet.AccessReviewRevisions(t.Context())
		if err != nil {
			t.Fatalf("AccessReviewRevisions: %v", err)
		}
		return sortedHandles(revisions)
	}
	next.ActivatedAt = earlier.At
	e.clearRemovedSeatReviews(t.Context(), next)
	if got := kept(); len(got) != 2 {
		t.Fatalf("a company the fleet has moved past cleared reviews: %v", got)
	}
	next.ActivatedAt = current.At
	e.clearRemovedSeatReviews(t.Context(), next)
	if got := kept(); len(got) != 1 || got[0] != "ceo" {
		t.Errorf("reviews kept for %v, want only the seat the company still has", got)
	}
}
