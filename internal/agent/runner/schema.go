package runner

// The JSON Schemas the two structured-output tools publish.
//
// Hand-written rather than reflected off the payload structs, deliberately.
// The DESCRIPTIONS are the contract with the model — they are where the
// decision rules live, and they are load-bearing prose, not documentation.
// Generating them from tags would put that prose in struct tags, where it
// cannot be read, reviewed or wrapped.
//
// The struct tags and these schemas must agree; the tests assert a submission
// shaped by the schema decodes into the struct.

var planSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"decision": map[string]any{
			"type": "string",
			"enum": []any{"plan", "direct", "skip"},
			"description": "plan: Execute runs the steps, then Review. " +
				"direct: a one-tool task with no multi-step plan. " +
				"skip: nobody was actually asking you to act — the turn " +
				"ends silently with your reasoning as its output. Do NOT " +
				"use skip when you were asked, mentioned or assigned and " +
				"are declining: use plan with one step that replies, so " +
				"the requester learns their message was received.",
		},
		"reasoning": map[string]any{
			"type":        "string",
			"description": "Why this plan. On a skip, this is the turn's whole output.",
		},
		"steps": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"intent": map[string]any{
						"type": "string", "description": "What this step accomplishes.",
					},
					"approach": map[string]any{
						"type": "string",
						"description": "How — prose, not code. If you already know the " +
							"exact content Execute should produce (the reply text, the " +
							"comment body), put it here: Execute sees it verbatim and " +
							"cannot see what you saw.",
					},
					"tools": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "Tools this step may call; a subset of tools_needed.",
					},
					"on_failure": map[string]any{
						"type": "string", "enum": []any{"retry", "skip"},
					},
				},
				"required": []any{"intent"},
			},
		},
		"tools_needed": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
			"description": "EVERY tool Execute will call — research AND the final " +
				"delivery tool. If the task ends by replying where it arrived, " +
				"include that channel's post tool; if it creates, updates or " +
				"transitions something, include that write tool. A plan listing " +
				"only research tools will compose text with no way to send it. " +
				"Plan-phase tool results are NOT forwarded to Execute.",
		},
		"success_criteria": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "What done looks like, for Review to judge against.",
		},
	},
	"required": []any{"decision"},
}

var reviewSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"decision": map[string]any{
			"type": "string",
			"enum": []any{"done", "self_iterate", "failed"},
			"description": "done: the success criteria are met. " +
				"self_iterate: the artifact is wrong or incomplete, a required " +
				"delivery tool was not actually called, or Execute said it lacks " +
				"a tool it needs. failed: this cannot be completed at all.",
		},
		"notes": map[string]any{
			"type": "string",
			"description": "Required on self_iterate: an actionable correction for " +
				"the next round. Blocked, lacking authority, or needing someone " +
				"else's identity? Still self_iterate — put the ask here so the " +
				"next plan adds an outreach step. Never hand over a naked " +
				"problem: include what was tried, the options, and your " +
				"recommendation.",
		},
		"completed_work": map[string]any{
			"type": "string",
			"description": "On self_iterate, what ALREADY landed this turn — " +
				"especially external side effects: posts, comments, status " +
				"changes — so the next round adds to it instead of firing it a " +
				"second time.",
		},
		"final_artifact": map[string]any{
			"type": "string",
			"description": "On done, the final text to return. Empty reuses " +
				"Execute's own output.",
		},
	},
	"required": []any{"decision"},
}

// The two submission tools' descriptions.
//
// This is the phase contract as the model receives it, and it is the most
// load-bearing prose in the engine: it is where a decision enum stops being
// three strings and becomes a rule about when to reply versus stay silent.
const submitPlanDescription = "Submit the final plan. Call exactly once.\n\n" +
	"`tools_needed` is REQUIRED for plan and direct, and MUST list EVERY " +
	"tool Execute will call — research AND the final delivery tool. If the " +
	"task ends by replying where it arrived, include that channel's post " +
	"tool; if it creates, updates or transitions something, include that " +
	"write tool. A plan listing only research tools will compose text with " +
	"no way to send it. If you do not know which tool yet, choose plan and " +
	"work it out there.\n\n" +
	"Plan-phase tool results are NOT forwarded to Execute. When you fetch " +
	"data to compose the answer, hand the answer to Execute in a step's " +
	"`approach` — do not assume Execute can see what you saw.\n\n" +
	"To hand work to a colleague, use plan with a step that reaches them " +
	"where the work lives — a chat mention, an issue comment, or an " +
	"agent-to-agent ask — and name that tool. There is no separate delegate " +
	"decision.\n\n" +
	"Review runs after Execute on every plan and direct decision. A skip " +
	"never runs Execute and therefore never runs Review."

const submitReviewDescription = "Submit your review decision. Call exactly once.\n\n" +
	"Choose self_iterate whenever the artifact is wrong or incomplete, a " +
	"required delivery tool was not actually called, or Execute narrated " +
	"that it lacks a tool it needs — tell the next plan to re-list " +
	"tools_needed with the missing tool.\n\n" +
	"Need a colleague or manager — blocked, lacking authority, or needing " +
	"someone else's identity or credentials? Still self_iterate: put the " +
	"ask in `notes` so the next plan adds an outreach step and Execute " +
	"reaches them directly. Never hand over a naked problem — include what " +
	"was tried, the options you see, and your recommendation.\n\n" +
	"The tool logs are the evidence. What Execute said about itself is not."
