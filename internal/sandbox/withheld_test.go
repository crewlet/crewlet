package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
)

// A RESUME IS HANDED THE CALL A PERSON APPROVED ONCE, whether or not its log
// holds it yet ([BridgeLog.withHeld]): when a refused relaunch's append never
// landed, the call is on the row alone and the round is judged on it; when it
// landed, the log holds it under its held id and it is not added twice. A held
// call not made yet, or another round's, adds nothing.
//
// Mutation: drop the held-id check from withApproved and the logged case
// holds the call twice; skip withHeld's Made check and the unmade case
// appends a call nobody made.
func TestAResumeIsHandedTheApprovedCallOnce(t *testing.T) {
	t.Parallel()
	made := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	approved := BridgeCall{Name: "tr_close_issue", Approved: true, HeldID: "held-1", At: made}
	earlier := BridgeCall{Name: "read_work_item", Seq: 4, At: made.Add(-time.Minute)}
	rowWith := func(held *HeldCall) PendingRun {
		return PendingRun{TurnID: "t1", LaunchID: "l1", Round: "r1", HeldCall: held}
	}
	logged := approved
	logged.Seq = 5

	for name, tc := range map[string]struct {
		log  BridgeLog
		held *HeldCall
		want []BridgeCall
	}{
		"made, and its append never landed": {
			log:  BridgeLog{Launch: "l1", Round: "r1", Calls: []BridgeCall{earlier}},
			held: &HeldCall{ID: "held-1", LaunchID: "l1", MadeAt: made, Call: &approved},
			want: []BridgeCall{earlier, logged},
		},
		"made, and already in the log": {
			log:  BridgeLog{Launch: "l1", Round: "r1", Calls: []BridgeCall{earlier, logged}},
			held: &HeldCall{ID: "held-1", LaunchID: "l1", MadeAt: made, Call: &approved},
			want: []BridgeCall{earlier, logged},
		},
		"held and not made yet": {
			log:  BridgeLog{Launch: "l1", Round: "r1", Calls: []BridgeCall{earlier}},
			held: &HeldCall{ID: "held-1", LaunchID: "l1", Call: &approved},
			want: []BridgeCall{earlier},
		},
		"made in another round": {
			log:  BridgeLog{Launch: "l1", Round: "r2", Calls: []BridgeCall{earlier}},
			held: &HeldCall{ID: "held-1", LaunchID: "l1", MadeAt: made, Call: &approved},
			want: []BridgeCall{earlier},
		},
		"no held call": {
			log:  BridgeLog{Launch: "l1", Round: "r1", Calls: []BridgeCall{earlier}},
			want: []BridgeCall{earlier},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := tc.log.withHeld(rowWith(tc.held))
			if len(got.Calls) != len(tc.want) {
				t.Fatalf("the resume is handed %d calls %+v, want %d %+v", len(got.Calls), got.Calls,
					len(tc.want), tc.want)
			}
			for i := range tc.want {
				if got.Calls[i].Name != tc.want[i].Name || got.Calls[i].Seq != tc.want[i].Seq ||
					got.Calls[i].HeldID != tc.want[i].HeldID {
					t.Errorf("call %d is %+v, want %+v", i, got.Calls[i], tc.want[i])
				}
			}
		})
	}
}

// AND THE COORDINATOR HANDS IT SO. The first attempt at an allow-once answer
// makes the call and records it on the row, stops before the log append lands,
// and its relaunch is refused; the later attempt's resume holds the call once,
// from the row. [ResumeRequest.BridgedRead] counts the log's calls only: the
// log takes the row's call at its own append or the opening, numbered after any
// call landing in between, which the opening marks late by this count.
//
// Mutation: drop withHeld from [Coordinator.resumeAndSettle] and the later
// attempt is judged on a round that never made its allowed call; count the
// read after adding the held call, or count none, and the count is not the
// log's.
func TestALaterAttemptAtAnAnswerIsHandedTheCallTheFirstMade(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	// ONE CALL OF THE JOB'S OWN, in its log beside the row-only call.
	launched := rig.get("t1")
	if ok, err := rig.pending.AppendBridgeCall(t.Context(), "t1", BridgeAppend{Launch: launched.LaunchID,
		Round: launched.Round, Call: BridgeCall{Name: "read_work_item", Seq: 1, StartedAt: rig.now}}); err != nil || !ok {
		t.Fatalf("AppendBridgeCall = %v, %v", ok, err)
	}
	id := parksOnAHeldCall(t, rig, "t1")

	// THE FIRST ATTEMPT makes and records the call; its relaunch is refused
	// mid-turn.
	rig.resumer.failsInTurn = true
	rig.resumer.failWith(errors.New("the relaunch's session floor refused it"))
	rig.resumer.during = func(ctx context.Context, run PendingRun) {
		fence := fenceOf(run)
		if _, ok, err := rig.pending.MarkHeldCall(ctx, "t1", run.LaunchID, fence); err != nil || !ok {
			t.Errorf("MarkHeldCall = %v, %v", ok, err)
			return
		}
		if ok, err := rig.pending.RecordHeldResult(ctx, "t1", run.LaunchID, HeldMade{Result: "sealed",
			Call: BridgeCall{Name: "tr_close_issue", At: rig.now}}, fence); err != nil || !ok {
			t.Errorf("RecordHeldResult = %v, %v", ok, err)
			return
		}
		// THE APPEND AFTER THE RESULT NEVER LANDED: the attempt stopped
		// between the two writes, so the log holds no approved call.
		if _, _, err := rig.pending.runs.MutateSandboxRunPart(ctx, "t1", coord.RunLog,
			func(current coord.Record, found bool) ([]byte, error) {
				// Kept apart: a missing record has no error to wrap, and one
				// Errorf would print %!w(<nil>).
				if !found {
					return nil, errors.New("the job opened no log")
				}
				var log BridgeLog
				if err := json.Unmarshal(current.Value, &log); err != nil {
					return nil, fmt.Errorf("decoding the job's log: %w", err)
				}
				log.Calls = slices.DeleteFunc(log.Calls, func(c BridgeCall) bool { return c.HeldID == id })
				return json.Marshal(log)
			}); err != nil {
			t.Errorf("taking the approved call's append back: %v", err)
		}
	}
	given, _ := givenAgainst(t, rig, "t1")
	given.Choice, given.Approval = HeldOnce, id
	ev := events.New(given, events.TraceContext{})
	if d, err := rig.coordinator.AnswerByTurn(t.Context(), given, ev); err != nil || d != AnswerConsumed {
		t.Fatalf("AnswerByTurn = %q, %v, want the approval consumed", d, err)
	}
	rig.fireRetries()
	if got := rig.get("t1"); got.HeldCall == nil || !got.HeldCall.Made() || got.HeldCall.Call == nil {
		t.Fatalf("the row holds %+v, want the call made and recorded on it", got.HeldCall)
	}

	// THE LATER ATTEMPT.
	rig.resumer.during = nil
	rig.resumer.failWith(nil)
	rig.fireRetries()
	requests := rig.resumer.calls()
	if len(requests) != 1 {
		t.Fatalf("%d resumes reached the turn, want the later attempt's", len(requests))
	}
	var held int
	for _, c := range requests[0].Bridged.Calls {
		if c.HeldID == id {
			held++
			if !c.Approved || c.Name != "tr_close_issue" {
				t.Errorf("the resume is handed %+v, want the approved call", c)
			}
		}
	}
	if held != 1 {
		t.Fatalf("the resume holds the approved call %d times in %+v, want once", held,
			requests[0].Bridged.Calls)
	}
	if got := requests[0].BridgedRead; got != 1 {
		t.Errorf("the resume is handed a read of %d calls, want the 1 its log took: the call on the row alone "+
			"is no call of the log's yet", got)
	}
}
