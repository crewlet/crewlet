package builtin

import (
	"encoding/json"
	"slices"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/tools"
)

// WarningsKey is the key a tool's answer carries its caveats under, in the
// MODEL'S words ([caveat.Caveat.Agent]): one top-level list of sentences, on
// every tool, written only by [caveated] (by way of [caveatedBy], the same
// writer over an encoding its caller chooses).
//
// ONE KEY AND ONE WRITER, because the act route answers a person with the
// caveats' PERSON words and removes this key from the receipt it hands back
// (ADR-0060): a second spelling, or a list nested under a half of the answer,
// is model advice that would reach a person's screen through the receipt.
const WarningsKey = "warnings"

// The keys a tool's answer carries a PARTIAL OUTCOME under, in the model's
// words: the item landed and part of what was asked did not, and the sentence
// says what finishes it with the calls a model makes. Each is written only by
// [partial], beside a caveat in a person's words, and like [WarningsKey] never
// reaches a person ([ModelOnlyKeys]).
const (
	DependenciesFailedKey = "dependencies_failed"
	MoveStoppedKey        = "move_stopped"
	SubtreeStoppedKey     = "subtree_stopped"
	// NoteKey is an artifact datastore write's note on a verdict that is
	// pending or a conflict.
	NoteKey = "note"
)

// ModelOnlyKeys are the keys of a tool's answer that hold its model's words
// and nothing a person's surface may answer: the act route and the human
// write surface take every one of them off a receipt, and answer the caveats
// written beside them instead (ADR-0060). The `provenance` envelope a
// fenced answer carries beside its fields ([provenanceField]) is one of
// them: its trust and its note tell a model how far to believe the words it
// labels. A fenced answer reaches a person from its own rendering for one,
// which carries no envelope ([ForPerson]), so the key is listed here as the
// backstop for an answer that comes to carry one without it.
func ModelOnlyKeys() []string {
	return []string{WarningsKey, DependenciesFailedKey, MoveStoppedKey,
		SubtreeStoppedKey, NoteKey, provenanceField}
}

// ForPerson is a tool's answer as a person's surface may answer it: the
// answer with every [ModelOnlyKeys] key taken off and the rest as the tool
// wrote it, or false for an answer that is not a JSON object (which every
// first-party write answers, so it is the tool's words to a model). The act
// route and the human write surface both answer through it.
//
// THE ANSWER AS THE TOOL WROTE IT FOR A PERSON, where it wrote one
// ([tools.Result.PersonReceipt]): an answer echoing words other principals
// wrote labels them for a model in Output, fenced and defused, and a person
// is answered those words as they were stored rather than the model's framing
// round them ([foreignRead.answer]).
func ForPerson(res tools.Result) (json.RawMessage, bool) {
	output := res.Output
	if res.PersonReceipt != nil {
		output = string(res.PersonReceipt)
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(output), &fields) != nil || fields == nil {
		return nil, false
	}
	carried := false
	for _, key := range ModelOnlyKeys() {
		if _, ok := fields[key]; ok {
			delete(fields, key)
			carried = true
		}
	}
	if !carried {
		return json.RawMessage(output), true
	}
	out, err := json.Marshal(fields)
	if err != nil {
		// Unreachable: every value was just decoded from valid JSON. The
		// answer is never handed on with a model's words left on it.
		return json.RawMessage("{}"), true
	}
	return out, true
}

// partial writes a partial outcome's model sentence under key in answer and
// answers its caveat, for [caveated]'s partials: the model reads the sentence
// where it always has, and a person's surface the caveat's own words.
func partial(answer map[string]any, key string, c caveat.Caveat) caveat.Caveat {
	answer[key] = c.Agent()
	return c
}

// caveated is answer as a tool's result with the caveats cs: their model
// phrasing under [WarningsKey] in the output a model reads, and the caveats
// themselves on [tools.Result.Caveats] for the surface that answers a person.
// Both are written here from the same values, so the two can never describe
// different events. With no caveats it is [jsonResult].
//
// AT THE TOP LEVEL, whichever half of a call a caveat came from: a model reads
// one list, and the act route has one key to remove.
//
// partials are partial outcomes [partial] already wrote under their own key:
// they reach a person's surface beside cs, and not the model's `warnings`,
// which would tell it the same thing twice.
func caveated(answer map[string]any, cs []caveat.Caveat,
	partials ...caveat.Caveat) (tools.Result, error) {

	return caveatedBy(jsonResult, answer, cs, partials...)
}

// caveatedBy is [caveated] for an answer encoded by render rather than by
// [jsonResult]: a write's receipt that echoes words its records declare
// somebody's is encoded by the [foreignRead] that labels them
// ([ownReceipt]), and carries the write's caveats all the same.
//
// THE SAME WRITER, NOT A SECOND ONE: the caveats reach the answer here
// whichever encoding it takes, so the model's `warnings` and the caveats a
// person's surface answers are still written from the same values in one
// place.
func caveatedBy(render func(any) (tools.Result, error), answer map[string]any,
	cs []caveat.Caveat, partials ...caveat.Caveat) (tools.Result, error) {

	if len(cs) > 0 {
		answer[WarningsKey] = caveat.Agents(cs)
	}
	res, err := render(answer)
	// A RESULT THAT FAILED TO RENDER carries none: it is a refusal, and
	// a caveat says something about a write that landed.
	if all := slices.Concat(cs, partials); err == nil && !res.Failed && len(all) > 0 {
		res.Caveats = all
	}
	return res, err
}
