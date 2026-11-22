package executor

import (
	"strings"
	"testing"
)

// EVERY CARRIER REFUSES A BOX THAT NAMED NO RELEASE, AND JUDGES A WRITER ALONE
// AS IT JUDGES IT BESIDE A BOX: a box release that does not read is no
// release any rule here was read for, so it never continues a carried
// session; and for a box whose release reads and holds every writer below,
// Continues answers exactly what ContinuesWriter does, which the restorer
// asks with no box ([Runner.Restores]).
//
// Mutation: take an empty or unreadable box release for a current one, or
// let ContinuesWriter judge a writer otherwise than Continues does, and a row
// fails.
func TestEveryCarrierRefusesABoxThatNamedNoReleaseAndJudgesAWriterAlone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		carrier carrier
		box     string
		writers []string
	}{
		{"claude-code", ClaudeCode{}, "2.1.322", []string{"2.1.310", "2.1.300", "2.1.299", ""}},
		{"opencode", OpenCode{}, "2.2.7", []string{"2.2.7", "2.0.32", "2.0.31", ""}},
		{"codex", Codex{}, codexCarryRelease, []string{codexCarryRelease, "0.170.0", "0.172.0", ""}},
	} {
		for _, box := range []string{"", "nightly", "1.2"} {
			if ok, why := c.carrier.Continues(box, c.writers[0]); ok || !strings.Contains(why, "named no release") {
				t.Errorf("%s: a box release %q continues %v (%q), want refused as naming no release",
					c.name, box, ok, why)
			}
		}
		for _, writer := range c.writers {
			ok, why := c.carrier.Continues(c.box, writer)
			alone, aloneWhy := c.carrier.ContinuesWriter(writer)
			if ok != alone || why != aloneWhy {
				t.Errorf("%s: writer %q continues %v (%q) beside box %s, and %v (%q) alone",
					c.name, writer, ok, why, c.box, alone, aloneWhy)
			}
		}
	}
}
