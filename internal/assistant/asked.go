package assistant

import (
	"slices"
	"strings"
	"unicode"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
)

// openVerbs are how a person asks to be taken to a place, and setupVerbs how
// they ask for something to be connected, which its setup page is where.
var (
	openVerbs = []string{"open", "go to", "take me to", "bring me to", "navigate to",
		"jump to", "switch to", "bring up", "pull up"}
	setupVerbs = []string{"connect", "set up", "setup", "configure"}
)

// askedToOpen reports whether the person's own question asks to be taken to
// the place a link opens: an open request (or, for a setup page, a request to
// connect) and one of the names the place goes by, both in their words. Only
// the question is read, so no page text or tool answer the turn read can make
// a card open by itself.
func askedToOpen(question string, names []string, setup bool) bool {
	words := " " + plainWords(question) + " "
	has := func(phrase string) bool {
		p := plainWords(phrase)
		return p != "" && strings.Contains(words, " "+p+" ")
	}
	verbs := openVerbs
	if setup {
		verbs = append(slices.Clone(openVerbs), setupVerbs...)
	}
	return slices.ContainsFunc(verbs, has) && slices.ContainsFunc(names, has)
}

// plainWords is s lowercased with every run of characters other than letters,
// digits and hyphens one space, so "Google_Cloud's" reads "google cloud s".
func plainWords(s string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	}), " ")
}

// questionOf is the person's question that opened turn, from its transcript.
func questionOf(messages []assistantdomain.Message, turn uuid.UUID) string {
	for _, m := range messages {
		if m.Turn == turn && m.Role == wire.RoleUser {
			return m.Text
		}
	}
	return ""
}

// openedAsked reports whether a card to the place names call opens by itself:
// the person's question asked for it, and no card of this turn already did,
// so one answer never moves the page twice.
func (t *turn) openedAsked(names []string, setup bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.opened || !askedToOpen(t.question, names, setup) {
		return false
	}
	t.opened = true
	return true
}
