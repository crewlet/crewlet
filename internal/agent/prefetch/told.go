package prefetch

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/learning"
)

// A NOTE OLDER THAN AN ACCESS CHANGE IT NAMES IS MARKED, never deleted or
// edited: a seat kept "I cannot reach production" before it was granted it,
// and the note is shown with a suffix saying it predates the change, beside
// "Your access", which is current. Deterministic and cheap: a whole-word match
// of the change's terms, which misses a paraphrase; "Your access" is what
// catches that.

// ToldChange is one change to the seat's access it was told of: when, whose
// access it was as a person reads it, and what a note naming it contains
// (internal/capability's Change.Terms).
type ToldChange struct {
	At     time.Time
	Vendor string
	Terms  []string
}

// WrittenBefore is the suffix for a note written before a told change whose
// terms it names as whole words, case-insensitively: the newest such change,
// and "" where none is or the note's instant is unknown.
func WrittenBefore(entry learning.DiaryEntry, told []ToldChange) string {
	if entry.CreatedAt.IsZero() {
		return ""
	}
	text := strings.ToLower(entry.Content)
	var newest *ToldChange
	for i := range told {
		c := &told[i]
		if !c.At.After(entry.CreatedAt) || strings.TrimSpace(c.Vendor) == "" {
			continue
		}
		if newest != nil && !c.At.After(newest.At) {
			continue
		}
		if namesAny(text, c.Terms) {
			newest = c
		}
	}
	if newest == nil {
		return ""
	}
	return " (written before your " + strings.TrimSpace(newest.Vendor) + " access changed on " +
		newest.At.UTC().Format(time.DateOnly) + "; check Your access)"
}

// namesAny reports whether lowered text holds any of terms as a whole word.
func namesAny(text string, terms []string) bool {
	for _, term := range terms {
		if term = strings.ToLower(strings.TrimSpace(term)); term != "" && holdsWord(text, term) {
			return true
		}
	}
	return false
}

// holdsWord reports whether text holds term with no letter, digit or
// underscore directly either side of it.
func holdsWord(text, term string) bool {
	for from := 0; from <= len(text)-len(term); {
		i := strings.Index(text[from:], term)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(term)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !wordRune(before)) && (end == len(text) || !wordRune(after)) {
			return true
		}
		from = start + 1
	}
	return false
}

func wordRune(r rune) bool { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

// currencyOf is what a seat's notes are held to under build, given the access
// changes it was told: when its access last changed.
func currencyOf(build string, told []ToldChange) learning.Currency {
	c := learning.Currency{Build: build}
	for _, change := range told {
		if change.At.After(c.AccessChangedAt) {
			c.AccessChangedAt = change.At
		}
	}
	return c
}
