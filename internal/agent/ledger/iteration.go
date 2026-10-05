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
// is also what lets this package import nothing — everything that HOLDS a
// ledger (the turn context, the prompt builder, the API layer) would otherwise
// drag the provider stack in behind it.
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
}

// FormatOptions controls how a run of calls renders.
//
// The zero value is the VERBATIM contract Review's single-iteration evidence
// log depends on: nothing fitted, no read cap, nothing dropped. The
// cross-round and cross-turn ledgers opt into the budgets explicitly.
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

	// MaxReadCalls caps the READ lines rendered; 0 renders every one.
	MaxReadCalls int

	// Fitted is the caller's rewrite of each piece [CallPieces] named.
	// A piece missing from it renders whole.
	Fitted Fitted
}

// Format is the budgeted form the cross-round and cross-turn ledgers use.
// The caller fits the pieces it names and sets [FormatOptions.Fitted].
func Format(skip, reads []string) FormatOptions {
	return FormatOptions{
		Skip: skip, Reads: reads,
		ValueLimit: ValueLimit, MaxReadCalls: MaxReadCalls,
	}
}

// visible is the calls a render shows: everything but the skipped names, and
// the reads past MaxReadCalls.
func visible(calls []Call, opts FormatOptions) []Call {
	out := make([]Call, 0, len(calls))
	reads := 0
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
		out = append(out, call)
	}
	return out
}

// FormatCalls renders an evidence-only summary of tool calls.
//
// Each line is `- name(args) → success | error: text`, with a trailing
// "(read)" when the tool is positively annotated read-only.
//
// No calls — or none that survive Skip — renders "(none)" rather than an empty
// string, so a reader sees an explicit "no action taken" signal rather than an
// absent section. The difference matters: a missing section reads as a section
// the engine forgot to fill in.
func FormatCalls(calls []Call, opts FormatOptions) string {
	shown := visible(calls, opts)
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
		lines = append(lines, "- "+call.Name+"("+renderArgs(call.Args, opts)+") → "+outcome+marker)
	}
	if dropped := readsIn(calls, opts) - readsShown; dropped > 0 {
		// Only READS are ever omitted, and they are re-runnable by
		// construction — the prompt permits re-running exactly those. A
		// write is the whole reason the ledger exists and always renders.
		lines = append(lines, "- (+"+itoa(dropped)+" further read call(s) omitted)")
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n")
}

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
// JSON tags are load-bearing, not decoration: a detached sandbox run ends the
// turn and its completion starts a NEW one, so without round-tripping the
// ledger through the pending run's persisted state the resumed turn would
// forget every earlier round and could re-fire its deliveries.
type Iteration struct {
	Iteration int `json:"iteration"`

	// Intent is what the round set out to do, in the executor's own words.
	Intent string `json:"intent,omitempty"`

	// Calls is everything the round invoked, engine-recorded.
	Calls []Call `json:"tool_calls,omitempty"`

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
		lines = append(lines, "Called:", FormatCalls(rec.Calls, opts))
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
