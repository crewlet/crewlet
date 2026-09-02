package engine

import (
	"context"
	"encoding/json"

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

// bridgeLedger appends a bridged run's calls to its pending-run row.
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
		// ENCODED HERE, once. The row is JSON in the coordination store,
		// so a decoded map would be re-encoded by the store's own pass —
		// and a large id survives one round trip through a
		// json.Number-aware decode and not two through the default one.
		Args:   encodeArgs(call.Args),
		Output: call.Output,
		Failed: call.Failed,
	})
	return err
}

// encodeArgs renders a call's arguments as the JSON text the row holds.
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
