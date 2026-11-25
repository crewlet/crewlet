package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// piTrimSession is the session [piPackRun] packs, and piTrimMember its file's
// member in the archive.
const (
	piTrimSession = "66666666-6666-4666-8666-666666666666"
	piTrimMember  = ".pi/sessions/" + piTrimSession + ".jsonl"
)

// piTrimEntry is what [piResumeLoads] reads of a session line: its own
// members, decoded.
type piTrimEntry struct {
	Type       string `json:"type"`
	ID         string `json:"id"`
	CustomType string `json:"customType"`
	FirstKept  string `json:"firstKeptEntryId"`
	Message    struct {
		Role string `json:"role"`
	} `json:"message"`
}

// piReadOffTheBranch reports whether a line before the firstKept one is one
// the measurement found a 1.6.0 resume reads there (a model or thinking level
// change, a system message) or its code reads (a router model's state),
// written out apart from [piTrimAwk], which the pack cuts by.
func piReadOffTheBranch(e piTrimEntry) bool {
	switch e.Type {
	case "model_change", "thinking_level_change":
		return true
	case "message":
		return e.Message.Role == "system"
	case "custom":
		return e.CustomType == "pi.virtual-model-state"
	}
	return false
}

// piResumeLoads is what a 1.6.0 resume of record loads, read as the
// measurement read it (each line decoded): its header; before the newest
// compaction's firstKept line, each line [piReadOffTheBranch] in order, its
// own parentId set onto the one kept before it (the first onto null); the
// firstKept line set onto the last of them; and every line after it as
// written. record whole where there is no such line, or nothing to leave out.
func piResumeLoads(t *testing.T, record []byte) []byte {
	t.Helper()
	lines := bytes.SplitAfter(record, []byte("\n"))
	lines = lines[:len(lines)-1]
	entries := make([]piTrimEntry, len(lines))
	compaction := -1
	for i, line := range lines {
		if err := json.Unmarshal(line, &entries[i]); err != nil {
			t.Fatalf("line %d does not decode: %v", i+1, err)
		}
		if entries[i].Type == "compaction" {
			compaction = i
		}
	}
	if compaction < 0 {
		return record
	}
	kept := slices.IndexFunc(entries[1:], func(e piTrimEntry) bool { return e.ID == entries[compaction].FirstKept }) + 1
	if kept < 1 || kept > compaction {
		return record
	}
	out, parent, left := slices.Clone(lines[0]), "null", 0
	for i := 1; i < kept; i++ {
		if !piReadOffTheBranch(entries[i]) {
			left++
			continue
		}
		out = append(out, piRelinked(t, lines[i], parent)...)
		id, err := json.Marshal(entries[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		parent = string(id)
	}
	if left == 0 {
		return record
	}
	out = append(out, piRelinked(t, lines[kept], parent)...)
	return append(out, bytes.Join(lines[kept+1:], nil)...)
}

// piRelinked is line with its own parentId, the member a decoder finds at its
// top level, set to parent, a JSON value.
func piRelinked(t *testing.T, line []byte, parent string) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		t.Fatalf("line %.80q is no object", line)
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			t.Fatal(err)
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			t.Fatal(err)
		}
		if key == "parentId" {
			end := int(dec.InputOffset())
			return slices.Concat(line[:end-len(value)], []byte(parent), line[end:])
		}
	}
	t.Fatalf("line %.80q holds no parentId", line)
	return nil
}

// piFixtureEdit is record with old replaced by repl in its one line that
// starts with line, failing the test where either is not there exactly once.
func piFixtureEdit(t *testing.T, record []byte, line, old, repl string) []byte {
	t.Helper()
	lines := bytes.SplitAfter(record, []byte("\n"))
	at := -1
	for i, l := range lines {
		if bytes.HasPrefix(l, []byte(line)) {
			if at >= 0 {
				t.Fatalf("two lines start with %.80q", line)
			}
			at = i
		}
	}
	if at < 0 || bytes.Count(lines[at], []byte(old)) != 1 {
		t.Fatalf("no one line starting with %.80q holds %.80q once", line, old)
	}
	lines[at] = bytes.Replace(lines[at], []byte(old), []byte(repl), 1)
	return bytes.Join(lines, nil)
}

// piFixtureLines is the fixture name's lines numbered, from 1, in order.
func piFixtureLines(t *testing.T, name string, numbers ...int) []byte {
	t.Helper()
	lines := bytes.SplitAfter(piFixture(t, name), []byte("\n"))
	var out []byte
	for _, n := range numbers {
		out = append(out, lines[n-1]...)
	}
	return out
}

// piPackRun packs record as session piTrimSession's file in a box of its own
// at at, the session written by release, and returns the file's member, the
// archive's size and the word left beside it ("" for none).
func piPackRun(t *testing.T, record []byte, release string, at trimPoints) (member []byte, size int64,
	mark string) {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	if err := os.MkdirAll(p.PiSessions(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PiSessionFile(piTrimSession), record, 0o600); err != nil {
		t.Fatal(err)
	}
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "s=" + piTrimSession + " " + trimRelVar + "=" + shellword.Quote(release) + " packed=\n" +
		piPackAt(p, packAt{staged: staged, dest: dest}, at) + `[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	members := codexMembers(t, archive)
	if len(members) != 1 {
		t.Fatalf("the archive holds %d members, want the session's file alone", len(members))
	}
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return []byte(members[piTrimMember]), int64(len(archive)), string(word)
}

// piMeasuredCuts are the bytes the measured cuts of the recorded sessions
// kept, whose resumes sent the request the whole ones did
// (docs/concepts/code-sandbox.md, Pi).
var piMeasuredCuts = map[string]int{"compacted-twice.session.jsonl": 23535, "compacted-custom.session.jsonl": 31745}

// A TRIMMED PI PACK KEEPS WHAT ITS RESUME LOADS: past the point, the packed
// file is its header, before the newest compaction's firstKept line only the
// lines a 1.6.0 resume reads off the branch there, each re-linked onto the one
// kept before it, the firstKept line re-linked onto the last of them, and
// every line after it as written, as the measured resume reads them; on the
// recorded sessions it is the measured cut, byte for byte, and the pack says
// what it kept beside the archive. A payload spelling a line's own members
// (an id, a parentId, a firstKeptEntryId) moves nothing.
//
// Mutation: keep a line of another kind or drop one read off the branch, leave
// a kept line's link or the firstKept line's as it was, cut at the oldest
// compaction, read an id, a link or a firstKept inside a payload, or match a
// custom entry's own members short of its end, and a case goes red.
func TestATrimmedPiPackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	twice, custom := piFixture(t, "compacted-twice.session.jsonl"), piFixture(t, "compacted-custom.session.jsonl")
	decoys := custom
	for _, e := range []struct{ line, old, repl string }{
		{`{"type":"custom","customType":"cp5b.armed","data":{},"id":"192de295"`, `"customType":"cp5b.armed","data":{}`,
			`"customType":"pi.virtual-model-state","data":{"model":"stub/stub-model"}`},
		{`{"type":"message","id":"457906c7"`, `"message":{"role":"system",`,
			`"message":{"role":"system","parentId":"0badc0de",`},
		{`{"type":"custom","customType":"cp5b.context","data":{"tokens":1,"contextWindow":200000},"id":"e0da6d78"`,
			`"contextWindow":200000}`, `"id":"48dd5b15","parentId":null,"timestamp":"t"}`},
		{`{"type":"custom","customType":"cp5b.context","data":{"tokens":1,"contextWindow":200000},"id":"48dd5b15"`,
			`"contextWindow":200000}`, `"id":"ffffffff","parentId":"eeeeeeee","timestamp":"t"}`},
		{`{"type":"compaction","id":"17433c98"`, `"details":{`, `"details":{"firstKeptEntryId":"e6578f04",`},
	} {
		decoys = piFixtureEdit(t, decoys, e.line, e.old, e.repl)
	}
	alone := piFixtureEdit(t, piFixtureLines(t, "compacted-twice.session.jsonl", 1, 5, 6, 13, 14),
		`{"type":"compaction","id":"971d3a16"`, `"firstKeptEntryId":"66bd5c9b"`, `"firstKeptEntryId":"c4c6fcf4"`)
	for name, record := range map[string][]byte{
		"compacted.session.jsonl":        piFixture(t, "compacted.session.jsonl"),
		"compacted-twice.session.jsonl":  twice,
		"compacted-custom.session.jsonl": custom,
		"payloads spelling members":      decoys,
		"a firstKept custom message": piFixtureEdit(t, custom, `{"type":"compaction","id":"17433c98"`,
			`"firstKeptEntryId":"48dd5b15"`, `"firstKeptEntryId":"62b23581"`),
		"a firstKept that is its compaction": piFixtureEdit(t, twice, `{"type":"compaction","id":"c6ea41c9"`,
			`"firstKeptEntryId":"59dd48fa"`, `"firstKeptEntryId":"c6ea41c9"`),
		"nothing kept before the firstKept": alone,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := piResumeLoads(t, record)
			if bytes.Equal(want, record) {
				t.Fatal("the record holds no compaction to trim at")
			}
			if measured, ok := piMeasuredCuts[name]; ok && len(want) != measured {
				t.Fatalf("the resume's load is %d bytes, want the %d the measured cut kept", len(want), measured)
			}
			member, size, mark := piPackRun(t, record, piSupported, trimEverything)
			if !bytes.Equal(member, want) {
				t.Errorf("the pack kept %d bytes of the file, want the %d its resume loads:\n%s", len(member),
					len(want), firstApart(string(member), string(want)))
			}
			if got := parseTrimMark(mark, size); got != (sandbox.ArchiveTrim{RecordBytes: int64(len(record)),
				KeptBytes: int64(len(want))}) {
				t.Errorf("the pack's word %q reads as %+v, want the file's bytes before and kept", mark, got)
			}
		})
	}
}

// A PI PACK WITH NOTHING TO TRIM AT IS PACKED WHOLE: a file with no
// compaction, one whose newest compaction's firstKept line is not there or
// comes after it, one holding compactions only inside a payload, one with a
// line before the firstKept one of a shape 1.6.0 does not write (members out
// of order, a custom entry its own members do not end), one whose header is
// no v3 session's, and one with nothing before the firstKept line to leave out
// keep every line, and no word says otherwise.
//
// Mutation: cut past a firstKept not found or after its compaction, match a
// compaction anywhere in a line, cut past a line of unknown shape, skip the
// header's check, or cut where nothing is left out, and a case goes red.
func TestAPiPackWithNothingToTrimAtIsPackedWhole(t *testing.T) {
	t.Parallel()
	twice := piFixture(t, "compacted-twice.session.jsonl")
	newest := `{"type":"compaction","id":"c6ea41c9"`
	var nested []byte
	for i, line := range bytes.SplitAfter(twice, []byte("\n")) {
		if bytes.HasPrefix(line, []byte(`{"type":"compaction",`)) {
			line = fmt.Appendf(nil, `{"type":"message","id":"%08x","parentId":null,"timestamp":"t",`+
				`"message":{"role":"toolResult","details":%s}}`+"\n", i, bytes.TrimSuffix(line, []byte("\n")))
		}
		nested = append(nested, line...)
	}
	for name, record := range map[string][]byte{
		"no compaction": piFixture(t, "tool.session.jsonl"),
		"a firstKept not found": piFixtureEdit(t, twice, newest, `"firstKeptEntryId":"59dd48fa"`,
			`"firstKeptEntryId":"deadbeef"`),
		"a firstKept after its compaction": piFixtureEdit(t, twice, newest, `"firstKeptEntryId":"59dd48fa"`,
			`"firstKeptEntryId":"6e891265"`),
		"compactions inside a payload": nested,
		"members out of order": piFixtureEdit(t, twice, `{"type":"message","id":"c4c6fcf4",`,
			`{"type":"message","id":"c4c6fcf4",`, `{"id":"c4c6fcf4","type":"message",`),
		"a custom entry its own members do not end": piFixtureEdit(t,
			piFixture(t, "compacted-custom.session.jsonl"), `{"type":"custom","customType":"cp5b.armed","data":{},"id":"192de295"`,
			`"timestamp":"2026-11-24T23:59:29.402Z"}`, `"timestamp":"2026-11-24T23:59:29.402Z","more":1}`),
		"a header of another version": piFixtureEdit(t, twice, `{"type":"session",`, `"version":3,`,
			`"version":2,`),
		"nothing to leave out": piFixtureEdit(t, piFixtureLines(t, "compacted-twice.session.jsonl", 1, 2, 3, 4, 5, 13),
			`{"type":"compaction","id":"971d3a16"`, `"firstKeptEntryId":"66bd5c9b"`, `"firstKeptEntryId":"3795d494"`),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			member, _, mark := piPackRun(t, record, piSupported, trimEverything)
			if !bytes.Equal(member, record) || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(member),
					len(record), mark)
			}
		})
	}
}

// ONLY THE MEASURED PI RELEASE IS TRIMMED: a session another release wrote,
// or one whose writer the pack was not told, is packed whole past the point.
//
// Mutation: drop the release's check, or trim on a release that reads as
// 1.6.0's prefix, and a case goes red.
func TestOnlyTheMeasuredPiReleaseIsTrimmed(t *testing.T) {
	t.Parallel()
	record := piFixture(t, "compacted-twice.session.jsonl")
	for _, release := range []string{"", "1.5.9", "1.6.1", piSupported + "1"} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			member, _, mark := piPackRun(t, record, release, trimEverything)
			if !bytes.Equal(member, record) || mark != "" {
				t.Errorf("a session %q wrote was packed as %d of %d bytes, saying %q; want it whole", release,
					len(member), len(record), mark)
			}
		})
	}
}

// EVERY PI PACK TRIMS AT THE TAKE'S BOUNDS, BY THE RELEASE THAT WROTE ITS
// SESSION: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt]; the run's end names the box's bare release, read as
// the engine reads a record's, and every pack made on demand the release its
// record names.
//
// Mutation: pack one path at other points or untrimmed, or leave a path's
// release unset, and a case goes red.
func TestEveryPiPackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	save, err := Pi{}.PackSave(p, sessionRecord{id: piTrimSession}, p.SessionSaveStaging("t", "1"),
		p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ script, want string }{
		"end": {piCapture(p), trimRelVar + "=$n\n" +
			piPackAt(p, packAt{staged: p.SessionStaging(), dest: p.SessionCapture()}, trimAt)},
		"stop": {Pi{}.Pack(p, piTrimSession, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
			piPackAt(p, packAt{staged: p.SessionPackStaging("1"), dest: p.SessionPackHeld("1"), working: true},
				trimAt)},
		"save": {save, piPackAt(p, packAt{staged: p.SessionSaveStaging("t", "1"), dest: p.SessionSave("t", "1"),
			working: true}, trimAt)},
	} {
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, fmt.Sprint(trimPlainPoint)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	for printed, want := range map[string]string{piSupported: piSupported, piSupported + "-beta.1": "", "": ""} {
		out, _, code := codexShell(t, t.TempDir(), "v="+shellword.Quote(printed)+"\n"+piReleaseParse()+
			piCapture(Paths{Home: t.TempDir()})+`printf '%s' "$`+trimRelVar+`"`)
		if code != 0 || out != want || releaseOf(printed) != want {
			t.Errorf("%q: the run's end trims by %q (exit %d), want %q as the engine reads it", printed, out, code,
				want)
		}
	}
	box := sandbox.NewFakeSandbox("box-1")
	box.Put(PathsFor(box).SessionRecord(), "id "+piTrimSession+"\ncontinued yes\nwhy \nrelease "+piSupported+"\n")
	box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: packNothing}, nil
	}
	if _, err := NewPi().PackSave(t.Context(), box, "node:1", piTrimSession); err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(trimRelVar+"="+shellword.Quote(piSupported)+"\n", "'", `'\''`)
	if execs := box.Execs(); len(execs) == 0 || !strings.Contains(execs[len(execs)-1].Command, want) {
		t.Errorf("a save names no release off its record to trim by: %+v", execs)
	}
}
