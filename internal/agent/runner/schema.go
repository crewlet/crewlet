package runner

// The JSON Schemas the two structured-output tools publish.
//
// Hand-written rather than reflected off the payload structs, deliberately.
// The DESCRIPTIONS are the contract with the model: what each field and each
// enum value means, which reaches the model beside the field on every round.
// Generating them from tags would put that prose in struct tags, where it
// cannot be read, reviewed or wrapped.
//
// ONE COPY OF EACH RULE between these and the phase's system prompt. A fact
// about a field — what an outcome means, that `deliveries` is checked, what
// `completed_work` carries — lives here, on the field; a rule about the turn —
// act with tool calls, finish the arc, how to judge a blocked round — lives in
// the prompt (internal/agent/prompts). They were three copies once, prompt,
// tool description and field, and the copies drifted until the review
// schema asked the reviewer about delivery in words its prompt had retired.
//
// The struct tags and these schemas must agree; the tests assert a submission
// shaped by the schema decodes into the struct.

import "github.com/crewlet/crewlet/internal/agent/phase"

var workSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"outcome": map[string]any{
			"type": "string",
			"enum": []any{"delivered", "no_action", "blocked"},
			"description": "delivered: the work is done, and where delivering it " +
				"meant calling a tool, you called it. " +
				"blocked: you cannot proceed — say what stopped you in `evidence`. " +
				"no_action: nobody was asking you to do anything, so the turn " +
				"ends silently. Declining something you were asked, mentioned in " +
				"or assigned is a reply, not no_action: report it as delivered.",
		},
		"summary": map[string]any{
			"type": "string",
			"description": "What you did, in your own words: what the reviewer " +
				"reads first, and what a later turn on this conversation sees as " +
				"what this one set out to do.",
		},
		"deliveries": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
			"description": "The tools you called that delivered something outside " +
				"the engine — the post, the comment, the status change. Names " +
				"only, of calls you actually made this turn: the engine checks " +
				"them against its own record, and a name it cannot find comes " +
				"back to you for correction.",
		},
		"evidence": map[string]any{
			"type": "string",
			"description": "Required when blocked: what you tried and what stopped " +
				"you — the failing call, the missing credential, the decision " +
				"you do not have the authority to make.",
		},
		"open_questions": map[string]any{
			"type": "string",
			"description": "Anything the reviewer or the next round should know " +
				"that the summary does not cover.",
		},
	},
	"required": []any{"summary"},
}

// reviewDecisionEnum is the submission schema's copy of what a reviewer may
// decide, taken from [phase.ReviewDecisions] rather than spelled again.
//
// Spelled again is what it was. A schema offering a value the decoder refuses
// bounces a model that did exactly what it was told, and nothing in either
// list can see the other.
func reviewDecisionEnum() []any {
	out := make([]any, 0, len(phase.ReviewDecisions()))
	for _, d := range phase.ReviewDecisions() {
		out = append(out, d.String())
	}
	return out
}

var reviewSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"decision": map[string]any{
			"type": "string",
			"enum": reviewDecisionEnum(),
			"description": "done: this turn is over. self_iterate: send it back " +
				"for another round, with `notes` saying what it must do " +
				"differently. failed: it cannot be completed at all, and another " +
				"round would not change that.",
		},
		"notes": map[string]any{
			"type": "string",
			"description": "Required on self_iterate: an actionable correction for " +
				"the next round — what was wrong, which tool, who to reach. Never " +
				"a naked problem: include what was tried, the options, and your " +
				"recommendation.",
		},
		"completed_work": map[string]any{
			"type": "string",
			"description": "On self_iterate, one sentence on what ALREADY landed " +
				"this turn — posts, comments, status changes — so the next round " +
				"adds to it instead of firing it a second time.",
		},
		"final_artifact": map[string]any{
			"type": "string",
			"description": "On done, the final text to return. Empty reuses " +
				"the executor's own output.",
		},
	},
	"required": []any{"decision"},
}

// The two submission tools' descriptions: what the call is and when to make
// it, and nothing a field's own description or the phase's system prompt
// already says.
//
// That a tool call is how anything gets done, and how to discover a tool, is
// the executor's header; that `deliveries` is checked is that field's. The
// reviewer's rules are its header; what each decision means is the `decision`
// field's. Each rule was written here as well, and the review copy had kept
// asking the reviewer whether the answer was delivered — the question its
// prompt says is settled before it runs, and the one whose re-derivation once
// read a real delivery as none and posted it twice.
const submitWorkDescription = "Report what you did and end the turn. Call " +
	"exactly once, when the work is finished or you are blocked."

const submitReviewDescription = "Submit your review decision and end the " +
	"review. Call exactly once."
