package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// chatTrimHarness is a chat harness the trim's cases pack: its runner's
// harness, the directory its chat file sits under, and the file's member.
type chatTrimHarness struct {
	name   string
	t      trimmer
	cfg    func(Paths) string
	member string
}

var (
	geminiTrimHarness = chatTrimHarness{"gemini", GeminiCLI{}, func(p Paths) string { return p.GeminiSlugDir() },
		"chats/session-2026-11-24T23-59-7c0ffee0.jsonl"}
	qwenTrimHarness = chatTrimHarness{"qwen", QwenCode{}, func(p Paths) string { return p.QwenDir() },
		"projects/-tmp-sb-work-proj/chats/11111111-2222-4333-8444-0000000005b5.jsonl"}
)

// chatTrimFixture is testdata's chat file name of h's measured release.
func chatTrimFixture(t *testing.T, h chatTrimHarness, name string) []byte {
	t.Helper()
	dir := map[string]string{"gemini": "gemini/0.70.0", "qwen": "qwen/0.28.4"}[h.name]
	return codexFile(t, filepath.Join("testdata", dir, name))
}

// chatPackRun packs record as h's chat file in a box of its own at at, its
// session written by release and its run begun after start lines ("" for not
// known), and returns the file as packed, the archive and the word beside it
// ("" for none).
func chatPackRun(t *testing.T, h chatTrimHarness, record []byte, release, start string,
	at trimPoints) (string, []byte, string) {
	t.Helper()
	p := Paths{Home: t.TempDir()}
	file := filepath.Join(h.cfg(p), h.member)
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, record, 0o600); err != nil {
		t.Fatal(err)
	}
	q := shellword.Quote
	staged, dest := p.SessionPackStaging("trim"), p.SessionPackHeld("trim")
	script := "s=x cm=" + q(h.member) + " " + trimRelVar + "=" + q(release) + " " + trimStartVar + "=" + q(start) +
		" packed=\n" + chatPackAt(p, packAt{staged: staged, dest: dest}, h.cfg(p), h.t, at) +
		`[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	archive := codexFile(t, dest)
	word, err := os.ReadFile(trimMarkOf(dest))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	members := codexMembers(t, archive)
	if len(members) != 1 {
		t.Fatalf("the archive holds %d members, want the chat file alone", len(members))
	}
	return members[h.member], archive, string(word)
}

// chatLinesOf is record's lines, each with its break.
func chatLinesOf(record []byte) [][]byte {
	lines := bytes.SplitAfter(record, []byte("\n"))
	if len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// chatCutOf is record as the measured rule cuts it before the line its newest
// compaction opens on (boundary, from 1; 0 for none) for a run begun after
// start lines: line 1 and every line past min(boundary-1, start), where that
// is 2 or more, whole otherwise; and where cut, the cut copy's lines that held
// the run's start, digested as the box's `cksum` digests them.
func chatCutOf(record []byte, boundary, start int) ([]byte, sandbox.LinePrefix) {
	c := min(boundary-1, start)
	if boundary == 0 || c < 2 {
		return record, sandbox.LinePrefix{}
	}
	lines := chatLinesOf(record)
	kept := slices.Concat(append([][]byte{lines[0]}, lines[c:]...)...)
	n := start - c + 1
	var sum cksum
	_, _ = sum.Write(slices.Concat(chatLinesOf(kept)[:n]...))
	return kept, chatRunStartOf(strconv.Itoa(n) + " " + sum.String())
}

// geminiNewestCompression is the line (from 1) record's newest compression
// opens on as the measurement found one, each line decoded: a `$set` of the
// session alone, then a user entry, then the harness's reply alone, its sync
// closed by a `$patch` removing entries before the next `$set` of the time;
// 0 for none.
func geminiNewestCompression(t *testing.T, record []byte) int {
	t.Helper()
	type entry struct {
		Set     map[string]json.RawMessage `json:"$set"`
		Patch   map[string]json.RawMessage `json:"$patch"`
		Type    string                     `json:"type"`
		Content json.RawMessage            `json:"content"`
	}
	lines := chatLinesOf(record)
	entries := make([]entry, len(lines))
	keys := make([]int, len(lines))
	for i, l := range lines {
		var m map[string]json.RawMessage
		if json.Unmarshal(l, &m) == nil && json.Unmarshal(l, &entries[i]) == nil {
			keys[i] = len(m)
		}
	}
	ack, _ := json.Marshal([]map[string]string{{"text": geminiAck}})
	newest := 0
	for i := 0; i+2 < len(lines); i++ {
		set, user, reply := entries[i], entries[i+1], entries[i+2]
		_, session := set.Set["sessionId"]
		if keys[i] != 1 || len(set.Set) != 1 || !session || user.Type != "user" || reply.Type != "gemini" ||
			!bytes.Equal(reply.Content, ack) {
			continue
		}
		for _, e := range entries[i+3:] {
			if _, removes := e.Patch["removeIds"]; removes {
				newest = i + 1
				break
			}
			if _, at := e.Set["lastUpdated"]; at {
				break
			}
		}
	}
	return newest
}

// qwenNewestCompaction is the line (from 1) record's newest compaction opens
// on, decoded: its newest `chat_compression` system record; 0 for none.
func qwenNewestCompaction(record []byte) int {
	newest := 0
	for i, l := range chatLinesOf(record) {
		var e struct{ Type, Subtype string }
		if json.Unmarshal(l, &e) == nil && e.Type == "system" && e.Subtype == "chat_compression" {
			newest = i + 1
		}
	}
	return newest
}

// chatTrimCase is one pack of a record from its run's start.
type chatTrimCase struct {
	h        chatTrimHarness
	fixture  string
	boundary func([]byte) int
}

// A TRIMMED CHAT PACK KEEPS WHAT ITS RESUME LOADS FROM ITS RUN'S START: past
// the point, a Gemini CLI or Qwen Code chat file keeps its first line and every
// line after the earlier of its newest compaction's first and its run's start,
// as the measured rule reads them, and the pack says what it kept and the run's
// start over the cut copy beside the archive.
//
// Mutation: drop line 1, cut at the oldest compaction, at its summary rather
// than its first line, past the run's start, or restate the record's start,
// and a case goes red.
func TestATrimmedGeminiPackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	gemini := func(record []byte) int { return geminiNewestCompression(t, record) }
	chatTrimCases(t, []chatTrimCase{{geminiTrimHarness, geminiPreambleSession, gemini},
		{geminiTrimHarness, "session-compressed.jsonl", gemini}}, geminiRelease)
}

// A TRIMMED QWEN CODE PACK KEEPS WHAT ITS LOAD LOADS FROM ITS RUN'S START, as
// a Gemini CLI one does ([TestATrimmedGeminiPackKeepsWhatItsResumeLoads]), at
// its newest `chat_compression` record, a run's in a git work tree and one's
// outside it alike.
//
// Mutation: cut at the oldest record, match it with its git branch alone, or
// past the run's start, and a case goes red.
func TestATrimmedQwenPackKeepsWhatItsResumeLoads(t *testing.T) {
	t.Parallel()
	chatTrimCases(t, []chatTrimCase{{qwenTrimHarness, "chat-compacted-thrice.jsonl", qwenNewestCompaction},
		{qwenTrimHarness, "chat-compact.jsonl", qwenNewestCompaction}}, qwenRelease)
}

// chatTrimCases packs each case's fixture from every run start that cuts it
// differently, and holds each pack to [chatCutOf].
func chatTrimCases(t *testing.T, cases []chatTrimCase, release string) {
	t.Helper()
	for _, c := range cases {
		record := chatTrimFixture(t, c.h, c.fixture)
		lines, boundary := len(chatLinesOf(record)), c.boundary(record)
		if boundary < 3 {
			t.Fatalf("%s holds no compression past its second line: the case proves nothing", c.fixture)
		}
		for _, start := range []int{lines, boundary + 1, boundary, boundary - 1, boundary - 5, 3, 2} {
			t.Run(fmt.Sprintf("%s from %d", c.fixture, start), func(t *testing.T) {
				t.Parallel()
				want, prefix := chatCutOf(record, boundary, start)
				got, archive, mark := chatPackRun(t, c.h, record, release, strconv.Itoa(start), trimEverything)
				if got != string(want) {
					t.Errorf("the pack kept %d of %d lines, want %d", strings.Count(got, "\n"), lines,
						strings.Count(string(want), "\n"))
				}
				wantMark := sandbox.ArchiveTrim{RecordBytes: int64(len(record)), KeptBytes: int64(len(want)),
					Prefix: prefix}
				if got := parseTrimMark(mark, int64(len(archive))); got != wantMark {
					t.Errorf("the pack's word %q reads as %+v, want %+v", mark, got, wantMark)
				}
			})
		}
	}
}

// A CHAT PACK IS NEVER CUT PAST ITS RUN'S START: a run that began its session
// (0), one whose start is not known, or a line, packs its file whole past the
// point, and says nothing.
//
// Mutation: read a start not known as 0 lines, cut a session its run began, or
// cut line 2 of a one-line start, and a case goes red.
func TestAChatPackIsNeverCutPastItsRunsStart(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		h       chatTrimHarness
		fixture string
	}{{geminiTrimHarness, geminiPreambleSession}, {qwenTrimHarness, "chat-compacted-thrice.jsonl"}} {
		for _, start := range []string{"0", "", "1", "x", "-5"} {
			t.Run(c.h.name+" from "+start, func(t *testing.T) {
				t.Parallel()
				record := chatTrimFixture(t, c.h, c.fixture)
				release := map[string]string{"gemini": geminiRelease, "qwen": qwenRelease}[c.h.name]
				got, _, mark := chatPackRun(t, c.h, record, release, start, trimEverything)
				if got != string(record) || mark != "" {
					t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(got),
						len(record), mark)
				}
			})
		}
	}
}

// A GEMINI SYNC LEFT OPEN IS NO BOUNDARY: where a compression's `$patch` never
// came before the next `$set` of the time, or the file ends inside its sync
// (a pack beside a harness mid-write), the pack cuts at the compression
// before it.
//
// Mutation: drop the guard, or let a `$set` of the time close a sync, and a
// case goes red.
func TestAGeminiSyncLeftOpenIsNoBoundary(t *testing.T) {
	t.Parallel()
	record := chatTrimFixture(t, geminiTrimHarness, geminiPreambleSession)
	lines := chatLinesOf(record)
	if !bytes.HasPrefix(lines[97], []byte(`{"$patch":{"removeIds":[`)) {
		t.Fatal("line 98 is not the newer compression's closing patch: the case proves nothing")
	}
	for name, open := range map[string][]byte{
		"its patch dropped":   slices.Concat(slices.Concat(lines[:97]...), slices.Concat(lines[98:]...)),
		"ended inside a sync": slices.Concat(lines[:97]...),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			boundary := geminiNewestCompression(t, open)
			if boundary != 48 {
				t.Fatalf("the oracle finds the boundary at %d, want the older compression's 48", boundary)
			}
			start := len(chatLinesOf(open))
			want, _ := chatCutOf(open, boundary, start)
			if got, _, _ := chatPackRun(t, geminiTrimHarness, open, geminiRelease, strconv.Itoa(start),
				trimEverything); got != string(want) {
				t.Errorf("the pack kept %d lines, want %d: cut at the older compression",
					strings.Count(got, "\n"), strings.Count(string(want), "\n"))
			}
		})
	}
}

// A CHAT PACK WITH NOTHING TO TRIM AT IS PACKED WHOLE: a file with no
// compression, one whose text holds a compression's lines as words, one whose
// reply comes with no `$set` before its summary, and a Qwen Code message
// naming a compaction's fields keep every line, and no word says otherwise.
//
// Mutation: match a form anywhere in a line, or a reply alone, and a case goes
// red.
func TestAChatPackWithNothingToTrimAtIsPackedWhole(t *testing.T) {
	t.Parallel()
	gemini := chatTrimFixture(t, geminiTrimHarness, "session-tools.jsonl")
	compressed := chatLinesOf(chatTrimFixture(t, geminiTrimHarness, geminiPreambleSession))
	words, _ := json.Marshal(string(slices.Concat(compressed[92:98]...)))
	worded := slices.Concat(gemini, []byte(`{"id":"w","timestamp":"t","type":"user","content":[{"text":`+
		string(words)+`}]}`+"\n"), slices.Concat(compressed[93:]...))
	unset := slices.Concat(gemini, slices.Concat(compressed[93:]...))
	qwen := chatTrimFixture(t, qwenTrimHarness, "chat-note.jsonl")
	boundary := chatLinesOf(chatTrimFixture(t, qwenTrimHarness, "chat-compacted-thrice.jsonl"))[63]
	said, _ := json.Marshal(string(boundary))
	named := slices.Concat(qwen, []byte(`{"uuid":"n","parentUuid":null,"sessionId":"s","timestamp":"t",`+
		`"type":"user","message":{"role":"user","parts":[{"text":`+string(said)+`}]}}`+"\n"), qwen[bytes.IndexByte(qwen, '\n')+1:])
	for name, c := range map[string]struct {
		h      chatTrimHarness
		record []byte
	}{
		"gemini with no compression":     {geminiTrimHarness, gemini},
		"gemini naming one in its words": {geminiTrimHarness, worded},
		"gemini with no set before it":   {geminiTrimHarness, unset},
		"qwen with no compaction":        {qwenTrimHarness, qwen},
		"qwen naming one in its message": {qwenTrimHarness, named},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			release := map[string]string{"gemini": geminiRelease, "qwen": qwenRelease}[c.h.name]
			start := strconv.Itoa(len(chatLinesOf(c.record)))
			got, _, mark := chatPackRun(t, c.h, c.record, release, start, trimEverything)
			if got != string(c.record) || mark != "" {
				t.Errorf("the pack kept %d of %d bytes and said %q, want it whole and unsaid", len(got),
					len(c.record), mark)
			}
		})
	}
}

// ONLY THE MEASURED CHAT RELEASES ARE TRIMMED: a session another release
// wrote, or one whose writer the pack was not told, is packed whole past the
// point, and so is one below the points.
//
// Mutation: drop the release's check or the points', and a case goes red.
func TestOnlyTheMeasuredChatReleasesAreTrimmed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		h                chatTrimHarness
		fixture, release string
	}{{geminiTrimHarness, geminiPreambleSession, geminiRelease}, {qwenTrimHarness, "chat-compacted-thrice.jsonl",
		qwenRelease}} {
		record := chatTrimFixture(t, c.h, c.fixture)
		start := strconv.Itoa(len(chatLinesOf(record)))
		for _, run := range []struct {
			release string
			at      trimPoints
		}{{"", trimEverything}, {"0.1.0", trimEverything}, {c.release + "1", trimEverything},
			{c.release, trimPoints{plain: 1 << 40, packed: 1 << 40}}} {
			t.Run(fmt.Sprintf("%s %q %d", c.h.name, run.release, run.at.plain), func(t *testing.T) {
				t.Parallel()
				got, _, mark := chatPackRun(t, c.h, record, run.release, start, run.at)
				if got != string(record) || mark != "" {
					t.Errorf("packed %d of %d bytes, saying %q; want it whole", len(got), len(record), mark)
				}
			})
		}
	}
}

// EVERY CHAT PACK TRIMS AT THE TAKE'S BOUNDS FROM ITS RUN'S START: the run's
// end, a pack on demand (a stop's) and a save of each chat harness run the one
// trim at [trimAt], the run's end naming the box's bare release and its
// script's own start (its line count where it continued, 0 where it began its
// session, none where neither is known), and every pack the engine runs the
// release and the start its record names.
//
// Mutation: pack one path at other points or untrimmed, set no start on a
// path, or read a start not known as 0, and a case goes red.
func TestEveryChatPackTrimsAtTheTakesBoundsFromItsRunsStart(t *testing.T) {
	t.Parallel()
	p := Paths{Home: "/home/user"}
	const session = "7c0ffee0-5b5b-4a4a-8c8c-000000002020"
	continued := Session{ID: session, Continues: true, Fresh: "7c0ffee0-5b5b-4a4a-8c8c-000000002021"}
	scripts := map[string]string{
		"gemini": geminiScript(sandbox.RunRequest{Brief: "x", Resume: session}, continued, p),
		"qwen":   qwenScript(sandbox.RunRequest{Brief: "x", Resume: session}, continued, p, time.Second),
	}
	for _, h := range []chatTrimHarness{geminiTrimHarness, qwenTrimHarness} {
		carrier := h.t.(interface {
			Pack(Paths, string, string, string, bool) string
			PackSave(Paths, sessionRecord, string, string) (string, error)
		})
		save, err := carrier.PackSave(p, sessionRecord{id: session}, p.SessionSaveStaging("t", "1"),
			p.SessionSave("t", "1"))
		if err != nil {
			t.Fatal(err)
		}
		end := bareReleaseShell(trimRelVar) + chatPackAt(p, packAt{staged: p.SessionStaging(),
			dest: p.SessionCapture()}, h.cfg(p), h.t, trimAt)
		for name, c := range map[string]struct{ script, want string }{
			"end": {scripts[h.name], strings.TrimSuffix(end, "\n")},
			"stop": {carrier.Pack(p, session, p.SessionPackStaging("1"), p.SessionPackHeld("1"), true),
				chatPackAt(p, packAt{staged: p.SessionPackStaging("1"), dest: p.SessionPackHeld("1"), working: true},
					h.cfg(p), h.t, trimAt)},
			"save": {save, chatPackAt(p, packAt{staged: p.SessionSaveStaging("t", "1"), dest: p.SessionSave("t", "1"),
				working: true}, h.cfg(p), h.t, trimAt)},
		} {
			if !strings.Contains(c.script, c.want) || !strings.Contains(c.want, fmt.Sprint(trimPlainPoint)) {
				t.Errorf("the %s %s pack does not trim at the take's bounds by its release", h.name, name)
			}
		}
		if !strings.Contains(scripts[h.name], chatRunStartFrom) {
			t.Errorf("the %s run's script sets no start to trim from", h.name)
		}
	}
	for name, c := range map[string]struct{ vars, want string }{
		"continued":           {`k=yes cf=f; printf '1\n2\n3\n' > f`, "3"},
		"began":               {`k=no cf=`, "0"},
		"continued with none": {`k=yes cf=`, "unset"},
	} {
		out, errText, code := codexShell(t, t.TempDir(), c.vars+"\n"+chatRunStartFrom+"\n"+
			`printf '%s' "${`+trimStartVar+`:-unset}"`)
		if code != 0 || out != c.want {
			t.Errorf("a run %s trims from %q (exit %d: %s), want %q", name, out, code, errText, c.want)
		}
	}
	for name, c := range map[string]struct{ record, want string }{
		"continued": {"continued yes\nwhy \nfrom 7 123 456\n", "\n" + trimStartVar + "=7\n"},
		"began":     {"continued no\nwhy \n", "\n" + trimStartVar + "=0\n"},
		"not known": {"continued yes\nwhy \n", "\n" + trimStartVar + "=\n"},
	} {
		box := sandbox.NewFakeSandbox("box-1")
		box.Put(PathsFor(box).SessionRecord(), "id "+session+"\n"+c.record+"release "+geminiRelease+"\n")
		box.ExecFunc = func(_ context.Context, _ string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
			return sandbox.ExecResult{ExitCode: packNothing}, nil
		}
		if _, err := NewGeminiCLI().PackSave(t.Context(), box, "node:1", session); err != nil {
			t.Fatal(err)
		}
		if execs := box.Execs(); len(execs) == 0 || !strings.Contains(execs[len(execs)-1].Command, c.want) {
			t.Errorf("a save of a run %s names no start %q off its record: %+v", name, c.want, execs)
		}
	}
}

// A TRIMMED CHAT TAKE FINDS ITS RUN START IN THE CUT COPY: a continued
// Gemini CLI run's pack trimmed past its record's start is taken with the
// start its pack restated, found whole in the archive, so the capture keeps
// the run's start at the cut copy's line; without the restatement, restating
// the record's own, or beside a record naming fewer lines than it restates,
// the take keeps none, since the record's lines are no longer the archive's.
//
// Mutation: check the record's start on a trimmed archive, or take a
// restatement longer than the record's, and a case goes red.
func TestATrimmedChatTakeFindsItsRunStartInTheCutCopy(t *testing.T) {
	t.Parallel()
	record := chatTrimFixture(t, geminiTrimHarness, geminiPreambleSession)
	const start, session = 110, "7c0ffee0-5b5b-4a4a-8c8c-000000002020"
	fromOf := func(lines int) string {
		var sum cksum
		_, _ = sum.Write(slices.Concat(chatLinesOf(record)[:lines]...))
		return strconv.Itoa(lines) + " " + sum.String()
	}
	from := fromOf(start)
	_, archive, mark := chatPackRun(t, geminiTrimHarness, record, geminiRelease, strconv.Itoa(start), trimEverything)
	restated := parseTrimMark(mark, int64(len(archive))).Prefix
	if restated.Lines != start-92+1 {
		t.Fatalf("the pack restated %+v, want the cut copy's %d lines", restated, start-92+1)
	}
	kept := strings.Join(strings.SplitN(mark, " ", 4)[:3], " ")
	for name, c := range map[string]struct {
		mark, from string
		want       int64
	}{
		"restated":                    {mark, from, restated.Lines},
		"not restated":                {"", from, 0},
		"restated as its record":      {kept + " " + from + "\n", from, 0},
		"beside a record of fewer":    {mark, fromOf(int(restated.Lines) - 1), 0},
		"beside a record of no start": {mark, "", 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := sandbox.NewFakeSandbox("box-1")
			paths := PathsFor(box)
			rec := "id " + session + "\ncontinued yes\nwhy \n"
			if c.from != "" {
				rec += "from " + c.from + "\n"
			}
			box.Put(paths.SessionRecord(), rec+"release "+geminiRelease+"\n")
			box.Put(paths.SessionCapture(), string(archive))
			if c.mark != "" {
				box.Put(trimMarkOf(paths.SessionCapture()), c.mark)
			}
			spool := sandbox.SessionSpool{Dir: t.TempDir(), Bytes: math.MaxInt64, Held: func(int64) {}}
			taken, err := NewGeminiCLI().TakeSession(t.Context(), box, sandbox.SessionTake{Spool: spool,
				MaxBytes: int64(len(archive)), Session: session})
			if err != nil || taken.Body == nil {
				t.Fatalf("the take: %+v, %v", taken, err)
			}
			if _, err := io.Copy(io.Discard, taken.Body); err != nil {
				t.Errorf("reading the take: %v", err)
			}
			_ = taken.Body.Close()
			if got := taken.RunLines(); got != c.want {
				t.Errorf("the take keeps the run's start at line %d, want %d", got, c.want)
			}
		})
	}
}

// A LINE FORM READS ALIKE IN GO AND IN AWK: every form a pack's awk is built
// from holds the same lines under `LC_ALL=C awk` as under [lineForm.holds]:
// every line of the measured chat files, and lines whose holes escape a quote
// or a backslash, end on a backslash at the bound, run to the bound and one
// past it whole and to their opening's end, hold a whole form to the bound
// and a byte more, or hold a character of several bytes before it.
//
// Mutation: count the bound in characters, walk a hole past an escaped quote,
// or read a whole form by its opening, in either, and a line goes red.
func TestALineFormReadsAlikeInGoAndInAwk(t *testing.T) {
	t.Parallel()
	forms := []namedForm{{"set", geminiCompressionSet}, {"user", geminiCompressionUser},
		{"ack", geminiCompressionAck}, {"closed", geminiSyncClosed}, {"touched", geminiSyncTouched}}
	forms = append(forms, qwenCompressions()...)
	var lines [][]byte
	for _, f := range []struct {
		h    chatTrimHarness
		name string
	}{{geminiTrimHarness, geminiPreambleSession}, {geminiTrimHarness, "session-compressed.jsonl"},
		{qwenTrimHarness, "chat-compacted-thrice.jsonl"}, {qwenTrimHarness, "chat-compact.jsonl"}} {
		for _, l := range chatLinesOf(chatTrimFixture(t, f.h, f.name)) {
			lines = append(lines, bytes.TrimSuffix(l, []byte("\n")))
		}
	}
	set := func(id string) string { return `{"$set":{"sessionId":"` + id + `"}}` }
	user := `{"id":"u","timestamp":"t","type":"user","content":[`
	// fill is the x's that take s to n bytes.
	fill := func(n int, s string) string { return strings.Repeat("x", n-len(s)) }
	whole := set("")
	for _, l := range []string{
		set(`a\"b`), set(`a\\`), set(`a\\\"`), set(`a\`), set(`é`),
		set(fill(lineFormMaxBytes, whole)), set(fill(lineFormMaxBytes+1, whole)),
		set(fill(lineFormMaxBytes, whole)) + "}",
		set("é" + fill(lineFormMaxBytes-1, whole)), set(strings.Repeat("x", lineFormMaxBytes)),
		`{"id":"` + fill(lineFormMaxBytes+1, user) + `","timestamp":"t","type":"user","content":[`,
		`{"id":"` + fill(lineFormMaxBytes+2, user) + `","timestamp":"t","type":"user","content":[`,
		`{"id":"` + strings.Repeat("é", (lineFormMaxBytes-len(user))/2+1) + `","timestamp":"t","type":"user","content":[`,
		`{"id":"` + fill(lineFormMaxBytes-1, user) + `\","timestamp":"t","type":"user","content":[`,
		`{"id":"a","timestamp":"t","type":"gemini","content":[{"text":"` + geminiAck + `"}]}` + "\r",
		`{"$patch":{"removeIds":[`, `{"$patch":{"removeIds":`, `{"$set":{"lastUpdated":"`, "", `"`, `\`,
	} {
		lines = append(lines, []byte(l))
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "lines"), append(bytes.Join(lines, []byte("\n")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := make([]string, len(forms))
	for i, f := range forms {
		calls[i] = "holds($0, " + awkString(f.name) + ")"
	}
	out, errText, code := codexShell(t, dir, "LC_ALL=C awk "+shellword.Quote(lineFormsAwk(forms...)+
		"{ print "+strings.Join(calls, " ")+" }")+" lines")
	if code != 0 {
		t.Fatalf("awk exited %d: %s", code, errText)
	}
	read := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(read) != len(lines) {
		t.Fatalf("awk read %d lines of %d", len(read), len(lines))
	}
	for i, l := range lines {
		var want strings.Builder
		for _, f := range forms {
			if f.form.holds(l) {
				want.WriteString("1")
			} else {
				want.WriteString("0")
			}
		}
		if read[i] != want.String() {
			t.Errorf("line %d (%.60q, %d bytes): awk holds %s, Go %s", i, l, len(l), read[i], want.String())
		}
	}
}
