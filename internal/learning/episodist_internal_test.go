package learning

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// windowWalkBudget is how long [episodeWindows] gets in the test below before
// the walk is called non-terminating.
//
// Generous by orders of magnitude on purpose: the walk over 8 KiB is tens of
// microseconds, so anything this side of a second is a loop that is not
// advancing rather than a machine that is busy — and the failure this guards
// is unbounded, not slow.
const windowWalkBudget = 5 * time.Second

// THE WINDOW WALK TERMINATES ON EVERY STRING, INCLUDING THE ONES THAT ARE NOT
// UTF-8 AT ALL.
//
// A task summary is `t.trigger.Summary` — whatever a vendor sent, arriving
// through a webhook body or a chat payload — and a Go string carries bytes,
// not runes. Given a run of CONTINUATION bytes longer than one window and no
// whitespace to walk to, an unbounded "back up to a rune start" gives the
// whole window back; the no-skip clamp then starts the next window where this
// one started, and episodeWindows never returns. It spins inside the reflect
// pass, so the symptom is a seat that quietly stops writing episodes and a
// core at 100% — nothing in the alarm table is about this, because the loop
// holds no lease and misses no deadline.
//
// The assertion is termination and coverage together: a walk that terminated
// by dropping the tail would be the cut this whole file exists to remove.
func TestTheWindowWalkTerminatesOnBytesThatAreNotUTF8(t *testing.T) {
	t.Parallel()
	// ONE VALID RUNE AND THEN NOTHING BUT CONTINUATION BYTES, with no space
	// or newline anywhere. The leading rune is the whole fixture: it is the
	// only rune start in the string, so an alignment that walks until it
	// finds one walks the ENTIRE window back to index 0 — which is a legal
	// answer to "where is the nearest boundary" and a window of zero bytes
	// to the loop that asked. A string of continuation bytes alone would
	// not catch that, because a walk that finds nothing at all falls back to
	// the index it was given and the loop advances by accident.
	summary := "→" + strings.Repeat("\x80", 3*EpisodeWindowBytes)
	if utf8.ValidString(summary) {
		t.Fatal("the fixture is valid UTF-8, so it exercises nothing")
	}

	done := make(chan []string, 1)
	go func() { done <- episodeWindows(summary) }()

	var windows []string
	select {
	case windows = <-done:
	case <-time.After(windowWalkBudget):
		t.Fatalf("episodeWindows did not return within %s on %d bytes that are "+
			"not UTF-8; the walk is standing still", windowWalkBudget, len(summary))
	}

	if len(windows) < 2 {
		t.Fatalf("a %d-byte summary produced %d window(s), want several",
			len(summary), len(windows))
	}
	covered := 0
	for _, window := range windows {
		covered += len(window)
	}
	// The windows overlap, so they hold MORE bytes than the summary. Less
	// would mean a gap, which is the silent cut by another name.
	if covered < len(summary) {
		t.Errorf("the windows hold %d bytes of a %d-byte summary, so some of "+
			"it reached no vector", covered, len(summary))
	}
}

// ALIGNMENT IS BOUNDED BY A RUNE'S OWN LENGTH, and answers the index it was
// given when there is no boundary within it.
//
// The two cases are the whole contract: valid text is moved back onto the
// character it landed inside, and invalid text is left where the byte
// arithmetic put it rather than walked to the floor. The second is what makes
// the loop's advance arithmetic true.
func TestAlignRuneStartGivesUpAfterARunesWorthOfBytes(t *testing.T) {
	t.Parallel()
	// "→" is three bytes, so index 4 is the middle of the second one.
	text := strings.Repeat("→", 3)
	if got := alignRuneStart(text, 4, 0); got != 3 {
		t.Errorf("alignRuneStart(%q, 4, 0) = %d, want the rune start at 3", text, got)
	}
	if got := alignRuneStart(text, 3, 0); got != 3 {
		t.Errorf("an index already on a boundary moved to %d", got)
	}
	// A rune start with a long run of continuation bytes behind it — the
	// shape that makes the bound load-bearing rather than tidy. There IS a
	// boundary at 0, and taking it would hand the caller back forty bytes
	// it was relying on advancing over. Past a rune's own length the text
	// is invalid whatever this answers, so the answer is the index itself.
	invalid := "→" + strings.Repeat("\x80", 64)
	if got := alignRuneStart(invalid, 40, 0); got != 40 {
		t.Errorf("alignRuneStart walked back to %d, want 40: past %d bytes "+
			"there is no rune to preserve, and a walk that keeps looking is "+
			"what stops the window loop advancing", got, runeAlignBack)
	}
	// The floor is respected before the bound is: a caller's window start
	// is never crossed.
	if got := alignRuneStart(invalid, 40, 39); got != 40 {
		t.Errorf("alignRuneStart crossed its floor and answered %d", got)
	}
}
