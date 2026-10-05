package ledger

import "strconv"

// Payload budgets, applied ONLY to the cross-round and cross-turn ledgers —
// never to Review's single-iteration evidence log, which stays verbatim by
// contract.
//
// ONE principle decides every number: BUDGET PAYLOADS, NEVER STRUCTURE. A
// payload (a message body, page HTML, a diff, an error document) is
// unbounded, re-authored from scratch next round, and unable to answer the
// ledger's question on its own — so carrying it whole only buries the two
// lines that can.
//
// Structure is everything else: the round's own account of what it set out to
// do, the draft under review, the reviewer's correction, the trigger, the
// reply that was sent. That is exactly what the next round has to act on, and
// it is carried VERBATIM. It used not to be: six further limits sat here
// cutting each of those at 400 to 2000 runes. A reviewer's correction trimmed
// mid-instruction loses the engine-critical part of the only carrier it has,
// and the ledger is not re-readable from anywhere: unlike a chat message or an
// issue comment, there is no surface to go back to.
//
// A PAYLOAD PAST ITS BUDGET IS REWRITTEN, NEVER CUT. These numbers used to be
// where a payload was cut, and what survived was its opening — a message
// body's greeting, an error document's preamble — read by the next round as
// though it were the call. They are now the size a payload is REWRITTEN to by
// the seat's auxiliary model (see [Piece]), so what survives is what the
// payload said rather than where it started. Prompt caching keys on the
// system+tools prefix, which the ledger never touches, so a larger block costs
// little; a rewrite costs one cheap call, once per payload per turn.
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
	// round's tool loop concatenated, thinking included — so its size is the
	// round cap times the phase's max_tokens, PER ITERATION, and the block
	// accumulates one of those per self_iterate and is re-sent on every round
	// of both phases that follow, the executor and the reviewer. That product,
	// not the single value, is what a bound has to answer.
	//
	// 4000 holds a full draft. Past it the text is rewritten with its
	// deliverable kept in full and the reasoning before it compressed
	// hardest — which is what the old tail cut was approximating, without
	// the risk of cutting the deliverable's own opening away.
	RenderedArtifactLimit = 4000
)

func itoa(n int) string { return strconv.Itoa(n) }
