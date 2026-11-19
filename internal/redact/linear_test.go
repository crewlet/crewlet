package redact

import (
	"strings"
	"testing"
	"time"
)

// A LINE OF MANY ARMOURS COSTS WHAT ITS BYTES COST, however long the line:
// key blocks of armours on lines past the bound costs what short lines do. Every
// armour used to scan the rest of its line (a BEGIN forward, an END back), so a
// one-line env dump cost armours times line: two megabytes, two minutes. Each
// scan now stops at the adjacent armour, not at [MaxKeyBlockBytes].
//
// A COMPARISON, NOT A CLOCK: the same bytes on lines of 4 KiB and twice the
// bound, by [withinFactor], so a busy runner slows both. A scan per armour
// costs 13 to 22 times as much, one to the bound 6 to 13; three allowed. Timed
// on [keyBlocks] alone; holds for cut keys ([lineBack]) too. Mutation: scan a
// BEGIN line's break to the bound, or an END line's start back to it.
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
// goes on before or after them: reading each value ([readValue]) on a 256 KiB
// line costs what it does on a 2 KiB one. A key opening with punctuation asks
// what the rest of its line is (the wrapping's suffix, if no key follows);
// asked to the line's end, every key scanned the rest of it. So the scan stops
// at the next key ([lineEndUnnamed]) and only the last key reads the gutter.
// Timed as above ([withinFactor]); filler, not more keys, shares the line, as
// under race a key costs what scanning 25 KiB does. A scan per key costs 26 to
// 29 times as much; reading back to the line's start, not past the markup
// ([pastMarkup]), 43 to 58. Mutation: either, and a group fails.
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

// A VALUE OF MANY ESCAPED BREAKS COSTS WHAT ITS BYTES COST, however long it
// runs without a space: a password's value in one run of 128 KiB costs what
// the same pieces cost in runs of 1 KiB. Whether a value runs on across a break
// turns on whether a letter or digit follows before the next space
// ([wordAfter]); reading to the space at every break cost breaks times length
// (days for the 32 MiB a box's file is read whole up to), as did a key after
// every break. Read up to its first letter or digit, a break's read never
// passes the next break's `n`.
//
// Timed as above ([withinFactor]), the text ending in a line break so Settled
// reads its values: reading to the space at every break costs 50 to 120 times
// as much; three allowed. Mutation: do that, and every group fails.
func TestAValueOfManyEscapedBreaksIsLinear(t *testing.T) {
	t.Parallel()
	read := func(text string) {
		_ = Secrets(text)
		_ = Settled(text)
	}
	for name, group := range map[string]struct{ key, piece string }{
		"a password in pieces":                   {"pwd=", `x\n`},
		"a password in pieces, its CRLF escaped": {"pwd=", `x\r\n`},
		"a password in pieces after punctuation": {"pwd=", `x\n-`},
		"a key after every break, over and over": {"", `pwd=x\n`},
	} {
		short, long := runsOf(group.key, group.piece, 1<<10), runsOf(group.key, group.piece, 128<<10)
		if ok, shortCost, longCost := withinFactor(read, short, long, 3); !ok {
			t.Errorf("%s: %v in one run of 128 KiB against %v in runs of 1 KiB — every break reads "+
				"the rest of its run again", name, longCost, shortCost)
		}
	}
}

// runsOf is 128 KiB of runs of up to runBytes, each key and then piece written
// over and over, a space after each run and a line break at the end.
func runsOf(key, piece string, runBytes int) string {
	const size = 128 << 10
	var b strings.Builder
	for b.Len()+len(key)+len(piece)+1 <= size {
		run := b.Len()
		b.WriteString(key)
		for b.Len()+len(piece)+1 <= size && b.Len()-run+len(piece) <= runBytes {
			b.WriteString(piece)
		}
		b.WriteByte(' ')
	}
	b.WriteByte('\n')
	return b.String()
}

// A LIVE VIEW OF MANY PASSWORD KEYS COSTS ONE READ OF THEM: [Settled] over
// lines of password keys, each line's last decided by the next, costs what its
// bytes cost. Read again in each prefix a hold cut the text at, the hold moved
// up a line and Settled read the text once a line (47 to 75 times [Secrets]
// over 32 KiB in lines of 256 bytes). So the text is read once ([valueHolds]).
//
// COUNTED, NOT TIMED: the bytes Settled hands its readers ([settle]), four
// reads of the text, six where keys were read beside values ([claims]); at
// least three, so a count that stopped counting fails too. Timed ratios moved
// with runner load ([withinFactor]). Mutation: reread in every prefix a line
// ends, and groups read some sixty times; count nothing, and every group fails.
func TestALiveViewOfManyPasswordKeysCostsOneRead(t *testing.T) {
	t.Parallel()
	for name, group := range passwordKeyGroups {
		text := linesOf(group, 32<<10, 256)
		settled, read := settle(text)
		switch {
		case settled != Settled(text):
			t.Errorf("%s: settle settled %d bytes and Settled %d", name, settled, Settled(text))
		case read < 3*len(text):
			t.Errorf("%s: Settled handed its readers %d bytes to read over a text of %d, less "+
				"than three reads of it: the count no longer counts what they read", name, read, len(text))
		case read > 6*len(text):
			t.Errorf("%s: Settled handed its readers %d bytes to read over a text of %d, more "+
				"than six reads of it, so it reads the text again for every line it holds",
				name, read, len(text))
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

// How many samples of each text one measurement takes, in turn, keeping the
// fastest of each, after one sample of each to warm up; how many measurements
// a comparison may take before it fails; the least a sample may cost for a
// comparison to judge it; and the most reads one sample may take to reach it.
const (
	interleavedReads = 5
	measureRounds    = 3
	judgeFloor       = 5 * time.Millisecond
	maxReps          = 1 << 12
)

// timer is what a comparison measures a text by: the time reps reads of it
// take. [withinFactor] reads the wall clock; the self-test hands [judge] one
// that answers by arithmetic, so what the comparison decides is tested with
// no scheduler in it.
type timer func(text string, reps int) time.Duration

// withinFactor reports whether reading long costs at most factor times what
// reading short does, with the cost of one read of each, as the last
// measurement took them. See [judge].
func withinFactor(read func(string), short, long string, factor int) (bool, time.Duration, time.Duration) {
	return judge(func(text string, reps int) time.Duration {
		start := time.Now()
		for range reps {
			read(text)
		}
		return time.Since(start)
	}, short, long, factor)
}

// judge is [withinFactor] over timed.
//
// EVERY SAMPLE IS AT LEAST [judgeFloor] LONG: reps doubled until the short
// text's sample reaches twice the floor, the long text taken at the same reps.
// A loaded runner's noise is a preemption's overshoot, milliseconds whatever
// the sample, so on a short one it is most of it. A text that cannot reach the
// floor in [maxReps] reads fails as too cheap to judge.
//
// EACH MEASUREMENT IS THE FASTEST OF [interleavedReads] SAMPLES OF EACH, taken
// in turn after a warm-up, so a busy runner slows both or neither. A FAILING
// COMPARISON IS MEASURED AGAIN, up to [measureRounds] times, and fails only if
// every one does; guarded regressions fail by six to eighty times the factor.
func judge(timed timer, short, long string, factor int) (bool, time.Duration, time.Duration) {
	// TWICE THE FLOOR, so that the fastest of a measurement's samples, which
	// can come in under the one that calibrated, still clears it.
	reps, calibrated := 1, timed(short, 1)
	for reps < maxReps && calibrated < 2*judgeFloor {
		reps *= 2
		calibrated = timed(short, reps)
	}
	// ONE READ OF EACH TO WARM UP, never a sample: at the calibrated
	// count, a regression's long text is the costliest thing here.
	timed(short, 1)
	timed(long, 1)
	var shortCost, longCost time.Duration
	for round := 0; round < measureRounds; {
		shortCost, longCost = time.Duration(1<<62), time.Duration(1<<62)
		for range interleavedReads {
			shortCost = min(shortCost, timed(short, reps))
			sample := timed(long, reps)
			longCost = min(longCost, sample)
			if sample > time.Duration(factor)*calibrated {
				// FAILED OUTRIGHT, on one sample: a long sample past
				// the factor times the calibrated short one fails the
				// round whatever the others say, and calibration
				// multiplies a regression's cost too, so its other
				// samples could outlast the test's own deadline and
				// report a timeout in place of the assertion.
				break
			}
		}
		if shortCost < judgeFloor {
			if reps >= maxReps {
				return false, shortCost / time.Duration(reps), longCost / time.Duration(reps)
			}
			// UNDER THE FLOOR STILL: read more, and measure again,
			// which is no round of the comparison's.
			reps *= 2
			calibrated = timed(short, reps)
			continue
		}
		if longCost <= time.Duration(factor)*shortCost {
			return true, shortCost / time.Duration(reps), longCost / time.Duration(reps)
		}
		round++
	}
	return false, shortCost / time.Duration(reps), longCost / time.Duration(reps)
}

// WHAT THE COMPARISON DECIDES, with no clock in it: a [timer] that answers by
// arithmetic, so the rounds, the calibration, the floor and the factor are
// held exactly, on any runner, in no time.
//
// A spin of the CPU per read would not do: its noise is a preemption's
// overshoot, milliseconds on a sample of two, which let a square cost pass
// against a factor it should have failed by 2.7 times. The comparison over the
// wall clock is held by the four tests above.
//
// Mutation: drop the calibration, the floor, the re-measure, its failing only
// when every round does, or a round's stop at a long sample past the factor,
// and a case here fails.
func TestWithinFactorFailsWhatEveryMeasurementSays(t *testing.T) {
	t.Parallel()
	const factor = 64
	small, large := strings.Repeat("x", 4), strings.Repeat("x", 64)
	// costs is a timer whose read of a text costs cost(len(text)), the
	// same every time: a sample of reps reads, reps times it.
	costs := func(cost func(n int) time.Duration) timer {
		return func(text string, reps int) time.Duration { return time.Duration(reps) * cost(len(text)) }
	}
	square := costs(func(n int) time.Duration { return time.Duration(n*n) * time.Millisecond })
	if ok, s, l := judge(square, small, large, factor); ok {
		t.Errorf("a square cost passed: %v against %v", l, s)
	}
	linear := costs(func(n int) time.Duration { return time.Duration(n) * time.Millisecond })
	if ok, s, l := judge(linear, small, large, factor); !ok {
		t.Errorf("a linear cost failed: %v against %v", l, s)
	}
	// A READ FAR BELOW THE FLOOR, a microsecond a byte (4µs and 64µs), is
	// read enough times to reach it, and judged: linear passes, square fails.
	cheapLinear := costs(func(n int) time.Duration { return time.Duration(n) * time.Microsecond })
	if ok, s, l := judge(cheapLinear, small, large, factor); !ok {
		t.Errorf("a cheap linear cost failed: %v against %v", l, s)
	}
	cheapSquare := costs(func(n int) time.Duration { return time.Duration(n*n) * time.Microsecond })
	if ok, s, l := judge(cheapSquare, small, large, factor); ok {
		t.Errorf("a cheap square cost passed: %v against %v", l, s)
	}
	// A READ NO NUMBER OF READS BRINGS TO THE FLOOR fails, whatever its
	// ratio: one that costs nothing.
	if ok, s, l := judge(costs(func(int) time.Duration { return 0 }), small, large, factor); ok {
		t.Errorf("a read that costs nothing passed: %v against %v", l, s)
	}
	// A BURST OUTLASTS TWO MEASUREMENTS of a linear cost, every long sample
	// ten times as slow, and the third passes it; the same burst on every
	// measurement fails it.
	bursts := func(slowSamples int) timer {
		longSamples := 0
		return func(text string, reps int) time.Duration {
			cost := linear(text, reps)
			if text == large {
				if longSamples < slowSamples {
					cost *= 10
				}
				longSamples++
			}
			return cost
		}
	}
	// The warm-up read, then one long sample a slowed measurement takes
	// before its stop: ten times the linear cost is past the factor.
	if ok, s, l := judge(bursts(1+2), small, large, factor); !ok {
		t.Errorf("a linear cost slowed for two measurements failed: %v against %v", l, s)
	}
	if ok, s, l := judge(bursts(1+measureRounds), small, large, factor); ok {
		t.Errorf("a linear cost slowed on every measurement passed: %v against %v", l, s)
	}
	// AN EXPENSIVE SQUARE STOPS AT ITS FIRST LONG SAMPLE OF EACH ROUND,
	// which is past the factor on its own: a regression's long text is
	// read once to warm up and once a round, never for every sample.
	longReads := 0
	counted := func(text string, reps int) time.Duration {
		if text == large {
			longReads++
		}
		return square(text, reps)
	}
	if ok, s, l := judge(counted, small, large, factor); ok {
		t.Errorf("an expensive square cost passed: %v against %v", l, s)
	}
	if want := 1 + measureRounds; longReads != want {
		t.Errorf("an expensive square's long text was timed %d times, want the warm-up and once a round, %d",
			longReads, want)
	}
}
