package sandbox

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A JOB'S OWED CALLS ARE CARRIED BEFORE ITS RECORD IS READ. A job's last call —
// its submission — has no later call of its own to carry its record onto the
// row when its own append met a store's blip, and the job's bridge session
// lives until its run settles or its next job replaces it, both after the
// collection has read what the job did. So once the job's box is read back the
// collection asks the job's session to carry what it still owes
// ([CoordinatorOptions.Settle]), and where it carried something reads the
// job's calls again: the resumed turn judges the round on the submission the
// run made, which the claim's read of the row did not hold.
//
// Mutation: collect without asking, or without reading the job's calls again
// after a carry, and the resume judges a run that submitted as one that did
// not.
func TestAJobsOwedCallsAreCarriedBeforeItsRecordIsRead(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	var (
		mu    sync.Mutex
		asked []string
	)
	rig.coordinator.settle = func(ctx context.Context, runID, launchID string) bool {
		mu.Lock()
		asked = append(asked, runID+"/"+launchID)
		mu.Unlock()
		recorded, err := rig.pending.AppendBridgeCall(ctx, runID, BridgeAppend{Launch: launchID,
			Owed: []OwedAppend{{Call: BridgeCall{Name: SubmitWorkTool, Seq: 1, StartedAt: rig.now,
				Args: `{"outcome":"delivered"}`, Digest: strings.Repeat("d", 64)}}}})
		if err != nil {
			t.Errorf("the carry: %v", err)
		}
		return recorded
	}

	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"t1/" + payload.LaunchID}; !slices.Equal(asked, want) {
		t.Errorf("the collection asked %v to carry what they owed, want %v: the job it collected", asked, want)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 {
		t.Fatalf("resumed %d times, want the turn back with what its run did", len(calls))
	}
	if !slices.ContainsFunc(calls[0].Run.BridgeCalls, func(c BridgeCall) bool {
		return c.Name == SubmitWorkTool && c.Seq == 1
	}) {
		t.Errorf("the resume judges the run on %+v, missing the submission its session carried",
			calls[0].Run.BridgeCalls)
	}
}

// AND A LOST JOB'S, BEFORE ITS ROUND IS CARRIED ON. A job lost with its host is
// carried on by the round's next job, whose launch ends the lost job's session
// where it lives — by which time the record holds the next job and takes
// nothing of this one's. So the carry is asked for before the round is carried
// on, and the resume reads what the lost job did, its last calls included: a
// round whose job had submitted is collected from its calls rather than carried
// on, which a submission missing from the record turns into a round run again.
//
// Mutation: carry a lost job's round on without asking, or without reading the
// job's calls again after a carry, and the resume reads a job that submitted as
// one that did not.
func TestALostJobsOwedCallsAreCarriedBeforeItsRoundIsCarriedOn(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	var (
		mu    sync.Mutex
		asked []string
	)
	rig.coordinator.settle = func(ctx context.Context, runID, launchID string) bool {
		mu.Lock()
		asked = append(asked, runID+"/"+launchID)
		mu.Unlock()
		recorded, err := rig.pending.AppendBridgeCall(ctx, runID, BridgeAppend{Launch: launchID,
			Owed: []OwedAppend{{Call: BridgeCall{Name: SubmitWorkTool, Seq: 1, StartedAt: rig.now,
				Args: `{"outcome":"delivered"}`, Digest: strings.Repeat("d", 64)}}}})
		if err != nil {
			t.Errorf("the carry: %v", err)
		}
		return recorded
	}
	rig.launch("t1")
	rig.runner.Lose(Result{Text: "submitted, and then the host went"})
	if fired := rig.tick(); fired != 1 {
		t.Fatalf("announced %d completions for a lost job, want 1", fired)
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"t1/" + payload.LaunchID}; !slices.Equal(asked, want) {
		t.Errorf("the lost job's collection asked %v to carry what they owed, want %v", asked, want)
	}
	calls := rig.resumer.calls()
	if len(calls) != 1 || calls[0].Lost == nil {
		t.Fatalf("resumed %+v, want the lost job's round carried on once", calls)
	}
	if !slices.ContainsFunc(calls[0].Run.BridgeCalls, func(c BridgeCall) bool {
		return c.Name == SubmitWorkTool && c.Seq == 1
	}) {
		t.Errorf("the resume reads the lost job's calls as %+v, missing the submission its session carried",
			calls[0].Run.BridgeCalls)
	}
}

// AND A LOST TURN'S END IS DECIDED ON THEM. A run's ending closes its session
// before it decides how the turn ended ([CoordinatorOptions.Ended]), and the
// close carries onto the row whatever the session still owed it — a reply
// whose own record met a store's blip; the end of a turn its run was lost
// under is decided by whether the turn's calls replied, so it reads them as
// the row holds them after that carry, never as the ending first read them.
//
// Mutation: decide the lost turn's end on the calls the ending read before the
// session's close, and a turn that replied is recorded as one that did not.
func TestALostTurnsEndIsDecidedOnTheCallsItsSessionCarried(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	spy := watchLosses(rig)
	rig.launchAsked("t1")
	rig.coordinator.ended = func(turnID string) {
		// THE SESSION'S END, carrying the reply it still owed the job's record.
		run, found, err := rig.pending.Get(context.Background(), turnID)
		if err != nil || !found {
			t.Errorf("reading the run its session ends for: %v %v", found, err)
			return
		}
		if _, err := rig.pending.AppendBridgeCall(context.Background(), turnID, BridgeAppend{Launch: run.LaunchID,
			Owed: []OwedAppend{{Call: BridgeCall{Name: "post_message", Seq: 1, StartedAt: rig.now,
				Args: `{"channel":"C1","text":"fixed"}`, Digest: strings.Repeat("e", 64)}}}}); err != nil {
			t.Errorf("the carry: %v", err)
		}
	}

	rig.loseOnCollect(t, "t1")

	calls := spy.seen()
	if len(calls) != 1 {
		t.Fatalf("the lost run was handed over %d times, want once", len(calls))
	}
	if !slices.ContainsFunc(calls[0].run.BridgeCalls, func(c BridgeCall) bool {
		return c.Name == "post_message" && c.Seq == 1
	}) {
		t.Errorf("the lost turn's end was decided on %+v, missing the reply its session carried as it ended",
			calls[0].run.BridgeCalls)
	}
}
