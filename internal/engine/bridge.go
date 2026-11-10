package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// Where a bridged run's tool calls are kept.
//
// An engine phase's calls sit on a surface in memory, and the turn writes them
// when it ends. An executor's run cannot: its calls are made by its harness,
// a process outside the engine, over the bridge, minutes or hours apart, and
// the run can outlive the node that started it. So each one is appended to the
// run's own log in the coordination store, kept beside its row
// ([sandbox.BridgeLog]), which is what the resume reads — and without it a
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
// ([sandbox.BridgeCall.Late]), which no save point holds.
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

// bridgeLedger appends a bridged run's calls to the log beside its pending-run
// row, and with each one what the run's bridged calls have cost the engine so
// far, which the row keeps.
//
// AN APPEND THAT DOES NOT LAND IS TRIED AGAIN, and then OWED. The call has run
// and its effect has happened, so it is never failed back to the box; but a
// call missing from the log is one a resumed segment's call log never counts,
// and a later call that asks for something else derives that call's operation
// id. So the append is tried [appendAttempts] times before the call's answer
// goes back — the box cannot end its run on a call it has no answer to, so a
// store's blip never costs the log its last calls — and a call that still
// did not land is owed WHOLE, as the append it was
// ([sandbox.BridgeAppend.Owed]): the session's next append carries it, and the
// log lists it as the call it is, once, however many appends carry it — the
// session cannot tell an append that did not land from one whose every answer
// was lost after it did, and the log can. An append one attempt landed is
// never owed, whatever the attempts after it answer: they are for its spend
// alone ([bridgeLedger.appendRetrying]).
//
// AND WHAT IS STILL OWED WHEN THE JOB IS OVER is carried by an append that
// names no call of its own: the one the run's collection asks for before its
// resume reads the job's log ([bridgeLedger.Settle]), so the log the resume
// reads holds the job's last calls — a submission whose own append met a
// store's blip, above all — and the one the session's end makes
// ([bridgeLedger.End]), the last carry of the session's own. ONE AT A TIME
// ([bridgeLedger.carrying]): a session that ends while the collection's carry
// is still out ends once that carry is done, and its own carry takes up what
// that one could not land. A call that none of those carried is on no record,
// and the engine's log names it, call by call: a carry the log refused because
// a relaunch had started a new round, or the run had ended
// ([bridgeLedger.afterTheirJob]), whichever append made it, and one the store
// did not take, or a call whose own record failed once the session had ended
// ([bridgeLedger.unrecorded]).
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
	// append — see [runner.BridgedSpend]. Nil records the calls alone.
	spend bridgedMeter

	// nudge tells whichever waiter drives the run that its work is in
	// ([sandbox.NudgeSubmitted]), once a `submit_work` the run made is in
	// its log. Nil tells nobody, and the run's next poll finds it.
	nudge func(ctx context.Context, n sandbox.Nudge)

	// log is where the ledger says what it could not record: the engine's
	// own, unless a case hands it one it reads ([bridgeLedger.logger]).
	log *slog.Logger

	// carrying is held by each carry of what the session owes for the whole of
	// it ([bridgeLedger.carry]), so they are made one at a time — and taken
	// before mu, never under it. For the session's end above all: its carry
	// is the session's last, and a carry the run's collection asked for may
	// still be out as the session ends, holding what the session owes until
	// the store has refused it. An end that did not wait would find nothing
	// owed, return while the collection's carry was out — the paths that end a
	// session go on to read the run's record, or to delete it — and leave what
	// that carry hands back to no carry at all. Waiting, its own carry takes
	// up what the collection's could not land.
	carrying sync.Mutex

	mu sync.Mutex
	// launch is the job this session's calls are recorded under, as the
	// store opened it, and round the round that job works in
	// ([sandbox.OpenedJob.Round]).
	launch, round string
	// owed are this session's appends that did not land as far as it
	// knows, whole, in the order they were made: carried by the next append
	// that lands, or by [bridgeLedger.Settle] or [bridgeLedger.End].
	owed []sandbox.OwedAppend
	// ended is set as the session's end takes what it carries
	// ([bridgeLedger.End]) — once any carry still out has handed back what
	// it could not land: no carry of the session's own comes after it, so
	// what a call still in flight fails to record from then on is said to be
	// on no record unless a later call of the session carries it, never
	// promised a carry. Read as the calls are handed back ([bridgeLedger.owe]),
	// never as it stood before.
	ended bool
}

// appendRetryWaits are the waits between the attempts of one bridged call's
// append ([bridgeLedger]): a QUARTER OF A SECOND, HALF OF ONE AND A WHOLE ONE,
// 1.75 s in all. The append is one compare-and-swap of the run's log, and one
// of its row where the call moved the spend, which a healthy coordination
// store answers in milliseconds; what a retry rides over
// is a stream's leader stepping down mid-write, whose successor the broker
// elects within about a second (nats-server's campaign timeout is 800 ms at
// the most). So the first waits are short, for a blip that clears at once,
// and the last attempt comes once that second has passed, however quickly the
// attempts before it failed: two waits of 250 and 500 ms ended the retry
// before a re-election it was there to outlast. Every second spent here is a
// second the box waits for its call's answer, and a store out for longer — a
// leader lost rather than stepping down, which its followers wait four seconds
// and more to replace — is an outage the session's later appends and its
// carries ride over instead, carrying the call as owed.
var appendRetryWaits = [...]time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// appendAttempts is how many times one bridged call's append is tried before
// the call is owed: once, and again after each of [appendRetryWaits].
const appendAttempts = len(appendRetryWaits) + 1

// settleBudget bounds one carry of what a session still owes
// ([bridgeLedger.Settle], [bridgeLedger.End]) from its ask, its wait for a
// carry still out and its retries included: FIVE SECONDS, as the write that
// records a job's session start is given ([sessionStartRecordBudget]), and for
// its reason — the carry is that same compare-and-swap of the log, tried as a
// call's own append is ([appendRetryWaits]), which a healthy store answers in
// milliseconds and a store that has not answered in five is in an outage the
// rest of the bound would not see the end of. And it is waited on: by the
// collection of the job's run before its resume reads the job's log, and by
// whatever ends the session — a run's settle, a stop, the relaunch that
// replaces the job, the seat leaving this node — none of which should stand
// for longer on calls the store will not take. So a session's end that waits
// for the collection's carry waits within its own bound, not beside it: the
// collection asked first, and its bound ends first.
const settleBudget = 5 * time.Second

// owedDigestPrefix is how much of a call's identity a line naming a call no
// record holds carries ([owedCalls]): EIGHT HEX DIGITS, the four bytes of a
// digest the log's own markers name a left-out value by (internal/kept), which
// tells a session's calls of one tool apart in a log line without making the
// line a second copy of the record.
const owedDigestPrefix = 8

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

// Append records one call. See [mcpbridge.Ledger] for why an error here never
// reaches the box.
//
// THE TOTAL IS READ AFTER THE CALL RAN, so it includes what the call itself
// cost; two calls finishing together each write the total they read, and the
// store keeps the newest of them ([sandbox.EngineSpend.Newest]). A call whose
// append fails has its cost carried by the next one that lands, since the
// total is cumulative — only the last call of a run can lose it, and then the
// task is charged short of what the turn shows rather than past it.
//
// A CALL ON A SESSION NO JOB WAS BOUND TO records nothing: it cannot happen
// through the launch, which binds before the box that makes calls exists, so it
// is a wiring mistake — said in the log rather than filed under whatever job
// the row holds.
func (l *bridgeLedger) Append(ctx context.Context, runID string, call mcpbridge.Call) error {
	if l == nil || l.store == nil {
		return nil
	}
	l.mu.Lock()
	launch, round := l.launch, l.round
	l.mu.Unlock()
	if launch == "" {
		l.logger().WarnContext(ctx, "bridge_ledger_unbound", "run_id", runID, "tool", call.Name,
			"detail", "a bridged call arrived on a session no job was bound to, so it "+
				"is recorded under none; the launch binds a session before its box exists")
		return nil
	}
	// WHAT THE SESSION STILL OWES THE LOG, taken now so no other append of
	// it carries the same calls, and handed back below if this one does not
	// land either.
	l.mu.Lock()
	owed := l.owed
	l.owed = nil
	l.mu.Unlock()
	a := sandbox.BridgeAppend{
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
			// when each ran and who answered it. See
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
		},
		// WHAT THIS CALL'S OWN WORK ADDED to the run's call log, and the
		// session's earlier appends the log has not heard of, whole.
		Absorbed: identitiesCalled(call.Absorbed),
		Owed:     owed,
		Spent:    l.spent(),
	}
	recorded, err := l.appendRetrying(ctx, runID, a)
	if err != nil && !recorded {
		// OWED, WHOLE, as kept as the log would keep it: the next append of
		// the session carries it, and the log lists it once whether this
		// one landed or not ([sandbox.BridgeAppend.Owed]).
		owed = append(owed, sandbox.OwedAppend{Call: sandbox.CarriedCall(a.Call), Absorbed: a.Absorbed})
		if l.owe(owed) {
			// THE SESSION HAS ENDED, its own last carry made: only another
			// of its calls still in flight can carry these now, so they are
			// said to be on no record unless one lands — not promised the
			// carry that line would.
			l.unrecorded(ctx, runID, launch, owed, err, true)
			return err
		}
		l.logger().WarnContext(ctx, "bridge_call_owed", "run_id", runID, "launch_id", launch,
			"tool", call.Name, "owed", l.owing(), "error", err.Error(),
			"detail", "the call ran and its answer goes back to the box, but the run's log did not "+
				"take it; the session's next append carries it to the log, or, once the job is over, "+
				"the carry its collection asks for and the one its session makes as it ends")
		return err
	}
	if !recorded {
		// THE ORDINARY END OF A ROUND for the call itself: its log is gone, or
		// takes a new round's calls. NOT for what it carried: each call the
		// session owed was one the log did not hold, and none of them is on
		// any record now — said call by call, as a carry's would be. A late
		// call of a round carried on never comes here: the log lists it,
		// marked, and the run's record says so where a person reads its
		// calls — `late` on the runs answer.
		if len(owed) > 0 {
			l.afterTheirJob(ctx, runID, launch, owed)
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

// sessionStartRecordBudget bounds the write that records which session a job's
// box said it started ([bridgeLedger.started]): FIVE SECONDS, as a note's pull
// mark is given ([steerPublishBudget]), and for its reason — one compare-and-swap
// of one row, which a healthy coordination store answers in milliseconds, and a
// store that has not answered in five is in an outage this write waits out no
// better. A record that missed it is read as a session of the job's own by
// every later rebuild of the run's guard: the safe direction.
const sessionStartRecordBudget = 5 * time.Second

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

// appendRetrying appends a, tried again on an error — see [bridgeLedger] — and
// answers whether ANY attempt recorded it. Asked again, an append whose earlier
// answer was lost after it landed is answered recorded and lands nothing twice
// ([sandbox.PendingStore.AppendBridgeCall]).
//
// RECORDED STICKS: nothing a later attempt answers takes a call back out of the
// log. An append writes the run's log and then, where its calls moved the
// job's spend, the job's record, and an attempt whose first write landed and
// whose second did not is answered recorded beside the second's error. All an
// attempt after it can still do is write that spend — the store finds the
// calls listed, writes the log nothing and the record again — so its answer
// says nothing of the calls: one that finds the store out of reach wrote
// nothing, and one that finds the job replaced says only that the spend can no
// longer reach this job's record, which ends the retry. Where an attempt
// landed, the error answered is the spend's, as the last attempt that landed
// answered it: nil once one wrote it. Answered by its last attempt alone, a
// call the log held was owed again, said to be on no record of the run, and
// its submission's waiter was never told the work was in.
//
// A REPLACED JOB IS ANSWERED ONE WAY, WHICHEVER RELAUNCH REPLACED IT: with the
// spend's error of an attempt that landed before, and nil where none did. The
// store answers a relaunch that started a new round, or the run's end, as not
// recorded, and one that carried the round on as recorded beside
// [sandbox.ErrJobReplaced], the calls listed in the round's log; either way
// the job's record takes no spend after it. So a call whose first append is
// late answers no error — its spend is no job's, by design, and nothing
// failed — and one whose earlier attempt landed without its spend answers that
// attempt's error in both cases: the write of what it cost failed while the
// job could still take it, which a relaunch after does not undo. Answered nil
// after a carried-on round alone, the same failure went unsaid there and said
// after a new round's.
func (l *bridgeLedger) appendRetrying(ctx context.Context, runID string, a sandbox.BridgeAppend) (bool, error) {
	landed := false
	// spend is what the last attempt that landed answered beside it: why the
	// spend of its calls is not on the job's record, or nil once it is.
	var spend error
	for attempt := 1; ; attempt++ {
		recorded, err := l.store.AppendBridgeCall(ctx, runID, a)
		switch {
		case recorded && errors.Is(err, sandbox.ErrJobReplaced), landed && !recorded && err == nil:
			// THE JOB WAS REPLACED — its round carried on, its calls in the
			// round's log; or a new round started, or the run ended, since
			// an attempt put them in it: the spend can no longer reach this
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

// owe hands appends back to what the session owes, AHEAD of whatever it came
// to owe meanwhile, so the log hears of its calls in the order they were made;
// and reports whether the session has ended ([bridgeLedger.ended]), past which
// nothing of its own carries them but a call still in flight.
func (l *bridgeLedger) owe(appends []sandbox.OwedAppend) (ended bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owed = append(slices.Clip(appends), l.owed...)
	return l.ended
}

// owing is how many appends the session owes.
func (l *bridgeLedger) owing() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.owed)
}

// Settle carries into the run's log whatever this session still owes it, in
// one append that names no call of its own, and reports whether that append
// landed: what the run's collection asks for through the bridge before its
// resume reads the job's log ([mcpbridge.Bridge.Settle]), so the log the
// resume reads holds the job's last calls. See [mcpbridge.Ledger].
//
// UNDER A CONTEXT OF ITS OWN ([settleBudget]), never the caller's
// cancellation: the paths that ask are the ones ending something — a settle,
// a stop, a drain — whose own context is often already done, and a carry
// that inherited it would carry nothing at all.
//
// WHEN IT CANNOT LAND, said call by call ([owedCalls]), for each of the two
// ways:
//
//   - THE LOG HAS MOVED ON (`bridge_owed_calls_after_their_job`): the store
//     answered, and the run's log is open neither for the job the calls were
//     made in nor for its round — a relaunch started a new round before its
//     owed calls were carried (the relaunch's opening ends this session only
//     once the log takes the new job's calls), or the run ended and its log is
//     gone. No record of the run will hear of them, and the session forgets
//     them. A relaunch that CARRIED THE ROUND ON is no such case: the log
//     lists the calls as late calls of the round ([sandbox.BridgeCall.Late]),
//     and the carry lands.
//   - THE STORE DID NOT TAKE THE CARRY (`bridge_owed_calls_unrecorded`):
//     every attempt within the bound failed, the store out of reach from the
//     calls through this carry. They stay owed, for the session's next append
//     or its next carry — the session's end's among them, which waits for this
//     one — or, where the session has ended, for a call of it still in
//     flight, and are otherwise on no record.
func (l *bridgeLedger) Settle(ctx context.Context, runID string) bool {
	return l.carry(ctx, runID, false)
}

// End is [bridgeLedger.Settle] as the session ends — its last carry of its own
// ([mcpbridge.Ledger.End]). It waits first for a carry still out — the run's
// collection's, asked before the session ended — so it carries what that one
// could not land, and returns only once both are done. And it marks the ledger
// ended as it takes what it carries, so a call of the session still in flight
// whose own record fails from then on is said to be on no record unless a
// later call carries it, rather than promised the session's end's carry, which
// has been made.
func (l *bridgeLedger) End(ctx context.Context, runID string) bool {
	return l.carry(ctx, runID, true)
}

// carry is [bridgeLedger.Settle], and with ending [bridgeLedger.End]: ONE AT A
// TIME ([bridgeLedger.carrying]), within [settleBudget] of its ask, its wait
// for the carry before it included.
func (l *bridgeLedger) carry(ctx context.Context, runID string, ending bool) bool {
	if l == nil || l.store == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleBudget)
	defer cancel()
	l.carrying.Lock()
	defer l.carrying.Unlock()
	l.mu.Lock()
	l.ended = l.ended || ending
	launch, round, owed := l.launch, l.round, l.owed
	l.owed = nil
	l.mu.Unlock()
	if launch == "" || len(owed) == 0 {
		return false
	}
	recorded, err := l.appendRetrying(ctx, runID, sandbox.BridgeAppend{Launch: launch, Round: round,
		Owed: owed, Spent: l.spent()})
	switch {
	case err != nil && !recorded:
		// SAID AS THE SESSION STANDS once they are handed back, as an
		// append's own failure is: whether a carry of the session's own is
		// still to come is what the line promises or denies them.
		l.unrecorded(ctx, runID, launch, owed, err, l.owe(owed))
		return false
	case !recorded:
		l.afterTheirJob(ctx, runID, launch, owed)
		return false
	}
	carried := []any{"run_id", runID, "launch_id", launch, "calls", len(owed)}
	if err != nil {
		// THE CALLS ARE IN THE LOG, and what they cost did not reach the
		// row: a later append of the session that lands carries the
		// running total, and without one the task is charged short of the
		// turn, never past it.
		carried = append(carried, "spend_error", err.Error())
	}
	l.logger().InfoContext(ctx, "bridge_owed_calls_carried", carried...)
	return true
}

// afterTheirJob says, call by call, owed appends an append took to a log that
// is open neither for the job they were made in nor for its round — a relaunch
// started a new round, or the run ended and its log is gone — whether a carry
// of the session's took them or a later call of the old job did: no record of
// the run will hear of them, the session forgets them, and this line is all
// that is left of them.
func (l *bridgeLedger) afterTheirJob(ctx context.Context, runID, launch string, owed []sandbox.OwedAppend) {
	l.logger().WarnContext(ctx, "bridge_owed_calls_after_their_job", "run_id", runID,
		"launch_id", launch, "calls", len(owed), "owed", owedCalls(owed),
		"detail", "the calls ran, but by the time the store took their carry the run's log no "+
			"longer took the calls of the round they were made in: a relaunch started a new round, "+
			"or the run ended; they are on no record of the run")
}

// unrecorded says, call by call, owed appends the store did not take within
// the bound, and what is left to carry them: the session's next append or
// carry, or — once the session has ended — only a call of it still in flight,
// and otherwise nothing.
func (l *bridgeLedger) unrecorded(ctx context.Context, runID, launch string, owed []sandbox.OwedAppend,
	err error, ended bool) {

	detail := "the calls ran and their answers went back to the box, but the run's log took neither " +
		"their own appends nor a carry of them within its bound; they stay owed to the session's next " +
		"append or carry"
	if ended {
		detail = "the calls ran and their answers went back to the box, but the run's log took none " +
			"of them, and the session has ended, its own last carry made: only a call of it still in " +
			"flight can carry them now, and unless one lands they are on no record of the run"
	}
	l.logger().WarnContext(ctx, "bridge_owed_calls_unrecorded", "run_id", runID, "launch_id", launch,
		"calls", len(owed), "owed", owedCalls(owed), "session_ended", ended, "error", err.Error(),
		"detail", detail)
}

// owedCalls names each owed append's call for a line that says no record holds
// it: its tool, its place among the job's calls ([sandbox.BridgeCall.Seq]) and
// the opening of the identity it was admitted under ([owedDigestPrefix]) —
// what finds the call in the box's own account of its run, and tells two calls
// of one tool apart. A call whose arguments ride sealed has no identity in the
// clear, and one nothing admitted has none at all; each says so.
func owedCalls(owed []sandbox.OwedAppend) []string {
	out := make([]string, 0, len(owed))
	for _, o := range owed {
		identity := "no identity"
		switch digest := o.Call.Digest; {
		case o.Call.Withheld():
			identity = "its arguments sealed"
		case digest != "":
			identity = "digest " + digest[:min(len(digest), owedDigestPrefix)]
		}
		out = append(out, fmt.Sprintf("%s at place %d, %s", o.Call.Name, o.Call.Seq, identity))
	}
	return out
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
