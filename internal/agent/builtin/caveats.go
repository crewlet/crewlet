package builtin

import (
	"encoding/json"
	"slices"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/tools"
)

// WarningsKey is the key a tool's answer carries its caveats under, in the
// model's words ([caveat.Caveat.Agent]): one top-level list, written only by
// [caveatedBy]. One key and one writer, because the act route removes this
// key from a person's receipt (ADR-0060): a second spelling, or a nested
// list, would put model advice on a person's screen.
const WarningsKey = "warnings"

// The keys a tool's answer carries a partial outcome under (the item landed,
// part of what was asked did not), in the model's words. Each is written only
// by [partial], beside a caveat in a person's words, and never reaches a
// person ([ModelOnlyKeys]).
const (
	DependenciesFailedKey = "dependencies_failed"
	MoveStoppedKey        = "move_stopped"
	SubtreeStoppedKey     = "subtree_stopped"
	// NoteKey is an artifact datastore write's note on a verdict that is
	// pending or a conflict.
	NoteKey = "note"
)

// ModelOnlyKeys are the keys of a tool's answer that hold model words only:
// the act route and the human write surface take each off a receipt and
// answer the caveats beside them instead (ADR-0060). The `provenance`
// envelope ([provenanceField]) is listed as a backstop: a fenced answer
// reaches a person through its envelope-free person rendering ([ForPerson]),
// but an answer could come to carry the envelope without one.
func ModelOnlyKeys() []string {
	return []string{WarningsKey, DependenciesFailedKey, MoveStoppedKey,
		SubtreeStoppedKey, NoteKey, provenanceField}
}

// ForPerson is a tool's answer as a person's surface may answer it, minus
// every [ModelOnlyKeys] key, or false for an answer that is not a JSON object
// (every first-party write answers one, so it is words to a model). The act
// route and the human write surface both answer through it.
//
// It starts from [tools.Result.PersonReceipt] where the tool wrote one: Output
// fences other principals' words for a model, and a person is answered them
// as stored ([foreignRead.answer]).
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
		// Unreachable (every value was just decoded), but never hand on
		// model words.
		return json.RawMessage("{}"), true
	}
	return out, true
}

// partial writes a partial outcome's model sentence under key in answer and
// returns its caveat, for [caveated]'s partials.
func partial(answer map[string]any, key string, c caveat.Caveat) caveat.Caveat {
	answer[key] = c.Agent()
	return c
}

// caveated is answer as a tool's result with the caveats cs: their model
// phrasing under [WarningsKey] (top level, whichever half of a call a caveat
// came from), and the caveats on [tools.Result.Caveats] for a person's
// surface, written from the same values so the two never disagree. With no
// caveats it is [jsonResult].
//
// partials are outcomes [partial] already wrote under their own key: they
// reach a person's surface beside cs, but not `warnings`, which would tell
// the model twice.
func caveated(answer map[string]any, cs []caveat.Caveat,
	partials ...caveat.Caveat) (tools.Result, error) {

	return caveatedBy(jsonResult, answer, cs, partials...)
}

// caveatedBy is [caveated] for an answer encoded by render rather than
// [jsonResult], such as a receipt echoing somebody's words, which the
// [foreignRead] labelling them encodes ([ownReceipt]). It is the one writer
// either way, so `warnings` and a person's caveats still share their values.
func caveatedBy(render func(any) (tools.Result, error), answer map[string]any,
	cs []caveat.Caveat, partials ...caveat.Caveat) (tools.Result, error) {

	if len(cs) > 0 {
		answer[WarningsKey] = caveat.Agents(cs)
	}
	res, err := render(answer)
	// A failed result is a refusal; caveats describe a write that landed.
	if all := slices.Concat(cs, partials); err == nil && !res.Failed && len(all) > 0 {
		res.Caveats = all
	}
	return res, err
}
