package builtin

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/tools"
)

// ToolAnswerBytes is the most ONE tool answer may weigh, set against its
// reader, a model, rather than the transport: every byte spends the turn's
// context beside the prompt and the conversation, and a 200 KiB answer
// (≈ 50k tokens) is most of the smallest context the shipped models offer.
//
// It is a tripwire, not the mechanism: every collection that GROWS is paged
// or capped, and this fires when a shape nobody bounded is about to spend a
// turn's budget, naming the argument that narrows it. It REFUSES rather than
// truncating, because a model handed half a JSON object fails to parse it or
// reads the half as the whole.
const ToolAnswerBytes = 64 << 10

// jsonAnswer renders a tool's answer, or refuses one too large to send.
//
// The narrowing argument is named by the CALLER, because only the tool knows
// what would make its own answer smaller — `include` on a detail read, a
// filter on a listing, a page size on a catalogue.
func jsonAnswer(v any, narrowWith string) (tools.Result, error) {
	// NOT AN ANSWER THAT CARRIES SOMEBODY'S WORDS: a record declaring a field
	// another principal wrote ([provenance.FieldTag]) is answered through a
	// [foreignRead], which fences and labels it, never bare here.
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
		// A Go error aborts the whole tool loop (internal/agent/toolloop),
		// ending the turn over one answer instead of letting the model ask
		// for a narrower one.
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refusedBy(tools.RefusalUnavailable, waitless(), "The result could not be rendered."), nil
	}
	return boundedAnswer(data, narrowWith), nil
}

// compactAnswer is [jsonAnswer] for a PAGE something below this package cut by
// its own encoded bytes (a Gitlet node's read by object id, at most
// gitlet.MaxGitAnswerBytes of compact JSON) and for every code tool's answer.
//
// COMPACT, BECAUSE THAT IS WHAT THE PAGE WAS MEASURED IN: indenting grows a
// page of short tree entries or diff files by about half, so a page cut to fit
// at the Gitlet node would arrive past the ceiling. Compact, it is the bytes
// the node counted plus the tool's wrapping, which gitlet.MaxGitAnswerBytes
// leaves room for.
func compactAnswer(v any, narrowWith string) (tools.Result, error) {
	// HELD TO THE SAME DECLARATION AS [jsonAnswer].
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
	// PLAIN, AS [encodeAnswer] IS: [fitLabelled] weighed the page as
	// [foreignRead.render] writes it, with no HTML escapes, and json.Marshal
	// would grow each `&`, `<` and `>` to six bytes, past the ceiling.
	data, err := plainjson.Marshal(v, "")
	if err != nil {
		//nolint:nilerr // A tool failure is a RESULT the model reads, not a Go error.
		return refusedBy(tools.RefusalUnavailable, waitless(), "The result could not be rendered."), nil
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
			kib(len(data)), ToolAnswerBytes>>10, narrowWith))
	}
	return tools.Result{Output: string(data)}
}

// kib is n bytes in whole KiB, ROUNDED UP, for a refusal that states a size
// beside the bound it is past: rounded down, a size a byte past the bound
// reads as the bound itself, and says nothing about how much to cut.
func kib(n int) int { return (n + 1<<10 - 1) >> 10 }

// encodeAnswer is a tool answer as its reader is handed it, and the bytes
// [ToolAnswerBytes] weighs: indented JSON with what a person wrote left as
// written ([plainjson.Marshal]; its package doc says why not
// [json.MarshalIndent]), so markup weighs what prose does.
func encodeAnswer(v any) ([]byte, error) { return plainjson.Marshal(v, "  ") }
