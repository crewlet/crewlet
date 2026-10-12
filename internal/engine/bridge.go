package engine

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// Where a bridged run's tool calls are kept.
//
// A native tool loop keeps its calls on a surface in memory, and the turn
// writes them when it ends. A BRIDGED run cannot: its calls are made by a
// process outside the engine, minutes or hours apart, and the run can outlive
// the node that started it. So each one is appended to the run's own row in
// the coordination store, which is the same row the resume reads — and without
// it a restart mid-run leaves the reviewer judging a turn whose entire tool log
// is gone, which the delivery check reads as a turn that acted on nothing.

// bridgedCalls turns a run's durable bridged-call log into the ledger shape
// the resumed phase reads.
//
// THE ONLY RECORD AN AGENT-MODE RESUME HAS. The process collecting a run may
// not be the one that launched it, so its tool surface is fresh and has
// executed nothing: the delivery check, the submission's citations and the
// iteration ledger all read this list. A call whose arguments cannot be
// decoded keeps its name and loses its arguments, which renders one ledger
// line worse — failing the resume over it would lose the whole turn.
func bridgedCalls(logged []sandbox.BridgeCall) []ledger.Call {
	out := make([]ledger.Call, 0, len(logged))
	for _, call := range logged {
		out = append(out, ledger.Call{
			Name:   call.Name,
			Args:   decodeBridgeArgs(call.Args),
			Result: call.Output,
			Failed: call.Failed,
		})
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

	mu sync.Mutex
	// launch is the job this session's calls are recorded under, as the
	// store opened it.
	launch string
}

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
func (l *bridgeLedger) Append(ctx context.Context, runID string, call tools.Call) error {
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
	recorded, err := l.store.AppendBridgeCall(ctx, runID, sandbox.BridgeAppend{
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
		},
		Spent: l.spent(),
	})
	if err != nil {
		return err
	}
	if !recorded {
		// The ordinary end of a job: its row is gone, or holds the next one.
		log.DebugContext(ctx, "bridge_call_after_its_job", "run_id", runID,
			"launch_id", launch, "tool", call.Name)
	}
	return nil
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
