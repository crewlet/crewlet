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
// lines of four KiB and on lines twice the bound, each timed as the fastest of
// three interleaved reads, so a runner busy for a moment slows both or neither
// and no absolute duration has to hold on a loaded machine — the clock this
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
	read := func(text string) time.Duration {
		start := time.Now()
		_, _ = keyBlocks(text)
		return time.Since(start)
	}
	for name, group := range map[string]string{
		"a bare BEGIN, over and over": "-----BEGIN PRIVATE KEY-----",
		"an END, over and over":       "-----END RSA PRIVATE KEY----- ",
		// A key on one line cut in its middle, over and over: each
		// armour's body read up to and back from the mark.
		"a cut key on one line, over and over":    "-----BEGIN PRIVATE KEY----- Ab12cdEF34gh[...truncated]Ij56klMN78op -----END PRIVATE KEY----- ",
		"a cut body before an END, over and over": "x[… 812 bytes truncated]Ab12cdEF34gh Ij56klMN78op=-----END RSA PRIVATE KEY----- ",
	} {
		short, long := lines(group, 4<<10), lines(group, 2*MaxKeyBlockBytes)
		shortCost, longCost := time.Duration(1<<62), time.Duration(1<<62)
		for range 3 {
			shortCost, longCost = min(shortCost, read(short)), min(longCost, read(long))
		}
		if longCost > 3*shortCost {
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
// test above is, fastest of three interleaved reads, on lines of 1 KiB and 30
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
	read := func(text string) time.Duration {
		start := time.Now()
		_, _ = keyBlocks(text)
		return time.Since(start)
	}
	short, long := texts(1<<10), texts(30<<10)
	for name := range short {
		shortCost, longCost := time.Duration(1<<62), time.Duration(1<<62)
		for range 3 {
			shortCost, longCost = min(shortCost, read(short[name])), min(longCost, read(long[name]))
		}
		if longCost > 3*shortCost {
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
// above are, fastest of three interleaved reads, through [Secrets] and
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
	read := func(text string) time.Duration {
		start := time.Now()
		_ = Secrets(text)
		_ = Settled(text)
		return time.Since(start)
	}
	for name, group := range passwordKeyGroups {
		for _, line := range []struct {
			where string
			of    func(group string, lineBytes int) string
		}{{"after", keysBefore}, {"before", keysAfter}} {
			short, long := line.of(group, 2<<10), line.of(group, 256<<10)
			shortCost, longCost := time.Duration(1<<62), time.Duration(1<<62)
			for range 3 {
				shortCost, longCost = min(shortCost, read(short)), min(longCost, read(long))
			}
			if longCost > 3*shortCost {
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
// eight times Settled over 4 KiB of the same lines, each timed as the fastest
// of three interleaved reads, as the tests above are. This used to compare
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
	timed := func(text string) time.Duration {
		start := time.Now()
		_ = Settled(text)
		return time.Since(start)
	}
	const scale = 8
	for name, group := range passwordKeyGroups {
		small, large := linesOf(group, 4<<10, 256), linesOf(group, scale*4<<10, 256)
		smallCost, largeCost := time.Duration(1<<62), time.Duration(1<<62)
		for range 3 {
			smallCost, largeCost = min(smallCost, timed(small)), min(largeCost, timed(large))
		}
		if largeCost > 3*scale*smallCost {
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
