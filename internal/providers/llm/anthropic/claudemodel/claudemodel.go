// Package claudemodel is what each Claude model accepts on a Messages request:
// how it thinks, which effort levels it takes, whether it takes a sampling
// parameter at all, and how many tokens it may write.
//
// It exists because THE REQUEST SHAPE IS A FUNCTION OF THE MODEL. One shape
// built for 2024-era Claude — a temperature on every call, thinking as an
// `enabled` budget — is a 400 on every current model from Opus 4.7 on, and a
// 400 is fatal to the call: the fallback chain does not retry it, so a
// configured fallback never fires on the one failure it was configured for.
// The backend reads the shape from here rather than sending a knob it cannot
// justify for the model in front of it.
//
// Three decisions, each the opposite of the obvious one:
//
//   - LOOKUP IS EXACT, after [Normalize] strips the spellings a deployment
//     wraps an id in (a Bedrock region prefix or ARN, a Vertex `@` snapshot,
//     a `-v1:0`, a `[1m]` context tag, a date, `-latest`). There is no prefix
//     or family fallback: a future `claude-opus-5-7` matched to the closest
//     row would inherit Opus 5's looser rules, and newer models only ever
//     REMOVE what a request may carry. An id the table does not know gets
//     [Modern] instead.
//   - [Modern] IS THE DEFAULT, not the oldest shape. It is the intersection
//     of what the newest models accept — adaptive thinking sent explicitly,
//     no sampling parameter, an output cap every current model allows — so
//     it is right for every model released after this table was written. It
//     is wrong only for a gateway alias that hides an OLDER model, and that
//     is what the entry's `claude_model` override names.
//   - NOT THE MODELS API. GET /v1/models/{id} reports thinking types, effort
//     levels and limits, but it has no leaf for the two things that 400 most
//     often — sampling parameters and a forced tool choice — so a table is
//     needed anyway; a gateway base_url may not serve /v1/models at all; and
//     a network call to decide a request shape would leave `crewlet validate`
//     unable to judge a config offline. The doctor cross-checks this table
//     against that API instead, which is where drift is worth reporting.
//
// The table also says WHICH MODELS BIND THEIR THINKING ([Profile.PrefixBinding]),
// though that shapes no field of a request. Claude Fable 5.1, Opus 5.5 and
// Sonnet 5.5 bind every thinking block to the request that produced it — the
// system prompt, the SET of tools (each one's name, description and schema)
// and every message before it — and refuse a block replayed into a request
// where any of that changed; on an account the vendor enforces (every one
// created on or after 2026-08-31) the refusal is a 400. The engine's tool set
// does change mid-conversation — `activate_tool` adds a definition, and a
// resumed run renders its definitions again from a registry that may have
// moved — so the backend SHEDS the reasoning a change invalidated, oldest
// first, which is the one removal the check accepts. A property of the MODEL
// rather than of the backend, because on a model without the check (Mythos
// 5.1, and everything before these three) the same blocks are still valid and
// shedding them would lose reasoning for nothing.
//
// Standard library only, because the config tier validates an entry against
// this table and must not import a vendor SDK to do it.
package claudemodel

import (
	"regexp"
	"slices"
	"strings"
)

// Thinking is how a model is asked to think.
//
// A NAMED TYPE over a closed set, because the backend maps each value onto a
// different wire shape and an unknown one must be a value it can refuse rather
// than a string that silently matched neither branch.
type Thinking string

const (
	// ThinkingAdaptive is `{type: "adaptive", display: "summarized"}`: the
	// model decides how much to think, and the depth is steered by effort.
	// On every model in this mode, a request with no thinking field either
	// thinks anyway (Fable, Mythos, Opus 5.5, Opus 5, Sonnet 5.5, Sonnet 5)
	// or does not think at all (Opus 4.6–4.8, Sonnet 4.6) — which is why the
	// mode is sent explicitly rather than left to the default.
	ThinkingAdaptive Thinking = "adaptive"

	// ThinkingBudget is `{type: "enabled", budget_tokens: N}`, or no
	// thinking field at all when no budget is configured. The only mode the
	// models before 4.6 have: they refuse `adaptive`.
	ThinkingBudget Thinking = "budget"
)

// Valid reports whether t is a mode the backend can map.
func (t Thinking) Valid() bool {
	switch t {
	case ThinkingAdaptive, ThinkingBudget:
		return true
	default:
		return false
	}
}

// Effort is a value of `output_config.effort`.
type Effort string

// The levels, lowest first. `xhigh` arrived with Opus 4.7.
const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// Efforts is every level, lowest first.
var Efforts = []Effort{EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax}

// Valid reports whether e is a level the API defines.
func (e Effort) Valid() bool {
	switch e {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	default:
		return false
	}
}

// Profile is what one model accepts.
type Profile struct {
	// ID is the model's canonical id — the alias the table files it under
	// when it has several.
	ID string

	// Thinking is how the model is asked to think.
	Thinking Thinking

	// Efforts are the levels `output_config.effort` accepts, lowest first.
	// NIL MEANS THE MODEL TAKES NO EFFORT AT ALL — Sonnet 4.5 and Haiku 4.5
	// answer one with a 400 — which is a different fact from "every level".
	Efforts []Effort

	// Sampling is whether temperature, top_p and top_k are accepted. False
	// is a 400 on any value, Sonnet 5.5 included (it takes only the default,
	// which is the same as sending nothing).
	Sampling bool

	// MaxOutput is the most tokens one response may hold: the ceiling for
	// max_tokens, and on a thinking model the room the thinking and the
	// answer share.
	MaxOutput int

	// PrefixBinding is whether the model runs the vendor's conversation
	// check on the thinking it is handed back: each thinking block is
	// bound to the top-level system prompt, the set of tools and every
	// message before it in the request that produced it, and a block
	// replayed into a request where any of those changed is refused — a
	// 400 on an account the vendor enforces. The backend reads it to shed
	// the reasoning a changed tool set invalidated rather than replay it.
	//
	// False is a model that runs no such check, where every block stays
	// valid however the tools moved and shedding one would only lose the
	// reasoning it carried. Fable 5.1, Opus 5.5 and Sonnet 5.5 run it;
	// Mythos 5.1 does not, and no model before them has it.
	PrefixBinding bool
}

// Modern is the profile of any id the table does not know. Its ID is empty,
// because it names no model.
//
// 64000 is the smallest output cap among the current models (Haiku 4.5), so it
// is allowed on any model released since and on an alias that turns out to be
// Haiku; every other field is what the newest models require.
//
// PrefixBinding is TRUE for the same reason, and because the two ways of being
// wrong cost different things: on a model that binds its thinking, a block
// replayed after the tools changed is a 400 the fallback chain does not retry,
// while on one that does not, shedding it costs the reasoning it carried and
// nothing else. The three newest models all bind theirs.
var Modern = Profile{
	ID:            "",
	Thinking:      ThinkingAdaptive,
	Efforts:       Efforts,
	Sampling:      false,
	MaxOutput:     64000,
	PrefixBinding: true,
}

// The rows. The MaxOutput values marked UNVERIFIED are not in the API
// reference this table was built from (the reference states 128K for Fable,
// for Mythos and for Opus and Sonnet from 4.6 on, and 64K for Haiku 4.5, and
// nothing else);
// they are the vendor's published figures as last known, and `crewlet llm
// doctor` compares an entry's row against the Models API's own max_tokens.
var rows = []struct {
	ids     []string
	profile Profile
}{
	// The current generation: adaptive thinking, every effort level, no
	// sampling parameter — and on the three that run the conversation
	// check, thinking bound to the request that produced it. Mythos 5.1 is
	// the one model of their generation the vendor says does not run it;
	// Fable 5 and Mythos 5 predate the check, which Fable 5.1 added.
	{[]string{"claude-fable-5-1"}, prefixBound()},
	{[]string{"claude-mythos-5-1"}, current()},
	{[]string{"claude-fable-5"}, current()},
	{[]string{"claude-mythos-5"}, current()},
	{[]string{"claude-opus-5-5"}, prefixBound()},
	{[]string{"claude-opus-5"}, current()},
	{[]string{"claude-opus-4-8"}, current()},
	{[]string{"claude-opus-4-7"}, current()},
	{[]string{"claude-sonnet-5-5"}, prefixBound()},
	{[]string{"claude-sonnet-5"}, current()},

	// 4.6: adaptive (the budget form is deprecated there but still works),
	// no `xhigh` — that level arrived with Opus 4.7 — and sampling accepted.
	{[]string{"claude-opus-4-6"}, Profile{Thinking: ThinkingAdaptive,
		Efforts:  []Effort{EffortLow, EffortMedium, EffortHigh, EffortMax},
		Sampling: true, MaxOutput: 128000}},
	{[]string{"claude-sonnet-4-6"}, Profile{Thinking: ThinkingAdaptive,
		Efforts:  []Effort{EffortLow, EffortMedium, EffortHigh, EffortMax},
		Sampling: true, MaxOutput: 128000}},

	// The budget generation: adaptive is refused. Opus 4.5 takes three
	// effort levels; Sonnet 4.5 and Haiku 4.5 take none.
	{[]string{"claude-opus-4-5"}, Profile{Thinking: ThinkingBudget,
		Efforts:  []Effort{EffortLow, EffortMedium, EffortHigh},
		Sampling: true, MaxOutput: 64000}}, // MaxOutput UNVERIFIED.
	{[]string{"claude-sonnet-4-5"}, legacy(64000)}, // MaxOutput UNVERIFIED.
	{[]string{"claude-haiku-4-5"}, legacy(64000)},

	// Older still, all in the budget shape with no effort. Every MaxOutput
	// here is UNVERIFIED.
	{[]string{"claude-opus-4-1"}, legacy(32000)},
	{[]string{"claude-opus-4-0", "claude-opus-4"}, legacy(32000)},
	{[]string{"claude-sonnet-4-0", "claude-sonnet-4"}, legacy(64000)},
	{[]string{"claude-3-7-sonnet"}, legacy(64000)},
}

// current is a current-generation row. Every one of them writes up to 128K
// tokens — the reference states it for Fable, for Mythos and for Opus and
// Sonnet from 4.6 on — so the cap is the generation's rather than a per-row argument.
func current() Profile {
	return Profile{Thinking: ThinkingAdaptive, Efforts: Efforts, MaxOutput: 128000}
}

// prefixBound is a current-generation row that runs the conversation check.
func prefixBound() Profile {
	profile := current()
	profile.PrefixBinding = true
	return profile
}

// legacy is a budget-generation row that takes no effort.
func legacy(maxOutput int) Profile {
	return Profile{Thinking: ThinkingBudget, Sampling: true, MaxOutput: maxOutput}
}

// table is the rows by every id they answer to.
var table = func() map[string]Profile {
	out := map[string]Profile{}
	for _, row := range rows {
		profile := row.profile
		profile.ID = row.ids[0]
		for _, id := range row.ids {
			out[id] = profile
		}
	}
	return out
}()

// Lookup is the profile of model, which may be any spelling [Normalize]
// reads. The bool is false for an id the table does not know, and the profile
// is then [Modern] — never a guess from the closest name.
func Lookup(model string) (Profile, bool) {
	profile, ok := table[Normalize(model)]
	if !ok {
		profile = Modern
	}
	// A copy of the levels, so a caller that edits its profile cannot edit
	// the table every other request is shaped from.
	profile.Efforts = slices.Clone(profile.Efforts)
	return profile, ok
}

// Known reports whether id is one of the table's own ids, exactly — what a
// `claude_model` override must name, since it exists to say which row an
// unreadable alias is.
func Known(id string) bool {
	_, ok := table[id]
	return ok
}

// IDs is every id the table answers to, for an error message listing them.
func IDs() []string {
	out := make([]string, 0, len(table))
	for _, row := range rows {
		out = append(out, row.ids...)
	}
	return out
}

// The suffixes a deployment appends to a model id, each anchored at the end.
var (
	bedrockVersion = regexp.MustCompile(`-v\d+(:\d+)?$`)
	contextTag     = regexp.MustCompile(`\[[^\[\]]*\]$`)
	snapshotDate   = regexp.MustCompile(`-\d{8}$`)
)

// Normalize reduces a model id as a deployment spells it to the id the table
// files it under:
//
//   - `us.anthropic.claude-opus-4-1-20250805-v1:0` (Bedrock, a regional
//     inference profile) and an ARN ending in either form keep what follows
//     the last `/` and then the last `anthropic.`;
//   - `claude-opus-4-1@20250805` (Vertex) loses its `@` snapshot;
//   - `-v1:0` (Bedrock), `[1m]` (a context tag), `-20250805` (a dated
//     snapshot) and `-latest` are stripped from the end, in any order.
//
// Pure, and deliberately no further: it does not lower-case, guess, or match
// a prefix. An id it cannot reduce is looked up as written and, if unknown,
// gets [Modern].
func Normalize(model string) string {
	id := strings.TrimSpace(model)
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}
	if i := strings.LastIndex(id, "anthropic."); i >= 0 {
		id = id[i+len("anthropic."):]
	}
	if i := strings.Index(id, "@"); i >= 0 {
		id = id[:i]
	}
	// To a fixed point, so `-20250929-v1:0[1m]` and `[1m]` before a date
	// read the same: each strip is anchored at the end and removes text, so
	// the loop ends.
	for {
		next := bedrockVersion.ReplaceAllString(id, "")
		next = contextTag.ReplaceAllString(next, "")
		next = snapshotDate.ReplaceAllString(next, "")
		next = strings.TrimSuffix(next, "-latest")
		if next == id {
			return id
		}
		id = next
	}
}
