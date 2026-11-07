// Package textindex is the engine's own lexical search: an analyzer that
// turns a document into terms, a BM25 ranker over the inverted list those
// terms are stored in, and [Find], which asks the same of one text no index
// holds — whether it holds a query's every term, and where the first is — and
// [FindIn] of one handed over in pieces, which [MayHold] says of a text
// whether any stretches cut from it could answer.
//
// # Why the engine has one at all
//
// Doclet has to answer "what do we already know about
// this" over the company's own pages, and the store cannot. Turso ships no
// fts5, and `USING fts` is behind an experimental flag the driver refuses
// without it — [store.Capabilities] measures both once per process and reports
// them on every open, so this is measured on the build that ships rather than
// assumed. The alternatives were refusing knowledge search on the only driver
// this build has (which is not a knowledge base), embedding a search library
// (a second index format, its own file, its own corruption and backup story,
// on every node), or writing the ranking over tables the store already keeps
// well. This is the third.
//
// # What it is not
//
// It is not a search engine. There is no phrase query, no proximity, no
// fielded query language and no spelling correction, because the caller is an
// agent writing a keyword line and a person typing into a box, and every
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
// can be quietly changed by an index rebuild. [internal/search] owns the
// tables and the statements — its Indexer maintains the inverted list this
// package's arithmetic ranks.
package textindex

import (
	"bytes"
	"iter"
	"math"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// A Profile is one corpus's BM25 tuning, chosen by the index that ranks with
// it rather than baked into the arithmetic.
//
// TWO CORPORA, ONE ARITHMETIC. The knowledge base and the company's chat are
// ranked by the same pure functions and differ in exactly one parameter,
// because their length distributions differ and nothing else about BM25 does.
// A second implementation would be two rankings to re-measure; a second
// parameter set is one line each and a test that says what it buys.
//
// It is a named string rather than a struct of numbers so the zero value is
// INVALID rather than a silent third tuning: an unset struct would score with
// K1 = 0 and B = 0, which is not "the default" but "ignore term frequency and
// ignore length" — a ranking nobody chose, returning plausible nonsense.
type Profile string

const (
	// ProseProfile ranks documents somebody sat down to write: a page, a
	// runbook, a work item and its description.
	ProseProfile Profile = "prose"

	// ChatProfile ranks a company's chat, where the length distribution is
	// bimodal — one-line remarks and pasted blocks — and nothing in
	// between.
	ChatProfile Profile = "chat"
)

// Valid reports a profile this package has a tuning for.
//
// An unknown profile is a PROGRAMMING error rather than a value off the wire:
// a profile is chosen by the indexer in code and never decoded from a record,
// so the honest answer is to refuse it here and let the caller's own
// validation say so, rather than to fall back to a tuning nobody asked for.
func (p Profile) Valid() bool {
	_, _, ok := p.params()
	return ok
}

// params is the tuning itself, and the one place either number is written.
//
// TUNED TO THESE CORPORA, not copied from a paper's defaults.
func (p Profile) params() (k1, b float64, ok bool) {
	switch p {
	case ProseProfile:
		// K1 saturates term frequency: how much the tenth occurrence of
		// a word adds over the second. 1.2, the low end of the usual
		// 1.2-2.0 range, because these documents repeat their subject
		// constantly — a page about the deploy pipeline says "deploy"
		// thirty times — and a higher K1 would rank a document by how
		// verbosely it says one word rather than by how many of the
		// query's words it covers.
		//
		// B is how hard length normalisation bites, from 0 (ignore
		// length) to 1 (fully normalise). 0.75, the standard value,
		// kept deliberately: the length spread here is real and
		// meaningful. A three-line page that mentions a term is usually
		// ABOUT that term; a long runbook that mentions it usually is
		// not. Dropping toward 0 would let every long page crowd out the
		// short answer, and pushing toward 1 would bury the runbook that
		// is genuinely the right hit for a broad query.
		return 1.2, 0.75, true
	case ChatProfile:
		// THE SAME SATURATION, because a person who says a word twice
		// in a message means it as much as a page that says it twice.
		//
		// A MUCH WEAKER LENGTH PENALTY, because the prose argument
		// inverts on this corpus. A chat corpus is bimodal: "ok" and
		// "shipped, thanks" beside a pasted stack trace or a paragraph
		// of reasoning, with almost nothing between them. At 0.75 the
		// short messages win nearly everything — the two-token
		// acknowledgement that happens to contain the query word
		// outranks the paragraph that actually answers it, which is the
		// opposite of what a person searching chat is looking for. At
		// 0.30 length is a mild correction rather than the dominant
		// term, and a substantive message beats an acknowledgement
		// while a wall of pasted logs still does not beat a considered
		// sentence. The reversal the two values buy is pinned by a
		// test.
		return 1.2, 0.30, true
	}
	return 0, 0, false
}

// K1 is this profile's term-frequency saturation.
func (p Profile) K1() float64 { k1, _, _ := p.params(); return k1 }

// B is this profile's length normalisation.
func (p Profile) B() float64 { _, b, _ := p.params(); return b }

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
	tokenize(text, func(term []byte, _ int) bool {
		terms[string(term)]++
		return true
	})
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
	tokenize(query, func(term []byte, _ int) bool {
		if seen[string(term)] {
			return true
		}
		seen[string(term)] = true
		out = append(out, string(term))
		return true
	})
	return out
}

// Find reports where in text the earliest occurrence of any of terms begins —
// the byte offset of its first character, -1 where none occurs — and whether
// text holds every one of them. Terms are a query's as [Terms] makes them, and
// each is found only as a whole term [Analyze] makes of text: "deploy" is not
// in "redeployment", and "Deploy" is in "deploy-pipeline". No terms at all are
// found nowhere: (-1, false), as a query of no words matches nothing.
//
// THROUGH THE ONE TOKENIZER, for [Terms]' reason: a match decided by any other
// reading of the text — a substring, another rule for case or for what splits
// a word — is one a search through the index would not make, and a word
// plainly in the text would be found by one and not by the other. And
// STREAMING: it reads text once, holds no term of it past the comparison, and
// stops as soon as every term has been seen, so a text of mebibytes costs one
// pass and no allocation per word.
func Find(text string, terms []string) (first int, all bool) {
	return FindIn(func(yield func(int, string) bool) { yield(0, text) }, terms)
}

// FindIn is [Find] over a text handed over in pieces, in order, each with the
// byte offset in the text it begins at: first is an offset in the whole text.
// A term is read within one piece and never across two, so pieces are cut
// where no term runs on — the stretches a redaction left between the markers
// it wrote, say (internal/redact's OwnWords), which a reader of the redacted
// text is shown as its own words, apart from the marker's.
//
// IN PIECES RATHER THAN A COPY OF THE TEXT WITH THE GAPS BLANKED, which would
// be one more copy of a text that may run to mebibytes, made only to be
// scanned once.
func FindIn(pieces iter.Seq2[int, string], terms []string) (first int, all bool) {
	if len(terms) == 0 {
		return -1, false
	}
	seen := make(map[string]bool, len(terms))
	for _, term := range terms {
		seen[term] = false
	}
	first, missing := -1, len(seen)
	for offset, piece := range pieces {
		tokenize(piece, func(term []byte, at int) bool {
			found, wanted := seen[string(term)]
			if !wanted {
				return true
			}
			if first < 0 {
				first = offset + at
			}
			if !found {
				seen[string(term)] = true
				missing--
			}
			return missing > 0
		})
		if missing == 0 {
			break
		}
	}
	return first, missing == 0
}

// MayHold reports whether stretches cut from text could hold every one of
// terms, as [FindIn] reads stretches: whether each term is inside one of
// text's words as the analyzer reads them — whole, or as a piece of a longer
// word, wherever in it the piece is. It is what a caller asks of a text
// before it pays to cut the stretches — a redaction, say — and it decides
// nothing itself: false says FindIn over any stretches of text finds not
// every term, and true only that the stretches are worth reading.
//
// A STRETCH CAN CUT A WORD SHORT, NEVER MAKE ONE UP. A stretch of text — the
// words a redaction leaves of it between the markers it writes, which it
// copies as they stand (internal/redact's OwnWords) — ends a word of text
// where the stretch ends, so a term can be a stretch's whole word and only a
// piece of the text's: `deploy` before a token the redaction takes out,
// where the text as recorded reads `deployghp_…`, which the analyzer reads as
// `deployghp`. [Find] of the text whole misses that term, so a caller that
// asked it first decided on the text rather than on what its reader is shown:
// the term was missed unless the text held it whole somewhere else — inside a
// credential the redaction took out, say — and then found, so what the
// redaction hid decided whether the reader was shown a hit. Every word of a
// stretch is a piece of a word of text, so MayHold reads text's words whole,
// at whatever length — a term a stretch's word is cut to past
// [MaxTermLength] is that word's opening, still a piece of the text's.
func MayHold(text string, terms []string) bool {
	if len(terms) == 0 {
		return false
	}
	wanted := make([][]byte, len(terms))
	for i, term := range terms {
		wanted[i] = []byte(term)
	}
	words(text, func(word []byte, _ int) bool {
		wanted = slices.DeleteFunc(wanted, func(term []byte) bool { return bytes.Contains(word, term) })
		return len(wanted) > 0
	})
	return len(wanted) == 0
}

// tokenize splits text into terms and hands each to yield, in order, with the
// byte offset in text where the term's first character is; it stops where
// yield answers false. A term is a word of text ([words]) of at least
// [MinTermLength] bytes, cut at [MaxTermLength]. A term is handed over as
// bytes that are valid only until yield returns: a caller that keeps one
// copies it, and one that only compares it allocates nothing.
func tokenize(text string, yield func(term []byte, at int) bool) {
	words(text, func(word []byte, at int) bool {
		if len(word) < MinTermLength {
			return true
		}
		if len(word) > MaxTermLength {
			word = truncateTerm(word)
		}
		return yield(word, at)
	})
}

// words hands each word of text to yield — a run of letters and digits as
// long as it runs, case folded — in order, with the byte offset in text where
// its first character is; it stops where yield answers false. A word is
// handed over as bytes valid only until yield returns.
//
// THE ONE READING OF WHAT A WORD IS, which [tokenize] makes terms of and
// [MayHold] reads whole: two readings would be two answers to whether a text
// holds a word.
func words(text string, yield func(word []byte, at int) bool) {
	var word []byte
	start := 0
	for at, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if len(word) == 0 {
				start = at
			}
			word = utf8.AppendRune(word, unicode.ToLower(r))
			continue
		}
		if len(word) == 0 {
			continue
		}
		if !yield(word, start) {
			return
		}
		word = word[:0]
	}
	if len(word) > 0 {
		yield(word, start)
	}
}

// truncateTerm cuts a long token at a rune boundary.
//
// A plain slice at MaxTermLength splits a multi-byte rune, and the invalid
// UTF-8 that produces is substituted by the JSON encoder and read back as a
// replacement character — so the query side would analyze the same word to a
// different term and never match it. (internal/textcut carries the general
// form of this rule; it is inlined here because this cut appends nothing and
// the two must not drift into needing each other.)
func truncateTerm[T ~string | ~[]byte](term T) T {
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

// Score is one term's BM25 contribution to one document, under this profile.
//
// A caller sums this over the query's terms. Split per term rather than
// scored per document so the caller can stream postings term by term — which
// is how the index is stored, and what keeps a query from materialising the
// whole corpus.
//
// AN INVALID PROFILE SCORES NOTHING, deliberately: a zero-valued Profile is a
// caller that never chose a tuning, and ranking it with a default would hide
// that behind a result list nobody can tell from a tuned one. Nothing ranks,
// which the caller's own tests see immediately.
func (p Profile) Score(idf float64, posting Posting, c Corpus) float64 {
	k1, b, ok := p.params()
	if !ok || posting.Freq <= 0 || idf <= 0 {
		return 0
	}
	avg := c.AvgLength
	if avg <= 0 {
		avg = 1
	}
	length := float64(posting.Length)
	if length <= 0 {
		// A document with no recorded length: score it as average rather
		// than as infinitely short, which would rank it above everything.
		length = avg
	}
	tf := float64(posting.Freq)
	norm := k1 * (1 - b + b*length/avg)
	return idf * (tf * (k1 + 1)) / (tf + norm)
}

// Snippet is the best window of body text for a hit, cut to limit bytes.
//
// It centres on the first query term the body actually contains, because the
// alternative (the document's opening) shows an agent the page's preamble
// rather than the sentence that made it a hit, and a snippet that does not
// contain the search term reads as a wrong result even when the ranking is
// right. A WHOLE TERM, as [Find] finds one through the one tokenizer, which is
// how a search decided the hit: centred on the first place the term occurred
// as a piece of a longer word, a snippet showed "redeployment" for a hit on
// "deploy" — and not the sentence holding the word, where that sentence was
// further on than the window reached.
//
// Cut on a RUNE boundary and, where one is near, on a word boundary. Bytes
// rather than runes for the limit itself: this is a budget against a prompt,
// and a prompt is billed in bytes on the wire.
func Snippet(body string, terms []string, limit int) string {
	return SnippetFrom(body, limit, func(folded string) int {
		at, _ := Find(folded, terms)
		return at
	})
}

// SnippetFrom is [Snippet] centred on the word first finds: first is handed
// body as the snippet is cut from it, its whitespace folded to single spaces,
// and answers the byte offset of the word to centre on in that, or -1 for
// none, which keeps the body's opening — as does an offset past the folded
// body's end, which no word it handed out is at: a snippet is a reader's
// glimpse of a hit, and a caller that mistook which text it was handed loses
// the centring, never the answer it is part of.
//
// FOR A CALLER THAT DECIDED ITS HIT ON LESS THAN EVERY WORD OF THE BODY — the
// words a redaction leaves between the markers it writes, say ([FindIn]) — so
// its snippet shows the word the hit was found by: centred by [Find] on the
// first term anywhere, it showed the same letters in a marker before that word
// (`token` in `[REDACTED:github-token]`), and in a body longer than the
// snippet left the word itself out.
func SnippetFrom(body string, limit int, first func(folded string) int) string {
	body = strings.Join(strings.Fields(body), " ")
	if body == "" || limit <= 0 {
		return ""
	}
	if len(body) <= limit {
		return body
	}
	start := 0
	if at := first(body); at > 0 && at < len(body) {
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
