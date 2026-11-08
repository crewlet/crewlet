package textindex_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/textindex"
)

// FIND NAMES EVERY TERM AND THE FIRST, AS THE ANALYZER READS THE TEXT. A text
// holds a query's terms only as whole terms the analyzer makes of it — case
// folded, split on anything that is not a letter or a digit, a word cut where
// the analyzer cuts it — never as a piece of a longer word; it holds them all
// or it does not; and the first is the byte offset where the earliest of them
// begins, whichever the query named first. Over every case, what Find says
// agrees with Analyze: every term holds where Analyze counts every one, the
// term at the offset is one of them, and Analyze finds none before it.
//
// Mutation: find a term as a substring of the text folded to lower case, and
// a case goes red.
func TestFindNamesEveryTermAndTheFirst(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", textindex.MaxTermLength+6)
	for _, c := range []struct {
		name, text, query string
		first             int
		all               bool
	}{
		{"every term, the earliest first", "deploy the pipeline", "pipeline deploy", 0, true},
		{"a term inside a longer word is none", "redeployment done", "deploy", -1, false},
		{"case folded", "We DEPLOY now", "deploy", 3, true},
		{"split on what is not a letter or digit", "work_items-deploy", "items deploy", 5, true},
		{"one term of two", "deploy only", "deploy rollback", 0, false},
		{"bytes, not characters, before it", "über deploy", "deploy", len("über "), true},
		{"the earliest, whichever the query named first", "alpha beta gamma", "gamma beta", 6, true},
		{"a long word cut as the analyzer cuts it", "see " + long + " here", long, 4, true},
		{"a one-letter word is no term", "a b deploy", "a deploy", 4, true},
		{"a term written twice is found at its first", "ship it, then ship more", "ship", 0, true},
		{"nothing to find", "deploy", "", -1, false},
	} {
		terms := textindex.Terms(c.query)
		first, all := textindex.Plain.Find(c.text, terms)
		if first != c.first || all != c.all {
			t.Errorf("%s: Find(%q, %q) = (%d, %v), want (%d, %v)", c.name, c.text, terms, first, all,
				c.first, c.all)
		}
		analyzed := textindex.Analyze(c.text)
		every := len(terms) > 0 && !slices.ContainsFunc(terms, func(term string) bool { return analyzed[term] == 0 })
		if all != every {
			t.Errorf("%s: Find says every term is held: %v, and Analyze counts %v of %q", c.name, all,
				analyzed, terms)
		}
		if first < 0 {
			continue
		}
		if at := textindex.Terms(c.text[first:]); len(at) == 0 || !slices.Contains(terms, at[0]) {
			t.Errorf("%s: the text at %d reads %q, want one of %q there", c.name, first, at, terms)
		}
		before := textindex.Analyze(c.text[:first])
		if slices.ContainsFunc(terms, func(term string) bool { return before[term] > 0 }) {
			t.Errorf("%s: Analyze finds one of %q before %d: %v", c.name, terms, first, before)
		}
	}
}

// A TEXT MAY HOLD WHAT ANY STRETCH OF IT FINDS. Wherever a stretch cut from a
// text — a redaction's own words between its markers, say — finds a term, a
// word the cut ended short among them, MayHold of the text is true: over every
// case, every stretch of the text is read for each term. So a term the text
// holds only as a piece of a longer word — glued to what a redaction would
// take out, or past where the analyzer cuts a long word — may be held, where
// Find of the text whole finds it nowhere; a term in no word of the text,
// whole or in part, may not; and every term is asked, not one.
//
// Mutation: ask Find of the text whole, or read its words cut where the
// analyzer cuts them, and a case goes red.
func TestATextMayHoldWhatAnyStretchOfItFinds(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", textindex.MaxTermLength)
	for _, c := range []struct {
		name, text, query string
		may               bool
	}{
		{"a whole word", "deploy the pipeline", "pipeline deploy", true},
		{"a piece a stretch can end", "deployghp_Q7wErTy9", "deploy", true},
		{"a piece at a word's end", "ghp_Q7wErTy9deploy", "deploy", true},
		{"a piece past where a word is cut", long + "deploy now", "deploy", true},
		{"case folded", "DEPLOYghp now", "deploy", true},
		{"in no word", "deploy now", "rollback", false},
		{"every term, not one", "deployghp only", "deploy rollback", false},
		{"across two words is in neither", "de ploy", "deploy", false},
		{"nothing to find", "deploy", "", false},
	} {
		terms := textindex.Terms(c.query)
		if got := textindex.Plain.MayHold(c.text, terms); got != c.may {
			t.Errorf("%s: MayHold(%q, %q) = %v, want %v", c.name, c.text, terms, got, c.may)
		}
		for _, term := range terms {
			found := false
			for from := 0; from < len(c.text) && !found; from++ {
				for to := from + 1; to <= len(c.text) && !found; to++ {
					_, found = textindex.Plain.Find(c.text[from:to], []string{term})
				}
			}
			if found && !textindex.Plain.MayHold(c.text, []string{term}) {
				t.Errorf("%s: a stretch of %q finds %q, and MayHold of the text says it may not", c.name,
					c.text, term)
			}
		}
	}
}

// A SNIPPET IS CENTRED ON THE WORD ITS CALLER FINDS. SnippetFrom hands its
// caller the body as the snippet is cut from it, its whitespace folded, and
// centres on the offset the caller answers, a third of the window before it,
// as Snippet places its own; where the caller finds nothing, or answers past
// the body's end, the snippet is the body's opening. And Snippet is
// SnippetFrom centred on the first whole term Find finds.
//
// Mutation: hand the caller the body before its whitespace is folded, centre
// on an offset past the body's end, or centre Snippet anywhere but where Find
// finds its term, and a check goes red.
func TestASnippetIsCentredOnTheWordItsCallerFinds(t *testing.T) {
	t.Parallel()
	body := "token [REDACTED:github-token]" + strings.Repeat(" \n   filler", 40) + " and the token was rotated"
	folded := strings.Join(strings.Fields(body), " ")
	own := strings.LastIndex(folded, "token was")
	got := textindex.SnippetFrom(body, 120, func(text string) int {
		if text != folded {
			t.Errorf("the caller is handed %q, want the body folded as the snippet is cut from it", text)
		}
		return own
	})
	if !strings.Contains(got, "the token was rotated") || !strings.HasPrefix(got, "…") {
		t.Errorf("the snippet centred on the caller's word reads %q, want the window around it", got)
	}
	opening := textindex.SnippetFrom(body, 120, func(string) int { return -1 })
	if !strings.HasPrefix(opening, "token [REDACTED:github-token]") {
		t.Errorf("the snippet the caller finds nothing for reads %q, want the body's opening", opening)
	}
	if past := textindex.SnippetFrom(body, 120, func(text string) int { return len(text) + 100 }); past != opening {
		t.Errorf("the snippet a caller answers past the body's end for reads %q, want the body's opening", past)
	}
	if whole, want := textindex.Snippet(body, []string{"rotated"}, 120), textindex.SnippetFrom(body, 120,
		func(text string) int { return strings.Index(text, "rotated") }); whole != want {
		t.Errorf("Snippet reads %q, and SnippetFrom centred where Find finds the term %q", whole, want)
	}
}

// pieces is text handed over as FindIn reads it: each piece with the offset it
// begins at.
func pieces(text string, cuts ...int) func(yield func(int, string) bool) {
	return func(yield func(int, string) bool) {
		from := 0
		for _, cut := range append(cuts, len(text)) {
			if !yield(from, text[from:cut]) {
				return
			}
			from = cut
		}
	}
}

// FIND IN A TEXT'S PIECES READS THEM AS THE TEXT: every term is looked for in
// every piece, the first is an offset in the whole text however many pieces
// came before it, and a term is read within one piece — a word a cut parts is
// two pieces of a word, not the word — so a text cut where no term runs on is
// read exactly as Find reads it whole.
//
// Mutation: give the first as an offset in its own piece, or stop at the first
// piece, and a case goes red.
func TestFindInReadsATextsPiecesAsTheText(t *testing.T) {
	t.Parallel()
	text := "alpha [cut] beta gamma"
	for _, c := range []struct {
		name  string
		cuts  []int
		query string
		first int
		all   bool
	}{
		{"one piece is Find", nil, "gamma beta", 12, true},
		{"the first counted in the whole text", []int{6, 11}, "gamma beta", 12, true},
		{"every piece is read", []int{6, 11}, "alpha gamma", 0, true},
		{"a word a cut parts is not the word", []int{14}, "beta", -1, false},
	} {
		first, all := textindex.Plain.FindIn(pieces(text, c.cuts...), textindex.Terms(c.query))
		if first != c.first || all != c.all {
			t.Errorf("%s: FindIn = (%d, %v), want (%d, %v)", c.name, first, all, c.first, c.all)
		}
		if c.cuts == nil {
			if wf, wa := textindex.Plain.Find(text, textindex.Terms(c.query)); wf != first || wa != all {
				t.Errorf("%s: FindIn of one piece says (%d, %v) and Find (%d, %v)", c.name, first, all, wf, wa)
			}
		}
	}
}

// escaped is the escape a JSON text writes the UTF-16 code unit unit in: a
// backslash, `u` and its four hex digits.
func escaped(unit rune) string { return fmt.Sprintf(`\u%04x`, unit) }

// A JSON TEXT'S WORDS ARE READ THROUGH ITS ESCAPES. Read as JSON, a string's
// escaped line break, tab, quote or backslash is the character it escapes,
// never a letter glued to the word after it; `\u` and four hex digits is the
// character it names, a surrogate pair's two halves the one they name
// together, a letter of a word where it is one and a lone half none; and a
// backslash that opens no escape is itself. A word's offset is where it begins
// as the text is written. Read as written, the word that opens a line, or one
// with a named letter, is in no whole word of the same text, and the escape's
// letter glued to the word after it is one. A reading nobody chose finds
// nothing.
//
// Mutation: read a JSON text as written, decode its line breaks and not its
// named characters, or read an escaped backslash as opening an escape, and a
// case goes red.
func TestAJSONTextsWordsAreReadThroughItsEscapes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, text, query string
		// at is the text the first term begins at, "" for none.
		at         string
		all, plain bool
	}{
		{"a line's first word", `{"command":"cd repo\ngit push origin main"}`, "git push", "git push",
			true, false},
		{"a word after a tab", `{"content":"func main() {\n\tfmt.Println(1)\n}"}`, "fmt", "fmt.Println",
			true, false},
		{"an escaped quote", `{"say":"\"quoted\" words"}`, "quoted words", "quoted", true, true},
		{"an escaped backslash before a letter", `{"path":"C:\\new\\test"}`, "new test", "new", true, true},
		{"a named letter", `{"name":"caf` + escaped('é') + ` au lait"}`, "café", "caf", true, false},
		{"a pair names one character", `{"x":"go` + escaped(0xd83d) + escaped(0xde00) + `lang"}`, "lang",
			"lang", true, false},
		{"a lone half is no letter", `{"x":"ab` + escaped(0xd83d) + `cd"}`, "cd", `cd"`, true, false},
		{"a backslash opening no escape", `ab\qcd`, "qcd", "qcd", true, true},
		{"the escape's letter is no word", `{"command":"cd repo\ngit push"}`, "ngit", "", false, true},
	} {
		terms := textindex.Terms(c.query)
		want := -1
		if c.at != "" {
			want = strings.Index(c.text, c.at)
		}
		if first, all := textindex.JSON.Find(c.text, terms); first != want || all != c.all {
			t.Errorf("%s: JSON.Find(%q, %q) = (%d, %v), want (%d, %v)", c.name, c.text, terms, first, all,
				want, c.all)
		}
		if _, all := textindex.Plain.Find(c.text, terms); all != c.plain {
			t.Errorf("%s: Plain.Find(%q, %q) says every term is held: %v, want %v", c.name, c.text, terms,
				all, c.plain)
		}
	}
	for _, r := range []textindex.Reading{"", "yaml"} {
		if first, all := r.Find("deploy", []string{"deploy"}); first != -1 || all || r.Valid() ||
			r.MayHold("deploy", []string{"deploy"}) {
			t.Errorf("the reading %q finds (%d, %v), and is valid: %v", r, first, all, r.Valid())
		}
	}
}
