package prompts

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/toolname"
)

// ReviewHeader is the reviewer's contract: the rules that decide its one
// decision.
//
// Every rule below is a turn-ending bug that has actually happened, and each
// is stated once, here. What the three decisions MEAN, and what `notes` and
// `completed_work` carry, is the submission schema's to say
// (runner/schema.go), beside the fields — a rule about judging the work lives
// in the prompt, a fact about a field lives on the field. The duplicate rule
// is keyed on target and content rather than tool name because keyed on the
// name it fired on the in-thread follow-up the prior-work header explicitly
// asks for — every corrected turn then looped until it terminated failed.
//
// THE BLOCKED RULE had contradicted itself in one breath: it named a single
// outcome, `self_iterate`, and then said the colleague "reply asynchronously
// and that re-triggers the agent". Both halves cannot hold — if the reply is
// what wakes the seat, then no round of THIS turn can produce it, and sending
// the round back asks a question that has already been asked. A seat that put
// a clarifying question to its founder in the founder's own thread was
// returned by the reviewer, asked the same question again two minutes later,
// and ended on the max-iterations guard.
//
// So the rule splits on the one fact the TOOL LOG holds — has the person
// been told — rather than on how stuck the agent sounds. What it must never
// become is a decision of its own: being blocked is the EXECUTOR's word for
// what happened (see turn.OutcomeBlocked), and `done` / `self_iterate` /
// `failed` are the REVIEWER's for whether that was good enough. A fourth
// decision spelling "blocked, and rightly so" would copy one axis onto the
// other, and the next kind of blocked would want a fifth.
//
// What the reviewer is NOT asked is whether the delivery happened. That
// question is settled before this prompt is built: the engine checks the
// executor's own claims against its record of the turn, refuses a bad one
// inside the loop where the model can still fix it, and overturns a `done`
// that delivered nothing. Asking a model to re-derive a fact the engine holds
// is how a real delivery got read as no delivery and posted twice — and the
// submission schema asked it anyway, in words this header had retired, until
// the two were made one contract.
//
// THE RECORD IS THE TURN'S, and the note says so because the evidence log
// below it is only this round's: a reviewer looking at a round that re-read
// an item after an earlier one answered sees no write here, and must not take
// that for a turn that reached nobody. Whether this round still owed a
// further delivery — a correction the first answer needs — is a question about
// the work's merits, which is the reviewer's.
//
// THE RUN'S OWN WORK IS IN NO LOG, and the header says so for every seat
// because every executor is a harness's own run: its edits, commands,
// tests and commits happen in its box, and the log holds only the calls it
// made to the seat's tools over the bridge. A reviewer that read the log as
// everything the agent did took a run that fixed a bug and pushed the branch
// for one that narrated a fix it never made — "no git or test calls, so the
// claim is fabricated" — and sent a delivered turn back to max_iterations.
const ReviewHeader = "\n## REVIEW phase" +
	"\nJudge the work below, then call `" + toolname.SubmitReview + "` exactly once." +
	"\n" +
	"\n`## What the agent did` is the evidence: calls marked `→ success` " +
	"happened, and `→ error` calls did not take effect. `## What the agent " +
	"produced` is the agent's own account of itself." +
	"\n**The agent ran as a harness with its own shell.** Its local work " +
	"— editing files, running commands and tests, committing — is not in " +
	"`## What the agent did`, which lists only the calls it made to your " +
	"tools. Judge that work on its report and on any branches or pull " +
	"requests listed under `## What the agent delivered`; a missing shell or " +
	"test call is not fabrication." +
	"\n**Delivery is already settled:** the engine checked the agent's " +
	"delivery claims against this turn's whole log, earlier rounds " +
	"included, and overturns a `done` when no round reached the person " +
	"waiting. Judge the work on its merits — is it right, is it complete, is " +
	"it what was asked for." +
	"\n**An `incomplete` outcome was written by the engine**, not by the " +
	"agent: the pass ended without reporting. Nobody stands behind it, so " +
	"choose `self_iterate` unless the record already shows the work finished." +
	"\n**Duplicates:** the same thing sent twice to the same place — in one " +
	"round, or again after `## Earlier rounds` — is a duplicate: " +
	"`self_iterate`. Judge by target and content, not tool name: a follow-up " +
	"that ADDS to an earlier delivery (a threaded reply, an edit) is correct." +
	"\n**A missing tool:** an agent that narrates it lacks one (\"I don't " +
	"have access to the tool needed to deliver this\") is `self_iterate`, " +
	"naming the tool in `notes` — the next round can discover and activate it." +
	"\n**Blocked on somebody — have they BEEN TOLD?** The tool log answers " +
	"that, not the agent's prose. Not yet → `self_iterate`, naming who to " +
	"reach in `notes`; the next round reaches them with its own tools, " +
	"wherever the work lives."

// reviewA2ARule names the agent-to-agent tool among the ways the next round
// can reach somebody, rendered only where the seat holds it
// ([ReviewInput.OffersA2A]): a reviewer told to send a round to a tool the
// seat does not have writes notes the next round can only fail to follow.
const reviewA2ARule = " An agent colleague is reached with `" + toolname.A2AAsk + "`."

// reviewBlockedOutcome closes the blocked rule: the two answers past "not
// yet", and the one obligation no handoff discharges.
const reviewBlockedOutcome = " Already asked → `done`: their reply " +
	"re-triggers this agent, so no round of THIS turn can produce it and " +
	"sending it back only asks twice. Nothing anyone replies would finish it " +
	"→ `failed`. Whoever triggered this turn is still owed a reply where " +
	"they asked, even one that only says who you are waiting on."

// ReviewInput is the evidence the reviewer judges against.
//
// Unlike the executor's, this is not a turn-start snapshot: it is what the
// round produced, assembled once when the Review phase starts. It is fixed
// for every round of that phase, which is the byte-stability the prefix cache
// needs; it legitimately differs between round 1 and round 2 of the same
// turn, because by then the evidence genuinely differs.
type ReviewInput struct {
	// Intent is the executor's own one-line account of what it did. The
	// first thing the reviewer reads, and deliberately labelled as the
	// agent's claim rather than as fact.
	Intent string

	// Outcome is the submitted outcome word — delivered, no_action,
	// blocked, or the engine's own `incomplete`.
	Outcome string

	// Rescued marks an outcome the engine synthesised because the executor
	// never submitted one. It selects the wording of the outcome line, so
	// the reviewer is told nobody committed to that word.
	Rescued bool

	// Evidence is what the executor tried and what stopped it, present on a
	// blocked round. The schema DEMANDS it there, so dropping it here would
	// make the engine ask for something and then throw it away — and the
	// reviewer's job on a blocked round is precisely to judge whether being
	// blocked is the honest end of this turn or a round that gave up early,
	// which it cannot do from the outcome word alone.
	Evidence string

	// OpenQuestions is whatever the executor flagged for the reviewer or
	// the next round that its summary does not cover.
	OpenQuestions string

	// Produced is the executor's final prose — its run's closing reply.
	Produced string

	// Delivered is the branches and pull requests the executor's run
	// reported delivering, rendered by [DeliveredRefs]. The one evidence of
	// its local work besides its own report, since that work is in no log.
	// Empty renders no section: a run that pushed nothing has nothing to
	// list, and the header names the section conditionally.
	Delivered string

	// ToolLog is the round's call log, VERBATIM: every call with its
	// success or error outcome. Always rendered, as "(none)" when empty —
	// the header points at it as the primary evidence, so an omitted
	// section would make those instructions point at nothing, and a
	// missing heading reads as "log unavailable" rather than "no calls
	// were made".
	ToolLog string

	// EarlierIterations is the prior-work ledger, empty on the first
	// round. The round's own tool log resets each round, which left the
	// reviewer unable to see a repeat ACROSS rounds. This restores exactly
	// that view, so the duplicate-delivery rule holds turn-wide.
	//
	// The one section that is dropped rather than rendered as "(none)":
	// its absence is unambiguous (it can only mean "this is round 1"), and
	// the header refers to it conditionally.
	EarlierIterations string

	// OffersA2A is whether the seat holds a2a_ask, which gates the blocked
	// rule's naming it among the ways the next round reaches a colleague.
	OffersA2A bool

	// Skills is the tool-skill registry. Review-phase skills are
	// operator-scoped: a skill listing the review phase surfaces here,
	// keyed on the role's MCP servers, even though Review has no domain
	// tools — guidance an operator wants the reviewer to weigh stays
	// available. Required skills render unmarked here; see
	// skillCatalogue.
	Skills SkillCatalogue
}

// BuildReview renders the Review-phase system prompt, and its outline.
//
// The reviewer sees the rules that decide its decision (what each decision
// means is `submit_review`'s schema, beside the field), what the agent said
// it set out to do, the tool-call log, and the text it produced — so it
// judges against evidence rather than self-narration — and what its run
// delivered, since its local work is in no log. `a2a_ask` is named only where
// the seat holds it. No domain tools, no catalogue, no policies /
// roster / backstory: the reviewer's question is whether this round's work is
// right, and everything it needs to answer that is in front of it.
//
// Each piece of evidence is ONE section of the outline however many headings
// of its own it carries: the produced text is an agent's prose and routinely a
// document with a "## Summary" in it, and the earlier rounds are a ledger of
// "###" entries — both nest inside the section that quotes them.
func BuildReview(seat Seat, in ReviewInput) Prompt {
	b := NewBuilder("\n")
	b.Lead("identity", "Identity", BuildIdentityLine(seat))
	b.Heading("review_phase", reviewHeader(in))
	b.Heading("tool_skills", skillCatalogue(in.Skills, PhaseReview, Surface{
		MCPServers: seat.mcpServers(),
	})...)
	// Evidence runs oldest-first so the reviewer reads the turn
	// top-to-bottom as one timeline: earlier rounds, then this round's
	// intent, then what it did, then what it produced.
	if in.EarlierIterations != "" {
		b.Heading("earlier_rounds", "\n## Earlier rounds (already delivered)", in.EarlierIterations)
	}
	if in.Intent != "" {
		b.Heading("intent", "\n## What the agent set out to do (its own account)", in.Intent)
	}
	if in.Outcome != "" {
		b.Heading("outcome", "\n## Reported outcome", outcomeLine(in.Outcome, in.Rescued))
	}
	if in.Evidence != "" {
		b.Heading("blocked_by", "\n## What blocked it (the agent's account)", in.Evidence)
	}
	b.Heading("tool_log", "\n## What the agent did", orNone(in.ToolLog))
	if in.OpenQuestions != "" {
		b.Heading("open_questions", "\n## Open questions the agent raised", in.OpenQuestions)
	}
	if in.Produced != "" {
		b.Heading("produced", "\n## What the agent produced", in.Produced)
	}
	if in.Delivered != "" {
		b.Heading("delivered_refs", "\n## What the agent delivered", in.Delivered)
	}
	return b.Build()
}

// DeliveredRefs renders the branches and pull requests a run reported for the
// reviewer, one per line, saying how many more it reported than the list
// carries — or "" when it reported none.
//
// THE ELIDED COUNT IS SAID, never dropped: the list is bounded where the run
// is collected, and a reviewer shown three refs of a run that pushed five
// would judge the five as three.
func DeliveredRefs(refs []string, elided int) string {
	if len(refs) == 0 && elided <= 0 {
		return ""
	}
	lines := make([]string, 0, len(refs)+1)
	for _, ref := range refs {
		lines = append(lines, "- "+ref)
	}
	if elided > 0 {
		lines = append(lines, fmt.Sprintf("- (and %d more the agent reported)", elided))
	}
	return strings.Join(lines, "\n")
}

// reviewHeader is [ReviewHeader] with the rule that holds only for some seats.
//
// A FUNCTION OF THE SEAT'S SURFACE, never of the round: the gate is frozen for
// the turn, so the review prompt stays byte-identical across the rounds of its
// phase.
func reviewHeader(in ReviewInput) string {
	header := ReviewHeader
	if in.OffersA2A {
		header += reviewA2ARule
	}
	return header + reviewBlockedOutcome
}

// outcomeLine renders the outcome word with who wrote it.
//
// The attribution is the whole point of the field: a rescued outcome is a
// word the ENGINE chose because the pass ended without reporting, and a
// reviewer that reads it as the agent's own verdict grades a commitment
// nobody made.
func outcomeLine(outcome string, rescued bool) string {
	if rescued {
		return "`" + outcome + "` — written by the engine: the agent's pass ended " +
			"without reporting an outcome at all."
	}
	return "`" + outcome + "` — the agent's own word for how this round ended."
}

// orNone renders the affirmative "no action taken" signal an empty log must
// carry, rather than leaving a blank the reviewer reads as a missing log.
func orNone(log string) string {
	if log == "" {
		return "(none)"
	}
	return log
}
