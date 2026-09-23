package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// Where a bridged run's tool calls are kept.
//
// A native tool loop keeps its calls on a surface in memory, and the turn
// writes them when it ends. A BRIDGED run cannot: its calls are made by a
// process outside the engine, minutes or hours apart, and the run can outlive
// the node that started it. So each one is recorded in the fleet's
// coordination store as a record of its own, under the run and its launch —
// see [sandbox.PendingStore.AppendBridgeCall] — and the resume reads every one
// of them back. Without that log a restart mid-run leaves the reviewer judging
// a turn whose entire tool log is gone, which the delivery check reads as a
// turn that acted on nothing.

// resumeBridged reads what an agent-mode run called over the bridge, for the
// phase its resume rebuilds.
//
// EVERY CALL, from the run's own log in the coordination store — the whole
// record, since the process collecting a run may not be the one that launched
// it and its surface has executed nothing. A native resume replays its own
// conversation instead and reads nothing here, so a store it cannot reach does
// not stop it.
//
// A READ THAT FAILS FAILS THE RESUME, which the coordinator hands back for a
// retry. Resuming on an empty log instead would have the delivery check read a
// turn that answered somebody as one that reached nobody, and send it round to
// answer them again.
func (e *Engine) resumeBridged(ctx context.Context, in resumeInput) ([]ledger.Call, error) {
	if !in.State.AgentRun {
		return nil, nil
	}
	if e.sandboxPending == nil {
		return nil, fmt.Errorf("%w: run %s is an agent-mode run and this node holds no run "+
			"store to read its bridged calls from", sandbox.ErrResumeUnavailable, in.Run.TurnID)
	}
	logged, err := e.sandboxPending.BridgeCalls(ctx, in.Run)
	if err != nil {
		return nil, err
	}
	return bridgedCalls(logged), nil
}

// bridgedCalls turns a run's durable bridged-call log into the ledger shape
// the resumed phase reads.
//
// The delivery check, the submission's citations and the iteration ledger all
// read the result, so it keeps every field they read. A call whose arguments
// cannot be decoded — or that the store did not keep, see
// [sandbox.BridgeCall.ArgsBytes] — keeps its name and loses its arguments,
// which renders one ledger line worse; failing the resume over it would lose
// the whole turn.
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
func decodeBridgeArgs(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil
	}
	return args
}

// bridgeLedger records a bridged run's calls in the run's durable log.
type bridgeLedger struct{ store sandbox.PendingStore }

var _ mcpbridge.Ledger = bridgeLedger{}

// Append records one call. See [mcpbridge.Ledger] for why an error here never
// reaches the box.
func (l bridgeLedger) Append(ctx context.Context, runID string, call tools.Call) error {
	if l.store == nil {
		return nil
	}
	_, err := l.store.AppendBridgeCall(ctx, runID, sandbox.BridgeCall{
		Name: call.Name,
		// ENCODED HERE, once. The record is JSON in the coordination
		// store, so a decoded map would be re-encoded by the store's own
		// pass — and a large id survives one round trip through a
		// json.Number-aware decode and not two through the default one.
		Args:   encodeArgs(call.Args),
		Output: call.Output,
		Failed: call.Failed,
	})
	return err
}

// encodeArgs renders a call's arguments as the JSON text the record holds.
//
// An UNENCODABLE argument is not an error worth failing a log append over: the
// call already ran. It records as empty, with the name and outcome intact,
// which is still the fact a reviewer needs.
func encodeArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	blob, err := json.Marshal(args)
	if err != nil {
		log.Warn("bridge_ledger_args_unencodable", "error", err)
		return ""
	}
	return string(blob)
}
