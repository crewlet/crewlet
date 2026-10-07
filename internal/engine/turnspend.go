package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tracker"
)

// Spend is on the task: what one completed turn SEGMENT charges to the work
// item the turn is on (ADR-0022).
//
// # A segment, not a turn
//
// A turn that detaches a coding run completes more than once under one run id:
// the dispatch segment parks, and each collected run resumes a segment of its
// own — minutes later, often on another node. Each segment charges what IT
// spent, under an operation id naming the segment:
//
//	turn/<run id>/dispatch
//	turn/<run id>/resume/<launch id>
//
// so a segment re-run after a failed resume, or a record delivered twice,
// lands on one turn row and is counted once. ONLY THE DISPATCH SEGMENT COUNTS
// A TURN (`turns=1`): a resumed segment is more of the same turn, and counting
// it again would put three turns on a task one turn worked on.
//
// # What a segment's tokens are
//
// Its own phases, as their records state them — a phase that resumed across a
// park states both halves, which is why a suspended phase is paid by the
// segment it finished in and never twice — plus the workers it delegated to
// and the extension judge, whose calls are metered beside the phases rather
// than inside them, and the AUXILIARY calls made inside it: its turn-start
// context, the rewrites its ledgers, its judge's evidence and its tools asked
// for, and its card's own rewrite. Those come from the segment's own tally
// (turnTelemetry.auxSpent, which every in-turn call adds to through the turn's
// attribution), never from the records, which the ledger flushes later and
// the fleet reads at query time; the card is rewritten after the charge is
// decided, so it tallies on its own and is added as the charge is written
// ([Engine.withCards]). A resumed segment adds the collected coding run it
// resumed from, which no phase of the engine's own ran — and what the ENGINE
// spent on that run while no segment was running (turnTelemetry.jobEngine):
// an agent-mode run's bridged calls, whose auxiliary rewrites and delegated
// workers are counted on the run's own meter and written to its row with
// every call ([runner.BridgedSpend]), and the condensation of the run's
// report, failure or question at collection, which the coordinator carries to
// this resume ([sandbox.ResumeRequest.Engine]). Each figure lands beside the
// segment's own of the same kind. REFLECTION IS NOT IN IT: the learning pass
// runs after the turn is over, on the seat's behalf rather than the item's,
// and charging it to whatever the turn was on would make a task's cost depend
// on how much its seat had to remember (ADR-0022's amendment).
//
// # A segment charged to nothing hands its spend on
//
// A turn nothing at dispatch named an item for may still be charged at its end
// by a sole write, and a segment that parks cannot conclude that. So such a
// segment charges nothing and CARRIES what it spent on the suspended
// conversation (execstate.State.Uncharged), and the segment that finishes the
// turn pays it with its own if it is charged, turn count included. If it is
// not, nothing ever is — which is what an unattributed turn is.
//
// ONLY A NATIVE ITEM IS CHARGED: the counters are the engine's own tracker's
// rows. A turn on a Jira issue or a pull request is attributed on its events
// and charges nothing here.

// segmentCharge is what one completed segment charges, decided once.
type segmentCharge struct {
	// item is the work item the segment is charged to, nil for none.
	item *types.WorkItem
	// opID is the segment's operation id, and the turn row's.
	opID string
	// record is the turn record to publish when item is native.
	record tracker.TurnRecord
	// carry is what a parked segment charged to nothing hands on to the
	// segment that finishes the turn; nil otherwise.
	carry *execstate.Uncharged
	// aux is the segment's attribution for the auxiliary calls its charge
	// makes — the card's rewrite — with no tally: [Engine.withCards] gives
	// the card one of its own, since the segment's was summed already.
	aux auxspend.Use
	// ended is the instant the segment ended, taken ONCE: the task's turn
	// row's wall time and the completion's end and duration are all
	// measured to it — see [Engine.endSegment].
	ended time.Time
}

// native reports a charge this engine's own tracker takes: one on a native
// work item. A turn on nothing charges nothing, and a turn on another
// tracker's item is attributed on its events and has no row here to add to.
func (c segmentCharge) native() bool {
	return c.item != nil && c.item.Backend == types.WorkNative
}

// segmentOpID is the operation id of one segment of a run.
func segmentOpID(runID, launch string, resumed bool) string {
	if !resumed {
		return "turn/" + runID + "/dispatch"
	}
	return "turn/" + runID + "/resume/" + launch
}

// chargeFor decides what this segment charges: its item from the turn's rules,
// and its spend from the runner's tally, the collected run it resumed from and
// whatever an earlier segment handed on.
func (t turnTelemetry) chargeFor(spend runner.Spend, res turn.Result, err error,
	ended time.Time,
) segmentCharge {
	item, _ := completedWorkItem(t.workItem, t.workItemBasis, t.written, res.Suspended)
	// THE JOB'S ENGINE SPEND: what its bridged calls and the condensation
	// of its collection cost, which no segment was running to tally.
	job := t.jobEngine
	own := withAux(withAux(tracker.TurnSpend{
		Rounds: spend.Rounds,
		Input: spend.InputTokens + spend.WorkerInput + spend.JudgeInput + t.jobInput +
			job.WorkerInput,
		Output: spend.OutputTokens + spend.WorkerOutput + spend.JudgeOutput + t.jobOutput +
			job.WorkerOutput,
		CacheRead:  spend.CacheRead,
		CacheWrite: spend.CacheWrite,
		WallMs:     int(max(ended.Sub(t.startedAt), 0) / time.Millisecond),
		Workers:    spend.Workers + job.Workers,
		SentBack:   spend.SentBack,
	}, t.auxSpent.Total()), auxSpentOf(job.Aux))
	if !t.resumed {
		own.Turns = 1
	}
	total := addUncharged(own, t.uncharged)
	// THE CARD'S ATTRIBUTION: the turn's, with neither its tally — summed
	// above, so a rewrite added there would reach no charge
	// ([Engine.withCards] tallies its own) — nor its METER. The card is
	// rewritten after the segment's last call, as a record of the turn
	// rather than an input to a round, so the meter has no call left to
	// hold for it; asked, it would refuse the card of every turn the budget
	// ended, which is the turn a person most needs the card of. Charged to
	// the seat's counters like any call the meter does not carry.
	cardUse := t.aux()
	cardUse.Tally, cardUse.Budget = nil, nil
	charge := segmentCharge{item: item, opID: segmentOpID(t.runID, t.launchID, t.resumed),
		aux: cardUse, ended: ended}
	if item == nil {
		if res.Suspended {
			charge.carry = unchargedOf(total)
		}
		return charge
	}
	charge.record = tracker.TurnRecord{
		Task: item.ID, Seat: t.handle, TurnID: t.runID, Trigger: t.trigger.Type,
		Outcome: segmentOutcome(res, err), Phases: spend.Phases, Spend: total,
		// WHAT THE SEGMENT DID, WHOLE until the write: the writer refuses
		// a text past [tracker.MaxTurnSummary] rather than cut it, since
		// every node stores it, and [Engine.recordTurnSpend] fits it to
		// the card there — rewritten, never cut. The same summary the
		// turn's completion event carries, so the task's card and the
		// trace agree.
		Summary: planSummary(res),
		Review:  spend.Review,
		Tools:   tracker.CountTurnTools(workTools(spend.AllTools)),
	}
	// WHICH PHASE BROKE, only where the segment is recorded as failed: a
	// phase can fail and the turn still end otherwise (a person's stop is
	// not a failure), and a name beside a success would be a claim about a
	// turn that did not fail.
	if charge.record.Outcome == string(phase.Failed) {
		charge.record.FailedIn = spend.FailedIn
	}
	return charge
}

// segmentOutcome is how a segment ended, in the vocabulary a task's turn list
// reads: parked on a coding run, failed, or the turn's own decision.
func segmentOutcome(res turn.Result, err error) string {
	switch {
	case res.Suspended:
		return "suspended"
	case err != nil || res.Decision == phase.Failed:
		return string(phase.Failed)
	}
	return string(res.Decision)
}

// withAux adds a segment's in-turn auxiliary spend to its charge. Tokens only:
// an auxiliary call is not a round, and the cache counts are a breakdown of the
// input it adds, as a phase's are.
func withAux(s tracker.TurnSpend, aux auxspend.Spent) tracker.TurnSpend {
	s.Input += aux.Input
	s.Output += aux.Output
	s.CacheRead += aux.CacheRead
	s.CacheWrite += aux.CacheWrite
	return s
}

// auxSpentOf is a run row's auxiliary figures as the tally's own shape.
func auxSpentOf(a sandbox.AuxTokens) auxspend.Spent {
	return auxspend.Spent{Input: a.Input, Output: a.Output,
		CacheRead: a.CacheRead, CacheWrite: a.CacheWrite}
}

// addUncharged folds what an earlier segment handed on into this one's spend.
func addUncharged(s tracker.TurnSpend, u *execstate.Uncharged) tracker.TurnSpend {
	if u == nil {
		return s
	}
	s.Turns += u.Turns
	s.Rounds += u.Rounds
	s.Input += u.Input
	s.Output += u.Output
	s.CacheRead += u.CacheRead
	s.CacheWrite += u.CacheWrite
	s.WallMs += u.WallMs
	s.Workers += u.Workers
	s.SentBack += u.SentBack
	return s
}

// unchargedOf is a spend in the suspended conversation's own shape.
func unchargedOf(s tracker.TurnSpend) *execstate.Uncharged {
	return &execstate.Uncharged{
		Turns: s.Turns, Rounds: s.Rounds, Input: s.Input, Output: s.Output,
		CacheRead: s.CacheRead, CacheWrite: s.CacheWrite, WallMs: s.WallMs,
		Workers: s.Workers, SentBack: s.SentBack,
	}
}

// turnRecorder is the one write a segment's charge needs, declared by its one
// caller.
type turnRecorder interface {
	RecordTurn(ctx context.Context, opID string, turn tracker.TurnRecord) (tracker.WriteResult, error)
}

// endSegment closes one segment of a turn, in the order its readers rely on:
// its card fitted, its in-turn auxiliary records published, its end published,
// and its charge written to its task.
//
// THE END FOLLOWS ITS COST. A reader holding the turn open asks for it again
// when the turn ends — the Turn screen refetches on the seat leaving the turn —
// so every in-turn record has to be on the stream before the completion, or
// that read misses what the turn's context and its rewrites cost and the
// page's tokens disagree with the turn list's for the same turn, with nothing
// asking again. The card's rewrite is the segment's LAST auxiliary call, so it
// is made first ([Engine.cardsFor]), and the ledger's records of the segment
// are flushed after it ([auxspend.Ledger.FlushTurn]).
//
// THE CHARGE IS WRITTEN AFTER THE END, as it always was: the task's turn row
// links to the turn, and it should not name a turn whose record says it is
// still running.
//
// THE ORDER CHANGES WHEN THINGS ARE PUBLISHED, NEVER WHAT IS MEASURED. The
// segment ended where [turnTelemetry.chargeFor] took its instant, and the
// completion's end and duration are measured to that same instant
// ([segmentCharge.ended]) — so a card rewrite of up to two auxiliary calls
// before the publish is in neither the turn's duration nor the task's wall
// time, and the two agree for the same segment, as they did when the
// completion went out first. What the rewrite does delay is the completion
// itself, and with it the seat's leaving `working` and the reflection wake;
// that is the true state of the seat rather than a cost of the order, because
// the turn's frame holds the seat until this returns whatever this does first
// — the seat takes no other work until the card is written either way.
// Publishing the completion first instead would put the card's record on the
// stream after the read the turn's end prompts, and nothing asks again.
//
// ONE FUNCTION for every way a segment ends — a turn that broke before its
// first phase, a turn that ran, a resumed segment — so the three cannot drift
// into different orders.
func (e *Engine) endSegment(ctx context.Context, tel turnTelemetry, spend runner.Spend,
	res turn.Result, err error, charge segmentCharge,
) {
	ready := e.cardsFor(ctx, charge)
	e.auxSpend.FlushTurn(ctx, tel.runID)
	e.publishTurnCompleted(ctx, tel, spend, res, err, charge.ended)
	e.recordTurnSpend(ctx, ready)
}

// cardedCharge is a segment's charge readied for its write: its card fitted
// and the writer it goes to, both decided once by [Engine.cardsFor].
type cardedCharge struct {
	segmentCharge
	// write is the tracker write the charge goes through; nil where there
	// is nothing to charge — no native item, or no writer on this node.
	write turnRecorder
}

// cardsFor readies a segment's charge: on a native item this node can write
// to, its card fitted to the task's turn list ([Engine.withCards]) and the
// writer it goes through; otherwise a charge with nothing to write, and no
// rewrite paid for a card nothing will show.
//
// # Through the router, as every tool's write is
//
// The writer is [Engine.trackerHalves]' own: a data node's router answers from
// its own copy and a node without `data` asks one that holds the task. It was
// this node's LOCAL writer once, which a stateless node does not have — so on
// exactly the topology where seats run apart from the estate, every turn's
// charge to the task that woke it was dropped without a word, and a task's
// spend read zero however much was spent on it.
func (e *Engine) cardsFor(ctx context.Context, charge segmentCharge) cardedCharge {
	if !charge.native() {
		return cardedCharge{segmentCharge: charge}
	}
	halves, ok := e.trackerHalves()
	if !ok {
		// A NATIVE ITEM ON A NODE THAT HANDS OUT NO TRACKER WRITER is a
		// company that moved its tracker off the engine while the turn ran,
		// or a data node in a maintenance mode, which publishes nothing:
		// there is nowhere to charge.
		return cardedCharge{segmentCharge: charge}
	}
	charge = e.withCards(ctx, charge)
	return cardedCharge{segmentCharge: charge, write: halves.as(builtin.Actor{
		Handle: charge.record.Seat, Kind: tracker.AuthorAgent,
		TurnID: charge.record.TurnID,
	})}
}

// recordTurnSpend writes a segment's readied charge to its native work item.
//
// TELEMETRY NEVER FAILS THE WORK, so this returns nothing, on the terms
// [Engine.publishTurnCompleted] states: the turn has finished and its result is
// already the caller's answer. A charge that could not be written is logged
// naming the item and the segment — the spend is still on the seat's counters
// and in the usage domain; what is lost is the task's share of it.
func (e *Engine) recordTurnSpend(ctx context.Context, charge cardedCharge) {
	if charge.write == nil {
		return
	}
	e.chargeSegment(ctx, charge.write, charge.segmentCharge)
}

// withCards fits the segment's summary and review to its task's card and adds
// what rewriting them cost to the charge.
//
// A TALLY OF ITS OWN: the card is the segment's last auxiliary call and the
// turn's work like the rest, but it is made after [turnTelemetry.chargeFor]
// summed the segment's tally, so a rewrite tallied there would reach no
// charge.
func (e *Engine) withCards(ctx context.Context, charge segmentCharge) segmentCharge {
	use := charge.aux
	use.Tally = auxspend.NewTally()
	fit := e.seatCompactor(e.Company(), charge.record.Seat, use)
	charge.record.Summary = turnCard(ctx, fit, charge.record.Summary)
	charge.record.Review = turnCard(ctx, fit, charge.record.Review)
	charge.record.Spend = withAux(charge.record.Spend, use.Tally.Total())
	return charge
}

// condensedCard is the marker a rewritten card line carries, so nobody reads
// a model's condensation as the turn's own words.
const condensedCard = "(condensed) "

// turnCard is a turn's summary or review as its task's card carries it:
// whole within [tracker.MaxTurnSummary], rewritten to fit by the seat's
// auxiliary model past it, and — where no rewrite can be had — a line saying
// how long the account is and where it is read whole. Never a cut: the card
// is the one line a person reads for a turn, and a cut one reads as the turn's
// whole account.
func turnCard(ctx context.Context, fit compact.Bound, text string) string {
	if len(text) <= tracker.MaxTurnSummary {
		return text
	}
	res, err := fit.Fit(ctx, compact.KindOutcome, text, tracker.MaxTurnSummary-len(condensedCard))
	if err == nil {
		return condensedCard + res.Text
	}
	return fmt.Sprintf("(this turn's account is %d bytes and could not be condensed "+
		"for its card — open the turn to read it whole)", len(text))
}

// chargeSegment is [Engine.recordTurnSpend] over the write it needs.
func (e *Engine) chargeSegment(ctx context.Context, w turnRecorder, charge segmentCharge) {
	if _, err := w.RecordTurn(ctx, charge.opID, charge.record); err != nil {
		level := log.WarnContext
		if errors.Is(err, context.Canceled) {
			level = log.InfoContext
		}
		level(ctx, "turn_spend_unrecorded", "turn_id", charge.record.TurnID,
			"op_id", charge.opID, "task", charge.item.ID, "key", charge.item.Key,
			"tokens", charge.record.Spend.Tokens(), "error", err.Error(),
			"detail", "the segment's spend is on the seat's counters and the "+
				"usage history, and not on the task it worked on")
	}
}

// workTools is the calls a segment made that did something, without the ones
// that only carried the phase's own answer out: `submit_work` is how an
// executor says what it concluded (`agent/structured`), and a turn card
// listing it beside the tools that touched the company reads as work done.
func workTools(calls []string) []string {
	out := make([]string, 0, len(calls))
	for _, name := range calls {
		if name == runner.SubmitWorkTool || name == runner.SubmitReviewTool {
			continue
		}
		out = append(out, name)
	}
	return out
}
