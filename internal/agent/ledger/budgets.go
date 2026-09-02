package ledger

import "strconv"

// Elision budgets, applied ONLY to the cross-round and cross-turn ledgers —
// never to Review's single-iteration evidence log, which stays verbatim by
// contract.
//
// ONE principle decides every number: ELIDE PAYLOADS, NEVER STRUCTURE. A
// payload (a message body, page HTML, a diff) is unbounded, is re-authored
// from the plan next round, and can never answer the ledger's question — so
// carrying it only buries the two lines that can. Structure (the plan's steps,
// the draft under review, the reviewer's correction) is bounded in practice
// and is exactly what the next round has to act on, so it is cut only as a
// guard against pathological output, never as routine trimming.
//
// Every one of these is a GUARD, not a diet. Prompt caching keys on the
// system+tools prefix, which the ledger never touches, so a larger block costs
// little; the reason to bound it at all is that an unbounded block eats the
// turn's own token budget and buries the signal.
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

	// IntentLimit caps the round's own account of what it set out to do.
	// A realistic multi-step account renders ~850 runes, so 1200 covers
	// ~8 steps: past anything an executor writes, while still bounding a
	// runaway one. Cutting lower chopped a routine account mid-step, which
	// reads as a SHORTER intent rather than a truncated one — worse than
	// omitting it.
	IntentLimit = 1200

	// ArtifactLimit caps the draft Review judged. The next round has to
	// improve that draft, so anything Review had enough of to judge, Plan
	// needs enough of to extend rather than rewrite from scratch. Review's
	// own summary of the same content uses the same number.
	ArtifactLimit = 2000

	// NoteLimit caps reviewer-authored prose. The correction is what the
	// next round acts on and the ledger is its only carrier, so trimming it
	// mid-instruction would lose engine-critical content. A pathological-
	// output guard for a verbose model, not a routine trim — the ask is one
	// or two sentences.
	NoteLimit = 2000

	// MaxReadCalls caps rendered tool-call lines per phase per round. Only
	// positively-known READS are ever dropped to fit: a write is the whole
	// reason the ledger exists and is never omitted, however many there are.
	// Execute's base round cap is 20 and its extension ceiling 40, so a busy
	// round can log dozens of calls; 12 covers the recon a normal round does
	// while keeping one block skimmable.
	MaxReadCalls = 12

	// TriggerLimit caps what triggered the turn: sender plus the opening of
	// what they said. Enough to recognise the message in a thread, never the
	// whole body — the body is re-readable on the surface it came from, and
	// the point of the entry is what the SEAT did about it.
	TriggerLimit = 400

	// ReplyLimit caps the reply the seat actually sent. Shares the artifact
	// budget: it is the same content Review judged, answering the same
	// question.
	ReplyLimit = ArtifactLimit
)

func itoa(n int) string { return strconv.Itoa(n) }
