package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/textcut"
)

// AN OAUTH ERROR KEEPS THE OPENING OF ITS WHOLE REDACTION: a vendor's code
// and description, and any other sign-in error, are kept as the cut of the
// whole text redacted, wherever a credential sits against the cut.
func TestAnOAuthErrorKeepsTheOpeningOfItsWholeRedaction(t *testing.T) {
	t.Parallel()
	for _, s := range redacttest.Shapes() {
		for start := oauthWordsBytes - len(s.Sample) + 1; start < oauthWordsBytes; start++ {
			description := strings.Repeat("w", start-len("invalid_grant: ")-1) + " " + s.Sample + " and more"
			said := "invalid_grant: " + strings.Join(strings.Fields(description), " ")
			if got, want := vendorSaid("invalid_grant", description), textcut.Bytes(redact.Secrets(said), oauthWordsBytes); got != want {
				t.Fatalf("%s at byte %d: the vendor's words kept %q; want %q", s.Name, start, got, want)
			}
			err := errors.New(strings.Repeat("w", start-1) + " " + s.Sample + " and more")
			if got, want := oauthWords(err), textcut.Bytes(redact.Secrets(err.Error()), oauthWordsBytes); got != want {
				t.Fatalf("%s at byte %d: the error kept %q; want %q", s.Name, start, got, want)
			}
		}
	}
}
