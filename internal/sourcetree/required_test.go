package sourcetree

import (
	"fmt"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// gatePatterns are the expressions the tree's gates scan with, as they were
// written when this suite was: every rule of the derivation is exercised by at
// least one of them, and each has a known answer below. Each gate's OWN
// planted positives also run through the prefilter, which is what certifies
// the pattern it holds today; these certify the derivation.
var gatePatterns = map[string][][]string{
	// internal/adr: a citation.
	`\bADR-(\d{4})\b`: {{"ADR-"}},
	// internal/adr: a test declaration. The capture ends the run, so its
	// "Test" is a clause of its own rather than an extension of "func ".
	`(?m)^func (Test[A-Za-z0-9_]*)\(`: {{"func "}, {"Test"}, {"("}},
	// internal/envfile: a file that writes a .env, and an assignment
	// built by hand in one.
	`\.env\b|envFile|EnvFile|envfile\.`: {{".env", "EnvFile", "envFile", "envfile."}},
	`fmt\.(Sprintf|Fprintf)\([^)]*"[^"]*%s\s*=\s*%[svq]|` + `"export "\s*\+`: {
		{`"export "`, "fmt.Fprintf(", "fmt.Sprintf("},
	},
	// internal/statelog/metrics: a reference to the catalogue.
	`\bmetrics\.([A-Z]\w*)\b`: {{"metrics."}},
	// internal/engine: a payload literal, a guard kind, a read's way in.
	`\btypes\.([A-Z][A-Za-z0-9_]*)\{`:         {{"types."}, {"{"}},
	`\bKind:\s*types\.(Guard[A-Za-z0-9_]+)\b`: {{"types."}, {"Guard"}, {"Kind:"}},
	`(?:\bVia:\s*|knowledgeRead\(turn,\s*)types\.(ReadVia[A-Za-z0-9_]+)\b`: {
		{"ReadVia"}, {"types."}, {"Via:", "knowledgeRead(turn,"},
	},
	// internal/statelog: the withdrawn-vocabulary gate's shape — folded
	// names joined into one alternation, one of them with a class and one
	// an alternation of its own. (Not its real names: this file is in the
	// tree that gate reads.)
	`(?i)(spend_window)|(CREWLET_Q[0-9])|(\bwarm\b\s+read|read\s+level\s+.?warm)`: {
		{"crewlet_q", "level", "spend_window", "warm"},
	},
	// internal/api: three of the price forms.
	`(?i)cost_?usd|priced_?calls`: {{"cost_usd", "costusd", "priced_calls", "pricedcalls"}},
	"[\"'`]currency[\"'`]": {{
		"\"currency\"", "\"currency'", "\"currency`",
		"'currency\"", "'currency'", "'currency`",
		"`currency\"", "`currency'", "`currency`",
	}},
	// A class of a letter's two cases is a folded literal to the parser,
	// so this one is folded throughout — and stays one exact set.
	`[€£]|\\u(?:20[aA][cC]|00[aA]3|\{20[aA][cC]\}|\{[aA]3\})|\\x[aA]3`: {{
		"\x5cu00a3", "\x5cu20ac", `\u{20ac}`, `\u{a3}`, `\xa3`, "£", "€",
	}},
}

// THE DERIVATION FINDS WHAT EACH GATE'S PATTERN CANNOT MATCH WITHOUT.
//
// The known answers are what make the property test below worth anything: a
// derivation that admitted every text would pass it trivially, and these are
// where it would have to say so.
func TestRequiredFindsWhatTheGatesLookFor(t *testing.T) {
	t.Parallel()
	for pattern, want := range gatePatterns {
		got := Required(regexp.MustCompile(pattern)).Clauses()
		if !sameClauses(got, want) {
			t.Errorf("Required(%s) = %q, want %q", pattern, got, want)
		}
	}
}

// sameClauses compares two clause lists as sets of sets, so the check order
// the prefilter picks is not part of the answer.
func sameClauses(got, want [][]string) bool {
	norm := func(clauses [][]string) []string {
		var out []string
		for _, c := range clauses {
			c = slices.Clone(c)
			slices.Sort(c)
			out = append(out, strings.Join(c, "\x00"))
		}
		slices.Sort(out)
		return out
	}
	return slices.Equal(norm(got), norm(want))
}

// NOTHING A PATTERN CAN MATCH IS EVER SKIPPED.
//
// The argument for soundness is in the package doc, rule by rule; this is
// the evidence, over patterns built from every construct the derivation
// handles and strings GENERATED to match them — folded runes from every orbit
// member, the long s and the Kelvin sign among them, and bytes that are not
// UTF-8 wherever the pattern can match one. Random text almost never matches
// a pattern, so a property test over random text would certify nothing; a
// generator walking the pattern's own syntax tree produces matches by
// construction, and the test asserts it did.
//
// A FIXED SEED, so a failure reproduces exactly; its breadth is the count. A
// thousand patterns puts every construct the generator writes into hundreds
// of them, twenty strings each, and stays under two seconds under the race
// detector — three thousand cost five and found nothing a thousand did not.
func TestRequiredAdmitsEveryMatch(t *testing.T) {
	t.Parallel()
	rng := rand.New(rand.NewPCG(20261008, 1))
	var patterns []string
	for pattern := range gatePatterns {
		patterns = append(patterns, pattern)
	}
	slices.Sort(patterns)
	for range 1000 {
		patterns = append(patterns, randomPattern(rng, 3))
	}

	matched, filtered, rejected := 0, 0, 0
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			t.Fatalf("the generator wrote %q, which does not compile: %v", pattern, err)
		}
		parsed, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("parse %q: %v", pattern, err)
		}
		need := Required(re)
		if len(need.Clauses()) > 0 {
			filtered++
		}
		for range 20 {
			var b strings.Builder
			b.WriteString(noise(rng))
			generate(rng, parsed, &b)
			b.WriteString(noise(rng))
			text := b.String()
			if !re.MatchString(text) {
				// A zero-width assertion the context broke; the
				// count below says how often.
				continue
			}
			matched++
			if !need.AdmitsString(text) || !need.Admits([]byte(text)) {
				t.Fatalf("Required(%s) (%v) skips %q, which the pattern matches",
					pattern, need, text)
			}
		}
		// AND IT STILL SKIPS SOMETHING, or the property above is one any
		// prefilter that admitted everything would pass.
		if text := noise(rng); !re.MatchString(text) && !need.AdmitsString(text) {
			rejected++
		}
	}
	if matched < len(patterns)*20/2 {
		t.Fatalf("only %d of %d generated strings matched their pattern, so the "+
			"property was hardly exercised", matched, len(patterns)*20)
	}
	if filtered < len(patterns)/3 || rejected == 0 {
		t.Fatalf("%d of %d patterns yielded a prefilter and %d text was skipped — "+
			"a derivation that admits everything passes the property above "+
			"without being one", filtered, len(patterns), rejected)
	}
	t.Logf("%d patterns, %d with a prefilter; %d generated matches all admitted; "+
		"%d non-matching texts skipped", len(patterns), filtered, matched, rejected)
}

// randomPattern writes an expression from the constructs the derivation has
// a rule for, nested to depth.
func randomPattern(rng *rand.Rand, depth int) string {
	var b strings.Builder
	switch rng.IntN(6) {
	case 0:
		b.WriteString("(?i)")
	case 1:
		b.WriteString("(?s)")
	case 2:
		b.WriteString("(?m)")
	}
	b.WriteString(sequence(rng, depth))
	return b.String()
}

func sequence(rng *rand.Rand, depth int) string {
	var b strings.Builder
	for range 1 + rng.IntN(4) {
		b.WriteString(atom(rng, depth))
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

// words are literal runs, each with a rune whose folding is the hard case: an
// ASCII letter with a non-ASCII partner, a non-ASCII orbit that lowers
// inconsistently, U+FFFD, and punctuation the gates write.
var words = []string{
	"ADR-", "types.", "kind", "class", "Sk", "metrics", "env", "_", ":", "{",
	"ſ", "\U0000212A", "é", "σ", "ς", "Σ", "\U000000B5", "\U0000FFFD", "x\U0000FFFDy", "%s", " ",
}

// classes are character classes and escapes, narrow and wide.
var classes = []string{
	"[abc]", "[a-c]", "[^x]", `\d`, `\w`, `\s`, "[kK]", "[sſ]", `[\x{FFFD}a]`,
	"[FS]", `[A-Za-z0-9_]`, ".", "[σς]",
}

func atom(rng *rand.Rand, depth int) string {
	choice := rng.IntN(10)
	if depth <= 0 && choice >= 6 {
		choice = rng.IntN(6)
	}
	switch choice {
	case 0, 1, 2:
		return regexp.QuoteMeta(words[rng.IntN(len(words))])
	case 3, 4:
		return classes[rng.IntN(len(classes))]
	case 5:
		return []string{`\b`, `\B`, "^", "$"}[rng.IntN(4)]
	case 6:
		return "(" + sequence(rng, depth-1) + ")"
	case 7:
		return "(?i:" + sequence(rng, depth-1) + ")"
	case 8:
		return "(?:" + sequence(rng, depth-1) + "|" + sequence(rng, depth-1) + ")"
	}
	return "(?:" + sequence(rng, depth-1) + "|" + sequence(rng, depth-1) + "|" +
		sequence(rng, depth-1) + ")"
}

// noise is context a match is embedded in: words, separators, a newline, the
// escapes, and a byte that is not UTF-8.
func noise(rng *rand.Rand) string {
	pieces := []string{"a", "Z", " ", "\n", ".", "ſ", "\U0000212A", "\xff", "ADR", "e", "-", "σ"}
	var b strings.Builder
	for range rng.IntN(4) {
		b.WriteString(pieces[rng.IntN(len(pieces))])
	}
	return b.String()
}

// generate writes a string re matches, choosing at random wherever re allows a
// choice: a member of a folded rune's orbit, a rune of a class (or a byte that
// is not UTF-8, where the class holds U+FFFD), a branch, a count.
func generate(rng *rand.Rand, re *syntax.Regexp, b *strings.Builder) {
	switch re.Op {
	case syntax.OpLiteral:
		for _, r := range re.Rune {
			if re.Flags&syntax.FoldCase != 0 {
				orbit := orbitOf(r)
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
		generate(rng, re.Sub[0], b)
	case syntax.OpStar:
		for range rng.IntN(3) {
			generate(rng, re.Sub[0], b)
		}
	case syntax.OpPlus:
		for range 1 + rng.IntN(2) {
			generate(rng, re.Sub[0], b)
		}
	case syntax.OpQuest:
		if rng.IntN(2) == 0 {
			generate(rng, re.Sub[0], b)
		}
	case syntax.OpRepeat:
		n := re.Min
		if re.Max > re.Min {
			n += rng.IntN(re.Max - re.Min + 1)
		}
		for range n {
			generate(rng, re.Sub[0], b)
		}
	case syntax.OpConcat:
		for _, sub := range re.Sub {
			generate(rng, sub, b)
		}
	case syntax.OpAlternate:
		generate(rng, re.Sub[rng.IntN(len(re.Sub))], b)
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

// orbitOf is every rune case-folded together with r, r included.
func orbitOf(r rune) []rune {
	out := []rune{r}
	for m := unicode.SimpleFold(r); m != r; m = unicode.SimpleFold(m) {
		out = append(out, m)
	}
	return out
}

// THE FOLDED COMPARISON IS SOUND FOR EVERY RUNE THERE IS.
//
// Not evidence but the proof, by exhaustion: for every rune a (?i) literal
// can hold, every rune the regexp would match in its place either lowers to
// what the prefilter looks for, or is an escape that admits the whole text —
// or the rune is one the prefilter refuses to compare, and a literal is cut
// there.
func TestTheFoldedComparisonIsSoundForEveryRune(t *testing.T) {
	t.Parallel()
	escaped := map[rune]bool{}
	for _, e := range escapes() {
		r, _ := utf8.DecodeRune(e)
		escaped[r] = true
	}
	compared, refused := 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		lowered, ok := foldedRune(r)
		if !ok {
			refused++
			continue
		}
		compared++
		for _, m := range orbitOf(r) {
			if unicode.ToLower(m) != lowered && !escaped[m] {
				t.Fatalf("(?i)%q matches %q, which lowers to %q, not to the %q "+
					"the prefilter looks for, and is not an escape", r, m,
					unicode.ToLower(m), lowered)
			}
		}
	}
	// THE TWO RUNES THE FINDING NAMED. The long s folds with `s` and does
	// not lower to it, so it must be an escape; the Kelvin sign folds with
	// `k` and DOES lower to it, so lowering alone covers it.
	if !escaped['ſ'] {
		t.Error("U+017F folds with s and lowers to itself, and it is not an escape: " +
			"a withdrawn name spelled with a long s would be skipped")
	}
	if escaped['\U0000212A'] {
		t.Error("U+212A lowers to k, so admitting every text that holds one is waste")
	}
	if refused == 0 {
		t.Error("no rune was refused, so the cut at an inconsistent orbit (σ, ς, Σ) " +
			"is not being made")
	}
	t.Logf("%d runes compared folded, %d refused; escapes %q", compared, refused, escapes())
}

// THE CASES THE FINDINGS NAMED, end to end through Admits.
func TestAFoldedPatternIsNotFooledByARuneThatFoldsOntoASCII(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ pattern, text string }{
		// The withdrawn-vocabulary gate's unsound core: ſ is `s` to (?i).
		{`(?i)\bcross-session\b`, "a croſſ-ſession b"},
		{`(?i)spend_window`, "SPEND_WINDOW"},
		{`(?i)spend_window`, "spend_window"},
		{`(?i)seat_status`, "ſeat_ſtatuS"},
		// The Kelvin sign is `k` to (?i), and lowers to it.
		{`(?i)TaskKind`, "Tas\U0000212A\U0000212Aind"},
		// A byte that is not UTF-8 matches U+FFFD in a pattern.
		{`a\x{FFFD}b`, "a\xffb"},
		{`a[\x{FFFD}c]b`, "a\xffb"},
	} {
		re := regexp.MustCompile(tc.pattern)
		if !re.MatchString(tc.text) {
			t.Fatalf("the case is wrong: %s does not match %q", tc.pattern, tc.text)
		}
		need := Required(re)
		if !need.AdmitsString(tc.text) || !need.Admits([]byte(tc.text)) {
			t.Errorf("Required(%s) (%v) skips %q, which the pattern matches",
				tc.pattern, need, tc.text)
		}
	}
	// AND STILL SKIPS what cannot match, or the cases above prove nothing.
	if Required(regexp.MustCompile(`(?i)spend_window`)).AdmitsString("spend window") {
		t.Error("a folded prefilter admits a text holding none of its substring")
	}
}

// A PATTERN THAT REQUIRES NOTHING ADMITS EVERYTHING, and so does a zero value.
func TestAPatternThatRequiresNothingAdmitsEverything(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		`.*`, `\w+`, `a*`, `x?`, `(?:abc|)`, `^`, `\b`, `[a-z]{3}`, `(?:abc|\d)`,
		`\x{FFFD}`, `.`,
	} {
		need := Required(regexp.MustCompile(pattern))
		if got := need.Clauses(); got != nil {
			t.Errorf("Required(%s) = %q, but the pattern can match without any of it",
				pattern, got)
		}
		if !need.AdmitsString("") || !need.Admits(nil) {
			t.Errorf("Required(%s) skips the empty text", pattern)
		}
	}
	if !(Prefilter{}).AdmitsString("anything") || !(Prefilter{}).Admits([]byte("x")) {
		t.Error("the zero Prefilter skips a text; it must admit everything")
	}
	if !(Identifiers{}).In([]byte("package p")) {
		t.Error("the zero Identifiers skips a file; it must admit everything")
	}
}

// AN IDENTIFIER PREFILTER IS ON IDENTIFIERS ONLY.
func TestIdentifiersRefusesWhatIsNotAnIdentifier(t *testing.T) {
	t.Parallel()
	ids := MustIdentifiers("UpdateStream", "Auxiliary")
	if !ids.In([]byte("js.CreateOrUpdateStream(ctx, c)")) || !ids.In([]byte("e.Auxiliary(")) {
		t.Error("a file spelling one of the identifiers was skipped")
	}
	if ids.In([]byte("js.CreateStream(ctx, c)")) {
		t.Error("a file spelling none of the identifiers was admitted")
	}
	for _, bad := range [][]string{nil, {"net/http"}, {"func"}, {"a b"}, {""}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("MustIdentifiers(%q) accepted a name a prefilter is not "+
						"exact for", bad)
				}
			}()
			MustIdentifiers(bad...)
		}()
	}
}
