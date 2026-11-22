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

// AN UNREAD WRITER CONTINUES IN NO BOX, for every carrier (all five): a
// session whose writer named no release this build reads (a pre-release, a
// snapshot, none, which releaseOf reads as "") is refused alone and beside a
// box of every release, saying why; read as current, it would be carried into
// a box older than its writer. And a Codex, Copilot or Kimi Code box of another
// release than its one supported continues not even its own copy
// ([inPlaceGate]).
//
// Mutation: take an unread writer for a current one in any carrier's
// ContinuesWriter, or let either box continue its own copy on another release,
// and a row goes red.
func TestAnUnreadWriterContinuesInNoBox(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		carrier carrier
		boxes   []string
	}{
		{"claude-code", ClaudeCode{}, []string{"2.1.310", "2.1.322", "9.0.0"}},
		{"opencode", OpenCode{}, []string{"2.0.32", "2.2.7", "9.0.0"}},
		{"codex", Codex{}, []string{codexCarryRelease}},
		{"copilot", Copilot{}, []string{copilotFloor()}},
		{"kimi-code", Kimi{}, []string{kimiSupported}},
	} {
		if ok, why := c.carrier.ContinuesWriter(""); ok || why == "" {
			t.Errorf("%s: an unread writer continues %v (%q), want refused, saying why", c.name, ok, why)
		}
		for _, box := range c.boxes {
			if ok, why := c.carrier.Continues(box, ""); ok || why == "" {
				t.Errorf("%s: an unread writer continues %v (%q) in box %s, want refused", c.name, ok, why, box)
			}
		}
	}
	// EVERY CARRIER WHOSE BOX HOLDS A RELEASE TO ITS OWN COPY refuses another
	// release in place, as its run's gate does ([inPlaceGate]).
	for name, gate := range map[string]inPlaceGate{"codex": Codex{}, "copilot": Copilot{}, "kimi-code": Kimi{}} {
		if ok, why := gate.continuesInPlace("9.9.9"); ok || why == "" {
			t.Errorf("%s: a box of 9.9.9 continues its own copy %v (%q), want refused", name, ok, why)
		}
	}
}
