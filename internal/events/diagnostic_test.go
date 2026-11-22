package events_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// The bound on a diagnostic field is a DELIVERY GUARANTEE, not a content
// budget: an event over the queue's 8 MiB ceiling is refused, and every
// telemetry publisher logs the refusal and moves on — so an unbounded error
// reaches the operator not shortened but absent.
func TestADiagnosticIsBoundedOnlySoTheEventCanBePublished(t *testing.T) {
	t.Parallel()

	// Anything written to be read passes through untouched. This is the case
	// that matters: the cut exists for pathological input, and a real
	// diagnosis must never meet it.
	real := "engine: apply revision 7: store: exec insert into company_config: " +
		strings.Repeat("near \"x\": syntax error; ", 200)
	if got := events.Diagnostic(real); got != real {
		t.Errorf("a %d-byte diagnosis was cut; the bound is for pathological "+
			"input, not for messages written to be read", len(real))
	}

	huge := strings.Repeat("x", events.MaxDiagnosticBytes*3)
	got := events.Diagnostic(huge)
	if len(got) > events.MaxDiagnosticBytes+200 {
		t.Errorf("a clipped diagnostic is %d bytes, past its own bound", len(got))
	}
	// MARKED. An unmarked cut is indistinguishable from an error that
	// really did end there.
	if !strings.Contains(got, "truncated") {
		t.Errorf("the cut is silent: %q", got[len(got)-80:])
	}
	// THE HEAD. A wrapped Go error reads outermost-first, so the head names
	// the operation that failed; and where this bound actually bites — a
	// decode failure quoting a document — the head is the message.
	if !strings.HasPrefix(got, "xxx") {
		t.Errorf("the tail was kept instead of the head: %q", got[:40])
	}
	// Two orders of magnitude below the queue's ceiling, so this field can
	// never be what pushes an event over it.
	if events.MaxDiagnosticBytes >= 1<<20 {
		t.Errorf("MaxDiagnosticBytes = %d is not comfortably below the 8 MiB "+
			"envelope ceiling it exists to keep events under",
			events.MaxDiagnosticBytes)
	}
}

// Never through a rune: a byte slice splits whatever multi-byte character
// straddles the cut, and a JSON encoder replaces the result with U+FFFD — so
// a diagnostic naming a non-ASCII path would arrive garbled rather than long.
func TestAClippedDiagnosticStaysValidUTF8(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		strings.Repeat("日本語", events.MaxDiagnosticBytes),
		strings.Repeat("é", events.MaxDiagnosticBytes),
		strings.Repeat("🙂", events.MaxDiagnosticBytes),
	} {
		got := events.Diagnostic(s)
		if !utf8.ValidString(got) {
			t.Errorf("clipping %q… produced invalid UTF-8", s[:12])
		}
	}
}

// A DIAGNOSTIC HOLDS NO SECRET HOWEVER ITS CUT FALLS. A provider's key and a
// bearer token, one inside the kept head and one with the cut falling at
// every offset of it, reach the event as markers: no piece of either, and no
// marker cut in part, which reads as the text's own words.
//
// Mutation: clip before redacting, and the cut keeps the head of the
// credential it ran across; drop the move back off a marker, and a cut inside
// one keeps it in part.
func TestADiagnosticHoldsNoSecretHoweverItsCutFalls(t *testing.T) {
	t.Parallel()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	for _, p := range []struct{ text, value string }{
		{sample["anthropic-key"], sample["anthropic-key"]},
		{"Authorization: Bearer " + sample["jwt"], sample["jwt"]},
	} {
		head := "deploy failed: " + p.text + "\n"
		// THE CUT AT EVERY OFFSET of the credential and of its marker, which
		// the head's own redaction moves.
		for at := 1; at < len(p.text)+2*redact.LongestMarker(); at++ {
			text := head + strings.Repeat("x", events.MaxDiagnosticBytes-len(head)-at-1) + " " + p.text + "\n" +
				strings.Repeat("y", 1<<10)
			got := events.Diagnostic(text)
			if !strings.Contains(got, "diagnostic truncated") {
				t.Fatalf("a %d-byte diagnostic was not cut", len(text))
			}
			for i := 0; i+6 <= len(p.value); i++ {
				if strings.Contains(got, p.value[i:i+6]) {
					t.Errorf("cut %d bytes into %q: the diagnostic holds %q of it", at, p.text[:12], p.value[i:i+6])
					break
				}
			}
			for _, words := range redact.OwnWords(got) {
				if strings.Contains(words, "REDACTED") {
					t.Errorf("cut %d bytes into %q: the diagnostic holds a marker in part", at, p.text[:12])
					break
				}
			}
		}
	}
}
