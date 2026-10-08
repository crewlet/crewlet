package sourcetree

import (
	"bytes"
	"fmt"
	"go/token"
	"maps"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// Prefilter is what a regular expression's matches cannot do without: a few
// CLAUSES, each a set of substrings, such that every match contains at least
// one substring of every clause. A text that fails a clause cannot match, so a
// scan may skip it without running the expression.
//
// # Why a gate needs one
//
// Go's regexp has one fast path, an IndexString jump to the expression's
// literal PREFIX, and a gate's pattern almost never has one: `\bADR-`,
// `(?m)^func`, `(?i)…` and an alternation all leave the prefix empty, so the
// matcher steps through every byte — about a megabyte a second under the race
// detector, measured over this tree's thirty-six megabytes of Go. A substring
// search is SIMD and does not care. So a gate tests a text with [Required]'s
// prefilter first and runs its expression only on what passes, and the verdict
// is the same set of matches, because what is skipped provably holds none.
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
// expression's own syntax tree by ONE implementation every gate shares, whose
// soundness is argued once, below, and certified by its own suite.
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
// skips. The gates here read source the compiler has already read, and none
// relied on it.
type Prefilter struct {
	// clauses are checked in order, most selective first; a text must
	// satisfy every one. None — the zero value, or an expression nothing
	// could be derived from — admits every text, the one answer that
	// cannot hide a match.
	clauses []clause
	// folds is whether any clause compares folded, which is what makes a
	// text's lowering and its [escapes] worth looking at.
	folds bool
}

// clause is satisfied by a text holding any one of its substrings: plain ones
// byte for byte, folded ones in the text's Unicode lowering (stored lowered).
type clause struct {
	plain, folded [][]byte
}

// Required derives the [Prefilter] of re.
//
// re must come from [regexp.Compile] or [regexp.MustCompile]: its source is
// parsed again with the flags those use ([syntax.Perl]). One from the POSIX
// constructors parses to the same literals, classes and structure, so the
// derivation holds for it too.
func Required(re *regexp.Regexp) Prefilter {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		// The source compiled once already, so this cannot happen; and
		// if it did, admitting everything is the one answer that cannot
		// hide a match.
		return Prefilter{}
	}
	need := analyze(parsed.Simplify()).need
	slices.SortStableFunc(need, func(a, b []piece) int {
		if better(a, b) {
			return -1
		}
		if better(b, a) {
			return 1
		}
		return 0
	})
	var p Prefilter
	for _, set := range need {
		var c clause
		for _, piece := range set {
			if piece.fold {
				c.folded = append(c.folded, []byte(piece.text))
				p.folds = true
			} else {
				c.plain = append(c.plain, []byte(piece.text))
			}
		}
		p.clauses = append(p.clauses, c)
	}
	return p
}

// Admits reports whether body could hold a match: false only when it fails a
// clause.
func (p Prefilter) Admits(body []byte) bool {
	if p.folds && escapes().in(body) {
		return true
	}
	var lowered []byte
	for _, c := range p.clauses {
		held := false
		for _, s := range c.plain {
			if bytes.Contains(body, s) {
				held = true
				break
			}
		}
		for i := 0; !held && i < len(c.folded); i++ {
			if lowered == nil {
				lowered = bytes.ToLower(body)
			}
			held = bytes.Contains(lowered, c.folded[i])
		}
		if !held {
			return false
		}
	}
	return true
}

// AdmitsString is [Prefilter.Admits] for a string.
func (p Prefilter) AdmitsString(text string) bool {
	if p.folds && escapes().inString(text) {
		return true
	}
	lowered, isLowered := "", false
	for _, c := range p.clauses {
		held := false
		for _, s := range c.plain {
			if strings.Contains(text, string(s)) {
				held = true
				break
			}
		}
		for i := 0; !held && i < len(c.folded); i++ {
			if !isLowered {
				lowered, isLowered = strings.ToLower(text), true
			}
			held = strings.Contains(lowered, string(c.folded[i]))
		}
		if !held {
			return false
		}
	}
	return true
}

// Clauses is what the prefilter looks for, in the order it checks: each
// clause's substrings sorted, a folded one in its lowered form. Nil when it
// admits everything. It is for a gate's controls and messages — a gate that
// wants its scan to stay cheap can assert its pattern still yields something
// to look for.
func (p Prefilter) Clauses() [][]string {
	var out [][]string
	for _, c := range p.clauses {
		var set []string
		for _, s := range c.plain {
			set = append(set, string(s))
		}
		for _, s := range c.folded {
			set = append(set, string(s))
		}
		slices.Sort(set)
		out = append(out, slices.Compact(set))
	}
	return out
}

// String names the prefilter for a failure message.
func (p Prefilter) String() string {
	if len(p.clauses) == 0 {
		return "admits every text"
	}
	var parts []string
	for _, set := range p.Clauses() {
		parts = append(parts, fmt.Sprintf("one of %q", set))
	}
	return "needs " + strings.Join(parts, " and ")
}

const (
	// maxExact bounds an exact set carried through a concatenation or an
	// alternation. Every string in a clause is one more substring search
	// per text, so past a handful the cross product costs more than the
	// extra length buys. It is maxClass squared, so a literal between two
	// of the widest classes enumerated stays one exact set — the price
	// scan's quoted `currency`, three quotes either side, is nine.
	maxExact = maxClass * maxClass

	// maxClass is the widest character class enumerated. A class is one
	// rune wide, so on its own it filters almost nothing; what enumerating
	// it buys is extending the literals on either side of it — `[FS]printf`
	// is two seven-byte strings rather than one of six — and a class wider
	// than four multiplies a set past what that is worth.
	maxClass = 4
)

// piece is one substring a match contains, compared exactly or folded.
type piece struct {
	// text is the substring, lowered when fold is set.
	text string
	fold bool
}

// facts is what one node of an expression says about every string it matches.
type facts struct {
	// exact is every string the node can match, when exactOK.
	exact   []piece
	exactOK bool
	// need is the clauses every match satisfies: each is a set of
	// substrings a match contains one of. None requires nothing.
	need [][]piece
}

// exactly is the facts of a node matching precisely set.
func exactly(set []piece) facts {
	if len(set) == 0 {
		// A node matching nothing at all never reaches a scan's verdict,
		// and requiring nothing is the answer that needs no argument.
		return facts{}
	}
	f := facts{exact: set, exactOK: true}
	if usable(set) {
		f.need = [][]piece{set}
	}
	return f
}

// empty is a node that matches only the empty string: an anchor, a word
// boundary, an empty match.
var empty = []piece{{}}

func analyze(re *syntax.Regexp) facts {
	switch re.Op {
	case syntax.OpLiteral:
		return literal(re.Rune, re.Flags&syntax.FoldCase != 0)
	case syntax.OpCharClass:
		return class(re.Rune)
	case syntax.OpEmptyMatch, syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return exactly(empty)
	case syntax.OpCapture:
		return analyze(re.Sub[0])
	case syntax.OpPlus:
		return facts{need: analyze(re.Sub[0]).need}
	case syntax.OpRepeat:
		// Simplify rewrites every counted repeat; this is here for one it
		// ever leaves. At least Min copies follow one another, and one
		// copy satisfies what the child requires.
		if re.Min < 1 {
			return facts{}
		}
		return facts{need: analyze(re.Sub[0]).need}
	case syntax.OpQuest:
		sub := analyze(re.Sub[0])
		if !sub.exactOK || len(sub.exact)+1 > maxExact {
			return facts{}
		}
		return exactly(dedupe(append(slices.Clone(sub.exact), piece{})))
	case syntax.OpConcat:
		return concat(re.Sub)
	case syntax.OpAlternate:
		return alternate(re.Sub)
	}
	// OpStar, OpAnyChar, OpAnyCharNotNL, OpNoMatch, and anything a later
	// Go adds: nothing is required.
	return facts{}
}

// literal is the facts of a literal run of runes.
//
// A run whose every rune can be compared — exactly, or folded when fold is
// set — is its own exact string. One that cannot is cut at each rune that
// cannot, and the longest piece is required: every match contains every
// piece, so any one of them is a sound requirement.
func literal(runes []rune, fold bool) facts {
	var pieces []string
	var current strings.Builder
	whole := true
	for _, r := range runes {
		text, ok := comparable(r, fold)
		if !ok {
			whole = false
			if current.Len() > 0 {
				pieces = append(pieces, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteString(text)
	}
	if current.Len() > 0 {
		pieces = append(pieces, current.String())
	}
	if whole {
		return exactly([]piece{{text: current.String(), fold: fold}})
	}
	if len(pieces) == 0 {
		return facts{}
	}
	longest := pieces[0]
	for _, p := range pieces[1:] {
		if len(p) > len(longest) {
			longest = p
		}
	}
	return facts{need: [][]piece{{{text: longest, fold: fold}}}}
}

// comparable is how a literal rune is looked for in a text: its UTF-8 as it
// is, or its folded form when fold is set. False for a rune a substring
// search cannot find soundly.
//
// U+FFFD is never comparable: in a pattern it matches the replacement
// character AND any byte that is not UTF-8, which decodes to it, so the bytes
// of U+FFFD are not something every match contains. Nor is a value that is
// not a rune at all, which no text decodes to.
func comparable(r rune, fold bool) (string, bool) {
	if r == utf8.RuneError || !utf8.ValidRune(r) {
		return "", false
	}
	if !fold {
		return string(r), true
	}
	lowered, ok := foldedRune(r)
	if !ok {
		return "", false
	}
	return string(lowered), true
}

// class is the facts of a character class: its runes, when there are few
// enough of them to be worth a search each.
func class(ranges []rune) facts {
	count := 0
	for i := 0; i+1 < len(ranges); i += 2 {
		lo, hi := ranges[i], ranges[i+1]
		if lo <= utf8.RuneError && utf8.RuneError <= hi {
			// It matches every byte that is not UTF-8 (see comparable).
			return facts{}
		}
		count += int(hi-lo) + 1
		if count > maxClass {
			return facts{}
		}
	}
	var set []piece
	for i := 0; i+1 < len(ranges); i += 2 {
		for r := ranges[i]; r <= ranges[i+1]; r++ {
			if !utf8.ValidRune(r) {
				// A surrogate: no text decodes to one, so it adds no
				// way to match.
				continue
			}
			set = append(set, piece{text: string(r)})
		}
	}
	return exactly(set)
}

// concat is the facts of a concatenation.
//
// Its match is its children's matches end to end. So it satisfies every
// child's clauses, and a RUN of adjacent children whose exact sets are known
// is the cross product of those sets — the longer strings a run builds are
// what make a clause selective. If every child is exact and the product stays
// small, the whole is exact.
func concat(subs []*syntax.Regexp) facts {
	var need [][]piece
	keep := func(set []piece) {
		if usable(set) {
			need = append(need, set)
		}
	}
	run, allExact := empty, true
	for _, sub := range subs {
		f := analyze(sub)
		if f.exactOK {
			if product, ok := cross(run, f.exact); ok {
				run = product
				continue
			}
			keep(run)
			run, allExact = f.exact, false
			continue
		}
		keep(run)
		need = append(need, f.need...)
		run, allExact = empty, false
	}
	if allExact {
		return exactly(run)
	}
	keep(run)
	return facts{need: need}
}

// alternate is the facts of an alternation.
//
// Its match is one branch's match, so when every branch is exact the whole is
// their union. Otherwise a match satisfies the clauses of the branch it came
// from — and since which branch is not known, the one clause that can be
// stated is the union of one clause from each, the most selective each has.
func alternate(subs []*syntax.Regexp) facts {
	var exact, union []piece
	exactOK, unionOK := true, true
	for _, sub := range subs {
		f := analyze(sub)
		if exactOK && f.exactOK && len(exact)+len(f.exact) <= maxExact {
			exact = append(exact, f.exact...)
		} else {
			exactOK = false
		}
		if !unionOK || len(f.need) == 0 {
			unionOK = false
			continue
		}
		best := f.need[0]
		for _, set := range f.need[1:] {
			if better(set, best) {
				best = set
			}
		}
		union = append(union, best...)
	}
	switch {
	case exactOK:
		return exactly(dedupe(exact))
	case unionOK:
		return facts{need: [][]piece{dedupe(union)}}
	}
	return facts{}
}

// cross is every string of a followed by one of b, or false when the product
// is too large.
//
// An exact string joined to a folded one is compared folded: a text holding
// the exact bytes holds their lowering in its own lowering, because both are
// lowered rune by rune with the same function — so the joined string is a
// weaker requirement than the exact half was, never a wrong one. That is what
// lets `\\u20[aA][cC]`, whose class the parser turns into a folded literal,
// stay one exact set.
func cross(a, b []piece) ([]piece, bool) {
	if len(a)*len(b) > maxExact {
		return nil, false
	}
	out := make([]piece, 0, len(a)*len(b))
	for _, x := range a {
		for _, y := range b {
			switch {
			case x.text == "":
				out = append(out, y)
			case y.text == "":
				out = append(out, x)
			case x.fold == y.fold:
				out = append(out, piece{text: x.text + y.text, fold: x.fold})
			default:
				out = append(out, piece{text: lowered(x) + lowered(y), fold: true})
			}
		}
	}
	return dedupe(out), true
}

// lowered is a piece in the form a folded comparison looks for it.
func lowered(p piece) string {
	if p.fold {
		return p.text
	}
	return strings.ToLower(p.text)
}

// usable reports whether set can be a clause at all: non-empty, and without
// the empty string, which every text contains.
func usable(set []piece) bool {
	return len(set) > 0 && !slices.ContainsFunc(set, func(p piece) bool { return p.text == "" })
}

// better prefers the set whose SHORTEST string is longest — the one a text is
// least likely to hold by chance — and, between equals, the one with fewer
// strings, which is fewer searches.
func better(a, b []piece) bool {
	la, lb := shortest(a), shortest(b)
	if la != lb {
		return la > lb
	}
	return len(a) < len(b)
}

func shortest(set []piece) int {
	n := -1
	for _, p := range set {
		if n < 0 || len(p.text) < n {
			n = len(p.text)
		}
	}
	return n
}

func dedupe(set []piece) []piece {
	seen := map[piece]bool{}
	out := make([]piece, 0, len(set))
	for _, p := range set {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// foldedRune is the form a (?i) literal rune is looked for in: the Unicode
// lowering of the text it is searched in, which is what [Prefilter.Admits]
// compares a folded substring against.
//
// The regexp matches a folded rune r against every rune in r's case-folding
// ORBIT (unicode.SimpleFold's cycle), so lowering is sound for r when every
// member of the orbit lowers to one value. Two kinds of orbit do not:
//
//   - one holding an ASCII letter whose other members do not all lower to
//     that letter's lowering. That is `s`, `S` and U+017F (ſ), which lowers
//     to itself. Those members are the [escapes]: a text holding one is
//     admitted whatever else it says, and r is looked for as the ASCII
//     letter's lowering;
//   - any other orbit whose members lower to different values — σ, ς and Σ,
//     for one. Its runes are not comparable, and a literal is cut at them.
//
// Both are derived from the tables the regexp package folds with, so the rule
// holds for whatever Unicode version this Go carries.
func foldedRune(r rune) (rune, bool) {
	lowered := unicode.ToLower(r)
	consistent := true
	var ascii rune = -1
	for m := unicode.SimpleFold(r); ; m = unicode.SimpleFold(m) {
		if m < utf8.RuneSelf && ascii < 0 {
			ascii = m
		}
		if unicode.ToLower(m) != lowered {
			consistent = false
		}
		if m == r {
			break
		}
	}
	switch {
	case consistent:
		return lowered, true
	case ascii >= 0:
		// Every member that does not lower to the ASCII letter's
		// lowering is an escape, by escapes' own definition.
		return unicode.ToLower(ascii), true
	}
	return 0, false
}

// escapeSet is the runes a folded comparison cannot see through: a text
// holding one is admitted outright.
type escapeSet [][]byte

func (e escapeSet) in(body []byte) bool {
	for _, s := range e {
		if bytes.Contains(body, s) {
			return true
		}
	}
	return false
}

func (e escapeSet) inString(text string) bool {
	for _, s := range e {
		if strings.Contains(text, string(s)) {
			return true
		}
	}
	return false
}

// escapes is every rune that is folded together with an ASCII letter and
// does not lower to that letter's lowering — derived from the Unicode tables
// rather than listed, so a table that grew one more is covered. Today it is
// U+017F alone; U+212A, the Kelvin sign, folds with `k` and lowers to it.
var escapes = sync.OnceValue(func() escapeSet {
	found := map[rune]bool{}
	for c := rune(0); c < utf8.RuneSelf; c++ {
		lowered := unicode.ToLower(c)
		for m := unicode.SimpleFold(c); m != c; m = unicode.SimpleFold(m) {
			if m >= utf8.RuneSelf && unicode.ToLower(m) != lowered {
				found[m] = true
			}
		}
	}
	var out escapeSet
	for _, m := range slices.Sorted(maps.Keys(found)) {
		out = append(out, []byte(string(m)))
	}
	return out
})

// LineLocal reports whether re finds, in any text, exactly the matches it
// finds in each of the text's lines on its own — so a gate may run it only on
// the lines its [Prefilter] admits, rather than over every byte of a file
// that holds one admitted line among thousands.
//
// That holds when no match can cross a line and no assertion reads a line's
// edge differently from the text's: nothing in re matches a newline (a
// literal or class holding one, or any-character under (?s)), and re does not
// anchor at the start or end of the TEXT, which a line would turn into its
// own start or end. A line's edges are where (?m)^ and $ match anyway, and a
// word boundary sees a newline and a text's edge alike, as a non-word.
func LineLocal(re *regexp.Regexp) bool {
	parsed, err := syntax.Parse(re.String(), syntax.Perl)
	if err != nil {
		return false
	}
	return lineLocal(parsed.Simplify())
}

func lineLocal(re *syntax.Regexp) bool {
	switch re.Op {
	case syntax.OpBeginText, syntax.OpEndText, syntax.OpAnyChar:
		return false
	case syntax.OpLiteral:
		// A newline folds to nothing but itself, so (?i) changes nothing.
		return !slices.Contains(re.Rune, '\n')
	case syntax.OpCharClass:
		for i := 0; i+1 < len(re.Rune); i += 2 {
			if re.Rune[i] <= '\n' && '\n' <= re.Rune[i+1] {
				return false
			}
		}
		return true
	}
	for _, sub := range re.Sub {
		if !lineLocal(sub) {
			return false
		}
	}
	return true
}

// Identifiers is a prefilter for Go source on the identifiers a gate's
// matcher compares: a file whose bytes spell none of them cannot hold the
// construct, so a gate need not parse it.
//
// EXACT, not a heuristic, because Go has no escapes in an identifier: a name
// in the source is the bytes of the name, whether it is a selector, a callee,
// a composite literal's type or key, or a declaration. A STRING is not — an
// import path or a literal's value can be spelled with escapes the bytes
// never show — which is why a gate keyed on an import parses the imports
// instead. So a gate that hands this the same table its matcher reads skips
// exactly the files it would have parsed and found nothing in.
type Identifiers struct {
	names [][]byte
}

// MustIdentifiers is the [Identifiers] prefilter for names, each of which
// must be a Go identifier: a prefilter on anything else is not exact, and a
// gate built on one would skip a file its matcher would flag. It panics, as
// [regexp.MustCompile] does, on a name that is not one, or on no names.
func MustIdentifiers(names ...string) Identifiers {
	if len(names) == 0 {
		panic("sourcetree: an Identifiers prefilter of no names is one a " +
			"gate's table emptied out from under it — name what the matcher compares")
	}
	var ids Identifiers
	for _, name := range names {
		if !token.IsIdentifier(name) {
			panic(fmt.Sprintf("sourcetree: %q is not a Go identifier, so a "+
				"prefilter on its bytes is not exact", name))
		}
		ids.names = append(ids.names, []byte(name))
	}
	return ids
}

// In reports whether src spells any of the identifiers. The zero value names
// none and admits every file, the one answer that cannot hide a construct.
func (ids Identifiers) In(src []byte) bool {
	if len(ids.names) == 0 {
		return true
	}
	for _, name := range ids.names {
		if bytes.Contains(src, name) {
			return true
		}
	}
	return false
}
