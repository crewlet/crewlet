package livestate

import "slices"

// WHAT ONE READER MAY SEE OF A SEAT'S LIVE STATE — the split between a seat's
// STATUS and its BODY, declared once (ADR-0033).
//
// # Status is everybody's, the body is the seat's trail
//
// Everybody a node resolves reads the company's overview: which seats exist,
// what each is doing, what it costs, which coding runs are in flight. What a
// seat is SAYING — the prompt it was sent, what the model answered, the tools
// it called and with what, the narration of each round, the message that woke
// it, and what a coding run was asked and asked back — is that seat's trail,
// and reaches only those internal/authz's `audit.seat.read` admits to the
// seat: an admin, and whoever leads the seat. A live call is the trail as it
// is being written, so it is masked exactly as the trail's questions are
// decided, never by a second statement of who may read it.
//
// And a WORK ITEM a seat is on is the tracker's: a reader without the tracker
// is shown that the seat is working, never which item.
//
// # Why one mask, over every shape a call travels in
//
// A live call reaches a reader in two shapes: the projection's own
// [*LiveCall] (a snapshot, the `agent` and `live_call` answers, `GET
// /agents`), and the [leanCall] an `agents` push leaves the heavy fields out
// of ([LiveCall.Without]). Masked in each surface that sends one, the push and
// the answer would be two statements of the body list, and the first field
// added to one and not the other would be a transcript on the overview. So
// every surface goes through [MaskCall], [MaskRow], [Overlay.For] or
// [SandboxEntry.For], and a walk over every field these types put on the wire
// (TestEveryLiveFieldIsClassified) fails the build on one nobody classified.
//
// # The masked call's versions
//
// A call withheld from its reader names NO version of a body field — every
// body member of [CallVersions] is zero, the rounds' kept — and says so
// (`body_withheld`). The dashboard asks for a call whole (`live_call`) when a
// push names a version newer than the copy it holds; a reader who will never
// be sent the body must never be told there is a newer one, or every push
// would send their tab round that loop.
//
// # It never writes through
//
// The projection hands out a clone, and a push shares one clone across every
// audience variant of one broadcast. So every change here is made on a COPY
// of the struct, and a map is replaced rather than edited: one variant's mask
// must never be what another variant receives.

// Reach is what one reader may see of one seat's live state.
type Reach struct {
	// Body is whether the reader may read the seat's trail: an admin, or
	// a principal `audit.seat.read` admits to the seat (its lead).
	Body bool

	// Tracker is whether the reader holds the tracker, which a work item
	// the seat is on belongs to.
	Tracker bool

	// Record is the reader's own record ([iam.RecordOwner]) — the seat a
	// coding run's parked question may be put to, which is read there by
	// the person it is put to whatever their reach to the seat that asked.
	Record string
}

// runningArguments is the key of a running tool call's arguments inside
// [LiveCall.RunningCall]: what the tool was asked to do, which is the body.
const runningArguments = "arguments"

// MaskCall is THE ONE MASK of a live call, whichever shape carries it: a
// [*LiveCall], the projection's own, or the [leanCall] an `agents` push carries
// once [LiveCall.Without] has left out what a tab holds. Nil answers nil, and
// so does a typed nil; any other dynamic type is withheld whole (nil), which
// TestMaskRowHandlesEveryShape keeps unreachable by feeding this both shapes
// the package produces.
//
// With r.Body, the call comes back as it was but for the tracker rule. Without
// it, the call comes back as a [withheldCall]: no body key at all, a running
// call without its arguments, every body version zero, and `body_withheld`.
// Without r.Tracker, either way, it carries no work item.
//
// It never writes through to v — see the package's paragraph on that.
func MaskCall(v any, r Reach) any {
	switch c := v.(type) {
	case nil:
		return nil
	case *LiveCall:
		if c == nil {
			return nil
		}
		return maskWhole(c, r)
	case leanCall:
		if c.LiveCall == nil {
			return nil
		}
		return maskLean(c, r)
	}
	return nil
}

// For is the call as reader r receives it — [MaskCall] over the projection's
// own shape.
func (c *LiveCall) For(r Reach) any { return MaskCall(c, r) }

// maskWhole masks the projection's own shape.
func maskWhole(c *LiveCall, r Reach) any {
	if r.Body && r.Tracker {
		return c
	}
	dup := *c
	if !r.Tracker {
		dup.WorkItem = nil
	}
	if r.Body {
		return &dup
	}
	// THE ROUNDS ARE STATUS and a whole call carries them always, so the
	// withheld one does too.
	return withhold(&dup, &dup.Rounds)
}

// maskLean masks the shape an `agents` push carries. c is a copy already —
// [leanCall] travels by value — so its shadow pointers are kept as they were:
// the push decided which heavy fields to leave out, and a reader with the
// body is sent exactly those.
func maskLean(c leanCall, r Reach) any {
	if r.Body && r.Tracker {
		return c
	}
	dup := *c.LiveCall
	if !r.Tracker {
		dup.WorkItem = nil
	}
	if r.Body {
		c.LiveCall = &dup
		return c
	}
	// THE ROUNDS AS THE PUSH CARRIED THEM: present only when it carried
	// them, since a tab holding them at the version the push names keeps
	// its copy.
	return withhold(&dup, c.Rounds)
}

// withhold builds the call a reader without its body receives from dup, a copy
// of the call it may no longer share with anybody, and the rounds as the input
// carried them.
//
// THE BODY IS CLEARED ON THE COPY TOO, beside the shadows that keep it off
// the wire: the value is handed out as `any`, and a reader that asserted the
// embedded call back out of it must not find the transcript there either.
func withhold(dup *LiveCall, rounds *[]any) withheldCall {
	dup.Trigger = nil
	dup.Prompt, dup.PromptMessages, dup.Response = "", nil, ""
	dup.ToolExecutions, dup.RoundNarration, dup.PartialRound = nil, nil, nil
	if dup.RunningCall != nil {
		running := make(map[string]any, len(dup.RunningCall))
		for k, v := range dup.RunningCall {
			if k != runningArguments {
				running[k] = v
			}
		}
		dup.RunningCall = running
	}
	dup.Versions = CallVersions{Rounds: dup.Versions.Rounds}
	return withheldCall{LiveCall: dup, Rounds: rounds, BodyWithheld: true}
}

// withheldCall is a live call as a reader without its seat's body receives it.
//
// [leanCall]'s trick: each field here SHADOWS the embedded call's field of the
// same JSON name, encoding/json takes the shallower of the two, and a nil
// pointer under omitempty leaves the key out altogether. The body keys are
// LEFT OUT rather than sent empty, because an empty prompt is a value a tab
// would draw — and the call's own body fields keep no omitempty, so only a
// shadow can drop them.
type withheldCall struct {
	*LiveCall
	Trigger        *struct{} `json:"trigger,omitempty"`
	Prompt         *struct{} `json:"prompt,omitempty"`
	PromptMessages *struct{} `json:"prompt_messages,omitempty"`
	Response       *struct{} `json:"response,omitempty"`
	ToolExecutions *struct{} `json:"tool_executions,omitempty"`
	RoundNarration *struct{} `json:"round_narration,omitempty"`
	PartialRound   *struct{} `json:"partial_round,omitempty"`
	// Rounds is carried as the input carried it — see [MaskCall].
	Rounds *[]any `json:"rounds,omitempty"`
	// BodyWithheld says the call was masked, so a reader draws "not shown
	// to you" rather than a call that said nothing.
	BodyWithheld bool `json:"body_withheld"`
}

// For is the overlay as reader r receives it: its call masked by [MaskCall]
// and its turn by the tracker rule.
//
// `any` AND NEVER *Overlay, because [Overlay.LiveCall] is typed *LiveCall and
// cannot hold a withheld call; the value is an [overlayView]. Nil answers nil.
func (o *Overlay) For(r Reach) any {
	if o == nil {
		return nil
	}
	return overlayView{Overlay: *o, LiveCall: MaskCall(o.LiveCall, r),
		Turn: maskTurn(o.Turn, r)}
}

// overlayView is an [Overlay] as one reader receives it: a copy of the
// overlay, with the two fields the mask changes shadowed.
//
// NEITHER SHADOW IS omitempty, because the overlay states both keys ALWAYS: a
// null call or turn is how a client merging the overlay learns the seat has
// none, and an omitted key would leave the one it held.
type overlayView struct {
	Overlay
	LiveCall any       `json:"live_call"`
	Turn     *LiveTurn `json:"turn"`
}

// maskTurn is the turn as reader r receives it: without its work item when
// they do not hold the tracker, as a copy.
func maskTurn(t *LiveTurn, r Reach) *LiveTurn {
	if t == nil || r.Tracker || t.WorkItem == nil {
		return t
	}
	dup := *t
	dup.WorkItem = nil
	return &dup
}

// For is the coding run as reader r receives it.
//
// WHAT THE RUN WAS ASKED (`task`) IS ITS SEAT'S BODY, and so is the question
// it parked on — except to the people the question is put to, who are asked
// it whatever their reach to the seat that asked, and could not answer what
// they were not shown. A reader shown neither is told so (`body_withheld`).
// The work item is the tracker's.
func (e SandboxEntry) For(r Reach) SandboxEntry {
	out := e
	if !r.Tracker {
		out.WorkItem = nil
	}
	if r.Body {
		return out
	}
	out.Task = ""
	out.BodyWithheld = true
	if r.Record == "" || !slices.Contains(e.AudienceHandles, r.Record) {
		out.Question = ""
	}
	return out
}

// MaskRow masks one `agents` or `seats` row — a roster row with the live
// overlay merged onto it ([LiveState.MergeAgents]), or an overlay row
// ([LiveState.OverlayRows]), its call already a [leanCall] where a push left
// fields out — and returns a NEW map: the call through [MaskCall], the turn
// through the tracker rule, every other key as it was.
func MaskRow(row map[string]any, r Reach) map[string]any {
	if row == nil {
		return nil
	}
	out := make(map[string]any, len(row))
	for k, v := range row {
		out[k] = v
	}
	if call, ok := row["live_call"]; ok {
		out["live_call"] = MaskCall(call, r)
	}
	if turn, ok := row["turn"].(*LiveTurn); ok {
		out["turn"] = maskTurn(turn, r)
	}
	return out
}
