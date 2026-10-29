package redact

import (
	"strings"
	"testing"
	"time"
)

// A LINE OF MANY ARMOURS COSTS WHAT ITS BYTES COST, however long the line:
// reading the key blocks of armours on lines past the bound costs what reading
// the same armours on short lines does. Every armour used to scan the rest of
// its line — a BEGIN forward, for its line's end and the armour after it; an
// END back, for its line's start — so a line of flattened key material (a
// coding agent's one-line env dump, a `cat`ted key store) cost the armour count
// times the line: two megabytes, nearly two minutes. Bounding each scan at
// [MaxKeyBlockBytes] only made it the armour count times the bound, and two
// megabytes still took thirty seconds under the race detector. Each scan now
// stops at the armour before or after it.
//
// A COMPARISON, NOT A CLOCK: the same armours in the same number of bytes, on
// lines of four KiB and on lines twice the bound, compared by [withinFactor]
// (the fastest of interleaved reads, measured again before it fails), so a
// runner busy for a moment slows both or neither and no absolute duration has
// to hold on a loaded machine. The clock this
// replaced, thirty seconds for two megabytes, failed two runs in three beside
// other packages under the race detector. Read linearly the two cost the same,
// the long lines a little less (measured 0.85 to 1.1 times, with and without
// the race detector); a scan per armour to its line's end costs thirteen to
// twenty-two times as much on the long lines, and one to the bound six to
// thirteen. Three times is allowed: room by a factor of two or more on either
// side. Timed on [keyBlocks] alone, an internal test for that reason: the
// rules' passes over the text cost the same on both and would only dilute the
// difference.
//
// The same holds for a key on one line a cut wrote into, its body read up to
// the cut's mark and back from it ([lineBack]), and for a body a window kept
// before an END on its line.
//
// Mutation: look for a BEGIN line's break as far as the bound before the armour
// after it (six times), or for an END line's start back to the bound rather
// than to the key armour before it (nine times), and this fails.
func TestALineOfManyArmoursIsLinear(t *testing.T) {
	t.Parallel()
	lines := func(group string, lineBytes int) string {
		var b strings.Builder
		for b.Len() < 128<<10 {
			for line := b.Len(); b.Len()-line < lineBytes; {
				b.WriteString(group)
			}
			b.WriteByte('\n')
		}
		return b.String()
	}
	read := func(text string) { _, _ = keyBlocks(text) }
	for name, group := range map[string]string{
		"a bare BEGIN, over and over": "-----BEGIN PRIVATE KEY-----",
		"an END, over and over":       "-----END RSA PRIVATE KEY----- ",
		// A key on one line cut in its middle, over and over: each
		// armour's body read up to and back from the mark.
		"a cut key on one line, over and over":    "-----BEGIN PRIVATE KEY----- Ab12cdEF34gh[...truncated]Ij56klMN78op -----END PRIVATE KEY----- ",
		"a cut body before an END, over and over": "x[… 812 bytes truncated]Ab12cdEF34gh Ij56klMN78op=-----END RSA PRIVATE KEY----- ",
	} {
		short, long := lines(group, 4<<10), lines(group, 2*MaxKeyBlockBytes)
		if ok, shortCost, longCost := withinFactor(read, short, long, 3); !ok {
			t.Errorf("%s: %v on lines of %d KiB against %v on lines of 4 KiB — a scan per armour "+
				"runs to its line's end or to the bound again", name, longCost, 2*MaxKeyBlockBytes>>10, shortCost)
		}
	}
}

// A LINE A WRAPPING'S PREFIX RUNS ACROSS COSTS WHAT ITS BYTES COST: reading
// where the prefix of a line a cut wrote into ends ([wrapping.headRun]), under
// an armour line whose gutter is a run of numbers, costs what it does on short
// lines. Asked of [wrapping.prefixOK] before each run, the line's shape was
// built again from its start at every run its prefix matched, so a line of
// numbers under a gutter of numbers cost its runs times its length: 256 KiB in
// lines of 30 KiB took 4.7 s against 0.17 s in lines of one. Timed as the
// test above is ([withinFactor]), on lines of 1 KiB and 30
// KiB — under [MaxKeyBlockBytes], since a read back reaches no line past it.
//
// Mutation: ask prefixOK before each run in headRun, and both directions take
// twenty times as long or more on the long lines.
func TestALineAWrappingsPrefixRunsAcrossIsLinear(t *testing.T) {
	t.Parallel()
	texts := func(lineBytes int) map[string]string {
		var back, forward strings.Builder
		gutter := strings.Repeat("1 ", lineBytes/2)
		for back.Len() < 256<<10 {
			back.WriteString(gutter + "x# Ab12cdEF34\n" + gutter + "-----END RSA PRIVATE KEY-----\n")
			forward.WriteString(gutter + "-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat("Ab12", 16) + "\n" +
				gutter + "x# Ab12cdEF34\n\n\n")
		}
		return map[string]string{"read back from an END": back.String(), "read forward from a BEGIN": forward.String()}
	}
	read := func(text string) { _, _ = keyBlocks(text) }
	short, long := texts(1<<10), texts(30<<10)
	for name := range short {
		if ok, shortCost, longCost := withinFactor(read, short[name], long[name], 3); !ok {
			t.Errorf("%s: %v on lines of 30 KiB against %v on lines of 1 KiB — the prefix's shape is "+
				"built again at every run", name, longCost, shortCost)
		}
	}
}

// A LINE OF MANY PASSWORD KEYS COSTS WHAT ITS BYTES COST, however long the line
// goes on after them or before them: reading where each one's value is
// ([readValue]) on a line that goes on for 256 KiB costs what it does on one
// that goes on for 2 KiB. A
// key whose first word is punctuation asks what the rest of its line is — the
// wrapping's suffix, if no key follows on the line — and asked as the line's
// end and then whether a key lies before it, every key on the line scanned the
// rest of the line: its keys times its length. So the scan stops at the next
// key ([lineEndUnnamed]), and only the line's last key, which the scan leaves at
// the line's end, reads its gutter and the lines after. Timed as the tests
// above are ([withinFactor]), through [Secrets] and
// [Settled] both, since each reads every key's value. The keys share their
// line with filler rather than with more keys, because under the race detector
// a key costs what scanning some 25 KiB does, and a line of keys alone would
// have to run for megabytes before the scans showed. Read linearly the two
// cost the same (measured 0.75 to 1.6 times); a scan per key to its line's end
// costs 26 to 29 times as much on the long line, and five times under the race
// detector. Every key reads back over its label for the markup its line
// opened before it ([pastMarkup]), no further than the value before it; read
// back to the line's start, every key read the line before it again: on a
// line that opens with 256 KiB, 43 to 58 times what the keys cost after 2 KiB,
// and 37 to 52 times under the race detector.
//
// Mutation: read a key's suffix as the rest of its line and only then ask it
// for another key, and this fails; read a key's label back to the line's
// start, past the values before it, and every group fails, with the race
// detector and without.
func TestALineOfManyPasswordKeysIsLinear(t *testing.T) {
	t.Parallel()
	read := func(text string) {
		_ = Secrets(text)
		_ = Settled(text)
	}
	for name, group := range passwordKeyGroups {
		for _, line := range []struct {
			where string
			of    func(group string, lineBytes int) string
		}{{"after", keysBefore}, {"before", keysAfter}} {
			short, long := line.of(group, 2<<10), line.of(group, 256<<10)
			if ok, shortCost, longCost := withinFactor(read, short, long, 3); !ok {
				t.Errorf("%s: %v on a line that goes on for 256 KiB %s them against %v for 2 KiB — "+
					"every key reads the line %s it", name, longCost, line.where, shortCost, line.where)
			}
		}
	}
}

// keysBefore is group written 1024 times at the start of a line and 256 KiB of
// filler after it, in lines of lineBytes: the keys' own line goes on for
// lineBytes of it.
func keysBefore(group string, lineBytes int) string {
	var b strings.Builder
	b.WriteString(strings.Repeat(group, 1024))
	for written := 0; written < 256<<10; written += lineBytes {
		b.WriteString(strings.Repeat("~", lineBytes))
		b.WriteByte('\n')
	}
	return b.String()
}

// keysAfter is 256 KiB of filler in lines of lineBytes and group written 1024
// times at the end of the last of them: the keys' own line opens with
// lineBytes of it.
func keysAfter(group string, lineBytes int) string {
	var b strings.Builder
	for written := lineBytes; written < 256<<10; written += lineBytes {
		b.WriteString(strings.Repeat("~", lineBytes))
		b.WriteByte('\n')
	}
	b.WriteString(strings.Repeat("~", lineBytes))
	b.WriteString(strings.Repeat(group, 1024))
	b.WriteByte('\n')
	return b.String()
}

// A LIVE VIEW OF MANY PASSWORD KEYS COSTS ONE READ OF THEM: [Settled] over
// lines of password keys, each line's last decided by the line after it,
// costs what its bytes cost. Read again in each prefix a hold cut the text at,
// that key was pending there, so the hold moved up a line, made the key above
// pending in its turn, and Settled read the text once a line: its lines times
// its length, 47 to 75 times what [Secrets] costs over 32 KiB in lines of 256
// bytes. So the whole text is read once ([valueHolds]).
//
// A COMPARISON ACROSS SIZES, NOT ACROSS FUNCTIONS: Settled over 32 KiB against
// eight times Settled over 4 KiB of the same lines, compared by
// [withinFactor] as the tests above are. This used to compare
// Settled with Secrets over the same text, but that ratio is a constant factor
// between two different functions, 0.8 to 2.4 on a quiet machine, and the race
// detector's overhead on a loaded runner moved it past the three times allowed
// (3.4 to 3.6 in CI) with nothing quadratic in sight. Against itself, the
// constant cancels: read once, eight times the bytes cost eight times as much
// (measured 0.9 to 1.4 times the eight, with the race detector and without,
// and beside the package's other tests); read once a line, they cost eight
// times that again. Three times is allowed.
//
// Mutation: read the holds again in every prefix a line of the text ends, and
// every group takes 50 to 80 times as long over eight times the bytes, and
// this fails, with the race detector and without.
func TestALiveViewOfManyPasswordKeysCostsOneRead(t *testing.T) {
	t.Parallel()
	read := func(text string) { _ = Settled(text) }
	const scale = 8
	for name, group := range passwordKeyGroups {
		small, large := linesOf(group, 4<<10, 256), linesOf(group, scale*4<<10, 256)
		if ok, smallCost, largeCost := withinFactor(read, small, large, 3*scale); !ok {
			t.Errorf("%s: Settled took %v over 32 KiB against %v over 4 KiB, more than %d "+
				"times as much for %d times the bytes, so it reads the text again for every "+
				"line it holds", name, largeCost, smallCost, 3*scale, scale)
		}
	}
}

// passwordKeyGroups are password keys written over and over, as a line of them
// is: each ends in what a wrapping's suffix is read from.
var passwordKeyGroups = map[string]string{
	"a key and an echo's quote, over and over":   `password:" `,
	"a key, a quote and a word, over and over":   `pwd=" x `,
	"a key and a concatenation's, over and over": `password:\n" + `,
	// The line's last key reads the line before it as the rule leaves it,
	// every value before it a marker ([keyPrefix]): once, not once a key.
	"a key and the value after it, over and over": `x pwd=`,
	// Each key reads back over its label for the markup its line opened
	// ([pastMarkup]), no further than the value before it.
	"a bold label and its value, over and over": `**pwd:** x `,
	"a table's cells, over and over":            `| pwd: | x `,
}

// linesOf is size bytes of group, written over and over, in lines of
// lineBytes.
func linesOf(group string, size, lineBytes int) string {
	var b strings.Builder
	for b.Len() < size {
		for line := b.Len(); b.Len()-line < lineBytes; {
			b.WriteString(group)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// How many reads of each text one measurement takes, in turn, keeping the
// fastest of each, after one read of each to warm up; how many measurements a
// comparison may take before it fails; and the least a long read may cost for
// a comparison to judge it at all.
const (
	interleavedReads = 5
	measureRounds    = 3
	judgeFloor       = 5 * time.Millisecond
)

// withinFactor reports whether reading long costs at most factor times what
// reading short does, with the two costs of the last measurement taken.
//
// EACH MEASUREMENT IS THE FASTEST OF [interleavedReads] READS OF EACH, taken
// in turn after one warm-up read of each, so a runner busy for a moment slows
// both or neither. AND A COMPARISON THAT FAILS IS MEASURED AGAIN, up to
// [measureRounds] times, and fails only if every measurement does. A burst of
// load from the other packages beside this one under the race detector can
// outlast every read of one measurement (the live view's 32 KiB read 264ms
// against 4ms in CI, with nothing quadratic in it), but it does not recur on
// every measurement; the regressions these tests guard cost six to eighty
// times the factor on every one, so a real regression still fails.
//
// THE RE-MEASURE ONLY EVER TURNS A FAILURE INTO A PASS, so the separation a
// comparison judges has to hold against the noisiest round, not the average:
// see [TestWithinFactorFailsWhatEveryMeasurementSays]. And a long read under
// [judgeFloor] fails as too small to judge: a read that costs nothing passes
// any factor against another that costs nothing, which is a test asserting
// nothing.
func withinFactor(read func(string), short, long string, factor int) (bool, time.Duration, time.Duration) {
	timed := func(text string) time.Duration {
		start := time.Now()
		read(text)
		return time.Since(start)
	}
	timed(short)
	timed(long)
	var shortCost, longCost time.Duration
	for range measureRounds {
		shortCost, longCost = time.Duration(1<<62), time.Duration(1<<62)
		for range interleavedReads {
			shortCost, longCost = min(shortCost, timed(short)), min(longCost, timed(long))
		}
		if longCost < judgeFloor {
			return false, shortCost, longCost
		}
		if longCost <= time.Duration(factor)*shortCost {
			return true, shortCost, longCost
		}
	}
	return false, shortCost, longCost
}

// THE COMPARISON STILL FAILS A STEADY REGRESSION, measured again or not, and
// passes a linear cost, with room on both sides for a noisy runner.
//
// SIXTEEN TIMES THE BYTES, judged at sixty-four times: a read whose cost grows
// with the square of its text costs 256 times as much, a linear one sixteen,
// and sixty-four is their geometric middle, four times from each. It was
// eight times the bytes judged at twenty-four, sixty-four against eight, which
// left a quadratic cost room by only 2.67 times: in CI under the race
// detector its 2ms short read measured 5.73ms on the fastest of five reads,
// 2.87 times, and the quadratic cost passed. A failed comparison is measured
// again and passes on any round that passes, so the margin has to hold on the
// noisiest round: the square case is checked again here with every short read
// three times its cost, and still fails.
//
// A READ TOO CHEAP TO JUDGE FAILS ([judgeFloor]), whatever its ratio.
//
// EVERY READ SPINS, well above the timer's slack. A spin is a floor, never a
// duration: on a loaded runner a 2ms spin can be preempted as it ends and
// measure several. At this unit the square case's short read is 2ms against a
// long read of 512ms, and the linear case's short read 25ms against 400ms.
//
// Mutation: drop the floor, report a pass after any round, or judge the old
// eight times the bytes at twenty-four, and a case here fails.
func TestWithinFactorFailsWhatEveryMeasurementSays(t *testing.T) {
	t.Parallel()
	const (
		unit   = 125 * time.Microsecond
		scale  = 16
		factor = 64
	)
	small, large := strings.Repeat("x", 4), strings.Repeat("x", 4*scale)
	square := func(text string) { spin(time.Duration(len(text)*len(text)) * unit) }
	if ok, s, l := withinFactor(square, small, large, factor); ok {
		t.Errorf("a square cost passed: %v against %v", l, s)
	}
	// EVERY SHORT READ SLOWED THREE TIMES, the noisiest a round has been
	// measured and more: a quadratic cost still fails.
	noisy := func(text string) {
		cost := time.Duration(len(text)*len(text)) * unit
		if text == small {
			cost *= 3
		}
		spin(cost)
	}
	if ok, s, l := withinFactor(noisy, small, large, factor); ok {
		t.Errorf("a square cost with every short read three times as slow passed: %v against %v", l, s)
	}
	linear := func(text string) { spin(time.Duration(len(text)) * 50 * unit) }
	if ok, s, l := withinFactor(linear, small, large, factor); !ok {
		t.Errorf("a linear cost failed: %v against %v", l, s)
	}
	// A READ TOO CHEAP TO JUDGE, linear at a microsecond a byte: its ratio
	// passes any factor this side of sixteen, and the floor fails it.
	cheap := func(text string) { spin(time.Duration(len(text)) * time.Microsecond) }
	if ok, s, l := withinFactor(cheap, small, large, factor); ok {
		t.Errorf("a read too cheap to judge passed: %v against %v", l, s)
	}
}

// spin holds the CPU for d, as a read whose cost is d does.
//
// NOT time.Sleep, whose floor is the timer's resolution rather than d: on a
// virtual machine that is about a millisecond, so the square case's 320µs
// short read measured 1.1ms, its ratio fell to nineteen against a factor of
// twenty-four, and a quadratic cost passed on every run. A spin costs what it
// is asked to, at any resolution, which is the property the case is about.
func spin(d time.Duration) {
	for start := time.Now(); time.Since(start) < d; {
	}
}
