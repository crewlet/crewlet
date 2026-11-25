package executor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// copilotTrimSession is the session [copilotPackRun] packs.
const copilotTrimSession = "77777777-7777-4777-8777-777777777777"

// copilotUnreadKinds are the events the measurement found a 1.0.114 resume
// never reads before its newest applied compaction, written out apart from
// [copilotTrimmedKinds], which the pack cuts by.
var copilotUnreadKinds = []string{"assistant.message", "assistant.turn_start", "assistant.turn_end",
	"tool.execution_start", "tool.execution_complete", "session.usage_record", "system.message", "hook.start",
	"hook.end", "session.shutdown", "session.resume", "session.usage_checkpoint", "session.compaction_start",
	"session.compaction_complete"}

// copilotResumeLoads is what a 1.0.114 resume of journal loads, read as the
// measurement read it (each line's own type and data, decoded): every line
// from the `session.compaction_start` of the newest compaction that applied
// on, and before it every line but those of [copilotUnreadKinds]; journal
// whole where no compaction applied past its first line.
func copilotResumeLoads(t *testing.T, journal []byte) []byte {
	t.Helper()
	lines := bytes.SplitAfter(journal, []byte("\n"))
	type event struct {
		Type string `json:"type"`
		Data struct {
			Attempt string `json:"compactionAttemptId"`
			Success bool   `json:"success"`
			Outcome string `json:"outcome"`
		} `json:"data"`
	}
	starts, boundary := map[string]int{}, -1
	kinds := make([]string, len(lines))
	for i, line := range lines {
		var e event
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		kinds[i] = e.Type
		switch {
		case e.Type == "session.compaction_start":
			starts[e.Data.Attempt] = i
		case e.Type == "session.compaction_complete" && e.Data.Success && e.Data.Outcome == "applied":
			if at, ok := starts[e.Data.Attempt]; ok {
				boundary = at
			}
		}
	}
	if boundary < 1 {
		return journal
	}
	var out []byte
	for i, line := range lines {
		if i >= boundary || !slices.Contains(copilotUnreadKinds, kinds[i]) {
			out = append(out, line...)
		}
	}
	return out
}

// copilotPackRun packs journal as session copilotTrimSession's events.jsonl in
// a box of its own, beside a workspace.yaml and a checkpoint, at at, the
// session written by release, and returns the archive's members, its size and
// the word left beside it ("" for none).
func copilotPackRun(t *testing.T, journal []byte, release string, at trimPoints) (members map[string]string,
	size int64, mark string) {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	dir := p.CopilotSessionDir(copilotTrimSession)
	for name, body := range map[string][]byte{"events.jsonl": journal, copilotWorkspace: []byte("summary_count: 1\n"),
		copilotCheckpoints + "/index.md": []byte("| # | Title | File |\n")} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "s=" + copilotTrimSession + " " + trimRelVar + "=" + shellword.Quote(release) + " packed=\n" +
		copilotPackAt(p, packAt{staged: staged, dest: dest}, at) + `[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return codexMembers(t, archive), int64(len(archive)), string(word)
}

// copilotTrimMember is the journal's member in [copilotPackRun]'s archive.
const copilotTrimMember = copilotSessionState + "/" + copilotTrimSession + "/events.jsonl"

// A TRIMMED COPILOT PACK KEEPS WHAT ITS RESUME LOADS: past the point, the packed
// journal is every line from its newest applied compaction's start on and,
// before it, every line a 1.0.114 resume reads (its first, every person's
// message and instruction update), as the measured resume reads them; the
// workspace and checkpoints go beside it whole, and the pack says what it
// kept beside the archive.
//
// Mutation: keep a dropped kind, drop a person's message, cut at the oldest
// compaction or at its complete rather than its start, or leave the files
// beside the journal out, and a case goes red.
func TestATrimmedCopilotPackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"compacted-after-tools.events.jsonl", "compacted-twice.events.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			journal := copilotFixture(t, fixture)
			want := copilotResumeLoads(t, journal)
			if bytes.Equal(want, journal) {
				t.Fatal("the fixture holds no compaction to trim at")
			}
			members, size, mark := copilotPackRun(t, journal, copilotFloor(), trimEverything)
			if got := members[copilotTrimMember]; got != string(want) {
				t.Errorf("the pack kept %d bytes of the journal, want the %d its resume loads", len(got), len(want))
			}
			held := copilotSessionState + "/" + copilotTrimSession + "/"
			if members[held+copilotWorkspace] == "" || members[held+copilotCheckpoints+"/index.md"] == "" {
				t.Errorf("the trimmed archive holds %d members, want the workspace and checkpoints beside the journal",
					len(members))
			}
			if got := parseTrimMark(mark, size); got != (sandbox.ArchiveTrim{RecordBytes: int64(len(journal)),
				KeptBytes: int64(len(want))}) {
				t.Errorf("the pack's word %q reads as %+v, want the journal's bytes before and kept", mark, got)
			}
		})
	}
}

// A COPILOT PACK WITH NOTHING TO TRIM AT IS PACKED WHOLE: a journal whose only
// compaction was cancelled, one with none, one whose tool results hold an
// applied compaction's events as objects, and a rotated head (unmeasured)
// keep every line, and no word says otherwise.
//
// Mutation: count a cancelled compaction, match a compaction's line anywhere
// in a line, or cut a journal that opens with no session.start, and a case
// goes red.
func TestACopilotPackWithNothingToTrimAtIsPackedWhole(t *testing.T) {
	t.Parallel()
	compacted := copilotFixture(t, "compacted-twice.events.jsonl")
	var nested []byte
	for _, line := range bytes.SplitAfter(compacted, []byte("\n")) {
		if bytes.HasPrefix(line, []byte(`{"type":"session.compaction_`)) {
			line = fmt.Appendf(nil, `{"type":"tool.execution_complete","data":{"result":%s}}`+"\n",
				bytes.TrimSuffix(line, []byte("\n")))
		}
		nested = append(nested, line...)
	}
	for name, journal := range map[string][]byte{
		"a cancelled compaction": copilotFixture(t, "compaction-cancelled.events.jsonl"),
		"no compaction":          copilotFixture(t, "tool.events.jsonl"),
		"a result naming them":   nested,
		"a rotated head":         copilotFixture(t, "compacted-resume-rotated.events.jsonl"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			members, _, mark := copilotPackRun(t, journal, copilotFloor(), trimEverything)
			if got := members[copilotTrimMember]; got != string(journal) || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(got), len(journal),
					mark)
			}
		})
	}
}

// ONLY THE MEASURED COPILOT RELEASE IS TRIMMED: a session another release
// wrote, or one whose writer the pack was not told, is packed whole past the
// point.
//
// Mutation: drop the release's check, or trim on a release that reads as
// 1.0.114's prefix, and a case goes red.
func TestOnlyTheMeasuredCopilotReleaseIsTrimmed(t *testing.T) {
	t.Parallel()
	journal := copilotFixture(t, "compacted-twice.events.jsonl")
	for _, release := range []string{"", "1.0.113", "1.0.120", copilotFloor() + "1"} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			members, _, mark := copilotPackRun(t, journal, release, trimEverything)
			if got := members[copilotTrimMember]; got != string(journal) || mark != "" {
				t.Errorf("a session %q wrote was packed as %d of %d bytes, saying %q; want it whole", release,
					len(got), len(journal), mark)
			}
		})
	}
}

// EVERY COPILOT PACK TRIMS AT THE TAKE'S BOUNDS, BY THE RELEASE THAT WROTE ITS
// SESSION: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt], the run's end naming the box's bare release, read as
// the engine reads a record's.
//
// Mutation: pack one path at other points or untrimmed, or leave the run's
// end's release unset, and a case goes red.
func TestEveryCopilotPackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	save, err := Copilot{}.PackSave(p, sessionRecord{id: copilotTrimSession}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ script, want string }{
		"end": {copilotCapture(p), bareReleaseShell(trimRelVar) +
			copilotPackAt(p, packAt{staged: p.SessionStaging(), dest: p.SessionCapture()}, trimAt)},
		"stop": {Copilot{}.Pack(p, copilotTrimSession, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
			copilotPackAt(p, packAt{staged: p.SessionPackStaging("1"), dest: p.SessionPackHeld("1"), working: true},
				trimAt)},
		"save": {save, copilotPackAt(p, packAt{staged: p.SessionSaveStaging("t", "1"),
			dest: p.SessionSave("t", "1"), working: true}, trimAt)},
	} {
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, fmt.Sprint(trimPlainPoint)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	for printed, want := range map[string]string{"GitHub Copilot CLI 1.0.114.": "1.0.114",
		"GitHub Copilot CLI 1.0.114-beta.1.": "", "": ""} {
		out, _, code := codexShell(t, t.TempDir(), "v="+shellword.Quote(printed)+"\n"+bareReleaseShell(trimRelVar)+
			`printf '%s' "$`+trimRelVar+`"`)
		if code != 0 || out != want || releaseOf(printed) != want {
			t.Errorf("%q: the run's end trims by %q (exit %d), want %q as the engine reads it", printed, out, code,
				want)
		}
	}
}
