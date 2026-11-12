package ledger

import (
	"slices"
	"strings"
)

// Call is one tool invocation, as the ledger needs it.
//
// A ledger-owned type rather than the tool loop's Execution because the two
// answer different questions: the loop's record is live state a phase is still
// accumulating, this is a closed fact a later round reads. Keeping them apart
// is also what keeps the tool loop out of this package's imports — everything
// that HOLDS a ledger (the turn context, the prompt builder, the API layer)
// would otherwise drag the provider stack in behind it.
type Call struct {
	Name string

	// Args is the tool's arguments as the model supplied them. A map, not
	// the JSON string the wire carries, because elision works per VALUE and
	// re-parsing a string at render time would make every reader of a
	// persisted ledger repeat the parse — and disagree about malformed input.
	Args map[string]any

	// Result is the tool's output, or its error text when Failed.
	Result string

	// Failed marks a tool that reported failure. Recorded rather than
	// inferred from Result: a tool whose successful output happens to begin
	// "error:" is not a failure, and a phase that reads it as one loops
	// trying to fix something that worked.
	Failed bool

	// Digest is the call's identity as its run's call log admitted it — the
	// digest of its whole canonical arguments (internal/agent/turnctx's
	// ArgsDigest) — and empty where none was recorded: a call nothing
	// admitted, or one a build before it recorded.
	//
	// CARRIED BESIDE ARGS BECAUSE ARGS MAY NOT BE WHOLE. An executor run's
	// calls are recorded in a log bounded in bytes, beside its row, which keeps
	// a value past its share as a marker (internal/kept), and a parked turn's
	// closed rounds are condensed the same way to fit theirs; a later segment
	// that seeds its call log from the arguments it holds would take the call
	// for another, and derive another operation's id. Seeded by this, it
	// cannot.
	Digest string `json:"Digest,omitempty"`

	// Withheld is a call whose arguments, and with them their digest, every
	// record withholds: a call held for a person's approval and the one they
	// approved, whose arguments ride the run's row sealed (ADR-0077). Args
	// and Digest are both empty on it, and a later segment's call log counts
	// it as a call of its tool whose arguments are not known — never as the
	// tool called with none, which it was not.
	Withheld bool `json:"Withheld,omitempty"`

	// Unfinished is a call whose OUTCOME IS NOT KNOWN: an executor run's log
	// held it in flight when its round's calls were read — begun, its identity
	// on the record, and not finished as its collection stopped waiting for it:
	// still running past its bound or its turn's cap, its process gone, or its
	// update refused by the store (internal/sandbox's BridgeLog.InFlight). It
	// is Failed, so no reader takes it for a delivery or a submission, and its
	// Result says its effects may stand and are to be checked before it is
	// made again, never that it failed; Args are empty, an entry keeping none.
	// A later segment's call log counts it by Digest, or, where it has none —
	// a tool that may ask a person first — as a call of its tool whose
	// arguments are not known, never as the tool called with none.
	Unfinished bool `json:"Unfinished,omitempty"`
}

// FormatOptions controls how a run of calls renders.
//
// The zero value is the VERBATIM contract Review's single-iteration evidence
// log depends on: nothing fitted, no read cap, nothing dropped. The
// cross-round ledger opts into the budgets explicitly.
type FormatOptions struct {
	// Skip names never render. Meta-tools (activate_tool, list_..._tools)
	// are never a delivery, so in a record whose only job is "what already
	// happened that matters" they are pure noise.
	Skip []string

	// Reads are the tools POSITIVELY annotated read-only, as the delivery
	// gate resolved them. Positively is the operative word — see phase.Delivery.
	Reads []string

	// ValueLimit is the budget past which an argument value or a failed
	// call's error is a [Piece] the caller fits; 0 renders every one whole.
	ValueLimit int

	// MaxReadCalls caps the READ lines one render shows: [FormatCalls]'s
	// list, or every group of [FormatCallGroups] together; 0 renders every
	// one.
	MaxReadCalls int

	// Fitted is the caller's rewrite of each piece [CallPieces] named.
	// A piece missing from it renders whole.
	Fitted Fitted
}

// Format is the budgeted form the cross-round ledger uses, and the lists of
// the calls a relaunch that continues a kept session is told its run made
// (internal/agent/runner's calledAfter): those after the session's save, which
// [FormatElided] follows there as it does a round here, and those that finished
// only after their run stopped waiting for them, rendered together under one
// read cap ([FormatCallGroups]).
// The caller fits the pieces it names and sets [FormatOptions.Fitted].
func Format(skip, reads []string) FormatOptions {
	return FormatOptions{
		Skip: skip, Reads: reads,
		ValueLimit: ValueLimit, MaxReadCalls: MaxReadCalls,
	}
}

// visible is the calls a render of one list shows: everything but the skipped
// names, and the reads past MaxReadCalls.
func visible(calls []Call, opts FormatOptions) []Call {
	return visibleGroups([][]Call{calls}, opts)[0]
}

// visibleGroups is the calls each group of one render shows: everything but
// the skipped names, and the reads past MaxReadCalls, COUNTED ACROSS THE
// GROUPS in the order given, so a group shows only the reads the groups before
// it left of the cap. Of their concatenation, [visible] shows exactly these
// calls, in the same order.
func visibleGroups(groups [][]Call, opts FormatOptions) [][]Call {
	out := make([][]Call, len(groups))
	reads := 0
	for i, calls := range groups {
		shown := make([]Call, 0, len(calls))
		for _, call := range calls {
			if slices.Contains(opts.Skip, call.Name) {
				continue
			}
			if slices.Contains(opts.Reads, call.Name) {
				if opts.MaxReadCalls > 0 && reads >= opts.MaxReadCalls {
					continue
				}
				reads++
			}
			shown = append(shown, call)
		}
		out[i] = shown
	}
	return out
}

// FormatCalls renders an evidence-only summary of tool calls.
//
// Each line is `- name(args) → success | error: text`, with a trailing
// "(read)" when the tool is positively annotated read-only. A call whose
// arguments every record withholds says so in their place ([Call.Withheld]).
//
// No calls — or none that survive Skip — renders "(none)" rather than an empty
// string, so a reader sees an explicit "no action taken" signal rather than an
// absent section. The difference matters: a missing section reads as a section
// the engine forgot to fill in.
//
// It is [FormatCallGroups]' render of one group, so the two never part.
func FormatCalls(calls []Call, opts FormatOptions) string {
	return FormatCallGroups([][]Call{calls}, opts)[0]
}

// FormatCallGroups renders the lists of calls one brief tells under headings
// of its own (internal/agent/runner's calledAfter: the calls after a session's
// save, and those that finished only after their run stopped waiting for them),
// one string per group in the order given, each as [FormatCalls] renders a
// list, "(none)" and its omitted line included, UNDER ONE READ CAP:
// [FormatOptions.MaxReadCalls] counts the read lines of every group together,
// spent in the order the groups are given, and each group's omitted line counts
// the reads of that group the cap left out.
//
// ONE CAP, NEVER ONE PER GROUP: the cap bounds what one render tells of a round,
// and a brief rendering a round's calls as N lists capped apart is told N times
// the reads the cap allows. Fit them with [CallPieces] over the groups'
// concatenation, which names exactly what this shows.
func FormatCallGroups(groups [][]Call, opts FormatOptions) []string {
	shown := visibleGroups(groups, opts)
	out := make([]string, len(groups))
	for i, calls := range groups {
		out[i] = renderCalls(calls, shown[i], opts)
	}
	return out
}

// renderCalls is one group's lines: shown, the calls of calls the render shows
// ([visibleGroups]), and the line counting the reads of calls it leaves out.
func renderCalls(calls, shown []Call, opts FormatOptions) string {
	var lines []string
	readsShown := 0
	for _, call := range shown {
		isRead := slices.Contains(opts.Reads, call.Name)
		outcome := "success"
		if call.Failed {
			// FITTED, like the arguments beside it. Call.Result is the
			// tool's own output, which is a PAYLOAD by this package's
			// definition — authored outside the engine and unbounded, so a
			// failed HTTP call can put a whole error document on one
			// ledger line, re-sent on every round of every later phase.
			// The full text is on the phase event this line summarises.
			text := call.Result
			if over(text, opts.ValueLimit) {
				text = opts.Fitted.text(Piece{Kind: PieceError, Text: text, Limit: opts.ValueLimit})
			}
			outcome = "error: " + text
		}
		marker := ""
		if isRead {
			marker = " (read)"
			readsShown++
		}
		args := renderArgs(call.Args, opts)
		if call.Withheld {
			// SAID, never drawn as `()`: a call whose arguments every record
			// withholds made a call with arguments, and `tool()` reads as
			// the tool called with none — a different call, and a repeat a
			// reader takes for a retry of nothing.
			args = withheldArgs
		}
		lines = append(lines, "- "+call.Name+"("+args+") → "+outcome+marker)
	}
	if dropped := readsIn(calls, opts) - readsShown; dropped > 0 {
		// Only READS are ever omitted, and they are re-runnable by
		// construction — the prompt permits re-running exactly those. A
		// write is the whole reason the ledger exists and always renders.
		lines = append(lines, "- (+"+itoa(dropped)+" further read call(s) omitted)")
	}
	if len(lines) == 0 {
		return noCalls
	}
	return strings.Join(lines, "\n")
}

// withheldArgs is what [FormatCalls] renders in place of the arguments of a
// call whose arguments every record withholds ([Call.Withheld]). ONE WORDING
// FOR BOTH such calls, which a [Call] does not tell apart: the one the run
// held, whose arguments wait sealed for a person's approval, and the one the
// person approved, made off those sealed arguments.
const withheldArgs = "arguments sealed for a person's approval, kept on no record"

// noCalls is what [FormatCalls] renders for a run of calls that shows none.
const noCalls = "(none)"

// readsIn counts the read calls a render would show without a cap.
func readsIn(calls []Call, opts FormatOptions) int {
	n := 0
	for _, call := range calls {
		if !slices.Contains(opts.Skip, call.Name) && slices.Contains(opts.Reads, call.Name) {
			n++
		}
	}
	return n
}

// Iteration is one completed executor → reviewer round of a single turn.
//
// Appended by the turn engine immediately before a self_iterate loops back, so
// it is a CLOSED snapshot: the phases it describes have finished and none of
// them will run again. Every record describes a self_iterate round — the done
// and failed branches end the turn instead of appending — so the review
// decision is implied and is not stored.
//
// ONE CALL LIST, because one phase makes the calls. It was two while the turn
// planned in one conversation and acted in another, and the split was
// load-bearing then: the delivery gate took a different view of each. Nothing
// takes two views of one list.
//
// JSON tags are load-bearing, not decoration: a detached executor ends the
// turn's segment and its completion starts a NEW one, so without
// round-tripping the ledger through the pending run's persisted state the
// resumed turn would forget every earlier round and could re-fire its
// deliveries.
type Iteration struct {
	Iteration int `json:"iteration"`

	// Intent is what the round set out to do, in the executor's own words.
	Intent string `json:"intent,omitempty"`

	// Calls is everything the round invoked, engine-recorded — every call
	// its run's record kept.
	Calls []Call `json:"tool_calls,omitempty"`

	// Elided is the round's calls its run's record did not keep, by identity
	// ([Elided]): carried with the round so the turn's later segments still
	// count them — a resumed call log seeds them, and a delivery check reads
	// them as calls of the turn ([Called]) — where the calls themselves are
	// gone.
	Elided []Elided `json:"elided_calls,omitempty"`

	// Absorbed is the calls the run's call log took in from the round's own
	// calls' work — a delegated worker's under a `delegate` — by identity
	// ([Elided], Failed unset): carried with the round so a later segment's
	// call log seeds them, and read by nothing else, since the round did not
	// make them — no delivery check counts a worker's call as the turn's.
	Absorbed []Elided `json:"absorbed_calls,omitempty"`

	// Reads are the names among the calls above that MCP annotations
	// positively mark read-only. Carried per-record rather than looked up
	// at render time because the surface can change between rounds, and a
	// read re-classified as a write would retroactively rewrite what the
	// ledger says happened.
	Reads []string `json:"read_only_names,omitempty"`

	Text        string `json:"text,omitempty"`
	ReviewNotes string `json:"review_notes,omitempty"`

	// CompletedWork is reviewer-authored prose naming what already landed.
	// Empty whenever Review itself never chose self_iterate — above all on
	// the engine's post-Review done→self_iterate override, which is
	// precisely why the tool-call lists above carry the guarantee and this
	// field only enriches it.
	CompletedWork string `json:"completed_work,omitempty"`

	// ArtifactHash is the hash of what the round produced, as the turn's
	// stall guard observed it when the round was sent back
	// (internal/agent/turn's ArtifactHash).
	//
	// ON THE RECORD because a turn's rounds run in different segments: each
	// executor launch parks the turn, and the round after it is reviewed
	// in a segment that never saw the rounds before. The guard that ends a
	// turn repeating itself has to compare across segments, so the closed
	// rounds carry what it compares. A hash rather than the text: the record
	// rides every park, and the text is already on it under Text where it
	// is the round's own prose rather than the reviewer's chosen artifact.
	ArtifactHash string `json:"artifact_hash,omitempty"`
}

// RenderIterations renders accumulated rounds as the shared prior-work block.
//
// Returns "" when there is nothing to show — the first round of every turn —
// so callers drop the whole section rather than emit an empty heading.
//
// Section HEADINGS are the caller's: the executor frames this as "already
// done, do not repeat" while the reviewer frames it as duplicate-delivery
// evidence, so engine prose stays in the prompt package and this stays a
// renderer.
//
// fitted is the caller's rewrite of the pieces [IterationPieces] named; nil
// renders every payload whole.
func RenderIterations(records []Iteration, skip []string, fitted Fitted) string {
	if len(records) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(records))
	for _, rec := range records {
		lines := []string{"### Iteration " + itoa(rec.Iteration)}
		if rec.Intent != "" {
			lines = append(lines, "Set out to: "+rec.Intent)
		}
		opts := Format(skip, rec.Reads)
		opts.Fitted = fitted
		lines = append(lines, "Called:")
		// "(none)" ONLY FOR A ROUND THAT CALLED NOTHING: one whose record
		// keeps its calls as a tally alone — a parked turn's closed round
		// condensed that far — called what its tally line says.
		if len(rec.Calls) > 0 || Count(rec.Elided) == 0 {
			lines = append(lines, FormatCalls(rec.Calls, opts))
		}
		if elided := FormatElided(rec.Elided, skip); elided != "" {
			lines = append(lines, elided)
		}
		if rec.Text != "" {
			// FITTED at render, whole in the record. See
			// RenderedArtifactLimit: this text is a whole tool loop's
			// assistant output, one per iteration, re-sent on every round
			// of every later phase — so past the limit it is rewritten
			// to fit, deliverable first, rather than cut from either end.
			text := rec.Text
			if over(text, RenderedArtifactLimit) {
				text = fitted.text(Piece{Kind: PieceProduced, Text: text, Limit: RenderedArtifactLimit})
			}
			lines = append(lines, "Produced: "+text)
		}
		if rec.CompletedWork != "" {
			lines = append(lines, "Reviewer, on what already landed: "+rec.CompletedWork)
		}
		if rec.ReviewNotes != "" {
			lines = append(lines, "Reviewer's correction: "+rec.ReviewNotes)
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}
