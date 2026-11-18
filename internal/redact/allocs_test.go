package redact

import (
	"strings"
	"testing"
)

// CLEAN TEXT SKIPS EVERY REPLACEMENT: `ReplaceAllString` allocates even with
// no match (it copies the input into a fresh buffer and converts it to a
// string), so testing a rule before running it spares clean transcripts, which
// are almost all of them, two full copies per rule.
//
// Measured against an unguarded loop built here from the same rule table, not
// a fixed number: the number moves when a rule is added, and the race detector
// adds an allocation per regexp execution, so an absolute count is 0 under
// `go test` and grows with the table under `-race`. An internal test because
// the rules are the baseline.
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
		for _, r := range rules() {
			s = r.pattern.ReplaceAllLiteralString(s, r.with)
		}
		_ = s
	})
	// The saving is one skipped replacement per rule, so the gap has to be
	// at least that. A bare `guarded < unguarded` would pass on measurement
	// noise: under the race detector each regexp execution allocates, and
	// the two loops execute a different number of them.
	saved := float64(len(rules()))
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

// CLEAN TEXT SKIPS EVERY REPLACEMENT AT A TRANSCRIPT'S LENGTH TOO: [Secrets]
// reads a text of any length whole, so a clean one several times a stage's
// window ([wholeBytes]) still costs a scan per rule and no copy, against the
// same unguarded loop as [TestCleanTextSkipsEveryReplacement] at 10 KB. Read
// by the stages a [Stream] redacts by, it paid a joined window and a written
// output a step for every stage, about 75 bytes allocated a byte of it.
//
// Mutation: redact a text longer than a stage's window by the stages, and
// Secrets over it allocates more than the unguarded loop.
func TestCleanTextSkipsEveryReplacementAtATranscriptsLength(t *testing.T) {
	line := "the build passed; see docs/concepts/code-sandbox.md\n"
	text := strings.Repeat(line, 4*wholeBytes/len(line))
	if Contains(text) {
		t.Fatal("the fixture is not clean; every rule must miss it")
	}
	guarded := testing.AllocsPerRun(5, func() { _ = Secrets(text) })
	unguarded := testing.AllocsPerRun(5, func() {
		s := text
		for _, r := range rules() {
			s = r.pattern.ReplaceAllLiteralString(s, r.with)
		}
		_ = s
	})
	if saved := float64(len(rules())); unguarded-guarded < saved {
		t.Errorf("Secrets over %d bytes of clean text allocated %v per call against %v for the "+
			"unguarded loop: it is not reading the text whole", len(text), guarded, unguarded)
	}
}
