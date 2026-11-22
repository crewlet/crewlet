package redact_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// hidesAndHolds fails t where Secrets of text shows any of words, or a second
// pass changes the first's answer.
func hidesAndHolds(t *testing.T, text string, words ...string) {
	t.Helper()
	once := redact.Secrets(text)
	for _, w := range words {
		if strings.Contains(once, w) {
			t.Errorf("%q is shown:\n text %q\n once %q", w, text, once)
		}
	}
	if twice := redact.Secrets(once); twice != once {
		t.Errorf("a second pass changed\n%q\nto\n%q", once, twice)
	}
}

// A VALUE READ ON A LATER PIECE THAN THE FIRST, where the string's opening is
// not written, is taken as well as the first piece's: a key on a string's
// first piece after two quotes is read from each, and a reading the first
// does not overlap is a second range, held to the bound rather than its line.
//
// Mutation: keep only the first reading's range ([redact.Secrets]'s second
// range dropped), and the later pieces' value is shown.
func TestAValueALaterPieceHoldsIsTakenBesideTheFirst(t *testing.T) {
	t.Parallel()
	hidesAndHolds(t, "{\"log\": \"  - \"pwd:\\n\"\\n  hunter2\\nhunter2x9  # rotate the password:\\npwd=ab\\ncd pwd=\"\\n…u1SU1Lf9x= -----END RSA PRIVATE KEY-----\\n--- FAIL: TestRSA2048Sign (0.01s)\\n\"}\n",
		"hunter2")
}

// A BLOCK THAT JOINS A KEY'S PIECE TO A STRING'S FIRST ONE IS WRITTEN APART
// from it at the escaped break before the key's piece, so the key's value is
// read against its own piece's lines.
//
// Mutation: write such a block whole, and the value is shown.
func TestABlockJoiningAKeysPieceToTheFirstIsWrittenApart(t *testing.T) {
	t.Parallel()
	hidesAndHolds(t, "{\"log\": \"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 \\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END ENCRYPTED PRIVATE KEY-----\\\", \\\"Password:\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END ENCRYPTED PRIVATE KEY-----\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 Retype password:\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 placeholder9y\\n\"}\n",
		"placeholder9y")
}

// THE KEY'S ROW KEEPS ITS GUTTER where a block written apart from a string's
// first piece ends on it, so the row below is read by that gutter.
//
// Mutation: begin the block's second range at the key's piece's start, and the
// row's value is shown.
func TestAKeysRowABlockEndsOnKeepsItsGutter(t *testing.T) {
	t.Parallel()
	hidesAndHolds(t, "{\"log\": \"| -----BEGIN OPENSSH PRIVATE KEY----- |\\n| MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun |\\n| u1SU1Lf=-----END OPENSSH PRIVATE KEY----- passwd: |\\n| Пароль9x |\\n\"}\n",
		"Пароль9x")
}

// TWO READINGS OF A KEY'S LINE THAT DO NOT OVERLAP STAY TWO RANGES, even on one
// line: joined, the range ran into a key's block, which was then no longer cut
// at that line's ending, and the rule took the key line's closing quote for
// the value and showed the word the claims read.
//
// Mutation: join readings that share a line, and the line's first word, which
// the claims take as the value, is shown.
func TestTwoReadingsOnALineThatDoNotOverlapStayApart(t *testing.T) {
	t.Parallel()
	hidesAndHolds(t, "{\"log\": \"echo \\\"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END EC PRIVATE KEY-----passwd:\\\" >> k\\n\\n\\necho \\\"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\\\" >> k\\necho \\\"MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6 -----END EC PRIVATE KEY-----\\\" >> k\\n\"}\n",
		`\n\n\necho`)
}

// A KEY PAST A BLOCK'S MARKER IS NO KEY OF THE LINE BEFORE IT: a block can hold
// line breaks, so what follows its marker may stand on another line as
// written, and read as the rest of the line, a key there named the line's
// value as none.
//
// Mutation: read the rest of the line past the marker, and the value is shown.
func TestAKeyPastABlocksMarkerIsNoKeyOfTheLineBeforeIt(t *testing.T) {
	t.Parallel()
	hidesAndHolds(t, "{\"log\": \"> Password:\\n> hunter2x9 MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6\\nq8Zr4TfL2w==-----END PRIVATE KEY-----|token: abc Password:\\n\\n\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\\nMIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\\n\"}\n",
		"hunter2x9")
}

// A LINE BELOW THAT REPEATS WHAT THE KEY'S LINE HAS UP TO THE STRING'S QUOTE IS
// READ PAST IT, on the string's first piece or a later one: a string's lines
// each written in the same wrapping (a list's, an echo's, a log nested in a
// log) open alike, and read from the quote alone, the wrapping's first word
// was taken for the value.
//
// Mutation: read such a line only by the gutter after the quote, and the value
// is shown.
func TestALineThatRepeatsTheKeysWrappingIsReadPastIt(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`- "Password:\n- "hunter2x9"` + "\n",
		`echo "Password:\necho "hunter2x9"` + "\n",
		"{\"log\": \"db2Password=\\n1. Password:\\n{\"log\": \"hunter2x9\"}\\n\"}\n",
	} {
		hidesAndHolds(t, text, "hunter2x9")
	}
}

// A SHELL'S APPEND AFTER A KEY'S STRING IS THE LINE'S SUFFIX, not the string's
// next line: past an escaped break, a rest that matches the key's line only at
// the string's quote spells none of its gutter.
//
// Mutation: read such a rest as the string's next line, and its redirection
// is taken for the value and a second pass changes the first's answer.
func TestAShellsAppendAfterAKeysStringIsItsSuffix(t *testing.T) {
	t.Parallel()
	hidesAndHolds(t, `echo "x" "Password:\n" >> k`+"\n"+`echo "hunter2x9" >> k`+"\n", "hunter2x9")
}
