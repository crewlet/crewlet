package sandbox

import (
	"errors"
	"testing"
)

// A CLAIM THE STORE DID NOT TAKE IS THE STORE'S SILENCE. A completion's claim is
// the first step its collection cannot go on without, and when the
// coordination store does not answer it, or this node refuses it while the
// store has yet to take the release of the job's last claim, the completion is
// handed back for the queue's backoff exactly as a collection's step the store
// did not answer is: the failure is this node's, never the message's, and the
// engine's control handler hands a store's silence back as the node's own, so a
// delivery whose budget a store outage spends is not kept as poison
// ([ErrStoreSilent], internal/engine's controlOutcome). SO IS THE RECORD OF A
// PARK'S QUESTION the store did not answer: the park follows a read the box's
// host answered, so no clock runs and nothing renews it, and its delivery,
// handed back as the message's, was kept with the poison once an outage spent
// its budget.
//
// Mutation: hand a claim's failure, or a park's record, back as it came, and
// the store's silence reads as the message's own failure.
func TestAClaimTheStoreDidNotTakeIsTheStoresSilence(t *testing.T) {
	t.Parallel()

	t.Run("a claim the store does not answer", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.runner.Finish(wholeAccount())
		rig.coordinator.pending = &unansweredClaims{PendingStore: rig.coordinator.pending, silent: 1,
			answered: func() {}}
		if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("a claim the store did not answer = %v; want it handed back as the store's silence", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Fatalf("the claim the store did not answer left the run %s; want it running", got.Status)
		}
	})

	t.Run("a claim refused while the release of the last one is owed", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.runner.Finish(wholeAccount())
		mine := &unansweredRelease{silentStore: &silentStore{PendingStore: rig.coordinator.pending,
			silent: []string{"ReleaseClaim"}}}
		rig.coordinator.pending = mine
		// THE FIRST COLLECTION'S BOX DOES NOT ANSWER, and the store does not
		// answer the claim's hand-back: this node owes its release.
		rig.runner.CollectErr = errBoxBlip
		if err := collectAgain(t, rig); err == nil {
			t.Fatal("the collection whose box did not answer was not handed back")
		}
		rig.runner.CollectErr = nil
		err := collectAgain(t, rig)
		if !errors.Is(err, errReleaseOwed) || !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("a claim while the release of the last one is owed = %v; want it refused as the store's "+
				"silence", err)
		}
	})

	t.Run("a park's question the store did not record", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.ask("t1", whichBranch())
		rig.runner.Finish(Result{})
		rig.coordinator.pending = &unansweredRelease{silentStore: &silentStore{PendingStore: rig.coordinator.pending,
			silent: []string{"MarkAwaiting"}}}
		if err := collectAgain(t, rig); !errors.Is(err, ErrStoreSilent) {
			t.Fatalf("a park whose question the store did not record = %v; want it handed back as the "+
				"store's silence", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Fatalf("the park the store did not record left the run %s; want it handed back to running",
				got.Status)
		}
	})
}
