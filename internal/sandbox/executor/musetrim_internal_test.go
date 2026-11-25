package executor

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// museTrimSession is the session [musePackRun] packs, filed under
// museTrimDay.
const (
	museTrimSession = "66666666-6666-4666-8666-666666666666"
	museTrimDay     = "2026/11/25/" + museTrimSession
)

// museTrimMember, museTrimChild and museTrimOutput are the record, a
// subagent's record and a tool's output in [musePackRun]'s archive.
const (
	museTrimMember = museTrimDay + "/session.jsonl"
	museTrimChild  = museTrimDay + "/subagent/01a21035-c000-7000-8000-000000000001/session.jsonl"
	museTrimOutput = museTrimDay + "/tool-outputs/01a21035-c000-7000-8000-000000000002/call_0001_0-bash.txt"
)

// museCompacted is testdata/muse/1.4.25/compacted-whole.session.jsonl.gz, as
// its lines: a real 1.4.25 record, every line kept, against a stand-in model.
// Its 191 lines hold installed compactions at lines 66, 99 and 132, each
// followed by its checkpoint, and a rejected candidate at 172, newer than all.
func museCompacted(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "muse", "1.4.25", "compacted-whole.session.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(raw, []byte("\n"))
	if len(lines) != 192 || len(lines[191]) != 0 {
		t.Fatalf("the fixture holds %d lines, want 191 whole ones", len(lines))
	}
	return lines[:191]
}

// museJoined is lines as one record, each part in turn.
func museJoined(parts ...[][]byte) []byte {
	var out []byte
	for _, lines := range parts {
		out = append(out, bytes.Join(lines, nil)...)
	}
	return out
}

// museEventKind is the kind of the event line, whole, records: "" for a line
// that is not a runtime session's whole event.
func museEventKind(line []byte) string {
	var l struct {
		PayloadType string `json:"payload_type"`
		Payload     struct {
			Event struct {
				Kind string `json:"kind"`
			} `json:"event"`
		} `json:"payload"`
	}
	if !bytes.HasSuffix(line, []byte("\n")) || json.Unmarshal(line, &l) != nil || l.PayloadType != "runtime.session" {
		return ""
	}
	return l.Payload.Event.Kind
}

// museResumeLoads is what a 1.4.25 resume of record loads, read as the
// measurement read it (each whole line decoded): every line from its newest
// installed compaction on, where that is past its first line and the next
// line, whole, is a checkpoint; record whole otherwise.
func museResumeLoads(t *testing.T, record []byte) []byte {
	t.Helper()
	lines := bytes.SplitAfter(record, []byte("\n"))
	newest := -1
	for i, line := range lines {
		if museEventKind(line) == "context_compaction_installed" {
			newest = i
		}
	}
	if newest < 1 || newest+1 >= len(lines) || museEventKind(lines[newest+1]) != "context_projection_checkpoint" {
		return record
	}
	return bytes.Join(lines[newest:], nil)
}

// museTrimBeside is the files [musePackRun] writes, by member name: record,
// and beside it a subagent's record, the whole compacted fixture, and a tool's
// output, neither of which a trim cuts.
func museTrimBeside(t *testing.T, record []byte) map[string][]byte {
	t.Helper()
	return map[string][]byte{museTrimMember: record, museTrimChild: museJoined(museCompacted(t)),
		museTrimOutput: []byte("total 0\n")}
}

// musePackRun packs record as session museTrimSession's in a box of its own,
// beside [museTrimBeside]'s files, at at, the session written by release,
// and returns the archive's members and the word left beside it ("" for none),
// checked against the archive's size.
func musePackRun(t *testing.T, record []byte, release string, at trimPoints) (members map[string]string,
	trim sandbox.ArchiveTrim, mark string) {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	for name, body := range museTrimBeside(t, record) {
		path := filepath.Join(p.MuseSessions(), name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "s=" + museTrimSession + " " + trimRelVar + "=" + shellword.Quote(release) + " packed=\n" +
		musePackAt(p, packAt{staged: staged, dest: dest}, at) + `[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return codexMembers(t, archive), parseTrimMark(string(word), int64(len(archive))), string(word)
}

// A TRIMMED MUSE PACK KEEPS WHAT ITS RESUME LOADS: past the point, the packed
// record is every line from its newest installed compaction on, that line and
// its checkpoint first, as the measured resume reads it, a record trimmed
// before and grown since alike; its subagents' records and tool outputs go
// beside it byte for byte, and the pack says what it kept beside the archive.
//
// Mutation: cut at the newest candidate or at the checkpoint, leave the
// installed line out, cut a subagent's record too, or leave the files beside
// the record out, and a case goes red.
func TestATrimmedMusePackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	lines := museCompacted(t)
	for name, record := range map[string][]byte{
		"a whole record":          museJoined(lines),
		"a record trimmed before": museJoined(lines[65:]),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := museResumeLoads(t, record)
			if bytes.Equal(want, record) {
				t.Fatal("the record holds no compaction to trim at")
			}
			members, trim, mark := musePackRun(t, record, museSupported(), trimEverything)
			if got := members[museTrimMember]; got != string(want) {
				t.Errorf("the pack kept %d bytes of the record, want the %d its resume loads", len(got), len(want))
			}
			for member, body := range museTrimBeside(t, record) {
				if member != museTrimMember && members[member] != string(body) {
					t.Errorf("the trimmed archive holds %d bytes of %s, want its %d whole", len(members[member]),
						member, len(body))
				}
			}
			if trim != (sandbox.ArchiveTrim{RecordBytes: int64(len(record)), KeptBytes: int64(len(want))}) {
				t.Errorf("the pack's word %q reads as %+v, want the record's bytes before and kept", mark, trim)
			}
		})
	}
}

// museNested is line, an installed compaction's whole record, carried as an
// object in another event's results under the same envelope: a line that
// spells an install, one line start and all, but records none.
func museNested(t *testing.T, line []byte) []byte {
	t.Helper()
	body := bytes.TrimSuffix(line, []byte("\n"))
	at := bytes.Index(body, []byte(`"payload":{"event":{`))
	if at < 0 {
		t.Fatalf("an installed line with no event: %.200s", body)
	}
	at += len(`"payload":{"event":{`)
	out := fmt.Appendf(nil, "%s\"kind\":\"tool_result_batch_committed\",\"results\":[%s]}}}\n", body[:at], body)
	if !json.Valid(out) || museEventKind(out) != "tool_result_batch_committed" {
		t.Fatalf("the nested line is not one result's record: %.200s", out)
	}
	return out
}

// A MUSE PACK WITH NOTHING TO TRIM AT IS PACKED WHOLE: a record with no
// installed compaction, one whose newest install has no checkpoint after it
// (left out, killed before it, torn, or every checkpoint gone), one whose
// newest install is its first line, and one whose results carry installed
// records as objects keep every whole line, and no word says otherwise. A
// torn last line is one the harness is still writing ([trimTar]).
//
// Mutation: cut where no checkpoint follows, cut at the first line, or match
// an install anywhere in a line, and a case goes red.
func TestAMusePackWithNothingToTrimAtIsPackedWhole(t *testing.T) {
	t.Parallel()
	lines := museCompacted(t)
	checkpoint := lines[132]
	var nested [][]byte
	for _, line := range lines {
		if museEventKind(line) == "context_compaction_installed" {
			line = museNested(t, line)
		}
		nested = append(nested, line)
	}
	for name, record := range map[string][]byte{
		"no installed compaction":      museJoined(lines[:65]),
		"its checkpoint left out":      museJoined(lines[:132], lines[133:]),
		"killed before its checkpoint": museJoined(lines[:132]),
		"its checkpoint torn":          museJoined(lines[:132], [][]byte{checkpoint[:len(checkpoint)-100]}),
		"every checkpoint gone":        museFixture(t, "compacted.session.jsonl"),
		"an install on its first line": museJoined(lines[131:]),
		"results naming installs":      museJoined(nested),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !bytes.Equal(museResumeLoads(t, record), record) {
				t.Fatal("the record holds a compaction the resume loads from")
			}
			want := record[:bytes.LastIndexByte(record, '\n')+1]
			members, _, mark := musePackRun(t, record, museSupported(), trimEverything)
			if got := members[museTrimMember]; got != string(want) || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want its %d in whole lines, unsaid", len(got),
					len(record), mark, len(want))
			}
		})
	}
}

// A MUSE CUT NEEDS ITS CHECKPOINT WHOLE: the cut alone, handed a record
// whose newest install's checkpoint is its last line and torn, writes nothing
// and fails; handed it whole, it writes from the install on. A pack hands it
// whole lines, so this is the cut's own guard.
//
// Mutation: drop the check that a line break follows the checkpoint, and the
// torn case goes red.
func TestAMuseCutNeedsItsCheckpointWhole(t *testing.T) {
	t.Parallel()
	lines := museCompacted(t)
	checkpoint := lines[132]
	for name, c := range map[string]struct {
		record []byte
		want   string
		code   int
	}{
		"torn":  {museJoined(lines[:132], [][]byte{checkpoint[:len(checkpoint)-100]}), "", 1},
		"whole": {museJoined(lines[:133]), string(museJoined(lines[131:133])), 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			src, dst := filepath.Join(dir, "session.jsonl"), filepath.Join(dir, "cut")
			if err := os.WriteFile(src, c.record, 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, code := codexShell(t, dir, Muse{}.trimRecord(shellword.Quote(src), shellword.Quote(dst)))
			got, err := os.ReadFile(dst)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			if (code == 0) != (c.code == 0) || string(got) != c.want {
				t.Errorf("the cut exited %d and wrote %d bytes, want status %d and %d bytes", code, len(got), c.code,
					len(c.want))
			}
		})
	}
}

// ONLY THE MEASURED MUSE CODE RELEASE IS TRIMMED: a session another release
// wrote, or one whose writer the pack was not told, is packed whole past the
// point.
//
// Mutation: drop the release's check, or trim on a release that reads as
// 1.4.25's prefix, and a case goes red.
func TestOnlyTheMeasuredMuseReleaseIsTrimmed(t *testing.T) {
	t.Parallel()
	record := museJoined(museCompacted(t))
	for _, release := range []string{"", "1.4.24", "1.4.26", museSupported() + "1"} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			members, _, mark := musePackRun(t, record, release, trimEverything)
			if got := members[museTrimMember]; got != string(record) || mark != "" {
				t.Errorf("a session %q wrote was packed as %d of %d bytes, saying %q; want it whole", release,
					len(got), len(record), mark)
			}
		})
	}
}

// EVERY MUSE PACK TRIMS AT THE TAKE'S BOUNDS, BY THE RELEASE THAT WROTE ITS
// SESSION: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt], the run's end naming the box's bare release, read as
// the engine reads a record's.
//
// Mutation: pack one path at other points or untrimmed, or leave the run's
// end's release unset, and a case goes red.
func TestEveryMusePackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	save, err := Muse{}.PackSave(p, sessionRecord{id: museTrimSession}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ script, want string }{
		"end": {museScript(sandbox.RunRequest{}, Session{ID: museTrimSession}, p, sandbox.JobStopGrace),
			bareReleaseShell(trimRelVar) + musePackAt(p, packAt{staged: p.SessionStaging(), dest: p.SessionCapture()},
				trimAt)},
		"stop": {Muse{}.Pack(p, museTrimSession, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
			musePackAt(p, packAt{staged: p.SessionPackStaging("1"), dest: p.SessionPackHeld("1"), working: true},
				trimAt)},
		"save": {save, musePackAt(p, packAt{staged: p.SessionSaveStaging("t", "1"), dest: p.SessionSave("t", "1"),
			working: true}, trimAt)},
	} {
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, fmt.Sprint(trimPlainPoint)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	for printed, want := range map[string]string{"Muse Code 1.4.25 (1.4.25-R9361.1)": "1.4.25",
		"Muse Code 1.4.25-rc.1 (1.4.25-rc.1-R9000.1)": "", "": ""} {
		out, _, code := codexShell(t, t.TempDir(), "v="+shellword.Quote(printed)+"\n"+bareReleaseShell(trimRelVar)+
			`printf '%s' "$`+trimRelVar+`"`)
		if code != 0 || out != want || releaseOf(printed) != want {
			t.Errorf("%q: the run's end trims by %q (exit %d), want %q as the engine reads it", printed, out, code,
				want)
		}
	}
}

// A TRIMMED MUSE SESSION READS FROM ITS NEWEST SUMMARY ON: Sessions and
// read_session, reading the record the trim keeps, open on the newest
// compaction's summary and read every entry after it as the whole record's
// read does.
//
// Mutation: read no summary off an installed line, or read the checkpoint's
// replacement as one too, and the case goes red.
func TestATrimmedMuseSessionReadsFromItsNewestSummaryOn(t *testing.T) {
	t.Parallel()
	whole := museJoined(museCompacted(t))
	read := func(record []byte) []TranscriptEntry {
		return museTranscriptOf(t, museArchive(t, map[string][]byte{museTrimMember: record}))
	}
	all, kept := read(whole), read(museResumeLoads(t, whole))
	newest := -1
	for i, e := range all {
		if e.Kind == EntrySummary {
			newest = i
		}
	}
	if newest < 1 {
		t.Fatalf("the whole record reads %d entries with no summary past its first", len(all))
	}
	if !slices.Equal(kept, all[newest:]) {
		t.Errorf("the trimmed record reads %d entries, want the %d from the whole one's newest summary on",
			len(kept), len(all)-newest)
	}
}
