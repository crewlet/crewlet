package sandbox

import (
	"strings"
	"testing"
)

// launchAnchored is [waiterRig.launch] for a run whose launch wired it to its
// turn's notes: its record names its seat, the anchor such a launch writes.
func (r *coordRig) launchAnchored(turnID string) PendingRun {
	r.t.Helper()
	ctx := r.t.Context()
	box, err := r.provider.Create(ctx, Spec{})
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	if _, err = r.pending.BeginLaunch(ctx, PendingRun{
		TurnID: turnID, AgentHandle: "swe", AgentID: sweID, Role: "SWE",
		CodingAgent: "claude-code", TraceID: "tr-1", CreatedAt: r.now,
		Steer: RunSteer{Seat: []byte("sealed-seat")},
	}, rigLease); err != nil {
		r.t.Fatalf("BeginLaunch: %v", err)
	}
	if err = r.pending.AttachSandbox(ctx, turnID, BoxRef{
		SandboxID: box.ID(), CommandID: "cmd-1", CodingAgent: "claude-code",
		PauseTTLSec: DefaultPauseTTL.Seconds(),
	}, rigLease); err != nil {
		r.t.Fatalf("AttachSandbox: %v", err)
	}
	r.suspend(turnID)
	return r.get(turnID)
}

// A RUN THAT SUCCEEDED WITHOUT ONCE ASKING FOR ITS NOTES IS SAID, as
// steer_reader_never_ran, at its collection: for a CLI that reads notes, a run
// that finishes always asks at its last step, so one that never did had its
// reading switched off in its box — and every note sent while it worked waited
// for its review. Said only where it means that: not for a run whose CLI asked
// (its record names the job as having pulled), not for a CLI that reads no
// notes, not for a run its launch wired to none (its record names no seat), and
// not for a run that did not succeed, which may never have reached its end.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: drop the warning, or any one of its conditions, and a row goes red.
func TestARunThatNeverAskedForItsNotesIsSaid(t *testing.T) {
	for name, tc := range map[string]struct {
		reads, anchored, pulled bool
		result                  Result
		said                    bool
	}{
		"a reader that never asked": {reads: true, anchored: true, result: Result{Success: true, Text: "done"}, said: true},
		"a reader that asked":       {reads: true, anchored: true, pulled: true, result: Result{Success: true, Text: "done"}},
		"a CLI that reads none":     {anchored: true, result: Result{Success: true, Text: "done"}},
		"a run wired to no notes":   {reads: true, result: Result{Success: true, Text: "done"}},
		"a run that failed":         {reads: true, anchored: true, result: Result{Error: "error_max_turns"}},
	} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newCoordRig(t)
			rig.runner.TakesNotes = tc.reads
			var run PendingRun
			if tc.anchored {
				run = rig.launchAnchored("t1")
			} else {
				run = rig.launch("t1")
			}
			if tc.pulled {
				if ok, err := rig.pending.MarkPulled(t.Context(), run.TurnID, run.LaunchID); err != nil || !ok {
					t.Fatalf("MarkPulled = %v, %v", ok, err)
				}
			}
			rig.runner.Finish(tc.result)
			payload, ev := rig.completion(run.TurnID)
			if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
				t.Fatalf("OnCompleted: %v", err)
			}
			line := logs.String()
			if said := strings.Contains(line, "steer_reader_never_ran"); said != tc.said {
				t.Fatalf("steer_reader_never_ran said: %v, want %v\n%s", said, tc.said, line)
			}
			if tc.said {
				for _, want := range []string{"turn_id=" + run.TurnID, "launch_id=" + run.LaunchID,
					"agent=claude-code", "disableAllHooks"} {
					if !strings.Contains(line, want) {
						t.Errorf("the warning does not carry %s: %q", want, line)
					}
				}
			}
		})
	}
}
