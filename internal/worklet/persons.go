package worklet

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PersonSeat is a person's seat as the org chart declares it: the handle and
// the name a wait's reason can name them by (ADR-0122).
type PersonSeat struct {
	Handle string `json:"handle"`
	Name   string `json:"name,omitempty"`
}

// PersonsNamed is the handles of the persons text names: by `@handle`, in any
// case, or by the name the chart gives them as a whole phrase in the case the
// chart writes it. THE ONE MATCHER the wait tool refuses by and the ready read
// hands a wait back by, so the two cannot disagree.
//
// NEVER BY A BARE HANDLE, nor by a name in another case: handles and one-word
// names are often common words (`will`, `may`, `max`), and "the release will
// be cut Friday" waits on nobody.
func PersonsNamed(text string, people []PersonSeat) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, p := range people {
		if p.Handle == "" || slices.Contains(out, p.Handle) {
			continue
		}
		name := strings.TrimSpace(p.Name)
		if phraseIn(lower, "@"+strings.ToLower(p.Handle)) || phraseIn(text, name) {
			out = append(out, p.Handle)
		}
	}
	return out
}

// phraseIn reports whether phrase occurs in text with no letter, digit, `_` or
// `-` on either side of it.
func phraseIn(text, phrase string) bool {
	if phrase == "" {
		return false
	}
	for from := 0; from < len(text); {
		at := strings.Index(text[from:], phrase)
		if at < 0 {
			return false
		}
		start, end := from+at, from+at+len(phrase)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !wordRune(before)) && (end == len(text) || !wordRune(after)) {
			return true
		}
		from = start + 1
	}
	return false
}

func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' }
