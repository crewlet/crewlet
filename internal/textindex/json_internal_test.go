package textindex

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// escapedUnit is the escape a JSON text writes the UTF-16 code unit unit in: a
// backslash, `u` and its four hex digits.
func escapedUnit(unit rune) string { return fmt.Sprintf(`\u%04x`, unit) }

// A JSON TEXT MAY HOLD WHAT ANY STRETCH OF IT READS. A stretch cut from a JSON
// text — a redaction's own words between its markers — can begin and end
// anywhere, inside an escape too, and JSON reads it from its first byte: begun
// after the backslash of an escaped line break, it reads the break's letter as
// the first of the word after it; inside `\u` and its digits, those as a word's;
// at the second backslash of an escaped one, the escape that backslash opens;
// inside a surrogate pair, either half alone. Over every stretch of every
// case's text, every term JSON reads in it is one MayHold of the whole text
// says the text may hold — so a text whose stretches a reader is shown hold a
// term is never passed over — and a term no stretch reads is one it may not.
//
// Mutation: read only the words JSON reads from the text's start, have MayHold
// read the text as written while a stretch is read as JSON, or leave out a
// pair's low half, or the escape a `\\`'s second backslash opens, and a case
// goes red.
func TestAJSONTextMayHoldWhatAnyStretchOfItReads(t *testing.T) {
	t.Parallel()
	u := escapedUnit
	texts := []string{
		`{"command":"cd repo\ngit push origin main","content":"package main\n\nfunc main() {\n\tfmt.Println(1)\n}"}`,
		`caf` + u('é') + ` ` + u('A') + `bc \` + u('A') + `bc \\\` + u('B') + `x ` + u('é') + u('é') + ` \ngit` +
			u('é') + ` ` + u('A') + u('é') + ` \` + u('A') + u('é') + ` \n` + u('A') + ` ` + u(' ') + `zz`,
		`go` + u(0xd835) + u(0xdc00) + `x ` + u(0xd83d) + u(0xde00) + `y ` + u(0xd83d) + `z ` + u(0xdc00) +
			`w ab` + u(0xd83d) + u(0xd83d) + u(0xde00) + `cd \` + u(0xd835) + u(0xdc00) + `q`,
		`\\n\\\nab\\\\\tz\\` + u('A') + `q\nn\q x\` + `u12g4 \"ok\" \/sl \b\f\r\tend\` + `u00`,
		`ab\`,
	}
	for _, text := range texts {
		// EVERY TERM ANY STRETCH READS, each asked once of the whole text.
		read := map[string]bool{}
		for from := 0; from < len(text); from++ {
			for to := from + 1; to <= len(text); to++ {
				JSON.tokenize(text[from:to], func(term []byte, _ int) bool {
					read[string(term)] = true
					return true
				})
			}
		}
		terms := make([]string, 0, len(read))
		for term := range read {
			terms = append(terms, term)
		}
		sort.Strings(terms)
		for _, term := range terms {
			if !JSON.MayHold(text, []string{term}) {
				t.Errorf("a stretch of %q reads %q, and MayHold of the text says it may not", text, term)
			}
		}
		if len(terms) > 0 && !JSON.MayHold(text, terms) {
			t.Errorf("the stretches of %q read every one of %q, and MayHold of all of them says no", text, terms)
		}
		for _, absent := range []string{"rollback", "zzzz", strings.Repeat("x", MaxTermLength+1)} {
			if JSON.MayHold(text, []string{absent}) {
				t.Errorf("MayHold of %q says it may hold %q, which no stretch of it reads", text, absent)
			}
		}
	}
}
