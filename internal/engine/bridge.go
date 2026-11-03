package engine

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/backoff"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// Where a bridged run's tool calls are kept.
//
// An engine phase's calls sit on a surface in memory, and the turn writes them
// when it ends. An executor's run cannot: its calls are made by its harness,
// a process outside the engine, over the bridge, minutes or hours apart, and
// the run can outlive the node that started it. So each one is appended to the run's own row in
// the coordination store, which is the same row the resume reads — and without
// it a restart mid-run leaves the reviewer judging a turn whose entire tool log
// is gone, which the delivery check reads as a turn that acted on nothing.

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
// AND WITH ITS IDENTITY AND WHAT THE ROW KEPT OF IT: the digest the call was
// admitted under, which a resumed segment seeds its call log by — or that its
// arguments are withheld, a call held for a person's approval and the one they
// approved, which it seeds as a call of its tool whose arguments are not known
// — and the size of the whole arguments and output where the row kept less
// ([sandbox.BridgeCall.ArgsBytes]), which the collection's replay decides by.
// The arguments are the row's, a left-out value's marker included: a reader
// that shows them shows a left-out payload as left out.
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
				// row keeps neither of a held call or an approved one.
				Withheld: call.Withheld(),
			},
			Seq:         call.Seq,
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
		})
	}
	return out
}

// identitiesOf is a tally a run's row keeps of calls it does not list, by
// identity — the round's calls it dropped ([sandbox.PendingRun.Elided]), or
// the ones its call log absorbed from a bridged call's own work
// ([sandbox.PendingRun.Absorbed]) — in the shape the turn's record carries it
// ([ledger.Elided]). Handed the row's tally as the round's, through those two
// readers, never off the fields.
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
// decode of the id the row went out of its way to keep exact: a bridged call
// naming a 19-digit issue id would otherwise reach the resumed turn's ledger
// as a rounded one.
func decodeBridgeArgs(raw string) map[string]any {
	return tools.ReadArgs(raw)
}

// bridgeLedger appends a bridged run's calls to its pending-run row, and with
// each one what the run's bridged calls have cost the engine so far.
//
// AN APPEND THAT DOES NOT LAND IS TRIED AGAIN, and then OWED. The call has run
// and its effect has happened, so it is never failed back to the box; but a
// call missing from the row is one a resumed segment's call log never counts,
// and a later call that asks for something else derives that call's operation
// id. So the append is tried [appendAttempts] times before the call's answer
// goes back — the box cannot end its run on a call it has no answer to, so a
// store's blip never costs the record its last calls — and a call that still
// did not land is owed WHOLE, as the append it was
// ([sandbox.BridgeAppend.Owed]): the session's next append carries it, and the
// row lists it as the call it is, once, however many appends carry it — the
// session cannot tell an append that did not land from one whose every answer
// was lost after it did, and the row can.
//
// AND WHAT IS STILL OWED WHEN THE JOB IS OVER is carried by an append that
// names no call of its own ([bridgeLedger.Settle]): the one the run's
// collection asks for before it reads the job's record, so the record its
// resume reads holds the job's last calls — a submission whose own append met
// a store's blip, above all — and the one the session's end makes, the last
// chance anything has to carry them. A call that none of those carried is on
// no record, and the log says so for each: see [bridgeLedger.Settle].
//
// ONE PER SESSION, so one per job, and BOUND TO THAT JOB BEFORE ITS BOX EXISTS
// ([bridgeLedger.bind], handed to [sandbox.LaunchRequest.Opened]): every append
// names the job, so a call that outlives its job never lands on the next one's
// record (see [sandbox.PendingStore.AppendBridgeCall]). It used to learn the job
// from the row on its first append, and a session whose FIRST call outlived its
// job — a delegated worker still running when the reviewer relaunched — learned
// the next job's name and filed its call and its spend there.
type bridgeLedger struct {
	store sandbox.PendingStore

	// spend is the run's bridged meter, whose running total rides every
	// append — see [runner.BridgedSpend]. Nil records the calls alone.
	spend bridgedMeter

	// nudge tells whichever waiter drives the run that its work is in
	// ([sandbox.NudgeSubmitted]), once a `submit_work` the run made is on
	// its row. Nil tells nobody, and the run's next poll finds it.
	nudge func(ctx context.Context, n sandbox.Nudge)

	mu sync.Mutex
	// launch is the job this session's calls are recorded under, as the
	// store opened it.
	launch string
	// owed are this session's appends that did not land as far as it
	// knows, whole, in the order they were made: carried by the next append
	// that lands, or by [bridgeLedger.Settle].
	owed []sandbox.OwedAppend
}

// The retry of one bridged call's append ([bridgeLedger]): THREE ATTEMPTS,
// spaced 250 ms and then 500 ms, held to a second at the most between two.
// One compare-and-swap of one row, which a healthy coordination store answers
// in milliseconds; what a retry rides over is a stream's leader stepping down
// mid-write, whose re-election the broker answers within about a second. Every
// second spent here is a second the box waits for its call's answer, and a
// store out for longer is an outage the session's later appends ride over
// instead, carrying the call as owed.
const (
	appendAttempts     = 3
	appendRetryBase    = 250 * time.Millisecond
	appendRetryCeiling = time.Second
)

// settleBudget bounds one carry of what a session still owes
// ([bridgeLedger.Settle]), its retries included: FIVE SECONDS, as the write
// that records a job's session start is given ([sessionStartRecordBudget]),
// and for its reason — the carry is that same compare-and-swap of one row,
// tried [appendAttempts] times about a second apart in all, which a healthy
// store answers in milliseconds and a store that has not answered in five is
// in an outage the rest of the bound would not see the end of. And it is
// waited on: by the collection of the job's run before it reads the job's
// record, and by whatever ends the session — a run's settle, a stop, the
// relaunch that replaces the job, the seat leaving this node — none of which
// should stand for longer on calls the store will not take.
const settleBudget = 5 * time.Second

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

// bind names the job this session serves. Called once, by the launch, the
// moment the store has opened the job and before its box exists.
func (l *bridgeLedger) bind(launch string) {
	l.mu.Lock()
	l.launch = launch
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
	launch := l.launch
	l.mu.Unlock()
	if launch == "" {
		log.WarnContext(ctx, "bridge_ledger_unbound", "run_id", runID, "tool", call.Name,
			"detail", "a bridged call arrived on a session no job was bound to, so it "+
				"is recorded under none; the launch binds a session before its box exists")
		return nil
	}
	// WHAT THE SESSION STILL OWES THE ROW, taken now so no other append of
	// it carries the same calls, and handed back below if this one does not
	// land either.
	l.mu.Lock()
	owed := l.owed
	l.owed = nil
	l.mu.Unlock()
	a := sandbox.BridgeAppend{
		Launch: launch,
		Call: sandbox.BridgeCall{
			Name: call.Name,
			// ENCODED HERE, once. The row is JSON in the coordination
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
			// row keeps any of it: the arguments above go whole, and the
			// store holds them to the call's share of the row, so this is
			// what a later segment's call log counts the call by.
			Digest: call.Digest,
		},
		// WHAT THIS CALL'S OWN WORK ADDED to the run's call log, and the
		// session's earlier appends the row has not heard of, whole.
		Absorbed: identitiesCalled(call.Absorbed),
		Owed:     owed,
		Spent:    l.spent(),
	}
	recorded, err := l.appendRetrying(ctx, runID, a)
	if err != nil {
		// OWED, WHOLE, as kept as the row would keep it: the next append of
		// the session carries it, and the row lists it once whether this
		// one landed or not ([sandbox.BridgeAppend.Owed]).
		l.owe(append(owed, sandbox.OwedAppend{Call: sandbox.OwedCall(a.Call), Absorbed: a.Absorbed}))
		log.WarnContext(ctx, "bridge_call_owed", "run_id", runID, "launch_id", launch,
			"tool", call.Name, "owed", l.owing(), "error", err.Error(),
			"detail", "the call ran and its answer goes back to the box, but the run's row did not "+
				"take it; the session's next append carries it to the row, or, once the job is over, "+
				"the carry its collection asks for and the one its session makes as it ends")
		return err
	}
	if !recorded {
		// The ordinary end of a job: its row is gone, or holds the next one.
		log.DebugContext(ctx, "bridge_call_after_its_job", "run_id", runID,
			"launch_id", launch, "tool", call.Name)
		return nil
	}
	// THE RUN'S WORK IS IN, or its question is asked, and its harness is about
	// to end its turn and exit: the waiter that drives it polls it now
	// rather than at its next pass ([sandbox.Waiter.Nudge]). After the call
	// is on the row, so the poll that finds the run done collects a log
	// that holds its submission, and a row that holds its question (the
	// ask was recorded before its call returned), as does a call held for a
	// person's approval, recorded as the job's question before its call
	// returned. A call the gate refused is no end (the run goes on) and
	// tells nobody.
	if l.nudge != nil && ((sandbox.EndsRun(call.Name) && !call.Failed) || call.Held) {
		l.nudge(ctx, sandbox.Nudge{TurnID: runID, LaunchID: launch, Why: sandbox.NudgeSubmitted})
	}
	return nil
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
// answers the last attempt. Asked again, an append whose earlier answer was
// lost after it landed is answered recorded and lands nothing twice
// ([sandbox.PendingStore.AppendBridgeCall]).
func (l *bridgeLedger) appendRetrying(ctx context.Context, runID string, a sandbox.BridgeAppend) (bool, error) {
	for attempt := 1; ; attempt++ {
		recorded, err := l.store.AppendBridgeCall(ctx, runID, a)
		if err == nil || attempt >= appendAttempts || ctx.Err() != nil {
			return recorded, err
		}
		timer := time.NewTimer(backoff.Doubling(attempt, appendRetryBase, appendRetryCeiling))
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return recorded, err
		}
	}
}

// owe hands appends back to what the session owes, AHEAD of whatever it came
// to owe meanwhile, so the row hears of its calls in the order they were made.
func (l *bridgeLedger) owe(appends []sandbox.OwedAppend) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.owed = append(slices.Clip(appends), l.owed...)
}

// owing is how many appends the session owes.
func (l *bridgeLedger) owing() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.owed)
}

// Settle carries onto the run's row whatever this session still owes it, in
// one append that names no call of its own, and reports whether that append
// landed. See [mcpbridge.Ledger]: the bridge asks for it as the session ends,
// the last chance anything has to carry the session's calls, and the run's
// collection asks for it through the bridge before it reads the job's record
// ([mcpbridge.Bridge.Settle]), so the record its resume reads holds the job's
// last calls.
//
// UNDER A CONTEXT OF ITS OWN ([settleBudget]), never the caller's
// cancellation: the paths that ask are the ones ending something — a settle,
// a stop, a drain — whose own context is often already done, and a carry
// that inherited it would carry nothing at all.
//
// WHEN IT CANNOT LAND, said by name, for each of the two ways:
//
//   - THE ROW HAS MOVED ON (`bridge_owed_calls_after_their_job`): the store
//     answered, and its row no longer holds the job the calls were made in —
//     a relaunch replaced the job before its owed calls were carried (the
//     relaunch's opening ends this session only once the new job is on the
//     row), or the run ended and its row is gone. No record of the run will
//     hear of them, and the session forgets them.
//   - THE STORE DID NOT TAKE THE CARRY (`bridge_owed_calls_unrecorded`):
//     every attempt within the bound failed, the store out of reach from the
//     calls through this carry. They stay owed, for the session's next append
//     or its next carry; a session that is ending has none, and they are on
//     no record.
func (l *bridgeLedger) Settle(ctx context.Context, runID string) bool {
	if l == nil || l.store == nil {
		return false
	}
	l.mu.Lock()
	launch, owed := l.launch, l.owed
	l.owed = nil
	l.mu.Unlock()
	if launch == "" || len(owed) == 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleBudget)
	defer cancel()
	recorded, err := l.appendRetrying(ctx, runID, sandbox.BridgeAppend{Launch: launch, Owed: owed,
		Spent: l.spent()})
	switch {
	case err != nil:
		l.owe(owed)
		log.WarnContext(ctx, "bridge_owed_calls_unrecorded", "run_id", runID, "launch_id", launch,
			"calls", len(owed), "tools", owedTools(owed), "error", err.Error(),
			"detail", "the calls ran and their answers went back to the box, but the run's row "+
				"took neither their own appends nor this carry of them within its bound; they stay "+
				"owed to the session's next append, and a session that is ending leaves them off "+
				"every record of the run")
		return false
	case !recorded:
		log.WarnContext(ctx, "bridge_owed_calls_after_their_job", "run_id", runID,
			"launch_id", launch, "calls", len(owed), "tools", owedTools(owed),
			"detail", "the calls ran, but by the time the store took their carry the run's row no "+
				"longer held the job they were made in: a relaunch replaced it, or the run ended; "+
				"they are on no record of the run")
		return false
	}
	log.InfoContext(ctx, "bridge_owed_calls_carried", "run_id", runID, "launch_id", launch,
		"calls", len(owed))
	return true
}

// owedTools names the tools of owed appends, for a line that says which calls
// no record holds.
func owedTools(owed []sandbox.OwedAppend) []string {
	out := make([]string, 0, len(owed))
	for _, o := range owed {
		out = append(out, o.Call.Name)
	}
	return out
}

// identitiesCalled is the calls a bridged call's own work added to the run's
// call log ([mcpbridge.Call.Absorbed]), as the row tallies them.
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

// encodeArgs renders a call's arguments as the JSON text the row holds.
//
// An UNENCODABLE argument is not an error worth failing a log append over: the
// call already ran. It records as empty, with the name and outcome intact,
// which is still the fact a reviewer needs.
//
// The encoding itself is [tools.RecordArgs], shared with the phase event's
// own log because the two are one rule; what stays here is the EMPTY
// spelling, and it differs on purpose — a bridged row carries "" so the screen
// can say "(none)", where a phase event carries `{}`.
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
