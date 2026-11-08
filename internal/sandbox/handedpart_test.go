package sandbox

import (
	"bytes"
	"errors"
	"strings"
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

// A TOKEN IS RECORDED ONLY IN THE SET OF THE JOB ITS ROW HOLDS RUNNING, before
// it is written into the job's box. The row is read first: a job its row holds
// launching, read back by a collection, lost or ending — or a poll's copy of a
// job a relaunch has replaced — takes nothing, add unasked, since an opening or
// a capture may have read its set for the last time. The part decides the rest
// by its own compare-and-swap: an answer that leaves the set as it was writes
// nothing; one that cannot hold the token marks the set, which reads as
// refusing every capture of the job, is asked nothing again and is never
// replaced by the job's own write landing late; a later job's set takes
// nothing; and a job with no set at all is marked so, add unasked, for that
// same late write.
//
// Mutation: skip the row's read, grow whatever set is beside the row, write a
// set add left as it was, leave a job with no set unmarked, ask a marked set
// again, or let the job's own write replace its mark, and a check below goes
// red.
func TestATokenIsRecordedOnlyInTheSetOfTheJobItsRowHoldsRunning(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig := newWaiterRig(t)
	asked := 0
	answer := func(next, why string) func(string) (string, string) {
		return func(string) (string, string) {
			asked++
			return next, why
		}
	}
	adds := func(run PendingRun, add func(string) (string, string)) bool {
		t.Helper()
		took, err := rig.pending.AddHanded(ctx, run, add)
		if err != nil {
			t.Fatalf("AddHanded: %v", err)
		}
		return took
	}
	version := func(turnID string) uint64 {
		t.Helper()
		record, _, err := rig.pending.runs.SandboxRunPart(ctx, turnID, coord.RunHanded)
		if err != nil {
			t.Fatalf("reading the part: %v", err)
		}
		return record.Version
	}
	marked := func(run PendingRun, why string) {
		t.Helper()
		if _, err := rig.pending.Handed(ctx, run); !errors.Is(err, ErrHandedIncomplete) ||
			!strings.Contains(err.Error(), why) {
			t.Errorf("the job's set reads (%v), want it marked: %s", err, why)
		}
	}
	setFor := func(run PendingRun, sealed string) {
		rig.pending.writeHanded(ctx, run.TurnID, handedSet{Launch: run.LaunchID, LaunchSeq: run.LaunchSeq,
			Sealed: sealed})
	}

	if adds(rig.launching("t0"), answer("sealed-grown", "")) || asked != 0 {
		t.Errorf("a launching job's set took a token (asked %d)", asked)
	}

	run := rig.launch("t1")
	setFor(run, "sealed-first")
	if !adds(run, answer("sealed-grown", "")) || asked != 1 {
		t.Fatalf("the running job's set did not take the token (asked %d)", asked)
	}
	if got, err := rig.pending.Handed(ctx, run); err != nil || got != "sealed-grown" {
		t.Fatalf("the job's set reads (%q, %v), want the one add answered", got, err)
	}
	before := version("t1")
	if !adds(run, answer("sealed-grown", "")) || version("t1") != before {
		t.Error("an answer that left the set as it was was written again")
	}

	for name, shape := range map[string]func(*PendingRun){
		"read back by a collection": func(r *PendingRun) { r.Launch.Collected = true },
		"lost":                      func(r *PendingRun) { r.Lost = &JobLoss{Cause: LossBoxGone} },
		"ending":                    func(r *PendingRun) { r.Ending = &RecordedEnding{ID: "e-1"} },
	} {
		turnID := "t-" + strings.ReplaceAll(name, " ", "-")
		job := rig.launch(turnID)
		setFor(job, "sealed-"+turnID)
		if _, _, err := rig.pending.mutateAny(ctx, turnID, func(r *PendingRun) bool {
			shape(r)
			return true
		}); err != nil {
			t.Fatalf("%s: shaping the row: %v", name, err)
		}
		asked = 0
		if adds(job, answer("sealed-grown", "")) || asked != 0 {
			t.Errorf("a job %s took a token (asked %d)", name, asked)
		}
	}

	asked = 0
	if !adds(run, answer("", handedOverflowed)) || asked != 1 {
		t.Fatalf("a set that cannot hold the token refused it (asked %d): the run is left without it", asked)
	}
	marked(run, handedOverflowed)
	if !adds(run, answer("sealed-again", "")) || asked != 1 {
		t.Errorf("a marked set was asked again (%d asks)", asked)
	}
	setFor(run, "sealed-late")
	marked(run, handedOverflowed)

	setFor(PendingRun{TurnID: "t1", LaunchID: "a-later-job", LaunchSeq: run.LaunchSeq + 1}, "sealed-later")
	asked = 0
	if adds(run, answer("sealed-grown", "")) || asked != 0 {
		t.Errorf("a later job's set took this job's token (asked %d)", asked)
	}
	if _, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t1", AgentHandle: "swe", AgentID: sweID,
		Role: "SWE", Harness: "claude-code", Continues: true}, rigLease); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if adds(run, answer("sealed-grown", "")) || asked != 0 {
		t.Errorf("a poll's copy of a job its relaunch replaced took a token (asked %d)", asked)
	}

	unkept := rig.launch("t2")
	if !adds(unkept, answer("sealed-grown", "")) || asked != 0 {
		t.Fatalf("a job with no set refused the token, or asked add of a set it has not (asked %d)", asked)
	}
	marked(unkept, handedUnkept)
	setFor(unkept, "sealed-late")
	marked(unkept, handedUnkept)
}
