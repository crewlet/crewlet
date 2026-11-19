// Package textcut shortens a string without breaking it.
//
// It replaced four helpers — three named truncate, in internal/agent/extension,
// internal/learning and internal/providers/llm/harness, plus internal/learning's
// preview — which were four copies of one rule. They already agreed on the hard
// part, walk back to a rune boundary, and disagreed on the easy one: two
// appended "…" and two appended "...", so the same cut read differently
// depending on which subsystem made it. Copies that agree today are copies
// that can stop agreeing. A fifth, the sandbox runner's private tail cut and
// the only one that kept the END, became [Tail] — and so did a sixth, the
// in-flight progress frame's own end-of-round cut in internal/agent/runner,
// the same rule written out again under the name of the bound it served.
//
// The rule they each re-derive is this: a plain s[:n] splits whatever
// multi-byte character straddles the boundary and yields invalid UTF-8. What
// that produces depends on where it goes — a JSON encoder substitutes U+FFFD,
// a model reads a replacement character, a terminal prints a box — so a
// byte-cutting version is not "faster", it is wrong in a way that only appears
// once the input stops being ASCII, which in a company's traffic is a matter
// of when rather than whether.
//
// # Cutting is the last resort, not the first
//
// Most of what this package once shortened is no longer shortened at all, and
// that is the better fix wherever it is available: content a turn reasons over
// is passed whole, a value with a vendor limit is REFUSED with a message
// naming the field rather than silently cut to fit, and text that genuinely
// has to fit a budget a model reads is REWRITTEN to fit by
// [github.com/crewlet/crewlet/internal/compact] — never cut by this package.
// What is left here is the cases where no reader takes the text for the whole
// of something: a diagnostic or a process's tail past a transport ceiling, a
// marked preview of what the reader can open (a search snippet, an inbox
// excerpt, a session's transcript entry), an identifier with a length limit — which keeps a digest of the
// whole beside the cut, so two that shorten alike still differ — and
// EMBEDDING INPUT, which no reader sees at all: its bound is the embedding
// model's, its rule is internal/providers/embeddings (Opening for a caller
// that embeds an opening, EmbedWhole for one that represents the whole text),
// and it is unmarked because a marker there would be a token in the vector
// rather than a note about one. Every cut is one of those, and a cut that is
// none of them is the bug this package was written to remove.
//
// Budgets are BYTES, never runes: every bound here is a size something else
// enforces. Text a model or a person acts on is never cut here; where a bound
// is genuine it is rewritten to fit, refused naming the limit, or described by
// its size.
package textcut

import "unicode/utf8"

// marker is what every cut here that says so is marked with, spelled once
// ([Ellipsis] says why it is this spelling).
const marker = "…"

// Ellipsis is the common case: at most max bytes of CONTENT, cut on a rune
// boundary, with a marker where it was cut.
//
// Where the budget is a ceiling something enforces, use [Within]. The marker
// keeps a reader from mistaking the remainder for the whole (a severed tool
// argument) and is NOT counted against max: a caller needing the total bounded
// passes a smaller max. ONE SPELLING, "…" rather than "...": one rune where the
// other is three, and every caller renders into a budget.
func Ellipsis(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return Bytes(s, max) + marker
}

// Within is at most max bytes INCLUDING the marker, cut on a rune boundary.
//
// [Ellipsis] does not count its marker against max, right where the budget is
// advisory and wrong where it is a CEILING somebody enforces: a tracker
// notification's excerpt is refused at MaxExcerpt by
// [github.com/crewlet/crewlet/internal/worklet.Notify.Validate], so a marker
// pushing a cut excerpt three bytes over would turn every long comment into a
// REFUSED WRITE rather than a marked one.
//
// A max too small to hold the marker yields the marker ALONE, not the empty
// string (it falls out of [Bytes]): a reader shown nothing cannot tell a cut
// from an empty value. It is the one case where the result exceeds max.
func Within(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return Bytes(s, max-len(marker)) + marker
}

// Opening is s — the opening of a text it was already kept short of — at most
// max bytes INCLUDING the marker, cut on a rune boundary, and marked whether or
// not it had to be cut further to fit.
//
// [Within] marks only what it cuts itself, which is right for a value it is
// handed whole. It is wrong for an opening a caller kept short for a reason
// of its own — the part of a text a redaction could settle, say — which can be
// well inside max and is still not the whole: unmarked, a reader takes it for
// all there was. The same two rules hold as for Within: the marker is inside
// the budget, and a max too small to hold it yields the marker alone.
func Opening(s string, max int) string {
	return Bytes(s, max-len(marker)) + marker
}

// Bytes is at most max bytes, cut on a rune boundary, with no marker.
//
// For the callers where the value is consumed by something that does not read
// prose — a fixed-width field, a fingerprint input — and an appended character
// would be part of the value rather than a note about it.
func Bytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	// Walk back to the start of the rune that straddles the cut. At most
	// three steps: a UTF-8 encoding is four bytes at its longest.
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

// Tail is the LAST at most max bytes of s, starting on a rune boundary, with a
// marker in front where it was cut.
//
// The mirror of [Ellipsis], for text whose useful end is the end: a process's
// own account of itself watched live (the last KiB of a running executor's
// activity log or stderr) and the end of a line too long to keep whole, where a
// process says what went wrong. It is never how a whole log is bounded for a
// record: that keeps whole lines from both ends and counts the middle, because
// a log's opening is its plan. The same two rules hold: the marker is not
// counted against max, and the cut never lands inside a rune (a slice taken
// from the end begins mid-rune on non-ASCII text, which a JSON encoder turns
// into U+FFFD). The budget is BYTES, not runes: the bound is an event's size.
func Tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 0 {
		return marker
	}
	// Walk FORWARD to the start of the rune that straddles the cut. At most
	// three steps, for the reason [Bytes] gives.
	start := len(s) - max
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return marker + s[start:]
}
