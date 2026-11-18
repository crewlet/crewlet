package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracing"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The turn-level events, published around turn.Run (the phases publish their
// own, internal/agent/runner/telemetry.go). agent_turn_started opens the turn
// before its context is gathered ([Engine.publishTurnStarted]); two close it,
// separate because their readers differ:
//
//   - agent_turn_completed is the DASHBOARD's summary and ends a seat's live
//     row: a turn that fails to publish it leaves a working indicator up.
//   - turn_completed is the LEARNING subsystem's record; one event serving
//     both would be the union of two schemas.
//
// Published from the engine, not the loop: only this frame knows what woke the
// turn, which conversation it served, how long it took, and that it ended.

// turnTelemetry is one turn's publishable identity, assembled once at the top
// of the turn and used at both ends.
type turnTelemetry struct {
	handle string
	role   string
	// runID is THIS EXECUTION's identity, minted once so a turn's opening
	// phase event and its completion name the same run (ADR-0017).
	runID string
	// workKey is the unit of work, stable across a re-run, and workSince when
	// it began: the instant every operation id derived from the key carries,
	// reproduced by a re-run as the key is.
	workKey   string
	workSince time.Time
	// rebasedTo is the instant this attempt's operation ids carry in place of
	// their identity's start, zero where they carry that start ([rebaseFor],
	// [turnctx.Turn.RebasedTo]).
	rebasedTo time.Time
	agentID   string
	trigger   types.Trigger

	// convKey is the CONVERSATION IDENTITY: what every event this turn
	// publishes is tagged with, the episode row is filed under, and a person's
	// answer is matched back to a detached run on. partKey is the inbox
	// PARTITION the trigger arrived in, carried so a detached executor's row
	// states its batch, which tells two runs parked on one direct message
	// apart ([sandbox.ConversationRef.Best]).
	convKey string
	partKey string
	// transport is the chat surface the trigger arrived on
	// ([turnctx.Turn.Transport]), empty for a turn no chat woke.
	transport string
	startedAt time.Time
	trace     events.TraceContext

	// deadline is when the turn's wall-clock cap ends ([turnctx.Turn.Deadline]),
	// fixed at dispatch and read off the park on a resume; zero for no cap.
	deadline time.Time

	// interactions is who spoke to this seat and what they said, one per
	// constituent of the partition, read off the trigger or the park. It rides
	// the completed-turn event because the reflect dispatcher is a QUEUE
	// CONSUMER that may run on a node that never saw the trigger.
	interactions []types.InboundInteraction

	// ask is what the turn was ASKED ([turnAsk]), read off the trigger or, on
	// a resume, the park: for the completed-turn event where the interactions
	// do not carry it ([types.TurnCompleted.Ask]) and for a suspension to park.
	ask string

	// askLabel is who wrote ask, as one label ([prefetch.Ask.Label]), nil for
	// the company's or the seat's own words; read and parked as ask is (ADR-0040).
	askLabel *provenance.Label

	// askParts is the ask part by part, each with who wrote it
	// ([turnAskFor]): what the turn-start judgements read. Empty on a
	// resume, which runs none of them.
	askParts prefetch.Ask

	// task is the trigger as the executor is handed it ([DescribeTrigger]),
	// with what a delivery fenced moved onto [turnTelemetry.boundary]: here
	// beside the token, so the ask and the segment's tools' fences cannot be
	// cut under two tokens. Empty on a resume, which carries the parked task.
	task string

	// title is what a person reads the turn's work by ([turnctx.Turn.Title]):
	// the ask, else the trigger's feed line; on a resume, the line its run is
	// already listed under.
	title string

	// senders is who spoke to this seat ([sendersSpoken] over the
	// interactions, on every segment), which a tool that re-runs the
	// turn-start memory filter is judged against ([turnctx.Turn.Senders]).
	senders []types.CanonicalIdentity

	// requester is the seat whose wake started this turn
	// ([turnctx.Turn.Requester]), off the trigger's first event or, on a
	// resume, the parked row, by handle (ADR-0013).
	requester string
	// workingFor is who a colleague's ask, or the answer to this seat's
	// own ask, handed the turn on to work for ([turnctx.Turn.WorkingFor]).
	workingFor *types.WorkingFor

	// requesterLogin and requesterApps are the credential acting as no seat
	// whose write started this turn and the apps its writes carried
	// ([turnctx.Turn.RequesterLogin]): off the trigger
	// ([credentialRequesterOf]) or the parked row.
	requesterLogin string
	requesterApps  []iam.Grant

	// skills is the synthesized-skill ids offered to this turn's prompt.
	// Set after the prefetch, which is the only thing that knows them.
	skills []string

	// workItem is the item this turn is on as dispatch resolved it — the
	// trigger's, the asker's or the parked row's — and workItemBasis the
	// rule that named it; both empty for a turn nothing named an item for.
	// See worksubject.go for the rules and their order.
	workItem *types.WorkItem
	// answered are the answers whose deliveries woke the turn ([turnctx.Turn.Answered]).
	answered      []string
	workItemBasis types.WorkItemBasis

	// written is the set of items this turn's writes commit to, shared with
	// every tool call and delegate worker through the turn context and read
	// back at completion for the one rule dispatch cannot decide.
	written *turnctx.Written

	// holds is what this turn has named that Gitlet's expiry could take from
	// under it, shared through the turn context ([turnctx.Turn.Holds]): fresh
	// for a dispatched turn, the row's for a resumed one with its writer
	// attached ([Engine.rowHolds]), so one set across every segment.
	holds *turnctx.Holds

	// resumed marks a segment that re-entered a parked run and launchID the
	// launch it collected: together they name the segment its charge is
	// recorded under (turnspend.go). jobInput/jobOutput are that run's tokens,
	// which the segment pays, jobCacheRead/jobCacheWrite their cached share,
	// jobEngine what the engine spent on the run between segments (paid here
	// too), and uncharged what earlier segments spent and charged to nothing.
	resumed                     bool
	launchID                    string
	jobInput, jobOutput         int
	jobCacheRead, jobCacheWrite int
	jobEngine                   sandbox.EngineSpend
	uncharged                   *execstate.Uncharged

	// soFar is what earlier segments measured (spans, reviews), carried on the
	// parked conversation; nil before any park and on a resume whose park
	// nobody measured ([turnTelemetry.measuredWhole]). The turn's last record
	// states it whole ([turnTelemetry.measured]).
	soFar *execstate.TurnSoFar

	// auxSpent is what this segment's in-turn auxiliary calls have cost,
	// shared through the turn context ([turnctx.Turn.AuxSpend]); fresh per
	// segment, since each is charged what IT spent (turnspend.go).
	auxSpent *auxspend.Tally

	// boundary is the token every fence this segment writes carries
	// ([turnctx.Turn.Boundary]): minted per segment, a resumed one included,
	// and kept here because the prompt and the tools must agree on it.
	boundary provenance.Boundary

	// budget is the meter this segment is charged through (rounds, judge,
	// workers and, via [turnTelemetry.aux], every in-turn auxiliary call), nil
	// without a counter. Set BEFORE the segment's first auxiliary call, the
	// context assembly: a meter built after it never hears of that window.
	budget *meter

	// closesStop marks the segment that only CLOSES a turn a person
	// stopped ([Engine.closeStopped]): charged as a segment of its own
	// ([segmentStop]) and counting no turn.
	closesStop bool

	// closing, where set, is the identity the records CLOSING a stopped parked
	// turn are published under ([Engine.closeStoppedTurn]): ids derived from
	// the run's ending and the record type, stamped at the ending's instant, so
	// a retry after a crash republishes the same records rather than a second
	// end. Nil for every segment that ran.
	closing *closingIdentity
}

// closingIdentity names the records that close one stopped parked turn: the
// run's id and its ending's ([sandbox.RecordedEnding]), and the instant the
// ending was decided.
type closingIdentity struct {
	turnID, ending string
	at             time.Time
}

// closingNamespace derives the ids a stopped turn's closing records are
// published under.
var closingNamespace = uuid.MustParse("6a0c2f6e-1b7d-5c3e-9f42-8d5e3a7b1c09")

// stamp gives a record this segment publishes the identity it is closed under,
// where it is a closing; it leaves every other segment's records as minted.
func (t turnTelemetry) stamp(ev *events.Event) *events.Event {
	if t.closing == nil || ev == nil {
		return ev
	}
	ev.ID = uuid.NewSHA1(closingNamespace,
		[]byte(t.closing.turnID+"/"+t.closing.ending+"/"+ev.Type))
	ev.Timestamp = t.closing.at.UTC()
	return ev
}

// aux is the attribution this segment's in-turn auxiliary calls state: the one
// its tools read off the turn context, derived by the turn context's own rule
// so the engine's calls and the tools' cannot be filed differently.
func (t turnTelemetry) aux() auxspend.Use {
	return (&turnctx.Turn{RunID: t.runID, WorkKey: t.workKey, AuxSpend: t.auxSpent,
		Budget: t.budget.auxiliary()}).Aux()
}

// newRunID mints the identity of ONE EXECUTION of a turn.
//
// FRESH, never derived: every derivable identity (the trigger's ids, the
// conversation, the seat, the rounded clock) is one a redelivery reproduces,
// the bug ADR-0017 records. Not a process counter: other processes read it,
// so it is unique across the fleet.
//
// TIME-ORDERED, and the instant is read: a write by a run with no unit of
// work is seeded from the run, so its operation id carries the run's start,
// which the state log checks against its ledger's watermark
// (builtin.Actor.OperationSince); a resumed run keeps it. Minted by
// [statelog.NewOpID] so that reader needs no second parser.
func newRunID() string { return statelog.NewOpID(time.Now(), "") }

// describeTurn assembles the identity for one dispatch.
//
// The trigger is the partition's FIRST event, whose thread the turn answers;
// the last would credit whichever message arrived while the seat was busy.
//
// It also decides where the turn's writes are minted, as the one frame that
// builds the turn its tools read: a trigger delivered later than the
// operation ledger remembers has its writes rebased ([rebaseFor]). An error
// is a turn that does not run, and the dispatcher NAKs it.
func (e *Engine) describeTurn(ctx context.Context, company *Company, req Request) (_ turnTelemetry, err error) {
	defer origin.Stamp(&err)
	t := turnTelemetry{
		handle:    req.Handle,
		runID:     req.RunID,
		workKey:   req.WorkKey,
		workSince: req.WorkSince,
		convKey:   req.ConversationKey,
		// Off the partition's own events, not a Request field a second
		// construction site could leave empty; the identity travels on the
		// Request because a resumed turn has no events.
		partKey: partitionKeyOf(req.Events),
		// Off the metadata the working indicator reads ([chatMetadataOf]),
		// so "woken by chat" has one answer.
		transport: chatMetadataOf(req.Events)[notify.TransportField],
		startedAt: time.Now().UTC(),
	}
	// THE TURN'S DEADLINE, fixed here, as the turn begins: every segment and
	// every run of the turn is held to this one instant.
	t.deadline = wallClockDeadline(t.startedAt, req.TimeoutSeconds)
	t.role, t.agentID = seatIdentity(company, req.Handle)
	// OFF THE ASK: a coalesced conversation's interactions come from the
	// merged digest's own constituent list, which is the same set the
	// partition held and the one place a merge combined them.
	t.interactions = e.interactionsOf(req.Ask(), req.Handle)
	// ONE READING OF WHO WROTE THE ASK, for the judgements that read it now
	// and for the record that keeps it.
	t.askParts = e.askOf(req)
	t.ask, t.askLabel = t.askParts.Text(), t.askParts.Label()
	t.senders = sendersSpoken(t.interactions)
	t.requester = requesterIdentity(company, requesterOf(req.Events, t.interactions, req.Handle))
	t.requesterLogin, t.requesterApps = credentialRequesterOf(req.Events, req.Ask(),
		t.interactions, req.Handle)
	t.workingFor = workingForIn(req.Events)
	// The turn's own span, not the trigger's: the dispatcher restored the
	// trigger's trace onto ctx before runTurn opened the turn span, so the trace
	// id is the trigger's and the span id this turn's, parented on the trigger.
	t.trace = tracing.TraceOf(ctx)
	for _, ev := range req.Events {
		if ev == nil {
			continue
		}
		t.trigger = types.DescribeTrigger(ev)
		break
	}
	// The ask, or where the trigger stated none (an attachment-only message)
	// its feed line, so a run the turn launches is never listed as nothing.
	t.title = t.ask
	if t.title == "" {
		t.title = t.trigger.Summary
	}
	// THE ITEM, from the same first event the trigger is described from,
	// and a fresh set for what the turn is about to write.
	t.workItem, t.workItemBasis = workItemOf(req)
	t.answered = worklet.AnswersIn(req.Events)
	t.written = &turnctx.Written{}
	// AND A FRESH HOLD SET, with no row behind it until a launch opens one:
	// held in this process's memory while the turn runs, which this node
	// answers Gitlet's expiry for ([Engine.codeHolds]).
	t.holds = turnctx.NewHolds(nil)
	t.auxSpent = auxspend.NewTally()
	t.boundary = provenance.NewBoundary()
	t.task = DescribeTrigger(req.Ask(), t.boundary)
	rebased, err := rebaseFor(ctx, e.rebases(), builtin.Actor{
		TurnID: t.runID, WorkKey: t.workKey, WorkSince: t.workSince,
	}, t.startedAt)
	if err != nil {
		return turnTelemetry{}, err
	}
	t.rebasedTo = rebased
	return t, nil
}

// runnerTurn is the identity handed to the phase runner.
//
// TWO IDENTITIES, NEVER ONE (ADR-0017): runID names this execution and is
// every event's turn_id; workKey names the unit of work a write is
// idempotent against. Both come off the telemetry, so the events at the
// turn's two ends and its tools' context cannot name different runs.
//
// withheld is the identity directory's reading pinned beside the org
// ([turnctx.Turn.Withheld]), taken from [Engine.WithheldContacts] at the
// turn's start.
func (t turnTelemetry) runnerTurn(company *Company,
	depth int, chain []string, task string, reply turn.Reply,
	withheld func(handle string) bool,
) runner.Turn {
	return runner.Turn{
		RunID: t.runID, WorkKey: t.workKey,
		AgentID:         t.agentID,
		Trigger:         t.trigger,
		ConversationKey: t.convKey, Trace: t.trace,
		Context: &turnctx.Turn{
			RunID:     t.runID,
			WorkKey:   t.workKey,
			WorkSince: t.workSince,
			RebasedTo: t.rebasedTo,
			// EVERY RUN STARTS ITS OWN CALL LOG ([turnctx.CallLog]), so a
			// re-run's repeat counts come out as before; the runner seeds a
			// RESUMED run's log with what it called before it parked.
			Calls: turnctx.NewCallLog(),
			// Fresh per segment, a resumed one included: a name looked up
			// before the park may name something else after it ([turnctx.Lookups]).
			Lookups: turnctx.NewLookups(),
			// Fresh per segment too: an atlas record cites a call this
			// segment made.
			Seen: turnctx.NewSeen(),
			// The turn's deadline, for the runs it launches to carry.
			Deadline: t.deadline,
			// Both off the pinned epoch, so a colleague lookup mid-turn
			// resolves against the roster the turn started under.
			Seat: company.Org.AgentSeatByHandle(t.handle),
			Org:  company.Org,
			// AND WHO MAY BE REACHED, pinned with the org: the roster
			// this turn's prompt renders and every colleague lookup its
			// tools make leave out the same people.
			Withheld: withheld,
			Depth:    depth,
			// The whole path that got here, so an ask this turn makes
			// carries its full provenance.
			Chain: chain,
			// The conversation the turn owes an answer to, for work it
			// detaches: the executor's row, the resumed turn's report, and the
			// matching of a person's answer, which lands on the conversation
			// rather than the batch. The partition lets the row tell two runs
			// parked on one direct message apart.
			ConversationKey: t.convKey,
			PartitionKey:    t.partKey,
			// AND THE SURFACE, so a task this turn files says where it
			// came from — see [turnctx.Turn.Transport].
			Transport: t.transport,
			// The brief and the delivery obligation, carried for the
			// same reason: a resumed turn sees neither its trigger nor
			// this frame, so both have to reach the row from here.
			Task:  task,
			Reply: reply.String(),
			// What a run it launches is listed under, where the brief above
			// would show a person its fence lines.
			Title: t.title,
			// And who woke it, for a question a run it detaches puts to
			// "the requester" long after this frame is gone.
			Requester: t.requester, WorkingFor: t.workingFor,
			// OR THE CREDENTIAL that did, which no question reaches: what
			// the turn may use for it is bounded by its apps (ADR-0109).
			RequesterLogin: t.requesterLogin,
			RequesterApps:  slices.Clone(t.requesterApps),
			// Off the telemetry, not the interactions a resumed segment
			// lacks ([turnctx.Turn.Senders]).
			Senders: t.senders,
			// THE ITEM THIS TURN IS ON, and the set its writes report
			// into. The item rides every phase event and the row of any
			// executor this turn detaches; the set is the one mutable
			// thing a turn points at, shared by every tool call.
			WorkItem:      t.workItem,
			WorkItemBasis: t.workItemBasis,
			Answered:      t.answered,
			Written:       t.written,
			// AND WHAT IT HOLDS FROM GITLET'S EXPIRY, which every
			// tool call takes its hold in before its request.
			Holds: t.holds,
			// AND THE SEGMENT'S AUXILIARY TALLY, which every in-turn
			// auxiliary call adds to through the turn's attribution, and
			// the meter every one of them is charged through and asks.
			AuxSpend: t.auxSpent,
			Budget:   t.budget.auxiliary(),
			// AND THE FENCE TOKEN the prompt's thread was written with,
			// so every tool answer of the segment is fenced alike.
			Boundary: t.boundary,
		},
	}
}

// publishTurnStarted puts a turn, or one resumed segment of it, on the record
// before it does anything slow (agent_turn_started), so a seat gathering its
// context is not read as idle and a turn that dies there leaves a row naming
// its run. Dispatch publishes it before the prefetch, a resume once the
// segment is certain to run ([Engine.resumeTurn]).
//
// EVERY START IS CLOSED by [Engine.publishTurnCompleted] on every path that
// returns; an unpaired start is a recovered panic ([panicBreach]) or a dead
// process. Depth and chain ride the ENVELOPE, whose keys they are (a payload
// field under one is dropped). Only dispatch's item is named (worksubject.go):
// a sole write has not happened yet, and a turn on nothing omits both keys.
func (e *Engine) publishTurnStarted(ctx context.Context, t turnTelemetry,
	depth int, chain []string, resumed bool,
) {
	ev := events.New(types.AgentTurnStarted{
		Agent:           t.agentID,
		AgentHandle:     t.handle,
		RoleName:        t.role,
		TurnID:          t.runID,
		WorkKey:         t.workKey,
		Trigger:         t.trigger,
		ConversationKey: t.convKey,
		WorkItem:        t.workItem,
		WorkItemBasis:   t.workItemBasis,
		// THE SAME INSTANT turn_completed reports, both read off the
		// telemetry, so the run's two records cannot disagree on its start.
		StartedAt: t.startedAt,
		Resumed:   resumed,
	}, t.trace)
	ev.DelegationDepth = depth
	// A copy, never the caller's backing array: the request that holds it
	// outlives this publish, and a listener handed the event keeps it.
	ev.DelegationChain = slices.Clone(chain)
	e.publishEvent(ctx, ev, t.role)
}

// publishTurnCompleted closes the turn for both readers.
//
// TELEMETRY NEVER FAILS THE WORK, so it returns nothing: a broker refusing
// these events must not turn finished work into a failed turn.
//
// ended is the caller's, not this function's clock: the segment's end, which
// the task's turn row is measured to too and the publish may trail by seconds
// ([Engine.endSegment]).
func (e *Engine) publishTurnCompleted(ctx context.Context, t turnTelemetry,
	spend runner.Spend, res turn.Result, err error, ended time.Time,
) {
	ended = ended.UTC()
	// A STOPPED TURN DID NOT FAIL, though it ended on an error (how the stop
	// reached here): read as failed, its seat would be drawn as in trouble.
	stopped := turn.Stopped(err)
	failed := !stopped && (err != nil || res.Decision == phase.Failed)
	decision := string(res.Decision)
	// THE ITEM CHARGED: dispatch's, else the one item its writes committed to,
	// which only a segment that FINISHED the turn may conclude; both records
	// carry it.
	item, basis := completedWorkItem(t.workItem, t.workItemBasis, t.written, res.Suspended)
	// THE TURN'S CALLS AS ITS RECORD LISTS THEM: every phase's, held to the
	// one share a record has of its message (internal/agent/runner's
	// recordbytes.go), with what neither list keeps said by tool.
	calls := spend.Calls()

	summary := types.AgentTurnCompleted{
		Agent:    t.agentID,
		RoleName: t.role,
		// The LAST phase's model, which a one-line row names; the per-phase
		// models ride beside it, since a fallback chain can run executor and
		// reviewer on two different models.
		Model:          lastModel(spend),
		Trigger:        t.trigger,
		Prompt:         t.trigger.Summary,
		Response:       res.Artifact,
		InputTokens:    spend.InputTokens,
		OutputTokens:   spend.OutputTokens,
		TotalTokens:    spend.Total(),
		ToolExecutions: calls.Rows,
		CallsElided:    calls.Elided,
		ElidedCalls:    calls.ByTool,
		TurnID:         t.runID,
		WorkKey:        t.workKey,
		ExecuteModel:   spend.ExecuteModel,
		ReviewModel:    spend.ReviewModel,
		// What this turn DELEGATED, kept apart from TotalTokens: a worker's
		// tokens are already charged through the shared meter, and the split
		// answers "how much was fan-out". A RESUMED SEGMENT adds the workers its
		// run delegated over the tool bridge, since it pays for them (turnspend.go).
		SubagentCount:        spend.Workers + t.jobEngine.Workers,
		SubagentTokens:       spend.WorkerTokens() + t.jobEngine.WorkerInput + t.jobEngine.WorkerOutput,
		SubagentInputTokens:  spend.WorkerInput + t.jobEngine.WorkerInput,
		SubagentOutputTokens: spend.WorkerOutput + t.jobEngine.WorkerOutput,
		// The cache's share of InputTokens over the turn's own phases,
		// as the phase records state it.
		CacheReadTokens:  spend.CacheRead,
		CacheWriteTokens: spend.CacheWrite,
		WorkItem:         item,
		WorkItemBasis:    basis,
		// A SEGMENT THAT PARKED says so: the same turn id completes again
		// when its launch is collected.
		Suspended:       res.Suspended,
		Iterations:      res.Rounds,
		Decision:        decision,
		Failed:          failed,
		Stopped:         stopped,
		ConversationKey: t.convKey,
	}
	if err != nil {
		// Bounded so the event is publishable (events.MaxDiagnosticBytes):
		// a refused event is dropped whole.
		summary.Error = events.ClipDiagnostic(err.Error())
		// The phase's own classifier, so a spent plan and a revoked login
		// are not the same word on the Turn screen.
		summary.ErrorKind = runner.ErrorKind(err)
		if place, ok := origin.Of(err); ok && !stopped {
			summary.ErrorFunction, summary.ErrorFile = place.Function, place.File
		}
		if stopped {
			// Whatever the error wraps, a turn a person ended is
			// `stopped` — the one kind that is not a failure.
			summary.ErrorKind = runner.StoppedKind
		}
	}
	if res.Breach != nil {
		// A guard breach is not an error: the turn ran and a rule stopped it,
		// and the RULE ("depth_cap", "stall") is what sends an operator somewhere.
		// THE RULE WINS WHEN BOTH ARE SET, as a panic does, so the engine-at-fault
		// kind is not read as "error"; the error's text, naming phase and round,
		// is kept.
		if summary.Error == "" {
			summary.Error = events.ClipDiagnostic(res.Breach.Detail)
		}
		summary.ErrorKind = string(res.Breach.Kind)
	}
	e.publishEvent(ctx, t.stamp(events.New(summary, t.trace)), t.role)
	e.publishFailure(ctx, t, res, err)

	completed := types.TurnCompleted{
		Agent:         t.agentID,
		AgentHandle:   t.handle,
		RoleName:      t.role,
		TurnID:        t.runID,
		WorkKey:       t.workKey,
		WorkItem:      item,
		WorkItemBasis: basis,
		Suspended:     res.Suspended,
		StartedAt:     t.startedAt,
		EndedAt:       ended,
		DurationMS:    int(ended.Sub(t.startedAt) / time.Millisecond),
		TaskSummary:   t.trigger.Summary,
		Ask:           t.unspokenAsk(),
		AskProvenance: t.unspokenAskLabel(),
		PlanSummary:   planSummary(res),
		// ReviewOutcome is the reviewer's decision, which is the turn's
		// decision except where a guard ended it first — so it is read off
		// the result rather than off the last review, and the two differ
		// exactly when the engine overrode the reviewer.
		ReviewOutcome: decision,
		Iterations:    res.Rounds,
		// EVERYTHING THE REFLECT DISPATCHER GATES ON: its workers may run on
		// another node, so a fact left off this payload is one no worker can
		// consult, and an absent tool sequence reads as "engaged with nothing",
		// silently skipping the successful turns worth learning from.
		ToolSequence:    spend.ExecuteTools,
		AllToolNames:    spend.AllTools,
		Outcome:         spend.Outcome,
		Acted:           actedOutside(calls, deliverySurface(e.ToolsFor(t.handle))),
		PickupAnswer:    res.PickupAnswer,
		SkillsUsed:      t.skills,
		Interactions:    t.interactions,
		ConversationKey: t.convKey,
	}
	if !res.Suspended && t.measuredWhole() {
		// THE WHOLE TURN, on the record that ends it: every segment's measurement
		// and whether it failed, which the usage domain reads (types.TurnTally);
		// summing records falls short when they span nodes. Stated only when every
		// earlier segment was measured ([turnTelemetry.measuredWhole]).
		whole := t.measured(spend, ended)
		completed.Turn = &types.TurnTally{DurationMS: whole.DurationMS, Failed: failed,
			Reviews: whole.Reviews, SentBack: whole.SentBack, Unaccepted: whole.Unaccepted,
			Tools: whole.Tools, Wake: whole.Wake, Tokens: whole.Tokens, Refusals: whole.Refusals,
			// A PICKUP ANSWERED TYPED MOVED ITS ITEM, an ask included, by the
			// gate's own reading; its ledger outcome says so (ADR-0122).
			Outcome: types.OutcomeOfPickup(t.written.Outcome(), res.PickupAnswer)}
	}
	e.publishEvent(ctx, t.stamp(events.New(completed, t.trace)), t.role)
	e.publishReflectionDue(ctx, completed, t)
}

// actedOutside reports whether a successful call of the turn's could reach
// somebody outside the engine on the seat's surface ([turn.Deliverable]):
// one its record lists, or one it counts by tool without listing — a call that
// succeeded acted whether or not the record had room to list it.
func actedOutside(calls runner.RecordCalls, s turn.Surface) bool {
	for _, ex := range calls.Rows {
		name, _ := ex["name"].(string)
		if ok, _ := ex["success"].(bool); ok && turn.Deliverable(name, s) {
			return true
		}
	}
	for _, e := range calls.ByTool {
		if e.Calls > e.Failed && e.Name != "" && turn.Deliverable(e.Name, s) {
			return true
		}
	}
	return false
}

// measuredWhole reports whether this segment knows the whole turn before it: a
// first segment does, and a resumed one only if its park carried what the
// earlier segments measured. Otherwise they are UNKNOWN, not zero, and a tally
// of only what followed would be read as the whole turn; such a turn states no
// tally at its end and hands none on at a park ([turnTelemetry.chargeFor]).
//
// The nil is a fact: every park hands on a measurement ([segmentCharge.soFar]),
// a zero one as `"so_far":{}`.
func (t turnTelemetry) measuredWhole() bool { return !t.resumed || t.soFar != nil }

// measured is the turn's measurement through this segment's end: what earlier
// segments carried on the park plus this one's span (computed as its own
// `duration_ms` is), reviews and MCP tool calls off the runner's tally. A
// parking segment hands it on ([segmentCharge.soFar]); the last states it
// ([Engine.publishTurnCompleted]).
func (t turnTelemetry) measured(spend runner.Spend, ended time.Time) execstate.TurnSoFar {
	out := execstate.TurnSoFar{
		DurationMS: int(max(ended.Sub(t.startedAt), 0) / time.Millisecond),
		Reviews:    spend.Reviews,
		SentBack:   spend.SentBack,
		Unaccepted: spend.Unaccepted,
		Tools:      types.SumToolTallies(spend.Tools),
		Refusals:   types.SumRefusals(spend.Refusals),
		Wake:       types.WakeOf(t.trigger),
		// THE RUN IT COLLECTED INCLUDED, which is most of an executor turn.
		Tokens: t.segmentTokens(spend),
	}
	if t.soFar != nil {
		out.DurationMS += t.soFar.DurationMS
		out.Reviews += t.soFar.Reviews
		out.SentBack += t.soFar.SentBack
		out.Unaccepted += t.soFar.Unaccepted
		out.Tools = types.SumToolTallies(t.soFar.Tools, spend.Tools)
		out.Refusals = types.SumRefusals(t.soFar.Refusals, spend.Refusals)
		out.Tokens = t.soFar.Tokens.Plus(out.Tokens)
		// THE WAKE IS THE FIRST SEGMENT'S: a resume is woken by an answer
		// or a completion, never by what started the turn.
		if t.soFar.Wake != "" {
			out.Wake = t.soFar.Wake
		}
	}
	return out
}

// publishReflectionDue wakes reflection ON THE SEAT'S HOLDER: a
// [types.ReflectionDue] on the seat's reflection subject, which only the node
// holding the seat attaches (this one, or the one it moved to, which has
// hydrated its memory). A wake, not turn_completed again: every event is
// published once, by one node.
//
// ADDRESSED BY THE SEAT'S ID (ADR-0013): a seat the company does not name as
// an agent seat has no id and no reflection subject.
func (e *Engine) publishReflectionDue(ctx context.Context, completed types.TurnCompleted, t turnTelemetry) {
	seat, err := uuid.Parse(t.agentID)
	if err != nil {
		return
	}
	subject := topics.AgentReflect(seat)
	if subject == "" {
		return
	}
	ev := t.stamp(events.New(types.ReflectionDue{Turn: completed}, t.trace))
	ev.Source = t.role
	if err := e.backends.Queue.Publish(ctx, subject, ev); err != nil {
		log.WarnContext(ctx, "reflection_due_publish_failed", "seat", t.handle,
			"turn_id", t.runID, "error", err,
			"detail", "the turn ran and is recorded; nothing will reflect on it")
	}
}

// publishFailure publishes the DEDICATED record of why a turn stopped, as
// [turn.Breach] and [toolloop.BudgetError] say their caller does: the live
// projection's failure hold (internal/api/livestate) is taken from exactly
// these three (llm_unavailable, turn.guard_breach, budget_exhausted), so
// without them the attention queue's "the engine stopped it" row, the seat
// banner and the `broken` rail are unreachable.
//
// The summary's error/error_kind is not a copy to keep in step: it is the
// one-line reason on a row about the turn, where these carry the chain tried,
// the ceiling that refused and the guard that fired.
func (e *Engine) publishFailure(ctx context.Context, t turnTelemetry,
	res turn.Result, err error,
) {
	// A breach and an error are not exclusive: an unhandled exception is
	// both, and reporting only one would drop the guard that named it.
	if res.Breach != nil {
		e.publishEvent(ctx, events.New(types.TurnGuardBreach{
			Agent:    t.agentID,
			RoleName: t.role,
			Kind:     res.Breach.Kind,
			Detail:   events.ClipDiagnostic(res.Breach.Detail),
			TurnID:   t.runID,
			WorkKey:  t.workKey,
		}, t.trace), t.role)
	}
	if err == nil {
		return
	}

	// BUDGET BEFORE PROVIDER: disjoint in practice (a refused charge never
	// reaches a provider), but ordered so it holds here rather than by
	// whichever wrapper is outermost.
	var budget *toolloop.BudgetError
	if errors.As(err, &budget) {
		e.publishEvent(ctx, events.New(types.BudgetExhausted{
			Agent:      t.agentID,
			RoleName:   t.role,
			TurnID:     t.runID,
			WorkKey:    t.workKey,
			BudgetType: types.BudgetScope(budget.Scope),
			UsedTokens: budget.Used,
			MaxTokens:  budget.Limit,
			Period:     string(budget.Period),
			Window:     budget.Window,
			ResetsAt:   rfc3339(budget.ResetsAt),
		}, t.trace), t.role)
		return
	}

	// EVERY MEMBER FAILED RETRYABLY: the seat has no model left. A
	// non-retryable failure comes back as its backend's own error, unwrapped,
	// and "unavailable" would blame a chain never walked.
	var exhausted *chain.Error
	if errors.As(err, &exhausted) {
		e.publishEvent(ctx, events.New(types.LLMUnavailable{
			Agent:         t.agentID,
			RoleName:      t.role,
			ProviderChain: exhausted.Attempted,
			AttemptCount:  len(exhausted.Attempted),
			LastErrorKind: llm.KindOf(exhausted.Err).String(),
			LastError:     events.ClipDiagnostic(exhausted.Error()),
			TurnID:        t.runID,
			WorkKey:       t.workKey,
		}, t.trace), t.role)
	}
}

// lastModel names the model that served the last phase to run, read off the
// two models in loop order: a guard-stopped executor has no review model and
// a turn that reached no provider has neither, so a third field would only
// copy them.
func lastModel(s runner.Spend) string {
	for _, m := range []string{s.ReviewModel, s.ExecuteModel} {
		if m != "" {
			return m
		}
	}
	return ""
}

// unspokenAsk is the turn's ask for [types.TurnCompleted.Ask]: carried only
// where no interaction carries it already, since a notification's ask IS its
// interactions' bodies.
func (t turnTelemetry) unspokenAsk() string {
	if len(t.interactions) > 0 {
		return ""
	}
	return t.ask
}

// unspokenAskLabel is who wrote [turnTelemetry.unspokenAsk]: carried exactly
// when it is, and nil with it.
func (t turnTelemetry) unspokenAskLabel() *provenance.Label {
	if t.unspokenAsk() == "" {
		return nil
	}
	return t.askLabel
}

// planSummary is the reviewer's account of the turn, or the artifact.
//
// The learning subsystem reads this to build an episode. The LAST review's
// completed-work prose is the better source where it exists — it is the
// semantic layer over the engine-built call ledger — and the artifact is what
// a turn that never reached Review has instead.
func planSummary(res turn.Result) string {
	if res.LastReview != nil && res.LastReview.CompletedWork != "" {
		return res.LastReview.CompletedWork
	}
	return res.Artifact
}

// publishEvent sends one turn-level event, or logs why it could not.
func (e *Engine) publishEvent(ctx context.Context, ev *events.Event, role string) {
	// The envelope's source is the seat, for the same reason the phase
	// events set it: a consumer with no other attribution renders an
	// unsourced event as "system".
	ev.Source = role
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		log.WarnContext(ctx, "turn_telemetry_publish_failed", "type", ev.Type,
			"role", role, "error", err)
	}
}

// describeResume is describeTurn for a re-entry.
//
// The trace comes from the SUSPENDED TURN's row, not the waking event, so the
// resumed phases join the original wake's span: a clarification ANSWER is an
// ordinary inbound whose trace would file the turn's second half under
// another root.
//
// Its writes are rebased as a dispatch's are ([rebaseFor]), judged at every
// attempt against the attempt's own clock: a resume retried weeks later is
// as far past the ledger as one never judged.
func (e *Engine) describeResume(ctx context.Context, company *Company, in resumeInput) (turnTelemetry, error) {
	t := turnTelemetry{
		handle: in.Run.AgentHandle,
		// THE SAME RUN, RESUMED: its remaining phases belong to the run that
		// started them, and a second id would split one turn on every screen.
		// The work key and its start ride the row, since the resume sees no
		// trigger and a different start would derive different operation ids;
		// read off the resumed turn (resumedTurn), which read the row once.
		runID:     in.Run.TurnID,
		workKey:   in.Turn.WorkKey,
		workSince: in.Turn.WorkSince,
		// EACH CONVERSATION VALUE FROM ITS OWN FIELD: events are tagged with
		// the conversation it answers on, and the partition it launched from
		// carries forward so a re-park writes the pair a first launch would.
		// Reading ConversationKey (the IDENTITY) for both loses what tells two
		// questions on one direct message apart ([sandbox.ConversationRef.Best]),
		// and a reply in one thread resumes the run waiting in the other.
		convKey:   in.Run.ConversationKey,
		partKey:   in.Run.PartitionKey,
		startedAt: time.Now().UTC(),
		role:      in.Run.Role,
		agentID:   in.Run.AgentID,
		// WHO ASKED, off the row rather than off whatever resumed it: an
		// answer is somebody replying to the run, not somebody asking this
		// turn for something, so a second run the resumed turn detaches is
		// still the first requester's.
		requester: in.Run.Requester, workingFor: in.Run.WorkingFor,
		// AND THE CREDENTIAL, off the row for the same reason: the turn is
		// still worked for whoever its first segment was.
		requesterLogin: in.Run.RequesterLogin,
		requesterApps:  slices.Clone(in.Run.RequesterApps),
		// WHAT IT WAS ASKED, off the park: the event that resumed it is a
		// collection or a reply, and its ask is not the turn's.
		ask:      in.State.Ask,
		askLabel: in.State.AskProvenance,
		// And what its run is listed under, off the row its first launch
		// wrote, for the same reason: a run this segment launches is listed
		// as that one is.
		title: in.Run.TaskDescription,
		// AND WHO SAID IT, off the park for the same reason — and who
		// asked, read off them as a dispatch reads it.
		interactions: in.State.Interactions,
		senders:      sendersSpoken(in.State.Interactions),
		// AND WHEN ITS CAP ENDS, off the park: the deadline is the turn's,
		// fixed when it began, never one measured from this segment.
		deadline: in.State.Deadline,
		// The resumed turn's OWN span, opened by resumeTurn under the
		// reconstructed suspended one, so its events carry a placeable span_id.
		trace: tracing.TraceOf(ctx),
	}
	if in.Trigger != nil {
		t.trigger = types.DescribeTrigger(in.Trigger)
	}
	// RULE 3: the item the parked row recorded, never one read off the
	// event that resumed it — a person's answer is a chat message, and it
	// names no item. And the set the turn had written before it parked,
	// carried on the suspension, so a completion concluding a sole write
	// judges the whole turn rather than its second half.
	t.workItem, t.workItemBasis = resumedWorkItem(in.Run)
	// AND THE ANSWERS THAT WOKE IT, off the row for the same reason.
	t.answered = slices.Clone(in.Run.Answered)
	committed := make([]types.CommittedChange, 0, len(in.State.Committed))
	for _, c := range in.State.Committed {
		committed = append(committed, types.CommittedChange(c))
	}
	t.written = turnctx.WrittenFrom(in.State.Written, in.State.WrittenMany, committed...)
	// WHAT THE RUN PUSHED ON GITLET is code the turn committed, collected here.
	if len(in.Account.Pushed) > 0 {
		t.written.Commit(types.CommittedCode)
	}
	// AND WHAT ITS CALLS COMMITTED, off its call log: they were made after
	// the park recorded the set above, so the outcome is settled here.
	for _, c := range in.Bridged.Changes() {
		t.written.Commit(types.CommittedChange(c))
	}
	// THE HOLDS THE ROW CARRIES, OFF THE RESUMED TURN for the reason its
	// unit of work is (resumedTurn read the row into it, with the row's
	// writer attached): the same turn, holding what it named before it parked.
	t.holds = in.Turn.Holds
	// A FRESH TALLY, never the parked one's: what the segments before this
	// one spent on auxiliary calls is in what they charged or carried.
	t.auxSpent = auxspend.NewTally()
	// AND A FRESH FENCE TOKEN: the parked conversation's fences keep the
	// token they were written with, and their content carries none.
	t.boundary = provenance.NewBoundary()
	// THE SEGMENT, for its charge: the job it collected and what that job
	// cost, and what the segments before it spent that nothing paid for.
	t.resumed, t.launchID = true, in.Run.LaunchID
	t.jobInput, t.jobOutput = in.InputTokens, in.OutputTokens
	t.jobCacheRead, t.jobCacheWrite = in.CacheRead, in.CacheWrite
	t.jobEngine = in.Engine
	t.uncharged = in.State.Uncharged
	t.soFar = in.State.SoFar
	// Re-derived from the org, so a resumed turn names its seat's role as
	// the running company calls it now.
	if role, agentID := seatIdentity(company, in.Run.AgentHandle); role != "" {
		t.role = role
		if agentID != "" {
			t.agentID = agentID
		}
	}
	rebased, err := rebaseFor(ctx, e.rebases(), builtin.Actor{
		TurnID: t.runID, WorkKey: t.workKey, WorkSince: t.workSince,
	}, t.startedAt)
	if err != nil {
		return turnTelemetry{}, err
	}
	t.rebasedTo = rebased
	return t, nil
}

// seatIdentity is the role name and agent id a seat's events are addressed
// to, or two empty strings for a handle this company does not name.
//
// ONE DERIVATION for the turn's telemetry, the resumed turn's and a panic's
// guard breach, so they agree on a human seat (no agent id) and an unknown
// handle (no role). Nil-safe on the company: the panic path can run on a node
// with no epoch yet.
func seatIdentity(company *Company, handle string) (role, agentID string) {
	if company == nil || company.Org == nil {
		return "", ""
	}
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return "", ""
	}
	if id, ok := company.Org.AgentIDFor(seat); ok {
		agentID = id.String()
	}
	return seat.Name, agentID
}

// seatAgentID is the agent id of the AGENT seat a handle names, through
// [org.Organization.AgentSeatByHandle], and false for a human seat, a handle
// no seat has, and a node with no active company.
// It is what the event log's party index is keyed on ([store.DB.SetEventSeats]).
func seatAgentID(company *Company, handle string) (string, bool) {
	if company == nil || company.Org == nil {
		return "", false
	}
	seat := company.Org.AgentSeatByHandle(handle)
	if seat == nil {
		return "", false
	}
	id, ok := company.Org.AgentIDFor(seat)
	if !ok {
		return "", false
	}
	return id.String(), true
}

// requesterIdentity is a requester's handle, or "" for one the org does not
// hold: a seat the company does not have is nobody to put a question to.
func requesterIdentity(company *Company, handle string) string {
	if handle == "" || company == nil || company.Org == nil {
		return ""
	}
	if seat := company.Org.Role(handle); seat != nil {
		return seat.Handle()
	}
	return ""
}

// requesterOf is the seat whose wake started seat's turn, or "" when no other
// seat's did.
//
// OFF THE FIRST EVENT, as [Engine.describeTurn] reads the trigger: a
// colleague's ask names its asker; otherwise the first speaker of the
// interactions, resolved to a handle in the chart or nothing. ONLY the first:
// a later voice in the thread did not start the conversation.
//
// NEVER THE SEAT ITSELF: the outcome of its own act (a workflow its push
// failed) wakes it as its own sender, and naming itself would route a run's
// questions back to it; the requester is whoever spoke first besides it.
func requesterOf(evs []*events.Event, interactions []types.InboundInteraction, seat string) string {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if ask, ok := events.DataAs[*types.A2ARequest](ev); ok {
			return ask.Requester
		}
		break
	}
	for _, in := range interactions {
		if in.Sender.Handle != "" && in.Sender.Handle == seat {
			continue
		}
		return in.Sender.Handle
	}
	return ""
}

// credentialRequesterOf is the credential acting as no seat whose write
// started seat's turn, by its login, and the apps every one of its writes in
// the turn carried, or nothing where [requesterOf]'s speaker is no such
// credential. A credential is never a seat's handle, so the two never both
// answer.
//
// ITS APPS ARE EVERY WRITE'S, intersected, off the ask's constituents (the
// interactions carry no apps): a merge must not launder a constraint. A write
// carrying none leaves none, and a credential's turn with no apps is worked
// for as a stranger (ADR-0109).
func credentialRequesterOf(evs, ask []*events.Event, interactions []types.InboundInteraction,
	seat string) (string, []iam.Grant) {

	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if _, ok := events.DataAs[*types.A2ARequest](ev); ok {
			return "", nil
		}
		break
	}
	login := ""
	for _, in := range interactions {
		if in.Sender.Handle != "" && in.Sender.Handle == seat {
			continue
		}
		login = in.Sender.Login
		break
	}
	if login == "" {
		return "", nil
	}
	var (
		apps []iam.Grant
		read bool
	)
	for _, w := range writersIn(ask) {
		if w == nil || w.Kind != iam.ActorOperator || strings.TrimSpace(w.Login) != login {
			continue
		}
		if !read {
			apps, read = slices.Clone(w.Apps), true
			continue
		}
		apps = slices.DeleteFunc(apps, func(g iam.Grant) bool { return !slices.Contains(w.Apps, g) })
	}
	return login, apps
}

// writersIn is every first-party writer a turn's trigger names, constituent
// by constituent in the order they spoke — the rule [interactionsFor] reads
// the interactions by: a coalesced notification's constituents, never its
// flat fields beside them, which are the latest constituent's again.
func writersIn(evs []*events.Event) []*types.NotificationWriter {
	var out []*types.NotificationWriter
	for _, n := range notificationsIn(evs) {
		if len(n.Messages) == 0 {
			out = append(out, n.Writer)
			continue
		}
		for _, m := range n.Messages {
			out = append(out, m.Writer)
		}
	}
	return out
}

// workingForIn is who the wake that started a turn handed it on to work for
// ([types.WorkingFor]), off the first event as [requesterOf] reads the asker:
// a colleague's ask carries whom the asking turn worked for; the answer to
// this seat's own ask, whom this seat worked for when it asked; an agent's
// write on Worklet, Doclet, Chatlet or Gitlet, or the engine's Gitlet record
// about an agent's earlier enqueue, whom that was made for ([writtenForIn]).
// Nil for any other wake, and for one that carried nobody.
func workingForIn(evs []*events.Event) *types.WorkingFor {
	for _, ev := range evs {
		if ev == nil {
			continue
		}
		if ask, ok := events.DataAs[*types.A2ARequest](ev); ok && ask != nil {
			return ask.WorkingFor
		}
		if answer, ok := events.DataAs[*types.A2AMessage](ev); ok && answer != nil {
			return answer.WorkingFor
		}
		// A PICKUP IS WORKED FOR WHOM ITS ITEM IS FOR (ADR-0109, ADR-0112).
		if pickup, ok := events.DataAs[*types.WorkPickup](ev); ok && pickup != nil {
			return pickup.WorkingFor
		}
		// A WRITE IS WORKED FOR WHOM IT WAS WRITTEN FOR (ADR-0109).
		if n, ok := events.DataAs[*types.ExternalNotification](ev); ok && n != nil {
			return writtenForIn(n)
		}
		return nil
	}
	return nil
}

// writtenForIn is whom the agent's write a notification announces was made
// for ([types.NotificationWriter.WorkingFor]): its FIRST constituent's, since
// the partition splits on it (notify.KeyOf), or the notification's own where
// nothing was coalesced. The engine's writer counts too, carrying an agent's
// earlier act's carrier (ADR-0109's amendment, notify.FirstPartyWriter). Nil
// for a vendor's notification and for a person's or credential's writer, who
// write for nobody but themselves.
//
// THE ONE READING for the turn it wakes, a parked run's reply
// ([replyWorkingFor]) and a chat edit's worked-through key, so none drifts
// from the partition that grouped the delivery.
func writtenForIn(n *types.ExternalNotification) *types.WorkingFor {
	w := n.Writer
	if len(n.Messages) > 0 {
		w = n.Messages[0].Writer
	}
	if w == nil || (w.Kind != iam.ActorAgent && w.Kind != iam.ActorSystem) {
		return nil
	}
	return w.WorkingFor.Clone()
}

// sendersSpoken is every distinct identifiable sender of a turn's
// interactions, in the order they first spoke — the set [sendersOf] gives the
// turn-start prefetch, read off the interactions the same resolution built.
func sendersSpoken(interactions []types.InboundInteraction) []types.CanonicalIdentity {
	var (
		out  []types.CanonicalIdentity
		seen = map[types.CanonicalIdentity]bool{}
	)
	for _, in := range interactions {
		id := in.Sender
		if (id.Handle == "" && (id.ExternalID == "" || id.Platform == "")) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
