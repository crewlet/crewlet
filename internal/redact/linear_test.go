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
