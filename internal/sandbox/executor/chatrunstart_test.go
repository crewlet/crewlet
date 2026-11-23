package executor_test

import (
	"bytes"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A CHAT FILE'S `FROM` IS READ ONLY AS ITS HARNESS WRITES IT: exactly
// `<lines> <crc> <bytes>` in decimal, the lines within their bound and above
// zero, the CRC a uint32, the bytes at least the lines, and only for a
// harness whose run records lines; an OpenCode cursor is never lines, nor
// lines a cursor; and a run that did not continue records no start.
//
// Mutation: decode the `from` by its shape rather than its harness, take an
// extra field, a leading zero or a count past the bound, and a row goes red.
func TestAChatFromIsReadOnlyAsItsHarnessWritesIt(t *testing.T) {
	t.Parallel()
	const cursor = "eyJpZCI6Im1zZ18xZDg2OTlmYWUwMDFRcDZFNEMxTVByNmtQNCIsIm9yZGVyIjoiZGVzYyIsImRpcmVjdGlvbiI6Im5leHQifQ"
	record := func(continued, from string) string {
		text := "id 9a8b7c6d-0000-4000-8000-000000000003\ncontinued " + continued + "\nwhy \n"
		if from != "" {
			text += "from " + from + "\n"
		}
		return text + "release 0.70.0\n"
	}
	most := strconv.Itoa(sandbox.MaxRunStartLines)
	for name, tc := range map[string]struct {
		h         executor.Harness
		text      string
		prefix    sandbox.LinePrefix
		cursorMsg string
	}{
		"gemini lines":          {h: executor.GeminiCLI{}, text: record("yes", "19 2511461881 10014"), prefix: sandbox.LinePrefix{Lines: 19, CRC: 2511461881, Bytes: 10014}},
		"qwen lines":            {h: executor.QwenCode{}, text: record("yes", "17 2511461881 10014"), prefix: sandbox.LinePrefix{Lines: 17, CRC: 2511461881, Bytes: 10014}},
		"lines at the bound":    {h: executor.QwenCode{}, text: record("yes", most+" 1 "+most), prefix: sandbox.LinePrefix{Lines: sandbox.MaxRunStartLines, CRC: 1, Bytes: sandbox.MaxRunStartLines}},
		"lines past the bound":  {h: executor.QwenCode{}, text: record("yes", strconv.Itoa(sandbox.MaxRunStartLines+1)+" 1 "+most+"0")},
		"no lines":              {h: executor.QwenCode{}, text: record("yes", "0 1 1")},
		"bytes below the lines": {h: executor.QwenCode{}, text: record("yes", "17 1 16")},
		"a crc past uint32":     {h: executor.QwenCode{}, text: record("yes", "17 4294967296 100")},
		"an extra field":        {h: executor.QwenCode{}, text: record("yes", "17 1 100 4")},
		"a leading zero":        {h: executor.QwenCode{}, text: record("yes", "017 1 100")},
		"a sign":                {h: executor.QwenCode{}, text: record("yes", "+17 1 100")},
		"a run that began":      {h: executor.QwenCode{}, text: record("no", "17 1 100")},
		"a cursor for gemini":   {h: executor.GeminiCLI{}, text: record("yes", cursor)},
		"lines for opencode":    {h: executor.OpenCode{}, text: record("yes", "17 1 100")},
		"a cursor for opencode": {h: executor.OpenCode{}, text: record("yes", cursor), cursorMsg: "msg_1d8699fae001Qp6E4C1MPr6kP4"},
	} {
		start, prefix := executor.RecordStarts(tc.h, tc.text)
		if prefix != tc.prefix {
			t.Errorf("%s: the record names lines %+v, want %+v", name, prefix, tc.prefix)
		}
		if start.After != tc.cursorMsg || start.Lines != 0 {
			t.Errorf("%s: the archive's start reads %+v, want after %q and no lines", name, start, tc.cursorMsg)
		}
	}
}

// chatFrom is the `from` of the session record in box, "" where it has none.
func chatFrom(t *testing.T, box sandbox.Sandbox) string {
	t.Helper()
	for _, line := range strings.Split(boxFile(t, box, paths(box).SessionRecord()), "\n") {
		if from, ok := strings.CutPrefix(line, "from "); ok {
			return from
		}
	}
	return ""
}

// prefixOf is `<lines> <cksum of them>` of text's first n lines.
func prefixOf(t *testing.T, text string, n int) string {
	t.Helper()
	lines := strings.SplitAfter(text, "\n")
	if len(lines) < n {
		t.Fatalf("the file holds %d lines, fewer than %d", len(lines), n)
	}
	return strconv.Itoa(n) + " " + digestOf(t, []byte(strings.Join(lines[:n], "")))
}

// withMember is archive with its one member's content replaced by edit's.
func withMember(t *testing.T, archive []byte, edit func(string) string) []byte {
	t.Helper()
	for name, body := range archiveMembers(t, bytes.NewReader(archive)) {
		return tarGz(t, name, []byte(edit(body)))
	}
	t.Fatal("the archive holds no member")
	return nil
}

// A GEMINI CLI RUN RECORDS THE LINES ITS SESSION HELD BEFORE IT: continuing a
// carried session whose last line was torn, its record's `from` (before its
// release) is the count and the POSIX cksum of the whole lines the gate left,
// as the harness took the file up; a run that began its session records none.
// (A count taken before the torn line is cut is the same count: `wc -l` and
// `head -n` see only whole lines either way.)
//
// Mutation: record `from` after the release, or none, and this goes red.
func TestAGeminiRunRecordsTheLinesItsSessionHeldBeforeIt(t *testing.T) {
	t.Parallel()
	ran, described, raw := geminiCarried(t, geminiCapturedRelease)
	file := "session-2026-11-21T18-48-" + ran.SessionID[:8] + ".jsonl"
	var carried string
	raw = withMember(t, raw, func(body string) string { carried = body; return body + `{"id":"torn` })
	runner, b := geminiBoxAt(t, geminiCapturedRelease, geminiCarryStandIn(t))
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
	n := strings.Count(carried, "\n")
	if got, want := chatFrom(t, b), prefixOf(t, boxFile(t, b, paths(b).GeminiChats()+"/"+file), n); got != want {
		t.Errorf("the run records its start as %q, want %q: the %d whole lines it took up", got, want, n)
	}
	if record := boxFile(t, b, paths(b).SessionRecord()); strings.Index(record, "from ") > strings.Index(record, "release ") {
		t.Errorf("the record names its start after its release:\n%s", record)
	}
	if from := chatFrom(t, mustRanFresh(t)); from != "" {
		t.Errorf("a run that began its session records a start %q", from)
	}
}

// mustRanFresh is a box whose Gemini CLI run began its own session.
func mustRanFresh(t *testing.T) sandbox.Sandbox {
	t.Helper()
	runner, a := geminiBoxAt(t, geminiCapturedRelease, geminiCarryStandIn(t))
	runToEnd(t, runner, a, sandbox.RunRequest{Brief: "begin"})
	return a
}

// A QWEN CODE RUN RECORDS THE LINES ITS SESSION HELD BEFORE IT: continuing a
// carried session that ends in a note no reply followed and a torn line, its
// `from` is the count and the cksum of the whole lines it loads, the note cut
// by the gate (qwenUnansweredCut), as the restore rewrote the first line's
// working directory, and the count its notes are confirmed past is that count.
//
// Mutation: count before the note is cut, or before the restore, and this goes
// red.
func TestAQwenRunRecordsTheLinesItsSessionHeldBeforeIt(t *testing.T) {
	t.Parallel()
	standIn := qwenCarryStandIn(t)
	first, a := qwenBox(t, standIn)
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	raw, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var carried string
	note := `{"uuid":"aaaaaaaa-0000-4000-8000-000000000001","parentUuid":null,"sessionId":"` + ran.SessionID +
		`","timestamp":"2026-11-23T00:00:00.000Z","type":"user","subtype":"mid_turn_user_message",` +
		`"message":{"role":"user","parts":[{"text":"\n[User message received during tool execution]: a note"}]}}` + "\n"
	raw = withMember(t, raw, func(body string) string { carried = body; return body + note + `{"uuid":"torn` })
	second, b := qwenBox(t, standIn)
	runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
	n := strings.Count(carried, "\n")
	cwd := strings.TrimSpace(boxFile(t, b, b.Home()+"/cwd"))
	chat := boxFile(t, b, paths(b).QwenDir()+"/projects/"+qwenKey(cwd)+"/chats/"+ran.SessionID+".jsonl")
	if got, want := chatFrom(t, b), prefixOf(t, chat, n); got != want {
		t.Errorf("the run records its start as %q, want %q: the %d whole lines it loaded", got, want, n)
	}
	if got := boxFile(t, b, paths(b).QwenRunStart()); got != ran.SessionID+" "+strconv.Itoa(n)+"\n" {
		t.Errorf("the run's notes are confirmed past %q, want its %d lines", got, n)
	}
}

// takeLines is the run start a take of the job in box keeps, read whole, its
// scrub handed scrub, packed now where pack says so.
func takeLines(t *testing.T, runner *executor.Runner, box sandbox.Sandbox, session string, pack bool,
	scrub ...string) (int64, []byte) {
	t.Helper()
	taken, err := runner.TakeSession(t.Context(), box, sandbox.SessionTake{Spool: testSpool(t), Pack: pack,
		Scrub: scrub, MaxBytes: takeBound, Session: session})
	if err != nil || taken.Body == nil {
		t.Fatalf("TakeSession = %+v, %v", taken, err)
	}
	kept, err := io.ReadAll(taken.Body)
	if err != nil {
		t.Fatalf("reading the take: %v", err)
	}
	if err = taken.Body.Close(); err != nil {
		t.Fatal(err)
	}
	return taken.RunLines(), kept
}

// planted is a value a take's scrub is handed, in a whole line of the
// session the run continued.
const plantedPrefix = "planted-prefix-value-7d1c"

// continuedWithPlant is a box whose Gemini CLI run continued a carried session
// holding plantedPrefix in a whole line, its runner, the session and the
// lines it began after.
func continuedWithPlant(t *testing.T) (*executor.Runner, sandbox.Sandbox, string, int64) {
	t.Helper()
	ran, described, raw := geminiCarried(t, geminiCapturedRelease)
	var lines int
	raw = withMember(t, raw, func(body string) string {
		body += `{"$set":{"lastUpdated":"` + plantedPrefix + `"}}` + "\n"
		lines = strings.Count(body, "\n")
		return body
	})
	runner, b := geminiBoxAt(t, geminiCapturedRelease, geminiCarryStandIn(t))
	runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
	return runner, b, ran.SessionID, int64(lines)
}

// A RUN START IS KEPT AS THE LINES IT NAMED: the take checks the box's digest
// against the raw bytes of the chat file it reads, before its scrub rewrites a
// value in those very lines, and keeps the count; a capture packed now, of the
// unchanged file, keeps it too.
//
// Mutation: digest the kept (scrubbed) bytes, or drop RunLines from the take,
// and this goes red.
func TestARunStartIsKeptAsTheLinesItNamed(t *testing.T) {
	t.Parallel()
	runner, b, session, lines := continuedWithPlant(t)
	got, kept := takeLines(t, runner, b, session, false, plantedPrefix)
	if got != lines {
		t.Errorf("the take keeps the run's start at line %d, want %d", got, lines)
	}
	res, err := b.Exec(t.Context(), "cat "+shellword.Quote(paths(b).GeminiChats())+"/*.jsonl", sandbox.ExecOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw := res.Stdout
	for name, body := range archiveMembers(t, bytes.NewReader(kept)) {
		if strings.Contains(body, plantedPrefix) {
			t.Fatalf("%s still holds the scrubbed value: the case proves no rewrite", name)
		}
		// THE SCRUB KEEPS A CHAT LINE ONE LINE, so the kept file holds the
		// lines its start counts.
		if strings.Count(body, "\n") != strings.Count(raw, "\n") {
			t.Errorf("%s holds %d lines as kept, %d in the box", name, strings.Count(body, "\n"),
				strings.Count(raw, "\n"))
		}
	}
	if got, _ = takeLines(t, runner, b, session, true, plantedPrefix); got != lines {
		t.Errorf("a pack made now of the same file keeps the run's start at line %d, want %d", got, lines)
	}
}

// A CHANGED PREFIX LEAVES THE CAPTURE NO START: a chat file whose lines before
// the run's start no longer digest as the box recorded them (one byte changed,
// its length and count the same), or that holds fewer of them (rewound, cut),
// keeps no start, so nothing reads it as the run's.
//
// Mutation: compare the counts and lengths alone, or keep a start whose lines
// the file lacks, and a row goes red.
func TestAChangedPrefixLeavesTheCaptureNoStart(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]string{
		"a byte changed": `sed -i 's/` + plantedPrefix + `/` + strings.Replace(plantedPrefix, "7", "8", 1) + `/' "$f"`,
		"a line fewer": `head -n "$(( $(grep -n ` + shellword.Quote(plantedPrefix) +
			` "$f" | cut -d: -f1) - 1 ))" "$f" > "$f.cut" && mv "$f.cut" "$f"`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b, session, _ := continuedWithPlant(t)
			inBox(t, b, `for f in `+shellword.Quote(paths(b).GeminiChats())+`/*.jsonl; do `+edit+`; done`)
			if got, _ := takeLines(t, runner, b, session, true); got != 0 {
				t.Errorf("the take keeps the run's start at line %d of a file whose prefix changed", got)
			}
		})
	}
}
