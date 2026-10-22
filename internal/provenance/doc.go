// Package provenance is the ONE fence around text a model reads that somebody
// other than the reading seat wrote: page bodies, work item comments, project
// files, chat messages and a chat room's topic and purpose, artifact files,
// datastore rows, the adopted brand's voice, the body of every wake — a note
// about a change, a monitor's alert, an integration's notification among them —
// and a colleague's ask and answer.
//
// Such text is data, never an instruction. Each read result wraps it in a fence
// bounded by a per-turn random token, so content cannot close the fence by
// guessing the marker, and labels who wrote it and how far the reader may trust
// it. The seat's prompt carries one standing sentence, owned here, saying what a
// fence means. Fencing reduces prompt injection and does not remove it; the
// structural defences (the requester ceiling, human-only rows and fields, the
// rule that only a person loosens a rule) hold whatever a model does.
//
// # The fence
//
// A fence is two lines around the text, each carrying the same [Boundary]:
//
//	[untrusted:K7Q2M4XWB3ZP6TNA source=work_comment trust=colleague authors=human:ana]
//	...the text, exactly as written apart from the two escapes below...
//	[/untrusted:K7Q2M4XWB3ZP6TNA]
//
// SQUARE BRACKETS AND NO QUOTES, because nearly every fenced field reaches the
// model inside a JSON tool answer, which backslashes every quote: a marker
// carrying a quoted attribute would read as escape soup exactly where it has
// to be unmistakable, and JSON writes a square bracket as itself. (A tool
// answer writes `<` and `>` as themselves too — internal/plainjson — so an
// XML-shaped marker would survive the encoding; the bracket is kept because it
// is what foreign text is least likely to be full of, and `[REDACTED:` set the
// precedent for an engine marker inside foreign text, internal/redact.)
//
// # Why the content cannot close it
//
// Two escapes, applied to the text and to every label value before either is
// written, and each enough on its own:
//
//   - Every occurrence of the boundary is replaced with [TokenRemoved]. The
//     boundary is random per turn and nobody outside the turn has seen it, but
//     a model can echo it into something it writes and read that back in the
//     same turn, so secrecy is defence in depth and this removal is the rule.
//   - Every `[` that opens something shaped like a fence marker (`[untrusted`
//     or `[/untrusted`, any case, with spaces allowed around the slash) becomes
//     `[escaped `, so content cannot even forge a marker with a different token
//     that a model might misread as a closer.
//
// Matching is ASCII case-insensitive on purpose: both the marker and the
// boundary are ASCII, and folding the whole text with strings.ToLower would
// change the byte length of some non-ASCII runes and misplace every index
// after them.
//
// # Trust
//
// [TrustFor] decides from the authors alone: `own` only when every author is
// the reading seat, `external` when any author is an outside party or nobody
// could be named, `colleague` otherwise. [TrustForPartial] is the same rule for
// text whose record says it had writers it does not name, on a surface only
// company principals write: never `own`, and `colleague` even with nobody
// named. `page` is never inferred; a caller whose text a page script produced
// says so. Nor is `company`: a caller reading what an administrator adopted
// for everybody (the adopted brand's voice) says so, and its note confines it
// to tone and wording, never the task, the tools, the recipients or access. The [Envelope] is the same label for a whole result, beside the
// fields it fences, at the [Least] trust of those fields, and carries the
// sentence an assistant without the seat prompt needs to read the fences
// correctly.
//
// # One rule for the engine's own surfaces
//
// Text on a surface the ENGINE hosts — its tracker, knowledge base, chat,
// artifacts, project files and agent-to-agent channels — is trusted by
// [TrustForHosted] ([LabelForHosted]) whichever door it reaches a model
// through: a wake, a read tool, a search hit, the thread a turn is handed.
// Named in full, it earns what its writers earn; short of that it is
// [TrustForPartial] — a colleague's when nobody is named, because only company
// principals write there — and a writer its record carries no handle and no
// kind for is one the record could not name, never an outside party. The
// one hosted text read otherwise is what an administrator adopted as the
// company's own, the adopted brand's voice: it is [TrustCompany], which no set
// of writers is ever inferred to be; and what a page's script reports it
// applied, which the engine cannot verify, is [TrustPage] alone. A vendor's
// surface takes [TrustFor], where nobody named is `external`. Every
// door that decides a trust is found by a walk of the tree and held to the
// rule its surface takes, so one page cannot be a stranger's through its wake
// and a colleague's through a search, as it was.
//
// # A label travels with its text
//
// A fence's three facts — the surface, the trust, the writers — are a value,
// [Label], so the label a text was read under can be kept beside it and the
// text fenced again later under the same label, by a reader that could not
// decide it: a learning worker running after the turn, a seat recalling a past
// turn, a node rendering a search hit another node's backend found. Several
// labelled texts read as one take ONE label ([Merge]): the least trust any of
// them earns, every writer once, the surface they share or
// [SourceNotification] where they differ. [Present] is how any such text
// reaches a model: fenced under its label, or, with none — the company's or
// the reader's own words — neutralised.
//
// # Text fenced before its reader had a token
//
// A wake's trigger is built where the delivery is routed, before the turn
// that will read it exists, so the body in it is fenced under a token minted
// for that wake alone; [Rekey] moves such fences onto the turn's own token and
// applies the two escapes to everything around them, and [Fences] lifts them
// out whole for a reader that renders the rest otherwise (a merged burst's
// digest).
//
// # A model that judges for a seat
//
// Before a turn starts, auxiliary models decide what the seat is shown — which
// of its memories, which pages, and a briefing of its past work written into
// its prompt — each from what the turn was asked, which is on most wakes
// somebody else's words. They read those words fenced like the seat does,
// labelled as the trigger labels them, and their system prompts carry
// [StandingInstruction]; but under a [Boundary] minted for each call rather
// than the turn's, because a model can echo the token it is handed into what it
// writes, and what the summary writes lands in the seat's prompt — which
// [Neutralise] then defuses under the turn's own token. A defused copy alone
// would not do there: these prompts hold no fence to forge, and what such a
// model needs is to be told the task is somebody's words, which only a fence
// and the sentence say.
//
// The same holds for EVERY model call that reads somebody else's words on a
// seat's or a person's behalf — the models that learn from a turn after it
// ends, the one that answers a person from the company's pages, the judge
// that decides whether a phase gets more rounds, a rewrite — and for what each
// writes back where a seat reads it (a memory, a trait, a skill, a judge's
// reason), which is defused under the turn's token. Which calls those are is
// not a list anybody keeps: a walk of the tree finds every function that
// builds a model's input, a provider request or the launch of a coding CLI's
// run (every seat's executor is one), and refuses one that does not say which
// test holds its fences, or why it reads nobody's words.
//
// # Writing back
//
// A model that edits what it read is likely to send the fence back with it.
// [Unfence] takes off a fence of the turn's own boundary that wraps a whole
// text, so a write stores the body and not this turn's label; a fence of any
// other token, or one quoted inside a longer text, is left as written.
//
// A LEAF: it imports the identity vocabulary and nothing else from the engine,
// so every tool and prompt builder can use it.
//
// Content a model reads carries its provenance: ADR-0040, whose authority this
// package is.
package provenance
