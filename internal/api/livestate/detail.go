package livestate

import "reflect"

// A live call's HEAVY fields, and the versions that let an `agents` push leave
// them out.
//
// # What a push used to carry, and why
//
// A running phase moves its seat's row on every progress frame — twice a
// round, once a tool call, and five times a second while a round streams —
// and every push carried the whole row: the prompt the opening frame brought,
// carried forward for the length of the phase; every committed round's
// narration; every tool call's arguments and the tail of its result; every
// round's timing. Measured over one long executor phase (thirty rounds, 1 311
// frames — `internal/api/stream`'s weight test): 185 MiB to every open tab,
// 144 KiB a push on average and 245 KiB at its largest, almost all of it text
// the tab already held. Only the round being written and the call in flight
// move at five a second; the rest moves once a round or once a call.
//
// # Why versions, and not deltas
//
// The socket hub drops a client's OLDEST envelope when the client falls behind
// (`stream.QueueDepth`), and that is why the frames were whole: a push that
// carried only what changed, dropped, would leave the tab wrong with nothing
// to say so. A VERSION survives the drop. Every push names, for each heavy
// field, the version of the copy it describes, and carries the field itself
// only when that version moved since the last push for the same call. A
// version is never handed out twice ([CallVersions]), so "moved" and "newer"
// are the same question for any two copies a tab can compare. A tab
// whose copy is at the version the push names keeps it; one whose copy is
// older — the push that moved it was dropped — knows it is behind from the
// very next push, and asks for the call whole (`live_call`). Snapshots, the
// `agent` and `live_call` answers and `GET /agents` carry every field always.

// CallVersions numbers the copy a live call holds of each of its heavy fields:
// a number this projection NEVER HANDS OUT TWICE, taken from its own sequence
// whenever the field is written — every field of a call of its own when it
// begins, and after that each field a frame changes. So a version only grows,
// for every call under every key, for as long as the projection runs; and it
// is a fact about this node's projection, which is the only one a tab's socket
// reads — a tab that reconnects, to this node or another, is sent a snapshot,
// which replaces every copy it holds.
//
// NEVER TWICE, rather than counted per call, because a call is cleared and
// built again under the SAME key: a suspended Execute phase publishes a
// completion checkpoint, which clears its call, and streams its resumed rounds
// under the same turn, phase and iteration. Counted per call, the rebuilt call
// numbered its fields from one again, and a tab still holding the copy from
// before the suspension — one that missed the push clearing it and the first
// push after — took every lower version for a push overtaken by what it held:
// it kept the old response, narration and tool calls, and asked for nothing,
// until the new count passed the old one, which it might never do.
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
}

// CallDetail is which of a live call's fields each version numbers, by the
// keys of both: the dashboard's merge (`LIVE_CALL_DETAIL`) reads a push by
// exactly this table, and [LiveCall.Without] leaves out exactly these.
var CallDetail = map[string][]string{
	"prompt":     {"prompt", "prompt_messages"},
	"response":   {"response"},
	"narration":  {"round_narration"},
	"executions": {"tool_executions"},
	"rounds":     {"rounds"},
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
			Executions: version, Rounds: version}
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
		Response:   moved(was.Response, prev.Response == next.Response),
		Narration:  moved(was.Narration, sameList(prev.RoundNarration, next.RoundNarration)),
		Executions: moved(was.Executions, sameList(prev.ToolExecutions, next.ToolExecutions)),
		Rounds:     moved(was.Rounds, sameList(prev.Rounds, next.Rounds)),
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
		out.ToolExecutions = &c.ToolExecutions
	}
	if c.Versions.Rounds != held.Rounds {
		out.Rounds = &c.Rounds
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
	RoundNarration *[]any  `json:"round_narration,omitempty"`
	Rounds         *[]any  `json:"rounds,omitempty"`
}
