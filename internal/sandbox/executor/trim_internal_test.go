package executor

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// trimEverything is a pair of points every record is past.
var trimEverything = trimPoints{plain: 1, packed: 1 << 40}

// codexResumeLoads is what a 0.171.0 resume of rollout loads, read as the
// measurement read it (each line's own type, decoded): its first line and
// every line from its newest `compacted` on; rollout whole where it has none
// past its first two lines.
func codexResumeLoads(t *testing.T, rollout []byte) []byte {
	t.Helper()
	lines := bytes.SplitAfter(rollout, []byte("\n"))
	newest := -1
	for i, line := range lines {
		var env struct {
			Type string `json:"type"`
		}
		if len(bytes.TrimSpace(line)) > 0 && json.Unmarshal(line, &env) == nil && env.Type == "compacted" {
			newest = i
		}
	}
	if newest < 2 {
		return rollout
	}
	return append(append([]byte(nil), lines[0]...), bytes.Join(lines[newest:], nil)...)
}

// codexPackRun packs rollout in a box of its own at at, the session written
// by release (its pack's [trimRelVar]), and returns the archive's one member,
// the archive's size and the word the pack left beside it ("" for none).
func codexPackRun(t *testing.T, rollout []byte, release string, at trimPoints) (member string, size int64,
	mark string) {
	t.Helper()
	p := codexBox(t, rollout)
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "sid=" + codexThread + " " + trimRelVar + "=" + shellword.Quote(release) + " packed=\n" +
		codexPackAt(p, staged, dest, fmt.Sprintf("exit %d", packNothing), fmt.Sprintf("exit %d", packFailed), at) +
		`[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	members := codexMembers(t, archive)
	if len(members) != 1 {
		t.Fatalf("the archive holds %d members, want the rollout alone", len(members))
	}
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return members[codexArchiveMember(codexThread)], int64(len(archive)), string(word)
}

// THE TRIM POINTS ARE THE TAKE'S BOUNDS: the plaintext point is what the take's
// scrub reads less the tar's own bytes, the packed one what a take keeps, so a
// pack trims exactly where its take would refuse it whole; and a bound moved
// breaks this test, so whoever moves it reads the trim again. The slack holds a
// one-member archive's tar bytes at the point.
//
// Mutation: put either point below or above its bound, or shrink the slack
// under a member's header, padding and end, and a check goes red.
func TestTheTrimPointsAreTheTakesBounds(t *testing.T) {
	t.Parallel()
	if scrubPlainMax != executorkeep.SessionPlainMaxBytes || executorkeep.SessionPlainMaxBytes != 256<<20 {
		t.Errorf("the scrub reads %d bytes of plaintext and a session is kept within %d, want both 256 MiB: "+
			"a bound moved, so read the trim's points again", scrubPlainMax, executorkeep.SessionPlainMaxBytes)
	}
	if executorkeep.SessionMaxBytes != 48<<20 {
		t.Errorf("a session is kept at %d bytes, want 48 MiB: a bound moved, so read the trim's points again",
			executorkeep.SessionMaxBytes)
	}
	if trimPlainPoint != scrubPlainMax-trimTarSlack || trimPackedPoint != executorkeep.SessionMaxBytes {
		t.Errorf("the points are %d plaintext and %d packed, want %d and %d", trimPlainPoint, trimPackedPoint,
			scrubPlainMax-trimTarSlack, executorkeep.SessionMaxBytes)
	}
	if trimAt != (trimPoints{plain: trimPlainPoint, packed: trimPackedPoint}) {
		t.Errorf("every pack trims at %+v, not the take's bounds", trimAt)
	}
	// A MEMBER AT THE POINT, tarred: its header, a long name's header pair,
	// its padding to the block and the archive's end.
	var tarred bytes.Buffer
	tw := tar.NewWriter(&tarred)
	name := strings.Repeat("n", 200) + "/" + codexArchiveMember(codexThread)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: 0, Format: tar.FormatGNU}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if overhead := int64(tarred.Len()) + 511; trimPlainPoint+overhead > scrubPlainMax {
		t.Errorf("a member at the point tars to %d bytes past it, past the scrub's bound", overhead)
	}
}

// A TRIMMED CODEX PACK KEEPS WHAT ITS RESUME LOADS: past the point, the packed
// rollout is its first line and every line from its newest `compacted` on, as
// the measured resume reads it, and nothing before; the pack says so beside the
// archive in a word naming the archive's size and the record's bytes before and
// after, which the take reads back as the trim.
//
// Mutation: cut a line early or late, keep no first line, match the oldest
// boundary, or leave no word, and a case goes red.
func TestATrimmedCodexPackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"compaction-note-before.jsonl", "compaction-note-after.jsonl"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			whole := codexFixture(t, fixture)
			second := append(append([]byte(nil), whole...), codexCompactedAgain(t, whole)...)
			for name, rollout := range map[string][]byte{"one compaction": whole, "two": second} {
				want := codexResumeLoads(t, rollout)
				if bytes.Equal(want, rollout) {
					t.Fatalf("%s: the fixture holds no compaction to trim at", name)
				}
				member, size, mark := codexPackRun(t, rollout, codexCarryRelease, trimEverything)
				if member != string(want) {
					t.Errorf("%s: the pack kept %d bytes, want the %d its resume loads", name, len(member), len(want))
				}
				if got := parseTrimMark(mark, size); got != (sandbox.ArchiveTrim{RecordBytes: int64(len(rollout)),
					KeptBytes: int64(len(want))}) {
					t.Errorf("%s: the pack's word %q reads as %+v, want the record's bytes before and kept", name,
						mark, got)
				}
			}
		})
	}
}

// codexCompactedAgain is a second compaction appended to rollout: its own
// newest compacted line and the turn after it, written again.
func codexCompactedAgain(t *testing.T, rollout []byte) []byte {
	t.Helper()
	loads := codexResumeLoads(t, rollout)
	_, after, _ := bytes.Cut(loads, []byte("\n"))
	return after
}

// A CODEX PACK NOT PAST THE POINT, OR WITH NOTHING TO CUT AT, IS PACKED WHOLE:
// a rollout at the plaintext point, one with no `compacted` line, one whose only
// `compacted` is a payload's word rather than a line's type, and one whose
// boundary is its second line keep every line, and no word says otherwise.
//
// Mutation: trim at the point rather than past it, match `"type":"compacted"`
// anywhere in a line, or trim where nothing before the boundary is dropped,
// and a case goes red.
func TestACodexPackWithNothingToTrimIsPackedWhole(t *testing.T) {
	t.Parallel()
	compacted := codexFixture(t, "compaction-note-before.jsonl")
	plain := codexFixture(t, "rollout.jsonl")
	worded := append(append([]byte(nil), plain...), []byte(`{"timestamp":"2026-11-21T18:40:00.000Z",`+
		`"ordinal":99,"type":"response_item","payload":{"type":"compacted","text":"{\"type\":\"compacted\"}"}}`+
		"\n")...)
	first, _, _ := bytes.Cut(compacted, []byte("\n"))
	_, fromBoundary, _ := bytes.Cut(codexResumeLoads(t, compacted), []byte("\n"))
	early := append(append(append([]byte(nil), first...), '\n'), fromBoundary...)
	for name, c := range map[string]struct {
		rollout []byte
		at      trimPoints
	}{
		"at the point":            {compacted, trimPoints{plain: int64(len(compacted)), packed: 1 << 40}},
		"no compaction":           {plain, trimEverything},
		"a payload's word":        {worded, trimEverything},
		"the boundary its second": {early, trimEverything},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			member, _, mark := codexPackRun(t, c.rollout, codexCarryRelease, c.at)
			if member != string(c.rollout) || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(member),
					len(c.rollout), mark)
			}
		})
	}
}

// ONLY THE MEASURED CODEX RELEASE IS TRIMMED: a session another release wrote,
// or one whose writer the pack was not told, is packed whole past the point.
//
// Mutation: drop the release's check, or trim on a release that reads as
// 0.171.0's prefix, and a case goes red.
func TestOnlyTheMeasuredCodexReleaseIsTrimmed(t *testing.T) {
	t.Parallel()
	whole := codexFixture(t, "compaction-note-before.jsonl")
	for _, release := range []string{"", "0.170.0", "0.172.0", codexCarryRelease + "1"} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			member, _, mark := codexPackRun(t, whole, release, trimEverything)
			if member != string(whole) || mark != "" {
				t.Errorf("a session %q wrote was packed as %d of %d bytes, saying %q; want it whole", release,
					len(member), len(whole), mark)
			}
		})
	}
}

// A CODEX PACK PAST THE PACKED POINT IS TRIMMED AND PACKED AGAIN, and one still
// past it once trimmed is packed all the same, for its take to refuse: the
// plaintext under its point, the archive past its own is cut and re-tarred, the
// archive holding the cut record, and the word naming the archive kept.
//
// Mutation: skip the re-pack, check the plaintext alone, or refuse a pack still
// past the point, and a case goes red.
func TestACodexPackPastThePackedPointIsTrimmed(t *testing.T) {
	t.Parallel()
	whole := codexFixture(t, "compaction-note-before.jsonl")
	member, size, mark := codexPackRun(t, whole, codexCarryRelease, trimPoints{plain: 1 << 40, packed: 1})
	if want := codexResumeLoads(t, whole); member != string(want) {
		t.Errorf("the archive holds %d bytes, want the %d its resume loads", len(member), len(want))
	}
	if !parseTrimMark(mark, size).Trimmed() {
		t.Errorf("the pack's word %q does not name its archive as trimmed", mark)
	}
}

// THE TRIM READS ONLY WHOLE LINES: a save beside a Codex still writing copies
// the rollout's whole lines and trims that copy, so the line being written is
// in neither the archive nor the count of the record's bytes.
//
// Mutation: trim the live rollout rather than the pack's copy, or count it,
// and a case goes red.
func TestTheTrimReadsOnlyWholeLines(t *testing.T) {
	t.Parallel()
	whole := codexFixture(t, "compaction-note-before.jsonl")
	torn := append(append([]byte(nil), whole...), `{"timestamp":"2026-11-22T07:40:16.000Z","ordinal":31,"ty`...)
	member, size, mark := codexPackRun(t, torn, codexCarryRelease, trimEverything)
	want := codexResumeLoads(t, whole)
	if member != string(want) {
		t.Errorf("the save kept %d bytes, want the %d of the whole lines' cut", len(member), len(want))
	}
	if got := parseTrimMark(mark, size); got.RecordBytes != int64(len(whole)) {
		t.Errorf("the save counted %d bytes of record, want the %d of its whole lines", got.RecordBytes, len(whole))
	}
}

// EVERY CODEX PACK TRIMS AT THE TAKE'S BOUNDS, BY THE RELEASE THAT WROTE ITS
// SESSION: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt]; the run's end names the box's release, and every pack
// made on demand the release its record names.
//
// Mutation: pack one path at other points or untrimmed, or leave a path's
// release unset, and a case goes red.
func TestEveryCodexPackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	save, err := Codex{}.PackSave(p, sessionRecord{id: codexThread}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	nothing, failed := fmt.Sprintf("exit %d", packNothing), fmt.Sprintf("exit %d", packFailed)
	for name, c := range map[string]struct{ script, want string }{
		"end": {codexCapture(p), trimRelVar + "=$n\n" +
			codexPackAt(p, p.SessionStaging(), p.SessionCapture(), ":", ":", trimAt)},
		"stop": {Codex{}.Pack(p, codexThread, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
			codexPackAt(p, p.SessionPackStaging("1"), p.SessionPackHeld("1"), nothing, failed, trimAt)},
		"save": {save, codexPackAt(p, p.SessionSaveStaging("t", "1"), p.SessionSave("t", "1"), nothing, failed,
			trimAt)},
	} {
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, trimStaged(Codex{}, trimAt, `"$trm"`)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(PathsFor(box).SessionRecord(), "id "+codexThread+"\ncontinued yes\nwhy \nrelease codex-cli 0.171.0\n")
	box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: packNothing}, nil
	}
	if _, err := NewCodex().PackSave(t.Context(), box, "node:1", codexThread); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(trimRelVar+"="+shellword.Quote(codexCarryRelease)+"\n", "'", `'\''`)
	if execs := box.Execs(); len(execs) == 0 || !strings.Contains(execs[len(execs)-1].Command, want) {
		t.Errorf("a save names no release off its record to trim by: %+v", execs)
	}
}

// A TAKE READS ITS PACK'S TRIM ONLY BESIDE ITS OWN ARCHIVE: the word is the
// trim where it names the archive's size and a record cut to fewer bytes, on
// the trimming harness's release, and a chat file's run start restated where
// it reads as a `from` within the bytes kept; a word naming another archive,
// malformed or past its bound, on another release or harness, or not there,
// is no trim, and nor is one whose restated start is malformed.
//
// Mutation: drop the size's check, read the word for any release or harness,
// read none for a harness whose trim leaves files out, read a word past its
// bound, or read a restated start loosely, and a case goes red.
func TestATakeReadsItsPacksTrimOnlyBesideItsOwnArchive(t *testing.T) {
	t.Parallel()
	const size = 4096
	trimmed := sandbox.ArchiveTrim{RecordBytes: 300 << 20, KeptBytes: 40 << 20}
	word := fmt.Sprintf("%d %d %d\n", size, trimmed.RecordBytes, trimmed.KeptBytes)
	restated := trimmed
	restated.Prefix = sandbox.LinePrefix{Lines: 19, CRC: 123, Bytes: 4567}
	chat := func(start string) string {
		return fmt.Sprintf("%d %d %d %s\n", size, trimmed.RecordBytes, trimmed.KeptBytes, start)
	}
	for name, c := range map[string]struct {
		runner  *Runner
		release string
		word    string
		want    sandbox.ArchiveTrim
	}{
		"its own archive":            {NewCodex(), codexCarryRelease, word, trimmed},
		"another archive":            {NewCodex(), codexCarryRelease, fmt.Sprintf("%d 300 40\n", size+1), sandbox.ArchiveTrim{}},
		"no word":                    {NewCodex(), codexCarryRelease, "", sandbox.ArchiveTrim{}},
		"no line break":              {NewCodex(), codexCarryRelease, strings.TrimSuffix(word, "\n"), sandbox.ArchiveTrim{}},
		"kept no less":               {NewCodex(), codexCarryRelease, fmt.Sprintf("%d 40 40\n", size), sandbox.ArchiveTrim{}},
		"a padded number":            {NewCodex(), codexCarryRelease, fmt.Sprintf("%d 0300 40\n", size), sandbox.ArchiveTrim{}},
		"past its bound":             {NewCodex(), codexCarryRelease, strings.Repeat(" ", trimMarkMaxBytes) + word, sandbox.ArchiveTrim{}},
		"another release":            {NewCodex(), "0.172.0", word, sandbox.ArchiveTrim{}},
		"a harness that never trims": {NewClaudeCode(), codexCarryRelease, word, sandbox.ArchiveTrim{}},
		"a chat file's start":        {NewGeminiCLI(), geminiRelease, chat("19 123 4567"), restated},
		"a chat file's start alone":  {NewQwenCode(), qwenRelease, word, trimmed},
		"a start of two fields":      {NewGeminiCLI(), geminiRelease, chat("19 123"), sandbox.ArchiveTrim{}},
		"a start of four fields":     {NewGeminiCLI(), geminiRelease, chat("19 123 4567 8"), sandbox.ArchiveTrim{}},
		"a start of no lines":        {NewGeminiCLI(), geminiRelease, chat("0 123 4567"), sandbox.ArchiveTrim{}},
		"a padded start":             {NewGeminiCLI(), geminiRelease, chat("019 123 4567"), sandbox.ArchiveTrim{}},
		"a start of fewer bytes":     {NewGeminiCLI(), geminiRelease, chat("19 123 18"), sandbox.ArchiveTrim{}},
		"a start past what was kept": {NewGeminiCLI(), geminiRelease, chat(fmt.Sprintf("19 123 %d", 40<<20+1)),
			sandbox.ArchiveTrim{}},
		"a start past the CRC":      {NewGeminiCLI(), geminiRelease, chat("19 4294967296 4567"), sandbox.ArchiveTrim{}},
		"hermes on its release":     {NewHermes(), hermesFloor(), word, trimmed},
		"hermes on another release": {NewHermes(), "0.23.4", word, sandbox.ArchiveTrim{}},
		"grok's own archive":        {NewGrok(), grokSupported(), word, trimmed},
		"grok, another release":     {NewGrok(), "1.0.93", word, sandbox.ArchiveTrim{}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := sandbox.NewFakeSandbox("box-1")
			path := PathsFor(box).SessionCapture()
			if c.word != "" {
				box.Put(trimMarkOf(path), c.word)
			}
			got := c.runner.trimOf(t.Context(), box, sandbox.SessionArchive{SessionID: codexThread,
				Version: c.release, Size: size, Path: path})
			if got != c.want {
				t.Errorf("the take read %+v, want %+v", got, c.want)
			}
		})
	}
}

// A TRIMMED CODEX TAKE IS THE TRIMMED ARCHIVE, SAID SO, AND STILL BOUNDED: the
// take describes the archive its pack trimmed as trimmed and keeps it whole,
// and an archive past the take's bound once trimmed is refused as too large,
// as one never trimmed is.
//
// Mutation: read the trim after the bound's check, or exempt a trimmed archive
// from it, and a case goes red.
func TestATrimmedCodexTakeIsSaidSoAndStillBounded(t *testing.T) {
	t.Parallel()
	whole := codexFixture(t, "compaction-note-before.jsonl")
	p := codexBox(t, whole)
	script := "sid=" + codexThread + " " + trimRelVar + "=" + codexCarryRelease + " packed=\n" +
		codexPackAt(p, p.SessionStaging(), p.SessionCapture(), "exit 3", "exit 2", trimEverything) +
		`[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	box := sandbox.NewFakeSandbox("box-1")
	paths := PathsFor(box)
	archive := codexFile(t, p.SessionCapture())
	box.Put(paths.SessionRecord(), "id "+codexThread+"\ncontinued yes\nwhy \nrelease codex-cli 0.171.0\n")
	box.Put(paths.SessionCapture(), string(archive))
	box.Put(trimMarkOf(paths.SessionCapture()), string(codexFile(t, trimMarkOf(p.SessionCapture()))))
	spool := sandbox.SessionSpool{Dir: t.TempDir(), Bytes: math.MaxInt64, Held: func(int64) {}}
	size := int64(len(archive))
	_, err := NewCodex().TakeSession(t.Context(), box, sandbox.SessionTake{Spool: spool, MaxBytes: size - 1,
		Session: codexThread})
	if !errors.Is(err, executorkeep.ErrSessionTooLarge) {
		t.Errorf("a trimmed archive a byte past the take's bound came to %v, want it refused as too large", err)
	}
	taken, err := NewCodex().TakeSession(t.Context(), box, sandbox.SessionTake{Spool: spool, MaxBytes: size,
		Session: codexThread})
	if err != nil || taken.Body == nil {
		t.Fatalf("the trimmed archive at the take's bound: %+v, %v", taken, err)
	}
	if _, err := io.Copy(io.Discard, taken.Body); err != nil {
		t.Errorf("reading the take: %v", err)
	}
	_ = taken.Body.Close()
	want := sandbox.ArchiveTrim{RecordBytes: int64(len(whole)), KeptBytes: int64(len(codexResumeLoads(t, whole)))}
	if taken.Archive.Trim != want {
		t.Errorf("the take describes its archive's trim as %+v, want %+v", taken.Archive.Trim, want)
	}
}

// THE HARNESSES NO CUT FITS ARE NO TRIMMERS: Claude Code (its own trim, and
// subagent threads a resume continues), Kimi Code (a replay that folds every
// record before its compaction) and Cursor Agent (no compacted chat carried)
// pack whole past the bound; a trim for one needs its own measurement first.
//
// Mutation: make one of them a trimmer, and the case goes red.
func TestTheHarnessesNoCutFitsAreNoTrimmers(t *testing.T) {
	t.Parallel()
	for _, h := range []Harness{ClaudeCode{}, Kimi{}, CursorAgent{}} {
		if _, ok := h.(trimmer); ok {
			t.Errorf("%s trims, though no cut of its record leaves out only what its resume skips", h.Name())
		}
	}
}
