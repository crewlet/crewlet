package prompts

import "strings"

// PriorWorkHeader introduces the prior-work ledger: what an EARLIER ROUND of
// this same turn already did.
//
// Each phase rebuilds its LLM conversation per iteration, so without this the
// model re-plans a delivery that already fired. The (read) distinction is the
// load-bearing part: a read's results are genuinely gone and must be
// re-fetched, while anything else changed the world and a human reads the
// second copy as a duplicate.
const PriorWorkHeader = "## Already done earlier in this turn" +
	"\nAn earlier round of this same turn ran and was sent back for another" +
	" pass. Every call below marked `→ success` ALREADY RAN. How to treat" +
	" each one:" +
	"\n- **`(read)`** — it only fetched data, and those results are NOT" +
	" carried into this round. Re-run it whenever you need the data again;" +
	" never invent data you no longer have." +
	"\n- **everything else** — assume it changed something outside the" +
	" engine (a post, a comment, a status change, a code run). Do NOT issue" +
	" it again: a human sees the second one as a duplicate." +
	"\n- **`→ error`** — it did NOT take effect. Retry it, fixed." +
	"\nPlan and deliver only what is still missing. If the gap is a follow-up" +
	" to something already delivered, ADD to it — reply in the existing" +
	" thread, edit the existing item — rather than re-sending the original." +
	" Doing that follow-up is expected and is not a duplicate."

// ConversationHistoryHeader introduces this seat's own earlier TURNS on the
// same thread / issue / item.
//
// The staleness warning is stronger here than in the prior-work ledger, and
// deliberately so: within a turn a read is minutes old, but across turns it
// can be a week old, and a model told to trust it will state last Tuesday's
// state as current.
const ConversationHistoryHeader = "## Earlier in this conversation" +
	"\nYour own earlier turns on this same thread / issue / item, oldest" +
	" first. This is what YOU already said and did here — nobody else's" +
	" record of it, and not a transcript of the conversation itself." +
	"\n- **Do not repeat a reply you already gave.** If your answer to what" +
	" has just come in is one you have already sent, say the new thing or" +
	" say nothing — a human reads the second copy as not having been read." +
	"\n- **`(read)` calls may be stale.** Their results were not carried" +
	" over and time has passed since. Re-run any whose data you need now;" +
	" never reuse a remembered value as if it were current." +
	"\n- **Everything else already took effect.** Treat those as landed," +
	" and follow up on them rather than re-issuing them." +
	"\n- **The conversation has moved on.** This is history, not the" +
	" current state — the task below is the newest thing said, and where" +
	" the two disagree the task wins."

// UserMessage is the per-round content of a Plan or Execute turn.
//
// This is the half of the prompt that is ALLOWED to move. Both ledgers grow
// as a turn iterates, so putting either in a system prompt would invalidate
// the provider's prefix cache on every loop — which is the whole reason the
// system prompts above take no ledger at all.
type UserMessage struct {
	// TaskDescription is the ask. Rendered as "(no description)" when
	// empty, so the model is told there is no task rather than being handed
	// a blank.
	TaskDescription string

	// PriorWork is the rendered iteration ledger — empty on the first
	// iteration of every turn, which is the common case, so the message
	// stays byte-identical to its pre-ledger form until a self_iterate
	// actually happens.
	PriorWork string

	// ConversationHistory is prior TURNS of this same conversation — empty
	// on its first turn and whenever the trigger has no reproducible
	// conversation key.
	ConversationHistory string
}

// BuildPhaseUserMessage renders the user message shared by the Plan and
// Execute phases.
//
// The three parts run oldest to newest — earlier turns of this conversation,
// then the ask, then earlier rounds of THIS turn (which happened after the
// ask arrived) — so the most recent context sits nearest the model's answer.
func BuildPhaseUserMessage(m UserMessage) string {
	task := m.TaskDescription
	if task == "" {
		task = "(no description)"
	}
	var parts []string
	if m.ConversationHistory != "" {
		parts = append(parts, ConversationHistoryHeader+"\n"+m.ConversationHistory)
	}
	parts = append(parts, "Task:\n"+task)
	if m.PriorWork != "" {
		parts = append(parts, PriorWorkHeader+"\n"+m.PriorWork)
	}
	return strings.Join(parts, "\n\n")
}
