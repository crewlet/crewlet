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

var workSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"outcome": map[string]any{
			"type": "string",
			"enum": []any{"delivered", "no_action", "blocked"},
			"description": "delivered: the work is done, and where delivering it " +
				"meant calling a tool, you called it. " +
				"blocked: you cannot proceed — say what stopped you in `evidence`. " +
				"no_action: nobody was actually asking you to do anything, so the " +
				"turn ends silently. Do NOT use no_action when you were asked, " +
				"mentioned or assigned and are declining: reply where you were " +
				"asked and report that as delivered, so the requester learns " +
				"their message was received.",
		},
		"summary": map[string]any{
			"type": "string",
			"description": "What you did, in your own words. This is what the " +
				"reviewer reads first and what a later turn on this conversation " +
				"sees as what this one set out to do.",
		},
		"deliveries": map[string]any{
			"type":  "array",
			"items": map[string]any{"type": "string"},
			"description": "The tools you called that delivered something outside " +
				"the engine — the post, the comment, the status change. Names " +
				"only, and they must be calls you actually made this turn: the " +
				"engine checks them against its own record.",
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

var reviewSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"decision": map[string]any{
			"type": "string",
			"enum": []any{"done", "self_iterate", "failed"},
			"description": "done: the work is finished and was actually " +
				"delivered. self_iterate: it is wrong or incomplete, the answer " +
				"was never delivered by a tool call, or the executor said it " +
				"lacks a tool it needs. failed: this cannot be completed at all.",
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
				"the executor's own output.",
		},
	},
	"required": []any{"decision"},
}

// The two submission tools' descriptions.
//
// This is the phase contract as the model receives it, and it is the most
// load-bearing prose in the engine: it is where an outcome enum stops being
// three strings and becomes a rule about when to reply versus stay silent.
const submitWorkDescription = "Report what you did and end the turn. Call " +
	"exactly once, when the work is finished or you are blocked.\n\n" +
	"USE TOOL CALLS FOR EVERY ACTION. Writing about an action does not " +
	"perform it: an answer that exists only in your reply reaches nobody. " +
	"If the task ends by replying where it arrived, call that channel's " +
	"post tool; if it creates, updates or transitions something, call that " +
	"write tool. You do not need to know the tool's name in advance — " +
	"`list_mcp_server_tools` lists a server's tools and `activate_tool` " +
	"puts one on your surface for the next round.\n\n" +
	"`deliveries` names the tools that actually delivered. The engine " +
	"checks them against what it recorded you calling, so a name you did " +
	"not call comes back for correction rather than ending the turn.\n\n" +
	"To hand work to a colleague, reach them where the work lives — a chat " +
	"mention, an issue comment, or an agent-to-agent ask — and report that " +
	"as the delivery. There is no separate delegate outcome.\n\n" +
	"A reviewer reads this turn's tool log afterwards and may send it back " +
	"with a correction."

const submitReviewDescription = "Submit your review decision. Call exactly once.\n\n" +
	"Choose self_iterate whenever the work is wrong or incomplete, the " +
	"answer was written but never delivered by a tool call, or the executor " +
	"narrated that it lacks a tool it needs — say which tool in `notes`.\n\n" +
	"Need a colleague or manager — blocked, lacking authority, or needing " +
	"someone else's identity or credentials? Still self_iterate: put the " +
	"ask in `notes` so the next round reaches them directly. Never hand " +
	"over a naked problem — include what was tried, the options you see, " +
	"and your recommendation.\n\n" +
	"The tool log is the evidence. What the executor said about itself is not."
