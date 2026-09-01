package ledger

import "strconv"

// Elision budgets, applied ONLY to the cross-round and cross-turn ledgers —
// never to Review's single-iteration evidence log, which stays verbatim by
// contract.
//
// ONE principle decides every number: ELIDE PAYLOADS, NEVER STRUCTURE. A
// payload (a message body, page HTML, a diff) is a tool ARGUMENT: unbounded,
// re-authored from the plan next round, and unable to answer the ledger's
// question — so carrying it whole only buries the two lines that can.
//
// Structure is everything else: the plan's steps, the draft under review, the
// reviewer's correction, the trigger, the reply that was sent. That is exactly
// what the next round has to act on, and it is now carried VERBATIM. It used
// not to be — six further limits sat here cutting each of those at 400 to 2000
// runes, which is the principle above applied to the half it excludes. A
// reviewer's correction trimmed mid-instruction loses the engine-critical part
// of the only carrier it has, and the ledger is not re-readable from anywhere:
// unlike a chat message or an issue comment, there is no surface to go back to.
//
// What is left bounds ARGUMENTS and the read-call list, and both say when they
// cut. Prompt caching keys on the system+tools prefix, which the ledger never
// touches, so a larger block costs little.
const (
	// ValueLimit caps an argument VALUE. Identifiers are what say WHICH
	// delivery fired — channel names, issue keys, page ids, handles, thread
	// timestamps, URLs — and a long page URL with query parameters runs to
	// ~180 runes, so 200 keeps the whole discriminator while still cutting
	// message bodies and HTML by an order of magnitude.
	ValueLimit = 200

	// BlobLimit backstops a call carrying many arguments: even with every
	// value elided, ~40 keys is still a wall of text. 800 holds roughly a
	// dozen identifier-shaped arguments — more than any real delivery tool
	// takes. Enforced by dropping whole keys; see fitArguments.
	BlobLimit = 800

	// MaxReadCalls caps rendered tool-call lines per phase per round. Only
	// positively-known READS are ever dropped to fit: a write is the whole
	// reason the ledger exists and is never omitted, however many there are.
	// Execute's base round cap is 20 and its extension ceiling 40, so a busy
	// round can log dozens of calls; 12 covers the recon a normal round does
	// while keeping one block skimmable.
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
	// of all three phases that follow. That product, not the single value, is
	// what a bound has to answer.
	//
	// 4000 runes is twice what the deleted write-time cut allowed and holds a
	// full draft; the TAIL is kept, because a round's deliverable is what it
	// ended with rather than what it opened by thinking. Marked, like every
	// other cut in this package.
	RenderedArtifactLimit = 4000
)

func itoa(n int) string { return strconv.Itoa(n) }
