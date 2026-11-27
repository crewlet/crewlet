package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
)

// THE ONE PROPERTY: a call that takes a run out of the set of things that will
// come back must tell the engine that the turn stopped.
//
// Hand-listing the safe paths was wrong each round, so the enumeration is
// derived. Every exported [Coordinator] entry point is driven once per status a
// run can hold, cleanly and then once per store call it makes with THAT CALL
// REFUSED: if the run would have come back before and not after, and the call
// did not hand the turn to the resumer, [CoordinatorOptions.Stopped] must have
// fired. Derived: the entry points ([coordinatorEntries]), the refusals
// (OBSERVED, not declared) and the starting status ([Active], every status a
// record can hold; [place] fails on one it does not know). No pair is skipped.

// entryDrive is one way of reaching a [Coordinator] entry point with a run in
// front of it.
//
// TWO HALVES, because only the second is the subject: arrange runs against the
// REAL store, so a drive can queue a result or a failure without the refusal
// under test hitting the arrangement instead of the act.
type entryDrive struct {
	name string

	// arrange is what this drive needs beyond the run itself — a finished
	// job to collect, a resumer that breaks. It runs on a run [place] has
	// already put into the status under test, before the refusing store is
	// installed and before the row is snapshotted. Nil where the drive
	// needs nothing of its own.
	arrange func(t *testing.T, rig *coordRig)

	// call is the entry point, reached with one store call refused.
	call func(t *testing.T, rig *coordRig)
}

// coordinatorEntries is EVERY exported [Coordinator] method, with the drives
// that put a run in front of it.
//
// A ROSTER, held both ways by [TestEveryCoordinatorEntryPointIsDriven]: a new
// method fails until its drive is written, as does an entry naming a missing one.
// A DRIVE VARIES WHAT IS AROUND THE RUN, NEVER ITS STATUS ([place]'s dimension).
// A one-line drive for a method that cannot move a run proves the property
// trivially; an exemption list would be a second place to state the judgement,
// and a wrong exemption is invisible where a trivial drive is visible.
// Concurrency is not simulated: every failure is a store REFUSAL, never a second
// party on the row; the losing half of a race has its own case
// ([TestASettleSomebodyElseEndedReportsNoStop]).
var coordinatorEntries = map[string][]entryDrive{
	"OnEvent": {{
		name:    "a completion routed by type",
		arrange: jobFinished,
		call: func(t *testing.T, rig *coordRig) {
			payload, _ := rig.completion("t1")
			ev := events.New(payload, events.TraceContext{})
			if err := rig.coordinator.OnEvent(t.Context(), ev); err != nil {
				t.Logf("OnEvent: %v", err)
			}
		},
	}},
	"OnStarted": {{
		name: "a start event, redelivered or not",
		call: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.OnStarted(t.Context(), types.ExecutorLaunched{
				AgentHandle: "swe", TurnID: "t1",
			}); err != nil {
				t.Logf("OnStarted: %v", err)
			}
		},
	}},
	"OnCompleted": {
		{
			name:    "a job that finished",
			arrange: jobFinished,
			call:    completes,
		},
		{
			name:    "a job that asks a person a question",
			arrange: jobAsks,
			call:    completes,
		},
		{
			name:    "a job whose box cannot be read back",
			arrange: collectFails,
			call:    completes,
		},
		{
			name:    "a job whose resume fails",
			arrange: resumeFails,
			call:    completes,
		},
		{
			name:    "a job whose resume broke after writing outside the engine",
			arrange: resumeAbandoned,
			call:    completes,
		},
		{
			name:    "a row carrying no suspended conversation",
			arrange: noConversation,
			call:    completes,
		},
		{
			name:    "a settle whose record cannot be read back",
			arrange: jobFinished,
			call:    completesUnreadable,
		},
	},
	"TryResumeFromAnswer": {{
		name: "a person's reply on the run's own conversation",
		call: func(t *testing.T, rig *coordRig) {
			if _, err := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
				chatReply(answerOnTheDM, "the release branch", nil)); err != nil {
				t.Logf("TryResumeFromAnswer: %v", err)
			}
		},
	}},
	"AnswerByTurn": {
		{
			name: "a person's answer naming the run by its turn",
			call: answersByTurn,
		},
		{
			name:    "an answer by turn whose resume fails",
			arrange: func(_ *testing.T, rig *coordRig) { rig.resumer.failWith(errors.New("the node lost the seat")) },
			call:    answersByTurn,
		},
		{
			name: "an answer by turn whose resume broke after writing outside the engine",
			arrange: func(_ *testing.T, rig *coordRig) {
				rig.resumer.failWith(fmt.Errorf("%w: the reviewer's provider went away", ErrResumeAbandoned))
			},
			call: answersByTurn,
		},
	},
	"OnAnswerGiven": {
		{
			name: "a person's answer arriving on the seat's control topic",
			call: answersOnControl,
		},
		{
			name:    "an answer on the control topic whose resume fails",
			arrange: func(_ *testing.T, rig *coordRig) { rig.resumer.failWith(errors.New("the node lost the seat")) },
			call:    answersOnControl,
		},
	},
	"OnWaitOver": {
		{
			name:    "the engine's wake for a run parked on its own call",
			arrange: parkedOnItsOwnCall,
			call:    wakesOnControl,
		},
		{
			name: "a wake whose carrying on fails",
			arrange: func(t *testing.T, rig *coordRig) {
				parkedOnItsOwnCall(t, rig)
				rig.resumer.failWith(errors.New("the node lost the seat"))
			},
			call: wakesOnControl,
		},
	},
	"Launch": {{
		name: "a relaunch on the run's own turn",
		call: func(t *testing.T, rig *coordRig) {
			if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
				t.Logf("Launch: %v", err)
			}
		},
	}},
	"ContinuedCapture": {{
		// IT ENDS NOTHING: it reads the run's record — waiting a moment for
		// the capture of the job a relaunch follows to be kept — and the
		// turn's row on the executor log, and answers which kept session the
		// relaunch continues, writing nothing. Asked with a restore to read,
		// so the drive reaches the run's record rather than answering that
		// this node continues nothing.
		name:    "a relaunch asking which kept session it continues",
		arrange: func(_ *testing.T, rig *coordRig) { rig.coordinator.restore = &keptRestore{} },
		call: func(t *testing.T, rig *coordRig) {
			if _, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", fakeRestorer{}); why != "" {
				t.Logf("ContinuedCapture: %s", why)
			}
		},
	}},
	"HoldingsOf": {{
		// IT ENDS NOTHING: it reads the run's record and the turn's row on
		// the executor log, hands the runner it is given an opener of the
		// kept session's object, and answers what that session holds of the
		// round's calls, writing nothing. Asked with a restore to read, so
		// the drive reaches the run's record rather than answering that this
		// node continues nothing.
		name:    "a relaunch asking what the kept session it continues holds of its round's calls",
		arrange: func(_ *testing.T, rig *coordRig) { rig.coordinator.restore = &keptRestore{} },
		call: func(t *testing.T, rig *coordRig) {
			run, _, _ := rig.pending.Get(t.Context(), "t1")
			if _, err := rig.coordinator.HoldingsOf(t.Context(), "t1", run.LaunchSeq, 1,
				askingAbout("toolu_01a"), &holdingsSpy{}); err != nil {
				t.Logf("HoldingsOf: %v", err)
			}
		},
	}},
	"FailRun": {{
		name: "a suspension that never reached the row",
		call: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.FailRun(t.Context(), "t1",
				types.ExecutorFailureSuspensionUnrecorded,
				"the suspended conversation could not be written"); err != nil {
				t.Logf("FailRun: %v", err)
			}
		},
	}},
	"RecoverSeat": {
		{
			name: "a seat claimed by a new owner",
			call: recovers,
		},
		{
			name:    "a seat claimed by a new owner whose parked run no build will resume",
			arrange: judgedUnresumable,
			call:    recovers,
		},
	},
	"SettleParked": {
		{
			name: "a pickup pass over the seat's parked runs",
			call: settlesParked,
		},
		{
			name:    "a pickup pass that ends a parked run no build will resume",
			arrange: judgedUnresumable,
			call:    settlesParked,
		},
	},
	"StopSeat": {
		{
			name: "a person's pause asking the seat's running work to stop",
			call: stopsTheSeat,
		},
		{
			name:    "a stop that finds the job's spend to read back",
			arrange: jobFinished,
			call:    stopsTheSeat,
		},
		{
			name:    "a stop whose box cannot be read back",
			arrange: collectFails,
			call:    stopsTheSeat,
		},
	},
	"RetireSeat": {{
		name: "the runs of a seat that left the company",
		call: func(t *testing.T, rig *coordRig) {
			if _, err := rig.coordinator.RetireSeat(t.Context(), sweSeat(), "swe", "node-a:1", 3); err != nil {
				t.Logf("RetireSeat: %v", err)
			}
		},
	}},
	"ReadHostedBox": {{
		// IT ENDS NOTHING: it reads, for the node collecting the run, the
		// box this node hosts — and pauses it where a collection would —
		// so whatever becomes of the run is the asker's to say.
		name:    "a peer's collection of a box on this node's host",
		arrange: jobFinished,
		call: func(t *testing.T, rig *coordRig) {
			run, _, _ := rig.pending.Get(t.Context(), "t1")
			if _, _, err := rig.coordinator.ReadHostedBox(t.Context(), "t1", run.LaunchID,
				run.SandboxID, BoxReadCollect); err != nil {
				t.Logf("ReadHostedBox: %v", err)
			}
		},
	}},
	"RecordHanded": {{
		// IT ENDS NOTHING: it records a token the poll's refresh is about to
		// write into the run's box in the sealed set beside the job's row,
		// and writes nothing of the row itself — a job the row no longer
		// holds running is answered as one, its token written nowhere, and
		// whatever becomes of the run is the poll's to say as it always does.
		name: "a token the poll renews in the run's box, recorded before it is written",
		call: func(t *testing.T, rig *coordRig) {
			run, _, _ := rig.pending.Get(t.Context(), "t1")
			if err := rig.coordinator.RecordHanded(t.Context(), run, "a-token-the-poll-renewed",
				rig.now.Add(15*time.Minute)); err != nil {
				t.Logf("RecordHanded: %v", err)
			}
		},
	}},
	"OweEnd": {{
		// IT ENDS NOTHING: it writes onto the run's record the end its turn
		// owes the executor log, which the run's own ending records before
		// its delete — so whatever ends the run says so as it always does.
		name: "the end a turn's frame decided, owed on its run's record",
		call: func(t *testing.T, rig *coordRig) {
			if _, err := rig.coordinator.OweEnd(t.Context(), "t1",
				executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}); err != nil {
				t.Logf("OweEnd: %v", err)
			}
		},
	}},
	"EndTurn": {{
		// IT ENDS NOTHING: it records a turn's end for a frame that found no
		// run's record to owe it on — on the executor log where the record
		// is gone, and owed on the record where it is there after all, for
		// the run's own ending to record — and deletes nothing.
		name: "the end of a turn whose frame found no run's record",
		call: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.EndTurn(t.Context(), TurnEnd{Handle: "swe", TurnID: "t1",
				Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
				t.Logf("EndTurn: %v", err)
			}
		},
	}},
	"ReleaseSeat": {{
		name: "a seat handed on, whose runs are its successor's",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.ReleaseSeat("swe") },
	}},
	"SeatRuns": {{
		name: "the screening's two answers",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.SeatRuns("swe") },
	}},
	"SeatHeldBySandbox": {{
		name: "the busy half of it",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.SeatHeldBySandbox("swe") },
	}},
	"ResumesBlocked": {
		// IT ENDS NOTHING: it reads this node's memory of the runs whose
		// last resume failed on a condition only an operator clears, asks
		// the lease seam whether this node still holds each one's seat
		// under the lease its attempt was taken with, and forgets, in that
		// memory alone, one it no longer does. It asks the store nothing
		// and writes nothing to it, so whatever becomes of the run is the
		// next completion's or recovery's to say. Driven with the run
		// remembered both ways, so the count and the letting go are each
		// reached.
		{
			name:    "the alarm's reading of a run waiting for the operator, its seat held",
			arrange: blockedUnder(rigLease),
			call:    readsBlocked,
		},
		{
			name:    "the alarm's reading of a run remembered under a lease this node no longer holds",
			arrange: blockedUnder(Fence{Owner: "node-a:1", Epoch: rigLease.Epoch - 1}),
			call:    readsBlocked,
		},
	},
	"LaunchStopped": {{
		// IT ENDS NOTHING: it hands the frame launching a run the stop
		// decided on it — already ended by the stop's own ending — and
		// forgets it.
		name: "the launching frame taking the stop that ended its run",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.LaunchStopped("t1") },
	}},
	"HoldSeat": {{
		name: "a delivery that reached a seat this node counts as held",
		call: func(t *testing.T, rig *coordRig) { rig.coordinator.HoldSeat(t.Context(), "swe") },
	}},
	"HoldForAside": {{
		name: "a delivery that would wait aside on an inbox holding as many as it keeps",
		call: func(t *testing.T, rig *coordRig) { rig.coordinator.HoldForAside(t.Context(), "swe") },
	}},
	"KeyBusy": {{
		name: "whether a run still holds the key a delivery would wait aside under",
		call: func(t *testing.T, rig *coordRig) { _, _ = rig.coordinator.KeyBusy(t.Context(), "swe", RunKey("t1")) },
	}},
	"SeatCount": {{
		name: "the seat's counts against its cap",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.SeatCount("swe") },
	}},
	"HeldBeside": {{
		name: "the runs a launch starts beside",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.HeldBeside("swe", "t9") },
	}},
	"ReadsNotes": {{
		name: "whether a harness takes another run's message, reading no row",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.ReadsNotes("claude-code") },
	}},
	"ConversationBusy": {{
		name: "whether a run works a delivery's conversation, reading the seat's rows",
		call: func(t *testing.T, rig *coordRig) { _, _ = rig.coordinator.ConversationBusy(t.Context(), "swe", "c") },
	}},
	"Readmit": {{
		name: "a signal that a seat's conditions may have cleared",
		call: func(_ *testing.T, rig *coordRig) {
			rig.coordinator.Readmit("paused", "swe")
			rig.fireRetries()
		},
	}},
	"SaveRunning": {{
		// IT ENDS NOTHING: a save's stamp is a place on the row its license
		// grants only a running job, and what came of it moves no status; what
		// becomes of the run is its poll's to say.
		name: "a running box the poll offers for a save of its session",
		call: func(t *testing.T, rig *coordRig) {
			run, _, _ := rig.pending.Get(t.Context(), "t1")
			rig.coordinator.SaveRunning(t.Context(), run, rig.runner)
		},
	}},
	"SaveHosted": {{
		// IT ENDS NOTHING: a drain's save is a save as an offered one is, of a
		// job the drain leaves running.
		name: "a draining node saving its Direct jobs' sessions",
		call: func(t *testing.T, rig *coordRig) {
			rig.coordinator.SaveHosted(t.Context())
		},
	}},
	"Manager": {{
		name: "the manager a caller mints a box through",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.Manager() },
	}},
	"SetManager": {{
		name: "a live reload of providers.sandbox",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.SetManager(rig.manager) },
	}},
	"Stop": {{
		name: "a node shutting down, whose recorded answers its successor drives",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.Stop() },
	}},
}

// placements is how a run REACHES each status a record can hold, by the route
// the engine takes to it.
//
// A ROSTER RATHER THAN A SWITCH, held to [Active] in BOTH DIRECTIONS by
// [TestEveryStatusHasAPlacement] — the same rule
// [TestEveryCoordinatorEntryPointIsDriven] applies to the methods, and the
// same both-ways, which is what a switch could not be. One direction is half a
// guard either way round: a status added to the state machine has to say how a
// run reaches it before anything here can certify what happens from there, and
// an arrangement for a status later dropped from [Active] is an arm nothing
// runs while looking exactly like coverage — these statuses are string
// constants, so the compiler never sees it go dead.
var placements = map[string]func(t *testing.T, rig *coordRig){
	StatusLaunching: func(_ *testing.T, rig *coordRig) {
		// The job is started and its box attached; the conversation a
		// resume re-enters is not on the row yet.
		rig.launching("t1")
	},
	StatusRunning: func(_ *testing.T, rig *coordRig) {
		rig.launch("t1")
	},
	StatusResumed: func(t *testing.T, rig *coordRig) {
		rig.launch("t1")
		// THROUGH THE COMPLETION'S OWN TAIL, because a claim is scoped
		// to the launch it is taken for and the status it comes out of
		// — a claim arranged any other way would be one this package
		// cannot actually take.
		run := rig.get("t1")
		if _, won, err := rig.pending.ClaimForResume(t.Context(), "t1",
			CompletionTail(run.LaunchID), rigLease); err != nil || !won {
			t.Fatalf("ClaimForResume = %v, %v", won, err)
		}
	},
	StatusAwaiting: func(_ *testing.T, rig *coordRig) {
		rig.launch("t1")
		rig.park("t1")
	},
	StatusAnswered: func(t *testing.T, rig *coordRig) {
		rig.launch("t1")
		rig.park("t1")
		// A person's reply recorded against the question, its resume
		// still owed — which a node that stopped between the two leaves
		// for the seat's next holder.
		run := rig.get("t1")
		words, answer := chatReply(answerOnTheDM, "the release branch", nil).answerOf(rig.now)
		if _, won, err := rig.pending.RecordAnswer(t.Context(), "t1", run.LaunchID,
			sealedOnto(t, rigAnswers(), run, words, answer), rigLease); err != nil || !won {
			t.Fatalf("RecordAnswer = %v, %v", won, err)
		}
	},
	StatusReseed: func(t *testing.T, rig *coordRig) {
		rig.launch("t1")
		rig.park("t1")
		// The pause reaper took the box; the run is not over, because the
		// answer can still arrive and re-seed from the pushed branch.
		if won, err := rig.pending.ExpirePause(t.Context(), "t1"); err != nil || !won {
			t.Fatalf("ExpirePause = %v, %v", won, err)
		}
	},
}

// place puts this rig's run in one status and counts the seat as the engine
// would.
//
// A STATUS IT DOES NOT KNOW FAILS RATHER THAN SKIPS, which is the roster's
// first direction reaching the one caller: the matrix drives every status in
// [Active], so one added to the state machine has to say how a run reaches it
// before anything here can certify what happens from there.
func place(t *testing.T, rig *coordRig, status string) {
	t.Helper()
	arrange, known := placements[status]
	if !known {
		t.Fatalf("no arrangement for status %q: this matrix drives every status in Active, "+
			"so a new one has to say how a run reaches it before anything can certify "+
			"what happens to a turn suspended into it", status)
	}
	arrange(t, rig)
	rig.coordinator.countRun("swe", status)
}

// Every status a run can hold is placed, and every placement names one.
//
// THE SECOND DIRECTION, which [place] cannot reach: it fails on a status it
// does not know, and nothing at all happens to a placement for a status that
// has left [Active]. That arm would simply stop running, silently, and read as
// a status this matrix still covers.
func TestEveryStatusHasAPlacement(t *testing.T) {
	t.Parallel()
	for _, status := range Active {
		if placements[status] == nil {
			t.Errorf("no placement for %q: this matrix drives every status in Active, so a "+
				"status added to the state machine has to say how a run reaches it", status)
		}
	}
	for status := range placements {
		if !slices.Contains(Active, status) {
			t.Errorf("placements arranges %q, which is not in Active any more: the arm is "+
				"never driven and certifies nothing", status)
		}
	}
}

// jobFinished queues the result a launch's finished job comes back with, so a
// completion has something to collect.
func jobFinished(_ *testing.T, rig *coordRig) {
	rig.runner.Finish(Result{Success: true, Text: "done"})
}

// jobAsks is the job that stopped to ask a person something.
//
// ASKED AS A JOB ASKS, through the store and under the launch the row holds,
// whatever status the matrix placed the run in: only a running job's ask is
// recorded, and a row placed past it refuses one, as it refuses a call that
// outlived its job. The completion then collects whatever the row holds.
func jobAsks(t *testing.T, rig *coordRig) {
	t.Helper()
	run := rig.get("t1")
	if _, err := rig.pending.RecordAsk(t.Context(), "t1", run.LaunchID, whichBranch()); err != nil {
		t.Fatalf("RecordAsk: %v", err)
	}
	rig.runner.Finish(Result{})
}

// collectFails is the box that died between finishing and being read back.
func collectFails(t *testing.T, rig *coordRig) {
	jobFinished(t, rig)
	rig.runner.CollectErr = errors.New("the box died mid-read")
}

// resumeFails is the re-entry that broke without reaching outside the engine,
// which is the one failure that KEEPS the engine's hold.
func resumeFails(t *testing.T, rig *coordRig) {
	jobFinished(t, rig)
	rig.resumer.err = errors.New("the node lost the seat mid-resume")
}

// resumeAbandoned is the re-entry that must not be re-entered again: it broke
// after its writes had landed, or it panicked.
func resumeAbandoned(t *testing.T, rig *coordRig) {
	jobFinished(t, rig)
	rig.resumer.err = fmt.Errorf("%w: the reviewer's provider went away", ErrResumeAbandoned)
}

// noConversation is a row with nothing to resume INTO: a row with no
// suspension, which the launch path never writes — a run holds
// [StatusLaunching] until its conversation is written — so it is written here
// directly.
//
// [PendingStore.BeginLaunch] is the one write that drops a suspension, so the
// status [place] chose is put back after it.
func noConversation(t *testing.T, rig *coordRig) {
	jobFinished(t, rig)
	run := rig.get("t1")
	if _, err := rig.pending.BeginLaunch(t.Context(), run, Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if run.Status == StatusLaunching {
		return
	}
	if err := rig.pending.SetStatus(t.Context(), "t1", run.Status, Fence{}); err != nil {
		t.Fatalf("SetStatus %s: %v", run.Status, err)
	}
}

// completes hands the coordinator the completion its waiter would publish.
func completes(t *testing.T, rig *coordRig) {
	t.Helper()
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		// LOGGED, NEVER FAILED. Every drive here is meant to fail somewhere
		// — that is the point — and what the call returns is asserted by the
		// named cases that own each path. The only question in this file is
		// whether a run that stopped said so.
		t.Logf("OnCompleted: %v", err)
	}
}

// completesUnreadable is the same completion with the settle's own read of the
// row refused: the failure that reaches the coordinator INSIDE a tail it cannot
// repeat, the turn having been re-entered. Its own drive, since the matrix
// refuses a call for the whole drive and the claim reads the row too.
//
// IT ASSERTS NO REPORT. [Coordinator.settleClaimed]'s two call sites sit behind
// a resume returning nil or [ErrResumeAbandoned] (other failures give the claim
// back, [Coordinator.unclaim]), both an OWNED re-entry, so from the statuses
// whose claim is won [entryDrive.run] takes the turn-ended-its-own-hold
// exemption; from [StatusLaunching] and [StatusResumed] the claim is refused and
// the earlier skip fires. It certifies only that the path RUNS; that the settle
// REPORTS is held by [TestASettleWhoseRecordCannotBeReadEndsTheRunAnyway].
func completesUnreadable(t *testing.T, rig *coordRig) {
	t.Helper()
	rig.coordinator.pending = &refusingStore{
		inner: rig.coordinator.pending, refuse: []string{"Get"},
	}
	completes(t, rig)
}

// answersByTurn hands the coordinator the answer an operator gave by naming
// the run, as the dispatcher does off the seat's inbox.
func answersByTurn(t *testing.T, rig *coordRig) {
	t.Helper()
	given := answerByTurnOnTheRow(t, rig)
	if _, err := rig.coordinator.AnswerByTurn(t.Context(), given,
		events.New(given, events.TraceContext{})); err != nil {
		t.Logf("AnswerByTurn: %v", err)
	}
	// THE RESUME IS QUEUED, never inline: it runs on the retry the record
	// armed, admitted as every retry is.
	rig.fireRetries()
}

// answersOnControl delivers an answer by turn the way the seat receives one: on
// its control topic.
func answersOnControl(t *testing.T, rig *coordRig) {
	t.Helper()
	given := answerByTurnOnTheRow(t, rig)
	if err := rig.coordinator.OnEvent(t.Context(), events.New(given, events.TraceContext{})); err != nil {
		t.Logf("OnEvent: %v", err)
	}
	rig.fireRetries()
}

// parkedOnItsOwnCall marks t1's job as parked on its own call alone, whatever
// status the row was placed in: what the engine's wake carries on where the
// row waits.
func parkedOnItsOwnCall(t *testing.T, rig *coordRig) {
	t.Helper()
	if _, _, err := rig.pending.mutate(t.Context(), "t1", func(run *PendingRun) bool {
		run.Question = CallWaitQuestion("delegate")
		run.ParkedCalls = []CallWait{{Tool: "delegate", Seq: 1, Ask: true, Job: run.LaunchID}}
		return true
	}); err != nil {
		t.Logf("marking the call park: %v", err)
	}
}

// wakesOnControl delivers the engine's wake for t1 on the seat's control
// topic, naming the job the row holds now.
func wakesOnControl(t *testing.T, rig *coordRig) {
	t.Helper()
	run, _, _ := rig.pending.Get(t.Context(), "t1")
	wake := types.ExecutorWaitOver{TurnID: "t1", Agent: sweID, AgentHandle: "swe", LaunchID: run.LaunchID}
	if err := rig.coordinator.OnEvent(t.Context(), events.New(wake, events.TraceContext{})); err != nil {
		t.Logf("OnEvent: %v", err)
	}
	rig.fireRetries()
}

// answerByTurnOnTheRow is the answer answer_executor gives t1, naming the
// question the row is on now, as answer_executor names it off the row it
// checked, whatever status the row was placed in.
func answerByTurnOnTheRow(t *testing.T, rig *coordRig) types.ExecutorAnswerGiven {
	t.Helper()
	run, _, _ := rig.pending.Get(t.Context(), "t1")
	given := types.ExecutorAnswerGiven{
		TurnID: "t1", Agent: sweID, AgentHandle: "swe", Answer: "the release branch",
		AnsweredBy: "founder", AnsweredByKind: "human", OperatorID: "pat:founder-token",
		LaunchID: run.LaunchID,
	}
	if given.LaunchID == "" {
		given.LaunchID = "launch-of-a-run-with-no-record"
	}
	return given
}

// stopsTheSeat is the pause a person put on the seat with `stop_running`,
// reaching the node that holds it.
func stopsTheSeat(t *testing.T, rig *coordRig) {
	t.Helper()
	if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{
		By: "founder", ByKind: "human", OperatorID: "pat:founder-token",
	}); err != nil {
		t.Logf("StopSeat: %v", err)
	}
}

// settlesParked is a pickup pass judging the seat's parked runs.
func settlesParked(t *testing.T, rig *coordRig) {
	t.Helper()
	if _, err := rig.coordinator.SettleParked(t.Context(), "swe"); err != nil {
		t.Logf("SettleParked: %v", err)
	}
}

// judgedUnresumable is an engine that judges every parked run unresumable.
func judgedUnresumable(_ *testing.T, rig *coordRig) {
	rig.coordinator.unresumable = func(context.Context, PendingRun) (*Unresumable, error) {
		return &Unresumable{Reason: types.ExecutorFailureItemFinished, Detail: "the item was finished"}, nil
	}
}

// blockedUnder is this node remembering that t1's last resume failed on a
// condition only an operator clears, under fence, its hand-back landed: the
// state [Coordinator.ResumesBlocked] reads. Written into the memory itself,
// because what puts it there is a completion's resume failing, which is
// OnCompleted's drive and not this one's.
func blockedUnder(fence Fence) func(*testing.T, *coordRig) {
	return func(_ *testing.T, rig *coordRig) {
		rig.coordinator.mu.Lock()
		defer rig.coordinator.mu.Unlock()
		if rig.coordinator.blocked == nil {
			rig.coordinator.blocked = map[string]blockedResume{}
		}
		rig.coordinator.blocked["t1"] = blockedResume{agent: "swe", kind: "disk_full", since: rig.now,
			fence: fence, counted: true}
	}
}

// readsBlocked is the reading the engine takes into the executor_resume_blocked
// alarm on every evaluation.
func readsBlocked(t *testing.T, rig *coordRig) {
	t.Logf("ResumesBlocked: %+v", rig.coordinator.ResumesBlocked())
}

// recovers takes the seat under a fresh lease, as a node claiming it does.
func recovers(t *testing.T, rig *coordRig) {
	t.Helper()
	if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
		t.Logf("RecoverSeat: %v", err)
	}
}

// Every exported entry point is named here, and every name here is an entry
// point.
//
// BOTH DIRECTIONS, in the tradition of [internal/solo]'s roster guard: a new
// method with no drive is the next silent stop, and a drive for a method that
// has been renamed or removed is a case that certifies nothing while looking
// like coverage.
func TestEveryCoordinatorEntryPointIsDriven(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(&Coordinator{})
	exported := map[string]bool{}
	for i := range typ.NumMethod() {
		exported[typ.Method(i).Name] = true
	}
	for name := range exported {
		if len(coordinatorEntries[name]) == 0 {
			t.Errorf("(*Coordinator).%s is not driven by coordinatorEntries: write how it "+
				"reaches a run, even if the answer is that it cannot — a path that ends a "+
				"suspended run without reporting is what this roster exists to catch", name)
		}
	}
	for name, drives := range coordinatorEntries {
		if !exported[name] {
			t.Errorf("coordinatorEntries names %q, which is not an exported method of "+
				"*Coordinator any more: the drive certifies nothing", name)
		}
		for _, drive := range drives {
			if drive.name == "" || drive.call == nil {
				t.Errorf("%s has a drive with no name or nothing to call", name)
			}
		}
	}
}

// AND A RUN THAT STOPS REPORTS IT, from whatever status it held and whichever
// store call was refused.
//
// The file comment above is the property; this is the matrix. Each drive runs
// once per status in [Active] against a store that works — which is itself a
// case, since a path that reports nothing when everything succeeds is the same
// defect — and then once per store call that drive made from that status, with
// that one call refused.
func TestAStoppedRunReportsItUnderEveryRefusedStoreCall(t *testing.T) {
	t.Parallel()
	for method, drives := range coordinatorEntries {
		for _, drive := range drives {
			t.Run(method+"/"+drive.name, func(t *testing.T) {
				t.Parallel()
				for _, status := range Active {
					t.Run("from "+status, func(t *testing.T) {
						t.Parallel()
						for _, call := range drive.run(t, status, "") {
							t.Run("with "+call+" refused", func(t *testing.T) {
								t.Parallel()
								drive.run(t, status, call)
							})
						}
					})
				}
			})
		}
	}
}

// run drives one entry point from one status with one store call refused (or
// none), asserts the property, and reports the store calls the drive made.
func (d entryDrive) run(t *testing.T, status, refused string) []string {
	t.Helper()
	rig := newCoordRig(t)
	place(t, rig, status)
	if d.arrange != nil {
		d.arrange(t, rig)
	}
	store := &refusingStore{inner: rig.coordinator.pending, refuse: refusals(refused)}
	rig.coordinator.pending = store

	before, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("reading the run before the drive: %v", err)
	}
	comingBack := willComeBack(before, found)
	reported := len(rig.stoppedTurns())
	owned := rig.resumer.ownedHolds()

	d.call(t, rig)

	after, found, err := rig.pending.Get(t.Context(), "t1")
	if err != nil {
		t.Fatalf("reading the run after the drive: %v", err)
	}
	stops := rig.stoppedTurns()[reported:]
	switch {
	case !comingBack || willComeBack(after, found):
		// Either there was nothing to lose, or nothing was lost.
	case rig.resumer.ownedHolds() > owned:
		// The turn was re-entered and its own frame took down what it had
		// raised — see [resumeSpy.owned] for why that is not the same as
		// "the resumer was reached".
	case len(stops) == 0:
		t.Errorf("the run stopped and nothing was told: it went from %q to %s with %q "+
			"refused, so no poll, redelivery, answer or reaper will ever pick it up — "+
			"and whatever was held up while its turn worked is still up",
			before.Status, describeRow(after, found), refused)
	case !slices.Contains(stops, "swe/t1"):
		t.Errorf("reported %v as stopped with %q refused, want the run that stopped", stops, refused)
	}
	return store.calls()
}

// willComeBack reports whether anything would still pick this run up.
//
// DERIVED FROM THE PACKAGE'S OWN DECLARATIONS, so a status added to [Claimable]
// changes this answer with it rather than leaving a second, older opinion here.
// [StatusLaunching] counts although nothing claims it: its turn has not
// suspended yet, so the frame that raised whatever is held up is still on the
// stack and ends it itself. A run whose ending is decided comes back to nothing
// whatever its status says: the row takes no write but the ending's own.
func willComeBack(run PendingRun, found bool) bool {
	return found && run.Ending == nil &&
		(slices.Contains(Claimable, run.Status) || run.Status == StatusLaunching)
}

func describeRow(run PendingRun, found bool) string {
	switch {
	case !found:
		return "no record at all"
	case run.Ending != nil:
		return "status " + run.Status + ", its ending decided"
	}
	return "status " + run.Status
}

// refusingStore refuses ONE named store call and records every call made.
//
// NOT AN EMBEDDED [PendingStore], which is the whole reason this type is long:
// embedding would promote the methods it forgot, so a method added to the
// contract would be un-refusable and every case above would keep passing while
// covering less. Written out, the compiler is the guard.
type refusingStore struct {
	inner PendingStore

	// refuse is every call name to fail. A SET rather than one name because
	// the failures this package has to be correct about come in sequences: a
	// park whose question does not land tries to give its claim back, and an
	// unreachable coordination store refuses both.
	refuse []string

	mu   sync.Mutex
	seen []string
}

var _ PendingStore = (*refusingStore)(nil)

// errRefusedCall is what a refused call answers.
//
// One sentence an operator would actually see from an unreachable coordination
// store, rather than a test marker, because several paths put it in a log line
// or an announcement detail.
var errRefusedCall = errors.New("the coordination store refused the call")

// called records one call and reports whether it is the refused one.
func (s *refusingStore) called(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.seen, name) {
		s.seen = append(s.seen, name)
	}
	return slices.Contains(s.refuse, name)
}

// refusals is the set one matrix case refuses: the named call, or nothing.
func refusals(name string) []string {
	if name == "" {
		return nil
	}
	return []string{name}
}

// calls is every store call this drive made, in first-seen order.
func (s *refusingStore) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

func (s *refusingStore) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) (PendingRun, error) {
	if s.called("BeginLaunch") {
		return PendingRun{}, errRefusedCall
	}
	return s.inner.BeginLaunch(ctx, run, fence)
}

func (s *refusingStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.called("Get") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.Get(ctx, turnID)
}

// RowCrossing is the inner store's, never refused nor recorded: a figure the
// store states about its connection, which reaches nothing.
func (s *refusingStore) RowCrossing() time.Duration { return s.inner.RowCrossing() }

// RowReadCrossing is the inner store's, as [refusingStore.RowCrossing] is.
func (s *refusingStore) RowReadCrossing() time.Duration { return s.inner.RowReadCrossing() }

func (s *refusingStore) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	if s.called("ClaimForResume") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.ClaimForResume(ctx, turnID, tail, fence)
}

func (s *refusingStore) ReleaseClaim(ctx context.Context, turnID string, r Release) (bool, error) {
	if s.called("ReleaseClaim") {
		return false, errRefusedCall
	}
	return s.inner.ReleaseClaim(ctx, turnID, r)
}

func (s *refusingStore) LiftHold(ctx context.Context, turnID string, cond Condition, hold uint64) (bool, error) {
	if s.called("LiftHold") {
		return false, errRefusedCall
	}
	return s.inner.LiftHold(ctx, turnID, cond, hold)
}

func (s *refusingStore) MarkAwaiting(ctx context.Context, turnID string, q Clarification) error {
	if s.called("MarkAwaiting") {
		return errRefusedCall
	}
	return s.inner.MarkAwaiting(ctx, turnID, q)
}

func (s *refusingStore) ClaimOwnership(ctx context.Context, turnID, owner string, epoch int64) (bool, error) {
	if s.called("ClaimOwnership") {
		return false, errRefusedCall
	}
	return s.inner.ClaimOwnership(ctx, turnID, owner, epoch)
}

func (s *refusingStore) SetStatus(ctx context.Context, turnID, status string, fence Fence) error {
	if s.called("SetStatus") {
		return errRefusedCall
	}
	return s.inner.SetStatus(ctx, turnID, status, fence)
}

func (s *refusingStore) DecideEnding(ctx context.Context, turnID string, d Decision,
) (PendingRun, bool, error) {
	if s.called("DecideEnding") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.DecideEnding(ctx, turnID, d)
}

func (s *refusingStore) CollectStop(ctx context.Context, turnID string, c StopCollection,
) (PendingRun, bool, error) {
	if s.called("CollectStop") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.CollectStop(ctx, turnID, c)
}

func (s *refusingStore) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	if s.called("Finish") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.Finish(ctx, turnID, ending)
}

func (s *refusingStore) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	if s.called("ExpirePause") {
		return false, errRefusedCall
	}
	return s.inner.ExpirePause(ctx, turnID)
}

func (s *refusingStore) MarkLost(ctx context.Context, turnID, launch string, loss JobLoss) (bool, error) {
	if s.called("MarkLost") {
		return false, errRefusedCall
	}
	return s.inner.MarkLost(ctx, turnID, launch, loss)
}

func (s *refusingStore) MarkClaimLost(ctx context.Context, turnID, launch string, loss JobLoss,
	fence Fence,
) (bool, error) {
	if s.called("MarkClaimLost") {
		return false, errRefusedCall
	}
	return s.inner.MarkClaimLost(ctx, turnID, launch, loss, fence)
}

func (s *refusingStore) AttachSandbox(ctx context.Context, turnID string, box BoxRef, fence Fence) error {
	if s.called("AttachSandbox") {
		return errRefusedCall
	}
	return s.inner.AttachSandbox(ctx, turnID, box, fence)
}

func (s *refusingStore) MarkBoxPaused(ctx context.Context, turnID string, at time.Time) error {
	if s.called("MarkBoxPaused") {
		return errRefusedCall
	}
	return s.inner.MarkBoxPaused(ctx, turnID, at)
}

func (s *refusingStore) ReleaseBox(ctx context.Context, turnID string) error {
	if s.called("ReleaseBox") {
		return errRefusedCall
	}
	return s.inner.ReleaseBox(ctx, turnID)
}

func (s *refusingStore) MarkSuspended(ctx context.Context, turnID string, state Suspension) (bool, error) {
	if s.called("MarkSuspended") {
		return false, errRefusedCall
	}
	return s.inner.MarkSuspended(ctx, turnID, state)
}

func (s *refusingStore) TurnLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	if s.called("TurnLog") {
		return BridgeLog{}, errRefusedCall
	}
	return s.inner.TurnLog(ctx, run)
}

func (s *refusingStore) BridgeLog(ctx context.Context, run PendingRun) (BridgeLog, error) {
	if s.called("BridgeLog") {
		return BridgeLog{}, errRefusedCall
	}
	return s.inner.BridgeLog(ctx, run)
}

func (s *refusingStore) Handed(ctx context.Context, run PendingRun) (string, error) {
	if s.called("Handed") {
		return "", errRefusedCall
	}
	return s.inner.Handed(ctx, run)
}

func (s *refusingStore) AddHanded(ctx context.Context, run PendingRun,
	add func(sealed string) (string, string)) (bool, error) {
	if s.called("AddHanded") {
		return false, errRefusedCall
	}
	return s.inner.AddHanded(ctx, run, add)
}

func (s *refusingStore) Leads(ctx context.Context, run PendingRun) (TurnLeads, error) {
	if s.called("Leads") {
		return TurnLeads{}, errRefusedCall
	}
	return s.inner.Leads(ctx, run)
}

func (s *refusingStore) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	if s.called("WatchLog") {
		return nil, errRefusedCall
	}
	return s.inner.WatchLog(ctx, turnID)
}

func (s *refusingStore) AppendBridgeCall(ctx context.Context, turnID string, a BridgeAppend) (bool, error) {
	if s.called("AppendBridgeCall") {
		return false, errRefusedCall
	}
	return s.inner.AppendBridgeCall(ctx, turnID, a)
}

func (s *refusingStore) RecordAsk(ctx context.Context, turnID, launch string, ask Ask) (AskOutcome, error) {
	if s.called("RecordAsk") {
		return "", errRefusedCall
	}
	return s.inner.RecordAsk(ctx, turnID, launch, ask)
}

func (s *refusingStore) ListActive(ctx context.Context) ([]PendingRun, error) {
	if s.called("ListActive") {
		return nil, errRefusedCall
	}
	return s.inner.ListActive(ctx)
}

func (s *refusingStore) ListRuns(ctx context.Context) ([]PendingRun, error) {
	if s.called("ListRuns") {
		return nil, errRefusedCall
	}
	return s.inner.ListRuns(ctx)
}

func (s *refusingStore) AddCodeHolds(ctx context.Context, turnID string, keys []string) (bool, error) {
	if s.called("AddCodeHolds") {
		return false, errRefusedCall
	}
	return s.inner.AddCodeHolds(ctx, turnID, keys)
}

func (s *refusingStore) RaiseCodeFloor(ctx context.Context, turnID, launchID string,
	raise func(current string) (string, bool),
) (bool, error) {
	if s.called("RaiseCodeFloor") {
		return false, errRefusedCall
	}
	return s.inner.RaiseCodeFloor(ctx, turnID, launchID, raise)
}

func (s *refusingStore) GrantRepo(ctx context.Context, turnID, launchID string, g RepoGrant,
) (RunRepo, GrantOutcome, error) {
	if s.called("GrantRepo") {
		return RunRepo{}, "", errRefusedCall
	}
	return s.inner.GrantRepo(ctx, turnID, launchID, g)
}

func (s *refusingStore) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	if s.called("ListActiveForSeat") {
		return nil, errRefusedCall
	}
	return s.inner.ListActiveForSeat(ctx, handle)
}

func (s *refusingStore) ListActiveForSeatID(ctx context.Context, seat uuid.UUID) ([]PendingRun, error) {
	if s.called("ListActiveForSeatID") {
		return nil, errRefusedCall
	}
	return s.inner.ListActiveForSeatID(ctx, seat)
}

func (s *refusingStore) RecordAnswer(ctx context.Context, turnID, launch string,
	answer RecordedAnswer, fence Fence,
) (PendingRun, bool, error) {
	if s.called("RecordAnswer") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.RecordAnswer(ctx, turnID, launch, answer, fence)
}

func (s *refusingStore) RecordRefusal(ctx context.Context, turnID, launch string,
	answer RecordedAnswer, refusal AnswerRefusal, fence Fence,
) (PendingRun, bool, error) {
	if s.called("RecordRefusal") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.RecordRefusal(ctx, turnID, launch, answer, refusal, fence)
}

func (s *refusingStore) DeclineAnswer(ctx context.Context, turnID, launch string,
	answer []string, handBack []HandedBack, fence Fence,
) (PendingRun, bool, error) {
	if s.called("DeclineAnswer") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.DeclineAnswer(ctx, turnID, launch, answer, handBack, fence)
}

func (s *refusingStore) OweHandBack(ctx context.Context, turnID string, letGo LetGo,
) (PendingRun, bool, error) {
	if s.called("OweHandBack") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.OweHandBack(ctx, turnID, letGo)
}

func (s *refusingStore) ReviveAnswer(ctx context.Context, turnID string, revival Revival,
) (PendingRun, bool, error) {
	if s.called("ReviveAnswer") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.ReviveAnswer(ctx, turnID, revival)
}

func (s *refusingStore) TakeAnswer(ctx context.Context, turnID, launch string, fence Fence) (bool, error) {
	if s.called("TakeAnswer") {
		return false, errRefusedCall
	}
	return s.inner.TakeAnswer(ctx, turnID, launch, fence)
}

func (s *refusingStore) ClaimWaitOver(ctx context.Context, turnID, launch string, fence Fence,
) (PendingRun, bool, error) {
	if s.called("ClaimWaitOver") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.ClaimWaitOver(ctx, turnID, launch, fence)
}

func (s *refusingStore) TakeWaitOver(ctx context.Context, turnID, launch string, fence Fence) (bool, error) {
	if s.called("TakeWaitOver") {
		return false, errRefusedCall
	}
	return s.inner.TakeWaitOver(ctx, turnID, launch, fence)
}

func (s *refusingStore) ReviveWaitOver(ctx context.Context, turnID string, revival Revival,
) (PendingRun, bool, error) {
	if s.called("ReviveWaitOver") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.ReviveWaitOver(ctx, turnID, revival)
}

func (s *refusingStore) MarkHeldCall(ctx context.Context, turnID, launch string, fence Fence) (HeldCall, bool, error) {
	if s.called("MarkHeldCall") {
		return HeldCall{}, false, errRefusedCall
	}
	return s.inner.MarkHeldCall(ctx, turnID, launch, fence)
}

func (s *refusingStore) RecordHeldResult(ctx context.Context, turnID, launch string, made HeldMade,
	fence Fence) (bool, error) {
	if s.called("RecordHeldResult") {
		return false, errRefusedCall
	}
	return s.inner.RecordHeldResult(ctx, turnID, launch, made, fence)
}

func (s *refusingStore) GiveDelegateVerdict(ctx context.Context, turnID, launch, heldID string,
	v HeldVerdict) (HeldCall, bool, error) {
	if s.called("GiveDelegateVerdict") {
		return HeldCall{}, false, errRefusedCall
	}
	return s.inner.GiveDelegateVerdict(ctx, turnID, launch, heldID, v)
}

func (s *refusingStore) EscalateHeld(ctx context.Context, turnID, launch, heldID string,
	e HeldEscalation) (bool, error) {
	if s.called("EscalateHeld") {
		return false, errRefusedCall
	}
	return s.inner.EscalateHeld(ctx, turnID, launch, heldID, e)
}

func (s *refusingStore) ClearHandBack(ctx context.Context, turnID string, ids []string) (bool, error) {
	if s.called("ClearHandBack") {
		return false, errRefusedCall
	}
	return s.inner.ClearHandBack(ctx, turnID, ids)
}

func (s *refusingStore) FileNote(ctx context.Context, turnID string, filing NoteFiling,
) (NoteFiled, PendingRun, error) {
	if s.called("FileNote") {
		return "", PendingRun{}, errRefusedCall
	}
	return s.inner.FileNote(ctx, turnID, filing)
}

func (s *refusingStore) FileWake(ctx context.Context, turnID string, filing WakeFiling) (RunNote, error) {
	if s.called("FileWake") {
		return RunNote{}, errRefusedCall
	}
	return s.inner.FileWake(ctx, turnID, filing)
}

func (s *refusingStore) CountRunSend(ctx context.Context, turnID, job string, delta int) error {
	if s.called("CountRunSend") {
		return errRefusedCall
	}
	return s.inner.CountRunSend(ctx, turnID, job, delta)
}

func (s *refusingStore) ListActiveOn(ctx context.Context, on RunsOn) ([]PendingRun, error) {
	if s.called("ListActiveOn") {
		return nil, errRefusedCall
	}
	return s.inner.ListActiveOn(ctx, on)
}

func (s *refusingStore) AskFoldIn(ctx context.Context, turnID string, f FoldIn) (FoldIn, error) {
	if s.called("AskFoldIn") {
		return FoldIn{}, errRefusedCall
	}
	return s.inner.AskFoldIn(ctx, turnID, f)
}

func (s *refusingStore) AnswerFoldIn(ctx context.Context, turnID, id string, a FoldInAnswered) (FoldIn, error) {
	if s.called("AnswerFoldIn") {
		return FoldIn{}, errRefusedCall
	}
	return s.inner.AnswerFoldIn(ctx, turnID, id, a)
}

func (s *refusingStore) TakeNotes(ctx context.Context, turnID string, take NoteTake) ([]RunNote, error) {
	if s.called("TakeNotes") {
		return nil, errRefusedCall
	}
	return s.inner.TakeNotes(ctx, turnID, take)
}

func (s *refusingStore) ServeNotes(ctx context.Context, turnID string, serve NoteServe) ([]RunNote, error) {
	if s.called("ServeNotes") {
		return nil, errRefusedCall
	}
	return s.inner.ServeNotes(ctx, turnID, serve)
}

func (s *refusingStore) AgainNotes(ctx context.Context, turnID string, ask NoteAgain) ([]string, []string, error) {
	if s.called("AgainNotes") {
		return nil, nil, errRefusedCall
	}
	return s.inner.AgainNotes(ctx, turnID, ask)
}

func (s *refusingStore) SettleTake(ctx context.Context, turnID string, settle NoteSettle) ([]RunNote, error) {
	if s.called("SettleTake") {
		return nil, errRefusedCall
	}
	return s.inner.SettleTake(ctx, turnID, settle)
}

func (s *refusingStore) ServeHook(ctx context.Context, turnID string, serve HookServe) ([]RunNote, error) {
	if s.called("ServeHook") {
		return nil, errRefusedCall
	}
	return s.inner.ServeHook(ctx, turnID, serve)
}

func (s *refusingStore) SettleHook(ctx context.Context, turnID string, settle HookSettle) ([]RunNote, error) {
	if s.called("SettleHook") {
		return nil, errRefusedCall
	}
	return s.inner.SettleHook(ctx, turnID, settle)
}

func (s *refusingStore) MarkPulled(ctx context.Context, turnID, launch string) (bool, error) {
	if s.called("MarkPulled") {
		return false, errRefusedCall
	}
	return s.inner.MarkPulled(ctx, turnID, launch)
}

func (s *refusingStore) MarkSessionStart(ctx context.Context, turnID, launch string, start SessionStart) (bool, error) {
	if s.called("MarkSessionStart") {
		return false, errRefusedCall
	}
	return s.inner.MarkSessionStart(ctx, turnID, launch, start)
}

func (s *refusingStore) MarkBridgeConnected(ctx context.Context, turnID, launch string) (bool, error) {
	if s.called("MarkBridgeConnected") {
		return false, errRefusedCall
	}
	return s.inner.MarkBridgeConnected(ctx, turnID, launch)
}

func (s *refusingStore) BindSession(ctx context.Context, turnID string, bind SessionBind) (PendingRun, error) {
	if s.called("BindSession") {
		return PendingRun{}, errRefusedCall
	}
	return s.inner.BindSession(ctx, turnID, bind)
}

func (s *refusingStore) ReleaseSessionClaim(ctx context.Context, turnID, sessionID string) error {
	if s.called("ReleaseSessionClaim") {
		return errRefusedCall
	}
	return s.inner.ReleaseSessionClaim(ctx, turnID, sessionID)
}

func (s *refusingStore) OweRefusal(ctx context.Context, turnID, launch string, owed OwedRefusal,
	fence Fence,
) (bool, error) {
	if s.called("OweRefusal") {
		return false, errRefusedCall
	}
	return s.inner.OweRefusal(ctx, turnID, launch, owed, fence)
}

func (s *refusingStore) MarkEndedAtDeadline(ctx context.Context, turnID, launch string) (bool, error) {
	if s.called("MarkEndedAtDeadline") {
		return false, errRefusedCall
	}
	return s.inner.MarkEndedAtDeadline(ctx, turnID, launch)
}

func (s *refusingStore) MarkEndedAtTurnCap(ctx context.Context, turnID, launch string) (bool, error) {
	if s.called("MarkEndedAtTurnCap") {
		return false, errRefusedCall
	}
	return s.inner.MarkEndedAtTurnCap(ctx, turnID, launch)
}

func (s *refusingStore) MarkHarnessEnd(ctx context.Context, turnID, launch string, read EndRead) (bool, error) {
	if s.called("MarkHarnessEnd") {
		return false, errRefusedCall
	}
	return s.inner.MarkHarnessEnd(ctx, turnID, launch, read)
}

func (s *refusingStore) MarkResumeBegun(ctx context.Context, turnID, launch string, fence Fence,
	carry ResumeCarry) (bool, error) {

	if s.called("MarkResumeBegun") {
		return false, errRefusedCall
	}
	return s.inner.MarkResumeBegun(ctx, turnID, launch, fence, carry)
}

func (s *refusingStore) WatchNotes(ctx context.Context, turnID string) (<-chan NotesView, error) {
	if s.called("WatchNotes") {
		return nil, errRefusedCall
	}
	return s.inner.WatchNotes(ctx, turnID)
}

func (s *refusingStore) StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	if s.called("StampCapture") {
		return false, errRefusedCall
	}
	return s.inner.StampCapture(ctx, turnID, capture)
}

func (s *refusingStore) SettleBegun(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	if s.called("SettleBegun") {
		return false, errRefusedCall
	}
	return s.inner.SettleBegun(ctx, turnID, capture)
}

func (s *refusingStore) ReserveSave(ctx context.Context, turnID string, save SessionCapture) (bool, error) {
	if s.called("ReserveSave") {
		return false, errRefusedCall
	}
	return s.inner.ReserveSave(ctx, turnID, save)
}

func (s *refusingStore) StandBy(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	if s.called("StandBy") {
		return false, errRefusedCall
	}
	return s.inner.StandBy(ctx, turnID, capture)
}

func (s *refusingStore) TakeOver(ctx context.Context, turnID string, standby, capture SessionCapture) (bool, error) {
	if s.called("TakeOver") {
		return false, errRefusedCall
	}
	return s.inner.TakeOver(ctx, turnID, standby, capture)
}

func (s *refusingStore) StandDown(ctx context.Context, turnID string, standby SessionCapture) (bool, error) {
	if s.called("StandDown") {
		return false, errRefusedCall
	}
	return s.inner.StandDown(ctx, turnID, standby)
}

func (s *refusingStore) RetireSessions(ctx context.Context, turnID string, loss executorkeep.Loss,
	sessions []string) error {
	if s.called("RetireSessions") {
		return errRefusedCall
	}
	return s.inner.RetireSessions(ctx, turnID, loss, sessions)
}

func (s *refusingStore) OweEnd(ctx context.Context, turnID string, ending json.RawMessage) (PendingRun, bool, error) {
	if s.called("OweEnd") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.OweEnd(ctx, turnID, ending)
}

func (s *refusingStore) ClearEnd(ctx context.Context, turnID string, owed json.RawMessage) (bool, error) {
	if s.called("ClearEnd") {
		return false, errRefusedCall
	}
	return s.inner.ClearEnd(ctx, turnID, owed)
}
