// Package textcut shortens a string without breaking it.
//
// It replaced four helpers — three named truncate, in internal/agent/extension,
// internal/learning and internal/providers/llm/cliagent, plus internal/learning's
// preview — which were four copies of one rule. They already agreed on the hard
// part, walk back to a rune boundary, and disagreed on the easy one: two
// appended "…" and two appended "...", so the same cut read differently
// depending on which subsystem made it. Copies that agree today are copies
// that can stop agreeing.
//
// The rule they each re-derive is this: a plain s[:n] splits whatever
// multi-byte character straddles the boundary and yields invalid UTF-8. What
// that produces depends on where it goes — a JSON encoder substitutes U+FFFD,
// a model reads a replacement character, a terminal prints a box — so a
// byte-cutting version is not "faster", it is wrong in a way that only appears
// once the input stops being ASCII, which in a company's traffic is a matter
// of when rather than whether.
//
// # The other shared cut, and why it is not this one
//
// [github.com/crewlet/crewlet/internal/agent/ledger.Elide] is also an exported,
// marked, rune-safe head cut, and its doc makes this package's own argument:
// "Two trimming functions would eventually disagree about where a limit falls
// and whether the cut is marked." They are still separate on purpose, because
// they disagree about the two things that matter most at a call site:
//
//   - ITS BUDGET IS RUNES, this one's is BYTES. A ledger budget is a character
//     count an operator configured; a payload cap and a log field are bytes.
//   - A LIMIT OF 0 MEANS UNBOUNDED THERE and empty HERE. Review's
//     single-iteration evidence log depends on 0 leaving the text verbatim,
//     and a payload cap of 0 that returned the whole payload would be the
//     opposite of a cap.
//
// Folding them together would mean one of those two contracts changing
// silently under callers that rely on it, so the honest answer is two
// functions whose docs point at each other rather than one that quietly
// means different things in different packages.
//
// # Cutting is the last resort, not the first
//
// Most of what this package once shortened is no longer shortened at all, and
// that is the better fix wherever it is available: content a turn reasons over
// is passed whole, and a value with a vendor limit is REFUSED with a message
// naming the field rather than silently cut to fit. What is left here is the
// cases where cutting is genuinely right — a diagnostic, a log field, a prompt
// budget — where the alternative to a bounded string is an unbounded one.
package textcut

import "unicode/utf8"

// Ellipsis is the common case: at most max bytes of CONTENT, cut on a rune
// boundary, with a marker where it was cut.
//
// Where the budget is a ceiling something enforces rather than a guide, the
// marker has to fit inside it — that is [Within].
//
// The marker matters wherever a reader could mistake the remainder for the
// whole — a severed tool argument read as a different argument, an error whose
// second half named the cause. It is NOT counted against max: the cap bounds
// the content, and a caller that needs the total bounded should pass a smaller
// max.
//
// ONE SPELLING of the marker, "…" rather than "...", because it is one rune
// where the other is three and every caller here renders into a budget.
func Ellipsis(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return Bytes(s, max) + "…"
}

// Within is at most max bytes INCLUDING the marker, cut on a rune boundary.
//
// [Ellipsis] deliberately does not count its marker against max — "the cap
// bounds the content" — and that is right wherever the budget is advisory. It
// is wrong wherever the budget is a CEILING somebody enforces: a tracker
// notification's excerpt is refused at MaxExcerpt by
// [github.com/crewlet/crewlet/internal/tracker.Notify.Validate], so a marker
// that pushed a cut excerpt three bytes over would turn every long comment
// into a REFUSED WRITE rather than a marked one.
//
// This existed by hand at four call sites before it had a name, each reducing
// the budget and appending "…" itself — which is the shape this package was
// written to end. Two of those four did not reduce the budget at all, so they
// were Ellipsis with extra steps and could exceed their own limit.
//
// A max too small to hold the marker yields the marker ALONE rather than the
// empty string, and that falls out of [Bytes] rather than being guarded for:
// a non-positive budget there is already the empty string, so the marker is
// what is left. It is the right answer either way — something was cut, and a
// reader shown nothing cannot tell that from a value that was empty to begin
// with — but it is worth saying, because it is the one case where the result
// is longer than max.
func Within(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const marker = "…"
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
