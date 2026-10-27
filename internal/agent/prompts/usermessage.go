package prompts

import (
	"fmt"
	"strings"
)

// PriorWorkHeader introduces the prior-work ledger: what an EARLIER ROUND of
// this same turn already did.
//
// Each phase rebuilds its LLM conversation per iteration, so without this the
// model re-plans a delivery that already fired. The (read) distinction is the
// load-bearing part: a read's results are genuinely gone and must be
// re-fetched, while anything else changed the world and a human reads the
// second copy as a duplicate. The markers are the ledger's own
// (ledger.FormatCalls), so the header reads the lines below it rather than a
// vocabulary of its own.
//
// AND AN EARLIER ROUND'S DELIVERY IS CITABLE, which the header says because a
// model forbidden to repeat a write it cannot cite is caught between the two:
// the engine reads the whole turn's record (turn.Record), and a round sent
// back for a re-read that believed otherwise submitted with no citation, or
// went looking for some other surface to deliver on.
const PriorWorkHeader = "## Already done earlier in this turn" +
	"\nAn earlier round of this turn was sent back. Every call below marked" +
	" `→ success` ALREADY RAN:" +
	"\n- **`(read)`** fetched data only, and its results are not carried" +
	" over: re-run it if you need the data, never invent it." +
	"\n- **Everything else** changed something outside the engine: do NOT" +
	" issue it again — a person sees the second one as a duplicate. If it" +
	" was the delivery, it still counts: name it in `submit_work`'s" +
	" `deliveries`." +
	"\n- **`→ error`** did not take effect: retry it, fixed." +
	"\nDeliver only what is still missing. A follow-up to an earlier" +
	" delivery ADDS to it — reply in its thread, edit the item — rather than" +
	" re-sending it, and is not a duplicate."

// ConversationHistoryHeader introduces this seat's own earlier TURNS on the
// same thread / issue / item.
//
// The staleness warning is stronger here than in the prior-work ledger, and
// deliberately so: within a turn a read is minutes old, but across turns it
// can be a week old, and a model told to trust it will state last Tuesday's
// state as current.
const ConversationHistoryHeader = "## Earlier in this conversation" +
	"\nYour own earlier turns on this same thread / issue / item, oldest" +
	" first — what YOU already said and did here." +
	"\n- **Do not repeat a reply you already gave:** say the new thing or" +
	" say nothing." +
	"\n- **`(read)` calls may be stale:** re-run any whose data you need" +
	" now rather than reusing a remembered value." +
	"\n- **Everything else already took effect:** follow up on it rather" +
	" than re-issuing it." +
	"\n- **This is history, not the current state:** where it and the task" +
	" below disagree, the task wins."

// UserMessage is the per-round content of an executor turn.
//
// This is the half of the prompt that is ALLOWED to move. Both ledgers grow
// as a turn iterates, so putting either in a system prompt would invalidate
// the provider's prefix cache on every loop — which is the whole reason the
// system prompts above take no ledger at all.
//
// The reviewer does not use it: its evidence is assembled once per round and
// is a system-prompt fact, not something that accumulates as its own loop
// runs.
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

	// Steered is every note a person sent this turn that the brief carries,
	// each rendered by [SteerMessage]: the ones the turn has already read, in
	// the order read, then the ones this brief is the first to read.
	//
	// IN THE BRIEF, LAST, because the run it opens is a fresh session: a
	// phase the engine drives carries a read note in its conversation, after
	// its opening, and a run has no conversation of the engine's to carry
	// it in — so a correction a reviewer read would otherwise bind every
	// phase of the turn but the one that does the work again.
	Steered []string
}

// taskHeader introduces the ask.
//
// A HEADING, like the two ledgers it sits between, and that is the whole of
// the change from the bare "Task:" label it replaces — the word is the same
// word. The label was the one block of this message that was not a section of
// it, so under any structural reading of the document the ask belonged to
// whatever heading preceded it: with a conversation block present, the newest
// thing anybody said was filed inside "Earlier in this conversation", whose
// own text points at "the task below" as a sibling. The dashboard's prompt
// outline reads the same structure and drew the same wrong picture.
const taskHeader = "## Task"

// BuildPhaseUserMessage renders the executor's user message.
//
// The three parts run oldest to newest — earlier turns of this conversation,
// then the ask, then earlier rounds of THIS turn (which happened after the
// ask arrived) — so the most recent context sits nearest the model's answer.
// All three are top-level headings: they are peers, and a reader that nests
// them gets the ask wrong.
//
// The TASK IS ONE SECTION of the outline, whatever it carries. It is a
// trigger's whole body — a chat notification with the engine's own "## Triage"
// and "## Thread context" written into it, a pull request whose description
// opens on its own "# Title" — and every heading in it belongs to the ask, not
// beside it. The ledgers are one section each for the same reason: their
// entries are "###" lines, and a reply quoted in one can hold anything.
func BuildPhaseUserMessage(m UserMessage) Prompt {
	task := m.TaskDescription
	if task == "" {
		task = "(no description)"
	}
	b := NewBuilder("\n\n")
	if m.ConversationHistory != "" {
		b.Heading("conversation_history", ConversationHistoryHeader+"\n"+m.ConversationHistory)
	}
	b.Heading("task", taskHeader+"\n"+task)
	if m.PriorWork != "" {
		b.Heading("prior_work", PriorWorkHeader+"\n"+m.PriorWork)
	}
	for i, note := range m.Steered {
		// A key per note: a section's key is unique within its prompt,
		// and each note is its own heading.
		b.Heading(fmt.Sprintf("steered_%d", i+1), note)
	}
	return b.Build()
}

// SteerMessage is a person's note to a running turn, as the model reads it.
//
// RUNTIME-NEUTRAL AND FACTUAL, because the same text is read in every place a
// note lands: as a user message at the opening of a run of the engine's loop,
// in a coding run's brief, and inside a coding run where its CLI places what a
// hook hands it — after tool results, or as it is about to finish
// (internal/agent/steer). So it says nothing about WHERE it arrived, which would
// be false in all but one of them, and states only what holds everywhere — and
// each has cost a turn when a bare note left the model to guess: WHO sent it,
// through what, so a note from the person who runs the company is not weighed
// like text a tool returned; that it arrived while the work was UNDER WAY, so
// the model neither restarts the task nor reads it as a new request needing its
// own reply; and that it STANDS for the rest of the turn and is the later word
// where it and the task disagree, so a correction made early still binds the
// work done at the end. Plain statements rather than commands, because a vendor
// warns that command-like text a hook adds can trip a model's guard against
// instructions arriving from where they should not. The note itself is last and
// verbatim.
//
// Phase-neutral on purpose: the reviewer reads the same message as the
// executor. For one the note is an instruction and for the other it is part
// of what the work is now judged against, and one sentence says both.
func SteerMessage(sender, note string) string {
	who, lead := sender, sender
	if strings.TrimSpace(sender) == "" {
		who, lead = "a person who runs this company", "A person who runs this company"
	}
	return "## A note from " + who + ", sent while this turn was running" +
		"\n" + lead + " sent this through Crewlet while the work was under way." +
		" It is not a new request and not a repeat of the task: it corrects or" +
		" adds to the work in hand and stands for the rest of this turn; where" +
		" it and the task disagree, the note is the later word. What has" +
		" already been done stays unless the note says otherwise." +
		"\n\n" + strings.TrimSpace(note)
}
