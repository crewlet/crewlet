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
// log depends on: no elision, no read cap, nothing dropped. The cross-round
// and cross-turn ledgers opt into the budgets explicitly.
type FormatOptions struct {
	// Skip names never render. Meta-tools (activate_tool, list_..._tools)
	// are never a delivery, so in a record whose only job is "what already
	// happened that matters" they are pure noise.
	Skip []string

	// Reads are the tools POSITIVELY annotated read-only, as the delivery
	// gate resolved them. Positively is the operative word — see phase.Delivery.
	Reads []string

	ValueLimit   int
	BlobLimit    int
	MaxReadCalls int
}

// Format is the budgeted form the cross-round and cross-turn ledgers use.
func Format(skip, reads []string) FormatOptions {
	return FormatOptions{
		Skip: skip, Reads: reads,
		ValueLimit: ValueLimit, BlobLimit: BlobLimit, MaxReadCalls: MaxReadCalls,
	}
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
	var lines []string
	readsRendered, readsDropped := 0, 0
	for _, call := range calls {
		if slices.Contains(opts.Skip, call.Name) {
			continue
		}
		isRead := slices.Contains(opts.Reads, call.Name)
		if isRead && opts.MaxReadCalls > 0 && readsRendered >= opts.MaxReadCalls {
			readsDropped++
			continue
		}
		outcome := "success"
		if call.Failed {
			outcome = "error: " + elide(call.Result, opts.ValueLimit)
		}
		marker := ""
		if isRead {
			marker = " (read)"
			readsRendered++
		}
		lines = append(lines, "- "+call.Name+"("+renderArgs(call.Args, opts)+") → "+outcome+marker)
	}
	if readsDropped > 0 {
		lines = append(lines, "- (+"+itoa(readsDropped)+" further read call(s) omitted)")
	}
	if len(lines) == 0 {
		return "(none)"
	}
	return strings.Join(lines, "\n")
}

func renderArgs(args map[string]any, opts FormatOptions) string {
	if len(args) == 0 {
		return ""
	}
	if opts.ValueLimit <= 0 {
		return marshal(args)
	}
	elided := make(map[string]any, len(args))
	for k, v := range args {
		elided[k] = elideValue(v, opts.ValueLimit)
	}
	return fitArguments(elided, opts.BlobLimit)
}

// Iteration is one completed Plan → Execute → Review round of a single turn.
//
// Appended by the turn engine immediately before a self_iterate loops back to
// Plan, so it is a CLOSED snapshot: the phases it describes have all finished
// and none of them will run again. Every record describes a self_iterate round
// — the done and failed branches end the turn instead of appending — so the
// review decision is implied and is not stored.
//
// JSON tags are load-bearing, not decoration: a detached sandbox run ends the
// turn and its completion starts a NEW one, so without round-tripping the
// ledger through the pending run's persisted state the resumed turn would
// forget every earlier round and could re-fire its deliveries.
type Iteration struct {
	Iteration    int    `json:"iteration"`
	PlanSummary  string `json:"plan_summary,omitempty"`
	PlanCalls    []Call `json:"plan_tool_calls,omitempty"`
	ExecuteCalls []Call `json:"execute_tool_calls,omitempty"`

	// Reads are the names among the calls above that MCP annotations
	// positively mark read-only, as resolved by the delivery gate. Carried
	// per-record rather than looked up at render time because the surface
	// can change between rounds, and a read re-classified as a write would
	// retroactively rewrite what the ledger says happened.
	Reads []string `json:"read_only_names,omitempty"`

	ExecuteText string `json:"execute_text,omitempty"`
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
// Section HEADINGS are the caller's: Plan and Execute frame this as "already
// done, do not repeat" while Review frames it as duplicate-delivery evidence,
// so engine prose stays in the prompt package and this stays a renderer.
func RenderIterations(records []Iteration, skip []string) string {
	if len(records) == 0 {
		return ""
	}
	blocks := make([]string, 0, len(records))
	for _, rec := range records {
		lines := []string{"### Iteration " + itoa(rec.Iteration)}
		if rec.PlanSummary != "" {
			lines = append(lines, "Planned: "+elide(rec.PlanSummary, PlanSummaryLimit))
		}
		opts := Format(skip, rec.Reads)
		for _, group := range []struct {
			label string
			calls []Call
		}{
			{"Plan called:", rec.PlanCalls},
			{"Execute called:", rec.ExecuteCalls},
		} {
			lines = append(lines, group.label, FormatCalls(group.calls, opts))
		}
		if rec.ExecuteText != "" {
			lines = append(lines, "Produced: "+elide(rec.ExecuteText, ArtifactLimit))
		}
		if rec.CompletedWork != "" {
			lines = append(lines, "Reviewer, on what already landed: "+elide(rec.CompletedWork, NoteLimit))
		}
		if rec.ReviewNotes != "" {
			lines = append(lines, "Reviewer's correction: "+elide(rec.ReviewNotes, NoteLimit))
		}
		blocks = append(blocks, strings.Join(lines, "\n"))
	}
	return strings.Join(blocks, "\n\n")
}
