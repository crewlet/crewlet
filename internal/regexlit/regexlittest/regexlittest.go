// Package regexlittest writes what a property test of a regular
// expression's derivation needs: expressions built from the constructs
// regexlit has a rule for, and strings an expression MATCHES, made by walking
// its own syntax tree.
//
// MATCHES BY CONSTRUCTION, because random text almost never matches a
// pattern: a property such as "the prefilter admits every text the pattern
// matches" checked over random text holds vacuously, while a generator that
// writes a string from the parsed expression — a member of a folded rune's
// case orbit, a rune of a class, a branch, a count — produces a match nearly
// every time, and the property is exercised where it can fail. A caller
// still asks the expression whether it matched, since a zero-width assertion
// the surrounding text breaks (`\b`, `^`) can undo one.
//
// ONE GENERATOR for every package that certifies a derivation of the same
// expressions — regexlit's prefilter first among them — because two copies
// of it drift, and a copy that stopped writing the long s or a byte that is
// not UTF-8 would leave its suite passing without ever meeting the case it
// was written for (ADR-0008). Each caller keeps its own [Vocabulary]: what
// its derivation finds hard is its own.
package regexlittest

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Vocabulary is what [Vocabulary.Pattern] writes an expression from.
type Vocabulary struct {
	// Flags are what an expression may open with, one chosen uniformly; ""
	// is none. Empty, an expression opens with no flags and no choice is
	// made.
	Flags []string
	// Words are literal runs, each quoted into the expression as it is.
	Words []string
	// Classes are character classes and escapes, written as they are.
	Classes []string
}

// Pattern writes an expression from v's constructs, nested to depth: a
// sequence of words, classes, anchors and groups — a capture, a case-folded
// group, alternations of two and three — each perhaps repeated.
func (v Vocabulary) Pattern(rng *rand.Rand, depth int) string {
	var b strings.Builder
	if len(v.Flags) > 0 {
		b.WriteString(v.Flags[rng.IntN(len(v.Flags))])
	}
	b.WriteString(v.sequence(rng, depth))
	return b.String()
}

func (v Vocabulary) sequence(rng *rand.Rand, depth int) string {
	var b strings.Builder
	for range 1 + rng.IntN(4) {
		b.WriteString(v.atom(rng, depth))
		switch rng.IntN(10) {
		case 0:
			b.WriteString("*")
		case 1:
			b.WriteString("+")
		case 2:
			b.WriteString("?")
		case 3:
			fmt.Fprintf(&b, "{%d}", rng.IntN(3))
		case 4:
			lo := rng.IntN(3)
			fmt.Fprintf(&b, "{%d,%d}", lo, lo+rng.IntN(3))
		}
	}
	return b.String()
}

func (v Vocabulary) atom(rng *rand.Rand, depth int) string {
	choice := rng.IntN(10)
	if depth <= 0 && choice >= 6 {
		choice = rng.IntN(6)
	}
	switch choice {
	case 0, 1, 2:
		return regexp.QuoteMeta(v.Words[rng.IntN(len(v.Words))])
	case 3, 4:
		return v.Classes[rng.IntN(len(v.Classes))]
	case 5:
		return []string{`\b`, `\B`, "^", "$"}[rng.IntN(4)]
	case 6:
		return "(" + v.sequence(rng, depth-1) + ")"
	case 7:
		return "(?i:" + v.sequence(rng, depth-1) + ")"
	case 8:
		return "(?:" + v.sequence(rng, depth-1) + "|" + v.sequence(rng, depth-1) + ")"
	}
	return "(?:" + v.sequence(rng, depth-1) + "|" + v.sequence(rng, depth-1) + "|" +
		v.sequence(rng, depth-1) + ")"
}

// Generate writes to b a string re matches, choosing at random wherever re
// allows a choice: a member of a folded rune's case orbit, a rune of a class
// (or a byte that is not UTF-8, where the class holds U+FFFD), a branch, a
// count. A zero-width assertion writes nothing, so the context b is written
// into can still keep it from matching there.
func Generate(rng *rand.Rand, re *syntax.Regexp, b *strings.Builder) {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 {
				orbit := Orbit(r)
				r = orbit[rng.IntN(len(orbit))]
			}
			writeRune(rng, b, r)
		}
	case syntax.OpCharClass:
		pairs := len(re.Rune) / 2
		if pairs == 0 {
			return
		}
		i := rng.IntN(pairs)
		lo, hi := re.Rune[2*i], re.Rune[2*i+1]
		span := int(min(hi-lo, 300))
		writeRune(rng, b, lo+rune(rng.IntN(span+1)))
	case syntax.OpAnyCharNotNL:
		writeRune(rng, b, []rune{'q', 'ſ', '\U0000212A', utf8.RuneError}[rng.IntN(4)])
	case syntax.OpAnyChar:
		writeRune(rng, b, []rune{'q', '\n', 'ſ', utf8.RuneError}[rng.IntN(4)])
	case syntax.OpCapture:
		Generate(rng, re.Sub[0], b)
	case syntax.OpStar:
		for range rng.IntN(3) {
			Generate(rng, re.Sub[0], b)
		}
	case syntax.OpPlus:
		for range 1 + rng.IntN(2) {
			Generate(rng, re.Sub[0], b)
		}
	case syntax.OpQuest:
		if rng.IntN(2) == 0 {
			Generate(rng, re.Sub[0], b)
		}
	case syntax.OpRepeat:
		n := re.Min
		if re.Max > re.Min {
			n += rng.IntN(re.Max - re.Min + 1)
		}
		for range n {
			Generate(rng, re.Sub[0], b)
		}
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			Generate(rng, sub, b)
		}
	case syntax.OpAlternate:
		Generate(rng, re.Sub[rng.IntN(len(re.Sub))], b)
	}
}

// writeRune writes r — and for U+FFFD, which a pattern matches against any
// byte that is not UTF-8, sometimes such a byte instead.
func writeRune(rng *rand.Rand, b *strings.Builder, r rune) {
	if r == utf8.RuneError && rng.IntN(2) == 0 {
		b.WriteByte(0xff)
		return
	}
	b.WriteRune(r)
}

// Orbit is every rune case-folded together with r, r included: what a (?i)
// literal r matches.
func Orbit(r rune) []rune {
	out := []rune{r}
	for m := unicode.SimpleFold(r); m != r; m = unicode.SimpleFold(m) {
		out = append(out, m)
	}
	return out
}
