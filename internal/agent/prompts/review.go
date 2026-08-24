package prompts

import "strings"

// ReviewHeader is the Review-phase contract: the decision enum plus the five
// rules that decide it.
//
// Every rule below is a turn-ending bug that has actually happened. The
// sandbox rule stops the reviewer reading a run_sandbox-delegated
// investigation as fabrication (the absence of shell calls is the DESIGN) and
// looping the turn to max_iterations. The duplicate rule is keyed on target
// and content rather than tool name because keyed on the name it fired on the
// in-thread follow-up the prior-work header explicitly asks for — every
// corrected turn then looped until it terminated failed.
const ReviewHeader = "\n## REVIEW phase" +
	"\nJudge the artifact Execute produced. Submit exactly one " +
	"`submit_review` call with one decision:" +
	"\n- **done** — meets the success criteria; return the artifact." +
	"\n- **self_iterate** — incomplete or wrong; loop back to Plan with " +
	"notes." +
	"\n" +
	"\n`## What Plan did` + `## What Execute did` are the evidence — each " +
	"line shows the call and its success/error outcome — not the " +
	"narration in `## What Execute produced`. Successful Plan calls " +
	"count as already-delivered (don't make Execute repeat them); failed " +
	"calls (`→ error`) do not." +
	"\n**Tool-delivery rule:** if a `tools_needed` action tool was NOT " +
	"successfully called in EITHER log, choose `self_iterate`." +
	"\n**Sandbox rule:** a successful `run_sandbox` call IS the code work " +
	"— a coding agent cloned the repo and ran the shell / tests inside " +
	"an isolated sandbox; its report is in `## What Execute produced`. " +
	"The absence of `git`/`shell`/`pytest`/`file` calls is NOT " +
	"fabrication — that work happens inside the sandbox, never as tool " +
	"calls here. Judge the report on its merits." +
	"\n**Duplicate-delivery rule:** judge by target and content, not tool " +
	"name. The same thing sent twice to the same place — across both " +
	"phases, or again after `## Earlier iterations` — is a duplicate: " +
	"choose `self_iterate`. A follow-up that ADDS to an earlier delivery " +
	"(threaded reply, edit) is correct." +
	"\n**On `self_iterate`, set `completed_work`:** one sentence on what " +
	"ALREADY landed, so the next round adds to it instead of re-firing " +
	"it." +
	"\n**Missing-tool rule:** if Execute narrates that it lacks a tool " +
	"(e.g. \"I don't have access to the tool needed to deliver this\"), " +
	"choose `self_iterate` — Plan can re-list `tools_needed` and Execute " +
	"gets the tool next pass." +
	"\n**Blocked / needs-a-colleague rule:** if the turn can't finish " +
	"without a manager or peer — a capability gap needing someone else's " +
	"identity / credentials, or a decision above the agent's authority — " +
	"choose `self_iterate` and say so in `notes`. Plan then adds an " +
	"outreach step and Execute reaches the colleague directly with its " +
	"own colleague-surface tools — a chat mention, an issue comment, a " +
	"doc comment, or `a2a_ask` — wherever the work lives. They reply " +
	"asynchronously and that re-triggers the agent. Never leave a direct " +
	"request unanswered."

// ReviewInput is the evidence the reviewer judges against.
//
// Unlike Plan and Execute this is not a turn-start snapshot: it is what the
// turn produced, assembled once when the Review phase starts. It is fixed for
// every round of that phase, which is the byte-stability the prefix cache
// needs; it legitimately differs between iteration 1 and iteration 2 of the
// same turn, because by then the evidence genuinely differs.
type ReviewInput struct {
	PlanSummary    string
	ExecuteSummary string

	// ExecuteToolLog and PlanToolLog are the actual call logs. BOTH are
	// always rendered, as "(none)" when empty — the header points at them
	// as the primary evidence, so an omitted section would make those
	// instructions point at nothing, and a missing heading reads as "log
	// unavailable" rather than "no calls were made".
	//
	// The Plan log matters because the planner can, and frequently does,
	// fire side-effecting calls during recon. Without it the reviewer
	// treats those actions as not-yet-done and demands Execute repeat
	// them — which is how one Slack reply got posted twice.
	ExecuteToolLog string
	PlanToolLog    string

	// EarlierIterations is the prior-work ledger, empty on the first
	// iteration. Both per-phase tool logs reset each iteration —
	// deliberately, so the engine's delivery gate cannot read iteration-1
	// calls as iteration-2 delivery — which left the reviewer unable to
	// see a repeat ACROSS iterations. This restores exactly that view, so
	// the duplicate-delivery rule holds turn-wide.
	//
	// The one section that is dropped rather than rendered as "(none)":
	// its absence is unambiguous (it can only mean "this is iteration 1"),
	// and the header refers to it conditionally.
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
// The reviewer sees the decision enum, the plan, Execute's text artifact and
// the tool-call logs from BOTH phases, so it judges against evidence rather
// than self-narration. No domain tools, no catalogue, no policies / roster /
// backstory: every correctness constraint that matters at Review time is
// already encoded in the plan's success_criteria.
func BuildReview(seat Seat, in ReviewInput) string {
	parts := []string{BuildIdentityLine(seat), ReviewHeader}
	if in.PlanSummary != "" {
		parts = append(parts, "\n## Plan", in.PlanSummary)
	}
	parts = injectSkillCatalogue(parts, in.Skills, PhaseReview, Surface{
		MCPServers: seat.mcpServers(),
	})
	// Evidence runs oldest-first so the reviewer reads the turn
	// top-to-bottom as one timeline: earlier rounds, then this round's
	// Plan, then its Execute.
	if in.EarlierIterations != "" {
		parts = append(parts, "\n## Earlier iterations (already delivered)", in.EarlierIterations)
	}
	parts = append(parts, "\n## What Plan did", orNone(in.PlanToolLog))
	parts = append(parts, "\n## What Execute did", orNone(in.ExecuteToolLog))
	if in.ExecuteSummary != "" {
		parts = append(parts, "\n## What Execute produced", in.ExecuteSummary)
	}
	return strings.Join(parts, "\n")
}

// orNone renders the affirmative "no action taken" signal an empty log must
// carry, rather than leaving a blank the reviewer reads as a missing log.
func orNone(log string) string {
	if log == "" {
		return "(none)"
	}
	return log
}
