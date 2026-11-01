package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

var workletItem = types.WorkItem{Backend: types.WorkWorklet, ID: "t-1", Key: "ENG-1", Project: "ENG"}

// segmentSpend is one segment's runner tally, with every figure a charge reads
// set to something distinct.
func segmentSpend(scale int) runner.Spend {
	return runner.Spend{
		InputTokens: 1000 * scale, OutputTokens: 200 * scale,
		CacheRead: 300 * scale, CacheWrite: 40 * scale,
		Workers: 2, WorkerInput: 500 * scale, WorkerOutput: 60 * scale,
		Judged: 1, JudgeInput: 70 * scale, JudgeOutput: 9 * scale,
		Rounds: 4, SentBack: 1, Phases: []string{"execute", "review"},
	}
}

// rollupOf is what a segment's own published records add up to: its phases'
// tokens, its workers', its judge's, its in-turn auxiliary calls' (the
// `turn`-stage auxiliary_spend records filed under its run) and the job it
// collected. The per-seat history and the turn's cost on every screen are
// built from these same records, so no charge to a task may ever come to
// more than they do.
func rollupOf(s runner.Spend, aux auxspend.Spent, jobIn, jobOut int) int {
	return s.Total() + s.WorkerTokens() + s.JudgeTokens() + aux.Tokens() + jobIn + jobOut
}

// jobEngineSpend is what the engine spent on a collected job outside every
// segment: its bridged calls' auxiliary spend and workers, and the
// condensation of its collection. Published as `turn`-stage auxiliary_spend
// records under the run and as the workers' own phase records, so it is part
// of the rollup like everything else.
var jobEngineSpend = sandbox.EngineSpend{
	Aux:     sandbox.AuxTokens{Input: 640, Output: 45, CacheRead: 200, CacheWrite: 12},
	Workers: 1, WorkerInput: 2100, WorkerOutput: 180,
}

// engineTokens is what an engine spend adds to the rollup.
func engineTokens(s sandbox.EngineSpend) int {
	return s.Aux.Input + s.Aux.Output + s.WorkerInput + s.WorkerOutput
}

// segmentAux is one segment's in-turn auxiliary spend, distinct per scale.
func segmentAux(scale int) auxspend.Spent {
	return auxspend.Spent{Calls: 3, Input: 80 * scale, Output: 12 * scale, CacheRead: 40 * scale}
}

// fixtureRun is the run every case here charges for: a run id as the engine
// mints one, time-ordered, carrying the instant the run began.
const fixtureRun = "018bcfe5-6800-7000-8000-000000000001"

func dispatchTel(item *types.WorkItem) turnTelemetry {
	t := turnTelemetry{
		handle: "dev", runID: fixtureRun, trigger: types.Trigger{Type: "work_item"},
		startedAt: time.Unix(1_700_000_000, 0).UTC(), written: &turnctx.Written{},
		auxSpent: auxspend.NewTally(),
	}
	if item != nil {
		copied := *item
		t.workItem, t.workItemBasis = &copied, types.BasisTrigger
	}
	return t
}

func resumeTel(item *types.WorkItem, carried *execstate.Uncharged, jobIn, jobOut int) turnTelemetry {
	t := dispatchTel(item)
	if item != nil {
		t.workItemBasis = types.BasisResume
	}
	t.resumed, t.launchID = true, "launch-1"
	t.jobInput, t.jobOutput, t.uncharged = jobIn, jobOut, carried
	// A RESUMED SEGMENT'S OWN TALLY, as describeResume gives it.
	t.auxSpent = auxspend.NewTally()
	return t
}

// A TASK'S SPEND NEVER EXCEEDS THE ROLLUP.
//
// What a turn charges its work item is a SHARE of what the turn's own records
// state it spent — the phases, the workers, the judge, the auxiliary calls made
// inside it and the launches it collected — and, across every segment of a
// turn, never more than their sum.
// Folding anything in twice (a resumed phase's pre-park half, a worker counted
// inside its host phase and again beside it) would show a task costing more
// than the company paid for it.
func TestTaskSpendNeverExceedsTheRollup(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()

	for _, tc := range []struct {
		name string
		item *types.WorkItem
		// sole is the item the turn's writes committed to, for a turn
		// dispatch named nothing for.
		sole *types.WorkItem
	}{
		{name: "an item from the trigger", item: &workletItem},
		{name: "an item from a sole write", sole: &workletItem},
		{name: "no item at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, second := segmentSpend(1), segmentSpend(3)
			jobIn, jobOut := 4000, 700

			dispatch := dispatchTel(tc.item)
			dispatch.auxSpent.Add(segmentAux(1))
			parked := dispatch.chargeFor(first, turn.Result{Suspended: true}, nil, ended)

			resume := resumeTel(tc.item, parked.carry, jobIn, jobOut)
			resume.auxSpent.Add(segmentAux(3))
			// AND WHAT THE ENGINE SPENT ON THE JOB BETWEEN THE TWO — its
			// bridged calls, its collection's condensation — which no
			// segment was running to tally, and which the resume pays.
			resume.jobEngine = jobEngineSpend
			if tc.sole != nil {
				resume.written = turnctx.WrittenFrom([]types.WorkItem{*tc.sole}, false)
			}
			finished := resume.chargeFor(second, turn.Result{Decision: phase.Done}, nil, ended)

			charged := 0
			for _, c := range []segmentCharge{parked, finished} {
				if c.onWorklet() {
					charged += c.record.Spend.Tokens()
				}
			}
			rollup := rollupOf(first, segmentAux(1), 0, 0) + rollupOf(second, segmentAux(3), jobIn, jobOut) +
				engineTokens(jobEngineSpend)
			if charged > rollup {
				t.Fatalf("the task was charged %d tokens for a turn whose records "+
					"add up to %d", charged, rollup)
			}
			// AND PAID IN FULL wherever the turn is charged at all: a
			// share short of the rollup is spend that went nowhere.
			if (tc.item != nil || tc.sole != nil) && charged != rollup {
				t.Fatalf("the task was charged %d tokens of the %d its turn "+
					"spent", charged, rollup)
			}
			if tc.item == nil && tc.sole == nil && charged != 0 {
				t.Fatalf("a turn on nothing charged %d tokens", charged)
			}
		})
	}
}

// A RESUMED SEGMENT PAYS WHAT THE ENGINE SPENT ON ITS JOB.
//
// Between the segment that launched the executor and the one that resumes from
// it, the engine spends on the job with no segment running: a run's bridged
// calls — the auxiliary rewrites its tools asked for, the workers it delegated
// to — and the condensation of the run's account at collection.
// The resume request carries it, and the resumed segment's charge adds each
// figure where its own of the same kind goes.
//
// Mutation: leave the job's engine spend out of chargeFor, and every figure
// below comes up short.
func TestAResumedSegmentPaysWhatTheEngineSpentOnItsJob(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()
	resume := resumeTel(&workletItem, nil, 0, 0)
	resume.jobEngine = jobEngineSpend
	spend := segmentSpend(1)
	got := resume.chargeFor(spend, turn.Result{Decision: phase.Done}, nil, ended).record.Spend

	// Every figure the job's engine spend carries lands where the segment's
	// own of the same kind does: workers beside its workers, auxiliary
	// tokens beside its own auxiliary calls, the cache shares as a
	// breakdown of the input they came with.
	aux, job := jobEngineSpend.Aux, jobEngineSpend
	if want := spend.InputTokens + spend.WorkerInput + spend.JudgeInput +
		job.WorkerInput + aux.Input; got.Input != want {
		t.Errorf("input = %d, want %d with the job's workers and auxiliary calls", got.Input, want)
	}
	if want := spend.OutputTokens + spend.WorkerOutput + spend.JudgeOutput +
		job.WorkerOutput + aux.Output; got.Output != want {
		t.Errorf("output = %d, want %d", got.Output, want)
	}
	if want := spend.CacheRead + aux.CacheRead; got.CacheRead != want {
		t.Errorf("cache read = %d, want %d", got.CacheRead, want)
	}
	if want := spend.CacheWrite + aux.CacheWrite; got.CacheWrite != want {
		t.Errorf("cache write = %d, want %d", got.CacheWrite, want)
	}
	if want := spend.Workers + job.Workers; got.Workers != want {
		t.Errorf("workers = %d, want %d with the ones the harness delegated to", got.Workers, want)
	}
	if got.Turns != 0 {
		t.Errorf("a resumed segment counted %d turns", got.Turns)
	}
}

// A SUSPENDED TURN IS COUNTED ONCE AND PAID IN FULL.
//
// One turn, two segments, one turn on the task: only the dispatch segment
// counts a turn, the resumed segment pays for the launch it collected, and
// a turn charged by its sole write — which a parked segment cannot conclude —
// is paid for its first half by the segment that finishes it.
func TestASuspendedTurnIsCountedOnceAndPaidInFull(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()

	t.Run("charged at dispatch", func(t *testing.T) {
		parked := dispatchTel(&workletItem).chargeFor(segmentSpend(1),
			turn.Result{Suspended: true}, nil, ended)
		finished := resumeTel(&workletItem, nil, 4000, 700).chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended)
		if !parked.onWorklet() || !finished.onWorklet() {
			t.Fatal("a segment of a turn on a Worklet item charged nothing")
		}
		if parked.record.Spend.Turns+finished.record.Spend.Turns != 1 {
			t.Fatalf("one turn counted %d times across its segments",
				parked.record.Spend.Turns+finished.record.Spend.Turns)
		}
		if parked.carry != nil {
			t.Errorf("a charged segment handed on %+v — it would be paid twice", parked.carry)
		}
		if parked.record.Outcome != "suspended" || finished.record.Outcome != "done" {
			t.Errorf("outcomes %q/%q, want the park told apart from the end",
				parked.record.Outcome, finished.record.Outcome)
		}
		if parked.opID == finished.opID {
			t.Errorf("both segments recorded under %q, so the second is dropped as "+
				"a redelivery of the first", parked.opID)
		}
		if got, want := finished.record.Spend.Input, 1000+500+70+4000; got != want {
			t.Errorf("the resumed segment's input is %d, want its phases, workers, "+
				"judge and the collected job: %d", got, want)
		}
	})

	t.Run("charged at the end by a sole write", func(t *testing.T) {
		parked := dispatchTel(nil).chargeFor(segmentSpend(1),
			turn.Result{Suspended: true}, nil, ended)
		if parked.onWorklet() || parked.carry == nil || parked.carry.Turns != 1 {
			t.Fatalf("a parked segment on nothing charged %v and handed on %+v, "+
				"want nothing charged and the turn handed on", parked.onWorklet(), parked.carry)
		}
		resume := resumeTel(nil, parked.carry, 4000, 700)
		resume.written = turnctx.WrittenFrom([]types.WorkItem{workletItem}, false)
		finished := resume.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Done}, nil, ended)
		if !finished.onWorklet() || finished.record.Task != "t-1" {
			t.Fatalf("the finishing segment charged %+v, want its sole write", finished.record)
		}
		if finished.record.Spend.Turns != 1 {
			t.Errorf("the turn counted %d times, want the dispatch's one carried",
				finished.record.Spend.Turns)
		}
		if got, want := finished.record.Spend.Input, 2*(1000+500+70)+4000; got != want {
			t.Errorf("the turn's input charged is %d, want both halves and the job: %d", got, want)
		}
	})
}

// AN UNATTRIBUTED TURN CHARGES NOTHING.
//
// A turn on nothing, or on another tracker's item, has no Worklet row to add
// to, and a charge that guessed an item would put one turn's cost on work it
// was not doing.
func TestAnUnattributedTurnChargesNothing(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()
	jira := types.WorkItem{Backend: types.WorkJira, ID: "10001", Key: "OPS-1"}
	two := dispatchTel(nil)
	two.written.Add(workletItem)
	two.written.Add(types.WorkItem{Backend: types.WorkWorklet, ID: "t-2", Key: "ENG-2"})

	for name, c := range map[string]segmentCharge{
		"on nothing": dispatchTel(nil).chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended),
		"on a jira issue": dispatchTel(&jira).chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended),
		"writing two items": two.chargeFor(segmentSpend(1),
			turn.Result{Decision: phase.Done}, nil, ended),
	} {
		if c.onWorklet() {
			t.Errorf("a turn %s charged %+v to Worklet", name, c.record)
		}
		if c.carry != nil {
			t.Errorf("a finished turn %s handed on %+v, which nothing will ever pay", name, c.carry)
		}
	}
}

// A SEGMENT'S OPERATION ID NAMES THE SEGMENT, and nothing else.
//
// The id is the turn row's own, so a resume re-run after a failed attempt must
// reach the SAME id — its spend is then counted once — while two launches of
// one turn must reach two.
func TestASegmentsOperationIDIsStableAcrossARetry(t *testing.T) {
	t.Parallel()
	first := dispatchTel(&workletItem)
	first.resumed, first.launchID = true, "launch-1"
	retry := first
	retry.startedAt = retry.startedAt.Add(time.Minute)
	ended := time.Unix(1_700_000_600, 0).UTC()
	if a, b := first.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Done}, nil, ended),
		retry.chargeFor(segmentSpend(2), turn.Result{Decision: phase.Done}, nil, ended); a.opID != b.opID {
		t.Fatalf("a re-run segment named a new operation: %q then %q", a.opID, b.opID)
	}
	var never time.Time
	for _, pair := range [][2]string{
		{segmentOpID("run-1", "", segmentDispatch, never), segmentOpID("run-1", "launch-1", segmentResume, never)},
		{segmentOpID("run-1", "launch-1", segmentResume, never), segmentOpID("run-1", "launch-2", segmentResume, never)},
		{segmentOpID("run-1", "", segmentDispatch, never), segmentOpID("run-2", "", segmentDispatch, never)},
		// A STOP'S CLOSE IS A SEGMENT OF ITS OWN: under the park's id the
		// ledger would collapse it, and the task would read parked for good.
		{segmentOpID("run-1", "", segmentDispatch, never), segmentOpID("run-1", "", segmentStop, never)},
		{segmentOpID("run-1", "launch-1", segmentResume, never), segmentOpID("run-1", "launch-1", segmentStop, never)},
	} {
		if pair[0] == pair[1] {
			t.Errorf("two segments share the operation %q", pair[0])
		}
	}
}

// fakeRecorder is a turn recorder that remembers what it was asked.
type fakeRecorder struct {
	calls []string
	err   error
}

func (f *fakeRecorder) RecordTurn(_ context.Context, opID string, _ worklet.TurnRecord) (worklet.WriteResult, error) {
	f.calls = append(f.calls, opID)
	return worklet.WriteResult{}, f.err
}

// A CHARGE THAT CANNOT BE WRITTEN DOES NOT FAIL THE TURN.
//
// The turn has finished and its answer is delivered; the charge is telemetry,
// and a tracker that refuses it costs the task its share and nothing else.
func TestAnUnwritableChargeIsLoggedNotRaised(t *testing.T) {
	t.Parallel()
	c := dispatchTel(&workletItem).chargeFor(segmentSpend(1),
		turn.Result{Decision: phase.Done}, nil, time.Unix(1_700_000_060, 0).UTC())
	rec := &fakeRecorder{err: errors.New("the tracker is behind")}
	(&Engine{}).chargeSegment(t.Context(), rec, c)
	if len(rec.calls) != 1 || rec.calls[0] != segmentOpID(fixtureRun, "", segmentDispatch, time.Time{}) {
		t.Fatalf("the recorder was asked %v, want the dispatch segment once", rec.calls)
	}
}

// A SEGMENT'S OPERATION ID IS IN THE GRAMMAR, minted at the run's start.
//
// The tracker's ledger reads the instant an id was minted to decide whether it
// can vouch for it; an id that carries none is answered `unknown` and never
// published on a node whose ledger has lost a row, so a segment's spend would
// silently go uncharged. And the instant is the RUN's — a resumed segment
// minutes later shares it — never the segment's or the call's.
//
// Mutation: go back to `turn/<run>/<segment>`, or mint at the segment's start,
// and this fails.
func TestASegmentsOperationIDIsMintedAtTheRunsStart(t *testing.T) {
	t.Parallel()
	began, ok := statelog.OpMintedAt(fixtureRun)
	if !ok {
		t.Fatal("the fixture run id carries no instant, so this case proves nothing")
	}
	for _, id := range []string{
		segmentOpID(fixtureRun, "", segmentDispatch, time.Time{}), segmentOpID(fixtureRun, "launch-1", segmentResume, time.Time{}),
	} {
		if err := statelog.CheckCallerOpID(id); err != nil {
			t.Errorf("segment id %q is outside the operation-id grammar: %v", id, err)
		}
		if at, minted := statelog.OpMintedAt(id); !minted || !at.Equal(began) {
			t.Errorf("segment id %q was minted at %v (%v), want the run's start %v",
				id, at, minted, began)
		}
	}
}

// A REBASED SEGMENT MINTS AT ITS REBASE (ADR-0029), and every attempt that
// carries the same rebase derives the same id.
//
// A segment that comes longer after its run began than the operation ledger
// remembers — a resume a month after its launch — is minted at the instant
// [rebaseFor] chose for the attempt, or its charge is answered `unknown` on
// every node and the segment's spend is lost. And the id is still the
// segment's: a retry carrying the recorded rebase reaches the same one.
//
// Mutation: ignore rebasedTo in [segmentOpID] and this fails.
func TestARebasedSegmentIsMintedAtItsRebase(t *testing.T) {
	t.Parallel()
	began, ok := statelog.OpMintedAt(fixtureRun)
	if !ok {
		t.Fatal("the fixture run id carries no instant, so this case proves nothing")
	}
	rebased := began.Add(40 * 24 * time.Hour)
	id := segmentOpID(fixtureRun, "launch-1", segmentResume, rebased)
	if err := statelog.CheckCallerOpID(id); err != nil {
		t.Fatalf("rebased segment id %q is outside the operation-id grammar: %v", id, err)
	}
	if at, minted := statelog.OpMintedAt(id); !minted || !at.Equal(rebased) {
		t.Errorf("rebased segment id %q was minted at %v (%v), want the rebase %v",
			id, at, minted, rebased)
	}
	if again := segmentOpID(fixtureRun, "launch-1", segmentResume, rebased); again != id {
		t.Errorf("a retry carrying the same rebase named %q, want %q", again, id)
	}
	if id == segmentOpID(fixtureRun, "launch-1", segmentResume, time.Time{}) {
		t.Error("a rebased segment derived the id its run's start mints")
	}
	// AND THE CHARGE CARRIES IT: the telemetry's rebase is what the
	// segment's charge is published under.
	tel := dispatchTel(&workletItem)
	tel.resumed, tel.launchID, tel.rebasedTo = true, "launch-1", rebased
	c := tel.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Done}, nil,
		rebased.Add(time.Minute))
	if c.opID != id {
		t.Errorf("a rebased attempt charged under %q, want %q", c.opID, id)
	}
}

// A TASK'S TURN RECORD SAYS WHAT THE SEGMENT DID.
//
// The task page's turn card is read off the tracker's own row, which outlives
// the event history and answers on any node — so the summary, the send-back's
// request and the tools have to ride the record. The text rides WHOLE until
// the write, where [turnCard] fits it (an overlong one is refused, not cut,
// by the writer).
func TestATurnRecordSaysWhatTheSegmentDid(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()
	spend := segmentSpend(1)
	spend.Review = strings.Repeat("add a test for the timeout path. ", 40)
	spend.AllTools = []string{"create_branch", "tracker_comment", "tracker_comment",
		"search_knowledge", "tracker_comment", runner.SubmitWorkTool}
	res := turn.Result{
		Decision:   phase.Done,
		LastReview: &turn.Review{Decision: phase.Done, CompletedWork: "added the retry"},
	}

	got := dispatchTel(&workletItem).chargeFor(spend, res, nil, ended).record

	if got.Summary != "added the retry" {
		t.Errorf("summary = %q, want the reviewer's account of what landed", got.Summary)
	}
	if got.Review != spend.Review {
		t.Errorf("review = %d bytes, want the send-back's notes whole until the write",
			len(got.Review))
	}
	want := []worklet.TurnTool{
		{Name: "create_branch", Calls: 1}, {Name: "tracker_comment", Calls: 3},
		{Name: "search_knowledge", Calls: 1},
	}
	if !slices.Equal(got.Tools, want) {
		t.Errorf("tools = %+v, want each tool once in first-call order with its "+
			"count, and not the phase's own answer %+v", got.Tools, want)
	}
}

// A FAILED SEGMENT NAMES THE PHASE THAT BROKE, and only a failed one does: a
// card ticking every phase that ran beside a "failed" pill could not say which
// step broke, and a phase name beside a turn that ended otherwise — a person's
// stop is not a failure — would claim a failure that did not happen.
func TestAFailedSegmentNamesThePhaseThatBroke(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()
	spend := segmentSpend(1)
	spend.FailedIn = "review"

	failed := dispatchTel(&workletItem).chargeFor(spend, turn.Result{Decision: phase.Failed},
		errors.New("provider refused"), ended).record
	if failed.Outcome != string(phase.Failed) || failed.FailedIn != "review" {
		t.Errorf("a failed segment recorded outcome %q in %q, want failed in review",
			failed.Outcome, failed.FailedIn)
	}

	done := dispatchTel(&workletItem).chargeFor(spend, turn.Result{Decision: phase.Done},
		nil, ended).record
	if done.FailedIn != "" {
		t.Errorf("a segment that ended %q names a failed phase %q", done.Outcome, done.FailedIn)
	}
}

type cardModels struct {
	answer string
	err    error
}

func (m cardModels) Auxiliary(*org.Role, auxspend.Use) (chain.Member, error) {
	if m.err != nil {
		return chain.Member{}, m.err
	}
	return chain.Member{Key: "aux", Provider: cardProvider(m)}, nil
}

type cardProvider cardModels

func (cardProvider) Model() string { return "aux" }
func (p cardProvider) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{Content: p.answer}, nil
}

// A TURN'S CARD IS WHOLE, REWRITTEN OR POINTED AWAY — NEVER CUT. The card is
// the one line a person reads for a turn, and a summary cut at its cap read as
// the turn's whole account.
func TestATurnCardIsWholeRewrittenOrPointedAway(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Writer"}
	if got := turnCard(t.Context(), compact.Bound{}, "fixed it"); got != "fixed it" {
		t.Errorf("a short account was altered: %q", got)
	}
	long := strings.Repeat("The turn investigated the flaky test and ", 40) + "opened !42."
	fit := compact.New(cardModels{answer: "Fixed the flaky test; opened !42."}, compact.NewCache()).
		For(seat, auxspend.Use{Stage: types.AuxStageTurn, TurnID: "run-1"})
	if got := turnCard(t.Context(), fit, long); got != condensedCard+"Fixed the flaky test; opened !42." {
		t.Errorf("a long account was not rewritten and marked: %q", got)
	}
	got := turnCard(t.Context(), compact.Bound{}, long)
	if len(got) > worklet.MaxTurnSummary || strings.Contains(got, "investigated") ||
		!strings.Contains(got, "open the turn") {
		t.Errorf("an account with no rewrite was %q, want a pointer to the turn and no fragment", got)
	}
}

// A TURN'S CARD IS PAID FOR BY ITS TASK. The card is rewritten after the
// segment's charge was decided — the segment's tally already summed — so the
// rewrite tallies on its own and is added as the charge is written: a long
// account condensed for its card is part of what the turn cost the task, like
// every other auxiliary call the turn made, and is filed under the turn.
func TestACardsRewriteIsChargedToItsTask(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Dev"}
	c := meteredCompany(config.TokenBudget{}, seat)
	registry, err := phase.NewRegistry([]phase.Entry{{Key: "cheap", Provider: &cardRewriter{
		answer: "Fixed the flaky test; opened !42.", in: 900, out: 30}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = registry
	pub := &capturedEvents{}
	e := &Engine{auxSpend: auxspend.NewLedger(pub)}
	e.epoch.current.Store(c)

	tel := dispatchTel(&workletItem)
	tel.handle = seat.Handle()
	tel.auxSpent.Add(segmentAux(1))
	charge := tel.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Done}, nil,
		time.Unix(1_700_000_060, 0).UTC())
	before := charge.record.Spend
	charge.record.Summary = strings.Repeat("The turn investigated the flaky test and ", 40) + "opened !42."

	charge = e.withCards(t.Context(), charge)
	if charge.record.Summary != condensedCard+"Fixed the flaky test; opened !42." {
		t.Fatalf("the card was not rewritten: %q", charge.record.Summary)
	}
	if got, want := charge.record.Spend.Tokens(), before.Tokens()+930; got != want {
		t.Fatalf("the task is charged %d tokens, want %d: the segment's own and the "+
			"card rewrite's 930", got, want)
	}
	e.auxSpend.Flush(t.Context())
	recs := pub.records(t)
	if len(recs) != 1 || recs[0].Stage != types.AuxStageTurn || recs[0].TurnID != fixtureRun ||
		recs[0].Purpose != types.AuxCondense(string(compact.KindOutcome)) {
		t.Fatalf("the rewrite was recorded as %+v, want the turn's condense_outcome", recs)
	}
}

// A TURN THE BUDGET ENDED STILL GETS ITS CARD.
//
// The turn's meter holds every call of the turn once a window is full, its
// auxiliary calls included, because what they would buy feeds a round that is
// never sent. The card is not such a call: it is written after the segment's
// last round, as the task's record of the turn — and the turn a budget ended
// is the one a person most needs the card of. So the card's rewrite is not
// asked of the meter, and a long account is condensed for it as on any turn.
func TestATurnTheBudgetEndedStillGetsItsCard(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Dev", TokenBudget: org.TokenCeilings{period.Day: 100}}
	c := meteredCompany(config.TokenBudget{}, seat)
	rewriter := &cardRewriter{answer: "Fixed the flaky test; ran out of budget.", in: 90, out: 10}
	registry, err := phase.NewRegistry([]phase.Entry{{Key: "cheap", Provider: rewriter}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	c.Models = registry
	e := &Engine{auxSpend: auxspend.NewLedger(&capturedEvents{})}
	e.epoch.current.Store(c)

	// The segment's meter, holding the seat's day its last call filled.
	fleet := coordmem.NewFleet()
	m := &meter{budgets: fleet, agentScope: scopeOf(t, c, seat), basis: basisOf(c, seat), now: time.Now}
	if err := m.Record(t.Context(), 150, time.Now()); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if m.Held(t.Context()) == nil {
		t.Fatal("the meter holds nothing; this case asserts nothing")
	}
	tel := dispatchTel(&workletItem)
	tel.handle, tel.budget = seat.Handle(), m
	charge := tel.chargeFor(segmentSpend(1), turn.Result{Decision: phase.Failed}, nil,
		time.Unix(1_700_000_060, 0).UTC())
	charge.record.Summary = strings.Repeat("The turn investigated the flaky test and ", 40) + "stopped."

	charge = e.withCards(t.Context(), charge)
	if charge.record.Summary != condensedCard+"Fixed the flaky test; ran out of budget." {
		t.Fatalf("the card of a turn the budget ended was not condensed: %q", charge.record.Summary)
	}
}

// cardRewriter answers a card rewrite at a known cost.
type cardRewriter struct {
	answer  string
	in, out int
}

func (cardRewriter) Model() string { return "aux" }
func (p *cardRewriter) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return &llm.Completion{Model: "aux", Content: p.answer, InputTokens: p.in, OutputTokens: p.out}, nil
}

// A RESUMED SEGMENT'S END COUNTS THE WORKERS ITS RUN DELEGATED TO. A run's
// workers ran over the tool bridge while no segment was running, so the
// segment that resumes from the run is the one that pays for them — and its
// completion is the one record of the turn that can say they ran, beside the
// workers it delegated to itself.
//
// Mutation: leave the job's workers out of the completion, and this goes red.
func TestAResumedSegmentsEndCountsItsRunsWorkers(t *testing.T) {
	t.Parallel()
	e, p, tel := failing(t)
	tel.resumed, tel.launchID = true, "launch-1"
	tel.jobEngine = jobEngineSpend
	spend := segmentSpend(1)

	e.publishTurnCompleted(t.Context(), tel, spend, turn.Result{Decision: phase.Done}, nil, time.Now())

	got := only[*types.AgentTurnCompleted](t, p, "agent_turn_completed")
	job := jobEngineSpend
	if got.SubagentCount != spend.Workers+job.Workers ||
		got.SubagentInputTokens != spend.WorkerInput+job.WorkerInput ||
		got.SubagentOutputTokens != spend.WorkerOutput+job.WorkerOutput ||
		got.SubagentTokens != spend.WorkerTokens()+job.WorkerInput+job.WorkerOutput {
		t.Fatalf("the completion counts %d workers, %d in / %d out (%d), want the "+
			"segment's own and the run's bridged ones together", got.SubagentCount,
			got.SubagentInputTokens, got.SubagentOutputTokens, got.SubagentTokens)
	}
}

// THE RECORD THAT ENDS A TURN STATES THE WHOLE TURN, its parked segments
// included.
//
// The usage domain reads a turn's statistics off that one record, because the
// turn's other records are not all on one node — a stateless node's are kept a
// batch at a time by whichever data node won each claim, and a turn resumed on
// another node leaves its first segment behind. So the segment that parks hands
// its span and its reviews on with the conversation, the segment that ends the
// turn adds its own and says whether the turn failed, and a segment that only
// parked states nothing — the turn has not ended.
//
// Mutation: leave the carried measurement out of the end, or state one on a
// parked segment, and this goes red.
func TestTheRecordThatEndsATurnStatesTheWholeTurn(t *testing.T) {
	t.Parallel()
	start := time.Unix(1_700_000_000, 0).UTC()
	first := runner.Spend{Reviews: 2, SentBack: 1, Unaccepted: 1}
	second := runner.Spend{Reviews: 1}

	dispatch := dispatchTel(nil)
	dispatch.startedAt = start
	parked := dispatch.chargeFor(first, turn.Result{Suspended: true}, nil, start.Add(time.Minute))
	if want := (execstate.TurnSoFar{DurationMS: 60_000, Reviews: 2, SentBack: 1, Unaccepted: 1}); parked.soFar == nil || *parked.soFar != want {
		t.Fatalf("the parked segment hands on %+v, want its own span and reviews %+v", parked.soFar, want)
	}
	e, p, tel := failing(t)
	tel.startedAt = start
	e.publishTurnCompleted(t.Context(), tel, first, turn.Result{Suspended: true}, nil, start.Add(time.Minute))
	if got := only[*types.TurnCompleted](t, p, "turn_completed"); got.Turn != nil {
		t.Errorf("a segment that parked stated the whole turn %+v — it has not ended", got.Turn)
	}

	for _, c := range []struct {
		name   string
		res    turn.Result
		failed bool
	}{
		{"accepted", turn.Result{Decision: phase.Done}, false},
		{"failed", turn.Result{Decision: phase.Failed}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			e, p, tel := failing(t)
			tel.resumed, tel.soFar = true, parked.soFar
			tel.startedAt = start.Add(time.Hour)
			e.publishTurnCompleted(t.Context(), tel, second, c.res, nil, tel.startedAt.Add(30*time.Second))
			got := only[*types.TurnCompleted](t, p, "turn_completed")
			want := types.TurnTally{DurationMS: 90_000, Failed: c.failed, Reviews: 3, SentBack: 1, Unaccepted: 1}
			if got.Turn == nil || *got.Turn != want {
				t.Errorf("the record ending the turn states %+v, want both segments' spans "+
					"and reviews and whether it failed: %+v — never the park in between, "+
					"which held no seat", got.Turn, want)
			}
			if got.DurationMS != 30_000 {
				t.Errorf("the ending segment's own duration_ms is %d, want its 30000 — the "+
					"whole turn is the tally's", got.DurationMS)
			}
		})
	}

	// A TURN WHOSE PAST NOBODY MEASURED — resumed from a park a build before
	// the tally made, so its conversation carried no measurement — states
	// none and hands none on: neither its end nor a park of its own can say
	// what its first segment ran, and a tally of the rest would be read as
	// the whole turn. Its end is then summed from its records.
	//
	// Mutation: drop the guard from either the end or the park, and this
	// names which.
	t.Run("an unmeasured past", func(t *testing.T) {
		e, p, tel := failing(t)
		tel.resumed, tel.soFar = true, nil
		tel.startedAt = start.Add(time.Hour)
		if again := tel.chargeFor(second, turn.Result{Suspended: true}, nil,
			tel.startedAt.Add(time.Minute)); again.soFar != nil {
			t.Errorf("a segment of a turn whose past nobody measured parks again handing on "+
				"%+v — its own share, which the end would read as the whole turn", again.soFar)
		}
		e.publishTurnCompleted(t.Context(), tel, second, turn.Result{Decision: phase.Done}, nil,
			tel.startedAt.Add(30*time.Second))
		if got := only[*types.TurnCompleted](t, p, "turn_completed"); got.Turn != nil {
			t.Errorf("the end of a turn whose past nobody measured states %+v — a tally of "+
				"its last segment alone", got.Turn)
		}
	})
}

// A STOP THAT CLOSES A TURN AT ITS PARK CHARGES ONLY WHAT THE PARK LEFT UNPAID.
//
// A person's stop can land between a launch and its park: the segment that
// would have parked has ended and charged, and the frame then closes the turn
// as stopped with a segment of its own. Built from the parking segment's
// telemetry, that close charged the item the segment's job, its bridged spend,
// its auxiliary calls and its wall time a second time, under the stop's own
// operation id, which the tracker's ledger does not collapse; and a turn the
// park charged to nothing lost the segment's whole spend, since only the park
// the stop refused would have carried it on. Across the park and the close, the
// item is charged everything the turn spent, once, and its turn counted once.
//
// Mutation: close with the parking segment's telemetry, and the two charges add
// up to more than the turn spent on an item, and to none of the park's spend on
// a turn that charged nothing at its park.
func TestAStopAtTheParkChargesOnlyWhatTheParkLeftUnpaid(t *testing.T) {
	t.Parallel()
	ended := time.Unix(1_700_000_060, 0).UTC()
	closedAt := ended.Add(time.Minute)
	stop := &stopError{handle: "dev"}
	sole := turnctx.WrittenFrom([]types.WorkItem{workletItem}, false)
	carried := &execstate.Uncharged{Turns: 1, Rounds: 2, Input: 900, Output: 90, WallMs: 4000}

	for _, tc := range []struct {
		name string
		// parking is the segment that would have parked; rollup is what it
		// and the segments before it spent, which the item is owed once, and
		// turns how many turns it counts across the park and the close.
		parking func() turnTelemetry
		rollup  int
		turns   int
	}{
		{"a dispatch on an item", func() turnTelemetry { return dispatchTel(&workletItem) },
			rollupOf(segmentSpend(1), segmentAux(1), 0, 0), 1},
		{"a resume on an item", func() turnTelemetry {
			tel := resumeTel(&workletItem, nil, 4000, 700)
			tel.jobEngine = jobEngineSpend
			return tel
		}, rollupOf(segmentSpend(1), segmentAux(1), 4000, 700) + engineTokens(jobEngineSpend), 0},
		{"a dispatch on nothing that wrote one item", func() turnTelemetry {
			tel := dispatchTel(nil)
			tel.written = sole
			return tel
		}, rollupOf(segmentSpend(1), segmentAux(1), 0, 0), 1},
		{"a resume on nothing that wrote one item", func() turnTelemetry {
			tel := resumeTel(nil, carried, 4000, 700)
			tel.jobEngine = jobEngineSpend
			tel.written = sole
			return tel
		}, rollupOf(segmentSpend(1), segmentAux(1), 4000, 700) + engineTokens(jobEngineSpend) +
			carried.Input + carried.Output, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tel := tc.parking()
			tel.auxSpent.Add(segmentAux(1))
			parked := tel.chargeFor(segmentSpend(1), turn.Result{Suspended: true}, nil, ended)
			closing := tel.closingAfter(parked, closedAt).chargeFor(runner.Spend{}, turn.Result{}, stop,
				closedAt)

			if !closing.onWorklet() {
				t.Fatal("the close charged nothing, so the item never hears the turn ended")
			}
			if closing.opID == parked.opID {
				t.Fatal("the close shares the park's operation id, and the ledger collapses it into the park")
			}
			if closing.record.Outcome != segmentStopped {
				t.Fatalf("the close's outcome is %q, want %q", closing.record.Outcome, segmentStopped)
			}
			charged, turns := 0, 0
			for _, c := range []segmentCharge{parked, closing} {
				if c.onWorklet() {
					charged += c.record.Spend.Tokens()
					turns += c.record.Spend.Turns
				}
			}
			if charged != tc.rollup || turns != tc.turns {
				t.Fatalf("the item was charged %d tokens and %d turns across the park and the close, "+
					"for a turn that spent %d and counts %d", charged, turns, tc.rollup, tc.turns)
			}
			// AND NOTHING OF ITS OWN: the close ran no phase, called no
			// model and took no time, so it pays what the park handed on.
			if want := addUncharged(worklet.TurnSpend{}, parked.carry); closing.record.Spend != want {
				t.Fatalf("the close charged %+v, want only what the park handed on, %+v",
					closing.record.Spend, want)
			}
		})
	}
}
