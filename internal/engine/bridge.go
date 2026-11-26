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

// Where a bridged run's tool calls are kept: an executor's calls are made by
// its harness over the bridge, minutes or hours apart, and the run can outlive
// the node that started it, so each is written to the run's own log in the
// coordination store ([sandbox.BridgeLog]): its identity before it runs, its
// outcome once it returns ([bridgeLedger]). The resume reads that log; without
// it a restart mid-run leaves the reviewer a turn with no tool log, which the
// delivery check reads as a turn that acted on nothing.

// bridgedCalls turns a run's durable bridged-call log into the shape the
// resumed phase reads: THE ONLY RECORD A RUN'S RESUME HAS, since the collecting
// process may not be the launcher and its fresh surface has executed nothing.
//
// A call whose arguments do not decode keeps its name, marked
// [ledger.Call.ArgsUnread]: left nil and unmarked it reads as a call made with
// none, and failing the resume would lose the whole turn. Every other field is
// the log's, carried as is for the readers [runner.BridgedCall] names (a zero
// start is "not measured", a left-out value keeps its marker); the round is not
// set here, since the runner states it from the run's record. The
// arguments' text rides unchanged beside them ([runner.BridgedCall.ArgsText]):
// an OpenCode reading keys a call by it, never by the decoded map.
func bridgedCalls(logged []sandbox.BridgeCall) []runner.BridgedCall {
	out := make([]runner.BridgedCall, 0, len(logged))
	for _, call := range logged {
		args, unread := decodeBridgeArgs(call.Args, call.ArgsUnparsed)
		out = append(out, runner.BridgedCall{
			Call: ledger.Call{
				Name:   call.Name,
				Args:   args,
				Result: call.Output,
				Failed: call.Failed,
				Digest: call.Digest,
				// The log keeps neither the arguments nor the digest of a
				// held call or an approved one.
				Withheld:   call.Withheld(),
				ArgsUnread: unread,
				Late:       call.Late,
			},
			Seq:         call.Seq,
			Appended:    call.Appended,
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
			CallID:      call.CallID,
			ArgsText:    call.Args,
		})
	}
	return out
}

// unfinishedCalls is a run's calls still in flight as its log was read
// ([sandbox.BridgeLog.InFlight]: running, its process gone, or its update
// refused), each as its identity alone (tool, place, start, CallID, and the
// digest, none where its tool may ask a person first), MARKED
// [ledger.Call.Unfinished]: failed, so no reader takes it for a delivery, a
// submission or a load, and answered [runner.UnfinishedNote], which says its
// effects may stand; a plain failure is one a run would make again. CallID
// rides so the runner asks a cut-off kept session of no id a call in flight
// also carries (runner.BridgedCall.CallID).
func unfinishedCalls(entries []sandbox.BridgeCall) []runner.BridgedCall {
	if len(entries) == 0 {
		return nil
	}
	out := make([]runner.BridgedCall, 0, len(entries))
	for _, entry := range entries {
		out = append(out, runner.BridgedCall{
			Call: ledger.Call{Name: entry.Name, Result: runner.UnfinishedNote, Failed: true,
				Digest: entry.Digest, Unfinished: true},
			Seq: entry.Seq, StartedAt: entry.StartedAt, CallID: entry.CallID,
		})
	}
	return out
}

// identitiesOf is the tally a run's log keeps of calls it does not list, the
// round's dropped ones ([sandbox.BridgeLog.Unlisted]) or those absorbed from a
// bridged call's own work ([sandbox.BridgeLog.Absorbed]), as [ledger.Elided].
// Hand it the tally through those two readers, never off the fields.
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

// decodeBridgeArgs reads a bridged call's recorded JSON, and whether its
// arguments are unread ([ledger.Call.ArgsUnread]): text that does not decode,
// or arguments the bridge could not parse ([sandbox.BridgeCall.ArgsUnparsed]).
// [tools.ReadArgs], not a bare unmarshal: this is the second decode, and a
// 19-digit issue id would otherwise reach the resumed ledger rounded. No text
// and not unparsed is a call made with none, as [encodeArgs] spells it (and
// spells arguments it could not encode, which it warns of; arguments decoded
// from the box's JSON always encode).
func decodeBridgeArgs(raw string, unparsed bool) (args map[string]any, unread bool) {
	args = tools.ReadArgs(raw)
	return args, unparsed || raw != "" && args == nil
}

// bridgeLedger writes a bridged run's calls on the log beside its pending-run
// row, each finished one with the run's bridged spend so far, which the row keeps.
//
// EVERY CALL IS ON THE LOG BEFORE IT RUNS ([bridgeLedger.Begin]), then gets ONE
// UPDATE as it returns ([bridgeLedger.Record]), so a resumed segment counts
// every call, one still running elsewhere included, and no later call derives
// an earlier one's operation id. No write carries another's.
//
// ONE PER SESSION, BOUND TO ITS JOB AND ROUND BEFORE THE BOX EXISTS
// ([bridgeLedger.bind]): every append names both, so a call outliving its job
// is listed late in its round, or in the next round where a new one began, its
// spend nobody's, never filed as the next job's ([sandbox.PendingStore.AppendBridgeCall]).
type bridgeLedger struct {
	store sandbox.PendingStore

	// spend is the run's bridged meter, whose running total rides every
	// record ([runner.BridgedSpend]). Nil records the calls alone.
	spend bridgedMeter

	// marks is this node's harness connections not yet on their rows, which
	// a collection here waits for ([connectMarks]); nil holds none.
	marks *connectMarks

	// seat is the handle of the seat the run is on, whose release here waits
	// for its runs' marks ([connectMarks.release]).
	seat string

	// nudge tells the waiter that drives the run that its work is in
	// ([sandbox.NudgeSubmitted]) once a `submit_work` is in its log. Nil
	// leaves it to the run's next poll.
	nudge func(ctx context.Context, n sandbox.Nudge)

	// log is where the ledger says what it could not record; nil is the
	// engine's own ([bridgeLedger.logger]).
	log *slog.Logger

	// runner names who runs the session's calls on each entry
	// ([sandbox.BridgeCall.Runner]): the incarnation of this process's taker
	// lease ([Engine.callRunner]), by which a collection on any node tells
	// whether a call still runs here; "" holds none, and its entries wait out
	// their bound alone. Set by [Engine.openBridge] before the session opens,
	// read-only after.
	runner string

	// clock is this process's incarnation, whose clock dates every entry's
	// bound ([sandbox.BridgeCall.UntilClock]); set with runner, read-only after.
	clock string

	mu sync.Mutex
	// launch is the job this session's calls are recorded under, as the
	// store opened it, and round its round ([sandbox.OpenedJob.Round]).
	launch, round string
}

// appendRetryWaits are the waits between attempts of one write of a bridged
// call's: 250 ms, 500 ms and 1 s, 1.75 s in all. A healthy store answers each
// compare-and-swap in milliseconds; a retry rides over a stream leader
// stepping down, whose successor is elected within about a second
// (nats-server's campaign timeout is 800 ms at most), so the last attempt
// comes after that second however fast the earlier ones failed. Each second
// here is one the box waits for its answer. A longer outage (a leader lost,
// replaced in four seconds and more) refuses a call's entry, so the call is
// not run, or leaves a finished call's update owed, which the bridge asks for
// again after answering the box (internal/api/mcpbridge's owedUpdateBase).
var appendRetryWaits = [...]time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

// appendAttempts is how many times one write of a bridged call's is tried:
// once, and again after each of [appendRetryWaits].
const appendAttempts = len(appendRetryWaits) + 1

// beginBudget bounds writing a call's entry ahead of it ([bridgeLedger.Begin]),
// retries included, and so a withdrawal: FIVE SECONDS, as
// [sessionStartRecordBudget]. It is one compare-and-swap, 11.89 ms at the
// median and 17.04 ms at p99 (internal/coord/kv's BenchmarkABoxRequestOnItsRun,
// begin arm), and a store silent for five is in an outage the call should not
// wait out: the call is not run, and its box may make it again. The box waits
// with nothing written on its response, and a harness gives a first byte a
// minute (Claude Code 2.1.310; internal/api/mcpbridge's answerEvery). No row
// crossing is added, unlike the session start record (sandbox.RowBudget): the
// write rides the harness's own request, which that minute bounds anyway, and
// writing less than the whole log keeps it inside (crewlet/crewlet#1150).
const beginBudget = 5 * time.Second

// nameDigestPrefix is how much of a call's digest a log line naming it carries
// ([namedCall]): EIGHT HEX DIGITS, the four bytes the log's markers name a
// left-out value by (internal/kept), enough to tell one tool's calls apart
// without the line becoming a second copy of the record.
const nameDigestPrefix = 8

var _ mcpbridge.Ledger = (*bridgeLedger)(nil)

// bridgedMeter is a run's bridged meter, as the ledger reads it.
type bridgedMeter interface{ Total() runner.Bridged }

// newBridgeLedger is the ledger of one bridge session. A nil meter records the
// calls alone, and is never stored as an interface holding a nil pointer.
func newBridgeLedger(store sandbox.PendingStore, meter *runner.BridgedSpend) *bridgeLedger {
	l := &bridgeLedger{store: store}
	if meter != nil {
		l.spend = meter
	}
	return l
}

// bind names the job this session serves and its round, as the row names it
// ([sandbox.PendingRun.NamedJob]): once, by the launch as the store opens the
// job ([sandbox.LaunchRequest.Opened]) and before its box exists, or by a node
// that takes the seat over.
func (l *bridgeLedger) bind(job sandbox.OpenedJob) {
	l.mu.Lock()
	l.launch, l.round = job.LaunchID, job.Round
	l.mu.Unlock()
}

// bound is the job and round the session's calls are recorded under, and
// whether it was bound: the launch binds before the box exists, so an unbound
// call is a wiring mistake, logged rather than filed under whatever job the
// row holds.
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
// ([bridgeLedger.runner], [mcpbridge.Begun.Until]), and its harness's CallID,
// which nothing finds the entry by. Retried within [beginBudget] under a
// context of its own: the box's request is not the call's, and a write cut
// with it would refuse a call the store would have taken.
//
// NIL IS A CALL THE BRIDGE MAY RUN. [mcpbridge.ErrNotTaken] is a log closed to
// the job and its round (the run ended, the job replaced, the round over); any
// other error is an entry not written (every attempt refused, or
// [sandbox.ErrCallsInFlight]). Each is logged as `bridge_call_not_recorded`:
// the call was not run, and its box is told so.
func (l *bridgeLedger) Begin(ctx context.Context, runID string, call mcpbridge.Begun) error {
	if l == nil || l.store == nil {
		return nil
	}
	launch, round, ok := l.bound(ctx, runID, call.Name)
	if !ok {
		return errUnbound
	}
	entry := sandbox.BridgeCall{Name: call.Name, Seq: call.Seq, StartedAt: call.StartedAt,
		Digest: call.Digest, CallID: call.CallID, Runner: l.runner, Until: call.Until, UntilClock: l.clock}
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

// Withdraw takes back the entry of a call that was then not run
// ([mcpbridge.Ledger.Withdraw]), within [beginBudget]. A log without the entry
// (its write never landed, or the log moved on) takes nothing: no error.
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

// Record is a finished call's ONE UPDATE ([mcpbridge.Ledger.Record]): its
// entry, found by identity, replaced by the call's listing with its answer,
// what it committed and the session's running spend. See [mcpbridge.Ledger]
// for why an error never reaches the box.
//
// The total is read after the call ran, so it includes its own cost; the store
// keeps the newest of concurrent totals ([sandbox.EngineSpend.Newest]), and a
// failed update's cost rides the next record, so only a run's last call can
// lose it, charging the task short rather than past. An update that did not
// land leaves the entry (read as a call of unknown outcome) and answers
// [mcpbridge.ErrUnrecorded]: the bridge asks again until it lands or the
// call's bound passes, and logs at warning, so this logs at debug.
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
		// The log lists the call as its job's while open for it, and as late
		// once a relaunch carried the round on ([sandbox.BridgeCall.Late]).
		Launch: launch, Round: round,
		Call: sandbox.BridgeCall{
			Name: call.Name,
			// Encoded once, here: the store would re-encode a decoded map,
			// and a large id survives one json.Number-aware round trip but
			// not two default ones.
			Args: encodeArgs(call.Args),
			// Without it, arguments that did not parse read as a call made
			// with none ([sandbox.BridgeCall.ArgsUnparsed]).
			ArgsUnparsed: call.ArgsUnparsed,
			Output:       call.Output,
			Failed:       call.Failed,
			// The bridge's measurement, the executor's record's one account
			// of call order, timing and who answered; with the tool, the
			// identity the update finds the entry by
			// ([sandbox.BridgeCall.StartedAt]).
			Seq:        call.Seq,
			StartedAt:  call.StartedAt,
			DurationMS: call.Duration.Milliseconds(),
			Origin:     call.Origin,
			Server:     call.Server,
			Source:     call.Source,
			Refusal:    string(call.Refusal()),
			Held:       call.Held,
			// The identity as the bridge admitted it: the store trims the
			// arguments to the call's share of the log, so a later
			// segment's call log counts the call by this.
			Digest: call.Digest,
			// The harness's id for the call, as its session names it;
			// nothing finds the call by it.
			CallID: call.CallID,
			// What the collection settles the turn's outcome by
			// ([Engine.describeResume]).
			Committed: changeNames(call.Committed),
			// A call its harness gave up on, or that outlived its opening,
			// is held by no save point ([sandbox.BridgeCall.Abandoned]).
			Abandoned: call.Abandoned,
		},
		// What this call's own work added to the run's call log.
		Absorbed: identitiesCalled(call.Absorbed),
		// The records its writes created or wrote, which the run's router
		// hands a wake about to the run ([Engine.routeSeat]).
		Wrote: call.Wrote,
		Spent: l.spent(),
	}
	recorded, err := l.appendRetrying(ctx, runID, a)
	if err != nil && !recorded {
		l.logger().DebugContext(ctx, "bridge_call_update_refused", "run_id", runID,
			"launch_id", launch, "call", namedCall(a.Call), "attempts", appendAttempts, "error", err.Error())
		return fmt.Errorf("%w: %w", mcpbridge.ErrUnrecorded, err)
	}
	if !recorded {
		// The ordinary end of a run for the call: its log is gone, or the
		// append named no round. A late call never comes here, of a carried-on
		// round or an earlier one: the log lists it, `late` on the runs answer.
		if call.Abandoned {
			// Unless nobody waited for it: neither its harness's session nor
			// any record of the run holds it, though its effect happened
			// (internal/api/mcpbridge, "An opening ends without waiting for
			// its calls").
			l.logger().WarnContext(ctx, "bridge_abandoned_call_after_its_job", "run_id", runID,
				"launch_id", launch, "call", namedCall(a.Call),
				"detail", "a call nobody waited for returned once the run's log was gone: the run "+
					"ended; it ran and its effect happened, and neither the session that made it nor any "+
					"record of the run holds it")
			return nil
		}
		l.logger().DebugContext(ctx, "bridge_call_after_its_job", "run_id", runID,
			"launch_id", launch, "tool", call.Name)
		return nil
	}
	// THE RUN'S WORK IS IN, or its question asked (an ask, or a call held for
	// approval, is on the row before its call returns), and its harness is
	// about to exit: the waiter polls now ([sandbox.Waiter.Nudge]). After the
	// record, so the poll that finds the run done collects its submission. A
	// refused call ends nothing. Told even if the spend write failed (the next
	// record carries it), and for a late call, whose nudge names a job no
	// waiter polls ([sandbox.Nudge]).
	if l.nudge != nil && ((sandbox.EndsRun(call.Name) && !call.Failed) || call.Held) {
		l.nudge(ctx, sandbox.Nudge{TurnID: runID, LaunchID: launch, Why: sandbox.NudgeSubmitted})
	}
	return err
}

// notRecorded logs, at warning, that call was not run because the run's log
// did not take its entry ahead of it ([bridgeLedger.Begin]), and why.
func (l *bridgeLedger) notRecorded(ctx context.Context, runID, launch string, call sandbox.BridgeCall,
	why string) {

	l.logger().WarnContext(ctx, "bridge_call_not_recorded", "run_id", runID, "launch_id", launch,
		"call", namedCall(call), "why", why,
		"detail", "the call was not run: its entry could not be written on the run's log ahead of it, "+
			"and its box was told so")
}

// sessionStartRecordBudget bounds the write recording which session a job's
// box started ([bridgeLedger.recordStart]), and the one recording that its
// harness connected its bridge ([bridgeLedger.recordConnected]), beside the row's crossing time
// (sandbox.RowBudget): FIVE SECONDS, as [steerPublishBudget] and for its
// reason: one compare-and-swap of one row, and a store silent for five is in
// an outage this write waits out no better. A missed record reads as a session
// of the job's own to every later rebuild of the run's guard: the safe
// direction. The crossing is added because a row of megabytes on a slow link
// outlasts the five alone (crewlet/crewlet#1076); off the request's path, it
// holds nothing up.
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
// run's skill guard (a collection, a carried-on relaunch, a takeover) to read.
//
// OFF THE REQUEST'S PATH, uncancellable, as [runInbox.markPulled] writes: that
// request is the run's first, likely its MCP handshake under the harness's
// short connect timeout, and this process's guard already holds the session.
// A record not yet written reads as a session of the job's own: the safe
// direction.
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
	go l.recordStart(context.WithoutCancel(ctx), runID, launch, start)
}

// recordStart is [bridgeLedger.started]'s write, under base, within
// [sessionStartRecordBudget] plus the row's crossing (sandbox.RowBudget); a
// write the store refuses is logged, and reads as a session of the job's own.
func (l *bridgeLedger) recordStart(base context.Context, runID, launch string, start sandbox.SessionStart) {
	ctx, cancel := context.WithTimeout(base, sandbox.RowBudget(l.store, sessionStartRecordBudget))
	defer cancel()
	if _, err := l.store.MarkSessionStart(ctx, runID, launch, start); err != nil {
		log.WarnContext(ctx, "executor_session_start_unrecorded", "run_id", runID,
			"launch_id", launch, "session_start", string(start), "error", err.Error(),
			"detail", "the run's tools are held to the session its box started; a later "+
				"rebuild of its skill guard takes that session for one of its own")
	}
}

// connected records that the job's harness connected its bridge
// ([sandbox.PendingStore.MarkBridgeConnected]), which its collection fails a
// job that ended without (internal/api/mcpbridge, connected.go). OFF THE
// LIST'S PATH, uncancellable, and held on this node until the row says so
// ([connectMarks]): the harness's first request never waits on a row write,
// and no collection reads the run as one that did not connect.
func (l *bridgeLedger) connected(ctx context.Context, runID string) {
	if l == nil || l.store == nil {
		return
	}
	l.mu.Lock()
	launch := l.launch
	l.mu.Unlock()
	if launch == "" {
		return
	}
	l.marks.record(context.WithoutCancel(ctx), runID, l.seat, func(ctx context.Context) error {
		return l.recordConnected(ctx, runID, launch)
	})
}

// recordConnected is one try of [bridgeLedger.connected]'s write, under base,
// within [sessionStartRecordBudget] plus the row's crossing
// (sandbox.RowBudget). A try the store refuses is logged and answered; its
// mark holds what the next try or a collection needs ([connectMarks]).
func (l *bridgeLedger) recordConnected(base context.Context, runID, launch string) error {
	ctx, cancel := context.WithTimeout(base, sandbox.RowBudget(l.store, sessionStartRecordBudget))
	defer cancel()
	if _, err := l.store.MarkBridgeConnected(ctx, runID, launch); err != nil {
		l.logger().WarnContext(ctx, "executor_bridge_connect_unrecorded", "run_id", runID,
			"launch_id", launch, "error", err.Error(),
			"detail", "the run's harness connected its bridge, and this try to record it failed: it is "+
				"tried again, and a collection on this node writes it before it reads the run")
		return err
	}
	return nil
}

// boxStarted is what a job's bridge session does once its box says which
// session it started ([mcpbridge.Session.Started], on its first request,
// before any call is served): holds the run's skill guard to that session
// through arm ([runner.AgentRunRequest.Started], or [runner.ReopenedRun.Started]
// for a run taken over), told whether the box continued its launch's session,
// and records the start ([bridgeLedger.started]). continuing is asked as the
// box speaks, since a launch decides it once it has its box. A job whose launch
// continued no session records nothing: its box started its own, which is what
// a record saying nothing reads as.
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

// appendRetrying makes a call's record a, retried on an error, and answers
// whether ANY attempt recorded it; a retried record that already landed lands
// nothing twice ([sandbox.PendingStore.AppendBridgeCall]).
//
// RECORDED STICKS: a record writes the log, then the job's spend on its row; a
// later attempt can only rewrite that spend, so its answer never takes a logged
// call off the log (and its submission's waiter unnudged). The error answered
// is the last landed attempt's spend error. A REPLACED JOB answers alike
// whichever relaunch replaced it (the run's end: not recorded; a carried-on or
// a new round: recorded with [sandbox.ErrJobReplaced]): nil if no attempt
// landed before, a late call's spend being nobody's, else that attempt's spend
// error, a failure while the job could still take it.
func (l *bridgeLedger) appendRetrying(ctx context.Context, runID string, a sandbox.BridgeAppend) (bool, error) {
	landed := false
	// spend is what the last attempt that landed answered beside it: why the
	// spend of its call is not on the job's record, or nil once it is.
	var spend error
	for attempt := 1; ; attempt++ {
		recorded, err := l.store.AppendBridgeCall(ctx, runID, a)
		switch {
		case recorded && errors.Is(err, sandbox.ErrJobReplaced), landed && !recorded && err == nil:
			// The job was replaced (its round carried on or a new one begun,
			// or the run's end since an attempt landed): the spend can no
			// longer reach this job's row, and nothing is left to retry.
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

// stepRetrying makes a call's begin or withdrawal a, retried on a store's
// error after each of [appendRetryWaits] while ctx lasts, and answers whether
// the log took it. A begin the log has no room for ([sandbox.ErrCallsInFlight])
// is not retried: room comes from calls finishing, not from asking again.
func (l *bridgeLedger) stepRetrying(ctx context.Context, runID string, a sandbox.BridgeAppend) (bool, error) {
	for attempt := 1; ; attempt++ {
		taken, err := l.store.AppendBridgeCall(ctx, runID, a)
		if err == nil || errors.Is(err, sandbox.ErrCallsInFlight) || attempt >= appendAttempts ||
			!sleepCtx(ctx, appendRetryWaits[attempt-1]) {
			return taken, err
		}
	}
}

// namedCall names call for a log line: its tool, its place among the job's
// calls ([sandbox.BridgeCall.Seq]) and its digest's opening ([nameDigestPrefix]),
// which find it in the box's own account and tell one tool's calls apart. A
// sealed call has no identity in the clear, and one with no digest (nothing
// admitted it, or its tool may ask a person first) says so.
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

// logger is [bridgeLedger.log] where a case handed one, else the engine's own.
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

// bridgedOf is [engineSpendOf] read back, the total a taken-over run's meter
// starts from ([runner.NewBridgedSpendFrom]). The row keeps no count of
// auxiliary calls, only their tokens, and nothing reads that count.
func bridgedOf(s sandbox.EngineSpend) runner.Bridged {
	return runner.Bridged{
		Aux: auxspend.Spent{Input: s.Aux.Input, Output: s.Aux.Output,
			CacheRead: s.Aux.CacheRead, CacheWrite: s.Aux.CacheWrite},
		Workers: s.Workers, WorkerInput: s.WorkerInput, WorkerOutput: s.WorkerOutput,
	}
}

// encodeArgs renders a call's arguments as the JSON text the log holds, by
// [tools.RecordArgs], one rule with the phase event's log. An unencodable
// argument records as empty rather than failing the append: the call already
// ran, and its name and outcome are what a reviewer needs. Empty is "" on
// purpose, so the screen says "(none)", where a phase event carries `{}`.
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
