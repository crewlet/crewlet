package compact

import (
	"fmt"
	"strings"
)

// The rewrite instructions.
//
// Tuned for a SMALL model, which is what an auxiliary chain usually points at:
// the limit is stated in characters as a number, first and last, because a
// limit stated once in the middle is the instruction a small model drops; and
// the rules are a short list of what to KEEP rather than an essay on
// summarising, because "keep every identifier exactly" is checkable and "be
// concise" is not.
//
// A reworded rule is a behaviour change, like every prompt in this tree.

// baseRules is what every rewrite must honour, whatever the text is.
const baseRules = `Your answer REPLACES the original for a reader who will act on it — it is not a summary for someone skimming.

Keep, exactly as written:
- every identifier: ids, keys, ticket, issue and pull-request numbers, URLs, file paths, branch names, channel names, handles, email addresses, version numbers, error codes, quoted strings;
- every number with its unit, every date and time;
- every name of who said, asked or did what.

Keep, in fewer words:
- every action that was taken, every change that was made and where, every decision, every commitment, every open question.

Drop first: repetition, greetings and sign-offs, boilerplate, formatting, and reasoning that led nowhere.

Never add anything that is not in the original. Never comment on the text or on this task. Answer in the original's language, as plain text; short lines or bullets are fine.`

// kindBriefs says, per kind, what the text is and what in it matters most.
var kindBriefs = map[Kind]string{
	KindConversation: "The text is the EARLIER part of a conversation an AI agent had: for each " +
		"turn, what it was asked, which tools it called with what, and what it replied. " +
		"Keep every delivery — what was posted, sent, created or changed, and where — and " +
		"every reply the agent sent, so a later turn never repeats one. Keep the turns in order.",
	KindThread: "The text is the EARLIER messages of a chat thread, oldest first, each " +
		"prefixed with who wrote it. Keep who said what, in order; keep every question " +
		"asked and whether it was answered.",
	KindArgument: "The text is ONE argument value of a tool call an AI agent already made — " +
		"usually a message body, a document or a diff it wrote. Say what it contained " +
		"concretely enough that the agent can recognise the call it made and not make it again.",
	KindToolError: "The text is what a FAILED tool call returned. Keep the cause, the status " +
		"code, and anything naming what to change — a missing scope, a field, a limit.",
	KindProduced: "The text is everything an AI agent wrote during one earlier round of its " +
		"work, thinking included, ENDING with what it produced. Keep the produced content " +
		"itself in full detail; compress the reasoning before it hardest.",
	KindSource: "The text is a document being read to answer a question. Keep everything " +
		"that bears on the question, verbatim where you can — steps, values, conditions, " +
		"names — and drop what does not bear on it.",
	KindTask: "The text is the TASK an AI agent was given — the message, notification " +
		"or request that started its work, as the engine rendered it. Keep what is asked, " +
		"of whom, by when, every constraint, and every reference to where the work is.",
	KindAnswer: "The text is the ANSWER another AI worker returned for a task it was given — " +
		"either its structured submission as JSON or its prose — and it is the input to the " +
		"next task. If it is JSON, answer with JSON of the same shape: every key kept, long " +
		"string values shortened, no array element dropped without saying how many. If it is " +
		"prose, keep every finding, value and conclusion.",
	KindOutcome: "The text is what an AI agent's turn of work CONCLUDED — its final answer, or " +
		"the reviewer's account of what landed — and the rewrite is the one line a task's " +
		"card shows for that turn. Say what the turn did and delivered, and where, with " +
		"its identifiers; leave out how it got there.",
	KindReport: "The text is the report a coding agent wrote about a coding task it finished. " +
		"Keep what it changed and where, what it delivered (branches, pull requests, " +
		"commits), what it tested and how, what failed, and what it says is left to do.",
	KindQuestion: "The text is a QUESTION a coding agent stopped to ask a person before it " +
		"could finish its task; the person answers it, and the agent's work resumes on that " +
		"answer. Keep the question itself as a question, every option it offers and what " +
		"each one means, what it has already tried or ruled out, and what it needs decided — " +
		"so the person can answer it without asking anything back.",
}

// systemPrompt is one rewrite's instructions.
func systemPrompt(kind Kind, focus string, budget int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You rewrite text so that it fits in %d characters without losing "+
		"what its reader needs.\n\n", budget)
	b.WriteString(kindBriefs[kind])
	if focus = strings.TrimSpace(focus); focus != "" {
		fmt.Fprintf(&b, "\n\nThe question it is read to answer: %s", focus)
	}
	b.WriteString("\n\n")
	b.WriteString(baseRules)
	fmt.Fprintf(&b, "\n\nYour whole answer must be at most %d characters.", budget)
	return b.String()
}

// userPrompt carries the text, fenced so instructions inside it read as
// content rather than as a request to this call.
func userPrompt(text, retry string) string {
	var b strings.Builder
	if retry != "" {
		b.WriteString(retry)
		b.WriteString("\n\n")
	}
	b.WriteString("Rewrite the text between the markers.\n\n<<<TEXT\n")
	b.WriteString(text)
	b.WriteString("\nTEXT>>>")
	return b.String()
}
