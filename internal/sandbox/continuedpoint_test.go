package sandbox

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// A LAUNCH RECORDS THE POINT OF THE KEPT CAPTURE IT HANDS ITS RUN, with that
// capture's launch, where it carries its round on: in the box the capture was
// taken in, its copy kept or replaced, and in another box. The basis beside it
// is the same capture's newest restart, never that of the copy the box holds.
// The copy continued in place, the conversation's base, a send-back that opens
// a new round, and a kept capture stating no point or zero record none; and a
// relaunch's record starts with none.
//
// Mutation: write it for a launch that opens its round, write the job
// before's copy's point in place or at the attach before the command, keep it
// across BeginLaunch, write BasisRestart from the box's own copy on the reused
// branch, or take its launch from the capture's place, and a case goes red.
func TestALaunchRecordsThePointOfTheKeptCaptureItHandsItsRun(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reused, interrupted, inPlace, sendBack bool
		// unkept is a kept capture no launch continues at all
		// ([followedCapture]).
		unkept  bool
		through *int
		want    *ContinuedPoint
		restart string
	}{
		"carried on, in the box it was taken in": {reused: true, through: new(7),
			want: &ContinuedPoint{LaunchSeq: 1, Through: 7}, restart: "u-kept"},
		"carried on, the box's copy cut off and replaced": {reused: true, interrupted: true, through: new(7),
			want: &ContinuedPoint{LaunchSeq: 1, Through: 7}, restart: "u-kept"},
		"carried on, in another box": {through: new(7), want: &ContinuedPoint{LaunchSeq: 1, Through: 7},
			restart: "u-kept"},
		"the box's copy, in place":         {reused: true, inPlace: true, through: new(7), restart: "u-box"},
		"a send-back, which opens a round": {sendBack: true, through: new(7), restart: "u-kept"},
		"a kept capture at zero":           {through: new(0), restart: "u-kept"},
		"a kept capture stating no point":  {unkept: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newCoordRig(t)
			rig.coordinator.spoolDir = t.TempDir()
			rig.runner.Carries = true
			key := objstore.KeyAt(time.Now())
			// ITS PLACE, 3, IS NOT ITS LAUNCH, 1, so the point names the one.
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 3, State: CaptureLanded, Final: true,
				Interrupted: tc.interrupted, Through: tc.through, Object: key.String(),
				ObjectHash: strings.Repeat("a", 64), ObjectSize: 14, Home: "/boxes/old/home",
				ResumedCount: &ResumedCount{Tokens: 1, Restart: new("u-kept")}}
			rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, kept)},
				archives: map[string][]byte{key.String(): []byte("the transcript")}}
			// THE BOX'S OWN COPY reads another newest restart than the capture.
			opened := PendingRun{TurnID: "t1", AgentHandle: "swe", LaunchSeq: 2, Capture: &kept, Kept: &kept,
				Prior: PriorBox{Restart: new("u-box")}}
			req := continuingKept(kept)
			req.Continue = &Continuation{}
			switch {
			case tc.sendBack:
				req.Continue = nil
			case tc.inPlace:
				req.Session = nil
				req.Continue = &Continuation{SessionID: "sess-ask", Brief: "carry on in the box you left"}
			}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if (got.kind == BriefFresh) != tc.unkept {
				t.Fatalf("the launch handed a %s brief, want the case's session continued: %v", got.kind,
					!tc.unkept)
			}
			if !reflect.DeepEqual(got.point, tc.want) {
				t.Errorf("the launch hands the point %+v, want %+v", got.point, tc.want)
			}
			if !tc.unkept && deref(got.restart) != tc.restart {
				t.Errorf("the launch hands the basis %s, want %s", deref(got.restart), tc.restart)
			}
		})
	}
	t.Run("the job's record, and the next launch's", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, restore, kept := keptTurn(t)
			if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
				run.Kept.Through = new(7)
				run.Kept.ResumedCount = &ResumedCount{Tokens: 1, Restart: new("u-7")}
				return true
			}); err != nil {
				t.Fatalf("placing the kept capture: %v", err)
			}
			req := continuingKept(kept)
			req.Continue = &Continuation{}
			if _, err := launchRestoring(t, rig, restore, req); err != nil {
				t.Fatalf("relaunch: %v", err)
			}
			run := rig.get("t1")
			want := &ContinuedPoint{LaunchSeq: kept.LaunchSeq, Through: 7}
			if !reflect.DeepEqual(run.Launch.Continued, want) || deref(run.Launch.BasisRestart) != "u-7" {
				t.Fatalf("the job records the point %+v and the basis %s, want %+v and the capture's u-7",
					run.Launch.Continued, deref(run.Launch.BasisRestart), want)
			}
			if _, err := rig.pending.BeginLaunch(t.Context(), run, Fence{}); err != nil {
				t.Fatalf("the next launch: %v", err)
			}
			if next := rig.get("t1").Launch.Continued; next != nil {
				t.Errorf("the next launch's record holds the point %+v, want none until its own attach", next)
			}
		})
	})
	t.Run("the attach before the command", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		rig.launch("t1")
		if err := rig.pending.AttachSandbox(t.Context(), "t1", BoxRef{SandboxID: "box-2",
			Continued: &ContinuedPoint{LaunchSeq: 1, Through: 7}}, rigLease); err != nil {
			t.Fatalf("AttachSandbox: %v", err)
		}
		if got := rig.get("t1").Launch.Continued; got != nil {
			t.Errorf("an attach naming no command recorded the point %+v, want it written with the job's "+
				"command alone", got)
		}
	})
	t.Run("the conversation's base", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, logbook, _ := conversingRig(t)
			base := logbook.endedTurn(t, "b1", "s-base", "the base's transcript")
			if _, err := rig.coordinator.Launch(t.Context(), rig.manager,
				continuingBase("t2", frozenReadOf(logbook, base))); err != nil {
				t.Fatalf("the turn's first launch: %v", err)
			}
			if got := rig.get("t2"); got.Launch.Brief != BriefContinuedConversation || got.Launch.Continued != nil {
				t.Errorf("the base's launch recorded brief %q and the point %+v, want the base and none",
					got.Launch.Brief, got.Launch.Continued)
			}
		})
	})
}

// AN OLDER BUILD'S WRITE KEEPS THE CONTINUED POINT. A fleet mid-upgrade
// rewrites the job's record on a build that does not know the point, and a
// save on this build reads it after: so it rides the record under its own key,
// which that build keeps verbatim and writes back.
//
// Mutation: tag the field `json:"-"`, or leave `continued` out of the wire
// keys, and this goes red.
func TestAnOlderBuildsWriteKeepsTheContinuedPoint(t *testing.T) {
	t.Parallel()
	carriedByAnOlderBuild(t, reflect.TypeFor[LaunchRecord](), []string{"continued"},
		LaunchRecord{Continued: &ContinuedPoint{LaunchSeq: 4, Through: 7}}, func(raw []byte) any {
			var back LaunchRecord
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatal(err)
			}
			return back.Continued
		})
}
