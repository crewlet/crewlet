package regexlit

import (
	"maps"
	"math/rand/v2"
	"regexp"
	"regexp/syntax"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/regexlit/regexlittest"
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
		patterns = append(patterns, vocabulary.Pattern(rng, 3))
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
			regexlittest.Generate(rng, parsed, &b)
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

// literals are literal runs, each with a rune whose folding is the hard case: an
// ASCII letter with a non-ASCII partner, a non-ASCII orbit that lowers
// inconsistently, U+FFFD, and punctuation the gates write.
var literals = []string{
	"ADR-", "types.", "kind", "class", "Sk", "metrics", "env", "_", ":", "{",
	"ſ", "\U0000212A", "é", "σ", "ς", "Σ", "\U000000B5", "\U0000FFFD", "x\U0000FFFDy", "%s", " ",
}

// classes are character classes and escapes, narrow and wide.
var classes = []string{
	"[abc]", "[a-c]", "[^x]", `\d`, `\w`, `\s`, "[kK]", "[sſ]", `[\x{FFFD}a]`,
	"[FS]", `[A-Za-z0-9_]`, ".", "[σς]",
}

// vocabulary is what the generated patterns are written from: the literals and
// classes above, and a flag in three of every six.
var vocabulary = regexlittest.Vocabulary{
	Flags: []string{"(?i)", "(?s)", "(?m)", "", "", ""}, Words: literals, Classes: classes,
}

// noise is context a match is embedded in: literals, separators, a newline, the
// escapes, and a byte that is not UTF-8.
func noise(rng *rand.Rand) string {
	pieces := []string{"a", "Z", " ", "\n", ".", "ſ", "\U0000212A", "\xff", "ADR", "e", "-", "σ"}
	var b strings.Builder
	for range rng.IntN(4) {
		b.WriteString(pieces[rng.IntN(len(pieces))])
	}
	return b.String()
}

// escapeByDefinition reports whether r, whose orbit is orbit, is an escape as
// the escape set's doc defines one: a rune that is not ASCII, folded together
// with an ASCII letter, that does not lower to that letter's lowering.
func escapeByDefinition(r rune, orbit []rune) bool {
	if r < utf8.RuneSelf {
		return false
	}
	for _, m := range orbit {
		if m < utf8.RuneSelf && unicode.ToLower(m) != unicode.ToLower(r) {
			return true
		}
	}
	return false
}

// THE FOLDED COMPARISON IS SOUND FOR EVERY RUNE THERE IS.
//
// Not evidence but the proof, by exhaustion: for every rune a (?i) literal
// can hold, every rune the regexp would match in its place either lowers to
// what the prefilter looks for, or is an escape that admits the whole text —
// or the rune is one the prefilter refuses to compare, and a literal is cut
// there.
//
// An escape is what HasEscape reports, because that is what Admits asks, and
// what a caller making the comparison from Terms asks: the proof is of the
// function they call. And it reports EXACTLY the runes that fold with an
// ASCII letter without lowering to its lowering (escapeByDefinition, which
// states that rune by rune where the package derives it from the ASCII
// letters): a rune it missed is a match skipped, and one more is a text
// admitted, or an index entry passed, for nothing.
func TestTheFoldedComparisonIsSoundForEveryRune(t *testing.T) {
	t.Parallel()
	var encoded [utf8.UTFMax]byte
	escaped := func(r rune) bool { return HasEscape(utf8.AppendRune(encoded[:0], r)) }
	compared, refused := 0, 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		orbit := regexlittest.Orbit(r)
		switch is, want := escaped(r), escapeByDefinition(r, orbit); {
		case is && !want:
			t.Fatalf("HasEscape reports %q, which folds with no ASCII letter it "+
				"fails to lower to, so every text holding it is admitted for nothing", r)
		case want && !is:
			t.Fatalf("%q folds with an ASCII letter and does not lower to its "+
				"lowering, and HasEscape does not report it", r)
		}
		lowered, ok := foldedRune(r)
		if !ok {
			refused++
			continue
		}
		compared++
		for _, m := range orbit {
			if unicode.ToLower(m) != lowered && !escaped(m) {
				t.Fatalf("(?i)%q matches %q, which lowers to %q, not to the %q "+
					"the prefilter looks for, and is not an escape", r, m,
					unicode.ToLower(m), lowered)
			}
		}
	}
	// THE TWO RUNES THE FINDING NAMED. The long s folds with `s` and does
	// not lower to it, so it must be an escape; the Kelvin sign folds with
	// `k` and DOES lower to it, so lowering alone covers it.
	if !escaped('ſ') {
		t.Error("U+017F folds with s and lowers to itself, and it is not an escape: " +
			"a withdrawn name spelled with a long s would be skipped")
	}
	if escaped('\U0000212A') {
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
}

// TERMS ARE THE CLAUSES' PIECES, EACH WITH THE COMPARISON ADMITS MAKES.
//
// A caller that looks a prefilter's substrings up somewhere other than in a
// text — an index of the pieces each text holds — reads them through Terms and
// makes Admits's comparison itself, so a Folded flag that disagreed with
// Admits would be a hole in that caller's answers which nothing else here
// would see. Held over the gates' patterns and generated ones: Terms are
// Clauses' substrings, clause by clause in the same order, a folded one in
// its lowered form; and the comparison made from Terms and HasEscape alone,
// as their docs state it, admits exactly the texts Admits does. Each text is
// also tried upper- and lower-cased, because a wrong flag shows only where
// the case differs from the pattern's: a plain `ADR-` compared folded admits
// `adr-`, and a folded `spend_window` compared as it is skips `SPEND_WINDOW`.
func TestTermsAreTheClausesPieces(t *testing.T) {
	t.Parallel()
	if got := (Prefilter{}).Terms(); got != nil {
		t.Errorf("the zero Prefilter has the terms %v; it looks for nothing", got)
	}
	rng := rand.New(rand.NewPCG(20261026, 3))
	patterns := slices.Sorted(maps.Keys(gatePatterns))
	for range 1000 {
		patterns = append(patterns, vocabulary.Pattern(rng, 3))
	}
	plain, folded, compared := 0, 0, 0
	for _, pattern := range patterns {
		need := Required(regexp.MustCompile(pattern))
		terms, clauses := need.Terms(), need.Clauses()
		if len(terms) != len(clauses) {
			t.Fatalf("Required(%s) has %d clauses of terms and %d of substrings",
				pattern, len(terms), len(clauses))
		}
		for i, clause := range terms {
			var texts []string
			for _, term := range clause {
				texts = append(texts, term.Text)
				if !term.Folded {
					plain++
					continue
				}
				folded++
				if lowered := strings.ToLower(term.Text); lowered != term.Text {
					t.Fatalf("Required(%s) has the folded term %q, which is not the "+
						"lowered form %q a text's lowering is searched for",
						pattern, term.Text, lowered)
				}
			}
			slices.Sort(texts)
			if texts = slices.Compact(texts); !slices.Equal(texts, clauses[i]) {
				t.Fatalf("Required(%s): clause %d's terms are %q and its "+
					"substrings %q", pattern, i, texts, clauses[i])
			}
		}
		parsed, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("parse %q: %v", pattern, err)
		}
		for range 10 {
			var b strings.Builder
			b.WriteString(noise(rng))
			if rng.IntN(4) > 0 {
				regexlittest.Generate(rng, parsed, &b)
			}
			b.WriteString(noise(rng))
			text := b.String()
			for _, variant := range []string{text, strings.ToUpper(text), strings.ToLower(text)} {
				compared++
				if got, want := admitsByTerms(terms, variant), need.AdmitsString(variant); got != want {
					t.Fatalf("Required(%s) (%v) admits %q: %v; its terms %v, "+
						"compared as their docs say: %v", pattern, need, variant,
						want, terms, got)
				}
			}
		}
	}
	// A FLAG NEVER SEEN IS A COMPARISON THE PROPERTY ABOVE NEVER CHECKED.
	if plain == 0 || folded == 0 {
		t.Fatalf("%d plain and %d folded terms, so one comparison was never checked",
			plain, folded)
	}
	t.Logf("%d patterns: %d plain and %d folded terms; %d texts compared both ways",
		len(patterns), plain, folded, compared)
}

// admitsByTerms is Admits made from Terms and HasEscape alone, as their docs
// state it: a text holding an escape is admitted outright when any term is
// Folded, and otherwise has to hold, for every clause, one of its terms —
// byte for byte, or in the text's lowering when the term is Folded.
func admitsByTerms(terms [][]Term, text string) bool {
	anyFolded := slices.ContainsFunc(terms, func(clause []Term) bool {
		return slices.ContainsFunc(clause, func(term Term) bool { return term.Folded })
	})
	if anyFolded && HasEscape([]byte(text)) {
		return true
	}
	lowered := strings.ToLower(text)
	for _, clause := range terms {
		held := slices.ContainsFunc(clause, func(term Term) bool {
			if term.Folded {
				return strings.Contains(lowered, term.Text)
			}
			return strings.Contains(text, term.Text)
		})
		if !held {
			return false
		}
	}
	return true
}

// A LINE-LOCAL PATTERN FINDS THE SAME MATCHES LINE BY LINE AS OVER THE TEXT.
//
// Generated as the property above is — matches built from each pattern's own
// syntax tree, joined by newlines and noise that holds newlines too — and
// compared both ways: every pattern LineLocal accepts finds, line by line,
// exactly the matches it finds over the whole text, in order.
func TestALineLocalPatternFindsTheSameMatchesLineByLine(t *testing.T) {
	t.Parallel()
	for pattern, want := range map[string]bool{
		`\btypes\.([A-Z][A-Za-z0-9_]*)\{`:       true,
		`\bmetrics\.([A-Z]\w*)\b`:               true,
		`(?m)^func (Test[A-Za-z0-9_]*)\(`:       true,
		`(?i)home\s+stream`:                     false, // \s matches a newline
		`\bKind:\s*types\.(Guard[A-Za-z0-9_]+)`: false,
		`^package`:                              false, // the text's start, not a line's
		`end$`:                                  false,
		`(?s)a.b`:                               false,
		`a.b`:                                   true,
		"[^x]":                                  false, // a negated class holds the newline
		`a\nb`:                                  false,
	} {
		if got := LineLocal(regexp.MustCompile(pattern)); got != want {
			t.Errorf("LineLocal(%s) = %v, want %v", pattern, got, want)
		}
	}

	rng := rand.New(rand.NewPCG(20261008, 2))
	local, compared := 0, 0
	for range 1000 {
		pattern := vocabulary.Pattern(rng, 3)
		re := regexp.MustCompile(pattern)
		if !LineLocal(re) {
			continue
		}
		local++
		parsed, err := syntax.Parse(pattern, syntax.Perl)
		if err != nil {
			t.Fatalf("parse %q: %v", pattern, err)
		}
		for range 10 {
			var b strings.Builder
			for range 1 + rng.IntN(4) {
				b.WriteString(noise(rng))
				regexlittest.Generate(rng, parsed, &b)
				b.WriteString([]string{"\n", " ", "\n\n", ""}[rng.IntN(4)])
			}
			text := b.String()
			whole := re.FindAllString(text, -1)
			var lines []string
			for _, line := range strings.Split(text, "\n") {
				lines = append(lines, re.FindAllString(line, -1)...)
			}
			if !slices.Equal(whole, lines) {
				t.Fatalf("LineLocal(%s) is true, and over %q it finds %q but line "+
					"by line %q", pattern, text, whole, lines)
			}
			compared++
		}
	}
	if local < 100 {
		t.Fatalf("only %d of 1000 generated patterns were line-local, so the "+
			"property was hardly exercised", local)
	}
	t.Logf("%d line-local patterns, %d texts compared", local, compared)
}
