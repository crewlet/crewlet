package engine

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/tools"
)

// maxLateItemUnits is the most UTF-16 units the account of one late call may
// take ([lateItemMessage]): what one Claude Code hook answer has left beside a
// person's widest note ([executor.ClaudeRoomBeside]), so the two always share
// one answer and neither reaches Claude Code's move of a long context into a
// file. COMPUTED from the bounds it is read against, never written out.
var maxLateItemUnits = executor.ClaudeRoomBeside(widestNote())

// widestNote is a person's note at its widest as a hook hands it over: from a
// sender at the widest handle a note names ([iam.MaxLogin]), its text at
// [steer.MaxNoteRunes] characters of two UTF-16 units each, in the wider of
// the two renderings a hook's answer uses.
func widestNote() string {
	by, text := strings.Repeat("a", iam.MaxLogin), strings.Repeat("\U0001F680", steer.MaxNoteRunes)
	plain, before := prompts.SteerMessage(by, text), prompts.SteerMessageSentBefore(by, text)
	if executor.UTF16Len(before) > executor.UTF16Len(plain) {
		return before
	}
	return plain
}

// lateItemMessage is item as the inbox hands it to the model of the job it is
// for: its call drawn as the brief's late section draws one ([bridgedCalls],
// [ledger.CallLine]), every value and error past the ledger's budget omitted
// by size and digest, never cut and never rewritten (no model runs on a hook's
// path), each half neutralised under boundary, the session's fence token, in
// the engine's words for a late call ([prompts.LateCallArrived]). Where that
// passes [maxLateItemUnits], the call's arguments are omitted whole, which
// always fits. See the package doc, "A late call is handed to the job working
// as it lands".
func lateItemMessage(ctx context.Context, item sandbox.LateCall, boundary provenance.Boundary) string {
	call := bridgedCalls([]sandbox.BridgeCall{item.Call})[0].Call
	opts := ledger.Format(nil, nil)
	opts.Fitted = ledgerfit.Fit(ctx, nil, ledger.CallPieces([]ledger.Call{call}, opts))
	called, outcome := ledger.CallLine(call, opts)
	outcome = provenance.Neutralise(boundary, outcome)
	submission := call.Name == toolname.SubmitWork && !call.Failed
	msg := prompts.LateCallArrived(outcome, provenance.Neutralise(boundary, called), submission)
	if executor.UTF16Len(msg) <= maxLateItemUnits {
		return msg
	}
	bare := ledger.Call{Name: call.Name, Result: call.Result, Failed: call.Failed}
	named, _ := ledger.CallLine(bare, opts)
	called = strings.TrimSuffix(named, "()") + "(" + compact.Omitted(item.Call.Args) + ")"
	return prompts.LateCallArrived(outcome, provenance.Neutralise(boundary, called), submission)
}

// sessionFence is the token the session serving s fences its tools' answers
// under: its turn's ([tools.Surface.Turn]), so nothing an item quotes can close
// a fence those answers open. A fresh one where s is no seat's surface.
func sessionFence(s mcpbridge.Surface) provenance.Boundary {
	if seat, ok := s.(*tools.Surface); ok && seat != nil {
		return seat.Turn().FenceBoundary()
	}
	return provenance.NewBoundary()
}
