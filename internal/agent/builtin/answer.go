package builtin

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/tools"
)

// ToolAnswerBytes is the most ONE tool answer may weigh, and the ceiling is
// stated against its reader rather than against the transport.
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
		// A tool failure is a RESULT the model reads: the surface's Go
		// error aborts the whole tool loop (see internal/agent/toolloop),
		// so returning one here would end a turn over a single answer
		// that would not encode, instead of letting the model ask for a
		// narrower one.
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refused(tools.RefusalUnavailable, "The result could not be rendered."), nil
	}
	return boundedAnswer(data, narrowWith), nil
}

// compactAnswer is [jsonAnswer] for an answer that is a PAGE something below
// this package cut by its own encoded bytes — a git node's read by object id,
// at most code.MaxGitAnswerBytes of compact JSON — and every answer of the
// family such pages belong to (the code tools).
//
// COMPACT, BECAUSE THAT IS WHAT THE PAGE WAS MEASURED IN. Indenting grows an
// answer by its structure rather than by its content: two spaces a level and
// a line a field, so a page of short tree entries or diff files gains about
// half again, and a page cut to fit the tool answer at the git node arrives
// here past it and is refused for its indentation. Compact, the page is the
// bytes the git node counted plus what the tool wraps round it, which is the
// bound code.MaxGitAnswerBytes leaves room for.
func compactAnswer(v any, narrowWith string) (tools.Result, error) {
	data, err := json.Marshal(v)
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refused(tools.RefusalUnavailable, "The result could not be rendered."), nil
	}
	return boundedAnswer(data, narrowWith), nil
}

// boundedAnswer is data as the tool's answer, or the refusal of an answer too
// large to send, naming narrowWith — the one ceiling both encodings meet.
func boundedAnswer(data []byte, narrowWith string) tools.Result {
	if len(data) > ToolAnswerBytes {
		return failed(fmt.Sprintf("That answer is %d KiB and the limit for one "+
			"tool answer is %d KiB — it would spend most of this turn's "+
			"context on a single call. %s",
			len(data)>>10, ToolAnswerBytes>>10, narrowWith))
	}
	return tools.Result{Output: string(data)}
}
