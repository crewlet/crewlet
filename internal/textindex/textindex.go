// Package textindex is the engine's own lexical search: an analyzer that
// turns a document into terms, and a BM25 ranker over the inverted list those
// terms are stored in.
//
// # Why the engine has one at all
//
// The native knowledge base has to answer "what do we already know about
// this" over the company's own pages, and the store cannot. Turso ships no
// fts5, and `USING fts` is behind an experimental flag the driver refuses
// without it — [store.Capabilities] probes for both on every open and reports
// what it found, so this is measured on the build that ships rather than
// assumed. The alternatives were refusing knowledge search on the only driver
// this build has (which is not a knowledge base), embedding a search library
// (a second index format, its own file, its own corruption and backup story,
// on every node), or writing the ranking over tables the store already keeps
// well. This is the third.
//
// # What it is not
//
// It is not a search engine. There is no phrase query, no proximity, no
// fielded query language and no spelling correction, because the caller is a
// planner writing a keyword line and a person typing into a box — and every
// one of those features is a promise about a query grammar this seam
// deliberately does not have (see [knowledge.Query]: plain text, never a
// backend fragment). What it does have is the part that decides whether a
// result list is useful: term frequency saturation and length normalisation,
// which is what stops a 20 KB runbook outranking the one-paragraph page that
// is actually the answer.
//
// # Where the SQL lives
//
// Not here. This package holds the analyzer and the arithmetic, as pure
// functions over values, so both are testable without a database and neither
// can be quietly changed by an index rebuild. [internal/projection] owns the
// tables and the statements.
package textindex

import (
	"math"
	"strings"
	"unicode"
)

// The BM25 parameters.
//
// TUNED TO THIS CORPUS, not copied from a paper's default, and the corpus is
// a company's own pages and work items: a few thousand documents from a few
// words (a bug title) to a few thousand (a runbook), written by colleagues
// who reuse each other's vocabulary heavily.
const (
	// K1 saturates term frequency: how much the tenth occurrence of a word
	// adds over the second.
	//
	// 1.2, the low end of the usual 1.2-2.0 range, because these documents
	// repeat their subject constantly — a page about the deploy pipeline
	// says "deploy" thirty times — and a higher K1 would rank a document
	// by how verbosely it says one word rather than by how many of the
	// query's words it covers.
	K1 = 1.2

	// B is how hard length normalisation bites, from 0 (ignore length) to
	// 1 (fully normalise).
	//
	// 0.75, the standard value, kept deliberately: the length spread here
	// is real and meaningful. A three-line page that mentions a term is
	// usually ABOUT that term; a long runbook that mentions it usually
	// is not. Dropping toward 0 would let every long page crowd out the
	// short answer, and pushing toward 1 would bury the runbook that is
	// genuinely the right hit for a broad query.
	B = 0.75
)

// MinTermLength is the shortest token indexed.
//
// Two, so "go", "ci", "k8s" and "s3" survive while single letters do not.
// A one-character token matches nearly every document and carries no signal,
// and the posting list for "a" over a company's whole wiki is the largest
// single row set in the index for the least benefit of any term in it.
const MinTermLength = 2

// MaxTermLength bounds a token, in bytes.
//
// Sixty-four. Past that a "word" is a base64 blob, a stack frame, a minified
// line or a URL with a session in it — content that appears once, is never
// searched for, and would otherwise put one posting row per occurrence into
// a table every query scans. Longer tokens are TRUNCATED rather than dropped,
// so a long identifier still matches its own prefix rather than vanishing.
const MaxTermLength = 64

// Analyze turns a document into its terms, in order of first appearance, with
// the count of each.
//
// The analyzer is deliberately small and its every rule is reversible from
// this doc, because an index is only as consistent as the promise that the
// query side ran exactly the same function. Both sides call this.
//
//   - Case is folded, so a title's "Deploy" matches a body's "deploy".
//   - Tokens break on anything that is not a letter or a digit, which keeps
//     "k8s" and "v2" whole while splitting "deploy-pipeline" into two terms
//     a query for either half will find.
//   - Underscores split too, so "work_items" is findable as "work".
//   - Stop words are NOT removed. A company's pages are full of titles like
//     "The Why", and a stop list is a rule about English that silently makes
//     some documents unfindable — the length normalisation above already
//     denies a common word most of its influence, which is the effect a stop
//     list was reaching for.
//   - No stemming. "deploys" and "deploying" are different terms, which
//     costs some recall and buys the property that matters more here: what a
//     person typed is what was searched, so a result they cannot explain
//     never appears.
func Analyze(text string) map[string]int {
	if text == "" {
		return nil
	}
	terms := map[string]int{}
	tokenize(text, func(term string) { terms[term]++ })
	if len(terms) == 0 {
		return nil
	}
	return terms
}

// Terms is a query's terms, deduplicated, in the order they were typed.
//
// A repeated query word does NOT count twice: scoring it twice would let
// somebody double a term's weight by typing it again, which is a query
// language this seam does not offer. The order is kept only so a caller can
// report what it searched for and highlight it in a snippet.
//
// THROUGH THE SAME TOKENIZER as [Analyze], which is not tidiness: an index is
// only as good as the promise that both sides ran one function, and a second
// loop here would be the thing that drifts — silently, as a query for a word
// that is plainly in the document returning nothing.
func Terms(query string) []string {
	var out []string
	seen := map[string]bool{}
	tokenize(query, func(term string) {
		if seen[term] {
			return
		}
		seen[term] = true
		out = append(out, term)
	})
	return out
}

// tokenize splits text into terms and hands each to yield, in order.
func tokenize(text string, yield func(term string)) {
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		term := b.String()
		b.Reset()
		if len(term) < MinTermLength {
			return
		}
		if len(term) > MaxTermLength {
			term = truncateTerm(term)
		}
		yield(term)
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		flush()
	}
	flush()
}

// truncateTerm cuts a long token at a rune boundary.
//
// A plain slice at MaxTermLength splits a multi-byte rune, and the invalid
// UTF-8 that produces is substituted by the JSON encoder and read back as a
// replacement character — so the query side would analyze the same word to a
// different term and never match it. (internal/textcut carries the general
// form of this rule; it is inlined here because this cut appends nothing and
// the two must not drift into needing each other.)
func truncateTerm(term string) string {
	cut := MaxTermLength
	for cut > 0 && !isRuneStart(term[cut]) {
		cut--
	}
	if cut == 0 {
		return term[:MaxTermLength]
	}
	return term[:cut]
}

// isRuneStart reports whether b begins a UTF-8 rune.
func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// Posting is one term's presence in one document.
type Posting struct {
	DocID string

	// Freq is how many times the term occurs in the document.
	Freq int

	// Length is the document's total token count, the |D| in the length
	// normalisation. Carried on the posting rather than looked up per
	// document so a scorer needs one query per term and no second pass.
	Length int
}

// Corpus is what scoring needs to know about the collection as a whole.
//
// Both fields come from the index's own counts. AvgLength of 0 is treated as
// 1 rather than dividing by zero: a corpus with no length data scores as
// though every document were average, which degrades to plain term-frequency
// ranking instead of returning nothing.
type Corpus struct {
	// Docs is how many documents the index holds.
	Docs int

	// AvgLength is the mean token count across them.
	AvgLength float64
}

// IDF is a term's inverse document frequency, in the BM25 form.
//
// The +0.5 smoothing and the +1 inside the logarithm are what keep this
// NON-NEGATIVE. The textbook form goes negative for a term more than half the
// corpus holds, which in a company wiki is the company's own name — and a
// negative weight means a document scores WORSE for containing a word the
// person searched for, which is indefensible to anyone reading the results.
func IDF(corpusDocs, termDocs int) float64 {
	if corpusDocs <= 0 || termDocs <= 0 {
		return 0
	}
	if termDocs > corpusDocs {
		// A posting count above the document count means the index is
		// mid-rebuild and one of the two reads saw the other half. Clamp
		// rather than produce a negative log: a search during a rebuild
		// should rank oddly, never invert.
		termDocs = corpusDocs
	}
	return math.Log(1 + (float64(corpusDocs)-float64(termDocs)+0.5)/(float64(termDocs)+0.5))
}

// Score is one term's BM25 contribution to one document.
//
// A caller sums this over the query's terms. Split per term rather than
// scored per document so the caller can stream postings term by term — which
// is how the index is stored, and what keeps a query from materialising the
// whole corpus.
func Score(idf float64, p Posting, c Corpus) float64 {
	if p.Freq <= 0 || idf <= 0 {
		return 0
	}
	avg := c.AvgLength
	if avg <= 0 {
		avg = 1
	}
	length := float64(p.Length)
	if length <= 0 {
		// A document with no recorded length: score it as average rather
		// than as infinitely short, which would rank it above everything.
		length = avg
	}
	tf := float64(p.Freq)
	norm := K1 * (1 - B + B*length/avg)
	return idf * (tf * (K1 + 1)) / (tf + norm)
}

// Snippet is the best window of body text for a hit, cut to limit bytes.
//
// It centres on the first query term the body actually contains, because the
// alternative — the document's opening — shows a planner the page's preamble
// rather than the sentence that made it a hit, and a snippet that does not
// contain the search term reads as a wrong result even when the ranking is
// right.
//
// Cut on a RUNE boundary and, where one is near, on a word boundary. Bytes
// rather than runes for the limit itself: this is a budget against a prompt,
// and a prompt is billed in bytes on the wire.
func Snippet(body string, terms []string, limit int) string {
	body = strings.Join(strings.Fields(body), " ")
	if body == "" || limit <= 0 {
		return ""
	}
	if len(body) <= limit {
		return body
	}
	start := 0
	if at := firstTermIndex(body, terms); at > 0 {
		// A third of the window before the match, so the term lands with
		// its own lead-in rather than at the very start of the snippet.
		start = at - limit/3
		if start < 0 {
			start = 0
		}
		for start > 0 && !isRuneStart(body[start]) {
			start--
		}
	}
	end := start + limit
	if end >= len(body) {
		return trimToWord(body[start:], start > 0, false)
	}
	for end > start && !isRuneStart(body[end]) {
		end--
	}
	return trimToWord(body[start:end], start > 0, true)
}

// firstTermIndex finds where the earliest query term appears, or -1.
func firstTermIndex(body string, terms []string) int {
	lower := strings.ToLower(body)
	best := -1
	for _, term := range terms {
		if at := strings.Index(lower, term); at >= 0 && (best < 0 || at < best) {
			best = at
		}
	}
	return best
}

// trimToWord drops a partial word at each cut end and marks the elision.
//
// The ellipsis is added only where text was actually removed, so a snippet
// that happens to start at the document's start does not claim otherwise.
func trimToWord(s string, elideLeft, elideRight bool) string {
	if elideLeft {
		if at := strings.IndexByte(s, ' '); at >= 0 && at < len(s)-1 {
			s = s[at+1:]
		}
	}
	if elideRight {
		if at := strings.LastIndexByte(s, ' '); at > 0 {
			s = s[:at]
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if elideLeft {
		s = "…" + s
	}
	if elideRight {
		s += "…"
	}
	return s
}
