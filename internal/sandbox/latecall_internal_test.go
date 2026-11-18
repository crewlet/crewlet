package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
)

// A LONG RUN'S LOG KEEPS A LATE CALL WHILE AN ORDINARY ONE IS LEFT. Past
// [MaxBridgeCalls] the log drops from its middle, and a dropped call keeps
// only its identity, not the [BridgeCall.Late] mark every continuation of the
// round is told. So the middle gives up a late call, as it does a call that
// ended its job, only once no ordinary call is left there.
//
// Mutation: drop the middle's oldest ordinary call whatever its mark, and the
// late call is lost.
func TestALongRunsLogKeepsALateCallWhileAnOrdinaryOneIsLeft(t *testing.T) {
	t.Parallel()
	var (
		calls  []BridgeCall
		elided int
	)
	add := func(call BridgeCall) {
		call.Seq = len(calls) + elided + 1
		calls, elided, _ = appendBounded(calls, elided, call)
	}
	for i := range MaxBridgeCalls {
		call := BridgeCall{Name: fmt.Sprintf("read_%d", i)}
		if i == MaxBridgeCalls/2+10 {
			call = BridgeCall{Name: "delegate", Late: true}
		}
		add(call)
	}
	for range MaxBridgeCalls {
		add(BridgeCall{Name: "read_file"})
	}
	if len(calls) != MaxBridgeCalls || elided != MaxBridgeCalls {
		t.Fatalf("the log holds %d calls with %d elided, want %d and %d",
			len(calls), elided, MaxBridgeCalls, MaxBridgeCalls)
	}
	kept := false
	for _, call := range calls {
		kept = kept || (call.Name == "delegate" && call.Late)
	}
	if !kept {
		t.Error("the log dropped a late call from its middle while ordinary calls were left there")
	}
}

// A LATE CALL'S MARK SURVIVES A BUILD THAT DOES NOT KNOW IT. Mid-upgrade an
// older build rewrites the run's log whole on every append, keeping a key it
// does not know in the call's Extra and writing it back verbatim (wirekeys.go).
// So the mark is a wire key of its own, `late`, which a decode declaring every
// other key of the call carries back for this build to read.
//
// Mutation: write the mark under no wire key (json:"-"), and the log never
// carries it.
func TestALateCallsMarkSurvivesABuildThatDoesNotKnowIt(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(BridgeCall{Name: "delegate", Seq: 2, Appended: 1, Late: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"late":true`) {
		t.Fatalf("the call is written %s, with no mark under the key an older build carries", raw)
	}

	// THE OLDER BUILD'S DECODE AND ENCODE: this build's codec, declaring every
	// key of the call but the mark's, into a call that holds no mark.
	type plain BridgeCall
	older := maps.Clone(wireKeys(reflect.TypeFor[BridgeCall]()))
	delete(older, "late")
	var held plain
	extra, err := unmarshalCarrying(raw, &held, older)
	if err != nil {
		t.Fatal(err)
	}
	held.Late = false
	rewritten, err := marshalCarrying(held, older, extra)
	if err != nil {
		t.Fatal(err)
	}

	var back BridgeCall
	if err := json.Unmarshal(rewritten, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Late || back.Appended != 1 || back.Name != "delegate" {
		t.Errorf("an older build's rewrite reads back as %+v (from %s), want the late call it was",
			back, rewritten)
	}
}

// A LAUNCH NAMES ITS JOB'S ROUND BESIDE THE JOB, so a call still running when
// a relaunch carries the round on lands in the round's log, marked late
// ([BridgeCall.Late]), rather than dropped. A job carrying a round on is told
// the asking job's round, and a job starting a new round its own, as the row
// the launch opened names them.
//
// Mutation: name the job without its round, and this goes red.
func TestALaunchNamesItsJobsRound(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	var told []OpenedJob
	opened := func(job OpenedJob) (Opening, error) {
		told = append(told, job)
		return Opening{}, nil
	}
	asking := askAndAnswerAfter(t, rig, false, nil, func(ctx context.Context, _ PendingRun, asked *AskedRun) {
		req := continuing(asked)
		req.Opened = opened
		if _, err := rig.launchVia(ctx, rig.manager, req); err != nil {
			t.Errorf("relaunch: %v", err)
		}
	})
	row := rig.get("t1")
	if len(told) != 1 || told[0].LaunchID != row.LaunchID || told[0].Round != asking.LaunchID {
		t.Fatalf("the carried-on job %q was told %+v, want the round the asking job %q opened",
			row.LaunchID, told, asking.LaunchID)
	}

	req := launchReq("t1")
	req.Opened = opened
	if _, err := rig.launchVia(t.Context(), rig.manager, req); err != nil {
		t.Fatalf("the next round's launch: %v", err)
	}
	row = rig.get("t1")
	if len(told) != 2 || told[1].Round != row.LaunchID || row.Round != row.LaunchID {
		t.Fatalf("the next round's job %q (row round %q) was told %+v, want a round of its own",
			row.LaunchID, row.Round, told[1:])
	}
}

// A LATE CALL THE MIDDLE DROPS IS COUNTED LATE. A call [appendBounded] drops
// keeps no mark, and a save point past its number would hold it, so the log
// counts each late call it drops ([BridgeLog.LateElided]) for every
// continuation of the round to be told whatever its save point. An ordinary
// call it drops is not counted.
//
// Mutation: drop a late call without counting it, and the log keeps no record
// that a late call is among the dropped.
func TestALateCallTheMiddleDropsIsCountedLate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		late bool
		want int
	}{
		{"a middle of late calls alone", true, 1},
		{"a middle of ordinary calls", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var l BridgeLog
			for i := range MaxBridgeCalls + 1 {
				call := BridgeCall{Name: fmt.Sprintf("read_%d", i), Seq: i + 1}
				call.Late = tc.late && i >= MaxBridgeCalls/2
				l.appendCall(call, nil)
			}
			if l.Elided != 1 || l.LateElided != tc.want {
				t.Fatalf("the log dropped %d calls and counts %d of them late, want 1 and %d", l.Elided,
					l.LateElided, tc.want)
			}
		})
	}
}

// A LOG'S COUNT OF ITS DROPPED LATE CALLS SURVIVES A BUILD THAT DOES NOT KNOW
// IT, as a call's mark does (TestALateCallsMarkSurvivesABuildThatDoesNotKnowIt):
// the count is a wire key of its own, `late_elided`, which an older build's
// decode declaring every other key of the log carries back in its Extra.
//
// Mutation: write the count under no wire key (json:"-"), and the log never
// carries it.
func TestALogsCountOfItsDroppedLateCallsSurvivesABuildThatDoesNotKnowIt(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(BridgeLog{Launch: "l1", Round: "r1", Elided: 3, LateElided: 2})
	if err != nil {
		t.Fatal(err)
	}
	type plain BridgeLog
	older := maps.Clone(wireKeys(reflect.TypeFor[BridgeLog]()))
	delete(older, "late_elided")
	var held plain
	extra, err := unmarshalCarrying(raw, &held, older)
	if err != nil {
		t.Fatal(err)
	}
	held.LateElided = 0
	rewritten, err := marshalCarrying(held, older, extra)
	if err != nil {
		t.Fatal(err)
	}
	var back BridgeLog
	if err := json.Unmarshal(rewritten, &back); err != nil {
		t.Fatal(err)
	}
	if back.LateElided != 2 || back.Elided != 3 {
		t.Errorf("an older build's rewrite reads back as %+v (from %s), want its 2 dropped late calls of 3",
			back, rewritten)
	}
}
