package redact_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
)

// A QUOTE AND THEN A MARKER NAMES NO VALUE: a value a pass replaced inside its
// quotes leaves the quotes standing, and a second pass that read the opening
// quote as the value wrote a second marker over it and lost the quote.
//
// Mutation: read the quote as the value, and the marker doubles.
func TestAQuoteBeforeAMarkerNamesNoValue(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		`{"password":"[REDACTED:password]"}`,
		`{'password':'[REDACTED:password]'}`,
		`password: "[REDACTED:password]"`,
		`password='[REDACTED:password]'`,
		`{"password":"[REDACTED:github-token]"}`,
	} {
		if got := redact.Secrets(text); got != text {
			t.Errorf("a redacted value's quotes were read as a value:\n text %q\n  got %q", text, got)
		}
	}
}
