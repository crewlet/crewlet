package mcp

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/textcut"
)

// AN ADDRESS SHOWN IS THE OPENING OF ITS WHOLE REDACTION: an answer or an
// error keeps the cut of the whole address redacted, wherever a credential
// sits against the cut.
func TestAnAddressShownIsTheOpeningOfItsWholeRedaction(t *testing.T) {
	t.Parallel()
	const host = "https://idp.example.com/"
	for _, s := range redacttest.Shapes() {
		if strings.ContainsAny(s.Sample, " \n") {
			continue
		}
		for start := maxMetadataWord - len(s.Sample) + 1; start < maxMetadataWord; start++ {
			raw := host + strings.Repeat("p", start-len(host)-1) + "/" + s.Sample + "/x"
			if got, want := shownAddress(raw), textcut.Bytes(redact.URL(raw), maxMetadataWord); got != want {
				t.Fatalf("%s at byte %d: shown %q; want %q", s.Name, start, got, want)
			}
		}
	}
}
