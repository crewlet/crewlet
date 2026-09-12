package textindex_test

import (
	"math"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textindex"
)

// ONE TOKENIZER, BOTH SIDES. An index is only as good as the promise that the
// query side ran exactly the function the write side ran — and the failure of
// that promise is silent: a query for a word plainly in the document returns
// nothing, and nothing anywhere says why.
func TestTheQuerySideTokenizesLikeTheIndexSide(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"Deploy the k8s pipeline",
		"work_items and deploy-pipeline, v2",
		"Übersicht — Q3 planning",
		"CI/CD: rollback? YES.",
	} {
		indexed := textindex.Analyze(text)
		queried := textindex.Terms(text)
		if len(indexed) != len(queried) {
			t.Errorf("%q indexed %d terms and queried %d: %v vs %v",
				text, len(indexed), len(queried), indexed, queried)
			continue
		}
		for _, term := range queried {
			if indexed[term] == 0 {
				t.Errorf("%q: query term %q is not in the indexed set %v",
					text, term, indexed)
			}
		}
	}
}

// The analyzer's rules, each with the failure it prevents.
func TestTheAnalyzersRules(t *testing.T) {
	t.Parallel()

	// Case folds, so a title's word matches a body's.
	if got := textindex.Analyze("Deploy deploy DEPLOY"); got["deploy"] != 3 {
		t.Errorf("case is not folded: %v", got)
	}

	// Hyphens and underscores split, so half a compound is findable.
	got := textindex.Analyze("deploy-pipeline work_items")
	for _, want := range []string{"deploy", "pipeline", "work", "items"} {
		if got[want] == 0 {
			t.Errorf("%q did not survive splitting: %v", want, got)
		}
	}

	// Digits stay attached, so k8s and v2 are one term each rather than a
	// dropped letter and a number.
	got = textindex.Analyze("k8s v2 s3")
	for _, want := range []string{"k8s", "v2", "s3"} {
		if got[want] != 1 {
			t.Errorf("%q was split or dropped: %v", want, got)
		}
	}

	// A single character carries no signal and would own the largest
	// posting list in the index.
	if got := textindex.Analyze("a b the"); len(got) != 1 || got["the"] != 1 {
		t.Errorf("one-character tokens survived: %v", got)
	}

	// STOP WORDS ARE KEPT, deliberately: a page titled "The Why" must be
	// findable, and length normalisation already denies a common word most
	// of its influence.
	if got := textindex.Analyze("The Why"); got["the"] != 1 || got["why"] != 1 {
		t.Errorf("a stop list crept in: %v", got)
	}

	// Empty in, nothing out — never an empty non-nil map a caller has to
	// distinguish.
	if textindex.Analyze("") != nil || textindex.Analyze("- , !") != nil {
		t.Error("a text with no terms produced a map")
	}
}

// A LONG TOKEN IS CUT ON A RUNE BOUNDARY. A plain byte slice splits a
// multi-byte rune, and the invalid UTF-8 that produces is substituted by the
// JSON encoder — so the query side analyzes the same word to a different term
// and never matches it.
func TestALongTokenIsCutWithoutBreakingARune(t *testing.T) {
	t.Parallel()
	// Two-byte runes, so a byte cut at 64 lands mid-rune.
	long := strings.Repeat("é", 60)
	terms := textindex.Terms(long)
	if len(terms) != 1 {
		t.Fatalf("terms = %v", terms)
	}
	if !utf8.ValidString(terms[0]) {
		t.Errorf("the cut term is not valid UTF-8: %q", terms[0])
	}
	if len(terms[0]) > 64 {
		t.Errorf("the term is %d bytes, past the cap", len(terms[0]))
	}
	// And the two sides still agree on it, which is the point of the cut.
	if textindex.Analyze(long)[terms[0]] == 0 {
		t.Error("the indexed term and the queried term differ after truncation")
	}
}

// A repeated query word must not weigh twice: that is a query language this
// seam does not offer, and somebody would find it by accident.
func TestARepeatedQueryWordCountsOnce(t *testing.T) {
	t.Parallel()
	got := textindex.Terms("deploy deploy deploy pipeline")
	if !slices.Equal(got, []string{"deploy", "pipeline"}) {
		t.Errorf("terms = %v, want the query's words once each in order", got)
	}
}

// IDF IS NEVER NEGATIVE. The textbook form goes negative for a term more than
// half the corpus holds — in a company wiki, the company's own name — and a
// negative weight means a document scores WORSE for containing a word the
// person searched for.
func TestIDFStaysNonNegativeForACommonTerm(t *testing.T) {
	t.Parallel()
	for _, termDocs := range []int{1, 50, 99, 100} {
		if got := textindex.IDF(100, termDocs); got < 0 {
			t.Errorf("IDF(100, %d) = %v, and a negative weight punishes a match",
				termDocs, got)
		}
	}
	// Rarer is worth more, which is the whole property.
	if textindex.IDF(100, 1) <= textindex.IDF(100, 50) {
		t.Error("a rare term does not outweigh a common one")
	}
	// A posting count above the corpus size means the index is mid-rebuild
	// and two reads saw different halves. Odd ranking is acceptable there;
	// inverted ranking is not.
	if got := textindex.IDF(10, 40); got < 0 {
		t.Errorf("a mid-rebuild count inverted the weight: %v", got)
	}
	// Nothing to divide by is zero, not a NaN that poisons a whole sum.
	for _, got := range []float64{textindex.IDF(0, 0), textindex.IDF(10, 0), textindex.IDF(0, 3)} {
		if got != 0 {
			t.Errorf("a degenerate corpus scored %v", got)
		}
	}
}

// LENGTH NORMALISATION IS WHAT MAKES THE LIST USEFUL: it stops a 20 KB
// runbook that mentions a term in passing outranking the one-paragraph page
// that is actually the answer.
func TestAShortDocumentBeatsALongOneAtEqualFrequency(t *testing.T) {
	t.Parallel()
	c := textindex.Corpus{Docs: 100, AvgLength: 400}
	idf := textindex.IDF(100, 10)
	short := textindex.Score(idf, textindex.Posting{DocID: "s", Freq: 3, Length: 60}, c)
	long := textindex.Score(idf, textindex.Posting{DocID: "l", Freq: 3, Length: 4000}, c)
	if short <= long {
		t.Errorf("short %v did not beat long %v at the same frequency", short, long)
	}
}

// TERM FREQUENCY SATURATES. Without it a document ranks by how verbosely it
// repeats one word rather than by how much of the query it covers.
func TestFrequencySaturates(t *testing.T) {
	t.Parallel()
	c := textindex.Corpus{Docs: 100, AvgLength: 400}
	idf := textindex.IDF(100, 10)
	at := func(freq int) float64 {
		return textindex.Score(idf, textindex.Posting{DocID: "d", Freq: freq, Length: 400}, c)
	}
	first, second, tenth := at(1), at(2), at(10)
	if !(second > first && tenth > second) {
		t.Fatalf("score is not monotonic in frequency: %v %v %v", first, second, tenth)
	}
	// MARGINAL gain, per occurrence: the tenth must be worth strictly less
	// than the second. Comparing totals would compare eight steps against
	// one and say nothing.
	if late, early := at(10)-at(9), second-first; late >= early {
		t.Errorf("the tenth occurrence added %v and the second added %v — "+
			"without saturation a document ranks by how verbosely it repeats "+
			"one word", late, early)
	}
	// Bounded above by idf*(K1+1), which is what saturation MEANS.
	if ceiling := idf * (textindex.K1 + 1); at(1_000_000) > ceiling {
		t.Errorf("score exceeded its ceiling %v", ceiling)
	}
}

// COVERING MORE OF THE QUERY WINS AT EQUAL FREQUENCY, which is the ranking
// property a person notices: a page that mentions both words they typed comes
// above one that mentions only the rarer.
//
// It is deliberately stated AT EQUAL FREQUENCY. A document saturating a rare
// term does out-score one covering two, and that is BM25 working rather than
// failing: a page saying "rollback" twenty times is about rollback, where one
// saying "rollback" and "deploy" twice each is clearly about neither. The
// ceiling asserted in TestFrequencySaturates is what bounds how far that can
// go.
func TestCoveringMoreOfTheQueryWins(t *testing.T) {
	t.Parallel()
	c := textindex.Corpus{Docs: 100, AvgLength: 200}
	rare, common := textindex.IDF(100, 5), textindex.IDF(100, 40)
	both := textindex.Posting{DocID: "a", Freq: 2, Length: 200}
	covers := textindex.Score(rare, both, c) + textindex.Score(common, both, c)
	one := textindex.Score(rare, textindex.Posting{DocID: "b", Freq: 2, Length: 200}, c)
	if covers <= one {
		t.Errorf("covering both terms (%v) lost to matching one (%v)", covers, one)
	}
	// And the common term is worth LESS than the rare one it is added to,
	// or the weighting is not doing its job.
	if textindex.Score(common, both, c) >= textindex.Score(rare, both, c) {
		t.Error("a term 40% of the corpus holds weighs as much as one 5% holds")
	}

	// THIS IS WHAT K1 BUYS, and the only assertion that pins its value.
	// At the tuned 1.2, covering both terms twice still beats saying the
	// rarer one five times — which is the case the doc names: these
	// documents repeat their subject constantly, so a K1 anywhere near the
	// top of the usual range ranks by verbosity instead of by coverage.
	// (Measured: the two are equal at about six occurrences with K1=1.2,
	// and coverage already loses at three with K1=2.0.)
	verbose := textindex.Score(rare, textindex.Posting{DocID: "c", Freq: 5, Length: 200}, c)
	if covers <= verbose {
		t.Errorf("covering both terms (%v) lost to five occurrences of the rarer "+
			"one (%v) — K1=%v saturates too slowly for a corpus that repeats "+
			"its subject", covers, verbose, textindex.K1)
	}
}

// A DEGENERATE CORPUS DEGRADES, never divides by zero or ranks a
// length-less document above everything.
func TestScoringSurvivesAMissingCorpus(t *testing.T) {
	t.Parallel()
	idf := textindex.IDF(10, 2)
	for _, tc := range []struct {
		name string
		p    textindex.Posting
		c    textindex.Corpus
	}{
		{"no average length", textindex.Posting{DocID: "d", Freq: 2, Length: 100}, textindex.Corpus{Docs: 10}},
		{"no document length", textindex.Posting{DocID: "d", Freq: 2}, textindex.Corpus{Docs: 10, AvgLength: 200}},
		{"neither", textindex.Posting{DocID: "d", Freq: 2}, textindex.Corpus{}},
	} {
		got := textindex.Score(idf, tc.p, tc.c)
		if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
			t.Errorf("%s scored %v", tc.name, got)
		}
	}
	// No occurrences is no contribution, never a phantom hit.
	if got := textindex.Score(idf, textindex.Posting{DocID: "d", Length: 10},
		textindex.Corpus{Docs: 10, AvgLength: 10}); got != 0 {
		t.Errorf("a zero-frequency posting scored %v", got)
	}
}

// A SNIPPET THAT DOES NOT CONTAIN THE SEARCH TERM READS AS A WRONG RESULT,
// even when the ranking is right. It centres on the match rather than showing
// the document's preamble.
func TestASnippetCentresOnTheMatch(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("preamble words that are not the answer. ", 20) +
		"the rollback procedure is documented here. " +
		strings.Repeat("trailing filler. ", 20)

	got := textindex.Snippet(body, []string{"rollback"}, 120)
	if !strings.Contains(got, "rollback") {
		t.Fatalf("the snippet does not contain the term: %q", got)
	}
	if len(got) > 120+len("……") {
		t.Errorf("snippet is %d bytes, past its budget: %q", len(got), got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix(got, "…") {
		t.Errorf("an elided snippet must say so at both ends: %q", got)
	}

	// A body that fits is returned whole, with no ellipsis claiming
	// something was cut.
	short := "the rollback procedure"
	if got := textindex.Snippet(short, []string{"rollback"}, 120); got != short {
		t.Errorf("a short body was altered: %q", got)
	}

	// No match: the opening is the honest fallback, and it still cuts.
	got = textindex.Snippet(body, []string{"nowhere"}, 60)
	if strings.HasPrefix(got, "…") {
		t.Errorf("the opening was marked as elided: %q", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("the cut broke a rune: %q", got)
	}

	// A cut in the middle of a multi-byte rune is the failure that reaches
	// a model as a replacement character.
	wide := strings.Repeat("é ", 200)
	if got := textindex.Snippet(wide, []string{"é"}, 61); !utf8.ValidString(got) {
		t.Errorf("the snippet is not valid UTF-8: %q", got)
	}

	// Whitespace is collapsed, so a markdown body's blank lines do not
	// spend the budget.
	if got := textindex.Snippet("a\n\n\nb   c", nil, 100); got != "a b c" {
		t.Errorf("whitespace was not collapsed: %q", got)
	}
	if got := textindex.Snippet("", []string{"x"}, 100); got != "" {
		t.Errorf("an empty body produced %q", got)
	}
}
