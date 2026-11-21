package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// servedByAHook serves note id to run's job at a hook after a batch, as the
// job's hook would, on the run's record.
func servedByAHook(t *testing.T, pending sandbox.PendingStore, run sandbox.PendingRun, id string) {
	t.Helper()
	served, err := pending.ServeHook(t.Context(), run.TurnID, sandbox.HookServe{IDs: []string{id},
		Launch: run.LaunchID, Statuses: steerTakeStatuses, Serve: sandbox.HookServed{Take: "take-1",
			Boundary: string(types.SteerBoundaryToolBatch), Iteration: run.Launch.Iteration}})
	if err != nil || len(served) != 1 {
		t.Fatalf("ServeHook = %+v, %v; want the note served", served, err)
	}
}

// showsEveryTake is a job's session that shows every take it is asked about
// taken after its last restart.
func showsEveryTake(_ context.Context, _ sandbox.Sandbox, ask sandbox.MainThreadAsk) sandbox.MainThreadReading {
	var held []string
	for _, take := range ask.Takes {
		held = append(held, take.Take)
	}
	return sandbox.MainThreadReading{Receipts: sandbox.HookReceipts{Held: held}}
}

// deliveredAtTheBatch fails the case unless recs is the one record of note
// id, delivered at a tool batch of job launch.
func deliveredAtTheBatch(t *testing.T, recs []types.AgentTurnSteered, id, launch string) {
	t.Helper()
	var got []types.AgentTurnSteered
	for _, rec := range recs {
		if rec.NoteID == id {
			got = append(got, rec)
		}
	}
	if len(got) != 1 || got[0].Outcome != types.SteerDelivered || got[0].Boundary != types.SteerBoundaryToolBatch ||
		got[0].LaunchID != launch {
		t.Errorf("note %s was recorded %+v, want it delivered once, at a tool batch of job %s", id, got, launch)
	}
}

// THE RESUMED SEGMENT CARRIES WHAT ITS COLLECTION CONFIRMED. A note a hook's
// serve handed a Claude Code job is read only once the job's session shows it,
// and the read that collects the job settles that after the claim it resumes
// the turn under read the run's row. So the segment carries the note as read
// by the job, at its batch, and the note is recorded delivered there: through
// a completion, the reviewer opens with it; through a lost job's carry, the
// fresh run that carries its round on is told it.
//
// Mutation: resume from the claim's copy of the notes (drop
// runSteers.claimedNotes from resumeTurn), and neither reader has the note;
// drop SteersConfirmed from the engine's coordinator options, and no record
// says the note was delivered.
func TestTheResumedSegmentCarriesWhatItsCollectionConfirmed(t *testing.T) {
	t.Parallel()
	t.Run("a completion", func(t *testing.T) {
		t.Parallel()
		c := newSteerCase(t, nil)
		parkOnARun(t, c.e, c.rig, "run-1")
		c.file(t, "run-1", "req-1", "use the staging database")
		run := c.rig.row(t, "run-1")
		if run.Harness != sandbox.ClaudeCodeHarness {
			t.Fatalf("the premise: the run is on %q, want a Claude Code job", run.Harness)
		}
		servedByAHook(t, c.e.sandbox.Load().pending, run, "req-1")
		c.rig.runners[run.Harness].MainThread = showsEveryTake
		if err := c.rig.collect(t, "run-1"); err != nil {
			t.Fatalf("collect: %v", err)
		}
		reviews := c.model.reviewed()
		if len(reviews) == 0 {
			t.Fatal("no reviewer ran")
		}
		if !strings.Contains(reviews[0], "use the staging database") {
			t.Errorf("the reviewer opened without the note the job's session showed it took:\n%s", reviews[0])
		}
		deliveredAtTheBatch(t, c.steeringOver(t, "run-1", 1), "req-1", run.LaunchID)
	})
	t.Run("a lost carry", func(t *testing.T) {
		t.Parallel()
		e, _ := spendingEngine(t)
		steerable(t, e)
		rig := rigRuns(t, e, modelPlayer(t, billingRun))
		res, err := e.runTurn(t.Context(), Request{
			Handle: "swe", WorkKey: "wk-lost", RunID: "run-lost", WorkSince: time.Now().UTC(),
		})
		if err != nil || !res.Suspended {
			t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
		}
		filed, err := e.RunSteers().File(t.Context(), "run-lost", steer.Note{ID: "req-1",
			Text: "use the staging database", By: "founder", ByKind: iam.ActorHuman, OperatorID: "session:f"})
		if err != nil || filed.Status != steer.StatusAccepted {
			t.Fatalf("filing the note = %+v, %v", filed, err)
		}
		first := rig.row(t, "run-lost")
		if first.Harness != sandbox.ClaudeCodeHarness {
			t.Fatalf("the premise: the run is on %q, want a Claude Code job", first.Harness)
		}
		pending := e.sandbox.Load().pending
		servedByAHook(t, pending, first, "req-1")
		runner := rig.runners[first.Harness]
		runner.MainThread = showsEveryTake
		runner.Lose(sandbox.Result{Text: "half way through the fix", InputTokens: 300, OutputTokens: 30})
		if marked, err := pending.MarkLost(t.Context(), "run-lost", first.LaunchID,
			sandbox.JobLoss{Cause: sandbox.LossEngineRestarted, Node: first.LaunchNode}); err != nil || !marked {
			t.Fatalf("MarkLost = %v, %v", marked, err)
		}
		completion := types.ExecutorFinished{
			Agent: first.AgentID, AgentHandle: first.AgentHandle, RoleName: first.Role,
			TurnID: first.TurnID, WorkKey: first.WorkKey, LaunchID: first.LaunchID,
			SandboxID: first.SandboxID, Harness: first.Harness,
		}
		ev := events.New(completion, events.TraceContext{TraceID: first.TraceID, ParentSpanID: first.SpanID})
		if err := e.sandbox.Load().coordinator.OnCompleted(t.Context(), completion, ev); err != nil {
			t.Fatalf("claiming the lost job: %v", err)
		}
		started := runner.Started()
		if len(started) != 2 {
			t.Fatalf("%d jobs were started, want the lost one and the one carrying its round on", len(started))
		}
		if !strings.Contains(started[1].Brief, "use the staging database") {
			t.Errorf("the run carrying the lost round on was not told the note the lost job's session "+
				"showed it took:\n%s", started[1].Brief)
		}
		q := e.backends.Queue.(*memory.Queue)
		var recs []types.AgentTurnSteered
		eventually(t, "the note's record", func() bool {
			recs = recs[:0]
			for _, ev := range q.History() {
				if ev.Type != (types.AgentTurnSteered{}).EventType() {
					continue
				}
				if rec, ok := events.DataAs[*types.AgentTurnSteered](ev); ok {
					recs = append(recs, *rec)
				}
			}
			return len(recs) > 0
		})
		deliveredAtTheBatch(t, recs, "req-1", first.LaunchID)
	})
}

// A READ ITS SESSION HELD ONLY BEFORE A RESTART REACHES THE RELAUNCH SO. A
// collection's verdict settles a hook's serve Restarted where the job's session
// shows it taken before its last restart; the turn carries that read with the
// word, which is the runner's only sign that a continued session lacks the note
// (its firstUnheld). A serve settled Held is carried without it.
//
// Mutation: drop Restarted from readingOf, and the restarted case goes red.
func TestARestartedReadReachesTheRelaunchAsRestarted(t *testing.T) {
	t.Parallel()
	for name, restarted := range map[string]bool{"held": false, "restarted": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := newSteerCase(t, nil)
			parkOnARun(t, c.e, c.rig, "run-1")
			c.file(t, "run-1", "req-1", "use the staging database")
			run := c.rig.row(t, "run-1")
			pending := c.e.sandbox.Load().pending
			servedByAHook(t, pending, run, "req-1")
			settle := sandbox.HookSettle{Launch: run.LaunchID, Held: []string{"take-1"}}
			if restarted {
				settle = sandbox.HookSettle{Launch: run.LaunchID, Restarted: []string{"take-1"}}
			}
			if _, err := pending.SettleHook(t.Context(), "run-1", settle); err != nil {
				t.Fatalf("SettleHook: %v", err)
			}
			carried := c.e.runSteers.carried(c.rig.row(t, "run-1"))
			if len(carried) != 1 || carried[0].ID != "req-1" || carried[0].Launch != run.LaunchID ||
				carried[0].Boundary != types.SteerBoundaryToolBatch || carried[0].Restarted != restarted {
				t.Fatalf("the turn carries %+v, want the note read by job %s at its batch, restarted: %v",
					carried, run.LaunchID, restarted)
			}
		})
	}
}

// A RESUMED SEGMENT TAKES THE RECORD'S NOTES ONLY UNDER ITS OWN CLAIM. The
// resume reads the run's row again for what its collection confirmed after
// the claim was read; a row that names another job, another owner or a later
// epoch is no longer that claim's, and the segment keeps the notes its claim
// read.
//
// Mutation: take the row's notes wherever it is found, and a later claim's
// case goes red.
func TestAResumedSegmentTakesTheRecordsNotesOnlyUnderItsOwnClaim(t *testing.T) {
	t.Parallel()
	c := newSteerCase(t, nil)
	parkOnARun(t, c.e, c.rig, "run-1")
	c.file(t, "run-1", "req-1", "use the staging database")
	row := c.rig.row(t, "run-1")
	if len(row.Steer.Notes) != 1 {
		t.Fatalf("the premise: the row holds %d notes, want the one filed", len(row.Steer.Notes))
	}
	for name, tc := range map[string]struct {
		claim func(*sandbox.PendingRun)
		takes bool
	}{
		"its own claim":    {claim: func(*sandbox.PendingRun) {}, takes: true},
		"another job":      {claim: func(r *sandbox.PendingRun) { r.LaunchID = "an-earlier-job" }},
		"another owner":    {claim: func(r *sandbox.PendingRun) { r.Owner = "another-node" }},
		"an earlier epoch": {claim: func(r *sandbox.PendingRun) { r.OwnerEpoch-- }},
	} {
		claim := row
		claim.Steer = sandbox.RunSteer{}
		tc.claim(&claim)
		got, err := c.e.runSteers.claimedNotes(t.Context(), claim)
		if err != nil {
			t.Fatalf("%s: claimedNotes: %v", name, err)
		}
		if took := len(got.Steer.Notes) == 1; took != tc.takes {
			t.Errorf("%s: the segment took the record's notes: %v, want %v", name, took, tc.takes)
		}
	}
}
