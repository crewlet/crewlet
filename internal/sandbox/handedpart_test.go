package sandbox

import (
	"bytes"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
)

// WHAT A JOB'S BOX WAS HANDED IS KEPT BESIDE THE RUN'S ROW, FOR THAT JOB
// ALONE. The sealed set is up to 64 KiB before sealing, and the row is read
// whole by every git request of a coding box and every listing, so the set is
// a part of its own beside it, named for the job its attach wrote it for:
//
//   - the attach that starts a job writes it, and the row carries none of it;
//   - the next launch's opening hands it on as the box's ([PriorBox.Handed]),
//     and from then it is none for the new job, whose own attach writes its
//     own — never the previous job's set read as the new job's, which would
//     scrub a capture of the new job's session of values its box was not
//     handed and leave in the ones it was;
//   - a write of an earlier job that lands after a later job's never replaces
//     the later job's set.
//
// Mutation: write the set onto the row, skip the job check in
// [handedSet.of], read the set after the reset rather than before, or drop
// the ordinal check in [CoordStore.writeHanded], and a check below goes red.
func TestWhatABoxWasHandedIsKeptBesideItsRowForItsJobAlone(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	fleet := memory.NewFleet()
	store := NewCoordStore(fleet)
	fence := Fence{Owner: "n1", Epoch: 1}
	seat := uuid.NewString()

	first, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentID: seat, AgentHandle: "h",
		Harness: "claude-code"}, fence)
	if err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-1",
		Handed: "sealed-first"}, fence); err != nil {
		t.Fatalf("AttachSandbox: %v", err)
	}
	if got, err := store.Handed(ctx, first); err != nil || got != "sealed-first" {
		t.Fatalf("the first job's set reads (%q, %v), want the one its attach wrote", got, err)
	}
	row, _, err := fleet.SandboxRun(ctx, "t1")
	if err != nil {
		t.Fatalf("SandboxRun: %v", err)
	}
	if bytes.Contains(row.Value, []byte("sealed-first")) {
		t.Errorf("the run's row carries what its box was handed: %s", row.Value)
	}
	if _, found, err := fleet.SandboxRunPart(ctx, "t1", coord.RunHanded); err != nil || !found {
		t.Errorf("no handed set beside the row: (%v, %v)", found, err)
	}

	second, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentID: seat, AgentHandle: "h",
		Harness: "claude-code", Continues: true}, fence)
	if err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if second.Prior.SandboxID != "box-1" || second.Prior.Handed != "sealed-first" {
		t.Errorf("the relaunch was handed the box %+v, want box-1 with the first job's set", second.Prior)
	}
	if got, err := store.Handed(ctx, second); err != nil || got != "" {
		t.Errorf("before its attach the relaunch's set reads (%q, %v), want none: the first job's is not its own",
			got, err)
	}

	if err := store.AttachSandbox(ctx, "t1", BoxRef{SandboxID: "box-1", CommandID: "cmd-2",
		Handed: "sealed-both"}, fence); err != nil {
		t.Fatalf("AttachSandbox again: %v", err)
	}
	store.writeHanded(ctx, "t1", handedSet{Launch: first.LaunchID, LaunchSeq: first.LaunchSeq, Sealed: "late"})
	if got, err := store.Handed(ctx, second); err != nil || got != "sealed-both" {
		t.Errorf("the relaunch's set after a late write of the first job reads (%q, %v), want its own", got, err)
	}
	if got, err := store.Handed(ctx, first); err != nil || got != "" {
		t.Errorf("the first job's set reads (%q, %v) once the relaunch replaced it, want none", got, err)
	}
}
