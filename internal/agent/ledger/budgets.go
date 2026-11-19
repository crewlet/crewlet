package ledger

import "strconv"

// Payload budgets, applied ONLY to the cross-round ledger — never to Review's
// single-iteration evidence log, which stays verbatim by contract.
//
// BUDGET PAYLOADS, NEVER STRUCTURE. A payload (a body, page HTML, a diff, an
// error document) is unbounded and buries the lines that answer the ledger's
// question. Structure (the round's account, the draft, the reviewer's
// correction, the trigger, the reply) is carried VERBATIM: a correction cut
// mid-instruction loses the engine-critical part, with no surface to go back to.
// A payload past its budget is REWRITTEN, NEVER CUT, since a cut keeps its
// opening, which the next round reads as the call: these are the sizes the
// seat's auxiliary model rewrites to (see [Piece]), one cheap call each.
const (
	// ValueLimit is the budget for an argument VALUE and for a failed call's
	// error. Identifiers are what say WHICH delivery fired — channel names,
	// issue keys, page ids, handles, thread timestamps, URLs — and a long
	// page URL with query parameters runs to ~180 runes, so a value of 200
	// is carried as itself, and only a body, a document or an error page is
	// past it. Two hundred bytes is also what a rewrite of one of those is
	// held to: about thirty words, which says what a message announced or
	// what a refusal was about.
	ValueLimit = 200

	// MaxReadCalls caps rendered tool-call lines per phase per round. Only
	// positively-known READS are ever omitted, and the line says how many: a
	// read is re-runnable by construction, and the prompt permits re-running
	// exactly those, so an omitted read is a call the next round can make
	// again rather than a fact it has lost. A write is the whole reason the
	// ledger exists and is never omitted, however many there are. Execute's
	// base round cap is 20 and its extension ceiling 40, so a busy round can
	// log dozens of calls; 12 covers the recon a normal round does while
	// keeping one block skimmable.
	MaxReadCalls = 12

	// RenderedArtifactLimit is the budget for a PRIOR round's produced text
	// as it is RENDERED into the next round's block. The Iteration record
	// keeps it whole; this is a render bound.
	//
	// It needs one because Execution.Text is every assistant message of that
	// round's tool loop concatenated: the round cap times what one round can
	// write, per iteration, accumulated per self_iterate and re-sent on every
	// round of the executor and reviewer. That product is what a bound answers.
	//
	// 4000 holds a full draft. Past it the text is rewritten with its
	// deliverable kept in full and the reasoning before it compressed hardest.
	RenderedArtifactLimit = 4000
)

func itoa(n int) string { return strconv.Itoa(n) }
