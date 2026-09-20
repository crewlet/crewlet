package ledger

import "strconv"

// Elision budgets, applied ONLY to the cross-round and cross-turn ledgers —
// never to Review's single-iteration evidence log, which stays verbatim by
// contract.
//
// ONE principle decides every number: ELIDE PAYLOADS, NEVER STRUCTURE. A
// payload (a message body, page HTML, a diff) is a tool ARGUMENT: unbounded,
// re-authored from scratch next round, and unable to answer the ledger's
// question — so carrying it whole only buries the two lines that can.
//
// Structure is everything else: the round's own account of what it set out to
// do, the draft under review, the reviewer's correction, the trigger, the
// reply that was sent. That is exactly what the next round has to act on, and
// it is now carried VERBATIM. It used not to be: six further limits sat here
// cutting each of those at 400 to 2000 runes, which is the principle above
// applied to the half it excludes. A reviewer's correction trimmed
// mid-instruction loses the engine-critical part of the only carrier it has,
// and the ledger is not re-readable from anywhere: unlike a chat message or an
// issue comment, there is no surface to go back to.
//
// What is left bounds ARGUMENTS and the read-call list, and both say when they
// cut. Prompt caching keys on the system+tools prefix, which the ledger never
// touches, so a larger block costs little.
//
// # Every cut below is now reachable another way
//
// The sentence above — "there is no surface to go back to" — was a statement
// about the ENGINE rather than about the ledger, and it is no longer true.
// `recall_iteration` (internal/agent/builtin) returns one closed round from
// this same record with nothing elided, off [Iteration] itself: the arguments
// are [Call.Args], kept whole, because every cut in this package happens at
// RENDER time. That is what makes these four numbers defensible rather than
// merely small — a budget whose overflow is unrecoverable is a data loss with
// a comment on it.
//
// It is ALSO why none of them shrank when the tool arrived, which is a
// decision and not an omission. Each is set by what the next round needs in
// order to act WITHOUT asking — the discriminating identifier, the draft it is
// revising — and a budget tuned instead to "enough to decide whether to
// recall" would spend a tool round on the common path to save tokens on the
// rare one. The tool is the floor under the cut, not a licence to cut deeper;
// each constant below says which of the two it is answering.
const (
	// ValueLimit caps an argument VALUE. Identifiers are what say WHICH
	// delivery fired — channel names, issue keys, page ids, handles, thread
	// timestamps, URLs — and a long page URL with query parameters runs to
	// ~180 runes, so 200 keeps the whole discriminator while still cutting
	// message bodies and HTML by an order of magnitude.
	//
	// PINNED TO THE LONGEST IDENTIFIER, so `recall_iteration` does not lower
	// it: below ~180 the discriminator itself starts getting cut, and a
	// block that could not say which of two deliveries fired would send the
	// model to the tool on every line it read.
	ValueLimit = 200

	// BlobLimit backstops a call carrying many arguments: even with every
	// value elided, ~40 keys is still a wall of text. 800 holds roughly a
	// dozen identifier-shaped arguments — more than any real delivery tool
	// takes. Enforced by dropping whole keys; see fitArguments.
	//
	// SET BY THE KEY COUNT rather than by payload weight, which is the same
	// reason ValueLimit does not move: it is a ceiling on how many WHOLE
	// identifiers a line carries, and lowering it drops identifiers.
	BlobLimit = 800

	// MaxReadCalls caps rendered tool-call lines per phase per round. Only
	// positively-known READS are ever dropped to fit: a write is the whole
	// reason the ledger exists and is never omitted, however many there are.
	// Execute's base round cap is 20 and its extension ceiling 40, so a busy
	// round can log dozens of calls; 12 covers the recon a normal round does
	// while keeping one block skimmable.
	//
	// THE ONE CUT THAT ALREADY REPORTED ITSELF USEFULLY — the line reads
	// "+N further read call(s) omitted", so the count was actionable before
	// `recall_iteration` existed and the tool only makes the names
	// retrievable too. It does not move for that: 12 is set by the recon a
	// round actually does against a 20-call cap, and the point of showing a
	// read at all is soft (the prompt explicitly permits re-running one).
	MaxReadCalls = 12

	// RenderedArtifactLimit bounds a PRIOR round's produced text as it is
	// RENDERED into the next round's block. The Iteration record keeps it
	// whole; this is a display bound, and that difference is the point of
	// the paragraph above.
	//
	// It needs one because Execution.Text is every assistant message of that
	// round's tool loop concatenated, thinking included — so its size is the
	// round cap times the phase's max_tokens, PER ITERATION, and the block
	// accumulates one of those per self_iterate and is re-sent on every round
	// of both phases that follow, the executor and the reviewer. That product,
	// not the single value, is what a bound has to answer.
	//
	// 4000 runes is twice what the deleted write-time cut allowed and holds a
	// full draft; the TAIL is kept, because a round's deliverable is what it
	// ended with rather than what it opened by thinking. Marked, like every
	// other cut in this package.
	//
	// IT HOLDS A DRAFT, which is why `recall_iteration` does not lower it
	// either — and this is the constant where the temptation is real, since
	// its cost is the limit times the rounds times both phases and it is four
	// times the next largest. The common self_iterate path is "revise what
	// you produced against the reviewer's correction", so the draft is what
	// the NEXT round acts on rather than what it decides whether to fetch.
	// Cutting to a sample would buy those tokens back by spending a tool
	// round on almost every iterating turn. The tool is for the draft that
	// runs past 4000, which is the exception this bound could not serve
	// before and does not have to now.
	RenderedArtifactLimit = 4000
)

func itoa(n int) string { return strconv.Itoa(n) }
