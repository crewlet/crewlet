// Package provenance is the ONE fence around text a model reads that somebody
// other than the reading seat wrote: page bodies, work item comments, project
// files, chat messages and a chat room's topic and purpose, artifact files,
// datastore rows, the adopted brand's voice, and the body of every wake — a
// note about a change, a monitor's alert, an integration's notification among
// them.
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
// # Text fenced before its reader had a token
//
// A wake's trigger is built where the delivery is routed, before the turn
// that will read it exists, so the body in it is fenced under a token minted
// for that wake alone; [Rekey] moves such fences onto the turn's own token and
// applies the two escapes to everything around them, and [Fences] lifts them
// out whole for a reader that renders the rest otherwise (a merged burst's
// digest).
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
