// Package regexlit is what a regular expression's matches cannot do
// without: the literal substrings every match contains, read off the
// expression's own syntax tree, so that a scan may skip a text holding none
// of them without running the expression, and lose no match by it.
//
// # Why a scan needs one
//
// Go's regexp has one fast path, an IndexString jump to the expression's
// literal PREFIX, and a gate's pattern almost never has one: `\bADR-`,
// `(?m)^func`, `(?i)…` and an alternation all leave the prefix empty, so the
// matcher steps through every byte — about a megabyte a second under the race
// detector, measured over this tree's thirty-six megabytes of Go. A substring
// search is SIMD and does not care. So a gate tests a text with [Required]'s
// [Prefilter] first and runs its expression only on what passes, and the
// verdict is the same set of matches, because what is skipped provably holds
// none. A search over more texts than it can afford to read looks the same
// substrings up in an index instead ([Prefilter.Terms]), and confirms what
// the index admits with the expression.
//
// # Why it is DERIVED, and derived here
//
// A hand-written list of "the literals this pattern needs" beside the pattern
// is a second copy that drifts, and the drift is silent in the worst
// direction: a prefilter that rejects a text its expression would have matched
// is a gate that has stopped guarding while reporting a clean tree. The
// withdrawn-vocabulary gate had exactly such a hand-rolled core, and it was
// unsound — it lowered the text with strings.ToLower, which leaves U+017F (ſ)
// as it is, while `(?i)s` matches it. So the clauses are read off the
// expression's own syntax tree by ONE implementation every caller shares,
// whose soundness is argued once, below, and certified by its own suite.
//
// # Why it is a package of its own
//
// It was written inside [internal/sourcetree], beside the walk the gates read
// this tree with, when every caller was a gate. The code host's code search
// is a caller outside the tests — it plans a pattern over an index of each
// file's substrings, and a plan looser than these rules would miss a file
// the pattern matches — and sourcetree is the gates' package: it imports
// package testing, and what it finds, this module's own source located from
// its compiled-in path, does not exist where the engine runs, so no
// production package imports it. A second derivation written for that
// caller is the copy the paragraph above argues against, and the one
// ADR-0008 forbids; so the derivation moved here whole, and this package
// imports the standard library alone.
//
// # Why it is sound
//
// Each node of the parsed, simplified expression yields the EXACT set of
// strings it matches when that set is small, and otherwise clauses every one
// of its matches satisfies — possibly none, which admits everything:
//
//   - a literal is its own exact string; under (?i) it is compared folded
//     (see [foldedRune]), and a rune that cannot be compared that way — or
//     U+FFFD, which in a pattern also matches any byte that is not UTF-8 —
//     cuts it, and its longest piece is required;
//   - a character class of at most [maxClass] runes is the exact set of them;
//   - a concatenation's match is its children's matches end to end, so it
//     satisfies every child's clauses, and a run of adjacent exact children
//     is the cross product of their sets (up to [maxExact] strings), folded
//     where any part of it is (see [cross]);
//   - an alternation's match is one branch's, so it satisfies the UNION of
//     one clause from each branch — and nothing, if any branch has none;
//   - a capture is its child, and one-or-more satisfies its child's clauses;
//     zero-or-more, an optional, an anchor and a word boundary can each match
//     the empty string, so they require nothing.
//
// Anything else — a wide class, any character, a node this list does not
// name — requires nothing and so admits everything, which is never wrong,
// only slower. That is the whole rule: the derivation may admit too much and
// never too little.
//
// A text skipped this way is never parsed or examined, so a gate that also
// reported a file it could not read loses that report for the files it now
// skips. The gates read source the compiler has already read, and none
// relied on it.
package regexlit
