package builtin

import (
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/tools"
)

// WarningsKey is the key a tool's answer carries its caveats under, in the
// MODEL'S words ([caveat.Caveat.Agent]): one top-level list of sentences, on
// every tool, written only by [caveated].
//
// ONE KEY AND ONE WRITER, because the act route answers a person with the
// caveats' PERSON words and removes this key from the receipt it hands back
// (ADR-0060): a second spelling, or a list nested under a half of the answer,
// is model advice that would reach a person's screen through the receipt.
const WarningsKey = "warnings"

// caveated is answer as a tool's result with the caveats cs: their model
// phrasing under [WarningsKey] in the output a model reads, and the caveats
// themselves on [tools.Result.Caveats] for the surface that answers a person.
// Both are written here from the same values, so the two can never describe
// different events. With no caveats it is [jsonResult].
//
// AT THE TOP LEVEL, whichever half of a call a caveat came from: a model reads
// one list, and the act route has one key to remove.
func caveated(answer map[string]any, cs []caveat.Caveat) (tools.Result, error) {
	if len(cs) > 0 {
		answer[WarningsKey] = caveat.Agents(cs)
	}
	res, err := jsonResult(answer)
	// A RESULT THAT FAILED TO RENDER carries none: it is a refusal, and
	// a caveat says something about a write that landed.
	if err == nil && !res.Failed && len(cs) > 0 {
		res.Caveats = cs
	}
	return res, err
}
