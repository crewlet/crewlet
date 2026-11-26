package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// ocTrimFixture is the trim fixture name (testdata/opencode/2.3.2/trim): an
// export the measurement grew on 2.3.2, or the cut of it whose resume it
// measured (`.cut.json`).
func ocTrimFixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode", "2.3.2", "trim", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// ocExport is an export as its document: `info`, and each message raw.
type ocExport struct {
	Info     json.RawMessage   `json:"info"`
	Messages []json.RawMessage `json:"messages"`
}

// ocMessage is what of a message the measured rule reads.
type ocMessage struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Metadata struct {
		KeepID string `json:"keepID"`
	} `json:"metadata"`
	Native          json.RawMessage `json:"native"`
	ProviderContext json.RawMessage `json:"providerContext"`
}

// ocDecode is export decoded, its messages read by the rule.
func ocDecode(t *testing.T, export []byte) (ocExport, []ocMessage) {
	t.Helper()
	var doc ocExport
	if err := json.Unmarshal(export, &doc); err != nil {
		t.Fatalf("the export does not decode: %v", err)
	}
	ms := make([]ocMessage, len(doc.Messages))
	for i, raw := range doc.Messages {
		if err := json.Unmarshal(raw, &ms[i]); err != nil {
			t.Fatalf("message %d does not decode: %v", i, err)
		}
	}
	return doc, ms
}

// ocWrite is doc as 2.3.2 writes an export: `JSON.stringify(doc, null, 2)`
// and a line break.
func ocWrite(t *testing.T, doc ocExport) []byte {
	t.Helper()
	var compact bytes.Buffer
	compact.WriteString(`{"info":`)
	if err := json.Compact(&compact, doc.Info); err != nil {
		t.Fatal(err)
	}
	compact.WriteString(`,"messages":[`)
	for i, m := range doc.Messages {
		if i > 0 {
			compact.WriteByte(',')
		}
		if err := json.Compact(&compact, m); err != nil {
			t.Fatal(err)
		}
	}
	compact.WriteString(`]}`)
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		t.Fatal(err)
	}
	out.WriteByte('\n')
	return out.Bytes()
}

// openCodeResumeLoads is what a 2.3.2 resume of export loads, and of it what
// a reading of the calls of a run that began after start reads, decoded:
// `info`, and every message from the keepID message of the newest compaction
// that completed and is no provider's own (from that compaction where it
// names none), or from start where that is older; export whole where the
// keepID names no older message or nothing would be left out.
func openCodeResumeLoads(t *testing.T, export []byte, start string) []byte {
	t.Helper()
	doc, ms := ocDecode(t, export)
	from := -1
	for i, m := range ms {
		if m.Type == "compaction" && m.Status == "completed" && m.Native == nil && m.ProviderContext == nil {
			from = i
		}
	}
	if from >= 0 && ms[from].Metadata.KeepID != "" {
		keep := ms[from].Metadata.KeepID
		from = slices.IndexFunc(ms[:from], func(m ocMessage) bool { return m.ID == keep })
	}
	if at := slices.IndexFunc(ms, func(m ocMessage) bool { return m.ID == start }); from > 0 && at >= 0 {
		from = min(from, at)
	}
	if from <= 0 {
		return export
	}
	return ocWrite(t, ocExport{Info: doc.Info, Messages: doc.Messages[from:]})
}

// ocVariant is export with each message compacted and handed to edit with
// its index, written as 2.3.2 writes one; a message edit returns "" for is
// left out.
func ocVariant(t *testing.T, export []byte, edit func(i int, m string) string) []byte {
	t.Helper()
	doc, _ := ocDecode(t, export)
	var kept []json.RawMessage
	for i, raw := range doc.Messages {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			t.Fatal(err)
		}
		if m := edit(i, b.String()); m != "" {
			kept = append(kept, json.RawMessage(m))
		}
	}
	return ocWrite(t, ocExport{Info: doc.Info, Messages: kept})
}

// ocCompactions is the index of every compaction in export, in order.
func ocCompactions(t *testing.T, export []byte) []int {
	t.Helper()
	_, ms := ocDecode(t, export)
	var at []int
	for i, m := range ms {
		if m.Type == "compaction" {
			at = append(at, i)
		}
	}
	return at
}

// ocStartCursor is the cursor a run's script records as its start: the
// `next` of the message list asked newest first, at message id.
func ocStartCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"id":"` + id + `","order":"desc","direction":"next"}`))
}

// openCodePackRun packs export in a box of its own at at, through a stand-in
// OpenCode that prints it, the session written by release and its run's start
// start ([trimStartVar]: a cursor, or 0 for a run that began it), and returns
// the export the archive holds, the archive's size and the word the pack left
// beside it ("" for none).
func openCodePackRun(t *testing.T, export []byte, release, start string, at trimPoints) (kept []byte, size int64,
	mark string) {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	bin := filepath.Join(p.Home, "stand-in")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	printed := filepath.Join(bin, "export.json")
	if err := os.WriteFile(printed, export, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte("#!/bin/sh\ncat "+shellword.Quote(printed)+"\n"),
		0o700); err != nil {
		t.Fatal(err)
	}
	q := shellword.Quote
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "PATH=" + q(bin) + `:"$PATH"` + "\nsid=ses_trim " + trimRelVar + "=" + q(release) + " " +
		trimStartVar + "=" + q(start) + " packed=\n" + openCodePackAt(p, staged, dest, "--standalone", "", true, at) +
		`[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	if kept, err = io.ReadAll(zr); err != nil {
		t.Fatal(err)
	}
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return kept, int64(len(archive)), string(word)
}

// ocNative marks a compaction its provider's own, as a model's native
// compaction writes one.
func ocNative(field string) func(int, string) string {
	return func(_ int, m string) string {
		return strings.Replace(m, `{"type":"compaction",`, `{"type":"compaction","`+field+`":{"messages":[]},`, 1)
	}
}

// A TRIMMED OPENCODE PACK KEEPS WHAT ITS RESUME LOADS: past the point, the
// packed export is `info` and every message from the newest completed
// compaction's keep point on (from the compaction where it names none, never
// at a provider's own), as the measured resume reads them, decoded; on the
// measured sessions exactly the cut whose resume was measured; and the pack
// says what it kept beside the archive.
//
// Mutation: drop `info`, cut at the compaction rather than its keep point, at
// the oldest compaction, or at a provider's own, and a case goes red.
func TestATrimmedOpenCodePackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	twice, turns := ocTrimFixture(t, "compacted-twice.json"), ocTrimFixture(t, "kept-turns.json")
	unkept := regexp.MustCompile(`"metadata":\{"keepID":"[^"]*"\},`)
	newest := ocCompactions(t, twice)
	for name, c := range map[string]struct {
		whole    []byte
		measured string
	}{
		"two compactions, a turn after the newest": {twice, "compacted-twice.cut.json"},
		"the keep point turns back":                {turns, "kept-turns.cut.json"},
		"a compaction naming no keep point": {whole: ocVariant(t, turns, func(_ int, m string) string {
			return unkept.ReplaceAllString(m, "")
		})},
		"a newer compaction a provider's own": {whole: ocVariant(t, twice, func(i int, m string) string {
			if i == newest[len(newest)-1] {
				return ocNative("native")(i, m)
			}
			return m
		})},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := openCodeResumeLoads(t, c.whole, "")
			if bytes.Equal(want, c.whole) {
				t.Fatal("the export holds no compaction to trim at")
			}
			if c.measured != "" {
				if doc, _ := ocDecode(t, c.whole); !bytes.Equal(ocWrite(t, doc), c.whole) {
					t.Fatal("the export is not written as JSON.stringify writes one, so its rule reads nothing")
				}
				if !bytes.Equal(want, ocTrimFixture(t, c.measured)) {
					t.Fatalf("the rule keeps %d bytes, not the %d whose resume was measured", len(want),
						len(ocTrimFixture(t, c.measured)))
				}
			}
			kept, size, mark := openCodePackRun(t, c.whole, openCodeRelease, "0", trimEverything)
			if !bytes.Equal(kept, want) {
				t.Errorf("the pack kept %d bytes of the export, want the %d its resume loads", len(kept), len(want))
			}
			if got := parseTrimMark(mark, size); got != (sandbox.ArchiveTrim{RecordBytes: int64(len(c.whole)),
				KeptBytes: int64(len(want))}) {
				t.Errorf("the pack's word %q reads as %+v, want the export's bytes before and kept", mark, got)
			}
		})
	}
}

// AN OPENCODE PACK WITH NOTHING TO TRIM IS PACKED WHOLE: an export with no
// compaction, with only a provider's own or a running one, whose newest
// compaction's keepID names no older message, whose messages hold a
// compaction as a string and an object, or one not written as 2.3.2 writes an
// export, keeps every byte, and no word says otherwise.
//
// Mutation: count a running compaction or a provider's own, cut at a keepID
// naming no older message, match a field at any depth, or read an export
// that does not open as 2.3.2's, and a case goes red.
func TestAnOpenCodePackWithNothingToTrimIsPackedWhole(t *testing.T) {
	t.Parallel()
	twice := ocTrimFixture(t, "compacted-twice.json")
	doc, ms := ocDecode(t, twice)
	compactions := ocCompactions(t, twice)
	onCompactions := func(edit func(string) string) []byte {
		return ocVariant(t, twice, func(i int, m string) string {
			if slices.Contains(compactions, i) {
				return edit(m)
			}
			return m
		})
	}
	keepID := regexp.MustCompile(`"keepID":"[^"]*"`)
	infoLast, err := json.Marshal(struct {
		Messages []json.RawMessage `json:"messages"`
		Info     json.RawMessage   `json:"info"`
	}{doc.Messages, doc.Info})
	if err != nil {
		t.Fatal(err)
	}
	var reordered bytes.Buffer
	if err := json.Indent(&reordered, infoLast, "", "  "); err != nil {
		t.Fatal(err)
	}
	reordered.WriteByte('\n')
	for name, c := range map[string]struct {
		export []byte
		ruled  bool
	}{
		"no compaction": {export: ocVariant(t, twice, func(i int, m string) string {
			if slices.Contains(compactions, i) {
				return ""
			}
			return m
		}), ruled: true},
		"only a provider's own": {export: onCompactions(func(m string) string {
			return ocNative("native")(0, m)
		}), ruled: true},
		"only a provider context": {export: onCompactions(func(m string) string {
			return ocNative("providerContext")(0, m)
		}), ruled: true},
		"only running ones": {export: onCompactions(func(m string) string {
			return strings.Replace(m, `"status":"completed"`, `"status":"running"`, 1)
		}), ruled: true},
		"a keepID naming no message": {export: onCompactions(func(m string) string {
			return keepID.ReplaceAllString(m, `"keepID":"msg_nowhere"`)
		}), ruled: true},
		"a keepID naming a later message": {export: onCompactions(func(m string) string {
			return keepID.ReplaceAllString(m, `"keepID":"`+ms[len(ms)-1].ID+`"`)
		}), ruled: true},
		"a compaction in a person's message": {export: ocVariant(t, twice, func(i int, m string) string {
			if !slices.Contains(compactions, i) {
				return m
			}
			text, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			return fmt.Sprintf(`{"id":"msg_payload%d","time":{"created":1},"text":%s,"files":[],"type":"user","data":%s}`,
				i, text, m)
		}), ruled: true},
		"its messages before its info": {export: reordered.Bytes()},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if c.ruled && !bytes.Equal(openCodeResumeLoads(t, c.export, ""), c.export) {
				t.Fatal("the rule finds a compaction to trim at in the case")
			}
			kept, _, mark := openCodePackRun(t, c.export, openCodeRelease, "0", trimEverything)
			if !bytes.Equal(kept, c.export) || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(kept),
					len(c.export), mark)
			}
		})
	}
}

// ONLY THE MEASURED OPENCODE RELEASE IS TRIMMED: a session another release
// wrote, or one whose writer the pack was not told, is packed whole past the
// point.
//
// Mutation: drop the release's check, or trim on a release that reads as
// 2.3.2's prefix, and a case goes red.
func TestOnlyTheMeasuredOpenCodeReleaseIsTrimmed(t *testing.T) {
	t.Parallel()
	export := ocTrimFixture(t, "kept-turns.json")
	for _, release := range []string{"", "2.3.1", "2.3.3", "2.4.0", openCodeRelease + "1"} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			kept, _, mark := openCodePackRun(t, export, release, "0", trimEverything)
			if !bytes.Equal(kept, export) || mark != "" {
				t.Errorf("a session %q wrote was packed as %d of %d bytes, saying %q; want it whole", release,
					len(kept), len(export), mark)
			}
		})
	}
}

// AN OPENCODE TRIM KEEPS THE RUN'S START: where the run that packs began after
// a message older than the keep point, the export is kept from it, so a
// reading of the run's calls reads it as the whole, where a cut at the keep
// point is no reading. A start in the kept turns, or a run that began its
// session (its reading counts no call before the newest compaction either
// way), leaves the cut at the keep point; a start not known, not read or not
// in the export, or at the first message, cuts nothing. Every cursor
// [runStartOf] reads names the message the trim keeps from.
//
// Mutation: cut at the keep point whatever the start, where it is not known
// or not in the export, read the cursor undecoded, or read 0 as a cursor, and
// a case goes red.
func TestAnOpenCodeTrimKeepsTheRunsStart(t *testing.T) {
	t.Parallel()
	whole := ocTrimFixture(t, "kept-turns.json")
	doc, ms := ocDecode(t, whole)
	var info struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(doc.Info, &info); err != nil {
		t.Fatal(err)
	}
	atKeep := openCodeResumeLoads(t, whole, "")
	_, kept := ocDecode(t, atKeep)
	keep := slices.IndexFunc(ms, func(m ocMessage) bool { return m.ID == kept[0].ID })
	if keep < 3 {
		t.Fatalf("the fixture's keep point is message %d, too early to begin a run before it", keep)
	}
	early, inKept := ms[2].ID, ms[keep+1].ID
	enc := func(object string) string { return base64.RawURLEncoding.EncodeToString([]byte(object)) }
	reordered := enc(`{"order":"desc","direction":"next","id":"` + early + `"}`)
	spaced := enc(`{ "id" : "` + early + `", "order" : "desc", "direction" : "next" }`)
	for name, c := range map[string]struct {
		start, take, first string
		whole              bool
	}{
		"begun before the keep point":    {start: ocStartCursor(early), take: early, first: early},
		"its keys in another order":      {start: reordered, take: early, first: early},
		"its keys spaced":                {start: spaced, take: early, first: early},
		"begun in the kept turns":        {start: ocStartCursor(inKept), take: inKept, first: ms[keep].ID},
		"a run that began its session":   {start: "0", first: ms[keep].ID},
		"begun after a message it lacks": {start: ocStartCursor("msg_gone"), take: "msg_gone", whole: true},
		"a cursor that does not read":    {start: "not a cursor", whole: true},
		"no start known":                 {whole: true},
		"begun after its first message":  {start: ocStartCursor(ms[0].ID), take: ms[0].ID, whole: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := runStartOf(c.start); got != c.take {
				t.Fatalf("the take reads the start as %q, want %q", got, c.take)
			}
			got, _, mark := openCodePackRun(t, whole, openCodeRelease, c.start, trimEverything)
			if c.whole {
				if !bytes.Equal(got, whole) || mark != "" {
					t.Errorf("the pack kept %d of %d bytes and said %q, want it whole", len(got), len(whole), mark)
				}
				return
			}
			_, gotMessages := ocDecode(t, got)
			if len(gotMessages) == 0 || gotMessages[0].ID != c.first ||
				!bytes.Equal(got, openCodeResumeLoads(t, whole, c.take)) {
				t.Errorf("the pack kept %d of %d bytes, not those from %q", len(got), len(whole), c.first)
			}
		})
	}
	ask := sandbox.HoldingsAsk{Calls: []sandbox.LoggedCall{ohCall(1, "echo", `{"t":1}`, "done")},
		Resumer: openCodeRelease}
	read := func(export []byte, start sandbox.RunStart) (sandbox.SessionHoldings, error) {
		return readOpenCodeHoldings(t.Context(), bytes.NewReader(export), info.ID, start, ask)
	}
	for start, cut := range map[sandbox.RunStart]string{{After: early}: ocStartCursor(early), {Began: true}: "0"} {
		got, _, _ := openCodePackRun(t, whole, openCodeRelease, cut, trimEverything)
		want, err := read(whole, start)
		if err != nil {
			t.Fatalf("the whole export does not read from %+v: %v", start, err)
		}
		if h, err := read(got, start); err != nil || !reflect.DeepEqual(h, want) {
			t.Errorf("the export trimmed from %+v reads %+v (%v), want %+v as the whole does", start, h, err, want)
		}
	}
	if _, err := read(atKeep, sandbox.RunStart{After: early}); err == nil {
		t.Error("an export cut past the run's start still reads, so the case shows nothing")
	}
}

// A TRIMMED OPENCODE EXPORT READS AS THE WHOLE ONE: the meter counts it as it
// counts the whole, the same restart, figure, provider, estimate and newest
// summary, and its transcript is the whole one's from the keep point on.
//
// Mutation: cut at the newest compaction itself, or past it, and a reading
// parts.
func TestATrimmedOpenCodeExportReadsAsTheWholeOne(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"compacted-twice.json", "kept-turns.json"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			whole := ocTrimFixture(t, fixture)
			cut, _, _ := openCodePackRun(t, whole, openCodeRelease, "0", trimEverything)
			if bytes.Equal(cut, whole) {
				t.Fatal("the pack trimmed nothing")
			}
			var wholeSummary, cutSummary summaryRead
			wholeRead, err := meterExport(t.Context(), bytes.NewReader(gzipOf(t, string(whole))), openCodeRelease,
				&wholeSummary)
			if err != nil || !wholeSummary.found {
				t.Fatalf("the whole export reads no summary (%v)", err)
			}
			cutRead, err := meterExport(t.Context(), bytes.NewReader(gzipOf(t, string(cut))), openCodeRelease,
				&cutSummary)
			if err != nil || !reflect.DeepEqual(cutRead, wholeRead) || cutSummary != wholeSummary {
				t.Errorf("the meter reads the trimmed export as %+v and %+v (%v), the whole as %+v and %+v", cutRead,
					cutSummary, err, wholeRead, wholeSummary)
			}
			entries := func(export []byte) []TranscriptEntry {
				got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(
					t.Context(), OpenCode{}, bytes.NewReader(gzipOf(t, string(export))))
				if err != nil {
					t.Fatal(err)
				}
				return got.Entries
			}
			all, tail := entries(whole), entries(cut)
			if len(tail) == 0 || len(tail) >= len(all) || !reflect.DeepEqual(tail, all[len(all)-len(tail):]) {
				t.Errorf("the trimmed export reads as %d entries, not the last of the whole one's %d", len(tail),
					len(all))
			}
		})
	}
}

// AN OPENCODE PACK PAST THE PACKED POINT IS TRIMMED: an export under the
// plaintext point whose archive is past its own is cut and gzipped again, the
// archive holding the cut, and the word naming that archive.
//
// Mutation: skip the gzip again, or check the plaintext alone, and the case
// goes red.
func TestAnOpenCodePackPastThePackedPointIsTrimmed(t *testing.T) {
	t.Parallel()
	whole := ocTrimFixture(t, "compacted-twice.json")
	kept, size, mark := openCodePackRun(t, whole, openCodeRelease, "0", trimPoints{plain: 1 << 40, packed: 1})
	if want := openCodeResumeLoads(t, whole, ""); !bytes.Equal(kept, want) {
		t.Errorf("the archive holds %d bytes, want the %d its resume loads", len(kept), len(want))
	}
	if !parseTrimMark(mark, size).Trimmed() {
		t.Errorf("the pack's word %q does not name its archive as trimmed", mark)
	}
}

// EVERY OPENCODE PACK TRIMS AT THE TAKE'S BOUNDS, BY ITS RELEASE AND ITS RUN'S
// START: the run's end, a pack on demand (a stop's) and a save each run the
// one trim at [trimAt] before a pack on demand's bound reads the export; the
// run's end by the box's release and the start its script recorded (0 for a
// session it began, none where it continues one and recorded none), every
// pack on demand by its record's.
//
// Mutation: pack one path at other points or untrimmed, bound the export
// before its trim, leave the run's end's release or start unset or stale, or
// a pack on demand's start, and a case goes red.
func TestEveryOpenCodePackTrimsAtTheTakesBounds(t *testing.T) {
	t.Parallel()
	const session = "ses_0123456789abcdef0123456789abcdef"
	p, times := Paths{Home: "/home/user"}, openCodeRunTimes()
	save, err := OpenCode{}.PackSave(p, sessionRecord{id: session, release: "opencode v2.3.2"},
		p.SessionSaveStaging("t", "1"), p.SessionSave("t", "1"))
	if err != nil {
		t.Fatal(err)
	}
	stop := OpenCode{}.Pack(p, session, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true)
	for name, c := range map[string]struct{ script, want string }{
		"end": {openCodeCapture(p, times), bareReleaseShell(trimRelVar) + openCodePackAt(p, p.SessionStaging(),
			p.SessionCapture(), `--server "$url"`, fmt.Sprintf("bounded export %d - ", secs(times.sessionCall)),
			false, trimAt)},
		"stop": {stop, openCodePackAt(p, p.SessionPackStaging("1"), p.SessionPackHeld("1"), "--standalone", "",
			true, trimAt)},
		"save": {save, openCodePackAt(p, p.SessionSaveStaging("t", "1"), p.SessionSave("t", "1"), "--standalone",
			"", true, trimAt)},
	} {
		if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, fmt.Sprint(trimPlainPoint)) {
			t.Errorf("the %s pack does not trim at the take's bounds by its release", name)
		}
	}
	staged := trimStaged(OpenCode{}, trimAt, shellword.Quote(p.SessionPackStaging("1")+"/export.json"))
	if trim, bound := strings.Index(stop, staged), strings.Index(stop, "a kept session's scrub reads"); trim < 0 ||
		bound < trim {
		t.Error("a pack on demand bounds its export before it trims it")
	}
	script := openCodeScript(sandbox.RunRequest{Resume: session}, OpenCode{}.Session(sandbox.RunRequest{
		Resume: session}), p, times)
	if cleared, start := strings.Index(script, "\n"+openCodeTrimStart+"\n"),
		strings.Index(script, openCodeRunStart(p, times, openCodeAnswerFile(p))); cleared < 0 || start < cleared {
		t.Error("the run's script records its start for its end's pack nowhere it clears it first")
	}
	for k, want := range map[string]string{"no": "0", "yes": ""} {
		out, _, code := codexShell(t, t.TempDir(), trimStartVar+"=stale k="+k+"\n"+openCodeTrimStart+"\n"+
			`printf '%s' "$`+trimStartVar+`"`)
		if code != 0 || out != want {
			t.Errorf("a run whose session continued %s starts its trim from %q, want %q", k, out, want)
		}
	}

	// THE RUN'S START, as its script records it, is what its end's pack reads.
	home := Paths{Home: t.TempDir()}
	if err := os.MkdirAll(home.WorkDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	cursor := ocStartCursor("msg_1d8699fae001Qp6E4C1MPr6kP4")
	for page, want := range map[string]string{
		`{"data":[],"cursor":{"previous":"cHJl","next":"` + cursor + `"}}`: cursor,
		`{"data":[]}`: "",
	} {
		stub := filepath.Join(t.TempDir(), "opencode")
		writeHostProgram(t, stub, []byte("#!/bin/sh\nprintf '%s' "+shellword.Quote(page)+"\n"), 0o700)
		out, errText, code := codexShell(t, home.Home, "PATH="+shellword.Quote(filepath.Dir(stub))+`:"$PATH"`+"\n"+
			openCodeCursor()+`bounded() { shift 3; "$@"; }`+"\nk=yes sid="+session+" w= v='opencode v2.3.2' "+
			trimStartVar+"=\n"+openCodeRunStart(home, times, openCodeAnswerFile(home))+`printf '%s' "$`+trimStartVar+`"`)
		if code != 0 || out != want {
			t.Errorf("a run whose server answered %s keeps %q for its end's trim (exit %d: %s), want %q", page, out,
				code, errText, want)
		}
	}

	box := sandbox.NewFakeSandbox("box-1")
	box.Put(PathsFor(box).SessionRecord(), "id "+session+"\ncontinued yes\nwhy \nfrom "+cursor+
		"\nrelease opencode v2.3.2\n")
	box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		return sandbox.ExecResult{ExitCode: packNothing}, nil
	}
	if _, err := NewOpenCode().PackSave(t.Context(), box, "node:1", session); err != nil {
		t.Fatal(err)
	}
	execs := box.Execs()
	for _, want := range []string{trimRelVar + "=" + shellword.Quote(openCodeRelease) + "\n",
		trimStartVar + "=" + cursor + "\n"} {
		if len(execs) == 0 || !strings.Contains(execs[len(execs)-1].Command, strings.ReplaceAll(want, "'", `'\''`)) {
			t.Errorf("a save names no %q off its record to trim by", want)
		}
	}
}
