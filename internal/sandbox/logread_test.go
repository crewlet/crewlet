package sandbox

import (
	"slices"
	"testing"
)

// THE COLLECTION'S READ NAMES ITS ROUND AND WHAT THE LOG HAD ABSORBED, beside
// how many calls it had taken ([ResumeRequest.BridgedRead]): the opening of a
// send-back's new round carries only what the log took after that read, of
// that round alone, and starts its absorbed tally with only what the log
// absorbed after it, the read's part being what this collection seeds.
//
// Mutation: build the read with no round, or with no absorbed calls, and this
// goes red.
func TestTheCollectionsReadNamesItsRoundAndWhatItHadAbsorbed(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	launched := rig.get("t1")
	worker := []ElidedCall{{Name: "read_file", Digest: "d1", Calls: 2}}
	for i, call := range []BridgeCall{
		{Name: "read_work_item", Seq: 1, StartedAt: rig.now},
		{Name: "delegate", Seq: 2, StartedAt: rig.now.Add(1)},
	} {
		var absorbed []ElidedCall
		if i == 1 {
			absorbed = worker
		}
		if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: launched.LaunchID,
			Round: launched.Round, Call: call, Absorbed: absorbed}); err != nil || !ok {
			t.Fatalf("AppendBridgeCall(%s) = %v, %v", call.Name, ok, err)
		}
	}
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	requests := rig.resumer.calls()
	if len(requests) != 1 {
		t.Fatalf("%d resumes reached the turn, want the collection's", len(requests))
	}
	read := requests[0].BridgedRead
	if read.Round != launched.Round || read.Took != 2 {
		t.Errorf("the collection's read is of round %q through %d calls, want round %q through the 2 its "+
			"log took", read.Round, read.Took, launched.Round)
	}
	if !slices.EqualFunc(read.Absorbed, worker, func(a, b ElidedCall) bool {
		return a.Name == b.Name && a.Digest == b.Digest && a.Calls == b.Calls
	}) {
		t.Errorf("the collection's read absorbed %+v, want the worker's %+v", read.Absorbed, worker)
	}
}
