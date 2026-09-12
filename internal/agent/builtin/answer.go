package builtin

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/tools"
)

// What a tool answer may WEIGH, and why the ceiling is stated against its
// reader rather than against the transport.
//
// # A tool answer is read by a model
//
// Every byte here lands in a context window beside the system prompt, the
// conversation, the other tools' answers and whatever the turn has already
// accumulated — so an answer's real cost is measured in the budget it spends,
// not in what a socket can carry. A 200 KiB catalogue is ≈ 50k tokens: most of
// the smallest context the shipped models offer, spent on one call, to answer
// a question the caller could have narrowed.
//
// # The ceiling is a tripwire, not the mechanism
//
// What keeps answers small is that every collection which GROWS is paged or
// capped — a comment thread, an option list, a history feed. This is the guard
// behind them: it fires when a shape nobody bounded is about to spend a turn's
// whole budget, and it names the argument that would narrow it, so the caller's
// next attempt is right rather than another guess.
//
// It REFUSES rather than truncating, because a truncated JSON answer is not an
// answer: a model handed half an object either fails to parse it or, worse,
// reads the half it got as the whole. A refusal naming the remedy is the only
// honest thing to send.
const ToolAnswerBytes = 64 << 10

// jsonAnswer renders a tool's answer, or refuses one too large to send.
//
// The narrowing argument is named by the CALLER, because only the tool knows
// what would make its own answer smaller — `include` on a detail read, a
// filter on a listing, a page size on a catalogue.
func jsonAnswer(v any, narrowWith string) (tools.Result, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return failed("The result could not be rendered."), nil
	}
	if len(data) > ToolAnswerBytes {
		return failed(fmt.Sprintf("That answer is %d KiB and the limit for one "+
			"tool answer is %d KiB — it would spend most of this turn's "+
			"context on a single call. %s",
			len(data)>>10, ToolAnswerBytes>>10, narrowWith)), nil
	}
	return tools.Result{Output: string(data)}, nil
}
