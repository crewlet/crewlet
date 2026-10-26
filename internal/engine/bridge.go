package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// Where a bridged run's tool calls are kept.
//
// An engine phase's calls sit on a surface in memory, and the turn writes them
// when it ends. An executor's run cannot: its calls are made by its coding CLI,
// a process outside the engine, over the bridge, minutes or hours apart, and
// the run can outlive the node that started it. So each one is appended to the run's own row in
// the coordination store, which is the same row the resume reads — and without
// it a restart mid-run leaves the reviewer judging a turn whose entire tool log
// is gone, which the delivery check reads as a turn that acted on nothing.

// bridgedCalls turns a run's durable bridged-call log into the executions the
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
// EXECUTIONS rather than bare ledger calls, because the row carries the
// bridge's measurement of each call — its start, its duration and who
// answered it — and the executor's record places the run's calls on its
// timeline by exactly that. A call the bridge did not measure keeps a zero
// start, which every reader takes for "not measured". The round is the
// runner's to state: the run's record has one, and it is not this layer's.
func bridgedCalls(logged []sandbox.BridgeCall) []toolloop.Execution {
	out := make([]toolloop.Execution, 0, len(logged))
	for _, call := range logged {
		out = append(out, toolloop.Execution{
			Name:      call.Name,
			Args:      decodeBridgeArgs(call.Args),
			Output:    call.Output,
			Failed:    call.Failed,
			StartedAt: call.StartedAt,
			Duration:  time.Duration(call.DurationMS) * time.Millisecond,
			Origin:    call.Origin,
			Server:    call.Server,
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

	// base is what the job's bridged calls had cost before this session's
	// meter began: zero for a launch's session, and the row's total for one
	// rebuilt after a restart ([Engine.reopenBridgeSession]), whose fresh
	// meter starts at nothing. The row keeps the larger of two totals
	// ([sandbox.EngineSpend.Newest]), so a rebuilt meter written alone
	// would be ignored until it overtook what the job had spent before.
	base sandbox.EngineSpend

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
			// THE BRIDGE'S MEASUREMENT, kept for the executor's record:
			// the one account of when each of the run's calls ran and
			// who answered it. See [sandbox.BridgeCall.StartedAt].
			StartedAt:  call.StartedAt,
			DurationMS: int(call.Duration / time.Millisecond),
			Origin:     call.Origin,
			Server:     call.Server,
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
		return l.base
	}
	return l.base.Plus(engineSpendOf(l.spend.Total()))
}

// nodeID is this node's id: "" for an engine built without a node, which no
// running node is.
func (e *Engine) nodeID() string {
	if e.node == nil {
		return ""
	}
	return e.node.ID()
}

// reopenBridgeSession rebuilds the bridge session runID's box holds a token
// for, on this node, after this process lost it — a restart — while the run
// goes on ([mcpbridge.Bridge.SetReopener]).
//
// REBUILT ONLY FOR THE JOB THE TOKEN NAMES, ON THE NODE THAT LAUNCHED IT,
// WHILE IT RUNS:
//
//   - the row's session nonce is the token's, or a relaunch has since
//     replaced the session and the token is an earlier box's;
//   - this node launched it, because the box's URL addresses the launching
//     node and no other — a call reaching another one is a misrouted URL,
//     and serving it there would split one run's calls across two
//     surfaces counting ordinals apart;
//   - the job is running, with its park on the row: a job that ended has no
//     tools to call, and one whose turn never parked was lost with the
//     process before it suspended — the recovery that fails it answers it.
//
// THE SURFACE IS THE PARKED TURN'S, built as the collection builds it
// ([Engine.parkedRunner], [runner.Runner.BridgeSurface]), in the epoch live
// now — the same choice a resume makes, for the same reason.
func (e *Engine) reopenBridgeSession(ctx context.Context, runID, nonce string) (*mcpbridge.Session, error) {
	rt := e.sandbox.Load()
	if rt == nil {
		return nil, errors.New("this node has no sandbox runtime")
	}
	row, found, err := rt.pending.Get(ctx, runID)
	switch {
	case err != nil:
		return nil, fmt.Errorf("reading the run: %w", err)
	case !found:
		return nil, errors.New("the run has ended")
	case row.BridgeNonce == "" || row.BridgeNonce != nonce:
		return nil, errors.New("the token names a session a later launch of the turn replaced")
	case row.LaunchNode != e.nodeID():
		return nil, fmt.Errorf("node %q launched the run, and its box's URL addresses "+
			"that node", row.LaunchNode)
	case row.Status != sandbox.StatusRunning:
		return nil, fmt.Errorf("the run is %s, and only a running job's box calls its tools",
			row.Status)
	}
	state, parkedOK, err := execstate.Decode(row.ExecuteState)
	if err != nil {
		return nil, fmt.Errorf("the run's park cannot be read: %w", err)
	}
	if !parkedOK {
		return nil, errors.New("the run's row holds no park: its turn has not parked on it yet")
	}
	company := e.Company()
	if company == nil {
		return nil, errors.New("this node has no applied company")
	}
	seat := company.Org.AgentSeatByHandle(row.AgentHandle)
	if seat == nil {
		return nil, fmt.Errorf("seat %q is not in this node's company", row.AgentHandle)
	}
	built, err := e.parkedRunner(ctx, company, resumeInput{
		Company: company, Run: row, State: state,
		Turn: resumedTurn(row, seat, company.Org),
	}, nil)
	if err != nil {
		return nil, err
	}
	surface, meter, err := built.runner.BridgeSurface(ctx)
	if err != nil {
		return nil, err
	}
	ledger := newBridgeLedger(rt.pending, meter)
	ledger.base = row.Launch.Bridged
	ledger.bind(row.LaunchID)
	return &mcpbridge.Session{
		RunID: runID, Handle: row.AgentHandle, Role: seat.Name,
		Surface: surface, Ledger: ledger,
		OnHost: sandbox.Placement(row.Placement) == sandbox.Direct,
	}, nil
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
