package events_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/events"
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
	if got := events.ClipDiagnostic(real); got != real {
		t.Errorf("a %d-byte diagnosis was cut; the bound is for pathological "+
			"input, not for messages written to be read", len(real))
	}

	huge := strings.Repeat("x", events.MaxDiagnosticBytes*3)
	got := events.ClipDiagnostic(huge)
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
		got := events.ClipDiagnostic(s)
		if !utf8.ValidString(got) {
			t.Errorf("clipping %q… produced invalid UTF-8", s[:12])
		}
	}
}
