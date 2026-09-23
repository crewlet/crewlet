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

	// A head and a tail that tell themselves apart, so which end was kept is
	// something the assertions below can see.
	huge := "engine: turn for ceo: " + strings.Repeat("x", events.MaxDiagnosticBytes*3) + " :the tail"
	got := events.ClipDiagnostic(huge)
	if len(got) > events.MaxDiagnosticBytes+200 {
		t.Errorf("a clipped diagnostic is %d bytes, past its own bound", len(got))
	}
	// MARKED, AND SAYING WHERE THE REST IS. An unmarked cut is
	// indistinguishable from an error that really did end there, and a mark
	// that names no place leaves a reader holding a head with no whole.
	if !strings.Contains(got, "cut at 64 KiB") {
		t.Errorf("the cut is silent, or names another bound: %q", got[len(got)-120:])
	}
	if !strings.Contains(got, "the whole is in the log of the node that published it") {
		t.Errorf("the cut does not say where the whole is: %q", got[len(got)-120:])
	}
	// THE HEAD. A wrapped Go error reads outermost-first, so the head names
	// the operation that failed.
	if !strings.HasPrefix(got, "engine: turn for ceo: ") || strings.Contains(got, ":the tail") {
		t.Errorf("the tail was kept instead of the head: %q … %q", got[:40], got[len(got)-160:])
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
