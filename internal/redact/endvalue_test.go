package redact_test

import (
	"strings"
	"testing"
	"unicode"

	"github.com/crewlet/crewlet/internal/redact"
)

// NO VALUE AFTER A KEY, ON A LINE ENDING IN AN ARMOUR'S END, IS PUBLISHED: a
// quoted value's key, a piece of a key and an END after it on its line, and a
// word, the same quote, piece and END on the next, whose suffix makes the word
// the value (the package doc's "A key's block beside a password"). Over the
// family, every part varied ([redact.ArmourEndFamily]), no value the rule
// reads below its key in the text as written ([redact.ValuesBelowKeys]) is in
// what Secrets answers.
//
// Mutation: claim a password from its key to its value's end again
// ([claims.build]), and the word under the probe's shape is published.
func TestNoValueAfterAKeyOnALineEndingInAnArmoursEndIsPublished(t *testing.T) {
	t.Parallel()
	read := 0
	for i, c := range redact.ArmourEndFamily() {
		name, text := c[0], c[1]
		values := redact.ValuesBelowKeys(text)
		if i < 3 && len(values) == 0 {
			t.Errorf("%s: the rule reads no value below its key in the text as written, so this case tests nothing", name)
		}
		once := redact.Secrets(text)
		for _, v := range values {
			if strings.Count(text, v) != 1 || !strings.ContainsFunc(v, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) {
				// Text that is in the case elsewhere too, or punctuation:
				// what Secrets answers says nothing of this value.
				continue
			}
			read++
			if strings.Contains(once, v) {
				t.Errorf("%s: the value %q is published:\n text %q\n once %q", name, v, text, once)
			}
		}
	}
	t.Logf("%d values read below their keys", read)
}

// REDACTING TWICE IS REDACTING ONCE after a key on a line ending in an armour's
// END. The keys' blocks of the first pass ended the key's line and the
// value's unalike, and the rule left the piece on the key's line, which the
// second pass, the value then its marker, read as a key's and redacted. Over
// the family, and over the texts of every key's shape the package's
// idempotence cases run over, a second pass changes nothing.
//
// Mutation: claim a password from its key to its value's end again
// ([claims.build]), and a second pass over the probe's shape redacts the piece
// the first left on the key's line.
func TestRedactingTwiceIsRedactingOnceAfterAKeyOnALineEndingInAnArmoursEnd(t *testing.T) {
	t.Parallel()
	texts := redact.ArmourEndFamily()
	for name, text := range keyFixtures() {
		texts = append(texts, [2]string{name, text})
	}
	for _, c := range texts {
		once := redact.Secrets(c[1])
		if twice := redact.Secrets(once); twice != once {
			t.Errorf("%s: a second pass changed\n%q\nto\n%q", c[0], once, twice)
		}
	}
}
