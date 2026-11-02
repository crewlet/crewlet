package executor

import (
	"strings"
	"testing"
	"time"
)

// A LINE OF MANY NOTE OPENINGS COSTS WHAT ITS BYTES COST, however long the
// line: finding a captured session's notes in a quarter of a mebibyte of
// openings written as one line costs what it does in the same openings on
// lines of four KiB — openings that are no heading, and openings on a line
// that closes as a heading does. Every opening used to look for its line's end
// again, and every opening on a line that closed as a heading was a note of
// its own whose next was looked for the same way: a line of a record runs to
// [maxLineBytes], and one written as nothing but openings cost the openings
// times the line — tens of minutes of a core, past every bound a read has and
// every check of its caller, for one line a box's own agent can write.
//
// A COMPARISON, NOT A CLOCK, timed as internal/redact's linear cases are
// ([withinFactor]): read linearly the two cost the same; read an opening at a
// time to its line's end, the one line costs fifty to a hundred times the
// short ones. Four times is allowed.
//
// Mutation: look for the line's end from every opening, or for the next
// heading from the opening after the one found rather than from its line's
// end, and the one line costs fifty times the short ones or more.
func TestALineOfManyNoteOpeningsCostsWhatItsBytesCost(t *testing.T) {
	t.Parallel()
	lines := func(lineBytes int, closing string) string {
		var b strings.Builder
		for b.Len() < 256<<10 {
			for start := b.Len(); b.Len()-start < lineBytes; {
				b.WriteString(noteHeadOpen)
			}
			b.WriteString(closing)
			b.WriteByte('\n')
		}
		return b.String()
	}
	read := func(text string) {
		for range notesIn(text) {
		}
	}
	for name, closing := range map[string]string{
		"openings that are no heading":            "",
		"openings on a line closing as a heading": noteHeadClose,
	} {
		short, long := lines(4<<10, closing), lines(256<<10, closing)
		if ok, shortCost, longCost := withinFactor(read, short, long, 4); !ok {
			t.Errorf("%s: %v on one line of 256 KiB against %v on lines of 4 KiB — an opening "+
				"looks for its line's end again", name, longCost, shortCost)
		}
	}
}

// costFloor is the shortest a timed sample of [withinFactor] is: the noise a
// loaded runner adds to a sample is a preemption's overshoot, milliseconds
// whatever the sample, so a sample of a millisecond or two is mostly noise.
const costFloor = 20 * time.Millisecond

// withinFactor reports whether read costs at most factor times as much over
// long as over short — the same bytes laid out otherwise — and what each cost:
// the fastest of five samples of each, taken in turn after a warm-up read of
// each, so a runner busy for a moment slows both or neither. Each sample is as
// many reads as it takes the short text's to reach twice [costFloor]. A
// comparison that fails is measured again, three times in all, and fails only
// if every measurement does: that only ever turns a failure into a pass, so
// what this guards has to cost far more than the factor every time, as a read
// that is quadratic in a line does. A long sample past the factor times the
// calibrated short one fails its round at once, so a regression's cost — its
// samples are the costliest thing here — is paid once a round rather than five
// times.
func withinFactor(read func(string), short, long string, factor int) (bool, time.Duration, time.Duration) {
	timed := func(text string, reps int) time.Duration {
		start := time.Now()
		for range reps {
			read(text)
		}
		return time.Since(start)
	}
	reps, calibrated := 1, timed(short, 1)
	for reps < 1<<20 && calibrated < 2*costFloor {
		reps *= 2
		calibrated = timed(short, reps)
	}
	timed(short, 1)
	timed(long, 1)
	var shortCost, longCost time.Duration
	for range 3 {
		shortCost, longCost = time.Duration(1<<62), time.Duration(1<<62)
		for range 5 {
			shortCost = min(shortCost, timed(short, reps))
			sample := timed(long, reps)
			longCost = min(longCost, sample)
			if sample > time.Duration(factor)*calibrated {
				break
			}
		}
		if longCost <= time.Duration(factor)*shortCost {
			return true, shortCost, longCost
		}
	}
	return false, shortCost, longCost
}
