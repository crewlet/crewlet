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
	"math"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/textcut"
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
			// THE SHARED CUT, not a private one. A plain slice at
			// MaxTermLength splits a multi-byte rune, and the
			// invalid UTF-8 that produces is substituted by the
			// JSON encoder and read back as a replacement
			// character — so the query side would analyze the same
			// word to a different term and never match it.
			// [textcut.Bytes] is exactly that rule for exactly
			// this case (a value consumed by something that does
			// not read prose, where an appended marker would
			// become part of the value). This was a hand-rolled
			// copy of it whose own doc argued the two "must not
			// drift into needing each other", which is the
			// argument this tree has lost four times: the copy had
			// already drifted, returning the raw unsafe slice on
			// the one branch the walk-back could not satisfy.
			term = textcut.Bytes(term, MaxTermLength)
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
// alternative (the document's opening) shows an agent the page's preamble
// rather than the sentence that made it a hit, and a snippet that does not
// contain the search term reads as a wrong result even when the ranking is
// right.
//
// Cut on a RUNE boundary and, where one is near, on a word boundary. Bytes
// rather than runes for the limit itself: this is a budget against a prompt,
// and a prompt is billed in bytes on the wire.
//
// # What a reader gets back, and where the rest of it is
//
// ALWAYS MARKED AT THE END THAT WAS CUT, and only there: an elided head opens
// with "…", an elided tail closes with one, and a window that reaches the
// document's own start or end claims nothing. A snippet is a POINTER — its
// job is to say which document to open — and the document itself is whole in
// the replicated estate, reachable through the seat's own knowledge and
// tracker tools (`search_knowledge` returns the id; reading the page or the
// work item returns the body) and through the same LexicalSource the indexer
// tokenises it from — [github.com/crewlet/crewlet/internal/search] owns both.
// Nothing here is the only copy of anything.
//
// THE MARKER IS NOT COUNTED AGAINST limit, which is [textcut.Ellipsis]'s rule
// and is deliberate for the same reason: the budget bounds the CONTENT, and a
// caller that needs the total bounded passes a smaller limit. So a result may
// run up to two markers — six bytes — past limit. That is safe here because
// the callers' budget is a prompt-cost guide rather than a ceiling anything
// refuses at; a value with a limit somebody enforces wants [textcut.Within]
// instead.
//
// # A limit of zero or less is EMPTY here, and UNBOUNDED in knowledge.Snippet
//
// Two functions of the same name doing the same job in one tree read opposite
// meanings off the same zero, so the contract is stated here rather than
// discovered: this one yields the empty string, and the difference is not an
// oversight to fold away.
//
// A budget that is a WINDOW cannot read its own absence as "no window". This
// function's limit is the width of the window it centres on the match, and a
// width of zero is zero bytes of it — the same reading [textcut.Bytes] gives
// a non-positive budget, which is the tree's authority on byte cuts and the
// family this one belongs to. Reading it as unbounded would make an unset
// field return the whole document, times the hit count, times the phase's
// round cap, into a prompt somebody pays for: a cap of 0 that returns
// everything is the opposite of a cap, and it fails expensively and silently
// where the empty snippet fails visibly and free.
//
// The other one, [github.com/crewlet/crewlet/internal/knowledge.Snippet], is
// not a window — it is a head cut from the document's own start, and it
// documents zero as unbounded. No production caller passes it zero (both pass
// a named constant), so the divergence costs nothing today and is purely a
// trap for the next reader who assumes one contract while holding the other.
// Hence this paragraph, and hence the test beside it that pins THIS side.
//
// A limit too small to hold content lands in the same place for the same
// reason, and that is the honest answer rather than a marker alone: a snippet
// that is only an ellipsis costs prompt bytes to say nothing, where an absent
// snippet leaves the hit's title and id — which are what a pointer is for —
// standing on their own.
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
		for start > 0 && !utf8.RuneStart(body[start]) {
			start--
		}
	}
	end := start + limit
	if end >= len(body) {
		return trimToWord(body[start:], start > 0, false)
	}
	for end > start && !utf8.RuneStart(body[end]) {
		end--
	}
	return trimToWord(body[start:end], start > 0, true)
}

// firstTermIndex finds where the earliest query term appears, ignoring case,
// as an offset into BODY's own bytes, or -1.
//
// SCANNED IN BODY'S OWN COORDINATES rather than in a lowercased copy of it,
// and that is a correctness requirement rather than a preference. Lowercasing
// maps rune by rune, and a lowercase rune can be LONGER than the one it came
// from: U+023A "Ⱥ" is two bytes and lowercases to U+2C65 "ⱥ" at three. So an
// index into the folded copy is not an index into body — and [Snippet] slices
// body with what this returns. A body carrying enough of those before the
// match indexed PAST len(body) and panicked the search that asked for the
// snippet (`index out of range`, measured on 100 of them ahead of the term),
// and short of the panic every such rune slid the window one byte further
// from the sentence the reader came for.
//
// Folding a copy and mapping each of its bytes back to the rune that produced
// it would answer the same question, at one int per byte of the document per
// hit. Folding the CANDIDATE RUNE instead allocates nothing — where the old
// shape allocated a whole second copy of the body — and cannot disagree with
// itself about where a byte came from. The scan is bounded: a term is at most
// [MaxTermLength] bytes, so a candidate position is rejected after at most
// that many rune comparisons, and the first one rejects almost all of them.
func firstTermIndex(body string, terms []string) int {
	// The first rune of each term, folded once rather than per candidate
	// position. Folded on BOTH sides throughout: a term [Terms] produced is
	// already lowercase, but this is exported surface and a term that
	// silently never matches is worse than one compare.
	heads := make([]rune, 0, len(terms))
	for _, term := range terms {
		head, _ := utf8.DecodeRuneInString(term)
		heads = append(heads, unicode.ToLower(head))
	}
	for i, r := range body {
		lower := unicode.ToLower(r)
		for t, term := range terms {
			if lower != heads[t] {
				continue
			}
			if foldedPrefix(body[i:], term) {
				return i
			}
		}
	}
	return -1
}

// foldedPrefix reports whether s begins with term, compared rune by rune under
// the same [unicode.ToLower] the tokenizer lowercases with — so a term the
// analyzer produced matches the text it was produced from, whatever case that
// text is in.
func foldedPrefix(s, term string) bool {
	for _, want := range term {
		if s == "" {
			return false
		}
		got, size := utf8.DecodeRuneInString(s)
		if unicode.ToLower(got) != unicode.ToLower(want) {
			return false
		}
		s = s[size:]
	}
	return true
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
