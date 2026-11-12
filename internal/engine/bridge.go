package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// Where a bridged run's tool calls are kept.
//
// An engine phase's calls sit on a surface in memory, and the turn writes them
// when it ends. An executor's run cannot: its calls are made by its harness,
// a process outside the engine, over the bridge, minutes or hours apart, and
// the run can outlive the node that started it. So each one is written to the
// run's own log in the coordination store, kept beside its row
// ([sandbox.BridgeLog]) — its identity before it runs, and what it did once it
// returns ([bridgeLedger]) — which is what the resume reads; and without it a
// restart mid-run leaves the reviewer judging a turn whose entire tool log is
// gone, which the delivery check reads as a turn that acted on nothing.

// bridgedCalls turns a run's durable bridged-call log into the shape the
// resumed phase reads.
//
// THE ONLY RECORD A RUN'S RESUME HAS. The process collecting a run may
// not be the one that launched it, so its tool surface is fresh and has
// executed nothing: the delivery check, the submission's citations, the
// iteration ledger and the executor's own record all read this list. A call
// whose arguments cannot be decoded keeps its name and loses its arguments,
// which renders one ledger line worse — failing the resume over it would lose
// the whole turn.
//
// EACH CALL WITH THE BRIDGE'S MEASUREMENT OF IT — its place in the order the
// bridge admitted the run's calls, its start, its duration and who answered
// it — because the executor's record places the run's calls on its timeline,
// and in their order, by exactly that. A call the bridge did not measure keeps
// a zero start, which every reader takes for "not measured". The round is the
// runner's to state: the run's record has one, and it is not this layer's.
//
// AND WITH ITS IDENTITY AND WHAT THE LOG KEPT OF IT: the digest the call was
// admitted under, which a resumed segment seeds its call log by — or that its
// arguments are withheld, a call held for a person's approval and the one they
// approved, which it seeds as a call of its tool whose arguments are not known
// — and the size of the whole arguments and output where the log kept less
// ([sandbox.BridgeCall.ArgsBytes]), which the collection's replay decides by.
// The arguments are the log's, a left-out value's marker included: a reader
// that shows them shows a left-out payload as left out.
//
// AND WITH ITS PLACE IN THE ORDER THE LOG TOOK THE ROUND'S CALLS
// ([sandbox.BridgeCall.Appended]), which a relaunch that continues a kept
// session tells the calls after its save point by (runner.SessionBasis.Through),
// and WHETHER THE LOG TOOK IT LATE, after its job was replaced
// ([sandbox.BridgeCall.Late]), or ITS HARNESS STOPPED WAITING FOR IT before it
// returned ([sandbox.BridgeCall.Abandoned]), neither of which a save point
// holds.
//
// AND THE HELD CALL AN APPROVED ONE WAS MADE FOR ([sandbox.BridgeCall.HeldID]):
// a later attempt at an answer reads the call that answer approved in the
// round's log, which its relaunch's lead tells what became of, so no list of
// the round's calls in that relaunch's brief tells it again — while one an
// earlier answer of the round approved, which no lead of it tells, is told as
// any call is (runner.BridgedCall.HeldID).
func bridgedCalls(logged []sandbox.BridgeCall) []runner.BridgedCall {
	out := make([]runner.BridgedCall, 0, len(logged))
	for _, call := range logged {
		out = append(out, runner.BridgedCall{
			Call: ledger.Call{
				Name:   call.Name,
				Args:   decodeBridgeArgs(call.Args),
				Result: call.Output,
				Failed: call.Failed,
				Digest: call.Digest,
				// WITHHELD, the arguments and their digest with them: the
				// log keeps neither of a held call or an approved one.
				Withheld: call.Withheld(),
			},
			Seq:         call.Seq,
			Appended:    call.Appended,
			Late:        call.Late,
			Abandoned:   call.Abandoned,
			StartedAt:   call.StartedAt,
			Duration:    time.Duration(call.DurationMS) * time.Millisecond,
			Origin:      call.Origin,
			Server:      call.Server,
			Source:      call.Source,
			Refusal:     mcp.Refusal(call.Refusal),
			ArgsBytes:   call.ArgsBytes,
			OutputBytes: call.OutputBytes,
			Held:        call.Held,
			Approved:    call.Approved,
			HeldID:      call.HeldID,
		})
	}
	return out
}

// unfinishedCalls is a run's calls still IN FLIGHT as its log was read
// ([sandbox.BridgeLog.InFlight]) — each begun and not finished: running still,
// or its process gone, or its update refused by the store — in the shape the
// resumed phase reads them: each one's identity alone, its tool, its place, its
// start and the digest it was admitted under (none where its tool may ask a
// person first, which the call log counts as a call of its tool whose arguments
// are not known), MARKED AS A CALL WHOSE OUTCOME IS UNKNOWN
// ([ledger.Call.Unfinished]): failed, so no reader takes it for a delivery, a
// submission or a load, and answered with the words that say its effects may
// stand ([runner.UnfinishedNote]), never as a plain failure, which a run would
// make again.
func unfinishedCalls(entries []sandbox.BridgeCall) []runner.BridgedCall {
	if len(entries) == 0 {
		return nil
	}
	out := make([]runner.BridgedCall, 0, len(entries))
	for _, entry := range entries {
		out = append(out, runner.BridgedCall{
			Call: ledger.Call{Name: entry.Name, Result: runner.UnfinishedNote, Failed: true,
				Digest: entry.Digest, Unfinished: true},
			Seq: entry.Seq, StartedAt: entry.StartedAt,
		})
	}
	return out
}

// identitiesOf is a tally a run's log keeps of calls it does not list, by
// identity — the round's calls it dropped ([sandbox.BridgeLog.Unlisted]), or
// the ones its call log absorbed from a bridged call's own work
// ([sandbox.BridgeLog.Absorbed]) — in the shape the turn's record carries it
// ([ledger.Elided]). Handed the log's tally through those two readers, never
// off the fields.
func identitiesOf(ids []sandbox.ElidedCall) []ledger.Elided {
	if len(ids) == 0 {
		return nil
	}
	out := make([]ledger.Elided, 0, len(ids))
	for _, e := range ids {
		out = append(out, ledger.Elided{Name: e.Name, Digest: e.Digest, Calls: e.Calls, Failed: e.Failed})
	}
	return out
}

// decodeBridgeArgs reads the JSON text a bridged call was recorded with.
//
// [tools.ReadArgs] rather than a bare unmarshal, because this is the SECOND
// decode of the id the log went out of its way to keep exact: a bridged call
// naming a 19-digit issue id would otherwise reach the resumed turn's ledger
// as a rounded one.
func decodeBridgeArgs(raw string) map[string]any {
	return tools.ReadArgs(raw)
}

// bridgeLedger writes a bridged run's calls on the log beside its pending-run
// row, and with each finished one what the run's bridged calls have cost the
// engine so far, which the row keeps.
//
// EVERY CALL IS ON THE LOG BEFORE IT RUNS. Its entry — its identity, and the id
// its harness gave it ([sandbox.BridgeCall.CallID]), which is no part of that
// identity — is written ahead of it ([bridgeLedger.Begin],
// [sandbox.BridgeLog.InFlight]), and the bridge runs the call only once the log
// has taken it; then the call makes ONE UPDATE of that entry as it returns
// ([bridgeLedger.Record]), with what it answered, cost and committed. So no
// call runs off the record of its run: a resumed segment's call log counts
// every call the run made, a call still running elsewhere included, and a later
// call that asks for something else never derives an earlier call's operation
// id. An ENTRY THE STORE DOES NOT TAKE within [beginBudget] is a call not run,
// its box told it could not be recorded and may make it again
// (`bridge_call_not_recorded`); an UPDATE THE STORE DOES NOT TAKE within
// [appendAttempts] leaves the entry — the call's identity on the record — and
// is answered [mcpbridge.ErrUnrecorded], so the call's own goroutine on the
// bridge asks for the same update again once the call's answer is out, until it
// lands or the call's bound passes. Nothing is carried by another write: no
// later write of the session has anything of this one's to bring.
//
// ONE PER SESSION, so one per job, and BOUND TO THAT JOB AND ITS ROUND BEFORE
// ITS BOX EXISTS ([bridgeLedger.bind], handed to
// [sandbox.LaunchRequest.Opened]): every append names both, so a call that
// outlives its job is listed in the round it was made in, marked late and its
// spend nobody's, where a relaunch carried the round on, and dropped where a
// relaunch started a new round — never one of the next job's own calls (see
// [sandbox.PendingStore.AppendBridgeCall]). It used to learn the job from the
// row on its first append, and a session whose FIRST call outlived its job — a
// delegated worker still running when the reviewer relaunched — learned the
// next job's name and filed its call and its spend there.
type bridgeLedger struct {
	store sandbox.PendingStore

	// spend is the run's bridged meter, whose running total rides every
	// record — see [runner.BridgedSpend]. Nil records the calls alone.
	spend bridgedMeter

	// nudge tells whichever waiter drives the run that its work is in
	// ([sandbox.NudgeSubmitted]), once a `submit_work` the run made is in
	// its log. Nil tells nobody, and the run's next poll finds it.
	nudge func(ctx context.Context, n sandbox.Nudge)

	// log is where the ledger says what it could not record: the engine's
	// own, unless a case hands it one it reads ([bridgeLedger.logger]).
	log *slog.Logger

	// runner is who runs the session's calls, as every entry it writes names
	// it ([sandbox.BridgeCall.Runner]): the incarnation this process holds its
	// taker lease under ([Engine.callRunner]), by which a collection on any
	// node reads whether this process still runs a call — "" where it holds
	// none, whose entries are waited for by their bound alone. Set by
	// [Engine.openBridge] before the session opens, and read-only after.
	runner string

	mu sync.Mutex
	// launch is the job this session's calls are recorded under, as the
	// store opened it, and round the round that job works in
	// ([sandbox.OpenedJob.Round]).
	launch, round string
}

// appendRetryWaits are the waits between the attempts of one write of a
// bridged call's ([bridgeLedger]): a QUARTER OF A SECOND, HALF OF ONE AND A
// WHOLE ONE, 1.75 s in all. Each is one compare-and-swap of the run's log —
// and, for a record that moved the spend, one of its row — which a healthy
// coordination store answers in milliseconds; what a retry rides over is a
// stream's leader stepping down mid-write, whose successor the broker elects
// within about a second (nats-server's campaign timeout is 800 ms at the
// most). So the first waits are short, for a blip that clears at once, and the
// last attempt comes once that second has passed, however quickly the attempts
// before it failed: two waits of 250 and 500 ms ended the retry before a
// re-election it was there to outlast. Every second spent here is a second the
// box waits for its call's answer. A store out for longer — a leader lost
// rather than stepping down, which its followers wait four seconds and more to
// replace — refuses a call's entry, so the call is not run, and leaves a
// finished call's update unwritten for now, its entry standing: the bridge asks
// for it again once the call's answer is out (internal/api/mcpbridge's
// owedUpdateBase), so the box waits for none of that.
var appendRetryWaits = [...]time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// appendAttempts is how many times one write of a bridged call's is tried:
// once, and again after each of [appendRetryWaits].
const appendAttempts = len(appendRetryWaits) + 1

// beginBudget bounds the write of a call's entry ahead of it
// ([bridgeLedger.Begin]), its retries ([appendRetryWaits]) included, and so a
// withdrawal's: FIVE SECONDS, as the write that records a job's session start
// is given ([sessionStartRecordBudget]), and for its reason — one
// compare-and-swap of the run's log, which a healthy coordination store
// answers in milliseconds (11.89 at the median and 17.04 at the 99th
// percentile, measured: internal/coord/kv's BenchmarkABoxRequestOnItsRun, its
// begin arm), and a store that has not answered in five is in an outage the
// call should not wait out: past it the call is
// not run, and its box is told it may make it again. The box waits on it with
// nothing written on the call's response — its first progress follows the
// write — which a harness gives a minute (Claude Code 2.1.310's first-byte
// budget, see internal/api/mcpbridge's answerEvery). None before: the call ran
// first, and was recorded after it.
const beginBudget = 5 * time.Second

// nameDigestPrefix is how much of a call's identity a line naming one carries
// ([namedCall]): EIGHT HEX DIGITS, the four bytes of a digest the log's own
// markers name a left-out value by (internal/kept), which tells a session's
// calls of one tool apart in a log line without making the line a second copy
// of the record.
const nameDigestPrefix = 8

var _ mcpbridge.Ledger = (*bridgeLedger)(nil)

// bridgedMeter is a run's bridged meter, as the ledger reads it.
type bridgedMeter interface{ Total() runner.Bridged }

// newBridgeLedger is the ledger of one bridge session. A nil meter — a run
// launched with none — records the calls alone, and is never stored as an
// interface holding a nil pointer.
func newBridgeLedger(store sandbox.PendingStore, meter *runner.BridgedSpend) *bridgeLedger {
	l := &bridgeLedger{store: store}
	if meter != nil {
		l.spend = meter
	}
	return l
}

// bind names the job this session serves, and the round it works in. Called
// once, by the launch, the moment the store has opened the job and before its
// box exists — or by the node that serves the job again once it takes its seat
// over — with the job as the row names it ([sandbox.PendingRun.NamedJob]).
func (l *bridgeLedger) bind(job sandbox.OpenedJob) {
	l.mu.Lock()
	l.launch, l.round = job.LaunchID, job.Round
	l.mu.Unlock()
}

// bound is the job and round the session's calls are recorded under, and
// whether it was bound to any: a call on a session no job was bound to cannot
// happen through the launch, which binds before the box that makes calls
// exists, so it is a wiring mistake — said in the log rather than filed under
// whatever job the row holds.
func (l *bridgeLedger) bound(ctx context.Context, runID, tool string) (launch, round string, ok bool) {
	l.mu.Lock()
	launch, round = l.launch, l.round
	l.mu.Unlock()
	if launch == "" {
		l.logger().WarnContext(ctx, "bridge_ledger_unbound", "run_id", runID, "tool", tool,
			"detail", "a bridged call arrived on a session no job was bound to, so it "+
				"is recorded under none; the launch binds a session before its box exists")
		return "", "", false
	}
	return launch, round, true
}

// errUnbound is a call's entry refused on a session no job was bound to
// ([bridgeLedger.bound]): with nothing to record it under, the call is not run.
var errUnbound = errors.New("engine: the call's bridge session is bound to no job, so its entry has nowhere to go")

// Begin writes a call's entry on its run's log ahead of it
// ([mcpbridge.Ledger.Begin]): its identity, who runs it until when
// ([bridgeLedger.runner], [mcpbridge.Begun.Until]), and the id its harness gave
// it ([sandbox.BridgeCall.CallID]), which the call's update carries too and
// nothing finds the entry by. Tried again on a store's error within
// [beginBudget], under a context of its own: the box's request is not the
// call's, and a write cut with it would refuse a call the store would have
// taken.
//
// NIL IS A CALL THE BRIDGE MAY RUN. [mcpbridge.ErrNotTaken] is a log open
// neither for the session's job nor, by its round, for a job of its — the run
// ended, a relaunch replaced the job, its round is over — and any other error
// an entry not written: a store that refused every attempt, or a log holding
// as many calls in flight as it keeps ([sandbox.ErrCallsInFlight]). Each is
// said, at warning, naming the call (`bridge_call_not_recorded`): the call was
// not run, and its box is told so.
func (l *bridgeLedger) Begin(ctx context.Context, runID string, call mcpbridge.Begun) error {
	if l == nil || l.store == nil {
		return nil
	}
	launch, round, ok := l.bound(ctx, runID, call.Name)
	if !ok {
		return errUnbound
	}
	entry := sandbox.BridgeCall{Name: call.Name, Seq: call.Seq, StartedAt: call.StartedAt,
		Digest: call.Digest, CallID: call.CallID, Runner: l.runner, Until: call.Until}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), beginBudget)
	defer cancel()
	taken, err := l.stepRetrying(ctx, runID, sandbox.BridgeAppend{Step: sandbox.AppendBegin,
		Launch: launch, Round: round, Call: entry})
	switch {
	case err != nil:
		l.notRecorded(ctx, runID, launch, entry, err.Error())
		return err
	case !taken:
		l.notRecorded(ctx, runID, launch, entry, "the run's log takes no more calls of this job: "+
			"the run ended, or a relaunch replaced the job")
		return mcpbridge.ErrNotTaken
	}
	return nil
}

// Withdraw takes back the entry of a call that, its entry written, was then not
// run ([mcpbridge.Ledger.Withdraw]), within [beginBudget] as its writing was. A
// log that holds no such entry — one whose write never landed, or that moved on
// — takes nothing, which is no error.
func (l *bridgeLedger) Withdraw(ctx context.Context, runID string, call mcpbridge.Begun) error {
	if l == nil || l.store == nil {
		return nil
	}
	launch, round, ok := l.bound(ctx, runID, call.Name)
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), beginBudget)
	defer cancel()
	_, err := l.stepRetrying(ctx, runID, sandbox.BridgeAppend{Step: sandbox.AppendWithdraw,
		Launch: launch, Round: round,
		Call: sandbox.BridgeCall{Name: call.Name, Seq: call.Seq, StartedAt: call.StartedAt}})
	return err
}

// Record is one finished call's ONE UPDATE ([mcpbridge.Ledger.Record]): its
// entry, found by its identity, taken out and the call listed in its place,
// with what it answered and what it committed, and the session's running
// spend. See [mcpbridge.Ledger] for why an error here never reaches the box.
//
// THE TOTAL IS READ AFTER THE CALL RAN, so it includes what the call itself
// cost; two calls finishing together each write the total they read, and the
// store keeps the newest of them ([sandbox.EngineSpend.Newest]). A call whose
// update fails has its cost carried by the next record that lands, since the
// total is cumulative — only the last call of a run can lose it, and then the
// task is charged short of what the turn shows rather than past it.
//
// AN UPDATE THAT DID NOT LAND LEAVES THE CALL'S ENTRY, its identity on the
// record, and is answered [mcpbridge.ErrUnrecorded] beside the store's error:
// the bridge asks for it again once the call's answer is out, and asked again
// a record whose earlier answer was lost after it landed lands nothing twice
// ([sandbox.PendingStore.AppendBridgeCall]). Until it lands, a resumed segment
// reads the call as one whose outcome is not known. Said at debug, naming the
// call: the bridge says at warning what it asks again and what it gave up on.
func (l *bridgeLedger) Record(ctx context.Context, runID string, call mcpbridge.Call) error {
	if l == nil || l.store == nil {
		return nil
	}
	launch, round, ok := l.bound(ctx, runID, call.Name)
	if !ok {
		return nil
	}
	a := sandbox.BridgeAppend{
		Step: sandbox.AppendRecord,
		// THE JOB AND ITS ROUND: the log lists the call as its job's own
		// while it is open for the job, and as a late call of the round once
		// a relaunch has carried the round on ([sandbox.BridgeCall.Late]).
		Launch: launch, Round: round,
		Call: sandbox.BridgeCall{
			Name: call.Name,
			// ENCODED HERE, once. The log is JSON in the coordination
			// store, so a decoded map would be re-encoded by the store's
			// own pass — and a large id survives one round trip through a
			// json.Number-aware decode and not two through the default one.
			Args:   encodeArgs(call.Args),
			Output: call.Output,
			Failed: call.Failed,
			// THE BRIDGE'S MEASUREMENT, kept for the executor's record:
			// the one account of the order the run's calls were made in,
			// when each ran and who answered it — and, with the tool, the
			// identity the update finds the call's entry by. See
			// [sandbox.BridgeCall.StartedAt].
			Seq:        call.Seq,
			StartedAt:  call.StartedAt,
			DurationMS: call.Duration.Milliseconds(),
			Origin:     call.Origin,
			Server:     call.Server,
			Source:     call.Source,
			Refusal:    string(call.Refusal()),
			Held:       call.Held,
			// THE CALL'S IDENTITY, as the bridge admitted it and before the
			// log keeps any of it: the arguments above go whole, and the
			// store holds them to the call's share of the log, so this is
			// what a later segment's call log counts the call by.
			Digest: call.Digest,
			// AND THE ID ITS HARNESS GAVE IT, as its entry carries it: the
			// box's word, which names the call as its harness's session
			// does, and which nothing finds the call by.
			CallID: call.CallID,
			// WHAT IT COMMITTED, which the run's collection settles the
			// turn's outcome by ([Engine.describeResume]).
			Committed: changeNames(call.Committed),
			// WHETHER ANYBODY WAITED FOR ITS ANSWER as it returned: a call
			// its harness gave up on, or that outlived its opening, is held by
			// no save point ([sandbox.BridgeCall.Abandoned]).
			Abandoned: call.Abandoned,
		},
		// WHAT THIS CALL'S OWN WORK ADDED to the run's call log.
		Absorbed: identitiesCalled(call.Absorbed),
		Spent:    l.spent(),
	}
	recorded, err := l.appendRetrying(ctx, runID, a)
	if err != nil && !recorded {
		l.logger().DebugContext(ctx, "bridge_call_update_refused", "run_id", runID,
			"launch_id", launch, "call", namedCall(a.Call), "attempts", appendAttempts, "error", err.Error())
		return fmt.Errorf("%w: %w", mcpbridge.ErrUnrecorded, err)
	}
	if !recorded {
		// THE ORDINARY END OF A ROUND for the call: its log is gone, or takes
		// a new round's calls. A late call of a round carried on never comes
		// here: the log lists it, marked, and the run's record says so where
		// a person reads its calls — `late` on the runs answer.
		if call.Abandoned {
			// NOR FOR A CALL NOBODY WAITED FOR: its harness was handed its
			// opening's end's answer, or gave up on it, so the session that
			// made it does not hold it either, and now no record of the run
			// does, though it ran and its effect happened. Which is what a
			// call still running as its run's ending deleted its record comes
			// to (internal/api/mcpbridge, "An opening ends without waiting for
			// its calls").
			l.logger().WarnContext(ctx, "bridge_abandoned_call_after_its_job", "run_id", runID,
				"launch_id", launch, "call", namedCall(a.Call),
				"detail", "a call nobody waited for returned once the run's log no longer took the "+
					"calls of its round: the run ended, or a new round began; it ran and its effect "+
					"happened, and neither the session that made it nor any record of the run holds it")
			return nil
		}
		l.logger().DebugContext(ctx, "bridge_call_after_its_job", "run_id", runID,
			"launch_id", launch, "tool", call.Name)
		return nil
	}
	// THE RUN'S WORK IS IN, or its question is asked, and its harness is about
	// to end its turn and exit: the waiter that drives it polls it now
	// rather than at its next pass ([sandbox.Waiter.Nudge]). After the call
	// is in the log, so the poll that finds the run done collects a log
	// that holds its submission, and a row that holds its question (the
	// ask was recorded before its call returned), as does a call held for a
	// person's approval, recorded as the job's question before its call
	// returned. A call the gate refused is no end (the run goes on) and
	// tells nobody. Told even where the call's spend could not be written to
	// the row after it: the call is in, and the next call that lands carries
	// the spend. And told of a late call of the round too, which names a job
	// the row has moved on from: no waiter polls a job for a nudge that names
	// another ([sandbox.Nudge]).
	if l.nudge != nil && ((sandbox.EndsRun(call.Name) && !call.Failed) || call.Held) {
		l.nudge(ctx, sandbox.Nudge{TurnID: runID, LaunchID: launch, Why: sandbox.NudgeSubmitted})
	}
	return err
}

// notRecorded says, at warning, that call was not run because the run's log
// did not take its entry ahead of it ([bridgeLedger.Begin]), and why.
func (l *bridgeLedger) notRecorded(ctx context.Context, runID, launch string, call sandbox.BridgeCall,
	why string) {

	l.logger().WarnContext(ctx, "bridge_call_not_recorded", "run_id", runID, "launch_id", launch,
		"call", namedCall(call), "why", why,
		"detail", "the call was not run: its entry could not be written on the run's log ahead of it, "+
			"and its box was told so")
}

// sessionStartRecordBudget bounds the write that records which session a job's
// box said it started ([bridgeLedger.started]): FIVE SECONDS, as a note's pull
// mark is given ([steerPublishBudget]), and for its reason — one compare-and-swap
// of one row, which a healthy coordination store answers in milliseconds, and a
// store that has not answered in five is in an outage this write waits out no
// better. A record that missed it is read as a session of the job's own by
// every later rebuild of the run's guard: the safe direction.
const sessionStartRecordBudget = 5 * time.Second

// changeNames is changes as the run's call log spells them.
func changeNames(changes []types.CommittedChange) []string {
	if len(changes) == 0 {
		return nil
	}
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, string(c))
	}
	return out
}

// started records which session the job's box said it started
// ([sandbox.PendingStore.MarkSessionStart]), for every later rebuild of the
// run's skill guard — a collection, a relaunch carrying the round on, a node
// taking the run over — to read off the job's record.
//
// OFF THE PATH OF THE REQUEST THAT SAID IT, under ctx stripped of its
// cancellation, as a note's pull mark is written ([runInbox.markPulled]): that
// request is the run's first to its bridge, its MCP handshake as likely as not,
// which a harness holds to a short connect timeout, and the guard it waits on is
// already held to the session in this process. A record not yet written reads
// as a session of the job's own, which is what a collection racing it then
// rebuilds: the safe direction.
func (l *bridgeLedger) started(ctx context.Context, runID string, start sandbox.SessionStart) {
	if l == nil || l.store == nil {
		return
	}
	l.mu.Lock()
	launch := l.launch
	l.mu.Unlock()
	if launch == "" {
		return
	}
	base := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(base, sessionStartRecordBudget)
		defer cancel()
		if _, err := l.store.MarkSessionStart(ctx, runID, launch, start); err != nil {
			log.WarnContext(ctx, "executor_session_start_unrecorded", "run_id", runID,
				"launch_id", launch, "session_start", string(start), "error", err.Error(),
				"detail", "the run's tools are held to the session its box started; a later "+
					"rebuild of its skill guard takes that session for one of its own")
		}
	}()
}

// boxStarted is what a job's bridge session does once the job's box says which
// session it started ([mcpbridge.Session.Started]: on the box's first request,
// before any call of the run is served there): it holds the run's skill guard
// to that session through arm — the runner's ([runner.AgentRunRequest.Started]
// for a launch, [runner.ReopenedRun.Started] for a run taken over), told whether
// the box continued the session its launch chose — and records which session
// the box started on the job's record ([bridgeLedger.started]). continuing is
// whether the job's launch continued a session, asked as the box speaks: a
// launch decides it once it has its box, before its run starts.
//
// A JOB WHOSE LAUNCH CONTINUED NO SESSION records nothing: its box was handed
// one token and started a session of its own, as it was told to, and a record
// that says nothing reads as exactly that.
func boxStarted(ledger *bridgeLedger, runID string, continuing func() bool,
	arm func(continued bool)) func(context.Context, bool) {

	return func(ctx context.Context, fellBack bool) {
		continues := continuing()
		if arm != nil {
			arm(continues && !fellBack)
		}
		if !continues {
			return
		}
		start := sandbox.SessionStartContinued
		if fellBack {
			start = sandbox.SessionStartFresh
			log.InfoContext(ctx, "executor_session_fell_back", "run_id", runID,
				"detail", "the job's box could not take up the session its launch continued and "+
					"started one of its own; the run's skill guard is held to that session, which "+
					"has read no skill")
		}
		ledger.started(ctx, runID, start)
	}
}

// appendRetrying makes a call's record a, tried again on an error — see
// [bridgeLedger] — and answers whether ANY attempt recorded it. Asked again, a
// record whose earlier answer was lost after it landed is answered recorded and
// lands nothing twice ([sandbox.PendingStore.AppendBridgeCall]).
//
// RECORDED STICKS: nothing a later attempt answers takes a call back out of the
// log. A record writes the run's log and then, where its calls moved the job's
// spend, the job's record, and an attempt whose first write landed and whose
// second did not is answered recorded beside the second's error. All an
// attempt after it can still do is write that spend — the store finds the
// call listed, writes the log nothing and the record again — so its answer
// says nothing of the call: one that finds the store out of reach wrote
// nothing, and one that finds the job replaced says only that the spend can no
// longer reach this job's record, which ends the retry. Where an attempt
// landed, the error answered is the spend's, as the last attempt that landed
// answered it: nil once one wrote it. Answered by its last attempt alone, a
// call the log held was said to be off it, and its submission's waiter was
// never told the work was in.
//
// A REPLACED JOB IS ANSWERED ONE WAY, WHICHEVER RELAUNCH REPLACED IT: with the
// spend's error of an attempt that landed before, and nil where none did. The
// store answers a relaunch that started a new round, or the run's end, as not
// recorded, and one that carried the round on as recorded beside
// [sandbox.ErrJobReplaced], the call listed in the round's log; either way
// the job's record takes no spend after it. So a call whose first record is
// late answers no error — its spend is no job's, by design, and nothing
// failed — and one whose earlier attempt landed without its spend answers that
// attempt's error in both cases: the write of what it cost failed while the
// job could still take it, which a relaunch after does not undo. Answered nil
// after a carried-on round alone, the same failure went unsaid there and said
// after a new round's.
func (l *bridgeLedger) appendRetrying(ctx context.Context, runID string, a sandbox.BridgeAppend) (bool, error) {
	landed := false
	// spend is what the last attempt that landed answered beside it: why the
	// spend of its call is not on the job's record, or nil once it is.
	var spend error
	for attempt := 1; ; attempt++ {
		recorded, err := l.store.AppendBridgeCall(ctx, runID, a)
		switch {
		case recorded && errors.Is(err, sandbox.ErrJobReplaced), landed && !recorded && err == nil:
			// THE JOB WAS REPLACED — its round carried on, its call in the
			// round's log; or a new round started, or the run ended, since
			// an attempt put it in it: the spend can no longer reach this
			// job's record, and nothing is left to try again.
			return true, spend
		case recorded:
			landed, spend = true, err
		}
		if err != nil && attempt < appendAttempts && sleepCtx(ctx, appendRetryWaits[attempt-1]) {
			continue
		}
		if landed {
			return true, spend
		}
		return false, err
	}
}

// stepRetrying makes a call's begin or withdrawal a, tried again on a store's
// error after each of [appendRetryWaits] while ctx lasts, and answers whether
// the log took it. A BEGIN THE LOG HAS NO ROOM FOR is not tried again
// ([sandbox.ErrCallsInFlight]): room is made by calls finishing, never by
// asking again within the moments a call's answer waits on.
func (l *bridgeLedger) stepRetrying(ctx context.Context, runID string, a sandbox.BridgeAppend) (bool, error) {
	for attempt := 1; ; attempt++ {
		taken, err := l.store.AppendBridgeCall(ctx, runID, a)
		if err == nil || errors.Is(err, sandbox.ErrCallsInFlight) || attempt >= appendAttempts ||
			!sleepCtx(ctx, appendRetryWaits[attempt-1]) {
			return taken, err
		}
	}
}

// namedCall names call for a line that says what became of it: its tool, its
// place among the job's calls ([sandbox.BridgeCall.Seq]) and the opening of
// the identity it was admitted under ([nameDigestPrefix]) — what finds the
// call in the box's own account of its run, and tells two calls of one tool
// apart. A call whose arguments ride sealed has no identity in the clear, and
// one whose record carries no digest — nothing admitted it, or its entry left
// it out for a tool that may ask a person first — says so.
func namedCall(call sandbox.BridgeCall) string {
	identity := "no digest kept"
	switch digest := call.Digest; {
	case call.Withheld():
		identity = "its arguments sealed"
	case digest != "":
		identity = "digest " + digest[:min(len(digest), nameDigestPrefix)]
	}
	return fmt.Sprintf("%s at place %d, %s", call.Name, call.Seq, identity)
}

// logger is where the ledger says what it could not record: [bridgeLedger.log]
// where a case handed it one to read, and the engine's own otherwise.
func (l *bridgeLedger) logger() *slog.Logger {
	if l.log != nil {
		return l.log
	}
	return log
}

// identitiesCalled is the calls a bridged call's own work added to the run's
// call log ([mcpbridge.Call.Absorbed]), as the log tallies them.
func identitiesCalled(called []toolloop.Called) []sandbox.ElidedCall {
	if len(called) == 0 {
		return nil
	}
	out := make([]sandbox.ElidedCall, 0, len(called))
	for _, c := range called {
		out = append(out, sandbox.ElidedCall{Name: c.Tool, Digest: c.Digest, Calls: 1})
	}
	return out
}

// spent is the meter's running total in the shape a run's row carries, and
// nothing where the run has no meter.
func (l *bridgeLedger) spent() sandbox.EngineSpend {
	if l.spend == nil {
		return sandbox.EngineSpend{}
	}
	return engineSpendOf(l.spend.Total())
}

// engineSpendOf is a bridged meter's total in the shape a run's row carries.
func engineSpendOf(b runner.Bridged) sandbox.EngineSpend {
	return sandbox.EngineSpend{Aux: auxTokensOf(b.Aux), Workers: b.Workers,
		WorkerInput: b.WorkerInput, WorkerOutput: b.WorkerOutput}
}

// bridgedOf is [engineSpendOf] read back: the total a run's row holds, as the
// meter of a run taken over while it works starts from it
// ([runner.NewBridgedSpendFrom]). The row keeps no count of auxiliary calls,
// only their tokens, and nothing reads the count off a bridged meter.
func bridgedOf(s sandbox.EngineSpend) runner.Bridged {
	return runner.Bridged{
		Aux: auxspend.Spent{Input: s.Aux.Input, Output: s.Aux.Output,
			CacheRead: s.Aux.CacheRead, CacheWrite: s.Aux.CacheWrite},
		Workers: s.Workers, WorkerInput: s.WorkerInput, WorkerOutput: s.WorkerOutput,
	}
}

// encodeArgs renders a call's arguments as the JSON text the log holds.
//
// An UNENCODABLE argument is not an error worth failing a log append over: the
// call already ran. It records as empty, with the name and outcome intact,
// which is still the fact a reviewer needs.
//
// The encoding itself is [tools.RecordArgs], shared with the phase event's
// own log because the two are one rule; what stays here is the EMPTY
// spelling, and it differs on purpose — a bridged call's entry carries "" so
// the screen can say "(none)", where a phase event carries `{}`.
func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	blob, err := tools.RecordArgs(args)
	if err != nil {
		log.Warn("bridge_ledger_args_unencodable", "error", err)
		return ""
	}
	return blob
}
