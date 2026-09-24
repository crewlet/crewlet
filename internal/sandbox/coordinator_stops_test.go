package sandbox

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// THE ONE PROPERTY: a call that takes a run out of the set of things that will
// come back must tell the engine that the turn stopped.
//
// # Why this file exists at all
//
// Four times now a path has ended or stranded a run whose turn was still
// suspended into it and told nothing above this package, and four times the
// symptom was a working indicator claiming an agent was busy for the life of the
// process. Each round closed the path it found by hand-listing the others and
// declaring them safe; each of those lists was wrong. Hand-enumeration is the
// defect, so the enumeration is taken out of the reviewer's hands here.
//
// # What it checks, and why the premise is what it is
//
// For every exported [Coordinator] entry point, driven once per status a run
// can hold, and each of those driven once cleanly and then once per store call
// that drive makes with THAT CALL REFUSED: if the run was still something that
// would come back before the call and is not after it, and this call did not
// hand the turn to the resumer, then [CoordinatorOptions.Stopped] must have
// fired for it.
//
// ALL THREE DIMENSIONS ARE DERIVED, which is the whole mechanism — a path that
// only misreports from one entry point, under one refusal, out of one starting
// status is found by the matrix rather than by whoever reads the diff:
//
//   - THE ENTRY POINTS are the coordinator's own exported method set, held to
//     [coordinatorEntries] in both directions by
//     [TestEveryCoordinatorEntryPointIsDriven].
//   - THE REFUSALS are OBSERVED, never declared: whatever the drive asked the
//     store for is what gets refused, so a path that starts calling something
//     new is injected into on the next run without anybody updating a list.
//   - THE STARTING STATUS is [Active], every status a record can hold, and
//     [place] fails rather than skips on one it does not know — so a status
//     added to the state machine has to say how a run reaches it before
//     anything here can certify what happens from there. It was hand-written
//     per drive, which is the same enumeration this file exists to end: a call
//     that misreports only from a status nobody thought to arrange looks
//     exactly like a call that is safe.
//
// NO (ENTRY, STATUS) PAIR IS SKIPPED, and none needs to be. Every entry point
// here is reached by something that carries no promise about the row — a
// broker delivery that is at-least-once and may be hours late, a seat lease
// changing hands, a retirement — so the status is whatever the store holds
// when the call lands, including the ones that make the call a no-op. A pair
// that does nothing costs one cheap rig and proves it does nothing.
//
// # What it deliberately does not simulate
//
// Concurrency. Every failure injected is a store REFUSAL, never a second party
// acting on the same row, so `ended == false` — the losing half of a race — is
// out of scope by construction and has its own case
// ([TestASettleSomebodyElseEndedReportsNoStop]). A property test that raced
// would have to decide which party owes the report, which is a different
// question from this one.

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
// A ROSTER RATHER THAN A HAND-PICKED LIST, guarded in both directions by
// [TestEveryCoordinatorEntryPointIsDriven]: a method added to the coordinator
// fails the build's tests until somebody writes down how it reaches a run, and
// an entry naming a method that no longer exists fails too. That is the whole
// mechanism — the next path that ends a suspended run silently is caught by a
// test rather than by whoever reads the diff.
//
// A DRIVE VARIES WHAT IS AROUND THE RUN, NEVER THE RUN'S OWN STATUS, which is
// [place]'s dimension: two drives differing only in where the run had got to
// would be a hand-written copy of the one the matrix already derives.
//
// Some drives are one line and prove the property trivially, because the method
// cannot move a run at all (it reads a count, or swaps the manager). That is the
// honest way to record it: an exemption list would be a second place to state
// the same judgement, and a wrong exemption is invisible where a trivial drive
// is at least visible.
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
			if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
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
				answerOnTheDM, "the release branch", nil); err != nil {
				t.Logf("TryResumeFromAnswer: %v", err)
			}
		},
	}},
	"FailRun": {{
		name: "a suspension that never reached the row",
		call: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.FailRun(t.Context(), "t1",
				types.SandboxFailureSuspensionUnrecorded,
				"the suspended conversation could not be written"); err != nil {
				t.Logf("FailRun: %v", err)
			}
		},
	}},
	"RecoverSeat": {{
		name: "a seat claimed by a new owner",
		call: recovers,
	}},
	"RetireSeat": {{
		name: "the runs of a seat that left the company",
		call: func(t *testing.T, rig *coordRig) {
			if err := rig.coordinator.RetireSeat(t.Context(), "swe", "node-a:1", 3); err != nil {
				t.Logf("RetireSeat: %v", err)
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
	"Manager": {{
		name: "the manager a caller mints a box through",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.Manager() },
	}},
	"SetManager": {{
		name: "a live reload of providers.sandbox",
		call: func(_ *testing.T, rig *coordRig) { rig.coordinator.SetManager(rig.manager) },
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
			CompletionTail(run.LaunchID)); err != nil || !won {
			t.Fatalf("ClaimForResume = %v, %v", won, err)
		}
	},
	StatusAwaiting: func(_ *testing.T, rig *coordRig) {
		rig.launch("t1")
		rig.park("t1")
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

// jobFinished queues the result a finished coding job comes back with, so a
// completion has something to collect.
func jobFinished(_ *testing.T, rig *coordRig) {
	rig.runner.Finish(Result{Success: true, Text: "done"})
}

// jobAsks is the job that stopped to ask a person something.
func jobAsks(_ *testing.T, rig *coordRig) {
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})
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

// noConversation is a row with nothing to resume INTO: what a build predating
// [StatusLaunching] wrote, read by this one across a rolling upgrade.
//
// [PendingStore.BeginLaunch] is the one write that drops a suspension, so the
// status [place] chose is put back after it.
func noConversation(t *testing.T, rig *coordRig) {
	jobFinished(t, rig)
	run := rig.get("t1")
	if err := rig.pending.BeginLaunch(t.Context(), run, Fence{}); err != nil {
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
// row refused, which is the one failure that reaches the coordinator INSIDE a
// tail it cannot repeat: the turn has been re-entered, so the delivery cannot
// come round again.
//
// Its own drive rather than a refusal of the matrix's, because the refusal
// dimension refuses a call for the whole drive and the claim reads the row too.
//
// WHAT THE MATRIX DOES NOT ASSERT HERE IS THE REPORT, and that is written down
// rather than left to be discovered, because a drive presenting itself as
// coverage it lacks is the defect this file exists to end and not an instance
// of it. The settle this reaches ([Coordinator.settleClaimed]) has two call
// sites and both sit behind a resume that returned nil or [ErrResumeActed] —
// every other resume failure gives the claim back instead, which is
// [Coordinator.unclaim]'s path and not this one. Both of those count as an
// OWNED re-entry, so from the three statuses whose claim is won
// ([StatusRunning], [StatusAwaiting], [StatusReseed]) [entryDrive.run] takes
// the turn-ended-its-own-hold exemption before it asks whether a stop was
// reported; from [StatusLaunching] and [StatusResumed] the claim is refused
// and the earlier skip fires. So what this drive certifies is that the path
// RUNS from every status with that read refused, and nothing about what it
// says. That the settle REPORTS is held by
// [TestASettleWhoseRecordCannotBeReadEndsTheRunAnyway], which drives both
// resume outcomes by name.
func completesUnreadable(t *testing.T, rig *coordRig) {
	t.Helper()
	rig.coordinator.pending = &refusingStore{
		inner: rig.coordinator.pending, refuse: []string{"Get"},
	}
	completes(t, rig)
}

// recovers takes the seat under a fresh lease, as a node claiming it does.
func recovers(t *testing.T, rig *coordRig) {
	t.Helper()
	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-b:1", 9); err != nil {
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
// stack and ends it itself.
func willComeBack(run PendingRun, found bool) bool {
	return found && (slices.Contains(Claimable, run.Status) || run.Status == StatusLaunching)
}

func describeRow(run PendingRun, found bool) string {
	if !found {
		return "no record at all"
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

func (s *refusingStore) BeginLaunch(ctx context.Context, run PendingRun, fence Fence) error {
	if s.called("BeginLaunch") {
		return errRefusedCall
	}
	return s.inner.BeginLaunch(ctx, run, fence)
}

func (s *refusingStore) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	if s.called("Get") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.Get(ctx, turnID)
}

func (s *refusingStore) ClaimForResume(ctx context.Context, turnID string, tail Tail) (PendingRun, bool, error) {
	if s.called("ClaimForResume") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.ClaimForResume(ctx, turnID, tail)
}

func (s *refusingStore) ReleaseClaim(ctx context.Context, turnID string, r Release) (bool, error) {
	if s.called("ReleaseClaim") {
		return false, errRefusedCall
	}
	return s.inner.ReleaseClaim(ctx, turnID, r)
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

func (s *refusingStore) Finish(ctx context.Context, turnID string, fence Fence, whileIn []string,
) (PendingRun, bool, error) {
	if s.called("Finish") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.Finish(ctx, turnID, fence, whileIn)
}

func (s *refusingStore) ExpirePause(ctx context.Context, turnID string) (bool, error) {
	if s.called("ExpirePause") {
		return false, errRefusedCall
	}
	return s.inner.ExpirePause(ctx, turnID)
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

func (s *refusingStore) AppendBridgeCall(ctx context.Context, turnID string, call BridgeCall) (bool, error) {
	if s.called("AppendBridgeCall") {
		return false, errRefusedCall
	}
	return s.inner.AppendBridgeCall(ctx, turnID, call)
}

func (s *refusingStore) ListActive(ctx context.Context) ([]PendingRun, error) {
	if s.called("ListActive") {
		return nil, errRefusedCall
	}
	return s.inner.ListActive(ctx)
}

func (s *refusingStore) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	if s.called("ListActiveForSeat") {
		return nil, errRefusedCall
	}
	return s.inner.ListActiveForSeat(ctx, handle)
}

func (s *refusingStore) FindAwaitingByConversation(ctx context.Context, handle string,
	conv ConversationRef,
) (PendingRun, bool, error) {
	if s.called("FindAwaitingByConversation") {
		return PendingRun{}, false, errRefusedCall
	}
	return s.inner.FindAwaitingByConversation(ctx, handle, conv)
}
