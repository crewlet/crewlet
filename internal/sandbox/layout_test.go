package sandbox

import "testing"

// EVERY READ OF A JOB IS HANDED THE LAYOUT ITS LAUNCH DECLARED. The runner
// that started the job says where its output landed, the launch writes that
// onto the job's own record, and the poll and the collection — on whichever
// node, after whatever restart — read the box by it. Read by this build's
// layout instead, a box reused across a rolling upgrade had its previous
// job's stream collected as the new job's transcript.
//
// Mutation: rebuild the handle from the command and session alone, and the
// reads are handed layout zero.
func TestEveryReadOfAJobIsHandedTheLayoutItsLaunchDeclared(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.runner.Layout = 3
	if _, err := Launch(t.Context(), rig.manager, rig.pending, rig.queue, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	rig.suspend("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	if got := rig.get("t1").Handle().Layout; got != 3 {
		t.Fatalf("the row hands back layout %d; want the 3 the launch declared", got)
	}

	// The poll, then the collection its completion drives.
	if _, err := rig.waiter.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rig.runner.Finish(Result{Success: true, Text: "Outcome: succeeded"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	handed := rig.runner.Handed()
	if len(handed) < 2 {
		t.Fatalf("the runner was handed %d handles; want the poll's and the collection's", len(handed))
	}
	for i, h := range handed {
		if h.Layout != 3 || h.CommandID == "" {
			t.Errorf("read %d was handed %+v; want the job's command and layout 3", i, h)
		}
	}
}
