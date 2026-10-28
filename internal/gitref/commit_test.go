package gitref_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/crewlet/crewlet/internal/gitref"
)

// GIT'S UTF-8 IS ITS OWN: CommitText admits what git writes into a commit as
// it is given — any valid UTF-8 but Unicode's noncharacters — and names what
// it would rewrite as Latin-1: bytes that are no UTF-8 at all, and each of
// the 66 noncharacters, read from Unicode's own table so a code point the
// rule's arithmetic missed or added is a failure, wherever in the text it
// sits. internal/code/gitproc holds the same rule to git itself, through the
// identity parts a commit-tree is given
// (TestThePoolAdmitsAnIdentityPartExactlyWhereGitWritesItAsGiven); this holds
// the set to Unicode's.
//
// Mutations: drop the noncharacter rule — every noncharacter passes, red;
// widen the range to U+FDF0 — U+FDF0, an ordinary code point, refused, red.
func TestCommitTextIsGitsUTF8(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "fix the README", "Grüße, 世界 😀", "a\nb\r\n\tc", "\ufdcf\ufdf0\ufffd"} {
		if why := gitref.CommitText(ok); why != "" {
			t.Errorf("CommitText(%q) = %q, want \"\": git writes it as given", ok, why)
		}
	}
	for _, bad := range []string{"\xff", "ab\xc3", "\xed\xa0\x80"} {
		if why := gitref.CommitText(bad); !strings.Contains(why, "is not UTF-8") {
			t.Errorf("CommitText(%q) = %q, want it named as no UTF-8", bad, why)
		}
	}
	var nonchars []rune
	// RUNES, never the table's own uint16: U+FFFF plus one wraps a uint16 to
	// zero, and the walk of the last range would never end.
	for _, r16 := range unicode.Noncharacter_Code_Point.R16 {
		for r := rune(r16.Lo); r <= rune(r16.Hi); r += rune(r16.Stride) {
			nonchars = append(nonchars, r)
		}
	}
	for _, r32 := range unicode.Noncharacter_Code_Point.R32 {
		for r := rune(r32.Lo); r <= rune(r32.Hi); r += rune(r32.Stride) {
			nonchars = append(nonchars, r)
		}
	}
	if len(nonchars) != 66 {
		t.Fatalf("Unicode's table names %d noncharacters, want its fixed 66", len(nonchars))
	}
	for _, r := range nonchars {
		for _, text := range []string{string(r), "subject " + string(r), string(r) + " body"} {
			if why := gitref.CommitText(text); !strings.Contains(why, fmt.Sprintf("noncharacter %U", r)) {
				t.Errorf("CommitText(%q) = %q, want the noncharacter %U named", text, why, r)
			}
		}
	}
	// The code points beside the ranges are ordinary.
	for _, r := range []rune{0xFDCF, 0xFDF0, 0xFFFD, 0x1FFFD, 0x10FFFD} {
		if why := gitref.CommitText(string(r)); why != "" {
			t.Errorf("CommitText(%U) = %q, want \"\": it is no noncharacter", r, why)
		}
	}
}

// WHAT GIT WRITES OF A TEXT IT WOULD NOT TAKE AS GIVEN is the text with every
// byte at which its UTF-8 finds no character read as Latin-1, one byte at a
// time: a lone high byte becomes its Latin-1 character, a noncharacter's three
// bytes six, a surrogate's three six — and a text git takes is itself. What it
// writes is always a text CommitText admits, so the pool takes it as given.
// internal/code/host holds the rewrite to git's own, through a cherry-pick of
// a commit whose message is Latin-1 (TestACherryPickIsGitsCherryPick).
//
// Mutations: rewrite a whole bad sequence at once — U+FFFE's six bytes become
// two, red; skip the noncharacter test — U+FFFE is kept, red.
func TestWrittenCommitTextIsGitsRewrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"fix the README", "fix the README"},
		{"Grüße, 世界 😀 \ufffd", "Grüße, 世界 😀 \ufffd"},
		{"\xff", "\u00ff"},
		{"caf\xe9 ol\xe9", "caf\u00e9 ol\u00e9"},
		{"ab\xc3", "ab\u00c3"},
		{"\ufffe", "\u00ef\u00bf\u00be"},
		{"x\ufdd0y", "x\u00ef\u00b7\u0090y"},
		{"\xed\xa0\x80", "\u00ed\u00a0\u0080"},
		{"\U0010FFFF.", "\u00f4\u008f\u00bf\u00bf."},
	} {
		got := gitref.WrittenCommitText(tc.in)
		if got != tc.want {
			t.Errorf("WrittenCommitText(%q) = %q, want %q", tc.in, got, tc.want)
		}
		if why := gitref.CommitText(got); why != "" {
			t.Errorf("WrittenCommitText(%q) = %q, which CommitText refuses: %s", tc.in, got, why)
		}
	}
}
