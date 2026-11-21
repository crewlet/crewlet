package livestate

import "reflect"

// A live call's HEAVY fields (prompt, narration, tool calls, timings) change
// once a round or call while the row moves five times a second; resent whole
// they cost 185 MiB per tab over one phase (`internal/api/stream`'s weight test).
//
// Versions, not deltas: the hub drops a client's OLDEST envelope when it falls
// behind (`stream.QueueDepth`), and a dropped delta leaves the tab silently
// wrong. A push names each heavy field's version and carries the field only
// when it moved. A version is never handed out twice ([CallVersions]), so a
// tab holding an older one knows it is behind and asks for the call whole
// (`live_call`). Snapshots, answers and `GET /agents` always carry every field.

// CallVersions numbers the copy a live call holds of each heavy field: a number
// this projection NEVER HANDS OUT TWICE, taken from its own sequence whenever
// the field is written. So a version only grows, for every call under every
// key. It is per node's projection: a tab that reconnects is sent a snapshot,
// which replaces every copy it holds.
//
// NEVER TWICE, rather than counted per call, because a call is cleared and
// built again under the SAME key: a parked turn's resume can run more than
// once, and each attempt's review opens under the turn, phase and iteration of
// the one before. Counted per call, the rebuilt call restarted at one, and a
// tab still holding the earlier attempt's copy took every lower version for a
// stale push, kept the old response and narration, and asked for nothing.
type CallVersions struct {
	// Prompt numbers `prompt` and `prompt_messages`, which move together:
	// once, when the phase's opening frame lands.
	Prompt int `json:"prompt"`
	// Response numbers `response`, the joined text of the committed
	// rounds — once a round.
	Response int `json:"response"`
	// Narration numbers `round_narration` — once a round.
	Narration int `json:"narration"`
	// Executions numbers `tool_executions` — once a tool call.
	Executions int `json:"executions"`
	// Rounds numbers `rounds`, each round's timing — once a round.
	Rounds int `json:"rounds"`
	// Invocations numbers `invocations`, the harness runs that have ended —
	// once a run. Versioned for rounds' reason, one level up: a phase that
	// is steered or delegates ends a run each time, and a list re-sent at
	// five pushes a second while one round streams is what versions remove.
	Invocations int `json:"invocations"`
}

// CallDetail is which of a live call's fields each version numbers, by the
// keys of both: the dashboard's merge (`LIVE_CALL_DETAIL`) reads a push by
// exactly this table, and [LiveCall.Without] leaves out exactly these.
var CallDetail = map[string][]string{
	"prompt":      {"prompt", "prompt_messages"},
	"response":    {"response"},
	"narration":   {"round_narration"},
	"executions":  {"tool_executions", "calls_elided", "elided_calls"},
	"rounds":      {"rounds", "rounds_elided"},
	"invocations": {"invocations"},
}

// restamp is the versions next holds, given the call it replaces: the same
// where a field did not change, and the projection's next version where it
// did. prev is nil when next is a call of its own, which writes every field —
// an empty one included, since a tab may hold a copy under the same key from
// the call before, and only a newer version tells it that copy is gone.
//
// Called under the projection's lock.
func (s *LiveState) restamp(prev, next *LiveCall) CallVersions {
	s.versions++
	version := s.versions
	if prev == nil {
		return CallVersions{Prompt: version, Response: version, Narration: version,
			Executions: version, Rounds: version, Invocations: version}
	}
	was := prev.Versions
	moved := func(held int, same bool) int {
		if same {
			return held
		}
		return version
	}
	return CallVersions{
		Prompt: moved(was.Prompt, prev.Prompt == next.Prompt &&
			sameList(prev.PromptMessages, next.PromptMessages)),
		Response:  moved(was.Response, prev.Response == next.Response),
		Narration: moved(was.Narration, sameList(prev.RoundNarration, next.RoundNarration)),
		Executions: moved(was.Executions, sameList(prev.ToolExecutions, next.ToolExecutions) &&
			prev.CallsElided == next.CallsElided && sameList(prev.ElidedCalls, next.ElidedCalls)),
		Rounds: moved(was.Rounds, sameList(prev.Rounds, next.Rounds) && prev.RoundsElided == next.RoundsElided),
		Invocations: moved(was.Invocations,
			sameList(prev.Invocations, next.Invocations)),
	}
}

// sameList reports whether two of a call's lists hold the same entries. An
// empty list and an absent one are the same: neither has anything to show.
func sameList(a, b []any) bool {
	if len(a) != len(b) {
		return false
	}
	return len(a) == 0 || reflect.DeepEqual(a, b)
}

// CallKey names one call: one run of a turn, one phase, one iteration — the
// key this whole projection is built on.
type CallKey struct {
	TurnID    string
	Phase     string
	Iteration int
}

// Key is the call's [CallKey].
func (c *LiveCall) Key() CallKey {
	return CallKey{TurnID: c.TurnID, Phase: c.Phase, Iteration: c.Iteration}
}

// Without is the call as an `agents` push carries it to tabs that hold its
// heavy fields at held: whole but for the fields whose version is still the
// one held, which are LEFT OUT — absent, never empty, because an empty list is
// a value a tab would draw.
func (c *LiveCall) Without(held CallVersions) any {
	out := leanCall{LiveCall: c}
	if c.Versions.Prompt != held.Prompt {
		out.Prompt, out.PromptMessages = &c.Prompt, &c.PromptMessages
	}
	if c.Versions.Response != held.Response {
		out.Response = &c.Response
	}
	if c.Versions.Narration != held.Narration {
		out.RoundNarration = &c.RoundNarration
	}
	if c.Versions.Executions != held.Executions {
		out.ToolExecutions, out.CallsElided, out.ElidedCalls = &c.ToolExecutions, &c.CallsElided, &c.ElidedCalls
	}
	if c.Versions.Rounds != held.Rounds {
		out.Rounds, out.RoundsElided = &c.Rounds, &c.RoundsElided
	}
	if c.Versions.Invocations != held.Invocations {
		out.Invocations = &c.Invocations
	}
	return out
}

// leanCall is a [LiveCall] with its heavy fields optional on the wire.
//
// Each field here SHADOWS the embedded call's own under the same JSON name —
// encoding/json takes the shallower of two fields named alike and never looks
// at the deeper one — so a nil pointer leaves the key out altogether, and a
// set one carries the call's value, whatever it is.
type leanCall struct {
	*LiveCall
	Prompt         *string `json:"prompt,omitempty"`
	PromptMessages *[]any  `json:"prompt_messages,omitempty"`
	Response       *string `json:"response,omitempty"`
	ToolExecutions *[]any  `json:"tool_executions,omitempty"`
	CallsElided    *int    `json:"calls_elided,omitempty"`
	ElidedCalls    *[]any  `json:"elided_calls,omitempty"`
	RoundNarration *[]any  `json:"round_narration,omitempty"`
	Rounds         *[]any  `json:"rounds,omitempty"`
	RoundsElided   *int    `json:"rounds_elided,omitempty"`
	Invocations    *[]any  `json:"invocations,omitempty"`
}
