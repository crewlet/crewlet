package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/providers/llm/httpapi"
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
// phase its resume rebuilds: the calls, and the stretch of them no log holds.
//
// EVERY CALL, from the run's own log in the coordination store — the whole
// record, since the process collecting a run may not be the one that launched
// it and its surface has executed nothing — and EACH CALL WHOLE: a call too
// large for one coordination record comes back reassembled from the parts
// filed under it (see [sandbox.PendingStore.BridgeCalls]), so the resumed
// phase's record carries what the box was handed rather than the record's cut
// of it — and that record is what outlives the run, whose end purges the log,
// parts and all, once a resume has returned. A call whose whole was not kept,
// or whose parts do not reassemble, comes back as its record's fitted form and
// says so where it was cut ([sandbox.BridgeLog.Calls]). The one log that can
// be missing calls is a launch an older build recorded, which kept only the
// ends of a long run; that stretch is carried to the resume as a count and a
// place (see [runner.DroppedCalls]) and logged here, because it is missing for
// good. A native resume replays its own conversation instead and reads nothing
// here, so a store it cannot reach does not stop it.
//
// A READ THAT FAILS FAILS THE RESUME, which the coordinator hands back for a
// retry. Resuming on an empty log instead would have the delivery check read a
// turn that answered somebody as one that reached nobody, and send it round to
// answer them again.
func (e *Engine) resumeBridged(ctx context.Context, in resumeInput) ([]ledger.Call, runner.DroppedCalls, error) {
	if !in.State.AgentRun {
		return nil, runner.DroppedCalls{}, nil
	}
	if e.sandboxPending == nil {
		return nil, runner.DroppedCalls{}, fmt.Errorf("%w: run %s is an agent-mode run and this node "+
			"holds no run store to read its bridged calls from", sandbox.ErrResumeUnavailable, in.Run.TurnID)
	}
	logged, err := e.sandboxPending.BridgeCalls(ctx, in.Run)
	if err != nil {
		return nil, runner.DroppedCalls{}, err
	}
	dropped := runner.DroppedCalls{Count: logged.Dropped, After: logged.DroppedAfter}
	if dropped.Count > 0 {
		log.WarnContext(ctx, "sandbox_bridge_calls_not_kept",
			"turn_id", in.Run.TurnID, "launch_id", in.Run.LaunchID,
			"calls_kept", len(logged.Calls), "calls_not_kept", dropped.Count, "not_kept_after", dropped.After,
			"detail", "an older build recorded this run and kept only the ends of its log; the "+
				"resumed phase's record and its review say where calls are missing")
	}
	return bridgedCalls(logged.Calls), dropped, nil
}

// bridgedCalls turns a run's durable bridged-call log into the ledger shape
// the resumed phase reads.
//
// The delivery check, the submission's citations and the iteration ledger all
// read the result, so it keeps every field they read. A call whose arguments
// cannot be decoded keeps its name and loses its arguments, which renders one
// ledger line worse; failing the resume over it would lose the whole turn. A
// call the log hands back in its record's fitted form — its whole not kept, or
// not readable back — is not that case: its arguments, when they did not fit,
// are a marker that decodes — [sandbox.ArgsNotKept] for a whole that was not
// kept, [sandbox.ArgsUnreadable] for one whose parts did not reassemble — and
// its texts say what became of the rest ([sandbox.BridgeLog.Calls]), so every
// reader of the call shows the cut as a cut.
func bridgedCalls(logged []sandbox.BridgeCall) []ledger.Call {
	out := make([]ledger.Call, 0, len(logged))
	for _, call := range logged {
		out = append(out, ledger.Call{
			Name:   call.Name,
			Args:   decodeBridgeArgs(call.Args, call.Name),
			Result: call.Output,
			Failed: call.Failed,
		})
	}
	return out
}

// decodeBridgeArgs reads the JSON text a bridged call was recorded with.
//
// THROUGH [httpapi.DecodeArgs], the one decode a tool call's arguments take,
// because it keeps an integer exact: the bridge decoded the box's arguments
// with json.Number before recording them, and a plain decode here would turn a
// 19-digit id back into a float64, so the resumed phase's record, its review
// and a replayed submission would name a different id from the one the tool
// was called with. Empty text is a call made with no arguments.
func decodeBridgeArgs(raw, tool string) map[string]any {
	if raw == "" {
		return nil
	}
	return httpapi.DecodeArgs([]byte(raw), tool)
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
	// ENCODED HERE, once. The record is JSON in the coordination store, so a
	// decoded map would be re-encoded by the store's own pass — and a large
	// id survives one round trip through a json.Number-aware decode and not
	// two through the default one.
	args, err := encodeArgs(call.Args)
	if err != nil {
		return fmt.Errorf("record bridged call %q of run %s: %w", call.Name, runID, err)
	}
	_, err = l.store.AppendBridgeCall(ctx, runID, sandbox.BridgeCall{
		Name: call.Name, Args: args, Output: call.Output, Failed: call.Failed,
	})
	return err
}

// encodeArgs renders a call's arguments as the JSON text the record holds, ""
// for a call made with none.
//
// It cannot fail on what the bridge hands it — every value came out of the
// bridge's own JSON decode — and the check stays because the alternative is
// worse than an error: arguments recorded as "" read as a call made with none,
// which is a claim about the call rather than a note that something went
// wrong. Returned, it reaches the bridge's own log line for a failed append.
func encodeArgs(args map[string]any) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	blob, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("the arguments are not JSON: %w", err)
	}
	return string(blob), nil
}
