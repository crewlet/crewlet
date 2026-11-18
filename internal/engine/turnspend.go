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
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// Spend is on the task: each completed turn SEGMENT charges what it spent to
// the Worklet item the turn is on (ADR-0022); a turn on another tracker's item
// is attributed on its events and charges nothing here. A detached turn
// completes once per segment — the dispatch, then a resume per collected run,
// often on another node — each under [segmentOpID], so a re-run or redelivered
// segment counts once, and only the dispatch counts a turn.
//
// REFLECTION IS NOT CHARGED: it runs after the turn, on the seat's behalf, and
// charging it would make a task's cost depend on how much its seat had to
// remember (ADR-0022's amendment).

// segmentCharge is what one completed segment charges, decided once.
type segmentCharge struct {
	// item is the work item the segment is charged to, nil for none.
	item *types.WorkItem
	// opID is the segment's operation id, and the turn row's.
	opID string
	// record is the turn record to publish when item is native.
	record worklet.TurnRecord
	// carry is what a parked segment charged to nothing hands on
	// (execstate.State.Uncharged): a sole write may yet name the turn's
	// item, which a parked segment cannot conclude, so the segment that
	// finishes the turn pays it with its own, turn count included, or
	// nobody does. nil otherwise.
	carry *execstate.Uncharged
	// soFar is what a parked segment hands on of the turn's own
	// measurement, charged or not ([turnTelemetry.measured]); nil on a
	// segment that did not park, or whose turn's earlier segments went
	// unmeasured ([turnTelemetry.measuredWhole]).
	soFar *execstate.TurnSoFar
	// aux is the segment's attribution for the card's rewrite, with no
	// tally: [Engine.withCards] gives the card its own, since the
	// segment's was summed already.
	aux auxspend.Use
	// ended is the instant the segment ended, taken ONCE: the task's turn
	// row's wall time and the completion's end and duration are all
	// measured to it ([Engine.endSegment]).
	ended time.Time
}

// onWorklet reports a charge Worklet takes: one on a Worklet work item. Any
// other tracker's item has no row here to add to.
func (c segmentCharge) onWorklet() bool {
	return c.item != nil && c.item.Backend == types.WorkWorklet
}

// segmentOpID is the operation id of one segment of a run, in the
// operation-id grammar ([statelog.DeriveOpID]): the tracker's ledger collapses
// a segment charged twice by the instant its id was minted, and an id outside
// the grammar is answered `unknown` where the ledger lost a row, its spend lost.
//
// Minted at the RUN's start ([statelog.OpMintedAt]), never the segment's (a
// re-run moves it) or the call's, so a run's segments differ only in which
// segment and, for a resume, the launch it resumed from. A REBASED attempt
// (ADR-0029) mints at its rebasedTo ([rebaseFor]), or one older than the
// ledger remembers is answered `unknown`; the rebase is recorded before the
// attempt writes, so a retry derives the same id.
func segmentOpID(runID, launch string, segment segmentKind, rebasedTo time.Time) string {
	at := rebasedTo
	if at.IsZero() {
		at, _ = statelog.OpMintedAt(runID)
	}
	return statelog.DeriveOpID(at, "turn-spend", runID, string(segment), launch)
}

// segmentKind is which segment of a run a charge is for — part of its
// operation id ([segmentOpID]).
type segmentKind string

const (
	// segmentDispatch is the segment a trigger started.
	segmentDispatch segmentKind = "dispatch"
	// segmentResume is a segment re-entered from a collected run or a
	// person's answer, told apart by the launch it resumed from.
	segmentResume segmentKind = "resume"
	// segmentStop is the segment that only CLOSES a turn a person stopped
	// while parked on its run, or before its park landed
	// ([Engine.closeStopped]). Its own kind: under the id of the segment
	// that already charged, the ledger would collapse the close into the
	// park and the task's card would go on saying the turn is parked.
	segmentStop segmentKind = "stop"
)

// segment is which segment this telemetry describes.
func (t turnTelemetry) segment() segmentKind {
	switch {
	case t.closesStop:
		return segmentStop
	case t.resumed:
		return segmentResume
	}
	return segmentDispatch
}

// chargeFor decides what this segment charges: its item from the turn's rules,
// and its spend from the runner's tally, the collected run it resumed from and
// whatever an earlier segment handed on.
func (t turnTelemetry) chargeFor(spend runner.Spend, res turn.Result, err error,
	ended time.Time,
) segmentCharge {
	item, _ := completedWorkItem(t.workItem, t.workItemBasis, t.written, res.Suspended)
	job := t.jobEngine
	tokens := t.segmentTokens(spend)
	own := withAux(withAux(worklet.TurnSpend{
		Rounds:     spend.Rounds,
		Input:      tokens.Input,
		Output:     tokens.Output,
		CacheRead:  tokens.CacheRead,
		CacheWrite: tokens.CacheWrite,
		WallMs:     int(max(ended.Sub(t.startedAt), 0) / time.Millisecond),
		Workers:    spend.Workers + job.Workers,
		SentBack:   spend.SentBack,
	}, t.auxSpent.Total()), auxSpentOf(job.Aux))
	// ONE TURN, COUNTED BY ITS DISPATCH: a resumed segment and a stop's
	// close are more of a turn already counted.
	if t.segment() == segmentDispatch {
		own.Turns = 1
	}
	total := addUncharged(own, t.uncharged)
	// THE CARD'S ATTRIBUTION, without the turn's tally (summed above;
	// [Engine.withCards] tallies its own) or its METER: the card is
	// rewritten after the last round, so the meter has no round to hold and
	// would only refuse the card of a turn the budget ended, the one a
	// person most needs. Charged to the seat's counters like any call the
	// meter does not carry.
	cardUse := t.aux()
	cardUse.Tally, cardUse.Budget = nil, nil
	charge := segmentCharge{item: item, opID: segmentOpID(t.runID, t.launchID, t.segment(), t.rebasedTo),
		aux: cardUse, ended: ended}
	if res.Suspended && t.measuredWhole() {
		// WHAT THE TURN HAS MEASURED OF ITSELF, handed on only by a turn
		// that knows its whole past: otherwise the segment that ends it
		// would read a part as the whole ([turnTelemetry.measuredWhole]).
		soFar := t.measured(spend, ended)
		charge.soFar = &soFar
	}
	if item == nil {
		if res.Suspended {
			charge.carry = unchargedOf(total)
		}
		return charge
	}
	charge.record = worklet.TurnRecord{
		Task: item.ID, Seat: t.handle, TurnID: t.runID, Trigger: t.trigger.Type,
		Outcome: segmentOutcome(res, err), Phases: spend.Phases, Spend: total,
		// WHOLE until the write: the writer refuses a text past
		// [worklet.MaxTurnSummary] rather than cut it, and [Engine.withCards]
		// rewrites it to fit, never cuts. The completion event's summary,
		// so the task's card and the trace agree.
		Summary: planSummary(res),
		Review:  spend.Review,
		Tools:   worklet.CountTurnTools(workTools(spend.AllTools)),
	}
	// WHICH PHASE BROKE, only on a failed segment: a phase can fail and the
	// turn still end otherwise (a person's stop is not a failure).
	if charge.record.Outcome == string(phase.Failed) {
		charge.record.FailedIn = spend.FailedIn
	}
	return charge
}

// segmentTokens is the segment's model tokens, the one sum the turn's tally
// and its item's charge both read: its phases as their records state them (a
// phase resumed across a park is paid by the segment it finished in, never
// twice), its delegated workers and extension judge, metered beside the
// phases, and the run it collected — that run's usage and what the engine
// spent on it while no segment ran ([runner.BridgedSpend],
// [sandbox.ResumeRequest.Engine]). In-turn auxiliary calls come from
// turnTelemetry.auxSpent, never the records the ledger flushes later.
func (t turnTelemetry) segmentTokens(spend runner.Spend) types.TurnTokens {
	job, aux := t.jobEngine, t.auxSpent.Total()
	return types.TurnTokens{
		Input: spend.InputTokens + spend.WorkerInput + spend.JudgeInput + t.jobInput +
			job.WorkerInput,
		Output: spend.OutputTokens + spend.WorkerOutput + spend.JudgeOutput + t.jobOutput +
			job.WorkerOutput,
		CacheRead:  spend.CacheRead + t.jobCacheRead,
		CacheWrite: spend.CacheWrite + t.jobCacheWrite,
		AuxInput:   aux.Input + job.Aux.Input,
		AuxOutput:  aux.Output + job.Aux.Output,
	}
}

// closingAfter is the telemetry of the segment that CLOSES a turn whose park a
// person's stop refused ([Engine.closeStoppedAtPark]): the turn's identity and
// item, none of the parking segment's own spend (it already charged), a span
// starting at the close, and what the parked segment handed on.
func (t turnTelemetry) closingAfter(parked segmentCharge, at time.Time) turnTelemetry {
	t.jobInput, t.jobOutput, t.jobCacheRead, t.jobCacheWrite = 0, 0, 0, 0
	t.jobEngine = sandbox.EngineSpend{}
	t.auxSpent = auxspend.NewTally()
	t.startedAt = at
	t.uncharged = parked.carry
	t.soFar = parked.soFar
	t.closesStop = true
	return t
}

// segmentOutcome is how a segment ended, in the vocabulary a task's turn list
// reads: parked on its executor, stopped by a person, failed, or the turn's own
// decision.
//
// STOPPED IS NOT FAILED, as on the turn's completion
// ([Engine.publishTurnCompleted]): a stop arrives as an error, but a turn
// somebody deliberately ended did not break.
func segmentOutcome(res turn.Result, err error) string {
	switch {
	case res.Suspended:
		return "suspended"
	case turn.Stopped(err):
		return segmentStopped
	case err != nil || res.Decision == phase.Failed:
		return string(phase.Failed)
	}
	return string(res.Decision)
}

// segmentStopped is the outcome of a segment a person's stop ended.
const segmentStopped = "stopped"

// withAux adds a segment's in-turn auxiliary spend to its charge. Tokens only:
// an auxiliary call is not a round, and the cache counts are a breakdown of the
// input it adds, as a phase's are.
func withAux(s worklet.TurnSpend, aux auxspend.Spent) worklet.TurnSpend {
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
func addUncharged(s worklet.TurnSpend, u *execstate.Uncharged) worklet.TurnSpend {
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

// unchargedOf is a spend in the parked turn state's own shape.
func unchargedOf(s worklet.TurnSpend) *execstate.Uncharged {
	return &execstate.Uncharged{
		Turns: s.Turns, Rounds: s.Rounds, Input: s.Input, Output: s.Output,
		CacheRead: s.CacheRead, CacheWrite: s.CacheWrite, WallMs: s.WallMs,
		Workers: s.Workers, SentBack: s.SentBack,
	}
}

// turnRecorder is the one write a segment's charge needs, declared by its one
// caller.
type turnRecorder interface {
	RecordTurn(ctx context.Context, opID string, turn worklet.TurnRecord) (worklet.WriteResult, error)
}

// endSegment closes one segment of a turn — however it ended, so no path can
// drift into another order — in the order its readers rely on: card fitted,
// in-turn auxiliary records flushed, end published, charge written.
//
// THE END FOLLOWS ITS COST: the Turn screen refetches once when the turn ends,
// so every in-turn record, the card's rewrite (the LAST auxiliary call,
// [Engine.cardsFor]) included, must be on the stream before the completion
// ([auxspend.Ledger.FlushTurn]). THE CHARGE FOLLOWS THE END, so the task's
// turn row never links a turn still running. The order moves publishing, never
// measuring: duration and wall time both end at [segmentCharge.ended]. The
// rewrite only delays the completion, and the turn's frame holds the seat
// until this returns either way.
func (e *Engine) endSegment(ctx context.Context, tel turnTelemetry, spend runner.Spend,
	res turn.Result, err error, charge segmentCharge,
) {
	ready := e.cardsFor(ctx, charge)
	e.auxSpend.FlushTurn(ctx, tel.runID)
	e.publishTurnCompleted(ctx, tel, spend, res, err, charge.ended)
	e.recordTurnSpend(ctx, ready)
	if !res.Suspended {
		// A TURN TOLD AN ACCESS CHANGE HAS REVIEWED IT once it ends (ADR-0111).
		e.endReviewOnTurnEnd(ctx, tel.handle, tel.runID)
	}
}

// cardedCharge is a segment's charge readied for its write: its card fitted
// and the writer it goes to, both decided once by [Engine.cardsFor].
type cardedCharge struct {
	segmentCharge
	// write is the tracker write the charge goes through; nil where there
	// is nothing to charge: no Worklet item, or no writer on this node.
	write turnRecorder
}

// cardsFor readies a segment's charge: on a Worklet item this node can write
// to, its card fitted ([Engine.withCards]) and the writer it goes through;
// otherwise nothing to write, and no rewrite paid for a card nothing shows.
//
// The writer is [Engine.trackerHalves]' router, as every tool's write is,
// never a local writer: a node without `data` has none and asks a data node
// holding the task, or a stateless seat's every charge would be dropped.
func (e *Engine) cardsFor(ctx context.Context, charge segmentCharge) cardedCharge {
	if !charge.onWorklet() {
		return cardedCharge{segmentCharge: charge}
	}
	halves, ok := e.trackerHalves()
	if !ok {
		// NO WORKLET WRITER: the company moved its tracker off the engine
		// while the turn ran, or the node is in a maintenance mode, which
		// publishes nothing ([Engine.writers]). Nowhere to charge.
		return cardedCharge{segmentCharge: charge}
	}
	charge = e.withCards(ctx, charge)
	return cardedCharge{segmentCharge: charge, write: halves.as(builtin.Actor{
		Handle: charge.record.Seat, Kind: worklet.AuthorAgent,
		// THE ENGINE'S OWN BOOKKEEPING, made as the seat for nobody
		// ([types.ForNobody]): no call of the turn makes it, and worklet's
		// RecordTurn writes it quiet.
		WrittenFor: types.ForNobody(),
		TurnID:     charge.record.TurnID,
	})}
}

// recordTurnSpend writes a segment's readied charge to its Worklet work item.
//
// TELEMETRY NEVER FAILS THE WORK ([Engine.publishTurnCompleted]), so this
// returns nothing. A charge that could not be written is logged naming the
// item and the segment: the spend stays on the seat's counters and in the
// usage domain, only the task's share is lost.
func (e *Engine) recordTurnSpend(ctx context.Context, charge cardedCharge) {
	if charge.write == nil {
		return
	}
	e.chargeSegment(ctx, charge.write, charge.segmentCharge)
}

// withCards fits the segment's summary and review to its task's card and adds
// what rewriting them cost to the charge.
//
// A TALLY OF ITS OWN: the card is rewritten after [turnTelemetry.chargeFor]
// summed the segment's tally, so a rewrite tallied there would reach no charge.
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
// whole within [worklet.MaxTurnSummary], past it rewritten to fit by the
// seat's auxiliary model, or else a line saying where to read it whole. Never
// a cut: a cut card reads as the turn's whole account.
func turnCard(ctx context.Context, fit compact.Bound, text string) string {
	if len(text) <= worklet.MaxTurnSummary {
		return text
	}
	res, err := fit.Fit(ctx, compact.KindOutcome, text, worklet.MaxTurnSummary-len(condensedCard))
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
// that only carried the phase's answer out (`submit_work`, see
// `agent/structured`), which a turn card would list as work done.
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
