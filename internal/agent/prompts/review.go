package prompts

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/kept"
	"github.com/crewlet/crewlet/internal/toolname"
	"github.com/crewlet/crewlet/internal/words"
)

// ReviewHeader is the reviewer's contract: the rules that decide its one
// decision, each a turn-ending failure that happened, stated once. What the
// decisions mean, and what `notes` and `completed_work` carry, is the
// submission schema's (runner/schema.go). Duplicates go by target and content:
// by tool name the rule fired on the threaded follow-up the prior-work header
// asks for. Delivery is the engine's to settle: a reviewer re-deriving it read
// a real delivery as none and posted twice; "this turn's whole log" because
// the evidence below is one round's. The run's local work is in no log, which
// holds only its bridge calls: a reviewer took a pushed fix for a fabricated
// one. A `→ error` call still in flight may have taken effect (runner's
// UnfinishedNote); that a round checks before retrying is the executor's rule
// ([PriorWorkHeader], [ContinuedRoundRules]), not paid for in every review.
const ReviewHeader = "\n## REVIEW phase" +
	"\nJudge the work below, then call `" + toolname.SubmitReview + "` exactly once." +
	"\n" +
	"\n`## What the agent did` is the evidence: calls marked `→ success` " +
	"happened, and a call marked `→ error` did not take effect, unless it says " +
	"its outcome is unknown: that one may have. `## What the agent " +
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
// yet", and the one obligation no handoff discharges. The rule splits on
// whether the log shows the person told: once asked, their reply re-triggers
// the seat, so a round sent back only asks again (and ends on max
// iterations). Blocked stays the executor's outcome (turn.OutcomeBlocked),
// never a fourth decision, which would copy one axis onto the other.
const reviewBlockedOutcome = " Already asked → `done`: their reply " +
	"re-triggers this agent, so no round of THIS turn can produce it and " +
	"sending it back only asks twice. Nothing anyone replies would finish it " +
	"→ `failed`. Whoever triggered this turn is still owed a reply where " +
	"they asked, even one that only says who you are waiting on."

// ReviewInput is the evidence the reviewer judges against: what the round
// produced, assembled once when the Review phase starts, so it is fixed for
// that phase's rounds (the prefix cache) and differs between a turn's rounds.
type ReviewInput struct {
	// Intent is the executor's own one-line account of what it did, labelled
	// as the agent's claim rather than as fact.
	Intent string

	// Outcome is the submitted outcome word — delivered, no_action,
	// blocked, or the engine's own `incomplete`.
	Outcome string

	// Rescued marks an outcome the engine synthesised because the executor
	// never submitted one. It selects the wording of the outcome line, so
	// the reviewer is told nobody committed to that word.
	Rescued bool

	// Evidence is what the executor tried and what stopped it, which the
	// schema demands on a blocked round: the outcome word alone cannot show
	// an honest end from a round that gave up early.
	Evidence string

	// OpenQuestions is whatever the executor flagged for the reviewer or
	// the next round that its summary does not cover.
	OpenQuestions string

	// Produced is the executor's final prose — its run's closing reply.
	Produced string

	// RefusedReplies is the replies to the round's questions that the engine
	// refused as too large to hand the agent, rendered by [RefusedReplies].
	// Empty renders no section.
	RefusedReplies string

	// Delivered is the branches and pull requests the executor's run
	// reported delivering, rendered by [DeliveredRefs]: the one evidence of
	// its local work besides its report, since that work is in no log.
	// Empty renders no section; the header names it conditionally.
	Delivered string

	// ToolLog is the round's call log, VERBATIM. Always rendered, as
	// "(none)" when empty: the header points at it, and a missing heading
	// reads as "log unavailable" rather than "no calls were made".
	ToolLog string

	// EarlierIterations is the prior-work ledger, which lets the duplicate
	// rule see across rounds since ToolLog is one round's. Empty on round 1,
	// and then dropped rather than "(none)": its absence can mean only that,
	// and the header refers to it conditionally.
	EarlierIterations string

	// OffersA2A is whether the seat holds a2a_ask, which gates the blocked
	// rule's naming it among the ways the next round reaches a colleague.
	OffersA2A bool

	// Skills is the tool-skill registry: a skill listing the review phase
	// surfaces here, keyed on MCPServers, though Review has no domain tools,
	// so the reviewer can weigh the work against it (see skillCatalogue).
	Skills SkillCatalogue

	// MCPServers is every MCP server the seat's registry holds a tool of,
	// sorted: the servers the work under review could reach, which the
	// catalogue's server triggers are matched against (see [Surface]).
	MCPServers []string
}

// ReviewLateCall is the words the reviewer's log ends a late call's line with
// (internal/agent/ledger's Call.Late): a call an earlier run of the work made,
// which its log took after that run was replaced. True of a call of an earlier
// job of the round, and of one of an earlier round, alike. Without them the
// reviewer reads it as the reviewed run's own, and judges that run on a call
// it never made. And ONE call: the round whose collection read it in flight
// lists it under the earlier rounds, its outcome unknown, which the duplicate
// rule would otherwise read as a second send.
const ReviewLateCall = "(made once, by an earlier run of this task that was replaced before the call " +
	"finished; an earlier round may list this same call with its outcome unknown, which is not a " +
	"second call)"

// BuildReview renders the Review-phase system prompt, and its outline.
//
// The reviewer judges against evidence (the tool-call log, the delivered
// refs) beside the agent's own account. No policies, roster or backstory:
// whether this round's work is right needs none of them. Each piece of
// evidence is ONE outline section however many headings it carries (an
// agent's "## Summary", the ledger's "###" entries).
func BuildReview(seat Seat, in ReviewInput) Prompt {
	b := NewBuilder("\n")
	b.Lead("identity", "Identity", BuildIdentityLine(seat))
	b.Heading("review_phase", reviewHeader(in))
	b.Heading("tool_skills", skillCatalogue(in.Skills, PhaseReview, Surface{
		MCPServers: in.MCPServers,
	})...)
	// Evidence runs oldest-first, so the turn reads as one timeline.
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
	if in.RefusedReplies != "" {
		b.Heading("refused_replies", "\n## Replies the agent did not get", in.RefusedReplies)
	}
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
// reviewer, one per line, or "" when it reported none. The count the bounded
// list elided is said, or a reviewer shown three of five refs judges three.
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

// RefusedReply is a reply to a question the round's agent asked that the engine
// refused as too large to hand it: how many messages it was, what it came to as
// the engine records an answer, and what an answer may — never its words.
type RefusedReply struct {
	Messages, Bytes, MaxBytes int
}

// RefusedReplies renders the round's refused replies for the reviewer — what
// the engine did and told the agent, then one line per reply, oldest first —
// or "" when there were none.
//
// The reply is in none of the round's calls, so without the engine's account
// a question asked again reads as asked twice (ADR-0114). Figures only: the
// reply's words reached nobody. The duplicate-rule exception is stated here,
// not in [ReviewHeader], which every review pays for.
func RefusedReplies(replies []RefusedReply) string {
	if len(replies) == 0 {
		return ""
	}
	lines := []string{"A person replied to a question the agent asked in this round, and the engine " +
		"did not take the reply: it was past what an answer to the agent can carry, so the agent was " +
		"given none of it. The engine told the agent so, and to ask again with `" + toolname.AskAndWait +
		"` for a shorter form or another way to receive it if it still needed the answer, or else to " +
		"carry on without it and say in its reply that the reply did not reach it. A question it asked " +
		"again after such a reply is that follow-up, not the same question sent twice: the duplicate " +
		"rule does not hold it against the agent.", ""}
	for _, r := range replies {
		lines = append(lines, fmt.Sprintf("- a reply of %s, %s as the engine records an answer, past "+
			"the %s an answer to the agent can carry", words.Count(max(r.Messages, 1), "message",
			"messages"), kept.SizeUp(r.Bytes), kept.Size(r.MaxBytes)))
	}
	return strings.Join(lines, "\n")
}

// reviewHeader is [ReviewHeader] with the rule that holds only for some seats:
// a function of the seat's surface, never the round, so it stays byte-stable.
func reviewHeader(in ReviewInput) string {
	header := ReviewHeader
	if in.OffersA2A {
		header += reviewA2ARule
	}
	return header + reviewBlockedOutcome
}

// outcomeLine renders the outcome word with who wrote it: a reviewer reading
// a rescued (engine-chosen) word as the agent's grades a commitment nobody made.
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
