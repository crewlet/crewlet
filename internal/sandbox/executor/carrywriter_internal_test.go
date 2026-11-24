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
		{"opencode", OpenCode{}, openCodeRelease, []string{openCodeRelease, "2.3.1", "2.3.3", ""}},
		{"codex", Codex{}, codexCarryRelease, []string{codexCarryRelease, "0.170.0", "0.172.0", ""}},
		{"pi", Pi{}, piSupported, []string{piSupported, "1.5.9", "1.6.1", ""}},
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

// AN UNREAD WRITER CONTINUES IN NO BOX, for every carrier (all ten): a
// session whose writer named no release this build reads (a pre-release, a
// snapshot, none, which releaseOf reads as "") is refused alone and beside a
// box of every release, saying why; read as current, it would be carried into
// a box older than its writer. And a Codex, Copilot, Kimi Code, Muse Code,
// Hermes Agent, Pi, Grok Build or Cursor Agent box of another release than its
// one supported continues not even its own copy ([inPlaceGate]).
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
		{"opencode", OpenCode{}, []string{openCodeRelease, "2.3.3", "9.0.0"}},
		{"codex", Codex{}, []string{codexCarryRelease}},
		{"copilot", Copilot{}, []string{copilotFloor()}},
		{"kimi-code", Kimi{}, []string{kimiSupported}},
		{"muse-code", Muse{}, []string{museSupported()}},
		{"hermes", Hermes{}, []string{hermesFloor()}},
		{"pi", Pi{}, []string{piSupported}},
		{"grok", Grok{}, []string{grokSupported()}},
		{"cursor-agent", CursorAgent{}, []string{cursorReleaseName}},
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
	for name, gate := range map[string]inPlaceGate{
		"codex": Codex{}, "copilot": Copilot{}, "kimi-code": Kimi{}, "muse-code": Muse{}, "hermes": Hermes{},
		"pi": Pi{}, "grok": Grok{}, "cursor-agent": CursorAgent{},
	} {
		if ok, why := gate.continuesInPlace("9.9.9"); ok || why == "" {
			t.Errorf("%s: a box of 9.9.9 continues its own copy %v (%q), want refused", name, ok, why)
		}
	}
}

// KIMI CODE, COPILOT, MUSE CODE, HERMES AGENT, PI, GROK BUILD AND CURSOR AGENT
// JUDGE A WRITER ALONE AS THEY DO BESIDE A BOX: only a session their one
// supported release wrote continues ([carrier] ContinuesWriter, which the
// restorer asks with no box), any other writer refused saying why, and
// Continues beside a supported box answers the same.
//
// Mutation: answer yes in any of their ContinuesWriter, or judge a writer
// otherwise than its Continues does, and its row goes red.
func TestSingleReleaseCarriersContinueOnlyTheirOneReleasesWriter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name      string
		carrier   carrier
		supported string
	}{
		{"kimi-code", Kimi{}, kimiSupported},
		{"copilot", Copilot{}, copilotFloor()},
		{"muse-code", Muse{}, museSupported()},
		{"hermes", Hermes{}, hermesFloor()},
		{"pi", Pi{}, piSupported},
		{"grok", Grok{}, grokSupported()},
		{"cursor-agent", CursorAgent{}, cursorReleaseName},
	} {
		for writer, want := range map[string]bool{c.supported: true, "9.9.9": false, "0.0.1": false, "": false} {
			ok, why := c.carrier.ContinuesWriter(writer)
			if ok != want || ok == (why != "") {
				t.Errorf("%s: writer %q continues %v (%q), want %v", c.name, writer, ok, why, want)
			}
			if beside, _ := c.carrier.Continues(c.supported, writer); beside != ok {
				t.Errorf("%s: writer %q continues %v beside box %s, and %v alone", c.name, writer, beside,
					c.supported, ok)
			}
		}
	}
}

// KIMI CODE'S, COPILOT'S, MUSE CODE'S, HERMES AGENT'S, PI'S, GROK BUILD'S AND
// CURSOR AGENT'S ARCHIVES ARE METERED AS NO FIGURE: this build reads no count
// or estimate of their sessions' next request off an archive, so the take's
// meter of each states none and fails nothing, whatever the archive holds.
//
// Mutation: hand any of them Claude Code's or Codex's meter, or a figure read
// off any line, and its row goes red.
func TestSingleReleaseArchivesAreMeteredAsNoFigure(t *testing.T) {
	t.Parallel()
	archive := `{"type":"usage.record","input_tokens":123456}` + "\n"
	for name, c := range map[string]carrier{"kimi-code": Kimi{}, "copilot": Copilot{}, "muse-code": Muse{},
		"hermes": Hermes{}, "pi": Pi{},
		"grok": Grok{}, "cursor-agent": CursorAgent{}} {
		read, err := c.meter(t.Context(), strings.NewReader(archive), "s-1", "1.0.0", nil)
		if err != nil || read != (meterReading{}) {
			t.Errorf("%s: the meter reads %+v (%v), want no figure", name, read, err)
		}
	}
}

// A RELEASE NAMES ITS THREE NUMBERS AND NO MORE, read as
// harnessprofile.Release reads it, so a writer or box check and the run's
// script read one release: a fourth number, a pre-release or a build suffix
// names none, and what a carrier is asked of it is refused, never taken for
// the release its first three numbers spell.
//
// Mutation: read the first three numbers up to the first other character, as
// releaseNumbers did, and a row naming a fourth number or a suffix reads.
func TestAReleaseNamesItsThreeNumbersAndNoMore(t *testing.T) {
	t.Parallel()
	for printed, want := range map[string]bool{
		"1.4.25": true, "v2.3.2": true, "2.1.310 (Claude Code)": true, "GitHub Copilot CLI 1.0.114.": true,
		"Muse Code 1.4.25 (1.4.25-R9361.1)": true, "1.4.25.1": false, "1.4.25-rc.1": false, "1.4.25+b": false,
		"1.4.x": false, "1.4": false, "": false,
	} {
		if _, got := releaseNumbers(printed); got != want {
			t.Errorf("releaseNumbers(%q) reads %v, want %v", printed, got, want)
		}
	}
	for name, c := range map[string]struct {
		carrier carrier
		writer  string
	}{
		"claude-code": {ClaudeCode{}, "2.1.310.1"},
		"opencode":    {OpenCode{}, openCodeRelease + ".1"},
		"copilot":     {Copilot{}, copilotFloor() + ".1"},
	} {
		if ok, why := c.carrier.ContinuesWriter(c.writer); ok || why == "" {
			t.Errorf("%s: a writer of %s continues %v (%q), want refused", name, c.writer, ok, why)
		}
	}
}
