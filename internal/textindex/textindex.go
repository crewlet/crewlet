// Package textindex is the engine's own lexical search: an analyzer that
// turns a document into terms, a BM25 ranker over the inverted list those
// terms are stored in, and [Reading.Find], which asks the same of one text no
// index holds — whether it holds a query's every term, and where the first is
// — read as it is written or through a JSON text's escapes ([Reading]), and
// [Reading.FindIn] of one handed over in pieces, which [Reading.MayHold] says
// of a text whether any stretches cut from it could answer.
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
//
// # One tokenizer, a profile per corpus
//
// The index, the query side and [Reading.Find] run the same tokenizer, so a
// search of one text finds what a search through the index would; a second
// reading would be a second answer. The BM25 tuning is a named profile, not
// package-level numbers: K1 is 1.2 on both, and B splits (see [ProseProfile]
// and [ChatProfile]) because a knowledge base and a chat want opposite length
// normalisations. The zero profile scores nothing rather than ranking by a
// tuning nobody chose.
package textindex

import (
	"bytes"
	"iter"
	"math"
	"math/bits"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// A Profile is one corpus's BM25 tuning, chosen by the index that ranks with
// it rather than baked into the arithmetic.
//
// The knowledge base and chat share one arithmetic; only their length
// distributions differ. A named string rather than a struct of numbers, so the
// zero value is INVALID: an unset struct would score with K1 = 0 and B = 0,
// ignoring term frequency and length.
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
		// K1 saturates term frequency: 1.2, low in the usual range,
		// because these documents repeat their subject; a higher K1
		// would rank by verbosity, not by query words covered.
		// B is length normalisation, 0 (ignore) to 1 (full): 0.75, the
		// standard value, kept deliberately. A short page that
		// mentions a term is usually ABOUT it, a long runbook usually
		// is not; toward 0 long pages crowd out the short answer,
		// toward 1 they bury the runbook that fits a broad query.
		return 1.2, 0.75, true
	case ChatProfile:
		// Prose's saturation, a MUCH WEAKER LENGTH PENALTY: the prose
		// argument inverts on a bimodal corpus ("ok" beside a pasted
		// stack trace). At 0.75 a two-token acknowledgement with the
		// query word outranks the paragraph that answers it; at 0.30
		// the substantive message wins while pasted logs still do not
		// beat a considered sentence. A test pins the reversal.
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
// Both the index and the query side call this one function.
//
//   - Case is folded.
//   - Tokens break on anything not a letter or digit (underscores too), keeping
//     "k8s" whole and splitting "deploy-pipeline".
//   - Stop words are NOT removed: a stop list makes titles like "The Why"
//     unfindable, and length normalisation already damps a common word.
//   - No stemming: it costs some recall but what a person typed is what was
//     searched, so a result they cannot explain never appears.
func Analyze(text string) map[string]int {
	if text == "" {
		return nil
	}
	terms := map[string]int{}
	Plain.tokenize(text, func(term []byte, _ int) bool {
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
// A repeated query word does NOT count twice: typing a word again must not
// double its weight. The order is kept only so a caller can report and
// highlight what it searched for.
//
// THROUGH THE SAME TOKENIZER as [Analyze]: a second loop here would drift
// silently, a query for a word plainly in the document returning nothing.
func Terms(query string) []string {
	var out []string
	seen := map[string]bool{}
	Plain.tokenize(query, func(term []byte, _ int) bool {
		if seen[string(term)] {
			return true
		}
		seen[string(term)] = true
		out = append(out, string(term))
		return true
	})
	return out
}

// A Reading is how a text's words are read where a search asks the text itself
// ([Reading.Find], [Reading.FindIn], [Reading.MayHold]): as written ([Plain]),
// or through the escapes a JSON text writes its strings in ([JSON]). A query's
// own terms are always read as written ([Terms]).
//
// A READING, NOT A SECOND TOKENIZER: it decides only which character each byte
// is part of ([Reading.char]); what a word is ([MinTermLength],
// [MaxTermLength]) is written once.
//
// The zero Reading is no reading and finds nothing, so a caller that never
// chose one is not answered by a reading nobody chose.
type Reading string

const (
	// Plain reads a text's characters as it writes them: every letter and
	// digit is one, whatever comes before it.
	Plain Reading = "plain"

	// JSON reads a JSON text, or as much of one as was written, through
	// its string escapes as a JSON decoder does: `\n`, `\r`, `\t`, `\b`,
	// `\f` are the breaks they stand for, `\"`, `\\`, `\/` the characters
	// they escape, `\u` and four hex digits the character they name (a
	// surrogate pair as one character, a lone half as none of a word). A
	// backslash that opens none of those is itself.
	//
	// A JSON TEXT'S WORDS ARE ITS STRINGS': read as written, `"cd repo\ngit"`
	// has the word `ngit`. Offsets are those of the text as written.
	//
	// ONLY FOR A TEXT THAT IS JSON: a text that writes a backslash as itself
	// (a Windows path's `\new`, a regexp's `\d+`) loses the letter after it.
	JSON Reading = "json"
)

// Readings are every reading a text's words are read by.
var Readings = []Reading{Plain, JSON}

// Valid reports whether r is a reading this package reads by.
func (r Reading) Valid() bool { return slices.Contains(Readings, r) }

// Find reports where in text the earliest occurrence of any of terms begins —
// the byte offset of its first character, -1 where none occurs — and whether
// text holds every one of them. Terms are a query's as [Terms] makes them, and
// each is found only as a whole term of text as r reads it, as [Analyze] makes
// terms: "deploy" is not in "redeployment", and "Deploy" is in
// "deploy-pipeline". No terms at all are found nowhere: (-1, false), as a
// query of no words matches nothing.
//
// THROUGH THE ONE TOKENIZER, for [Terms]' reason: any other reading (a
// substring, another case rule) would match differently from a search through
// the index. And STREAMING: one pass, no term held past the comparison, a stop
// once every term has been seen, so mebibytes cost no allocation per word.
func (r Reading) Find(text string, terms []string) (first int, all bool) {
	return r.FindIn(func(yield func(int, string) bool) { yield(0, text) }, terms)
}

// FindIn is [Reading.Find] over a text handed over in pieces, in order, each
// with the byte offset in the text it begins at: first is an offset in the
// whole text. A term is read within one piece and never across two, each piece
// read from its first byte, so pieces are cut where no term runs on — the
// stretches a redaction left between the markers it wrote, say
// (internal/redact's OwnWords), which a reader of the redacted text is shown as
// its own words, apart from the marker's.
//
// IN PIECES RATHER THAN A COPY OF THE TEXT WITH THE GAPS BLANKED, which would
// be one more copy of a text that may run to mebibytes, made only to be
// scanned once.
func (r Reading) FindIn(pieces iter.Seq2[int, string], terms []string) (first int, all bool) {
	if len(terms) == 0 || !r.Valid() {
		return -1, false
	}
	seen := make(map[string]bool, len(terms))
	for _, term := range terms {
		seen[term] = false
	}
	first, missing := -1, len(seen)
	for offset, piece := range pieces {
		r.tokenize(piece, func(term []byte, at int) bool {
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
// terms, as [Reading.FindIn] reads stretches: whether each term is inside a
// word some stretch reads, whole or as a piece of a longer word. A caller asks
// before paying to cut the stretches (a redaction, say): false says FindIn
// finds not every term, true only that they are worth reading.
//
// A STRETCH CAN CUT A WORD SHORT, NEVER MAKE ONE UP. A term can be a stretch's
// whole word and only a piece of the text's: `deploy` before a redacted token,
// where the text reads `deployghp`. Find of the whole text misses it, so what
// the redaction hid would decide whether a hit is shown. MayHold thus reads the
// text's words whole, at any length. A [JSON] stretch can also begin inside an
// escape ([jsonMayHold]).
func (r Reading) MayHold(text string, terms []string) bool {
	switch {
	case len(terms) == 0:
		return false
	case r == JSON:
		return jsonMayHold(text, terms)
	case r != Plain:
		return false
	}
	wanted := make([][]byte, len(terms))
	for i, term := range terms {
		wanted[i] = []byte(term)
	}
	r.words(text, func(word []byte, _ int) bool {
		wanted = slices.DeleteFunc(wanted, func(term []byte) bool { return bytes.Contains(word, term) })
		return len(wanted) > 0
	})
	return len(wanted) == 0
}

// jsonMayHold is [Reading.MayHold] for [JSON]: whether each of terms is inside
// a word some stretch of text reads, the stretch read from its first byte.
//
// A STRETCH CAN BEGIN INSIDE AN ESCAPE, where no word read from the text's
// start begins, and read what is left of the escape as written: after the
// backslash of `\ngit` it reads `ngit`, and at the second backslash of `\\` it
// reads the escape that backslash opens. Each such word begins with an OPENING
// and goes on as the word the text reads from where the opening ends. So words
// are read whole beside every opening and its following word; each opening is
// asked only for the terms that could begin inside it, followed a byte at a
// time. One pass. A stretch that ENDS inside an escape reads a word cut short.
func jsonMayHold(text string, terms []string) bool {
	h := newHeldTerms(terms)
	if h == nil {
		return false
	}
	var word []byte
	for at := 0; at < len(text) && h.left > 0; {
		h.arrive(at)
		c, n := JSON.char(text[at:])
		if isWordChar(c) {
			from := len(word)
			word = utf8.AppendRune(word, unicode.ToLower(c))
			h.follow(word[from:])
		} else {
			h.inWord(word)
			word = word[:0]
			h.endWord()
		}
		h.openings(text[at:], at, n)
		at += n
	}
	h.inWord(word)
	return h.left == 0
}

// heldTerms is what [jsonMayHold] has found of its terms, and what it follows
// on: for each term not found yet, the lengths of its openings that an escape's
// opening ended in, each waiting on the word after the opening to say whether
// the rest of the term follows — bit k of waiting[i] set where terms[i][:k]
// ended one. A term is at most [MaxTermLength] bytes, so its waiting lengths
// fit one word of bits.
type heldTerms struct {
	terms   [][]byte
	found   []bool
	waiting []uint64
	left    int

	// waits says some term's waiting lengths are not empty, so a word's
	// bytes that nothing waits on cost no look at every term.
	waits bool

	// later are openings whose word begins further on than the token that
	// read them: the escape a `\\`'s second backslash opens ends past the
	// `\\`, and its word goes on from there.
	later []laterOpening
}

// laterOpening is the waiting lengths an opening ended in, for each term, from
// the byte offset at on, where the word after it begins.
type laterOpening struct {
	at      int
	waiting []uint64
}

// newHeldTerms is nothing found of terms yet, or nil where one of them is no
// term a stretch can read — empty, or longer than [MaxTermLength] — so the
// stretches cannot hold every one.
func newHeldTerms(terms []string) *heldTerms {
	h := &heldTerms{terms: make([][]byte, len(terms)), found: make([]bool, len(terms)),
		waiting: make([]uint64, len(terms)), left: len(terms)}
	for i, term := range terms {
		if term == "" || len(term) > MaxTermLength {
			return nil
		}
		h.terms[i] = []byte(term)
	}
	return h
}

// find marks term i found.
func (h *heldTerms) find(i int) {
	if !h.found[i] {
		h.found[i], h.waiting[i] = true, 0
		h.left--
	}
}

// inWord finds every term word holds, whole or as a piece of it.
func (h *heldTerms) inWord(word []byte) {
	if len(word) == 0 {
		return
	}
	for i, term := range h.terms {
		if !h.found[i] && bytes.Contains(word, term) {
			h.find(i)
		}
	}
}

// follow goes on with every waiting term through chars, the next bytes of the
// word being read: a waiting length goes on where its term's next byte is the
// byte read, and a term whose every byte has followed is found.
func (h *heldTerms) follow(chars []byte) {
	if !h.waits {
		return
	}
	h.waits = false
	for i, waiting := range h.waiting {
		term := h.terms[i]
		for _, c := range chars {
			if waiting == 0 {
				break
			}
			next := uint64(0)
			for w := waiting; w != 0; w &= w - 1 {
				k := bits.TrailingZeros64(w)
				switch {
				case term[k] != c:
				case k+1 == len(term):
					h.find(i)
				default:
					next |= 1 << (k + 1)
				}
			}
			waiting = next
			if h.found[i] {
				waiting = 0
			}
		}
		h.waiting[i] = waiting
		h.waits = h.waits || waiting != 0
	}
}

// endWord drops every waiting length: the word they waited on ended.
func (h *heldTerms) endWord() {
	if h.waits {
		clear(h.waiting)
		h.waits = false
	}
}

// arrive takes up the openings whose word begins at the byte offset at.
func (h *heldTerms) arrive(at int) {
	if len(h.later) == 0 {
		return
	}
	kept := h.later[:0]
	for _, o := range h.later {
		if o.at > at {
			kept = append(kept, o)
			continue
		}
		for i, waiting := range o.waiting {
			if !h.found[i] && waiting != 0 {
				h.waiting[i] |= waiting
				h.waits = true
			}
		}
	}
	h.later = kept
}

// openings reads the openings of the token text opens with, at the byte offset
// at and n bytes long, as [JSON] read it: after an escape it decodes, the
// escape's body as written, whose word goes on after the escape — but for a
// surrogate pair's high half, which a stretch begun inside it reads before the
// low half read alone, no character of a word; and after a `\\`, the character
// the escape its second backslash opens names, where that is one of a word.
func (h *heldTerms) openings(text string, at, n int) {
	if n < 2 || text[0] != '\\' {
		return
	}
	var lead [utf8.UTFMax + 1]byte
	switch text[1] {
	case 'b', 'f', 'n', 'r', 't':
		h.waits = h.open(lowerASCII(lead[:0], text[1:2]), h.waiting) || h.waits
	case 'u':
		if n == 12 {
			h.open(lowerASCII(lead[:0], text[1:6]), nil)
			h.waits = h.open(lowerASCII(lead[:0], text[7:12]), h.waiting) || h.waits
			return
		}
		h.waits = h.open(lowerASCII(lead[:0], text[1:6]), h.waiting) || h.waits
	case '\\':
		c, m := jsonEscape(text[1:])
		if m == 0 || !isWordChar(c) {
			return
		}
		waiting := make([]uint64, len(h.terms))
		if h.open(utf8.AppendRune(lead[:0], unicode.ToLower(c)), waiting) {
			h.later = append(h.later, laterOpening{at: at + 1 + m, waiting: waiting})
		}
	}
}

// open finds every term not found yet that lead holds, and adds to into, for
// each other, the lengths of it lead ends in — where into is not nil, the word
// after lead being one a stretch can go on into — answering whether it added
// any.
func (h *heldTerms) open(lead []byte, into []uint64) bool {
	added := false
	for i, term := range h.terms {
		if h.found[i] {
			continue
		}
		if bytes.Contains(lead, term) {
			h.find(i)
			continue
		}
		if into == nil {
			continue
		}
		for k := 1; k < len(term) && k <= len(lead); k++ {
			if bytes.HasSuffix(lead, term[:k]) {
				into[i] |= 1 << k
				added = true
			}
		}
	}
	return added
}

// lowerASCII appends s to dst, its ASCII letters in lower case: an escape's
// body, whose letters and hex digits a stretch reads as a word's characters.
func lowerASCII(dst []byte, s string) []byte {
	for i := range len(s) {
		c := s[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		dst = append(dst, c)
	}
	return dst
}

// tokenize splits text, as r reads it, into terms and hands each to yield, in
// order, with the byte offset in text where the term's first character is; it
// stops where yield answers false. A term is a word of text ([Reading.words])
// of at least [MinTermLength] bytes, cut at [MaxTermLength]. A term is handed
// over as bytes that are valid only until yield returns: a caller that keeps
// one copies it, and one that only compares it allocates nothing.
func (r Reading) tokenize(text string, yield func(term []byte, at int) bool) {
	r.words(text, func(word []byte, at int) bool {
		if len(word) < MinTermLength {
			return true
		}
		if len(word) > MaxTermLength {
			word = truncateTerm(word)
		}
		return yield(word, at)
	})
}

// words hands each word of text, as r reads its characters ([Reading.char]),
// to yield — a run of letters and digits as long as it runs, case folded — in
// order, with the byte offset in text where its first character is written; it
// stops where yield answers false. A word is handed over as bytes valid only
// until yield returns.
//
// THE ONE READING OF WHAT A WORD IS, which [Reading.tokenize] makes terms of
// and [Reading.MayHold] reads whole: two readings would be two answers to
// whether a text holds a word.
func (r Reading) words(text string, yield func(word []byte, at int) bool) {
	var word []byte
	start := 0
	for at := 0; at < len(text); {
		c, n := r.char(text[at:])
		switch {
		case isWordChar(c):
			if len(word) == 0 {
				start = at
			}
			word = utf8.AppendRune(word, unicode.ToLower(c))
		case len(word) > 0:
			if !yield(word, start) {
				return
			}
			word = word[:0]
		}
		at += n
	}
	if len(word) > 0 {
		yield(word, start)
	}
}

// char is the character text opens with as r reads it, and how many bytes it
// is written in: a rune, or under [JSON] an escape ([jsonEscape]). A byte that
// begins no rune is [utf8.RuneError], one byte long, as a range over a string
// reads it.
func (r Reading) char(text string) (rune, int) {
	if r == JSON && text[0] == '\\' {
		if c, n := jsonEscape(text); n > 0 {
			return c, n
		}
	}
	return utf8.DecodeRuneInString(text)
}

// isWordChar reports whether c is a character of a word: a letter or a digit,
// in any script.
func isWordChar(c rune) bool { return unicode.IsLetter(c) || unicode.IsDigit(c) }

// jsonEscape is the character the escape text opens with names, and how many
// bytes it is written in — or 0 where text opens with no escape a JSON string
// writes: a backslash and one of `"\/bfnrt`, or `\u` and four hex digits, two
// of those where the first is a surrogate pair's high half and the second its
// low, read as the one character they name. A lone half is
// [unicode.ReplacementChar], as a JSON decoder reads one.
func jsonEscape(text string) (rune, int) {
	if len(text) < 2 || text[0] != '\\' {
		return 0, 0
	}
	switch text[1] {
	case '"', '\\', '/':
		return rune(text[1]), 2
	case 'b':
		return '\b', 2
	case 'f':
		return '\f', 2
	case 'n':
		return '\n', 2
	case 'r':
		return '\r', 2
	case 't':
		return '\t', 2
	case 'u':
		high, ok := hexUnit(text[2:])
		switch {
		case !ok:
			return 0, 0
		case !utf16.IsSurrogate(high):
			return high, 6
		}
		if len(text) >= 12 && text[6] == '\\' && text[7] == 'u' {
			if low, ok := hexUnit(text[8:]); ok {
				if c := utf16.DecodeRune(high, low); c != unicode.ReplacementChar {
					return c, 12
				}
			}
		}
		return unicode.ReplacementChar, 6
	}
	return 0, 0
}

// hexUnit is the UTF-16 code unit the four hex digits text opens with write,
// and false where it opens with no four.
func hexUnit(text string) (rune, bool) {
	if len(text) < 4 {
		return 0, false
	}
	var unit rune
	for i := range 4 {
		var digit byte
		switch c := text[i]; {
		case '0' <= c && c <= '9':
			digit = c - '0'
		case 'a' <= c && c <= 'f':
			digit = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			digit = c - 'A' + 10
		default:
			return 0, false
		}
		unit = unit<<4 | rune(digit)
	}
	return unit, true
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
// It centres on the first query term the body contains: the opening shows the
// preamble. A WHOLE TERM FIRST, as [Reading.Find] finds one (how a lexical search
// decided the hit): a piece of a longer word ("redeployment" for "deploy")
// could centre away from the real sentence. THEN A WORD HOLDING ONE ([holding]):
// a hybrid search snippets every candidate it fused, including those only the
// semantic ranker found, and with no stemming such a body often holds the
// query's words only inside longer ones.
//
// Cut on a RUNE boundary and, where near, a word boundary. The limit is in
// bytes, a budget against a prompt billed in bytes on the wire.
func Snippet(body string, terms []string, limit int) string {
	return SnippetFrom(body, limit, func(folded string) int {
		if at, _ := Plain.Find(folded, terms); at >= 0 {
			return at
		}
		return holding(folded, terms)
	})
}

// holding is where the first word of text holding one of terms begins — whole,
// or as a piece of a longer word — or -1 where none does: a word as
// [Reading.words] reads one as written, case folded, as the terms are.
func holding(text string, terms []string) int {
	wanted := make([][]byte, 0, len(terms))
	for _, term := range terms {
		if term != "" {
			wanted = append(wanted, []byte(term))
		}
	}
	at := -1
	if len(wanted) == 0 {
		return at
	}
	Plain.words(text, func(word []byte, start int) bool {
		if slices.ContainsFunc(wanted, func(term []byte) bool { return bytes.Contains(word, term) }) {
			at = start
			return false
		}
		return true
	})
	return at
}

// SnippetFrom is [Snippet] centred on the word first finds: first is handed
// body as the snippet is cut from it, its whitespace folded to single spaces,
// and answers the byte offset of the word to centre on in that, or -1 for
// none, which keeps the body's opening, as does an offset past the folded
// body's end: a caller that mistook which text it was handed loses the
// centring, never the answer.
//
// FOR A CALLER THAT DECIDED ITS HIT ON LESS THAN EVERY WORD OF THE BODY (the
// words a redaction leaves between its markers, [Reading.FindIn]), so the
// snippet shows the word the hit was found by: [Reading.Find] could centre on
// the same letters in a marker (`token` in `[REDACTED:github-token]`) and
// leave the word itself out.
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
