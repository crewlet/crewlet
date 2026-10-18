package query

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE FIELD GRAMMAR, written once. The applier validates every field a write
// names by it and this compiler parses every path a query names by it, and the
// two must never disagree about what a field is called, so the applier reads it
// from here rather than holding a copy.

// fieldPattern is a field name: a letter, then letters, digits and
// underscores, at most [artifacts.MaxFieldNameBytes] in all. A leading
// underscore is reserved for the engine and is refused by the first character.
var fieldPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]*$`)

// ValidField reports whether name is a field a document may carry.
func ValidField(name string) bool {
	return len(name) <= artifacts.MaxFieldNameBytes && fieldPattern.MatchString(name)
}

// ValidPath reports whether path is a field name or a dotted path one level
// deep (`address.city`): the only shapes the field index holds.
func ValidPath(path string) bool {
	head, tail, dotted := strings.Cut(path, ".")
	if !ValidField(head) {
		return false
	}
	return !dotted || ValidField(tail)
}

// TextIndexBytes is how many bytes of a string the field index keeps.
//
// TWO HUNDRED AND FIFTY-SIX: every chip label, status, person handle, date and
// short title fits whole, so equality on them is answered by the index alone,
// while a long note costs the index a bounded row rather than its whole text. A
// longer literal is matched on its prefix and re-checked against the body
// ([Compiled.Recheck]).
const TextIndexBytes = 256

// TruncateText is the text the field index keeps for s: its first
// [TextIndexBytes] bytes, cut back to a rune boundary so the column always
// holds valid UTF-8. ONE RULE, for the applier that writes the index and the
// compiler that queries it.
func TruncateText(s string) string {
	if len(s) <= TextIndexBytes {
		return s
	}
	cut := TextIndexBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
