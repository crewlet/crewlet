package redact

import (
	"strings"
	"testing"
)

// CLEAN TEXT SKIPS EVERY REPLACEMENT, which is the whole point of testing a
// rule before running it.
//
// `ReplaceAllString` allocates even when it changes nothing: with no match it
// still copies the whole input into a fresh buffer and converts that buffer to
// a string. Eleven rules over a clean transcript was therefore twenty-two full
// copies of it — paid on every sandbox result and every transcript, of which
// almost none carry a credential.
//
// Measured against the unguarded loop BUILT HERE from the same rule table
// rather than against a fixed number, for two reasons: the number moves when a
// rule is added, and the race detector adds an allocation per regexp
// execution, so an absolute count is 0 under `go test` and 14 under `-race`.
// A comparison against the shape this replaced holds under both, and is what
// the change actually claims. An internal test because the rules are the
// baseline.
func TestCleanTextSkipsEveryReplacement(t *testing.T) {
	// Transcript-shaped: long enough that a copy per rule dominates, and
	// carrying the words the rules look near without matching any of them.
	text := strings.Repeat("the build passed; see docs/concepts/code-sandbox.md\n", 200)
	if Contains(text) {
		t.Fatal("the fixture is not clean; every rule must miss it")
	}

	guarded := testing.AllocsPerRun(100, func() { _ = Secrets(text) })
	unguarded := testing.AllocsPerRun(100, func() {
		s := text
		for _, r := range rules {
			s = r.pattern.ReplaceAllString(s, r.with)
		}
		_ = s
	})
	// The saving is one skipped replacement per rule, so the gap has to be
	// at least that. A bare `guarded < unguarded` would pass on measurement
	// noise: under the race detector each regexp execution allocates, and
	// the two loops execute a different number of them.
	saved := float64(len(rules))
	if unguarded-guarded < saved {
		t.Errorf("Secrets over clean text allocated %v per call against %v for the "+
			"unguarded loop — the match test saved less than one allocation per "+
			"rule, so it is not skipping the replacements", guarded, unguarded)
	}

	// Contains must not be the whole pass with a comparison bolted on: it
	// answers one bit and may never build the redacted string.
	contains := testing.AllocsPerRun(100, func() { _ = Contains(text) })
	if unguarded-contains < saved {
		t.Errorf("Contains over clean text allocated %v per call against %v for a "+
			"full unguarded pass — it is still redacting to answer", contains, unguarded)
	}
}
