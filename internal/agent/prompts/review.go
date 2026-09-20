package prompts

import "strings"

// ReviewHeader is the reviewer's contract: the decision enum plus the rules
// that decide it.
//
// Every rule below is a turn-ending bug that has actually happened. The
// sandbox rule stops the reviewer reading a run_sandbox-delegated
// investigation as fabrication (the absence of shell calls is the DESIGN) and
// looping the turn to max_iterations. The duplicate rule is keyed on target
// and content rather than tool name because keyed on the name it fired on the
// in-thread follow-up the prior-work header explicitly asks for — every
// corrected turn then looped until it terminated failed.
//
// THE BLOCKED RULE IS THE NEWEST, and it had contradicted itself in one
// breath: it named a single outcome, `self_iterate`, and then said the
// colleague "reply asynchronously and that re-triggers the agent". Both
// halves cannot hold — if the reply is what wakes the seat, then no round
// of THIS turn can produce it, and sending the round back asks a question
// that has already been asked. A seat that put a clarifying question to its
// founder in the founder's own thread was returned by the reviewer, asked
// the same question again two minutes later, and ended on the
// max-iterations guard: three rounds, a duplicate nag at a person who had
// answered nothing yet, and a turn that did exactly the right thing
// recorded as a breach.
//
// So the rule splits on the one fact the TOOL LOG holds — has the person
// been told — rather than on how stuck the agent sounds. What it must never
// become is a decision of its own: being blocked is the EXECUTOR's word for
// what happened (see turn.OutcomeBlocked, whose own doc already casts the
// reviewer's job here as exactly this yes/no), and `done` / `self_iterate`
// / `failed` are the REVIEWER's for whether that was good enough. A fourth
// decision spelling "blocked, and rightly so" would copy one axis onto the
// other, and the next kind of blocked would want a fifth.
//
// What the reviewer is NOT asked any more is whether the delivery happened.
// That question is settled before this prompt is built: the engine checks the
// executor's own claims against its record of the turn, refuses a bad one
// inside the loop where the model can still fix it, and overturns a `done`
// that delivered nothing. Asking a model to re-derive a fact the engine holds
// is how a real delivery got read as no delivery and posted twice.
const ReviewHeader = "\n## REVIEW phase" +
	"\nJudge the work below. Submit exactly one `submit_review` call with " +
	"one decision:" +
	"\n- **done** — this turn is over: the work meets the ask, or it is " +
	"honestly blocked on somebody already told. Return the artifact." +
	"\n- **self_iterate** — incomplete or wrong; send it back with notes " +
	"saying what the next round must do differently." +
	"\n- **failed** — it cannot be completed at all, and another round " +
	"would not change that." +
	"\n" +
	"\n`## What the agent did` is the evidence — each line shows the call " +
	"and its success/error outcome — not the narration in `## What the " +
	"agent produced`, which is the agent's own account of itself. Calls " +
	"marked `→ success` happened; `→ error` calls did not take effect." +
	"\n**Whether anything was DELIVERED is already settled** — the engine " +
	"checked the agent's delivery claims against that log before you saw " +
	"either, and will overturn a `done` on a turn that answered in text " +
	"and never called a tool. Judge the work on its merits instead: is it " +
	"right, is it complete, is it what was asked for." +
	"\n**Incomplete rule:** an outcome of `incomplete` was written by the " +
	"engine, not by the agent — the pass ended without reporting at all. " +
	"Nobody stands behind it. Judge the tool log and the produced text on " +
	"their own, and choose `self_iterate` unless the record already shows " +
	"the work finished." +
	"\n**Sandbox rule:** a successful `run_sandbox` call IS the code work " +
	"— a coding agent cloned the repo and ran the shell / tests inside " +
	"an isolated sandbox; its report is in `## What the agent produced`. " +
	"The absence of `git`/`shell`/`pytest`/`file` calls is NOT " +
	"fabrication — that work happens inside the sandbox, never as tool " +
	"calls here. Judge the report on its merits." +
	"\n**Duplicate-delivery rule:** judge by target and content, not tool " +
	"name. The same thing sent twice to the same place — in one round, " +
	"or again after `## Earlier rounds` — is a duplicate: choose " +
	"`self_iterate`. A follow-up that ADDS to an earlier delivery " +
	"(threaded reply, edit) is correct." +
	"\n**On `self_iterate`, set `completed_work`:** one sentence on what " +
	"ALREADY landed, so the next round adds to it instead of re-firing " +
	"it." +
	"\n**Missing-tool rule:** if the agent narrates that it lacks a tool " +
	"(e.g. \"I don't have access to the tool needed to deliver this\"), " +
	"choose `self_iterate` and name the tool in `notes` — the next round " +
	"can discover and activate it." +
	"\n**Blocked / needs-a-colleague rule — has the person BEEN TOLD?** " +
	"The tool log answers that, not the agent's prose. Not yet → " +
	"`self_iterate`, naming who to reach in `notes`; the next round " +
	"reaches them with its own tools — a chat mention, an issue comment, " +
	"a doc comment, `a2a_ask` — wherever the work lives. Already asked → " +
	"`done`: their reply is what re-triggers this agent, so no round of " +
	"THIS turn can produce it and sending it back only asks twice. " +
	"Nothing anyone replies would finish it → `failed`. Never leave a " +
	"direct request unanswered: whoever triggered this turn is owed a " +
	"reply where they asked, even one that only says who you are waiting " +
	"on."

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

	// Produced is the executor's final prose — the answer where the answer
	// is prose, the sandbox report where a coding run produced one.
	Produced string

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

	// Skills is the tool-skill registry. Review-phase skills are
	// operator-scoped: a skill listing the review phase surfaces here,
	// keyed on the role's MCP servers, even though Review has no domain
	// tools — guidance an operator wants the reviewer to weigh stays
	// available. Required skills render unmarked here; see
	// injectSkillCatalogue.
	Skills SkillCatalogue
}

// BuildReview renders the Review-phase system prompt.
//
// The reviewer sees the decision enum, what the agent said it set out to do,
// the tool-call log, and the text it produced — so it judges against evidence
// rather than self-narration. No domain tools, no catalogue, no policies /
// roster / backstory: the reviewer's question is whether this round's work is
// right, and everything it needs to answer that is in front of it.
func BuildReview(seat Seat, in ReviewInput) string {
	parts := []string{BuildIdentityLine(seat), ReviewHeader}
	parts = injectSkillCatalogue(parts, in.Skills, PhaseReview, Surface{
		MCPServers: seat.mcpServers(),
	})
	// Evidence runs oldest-first so the reviewer reads the turn
	// top-to-bottom as one timeline: earlier rounds, then this round's
	// intent, then what it did, then what it produced.
	if in.EarlierIterations != "" {
		parts = append(parts, "\n## Earlier rounds (already delivered)", in.EarlierIterations)
	}
	if in.Intent != "" {
		parts = append(parts, "\n## What the agent set out to do (its own account)", in.Intent)
	}
	if in.Outcome != "" {
		parts = append(parts, "\n## Reported outcome", outcomeLine(in.Outcome, in.Rescued))
	}
	if in.Evidence != "" {
		parts = append(parts, "\n## What blocked it (the agent's account)", in.Evidence)
	}
	parts = append(parts, "\n## What the agent did", orNone(in.ToolLog))
	if in.OpenQuestions != "" {
		parts = append(parts, "\n## Open questions the agent raised", in.OpenQuestions)
	}
	if in.Produced != "" {
		parts = append(parts, "\n## What the agent produced", in.Produced)
	}
	return strings.Join(parts, "\n")
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
