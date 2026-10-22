package builtin

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/plainjson"
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
	// NOT AN ANSWER THAT CARRIES SOMEBODY'S WORDS: a record declaring a field
	// another principal wrote ([provenance.FieldTag]) is answered through a
	// [foreignRead], which fences and labels it, and never here, where it
	// would reach the model bare and unattributed.
	switch declared, err := declaredIn(v); {
	case err != nil:
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refused(tools.RefusalInternalError, "The result could not be rendered."), nil
	case len(declared) > 0:
		log.Error("builtin_answer_unlabelled", "field", readablePath(declared[0].path.id()),
			"kind", declared[0].kind)
		return refused(tools.RefusalInternalError, "The result could not be rendered: it "+
			"carries text somebody else wrote that this tool does not label."), nil
	}
	data, err := encodeAnswer(v)
	if err != nil {
		// A tool failure is a RESULT the model reads: the surface's Go
		// error aborts the whole tool loop (see internal/agent/toolloop),
		// so returning one here would end a turn over a single answer
		// that would not encode, instead of letting the model ask for a
		// narrower one.
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refused(tools.RefusalUnavailable, "The result could not be rendered."), nil
	}
	if len(data) > ToolAnswerBytes {
		return failed(fmt.Sprintf("That answer is %d KiB and the limit for one "+
			"tool answer is %d KiB — it would spend most of this turn's "+
			"context on a single call. %s",
			kib(len(data)), ToolAnswerBytes>>10, narrowWith)), nil
	}
	return tools.Result{Output: string(data)}, nil
}

// kib is n bytes in whole KiB, ROUNDED UP, for a refusal that states a size
// beside the bound it is past.
//
// ROUNDED DOWN, a size one byte past a bound read as the bound itself: an
// answer of 64 KiB and a byte was refused as "64 KiB and the limit … is 64
// KiB", which reads as a refusal of something AT its limit and tells the
// reader nothing about how much to cut.
func kib(n int) int { return (n + 1<<10 - 1) >> 10 }

// encodeAnswer is a tool answer as its reader is handed it: indented JSON,
// with what a person wrote left as they wrote it — [plainjson.Marshal], whose
// package doc says why that is not [json.MarshalIndent] — and these are the
// bytes [ToolAnswerBytes] weighs, so a page of markup weighs what the page of
// prose beside it does rather than four times it.
func encodeAnswer(v any) ([]byte, error) { return plainjson.Marshal(v, "  ") }
