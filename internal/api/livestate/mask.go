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
// And a WORK ITEM a seat is on is the tracker's: a reader without Worklet
// is shown that the seat is working, never which item.
//
// # The company's configuration is an admin's
//
// Which `providers.llm` entries a company configures, and so which of them
// served a run or failed it, is the company's configuration: the org
// projection hands only an admin each seat's chain, and Settings, Models is
// an admin's screen. Three live fields name entries: a CLI run's
// `provider_key` ([LiveCall.Invocations]), the entry a coding run was
// launched on ([SandboxEntry.ProviderKey]), which a fallback at launch makes
// other than the chain's head, and a failure's own text ([ErrorInfo.Detail]),
// which also carries whatever a vendor or a tool wrote. All three reach only
// a reader with [Reach.Config], whatever their reach to the seat's body: a
// seat's lead reads its trail and is still not an admin. Everybody else is
// told a failure by its kind, and a coding run by the model it runs under.
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

	// Tracker is whether the reader holds Worklet, which a work item
	// the seat is on belongs to.
	Tracker bool

	// Config is whether the reader may read the company's configuration
	// (an admin): which entries served a run or launched a coding run, and
	// a failure's own text.
	// It gates the LIVE BROADCAST only. A seat's history read
	// (maskHistory in internal/api/queries) is held to `audit.seat.read`,
	// which a lead holds over that seat's trail, so its raw errors are
	// within the lead's own grant there.
	Config bool

	// Record is the reader's own record ([iam.RecordOwner]) — the seat a
	// coding run's parked question may be put to, which is read there by
	// the person it is put to whatever their reach to the seat that asked.
	Record string
}

// runningArguments is the key of a running tool call's arguments inside each
// entry of [LiveCall.RunningCalls]: what the tool was asked to do, which is
// the body.
const runningArguments = "arguments"

// invocationEntry is the key of a CLI run's providers.llm entry inside each
// entry of [LiveCall.Invocations]: the company's configuration.
const invocationEntry = "provider_key"

// MaskCall is THE ONE MASK of a live call, whichever shape carries it: a
// [*LiveCall], the projection's own, or the [leanCall] an `agents` push carries
// once [LiveCall.Without] has left out what a tab holds. Nil answers nil, and
// so does a typed nil; any other dynamic type is withheld whole (nil), which
// TestMaskRowHandlesEveryShape keeps unreachable by feeding this both shapes
// the package produces.
//
// With r.Body, the call comes back as it was but for the tracker rule. Without
// it, the call comes back as a [withheldCall]: no body key at all, every
// running call without its arguments, every body version zero, and
// `body_withheld`. Without r.Tracker, either way, it carries no work item.
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
	if r.Body && r.Tracker && r.Config {
		return c
	}
	dup := *c
	if !r.Tracker {
		dup.WorkItem = nil
	}
	if !r.Config {
		dup.Error = dup.Error.For(r)
		dup.Invocations = withoutEntries(dup.Invocations)
	}
	if r.Body {
		return &dup
	}
	// THE ROUNDS AND THE RUNS ARE STATUS and a whole call carries them
	// always, so the withheld one does too.
	return withhold(&dup, &dup.Rounds, &dup.Invocations)
}

// maskLean masks the shape an `agents` push carries. c is a copy already —
// [leanCall] travels by value — so its shadow pointers are kept as they were:
// the push decided which heavy fields to leave out, and a reader with the
// body is sent exactly those.
func maskLean(c leanCall, r Reach) any {
	if r.Body && r.Tracker && r.Config {
		return c
	}
	dup := *c.LiveCall
	if !r.Tracker {
		dup.WorkItem = nil
	}
	if !r.Config {
		dup.Error = dup.Error.For(r)
		dup.Invocations = withoutEntries(dup.Invocations)
		if c.Invocations != nil {
			runs := withoutEntries(*c.Invocations)
			c.Invocations = &runs
		}
	}
	if r.Body {
		c.LiveCall = &dup
		return c
	}
	// THE ROUNDS AND THE RUNS AS THE PUSH CARRIED THEM: present only when
	// it carried them, since a tab holding them at the version the push
	// names keeps its copy.
	return withhold(&dup, c.Rounds, c.Invocations)
}

// withhold builds the call a reader without its body receives from dup, a copy
// of the call it may no longer share with anybody, and the rounds and the runs
// as the input carried them.
//
// THE BODY IS CLEARED ON THE COPY TOO, beside the shadows that keep it off
// the wire: the value is handed out as `any`, and a reader that asserted the
// embedded call back out of it must not find the transcript there either.
//
// EVERY RUNNING CALL IS A FRESH MAP: the list's entries are the projection's
// own, shared with every other variant of the broadcast, so the arguments are
// left out of a copy rather than deleted from the entry.
func withhold(dup *LiveCall, rounds, runs *[]any) withheldCall {
	dup.Trigger = nil
	dup.Prompt, dup.PromptMessages, dup.Response = "", nil, ""
	dup.ToolExecutions, dup.RoundNarration, dup.PartialRound = nil, nil, nil
	if dup.RunningCalls != nil {
		calls := make([]any, 0, len(dup.RunningCalls))
		for _, entry := range dup.RunningCalls {
			call, ok := entry.(map[string]any)
			if !ok {
				// Nothing the projection keeps (see runningCalls), and
				// nothing that could be stripped of its arguments.
				continue
			}
			running := make(map[string]any, len(call))
			for k, v := range call {
				if k != runningArguments {
					running[k] = v
				}
			}
			calls = append(calls, running)
		}
		dup.RunningCalls = calls
	}
	dup.Versions = CallVersions{Rounds: dup.Versions.Rounds,
		Invocations: dup.Versions.Invocations}
	return withheldCall{LiveCall: dup, Rounds: rounds, Invocations: runs, BodyWithheld: true}
}

// withoutEntries is a CLI run list without the entry each run was served by,
// as fresh maps: the list's entries are the projection's own, shared with
// every other variant of the broadcast. Nil stays nil.
func withoutEntries(runs []any) []any {
	if runs == nil {
		return nil
	}
	out := make([]any, 0, len(runs))
	for _, entry := range runs {
		run, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		fresh := make(map[string]any, len(run))
		for k, v := range run {
			if k != invocationEntry {
				fresh[k] = v
			}
		}
		out = append(out, fresh)
	}
	return out
}

// For is the failure as reader r receives it: without its own text unless
// they read the company's configuration, as a copy. Nil answers nil.
func (e *ErrorInfo) For(r Reach) *ErrorInfo {
	if e == nil || r.Config || e.Detail == "" {
		return e
	}
	dup := *e
	dup.Detail = ""
	return &dup
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
	// Rounds and Invocations are carried as the input carried them — see
	// [MaskCall].
	Rounds      *[]any `json:"rounds,omitempty"`
	Invocations *[]any `json:"invocations,omitempty"`
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
	view := overlayView{Overlay: *o, LiveCall: MaskCall(o.LiveCall, r),
		Turn: maskTurn(o.Turn, r)}
	view.LastError = o.LastError.For(r)
	return view
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
// they do not hold Worklet, as a copy.
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
// The work item is the tracker's, and the entry the run was launched on the
// company's configuration, an admin's whatever else the reader may read (see
// the package's doc); the model it runs under stays everybody's.
func (e SandboxEntry) For(r Reach) SandboxEntry {
	out := e
	if !r.Tracker {
		out.WorkItem = nil
	}
	if !r.Config {
		out.ProviderKey = ""
	}
	if r.Body {
		return out
	}
	out.Task = ""
	out.BodyWithheld = true
	if r.Record == "" || !slices.Contains(e.AudienceHandles, r.Record) {
		out.Question, out.QuestionContext, out.Decision = "", "", nil
	}
	return out
}

// MaskRow masks one `agents` or `seats` row (a roster row with the live
// overlay merged onto it, [LiveState.MergeAgents], or an overlay row,
// [LiveState.OverlayRows], its call already a [leanCall] where a push left
// fields out) and returns a NEW map: the call through [MaskCall], the turn
// through the tracker rule, the failure through [ErrorInfo.For], every other
// key as it was.
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
	if failure, ok := row["last_error"].(*ErrorInfo); ok {
		out["last_error"] = failure.For(r)
	}
	return out
}
