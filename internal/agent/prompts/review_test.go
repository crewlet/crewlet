package prompts

import "testing"

func TestReviewPromptDescribesTheDecisionEnum(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{})
	contains(t, p, "done", "self_iterate", "failed")
	// The colleague handoff is the next round's outreach, not a decision.
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
		// Evidence beats narration.
		"is the evidence", "the agent's own account of itself",
		// A failed call is not a delivery: a 5xx on a post read as
		// "delivered" and the reviewer blessed a turn whose side effect
		// never landed.
		"→ error", "did not take effect",
		// DELIVERY IS SETTLED BEFORE THIS PROMPT EXISTS. Asking a model to
		// re-derive a fact the engine holds is how a real delivery got
		// read as no delivery and posted twice.
		"already settled", "Judge the work on its merits",
		// An engine-written outcome carries nobody's commitment, and a
		// reviewer that grades it as the agent's verdict is grading a
		// claim nobody made.
		"Incomplete rule", "written by the engine",
		// The sandbox rule stops the reviewer reading a delegated
		// investigation as fabrication and looping the turn forever.
		"Sandbox rule", "run_sandbox",
		// Keyed on target and content, not tool name: keyed on the name it
		// fired on the in-thread follow-up PriorWorkHeader asks for, and
		// every corrected turn looped until it terminated failed.
		"Duplicate-delivery rule", "target and content",
		"completed_work",
		// An agent narrating that it lacks a tool is a self_iterate, not a
		// failure: the next round can discover and activate it.
		"Missing-tool rule", "discover and activate it",
		"I don't have access to the tool needed to deliver this",
		// Blocked on a colleague routes through self_iterate + outreach;
		// the colleague replies asynchronously and that re-triggers the
		// agent, so the reviewer must not wait.
		"Blocked / needs-a-colleague rule", "reply asynchronously",
	)
	// The cross-round half of the duplicate rule is worthless if it can
	// only see inside one round.
	contains(t, ReviewHeader, "Earlier rounds", "completed_work")
	// And the retired instruction must not linger: there is no plan to
	// re-list tools in, and a reviewer told to demand one sends every
	// blocked turn back for a phase that does not exist.
	excludes(t, ReviewHeader, "tools_needed", "Plan", "Execute phase")
}

func TestReviewPromptRendersTheEvidenceTimeline(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{
		Intent:            "INTENT:post the summary",
		Outcome:           "delivered",
		Produced:          "PRODUCED:x",
		ToolLog:           "- update_item(...) → success",
		EarlierIterations: "### Iteration 1\nCalled:\n- post_message(...) → success",
	})
	contains(t, p, "INTENT:post the summary", "PRODUCED:x", "### Iteration 1",
		"post_message", "update_item", "`delivered`")
	// Oldest first, so the reviewer reads the whole turn top-to-bottom as
	// one timeline. Anchored on the section breaks: ReviewHeader names
	// these headings in its own prose.
	order(t, p,
		"\n## Earlier rounds (already delivered)\n",
		"\n## What the agent set out to do (its own account)\n",
		"\n## Reported outcome\n",
		"\n## What the agent did\n",
		"\n## What the agent produced\n",
	)
}

// A RESCUED OUTCOME IS THE ENGINE'S WORD, and the reviewer has to be told so:
// read as the agent's own verdict it is a commitment nobody made, and the
// whole point of `incomplete` being its own value is that the two stay
// distinguishable.
func TestTheOutcomeLineSaysWhoWroteIt(t *testing.T) {
	t.Parallel()
	own := BuildReview(engineer(), ReviewInput{Outcome: "delivered"})
	contains(t, own, "the agent's own word")
	excludes(t, own, "written by the engine: the agent's pass ended")

	rescued := BuildReview(engineer(), ReviewInput{Outcome: "incomplete", Rescued: true})
	contains(t, rescued, "written by the engine: the agent's pass ended")
	excludes(t, rescued, "the agent's own word")
}

// The tool log is ALWAYS rendered, as "(none)" when empty: the header points
// at it as the primary evidence, so an omitted section makes those
// instructions point at nothing, and a missing heading reads as "log
// unavailable" rather than "no calls were made".
func TestReviewPromptRendersAnEmptyToolLogAsNone(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{Intent: "I", Produced: "E"})
	contains(t, p, "\n## What the agent did\n(none)")
	// The timeline order holds uniformly, not only when the log has
	// entries.
	order(t, p, "\n## What the agent set out to do (its own account)\n",
		"\n## What the agent did\n", "\n## What the agent produced\n")
}

// The one section dropped rather than rendered as "(none)": its absence can
// only mean "this is round 1", and the header refers to it conditionally.
func TestReviewPromptOmitsEarlierRoundsOnTheFirstPass(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{Intent: "I", Produced: "E"})
	excludes(t, p, "\n## Earlier rounds (already delivered)\n")
}
