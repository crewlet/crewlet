package prompts

import (
	"strings"
	"testing"
)

func TestReviewPromptDescribesTheDecisionEnum(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{}).Text
	contains(t, p, "done", "self_iterate", "failed")
	// The colleague handoff is the next round's outreach, not a decision.
	excludes(t, p, "ask_colleague")
	// No catalogue, no policies, no org context, no identity block.
	excludes(t, p, "## Available tools", "## Tools to discover", "Company Policies", "Company policies",
		"Respect teammates.", "Build great things.", "## Your Identity")
}

// Each rule below maps to a turn-ending failure that has actually happened.
func TestReviewHeaderCarriesEveryDecisionRule(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{}).Text
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
		"`incomplete` outcome was written by the engine",
		// Keyed on target and content, not tool name: keyed on the name it
		// fired on the in-thread follow-up PriorWorkHeader asks for, and
		// every corrected turn looped until it terminated failed.
		"Duplicates", "target and content",
		// An agent narrating that it lacks a tool is a self_iterate, not a
		// failure: the next round can discover and activate it.
		"missing tool", "discover and activate it",
		"I don't have access to the tool needed to deliver this",
		// Blocked on a colleague splits on whether they have BEEN TOLD,
		// because that is the half the tool log can settle: not yet is the
		// next round's outreach, already asked is the end of the turn. The
		// rule named only `self_iterate` once, while saying in the same
		// breath that the reply re-triggers the agent — so a seat that had
		// put a question to its founder was sent back to ask again and
		// ended on the max-iterations guard. Both halves are pinned: drop
		// the split and the loop returns, drop the async fact and the
		// reviewer has no reason to stop waiting.
		"BEEN TOLD", "re-triggers this agent",
		"still owed a reply where they asked",
	)
	// The cross-round half of the duplicate rule is worthless if it can
	// only see inside one round.
	contains(t, ReviewHeader, "Earlier rounds")
	// And the retired instructions must not linger: there is no plan to
	// re-list tools in, and a reviewer told to demand one sends every
	// blocked turn back for a phase that does not exist. What a field
	// carries — `completed_work` — is the field's own description, beside
	// it in the submission schema.
	excludes(t, ReviewHeader, "tools_needed", "Plan", "Execute phase", "completed_work")
}

// THE RULE ABOUT A TOOL FOLLOWS THE TOOL, like the executor's own sections:
// `a2a_ask` is named among the ways to reach somebody only where the seat
// holds it, or the reviewer writes notes the next round can only fail to
// follow.
func TestTheReviewRulesAboutAToolFollowTheSeatsSurface(t *testing.T) {
	t.Parallel()
	plain := BuildReview(engineer(), ReviewInput{}).Text
	excludes(t, plain, "a2a_ask")

	a2a := BuildReview(engineer(), ReviewInput{OffersA2A: true}).Text
	contains(t, a2a, "`a2a_ask`")
	// Inside the blocked rule it belongs to, after "not yet" and before the
	// two answers past it.
	order(t, a2a, "Not yet → `self_iterate`", "`a2a_ask`", "Already asked → `done`")
}

// THE RUN'S OWN WORK IS IN NO LOG, and every review is told so, because every
// executor is a harness's own run: its edits, commands, tests and commits
// happen in its box, and `## What the agent did` holds only the calls it made
// to the seat's tools. A reviewer that read the log as everything the agent
// did called a run that pushed its fix a fabrication and sent a delivered turn
// back until max_iterations ended it.
//
// UNCONDITIONAL: the plainest review, with no gate on, carries it.
//
// Mutation: drop the rule from ReviewHeader, and this goes red.
func TestEveryReviewIsToldTheRunsLocalWorkIsInNoLog(t *testing.T) {
	t.Parallel()
	plain := BuildReview(engineer(), ReviewInput{}).Text
	contains(t, plain, "ran as a harness with its own shell",
		"is not in `## What the agent did`", "is not fabrication",
		"`## What the agent delivered`")
}

// WHAT THE AGENT DELIVERED IS EVIDENCE, rendered after what it produced: the
// branches and pull requests are the one record of its local work besides its
// own report. Rendered only when there is something to list, and the count
// the list was bounded by is said rather than dropped.
func TestTheRunsDeliveredRefsReachTheReviewer(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{
		Produced:  "PRODUCED:x",
		Delivered: DeliveredRefs([]string{"acme/api#41", "fix/flake"}, 3),
	}).Text
	contains(t, p, "- acme/api#41", "- fix/flake", "(and 3 more the agent reported)")
	order(t, p, "\n## What the agent produced\n", "\n## What the agent delivered\n")

	// None reported: no section — the header names it conditionally.
	none := BuildReview(engineer(), ReviewInput{Delivered: DeliveredRefs(nil, 0)}).Text
	if strings.Count(none, "## What the agent delivered") != 1 {
		t.Errorf("an empty list rendered a section of its own:\n%s", none)
	}
}

func TestReviewPromptRendersTheEvidenceTimeline(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{
		Intent:            "INTENT:post the summary",
		Outcome:           "delivered",
		Produced:          "PRODUCED:x",
		ToolLog:           "- update_item(...) → success",
		EarlierIterations: "### Iteration 1\nCalled:\n- post_message(...) → success",
	}).Text
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
	own := BuildReview(engineer(), ReviewInput{Outcome: "delivered"}).Text
	contains(t, own, "the agent's own word")
	excludes(t, own, "written by the engine: the agent's pass ended")

	rescued := BuildReview(engineer(), ReviewInput{Outcome: "incomplete", Rescued: true}).Text
	contains(t, rescued, "written by the engine: the agent's pass ended")
	excludes(t, rescued, "the agent's own word")
}

// The tool log is ALWAYS rendered, as "(none)" when empty: the header points
// at it as the primary evidence, so an omitted section makes those
// instructions point at nothing, and a missing heading reads as "log
// unavailable" rather than "no calls were made".
func TestReviewPromptRendersAnEmptyToolLogAsNone(t *testing.T) {
	t.Parallel()
	p := BuildReview(engineer(), ReviewInput{Intent: "I", Produced: "E"}).Text
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
	p := BuildReview(engineer(), ReviewInput{Intent: "I", Produced: "E"}).Text
	excludes(t, p, "\n## Earlier rounds (already delivered)\n")
}
