package prompts

import "testing"

func TestReviewPromptDescribesTheDecisionEnum(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{})
	contains(t, p, "done", "self_iterate")
	// Review decides done | self_iterate and nothing else; the colleague
	// handoff is an Execute-phase outreach step, not a third decision.
	excludes(t, p, "ask_colleague")
	// No catalogue, no policies, no org context, no identity block.
	excludes(t, p, "## Available tools", "Company Policies", "Company policies",
		"Respect teammates.", "Build great things.", "# Your Identity")
}

// Each rule below maps to a turn-ending failure that has actually happened.
func TestReviewHeaderCarriesEveryDecisionRule(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{})
	contains(t, p,
		// Evidence beats narration, and a Plan-phase call counts as
		// delivered — without that, a reply the planner posted during
		// recon gets demanded again and posted twice.
		"are the evidence", "already-delivered",
		// A failed call is not a delivery: a 5xx on a Plan-phase post read
		// as "Plan delivered" and the reviewer blessed a turn whose side
		// effect never landed.
		"→ error", "do not",
		// Delivery can be evidenced by EITHER log, or the rule would
		// contradict the "## What Plan did" section.
		"Tool-delivery rule", "EITHER log",
		// The sandbox rule stops the reviewer reading a delegated
		// investigation as fabrication and looping the turn forever.
		"Sandbox rule", "run_sandbox",
		// Keyed on target and content, not tool name: keyed on the name it
		// fired on the in-thread follow-up PriorWorkHeader asks for, and
		// every corrected turn looped until it terminated failed.
		"Duplicate-delivery rule", "target and content",
		"completed_work",
		// Execute narrating that it lacks a tool is a self_iterate, not a
		// failure: Plan can re-list tools_needed and Execute gets it.
		"Missing-tool rule", "re-list `tools_needed`",
		"I don't have access to the tool needed to deliver this",
		// Blocked on a colleague routes through self_iterate + outreach;
		// the colleague replies asynchronously and that re-triggers the
		// agent, so the reviewer must not wait.
		"Blocked / needs-a-colleague rule", "reply asynchronously",
	)
	// The cross-iteration half of the duplicate rule is worthless if it can
	// only see inside one iteration.
	contains(t, ReviewHeader, "Earlier iterations", "completed_work")
}

func TestReviewPromptRendersTheEvidenceTimeline(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{
		PlanSummary:       "PLAN:1,2,3",
		ExecuteSummary:    "EXECUTED:x",
		PlanToolLog:       "- post_message(...) → success",
		ExecuteToolLog:    "- update_item(...) → success",
		EarlierIterations: "### Iteration 1\nExecute called:\n- post_message(...) → success",
	})
	contains(t, p, "PLAN:1,2,3", "EXECUTED:x", "### Iteration 1",
		"post_message", "update_item")
	// Oldest first, so the reviewer reads the whole turn top-to-bottom as
	// one timeline. Anchored on the section breaks: ReviewHeader names
	// every one of these headings in its own prose.
	order(t, p,
		"\n## Plan\n",
		"\n## Earlier iterations (already delivered)\n",
		"\n## What Plan did\n",
		"\n## What Execute did\n",
		"\n## What Execute produced\n",
	)
}

// Both tool logs are ALWAYS rendered, as "(none)" when empty: the header
// points at them as the primary evidence, so an omitted section makes those
// instructions point at nothing, and a missing heading reads as "log
// unavailable" rather than "no calls were made".
func TestReviewPromptRendersEmptyToolLogsAsNone(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{PlanSummary: "P", ExecuteSummary: "E"})
	contains(t, p, "\n## What Plan did\n(none)", "\n## What Execute did\n(none)")
	// The timeline order holds uniformly, not only when the logs have
	// entries.
	order(t, p, "\n## What Plan did\n", "\n## What Execute did\n")
}

// The one section dropped rather than rendered as "(none)": its absence can
// only mean "this is iteration 1", and the header refers to it conditionally.
func TestReviewPromptOmitsEarlierIterationsOnTheFirstPass(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{PlanSummary: "P", ExecuteSummary: "E"})
	excludes(t, p, "\n## Earlier iterations (already delivered)\n")
}
