package executor_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A captured session read back as its transcript. Every archive a run can
// pack here is PACKED BY THE RUNNER'S OWN SCRIPT — a run in a real box whose
// stand-in harness files its record where the pinned layout names, or the
// OpenCode run's script exporting through the fake opencode — so a case reads
// exactly the bytes a collection keeps. The archives no pack makes — bytes that
// are no gzip, a gzip of no tar, a tar member that is sparse — are built here,
// each saying why: they are what a box's own agent could leave in place of
// what its run packed.

// plantedToken is a GitHub token's shape, which the redaction knows: what a
// harness's record carries when a run read a file holding one.
var plantedToken = "ghp_" + strings.Repeat("Qx7Rb2", 6)

// claudeSessionRecord is the record the stand-in Claude Code files for its
// session: a brief, the model's words, its thinking, a tool call answered as a
// failure that quotes a token, the harness's own bookkeeping, a compaction
// summary, a line that is no record, a subagent's task and its result, and a
// last message of a couple of kibibytes.
var claudeSessionRecord = []string{
	`{"type":"user","message":{"role":"user","content":"Fix the flaky test in ENG-7."},"timestamp":"2026-10-30T12:00:00.000Z"}`,
	`{"type":"user","isMeta":true,"message":{"role":"user","content":"<local-command-caveat>Caveat</local-command-caveat>"},"timestamp":"2026-10-30T12:00:00.100Z"}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"the race is in the cache"}]},"timestamp":"2026-10-30T12:00:01.000Z"}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Running the tests first."}]},"timestamp":"2026-10-30T12:00:02.000Z"}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"go test ./cache/...","description":"run the tests"}}]},"timestamp":"2026-10-30T12:00:03.000Z"}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"FAIL cache_test.go:40 GITHUB_TOKEN=` + plantedToken + `","is_error":true}]},"timestamp":"2026-10-30T12:00:04.000Z"}`,
	`{"type":"file-history-snapshot","messageId":"m1","snapshot":{}}`,
	`{"type":"system","subtype":"compact_boundary","content":"Conversation compacted","timestamp":"2026-10-30T12:00:05.000Z"}`,
	`{"type":"user","isCompactSummary":true,"message":{"role":"user","content":"This session is being continued. The test races on the cache."},"timestamp":"2026-10-30T12:00:06.000Z"}`,
	`not a record`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_2","name":"Task","input":{"description":"find the race","prompt":"Look for the race."}}]},"timestamp":"2026-10-30T12:00:07.000Z"}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","content":[{"type":"text","text":"The cache map is written unlocked."}]}]},"timestamp":"2026-10-30T12:00:30.000Z"}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"Fixed: the cache takes its lock."}]},"timestamp":"2026-10-30T12:01:00.000Z"}`,
	`{"type":"summary","summary":"Fix the flaky cache test","leafUuid":"u1"}`,
	`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` +
		strings.Repeat("the account ", 180) + `"}]},"timestamp":"2026-10-30T12:01:01.000Z"}`,
}

// claudeSidecar is the subagent's own transcript, beside the session's.
var claudeSidecar = []string{
	`{"type":"user","isSidechain":true,"message":{"role":"user","content":"Look for the race."},"timestamp":"2026-10-30T12:00:08.000Z"}`,
	`{"type":"assistant","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":"The cache map is written unlocked."}]},"timestamp":"2026-10-30T12:00:29.000Z"}`,
}

// standInHead is how every stand-in `claude` here begins: it answers its
// version, takes the session it was started in, and knows where the pinned
// layout files that session's record (`$p`).
const standInHead = `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
`

// standInEnd is how every stand-in ends: on a successful result.
const standInEnd = `printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`

// transcriptHarness is a stand-in `claude` that files transcript and sidecar
// as the session it was started in, where the pinned layout names them —
// beside a tool result spilled to a file and a task list, which a capture packs
// and a transcript does not read — and ends on a successful result.
func transcriptHarness(transcript, sidecar []string) string {
	return standInHead + `mkdir -p "$p/$s/subagents" "$p/$s/tool-results" "$CLAUDE_CONFIG_DIR/tasks/$s"
cat > "$p/$s.jsonl" <<'TRANSCRIPT'
` + strings.Join(transcript, "\n") + `
TRANSCRIPT
cat > "$p/$s/subagents/agent-a1.jsonl" <<'SIDECAR'
` + strings.Join(sidecar, "\n") + `
SIDECAR
echo spilled > "$p/$s/tool-results/toolu_1.txt"
echo '[]' > "$CLAUDE_CONFIG_DIR/tasks/$s/1.json"
` + standInEnd
}

// claudeCaptureOf runs the stand-in that files transcript and sidecar in a
// real box, and answers the archive the run packed, as the collection
// describes it, and its bytes.
func claudeCaptureOf(t *testing.T, transcript, sidecar []string) (sandbox.SessionArchive, []byte) {
	t.Helper()
	return claudeCaptureWith(t, transcriptHarness(transcript, sidecar))
}

// claudeCaptureWith runs the stand-in `claude` script in a real box, and
// answers the archive the run packed, as the collection describes it, and its
// bytes.
func claudeCaptureWith(t *testing.T, script string) (sandbox.SessionArchive, []byte) {
	t.Helper()
	runner, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", script)
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if res.Captured == nil {
		t.Fatalf("the run's session was not captured:\n%s", boxFile(t, b, paths(b).Err()))
	}
	described, body, err := runner.OpenCapture(t.Context(), b)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading the capture: %v", err)
	}
	return described, raw
}

// openCodeCaptureOf runs the OpenCode run's script against the fake opencode,
// whose session export is export, and answers the archive the script packed.
func openCodeCaptureOf(t *testing.T, export string) []byte {
	t.Helper()
	run := runScript(t, scriptOpts{login: seededLogin,
		req: sandbox.RunRequest{MCPServers: bridgeOnly, Steer: steerWiring},
		env: []string{"FAKE_SESSION_EXPORT=" + export}})
	if run.code != 0 {
		t.Fatalf("exit %d:\n%s", run.code, run.stderr)
	}
	raw, err := os.ReadFile(filepath.Join(run.home, ".crewlet", "session-capture"))
	if err != nil {
		t.Fatalf("the session was not captured: %v\n%s", err, run.stderr)
	}
	return raw
}

// opener opens an archive whose bytes r hands out, as the object store opens a
// session's object: what a read is handed in its place.
func opener(r io.Reader) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) { return io.NopCloser(r), nil }
}

// neverOpened is an opener a read must not call: the case fails if it does.
func neverOpened(t *testing.T, why string) func(context.Context) (io.ReadCloser, error) {
	return func(context.Context) (io.ReadCloser, error) {
		t.Errorf("the archive was opened: %s", why)
		return nil, errors.New("this archive is not to be opened")
	}
}

// readOpenCode is an OpenCode archive's transcript from its first entry, held
// to bounds.
func readOpenCode(t *testing.T, raw []byte, bounds executor.TranscriptBounds) executor.Transcript {
	t.Helper()
	got, err := executor.ReadTranscriptWithin(t.Context(), executor.OpenCodeName,
		executor.OpenCodeArchiveFormat, opener(bytes.NewReader(raw)), 0, bounds)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
	return got
}

// readClaude is a Claude Code archive's transcript from its first entry, held
// to bounds.
func readClaude(t *testing.T, raw []byte, bounds executor.TranscriptBounds) executor.Transcript {
	t.Helper()
	got, err := executor.ReadTranscriptWithin(t.Context(), executor.ClaudeCodeName,
		executor.ClaudeArchiveFormat, opener(bytes.NewReader(raw)), 0, bounds)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
	return got
}

// shown is an entry as a case states it: who, which thread and tool, whether
// it failed, and its text.
type shown struct {
	kind         executor.EntryKind
	thread, tool string
	failed       bool
	text         string
}

// shownOf is every entry as a case states it.
func shownOf(entries []executor.TranscriptEntry) []shown {
	out := make([]shown, len(entries))
	for i, e := range entries {
		out[i] = shown{kind: e.Kind, thread: e.Thread, tool: e.Tool, failed: e.Failed, text: e.Text}
	}
	return out
}

// sameEntries reports, entry by entry, where got is not want.
func sameEntries(t *testing.T, got []executor.TranscriptEntry, want []shown) {
	t.Helper()
	have := shownOf(got)
	if len(have) != len(want) {
		t.Errorf("the transcript has %d entries, want %d:\n%+v", len(have), len(want), have)
	}
	for i := range min(len(have), len(want)) {
		if have[i] != want[i] {
			t.Errorf("entry %d is %+v, want %+v", i, have[i], want[i])
		}
	}
}

// A CLAUDE CODE SESSION READS AS THE TRANSCRIPT ITS HARNESS RECORDED. The
// archive a run packed reads as its entries in order: the brief, the model's
// words, each tool call with its input as JSON and the result that answered
// it — named by the call's tool, a failure marked — the compaction summary,
// a line that is no record said where it was, and then the subagent's own
// transcript, each of its entries naming it. The harness's bookkeeping (a
// record it wrote for itself, a snapshot of files, its system notice, a
// session title) and the model's thinking are no entry, and neither is a tool
// result spilled to a file or the task list. The entries carry the instant the
// harness recorded, the page is the whole transcript, and the token a tool
// result quoted reads redacted.
//
// Mutation: read the thinking or the meta record as entries, drop the
// subagents' sidecars, name a result by nothing, or skip the redaction, and
// the case goes red.
func TestAClaudeCodeSessionReadsAsTheTranscriptItsHarnessRecorded(t *testing.T) {
	t.Parallel()
	failure := "FAIL cache_test.go:40 GITHUB_TOKEN=" + plantedToken
	redacted := redact.Secrets(failure)
	if !strings.Contains(redacted, redact.Marker) {
		t.Fatalf("the planted token is not one the redaction knows: %q", redacted)
	}
	described, raw := claudeCaptureOf(t, claudeSessionRecord, claudeSidecar)
	got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if got.Cut != "" || got.More() || got.From != 0 || got.Next != len(got.Entries) {
		t.Errorf("the read is cut %q from %d to %d with more %v, want the whole transcript",
			got.Cut, got.From, got.Next, got.More())
	}
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix the flaky test in ENG-7."},
		{kind: executor.EntryModel, text: "Running the tests first."},
		{kind: executor.EntryToolCall, tool: "Bash",
			text: `{"command":"go test ./cache/...","description":"run the tests"}`},
		{kind: executor.EntryToolResult, tool: "Bash", failed: true, text: redacted},
		{kind: executor.EntrySummary, text: "This session is being continued. The test races on the cache."},
		{kind: executor.EntryUnread,
			text: "a line of 12 bytes of the harness's record is not a record, and was not read"},
		{kind: executor.EntryToolCall, tool: "Task",
			text: `{"description":"find the race","prompt":"Look for the race."}`},
		{kind: executor.EntryToolResult, tool: "Task", text: "The cache map is written unlocked."},
		{kind: executor.EntryModel, text: "Fixed: the cache takes its lock."},
		{kind: executor.EntryModel, text: strings.Repeat("the account ", 180)},
		{kind: executor.EntryBrief, thread: "agent-a1", text: "Look for the race."},
		{kind: executor.EntryModel, thread: "agent-a1", text: "The cache map is written unlocked."},
	})
	for _, e := range got.Entries {
		if strings.Contains(e.Text, plantedToken) {
			t.Errorf("an entry hands out the token a tool result quoted: %q", e.Text)
		}
	}
	if len(got.Entries) > 0 {
		if want := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC); !got.Entries[0].At.Equal(want) {
			t.Errorf("the brief is dated %s, want the instant the harness recorded, %s",
				got.Entries[0].At, want)
		}
	}
	if len(got.Entries) > 3 && got.Entries[3].Bytes != len(failure) {
		t.Errorf("the redacted result states %d bytes, want the %d the harness recorded",
			got.Entries[3].Bytes, len(failure))
	}
}

// openCodeExportDoc is the export the fake opencode prints for a run: the
// brief, a step with the model's reasoning, its words, a call that completed
// and one that failed, a compaction's summary, and a person's next message —
// whose `summary` is an object, which is no summary.
func openCodeExportDoc() string {
	return `{"info":{"id":"ses_0123456789abcdef0123456789abcdef","title":"crewlet","time":{"created":1793361600000}},` +
		`"messages":[` +
		`{"info":{"id":"msg_1","role":"user","time":{"created":1793361600000}},` +
		`"parts":[{"id":"prt_1","type":"text","text":"Fix the flaky test in ENG-7."}]},` +
		`{"info":{"id":"msg_2","role":"assistant","time":{"created":1793361601000}},"parts":[` +
		`{"type":"step-start"},{"type":"reasoning","text":"the race is in the cache"},` +
		`{"type":"text","text":"Running the tests first."},` +
		`{"type":"tool","callID":"c1","tool":"bash","state":{"status":"completed",` +
		`"input":{"command":"go test ./cache/..."},"output":"ok cache 0.2s",` +
		`"time":{"start":1793361602000,"end":1793361603000}}},` +
		`{"type":"tool","callID":"c2","tool":"read","state":{"status":"error",` +
		`"input":{"filePath":"/work/missing.go"},"error":"no such file",` +
		`"time":{"start":1793361604000,"end":1793361604500}}},` +
		`{"type":"step-finish","tokens":{"input":10}}]},` +
		`{"info":{"id":"msg_3","role":"assistant","summary":true,"time":{"created":1793361605000}},` +
		`"parts":[{"type":"text","text":"The test races on the cache; the lock is taken now."}]},` +
		`{"info":{"id":"msg_4","role":"user","summary":{"title":"Fix","diffs":[]},` +
		`"time":{"created":1793361606000}},"parts":[{"type":"text","text":"Carry on."}]}` +
		`]}`
}

// AN OPENCODE SESSION READS AS THE TRANSCRIPT ITS EXPORT HOLDS. The export the
// run's script packed reads a message at a time: a person's text as the
// brief, the model's as its words, a tool part as its call and — once it
// settled — its result, a failure marked, and a compaction's message as the
// summary; the reasoning and the steps' bookkeeping are no entry, and a
// person's message whose `summary` is an object is still a brief. A call is
// dated when it started and its result when it ended.
//
// Mutation: read a user message's summary object as a summary, drop a tool
// part's result, or read the reasoning, and the case goes red.
func TestAnOpenCodeSessionReadsAsTheTranscriptItsExportHolds(t *testing.T) {
	t.Parallel()
	got := readOpenCode(t, openCodeCaptureOf(t, openCodeExportDoc()), executor.TranscriptReadBounds())
	if got.Cut != "" || got.More() {
		t.Errorf("the read is cut %q, want the whole transcript", got.Cut)
	}
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix the flaky test in ENG-7."},
		{kind: executor.EntryModel, text: "Running the tests first."},
		{kind: executor.EntryToolCall, tool: "bash", text: `{"command":"go test ./cache/..."}`},
		{kind: executor.EntryToolResult, tool: "bash", text: "ok cache 0.2s"},
		{kind: executor.EntryToolCall, tool: "read", text: `{"filePath":"/work/missing.go"}`},
		{kind: executor.EntryToolResult, tool: "read", failed: true, text: "no such file"},
		{kind: executor.EntrySummary, text: "The test races on the cache; the lock is taken now."},
		{kind: executor.EntryBrief, text: "Carry on."},
	})
	if len(got.Entries) >= 4 {
		started, ended := time.UnixMilli(1793361602000).UTC(), time.UnixMilli(1793361603000).UTC()
		if !got.Entries[2].At.Equal(started) || !got.Entries[3].At.Equal(ended) {
			t.Errorf("the call is dated %s and its result %s, want %s and %s",
				got.Entries[2].At, got.Entries[3].At, started, ended)
		}
	}
}

// AN INSTANT AN ANSWER CANNOT CARRY IS NO TIME. An answer writes an entry's
// instant in RFC 3339, years 0 to 9999, and a box's own agent can write any
// instant into what its run packs: Claude Code's year 0000 an hour east of
// UTC, which is the year before it in UTC, or 9999's last second an hour west,
// which is 10000, and an export's milliseconds of the year 10000. Each entry
// reads with no time, beside an instant in range that reads as recorded — and
// the page encodes, which with any of them it did not.
//
// Mutation: take any instant the parser does, and the page fails to encode.
func TestAnInstantAnAnswerCannotCarryIsNoTime(t *testing.T) {
	t.Parallel()
	recorded := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	_, claude := claudeCaptureOf(t, []string{
		`{"type":"user","message":{"role":"user","content":"before the year 0"},"timestamp":"0000-01-01T00:00:00+01:00"}`,
		`{"type":"user","message":{"role":"user","content":"past the year 9999"},"timestamp":"9999-12-31T23:59:59-01:00"}`,
		`{"type":"user","message":{"role":"user","content":"in range"},"timestamp":"2026-10-30T12:00:00.000Z"}`,
	}, nil)
	export := `{"info":{},"messages":[` +
		`{"info":{"role":"user","time":{"created":253402300800000}},"parts":[{"type":"text","text":"past the year 9999"}]},` +
		`{"info":{"role":"user","time":{"created":` + strconv.FormatInt(recorded.UnixMilli(), 10) + `}},` +
		`"parts":[{"type":"text","text":"in range"}]}]}`
	for name, got := range map[string]executor.Transcript{
		"claude-code": readClaude(t, claude, executor.TranscriptReadBounds()),
		"opencode":    readOpenCode(t, openCodeCaptureOf(t, export), executor.TranscriptReadBounds()),
	} {
		if len(got.Entries) == 0 {
			t.Errorf("%s read no entries", name)
			continue
		}
		last := len(got.Entries) - 1
		for _, e := range got.Entries[:last] {
			if !e.At.IsZero() {
				t.Errorf("%s: %q is dated %s, which no answer can carry", name, e.Text, e.At)
			}
		}
		if e := got.Entries[last]; e.Text != "in range" || !e.At.Equal(recorded) {
			t.Errorf("%s: the last entry reads %q dated %s, want %q at %s", name, e.Text, e.At,
				"in range", recorded)
		}
		if _, err := json.Marshal(got.Entries); err != nil {
			t.Errorf("%s: the page does not encode: %v", name, err)
		}
	}
}

// note is a note as the engine renders one for a running turn: its heading,
// naming who sent it, its framing line, and the words.
func note(who, words string) string {
	return "## A note from " + who + ", sent while this turn was running\n" + who +
		" sent this through Crewlet while the work was under way.\n\n" + words
}

// jsonText is text as a JSON string, quotes included.
func jsonText(t *testing.T, text string) string {
	t.Helper()
	raw, err := json.Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A NOTE A PERSON SENT THE RUN READS AS A NOTE, WHEREVER ITS HARNESS FILED IT.
// Claude Code hands a note to its model as a hook's additional context, and
// keeps it in a record of its own: whichever record, under whichever field,
// a note there reads as a note in its place — framed in a system reminder or
// not, two of one step's notes as two — and the line the run's own hook
// printed as it handed them over, which the live transcript shows as
// `[steer]`, reads as a note saying whose reached the run. OpenCode admits a
// note as a person's message, and it reads as a note rather than a brief. A
// brief that carries a note further down, as a launch's brief carries the
// notes that arrived before it, is still the brief, and a tool's result
// quoting a note's heading is still the tool's: text a tool produced is never
// a note.
//
// Mutation: skip the records the harness wrote for itself, read a note by its
// record's shape alone, drop the hook's line, take a heading anywhere in a
// brief for a note, or read OpenCode's note as a brief, and a row goes red.
func TestANoteAPersonSentTheRunReadsAsANote(t *testing.T) {
	t.Parallel()
	batch := note("ada", "Target the release branch.") + "\n\n" + note("cy", "And keep the old flag.")
	stop := "<system-reminder>\nStop hook additional context: " + note("bob", "Stop after the fix.") +
		"\n</system-reminder>"
	quoted := "notes.md: " + note("eve", "not a note") // a file a tool read
	record := []string{
		`{"type":"user","message":{"role":"user","content":` +
			jsonText(t, "Fix the flake.\n\n"+note("ada", "use main")) + `},"timestamp":"2026-10-30T12:00:00.000Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"notes.md"}}]},"timestamp":"2026-10-30T12:00:01.000Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` +
			jsonText(t, quoted) + `}]},"timestamp":"2026-10-30T12:00:02.000Z"}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","hookEvent":"PostToolBatch","content":[` +
			jsonText(t, batch) + `]},"timestamp":"2026-10-30T12:00:03.000Z"}`,
		`{"type":"system","subtype":"informational","content":"PostToolBatch says: crewlet: 2 notes from ada, cy reached the run","timestamp":"2026-10-30T12:00:03.100Z"}`,
		`{"type":"user","isMeta":true,"message":{"role":"user","content":` + jsonText(t, stop) +
			`},"timestamp":"2026-10-30T12:00:04.000Z"}`,
	}
	described, raw := claudeCaptureOf(t, record, nil)
	got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix the flake.\n\n" + note("ada", "use main")},
		{kind: executor.EntryToolCall, tool: "Read", text: `{"file_path":"notes.md"}`},
		{kind: executor.EntryToolResult, tool: "Read", text: quoted},
		{kind: executor.EntryNote, text: note("ada", "Target the release branch.")},
		{kind: executor.EntryNote, text: note("cy", "And keep the old flag.")},
		{kind: executor.EntryNote, text: "2 notes from ada, cy reached the run"},
		{kind: executor.EntryNote, text: note("bob", "Stop after the fix.")},
	})
	if len(got.Entries) == 7 {
		if want := time.Date(2026, 10, 30, 12, 0, 4, 0, time.UTC); !got.Entries[6].At.Equal(want) {
			t.Errorf("the note at the finish is dated %s, want its record's %s", got.Entries[6].At, want)
		}
	}

	export := `{"info":{},"messages":[` +
		`{"info":{"role":"user"},"parts":[{"type":"text","text":"Fix the flake."}]},` +
		`{"info":{"role":"user"},"parts":[{"type":"text","text":` + jsonText(t, note("ada", "Target main.")) + `}]}` +
		`]}`
	open := readOpenCode(t, openCodeCaptureOf(t, export), executor.TranscriptReadBounds())
	sameEntries(t, open.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix the flake."},
		{kind: executor.EntryNote, text: note("ada", "Target main.")},
	})
}

// THE LINE THE RUN'S HOOK PRINTED IS A NOTE AS THE HARNESS RECORDED IT. The
// line saying whose notes reached the run names every sender, and five run
// past the hundred and sixty bytes the live transcript shows of a line: its
// entry is the line whole, its size its own and nothing cut, with nothing the
// harness did not write after it — and a token it quotes is redacted, as every
// entry's text is.
//
// Mutation: read the hook's line as the live transcript renders it, and the
// entry is cut short of its senders and marked as whole, or carries a count of
// lines nobody wrote.
func TestTheLineTheRunsHookPrintedIsANoteAsTheHarnessRecordedIt(t *testing.T) {
	t.Parallel()
	senders := "ada.lovelace-analytical-engine, grace.hopper-compilers, " +
		"margaret.hamilton-flight-software, katherine.johnson-orbits, dorothy.vaughan-fortran"
	line := "5 notes from " + senders + " reached the run"
	if len(line) <= 160 {
		t.Fatalf("the hook's line is %d bytes, and has to run past the live view's 160", len(line))
	}
	quoted := "1 note from eve reached the run with " + plantedToken
	record := []string{
		`{"type":"system","subtype":"informational","content":` +
			jsonText(t, "PostToolBatch says: crewlet: "+line+"\nand the hook went on") +
			`,"timestamp":"2026-10-30T12:00:03.100Z"}`,
		`{"type":"system","subtype":"informational","content":` +
			jsonText(t, "Stop says: crewlet: "+quoted) + `,"timestamp":"2026-10-30T12:00:04.100Z"}`,
	}
	described, raw := claudeCaptureOf(t, record, nil)
	got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryNote, text: line},
		{kind: executor.EntryNote, text: redact.Secrets(quoted)},
	})
	if len(got.Entries) > 1 {
		if e := got.Entries[0]; e.Bytes != len(line) || e.Cut {
			t.Errorf("the hook's line states %d bytes, cut %v; want its own %d, whole", e.Bytes, e.Cut,
				len(line))
		}
		if e := got.Entries[1]; strings.Contains(e.Text, plantedToken) || e.Bytes != len(quoted) {
			t.Errorf("the hook's line quoting a token reads %q stating %d bytes, want it redacted, "+
				"stating the %d the harness recorded", e.Text, e.Bytes, len(quoted))
		}
	}
}

// A RECORD OF NO SHAPE THIS READ KNOWS IS SAID, NEVER READ AS SILENCE. A line
// of Claude Code's record that is JSON but no object — an array, a number — is
// no record; a person's or the model's record whose message is no object, or
// whose content is neither text nor a list, has no shape this read knows; and
// a message of an OpenCode export that is no object, or whose parts are no
// list, is said too. Each is an entry where it was. A field a release retyped
// inside a record is still skipped, the record read.
//
// Mutation: tolerate a record that is no object, a message or content of
// another shape, or parts that are no list, and the entry vanishes; refuse a
// retyped field and the record that held it does.
func TestARecordOfNoShapeThisReadKnowsIsSaid(t *testing.T) {
	t.Parallel()
	described, raw := claudeCaptureOf(t, []string{
		`[1]`,
		`42`,
		`{"type":"user","message":"Fix it."}`,
		`{"type":"assistant","message":{"content":{"text":"an object"}}}`,
		`{"type":"assistant","message":{"content":7}}`,
		`{"type":"user","message":{"content":null}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"still read"}]},"timestamp":5}`,
	}, nil)
	got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	unshaped := "a message of the harness's record has no shape this read knows, and was not read"
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryUnread, text: "a line of 3 bytes of the harness's record is not a record, and was not read"},
		{kind: executor.EntryUnread, text: "a line of 2 bytes of the harness's record is not a record, and was not read"},
		{kind: executor.EntryUnread, text: unshaped},
		{kind: executor.EntryUnread, text: unshaped},
		{kind: executor.EntryUnread, text: unshaped},
		{kind: executor.EntryModel, text: "still read"},
	})

	export := `{"info":{},"messages":["not a message",` +
		`{"info":{"role":"user"},"parts":{"type":"text","text":"parts of an object"}},` +
		`{"info":{"role":"user"},"parts":[{"type":"text","text":"read"}]},` +
		`{"info":{"role":"assistant","time":{"created":"soon"}},"parts":[{"type":"text","text":"retyped"}]}]}`
	open := readOpenCode(t, openCodeCaptureOf(t, export), executor.TranscriptReadBounds())
	sameEntries(t, open.Entries, []shown{
		{kind: executor.EntryUnread, text: "a message of the export is not a message, and was not read"},
		{kind: executor.EntryUnread, text: "a message of the export whose parts are not a list was not read"},
		{kind: executor.EntryBrief, text: "read"},
		{kind: executor.EntryModel, text: "retyped"},
	})
}

// briefs is an export of n person's messages, `message 0` to `message n-1`.
func briefs(n int) string {
	var b strings.Builder
	b.WriteString(`{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`)
	for i := range n {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"info":{"role":"user","time":{"created":%d}},"parts":[{"type":"text","text":"message %d"}]}`,
			1793361600000+int64(i)*1000, i)
	}
	b.WriteString("]}")
	return b.String()
}

// within is the engine's bounds with the given ones lowered.
func within(change func(*executor.TranscriptBounds)) executor.TranscriptBounds {
	bounds := executor.TranscriptReadBounds()
	change(&bounds)
	return bounds
}

// pageAll reads raw a page at a time under bounds, from the first entry until
// a page says nothing follows, and answers every page.
func pageAll(t *testing.T, raw []byte, bounds executor.TranscriptBounds) []executor.Transcript {
	t.Helper()
	var pages []executor.Transcript
	for from := 0; ; {
		page, err := executor.ReadTranscriptWithin(t.Context(), executor.OpenCodeName,
			executor.OpenCodeArchiveFormat, opener(bytes.NewReader(raw)), from, bounds)
		if err != nil {
			t.Fatalf("reading from %d: %v", from, err)
		}
		pages = append(pages, page)
		if !page.More() {
			return pages
		}
		if page.Next <= from || len(pages) > 20 {
			t.Fatalf("a page from %d says more follows from %d: the reading would never end",
				from, page.Next)
		}
		from = page.Next
	}
}

// A TRANSCRIPT IS READ A PAGE AT A TIME, WITH NOTHING LOST BETWEEN PAGES. A
// read holds at most the bound's entries and the bound's bytes of text, says
// which bound it stopped at and where the next read starts, and the pages read
// from each other's ends are the whole transcript in order, each entry once;
// the last says nothing follows.
//
// Mutation: start a page at the entry after Next, count the entries before the
// page into its bound, or keep the entry that went past the bytes, and the
// pages lose, repeat or overrun an entry.
func TestATranscriptIsReadAPageAtATimeWithNothingLostBetweenPages(t *testing.T) {
	t.Parallel()
	raw := openCodeCaptureOf(t, briefs(7))
	var want []string
	for i := range 7 {
		want = append(want, fmt.Sprintf("message %d", i))
	}
	for name, c := range map[string]struct {
		bounds executor.TranscriptBounds
		cut    executor.TranscriptCut
		sizes  []int
	}{
		"three entries a page": {within(func(b *executor.TranscriptBounds) { b.Entries = 3 }),
			executor.CutEntries, []int{3, 3, 1}},
		// "message n" is nine bytes, so two fit in twenty and a third does not.
		"twenty bytes a page": {within(func(b *executor.TranscriptBounds) { b.Bytes = 20 }),
			executor.CutBytes, []int{2, 2, 2, 1}},
	} {
		t.Run(name, func(t *testing.T) {
			pages := pageAll(t, raw, c.bounds)
			var read []string
			var sizes []int
			for i, page := range pages {
				sizes = append(sizes, len(page.Entries))
				for _, e := range page.Entries {
					read = append(read, e.Text)
				}
				last := i == len(pages)-1
				if (page.Cut == "") != last || (!last && page.Cut != c.cut) {
					t.Errorf("page %d is cut %q, want %q on every page but the last", i, page.Cut, c.cut)
				}
			}
			if !slices.Equal(read, want) {
				t.Errorf("the pages read %q, want %q", read, want)
			}
			if !slices.Equal(sizes, c.sizes) {
				t.Errorf("the pages held %v entries, want %v", sizes, c.sizes)
			}
		})
	}
}

// AN ENTRY IS REDACTED BEFORE IT IS CUT. A tool's output longer than an
// entry's bound, a token straddling the bound, reads cut to the bound and
// marked, its whole size stated — and with no part of the token in it: cut
// first, the token's opening would no longer be a token the redaction knows.
// And a private key whose body begins inside the bound and whose END is many
// KiB past it — a line of prose straight after its BEGIN, so the body inside
// the bound is reached only by reading back from the END — is read whole and
// redacted as one: what is redacted reaches a key block's length past the
// bound.
//
// Mutation: cut the text before redacting it, redact only the bound's own
// bytes of a long text, or read a window short of a key block past the bound,
// and the opening of the token or the key's body is handed out.
func TestAnEntryIsRedactedBeforeItIsCut(t *testing.T) {
	t.Parallel()
	small := within(func(b *executor.TranscriptBounds) { b.EntryBytes = 64 })
	tool := func(output string) string {
		return `{"info":{},"messages":[{"info":{"role":"assistant"},"parts":[{"type":"tool","tool":"bash",` +
			`"state":{"status":"completed","input":{},"output":` + jsonText(t, output) + `}}]}]}`
	}
	output := strings.Repeat("x", 50) + " " + plantedToken + " the rest of the output"
	got := readOpenCode(t, openCodeCaptureOf(t, tool(output)), small)
	if len(got.Entries) != 2 {
		t.Fatalf("the transcript has %d entries, want the call and its result: %+v",
			len(got.Entries), got.Entries)
	}
	result := got.Entries[1]
	if !result.Cut || len(result.Text) > 64 || result.Bytes != len(output) {
		t.Errorf("the result is %d bytes cut %v stating %d, want at most 64, cut, stating %d",
			len(result.Text), result.Cut, result.Bytes, len(output))
	}
	if strings.Contains(result.Text, "ghp_") {
		t.Errorf("the cut result hands out the token's opening: %q", result.Text)
	}
	if !strings.HasPrefix(result.Text, strings.Repeat("x", 50)+" "+redact.Marker) {
		t.Errorf("the cut result is %q, want the output up to the token's redaction", result.Text)
	}

	// THE KEY'S BODY BEGINS INSIDE THE BOUND, and its END — the one thing
	// that reaches the body, past the prose that ends the BEGIN's own block
	// — is two hundred lines past it, about 13 KiB: further than any window
	// short of a key block reads.
	body := strings.Repeat("MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n", 200)
	key := "-----BEGIN RSA PRIVATE KEY-----\nsee below\n" + body + "-----END RSA PRIVATE KEY-----\ndone\n"
	got = readOpenCode(t, openCodeCaptureOf(t, tool(key)), small)
	if len(got.Entries) != 2 {
		t.Fatalf("the transcript has %d entries, want the call and its result", len(got.Entries))
	}
	result = got.Entries[1]
	if strings.Contains(result.Text, "MIIEow") || !result.Cut || result.Bytes != len(key) {
		t.Errorf("the key's result reads %q (cut %v, %d bytes), want it cut with the key's body "+
			"redacted", result.Text, result.Cut, result.Bytes)
	}
	if !strings.Contains(result.Text, redact.Marker) {
		t.Errorf("the key's result is %q, want the output up to the key's redaction", result.Text)
	}
}

// A READ STOPS AT ITS BUDGET AND SAYS SO. An archive whose decompressed bytes
// run past the read budget hands out the entries read within it and says the
// read was cut there, with nothing a later read could add: a read from where it
// stopped reaches the same budget first.
//
// Mutation: read the archive to its end whatever it inflates to, or call the
// budget's end the transcript's, and the read goes on, or says nothing was cut.
func TestAReadStopsAtItsBudgetAndSaysSo(t *testing.T) {
	t.Parallel()
	raw := openCodeCaptureOf(t, briefs(50))
	bounds := within(func(b *executor.TranscriptBounds) { b.ReadBytes = 1 << 10 })
	first := readOpenCode(t, raw, bounds)
	if first.Cut != executor.CutRead || first.More() || len(first.Entries) == 0 || len(first.Entries) >= 50 {
		t.Fatalf("the read held %d entries cut %q (more %v), want some of the fifty, cut at the budget",
			len(first.Entries), first.Cut, first.More())
	}
	again, err := executor.ReadTranscriptWithin(t.Context(), executor.OpenCodeName,
		executor.OpenCodeArchiveFormat, opener(bytes.NewReader(raw)), first.Next, bounds)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin from %d: %v", first.Next, err)
	}
	if len(again.Entries) != 0 || again.Cut != executor.CutRead {
		t.Errorf("a read from where the budget stopped one held %d entries cut %q, want none, "+
			"cut at the same budget", len(again.Entries), again.Cut)
	}
}

// A READ CUT BY ITS BUDGET HANDS OUT A PREFIX OF THE TRANSCRIPT. Wherever the
// budget falls — between two lines of Claude Code's record or inside one — the
// entries a cut read hands out are the whole read's, in its order, and none it
// does not have: a line the budget cut in two is not a whole line, and read as
// one it would be "a line that is no record", said where a valid one was.
//
// Mutation: hand the line a failed read cut short to the record's reader, and
// a cut read ends on an entry the whole read does not have.
func TestAReadCutByItsBudgetHandsOutAPrefixOfTheTranscript(t *testing.T) {
	t.Parallel()
	_, raw := claudeCaptureOf(t, claudeSessionRecord, claudeSidecar)
	whole := readClaude(t, raw, executor.TranscriptReadBounds())
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	inflated, err := io.Copy(io.Discard, zr)
	if err != nil {
		t.Fatal(err)
	}
	cuts := 0
	for budget := int64(512); budget < inflated; budget += 97 {
		got := readClaude(t, raw, within(func(b *executor.TranscriptBounds) { b.ReadBytes = budget }))
		switch got.Cut {
		case executor.CutRead:
			cuts++
		case "":
		default:
			t.Fatalf("a read of %d bytes is cut %q", budget, got.Cut)
		}
		if len(got.Entries) > len(whole.Entries) ||
			!slices.Equal(shownOf(got.Entries), shownOf(whole.Entries[:len(got.Entries)])) {
			t.Fatalf("a read of %d bytes hands out %+v, which is not the start of the whole read's %+v",
				budget, shownOf(got.Entries), shownOf(whole.Entries))
		}
	}
	if cuts == 0 {
		t.Fatal("no budget cut the read, so this proves nothing about a cut one")
	}
}

// tarBlock is one header block of a ustar archive naming name, of typeflag,
// with size bytes of data after it.
func tarBlock(name string, typeflag byte, size int) []byte {
	var b [512]byte
	copy(b[0:100], name)
	copy(b[100:108], "0000644\x00")
	copy(b[108:116], "0000000\x00")
	copy(b[116:124], "0000000\x00")
	copy(b[124:136], fmt.Sprintf("%011o\x00", size))
	copy(b[136:148], fmt.Sprintf("%011o\x00", 0))
	b[156] = typeflag
	copy(b[257:263], "ustar\x00")
	copy(b[263:265], "00")
	// THE CHECKSUM IS OF THE BLOCK with its own field read as spaces.
	copy(b[148:156], "        ")
	sum := 0
	for _, c := range b {
		sum += int(c)
	}
	copy(b[148:156], fmt.Sprintf("%06o\x00 ", sum))
	return b[:]
}

// paxRecord is one record of a PAX extended header: its whole length in
// decimal, the length's own digits included, then `key=value` and a newline.
func paxRecord(key, value string) string {
	body := " " + key + "=" + value + "\n"
	n := len(body) + 1
	for len(strconv.Itoa(n))+len(body) != n {
		n = len(strconv.Itoa(n)) + len(body)
	}
	return strconv.Itoa(n) + body
}

// padded is data and the zeros that fill out its last block.
func padded(data []byte) []byte {
	if rem := len(data) % 512; rem != 0 {
		data = append(data, make([]byte, 512-rem)...)
	}
	return data
}

// sparseBomb is a gzipped tar whose one member is named as a session's
// transcript and is a GNU sparse file (PAX format 1.0) declaring size bytes,
// every one of them a hole: a few blocks of archive that hand their reader
// size bytes the archive does not hold.
//
// BUILT BY HAND: no pack makes one — a box's tar archives the files a harness
// wrote, and sparsely only when asked to — and archive/tar's writer drops the
// records that make one. It is what a box's own agent could leave in place of
// what its run packed.
func sparseBomb(t *testing.T, size int64) []byte {
	t.Helper()
	name := "projects/" + executor.ClaudeProjectDir + "/s.jsonl"
	pax := paxRecord("GNU.sparse.major", "1") + paxRecord("GNU.sparse.minor", "0") +
		paxRecord("GNU.sparse.name", name) + paxRecord("GNU.sparse.realsize", strconv.FormatInt(size, 10))
	var raw bytes.Buffer
	raw.Write(tarBlock("PaxHeaders/s.jsonl", 'x', len(pax)))
	raw.Write(padded([]byte(pax)))
	// THE MEMBER'S DATA IS ITS SPARSE MAP, and the map names no data: one
	// block saying it has no entries, so the whole of its size is a hole.
	raw.Write(tarBlock("GNUSparseFile.0/s.jsonl", '0', 512))
	raw.Write(padded([]byte("0\n")))
	raw.Write(make([]byte, 1024))
	return gzipped(t, raw.String())
}

// A MEMBER HANDS OUT NO MORE THAN THE READ BUDGET, WHATEVER IT DECLARES. A
// sparse member named as the transcript declares an exbibyte its archive does
// not hold; what it hands the reader is held to the read budget apart from what
// the archive inflates to — which it never reaches — so the read stops at the
// budget and says so, as an archive that inflates past it does.
//
// Mutation: meter only what the archive inflates to, and the read counts the
// member's holes for as long as it declares them: it never ends.
func TestAMemberHandsOutNoMoreThanTheReadBudget(t *testing.T) {
	t.Parallel()
	got := readClaude(t, sparseBomb(t, 1<<62), within(func(b *executor.TranscriptBounds) {
		b.ReadBytes = 1 << 20
	}))
	if got.Cut != executor.CutRead || len(got.Entries) != 0 {
		t.Errorf("the sparse member read as %+v cut %q, want nothing, cut at the budget",
			shownOf(got.Entries), got.Cut)
	}
}

// A TRANSCRIPT THAT IS NO PLAIN FILE IS SAID, NOT READ THROUGH. A box's tar
// packs a link as a link: a session's transcript that is a symbolic link names
// a path the archive does not hold, and a subagent's that is a hard link to the
// session's own names another member — neither is the record, so each is an
// entry saying so, where the transcript was, and the session's own record is
// read as ever.
//
// Mutation: read every member named like a transcript whatever it is, or skip
// the ones that are no file silently, and a row goes red.
func TestATranscriptThatIsNoPlainFileIsSaid(t *testing.T) {
	t.Parallel()
	notAFile := "a transcript of the harness's record is not a plain file in the archive, and was not read"
	brief := `{"type":"user","message":{"role":"user","content":"Fix it."}}`

	described, raw := claudeCaptureWith(t, standInHead+`mkdir -p "$p"
cat > "$CLAUDE_CONFIG_DIR/outside.jsonl" <<'TRANSCRIPT'
`+brief+`
TRANSCRIPT
ln -s ../../outside.jsonl "$p/$s.jsonl"
`+standInEnd)
	got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	sameEntries(t, got.Entries, []shown{{kind: executor.EntryUnread, text: notAFile}})

	described, raw = claudeCaptureWith(t, standInHead+`mkdir -p "$p/$s/subagents"
cat > "$p/$s.jsonl" <<'TRANSCRIPT'
`+brief+`
TRANSCRIPT
ln "$p/$s.jsonl" "$p/$s/subagents/agent-a1.jsonl"
`+standInEnd)
	got, err = executor.ReadTranscript(t.Context(), described.Harness, described.Format,
		opener(bytes.NewReader(raw)), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	sameEntries(t, got.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix it."},
		{kind: executor.EntryUnread, thread: "agent-a1", text: notAFile},
	})
}

// A PART OF THE RECORD PAST THE BOUND IS SAID WHERE IT WAS. A line of Claude
// Code's record past the bound one line may hold reads as an entry saying so,
// in its place, and the lines after it are read; a message of an OpenCode
// export past the same bound is said too, and ends the read there, cut —
// nothing after a value too large to hold can be found.
//
// Mutation: drop the line past the bound silently, or carry on past an
// oversized message, and the gap reads as continuity or the read misreads what
// follows.
func TestAPartOfTheRecordPastTheBoundIsSaidWhereItWas(t *testing.T) {
	t.Parallel()
	_, raw := claudeCaptureOf(t, claudeSessionRecord, claudeSidecar)
	small := within(func(b *executor.TranscriptBounds) { b.LineBytes = 1 << 10 })
	got := readClaude(t, raw, small)
	kinds := make([]executor.EntryKind, len(got.Entries))
	for i, e := range got.Entries {
		kinds[i] = e.Kind
	}
	if len(got.Entries) != 12 || got.Entries[9].Kind != executor.EntryUnread ||
		!strings.Contains(got.Entries[9].Text, "past the 1 KiB one line may hold") ||
		got.Entries[10].Thread != "agent-a1" || got.Cut != "" {
		t.Errorf("the long line read as %v (cut %q), want an unread entry in its place and the "+
			"subagent's transcript after it", kinds, got.Cut)
	}

	export := `{"info":{},"messages":[` +
		`{"info":{"role":"user"},"parts":[{"type":"text","text":"first"}]},` +
		`{"info":{"role":"assistant"},"parts":[{"type":"text","text":"` + strings.Repeat("y", 4<<10) + `"}]},` +
		`{"info":{"role":"user"},"parts":[{"type":"text","text":"after"}]}]}`
	open := readOpenCode(t, openCodeCaptureOf(t, export), small)
	if len(open.Entries) != 2 || open.Entries[0].Text != "first" ||
		open.Entries[1].Kind != executor.EntryUnread || open.Cut != executor.CutRead {
		t.Errorf("the export read as %+v cut %q, want the first message, then the oversized one "+
			"said, cut where it was", shownOf(open.Entries), open.Cut)
	}
}

// failingReader hands out its bytes and then fails as a store that stopped
// answering does.
type failingReader struct {
	data []byte
	err  error
}

func (f *failingReader) Read(p []byte) (int, error) {
	if len(f.data) == 0 {
		return 0, f.err
	}
	n := copy(p, f.data)
	f.data = f.data[n:]
	return n, nil
}

// AN ARCHIVE IS BLAMED ONLY FOR ITS OWN FAULTS. A harness or a format this
// build does not pack is refused as unsupported before anything is opened; an
// archive that is not what its format describes is a malformed one; and a
// source that fails — an object the store no longer holds as it is opened, one
// it stopped answering for part way, a seal that no longer opens — is that
// failure, wrapped, never a malformed archive, whatever the decompression made
// of the stream stopping.
//
// Mutation: report the source's failure as a malformed archive, open the
// archive before its format is known to be read, or read an unknown format as
// one this build packs, and a row goes red.
func TestAnArchiveIsBlamedOnlyForItsOwnFaults(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		harness string
		format  int
	}{{"codex", 1}, {executor.ClaudeCodeName, executor.ClaudeArchiveFormat + 1}} {
		_, err := executor.ReadTranscript(t.Context(), c.harness, c.format,
			neverOpened(t, "an archive of a format this build does not read"), 0)
		if !errors.Is(err, executor.ErrUnsupportedArchive) {
			t.Errorf("%s's format %d: %v, want ErrUnsupportedArchive", c.harness, c.format, err)
		}
	}
	gone := errors.New("the store no longer holds the object")
	_, err := executor.ReadTranscript(t.Context(), executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		func(context.Context) (io.ReadCloser, error) { return nil, gone }, 0)
	if !errors.Is(err, gone) || errors.Is(err, executor.ErrMalformedArchive) {
		t.Errorf("a source that did not open: %v, want its own failure and no malformed archive", err)
	}
	stopped := errors.New("the store stopped answering")
	packed := gzipped(t, briefs(20))
	_, err = executor.ReadTranscript(t.Context(), executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		opener(&failingReader{data: packed[:len(packed)/2], err: stopped}), 0)
	if !errors.Is(err, stopped) || errors.Is(err, executor.ErrMalformedArchive) {
		t.Errorf("a source that failed part way: %v, want its own failure and no malformed archive", err)
	}
}

// A MALFORMED ARCHIVE IS SAID IN THIS PACKAGE'S WORDS ALONE. Each way an
// archive is not one its format describes — no gzip, compressed bytes that do
// not inflate, a stream cut short, a gzip of no tar, an export that is no
// export document — is a malformed archive whose reason is the one sentence
// this package writes for that way, and nothing the archive holds reaches the
// reason or the error: an export whose document is a string planted with a
// token and a mebibyte of a box's words is said as no export document, and
// carries neither. An export that ends before it closes ends part way through
// wherever it was cut — in its list's opening, after a whole message, after
// the comma before the next — whatever the JSON decoder calls the end it met.
//
// Mutation: say a malformed archive in the decoder's words, or name the token
// the export held where an object belonged, and the token, or the mebibyte,
// reaches the reason; read every syntax error an export's walk meets as no
// export document, and an export cut between its messages is said as one.
func TestAMalformedArchiveIsSaidInThisPackagesWordsAlone(t *testing.T) {
	t.Parallel()
	_, capture := claudeCaptureOf(t, claudeSessionRecord, nil)
	planted := plantedToken + strings.Repeat(" the box's words", 64<<10)
	for _, c := range []struct {
		name    string
		harness string
		format  int
		raw     []byte
		why     string
	}{
		{"bytes that are no gzip", executor.ClaudeCodeName, executor.ClaudeArchiveFormat,
			[]byte("not an archive " + plantedToken), executor.WhyNotGzip},
		// A GZIP HEADER, then a deflate block of the reserved type, which no
		// encoder writes.
		{"compressed bytes that do not inflate", executor.ClaudeCodeName, executor.ClaudeArchiveFormat,
			[]byte{0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 0xff}, executor.WhyCorrupt},
		{"an archive cut short", executor.ClaudeCodeName, executor.ClaudeArchiveFormat,
			capture[:len(capture)/2], executor.WhyCutShort},
		{"a gzip of no tar", executor.ClaudeCodeName, executor.ClaudeArchiveFormat,
			gzipped(t, strings.Repeat("not a tar header "+plantedToken, 64)), executor.WhyNotTar},
		{"an export that is a string", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, jsonText(t, planted)), executor.WhyNotExport},
		{"an export whose messages are a string", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":`+jsonText(t, planted)+`}`), executor.WhyNotExport},
		{"an export cut short", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[`), executor.WhyCutShort},
		{"an export cut after a whole message", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[{"info":{"role":"user"},"parts":[]}`+"\n"), executor.WhyCutShort},
		{"an export cut after the comma before a message", executor.OpenCodeName,
			executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[{"info":{"role":"user"},"parts":[]},`), executor.WhyCutShort},
		{"an export cut inside a message", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[{"info":{"role":"us`), executor.WhyCutShort},
		{"an export whose list closes on a brace", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[}`), executor.WhyNotExport},
	} {
		_, err := executor.ReadTranscript(t.Context(), c.harness, c.format,
			opener(bytes.NewReader(c.raw)), 0)
		var malformed *executor.MalformedArchiveError
		if !errors.As(err, &malformed) || !errors.Is(err, executor.ErrMalformedArchive) {
			t.Errorf("%s: %v, want a malformed archive", c.name, err)
			continue
		}
		if malformed.Why != c.why {
			t.Errorf("%s is said %q, want %q", c.name, malformed.Why, c.why)
		}
		if said := err.Error(); strings.Contains(said, "ghp_") || strings.Contains(said, "box's words") {
			t.Errorf("%s hands out what the archive holds: %.200q", c.name, said)
		}
	}
}

// A READ STOPS WHEN ITS CALLER IS GONE. A read whose caller leaves as its
// archive opens stops at the next line or message it would read, with the
// context's own error — never a transcript, and never a malformed archive; one
// whose open fails as its caller leaves is answered as left, whatever the store
// made of the open stopping under it; and a read whose caller had left before
// it began opens nothing at all, a slot free or not.
//
// Mutation: drop the check between lines, and the read answers the whole
// transcript to a caller that left; answer a failed open as it failed whether
// or not the caller is still there, and the store's words reach a caller that
// left; take a slot for a caller already gone, and the read opens its archive
// for nobody — at random, as a select between a free slot and a done context
// picks, which is why that half asks thirty-two times.
func TestAReadStopsWhenItsCallerIsGone(t *testing.T) {
	t.Parallel()
	_, claude := claudeCaptureOf(t, claudeSessionRecord, nil)
	open := openCodeCaptureOf(t, briefs(3))
	for name, c := range map[string]struct {
		harness string
		format  int
		raw     []byte
	}{
		"claude-code": {executor.ClaudeCodeName, executor.ClaudeArchiveFormat, claude},
		"opencode":    {executor.OpenCodeName, executor.OpenCodeArchiveFormat, open},
	} {
		// THE CALLER LEAVES AS THE ARCHIVE OPENS: a caller gone before the
		// read began would have it open nothing (below), so only one who
		// leaves once the read is under way reaches the check between lines.
		gone, leave := context.WithCancel(t.Context())
		got, err := executor.ReadTranscript(gone, c.harness, c.format,
			func(context.Context) (io.ReadCloser, error) {
				leave()
				return io.NopCloser(bytes.NewReader(c.raw)), nil
			}, 0)
		if !errors.Is(err, context.Canceled) || errors.Is(err, executor.ErrMalformedArchive) {
			t.Errorf("%s read for a caller that left: (%d entries, %v), want the context's error",
				name, len(got.Entries), err)
		}
	}

	gone, leave := context.WithCancel(t.Context())
	broke := errors.New("the store's connection closed under the open")
	_, err := executor.ReadTranscript(gone, executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		func(context.Context) (io.ReadCloser, error) {
			leave()
			return nil, broke
		}, 0)
	if !errors.Is(err, context.Canceled) || errors.Is(err, broke) {
		t.Errorf("an open that failed as its caller left: %v, want the caller's own error", err)
	}

	left, cancel := context.WithCancel(t.Context())
	cancel()
	for i := 0; i < 32 && !t.Failed(); i++ {
		_, err := executor.ReadTranscript(left, executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			neverOpened(t, "its caller had left before the read began"), 0)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("a read whose caller had left before it began: %v, want the context's error", err)
		}
	}
}

// leavesAfter is a read's context whose caller leaves once the read has asked
// whether it is still there more than a given number of times: a caller who
// leaves part way through one line of the record, at a point the case names
// by the read's own checks rather than by a clock.
type leavesAfter struct {
	context.Context
	asks atomic.Int64
	gone chan struct{}
	once sync.Once
}

// leavingAfter is a context under parent whose caller leaves at the read's
// ask after the first asks.
func leavingAfter(parent context.Context, asks int64) *leavesAfter {
	c := &leavesAfter{Context: parent, gone: make(chan struct{})}
	c.asks.Store(asks)
	return c
}

// Err is nil for the first asks, and the caller's leaving after them.
func (c *leavesAfter) Err() error {
	if c.asks.Add(-1) >= 0 {
		return nil
	}
	c.once.Do(func() { close(c.gone) })
	return context.Canceled
}

// Done is closed once the caller has left.
func (c *leavesAfter) Done() <-chan struct{} { return c.gone }

// A READ STOPS BETWEEN THE NOTES AND THE VALUES ONE LINE HOLDS. One line of
// Claude Code's record runs to mebibytes, and a record the harness wrote for
// itself is searched value by value for the notes it carries: a value holding
// a note on each of a thousand lines, or a thousand values before the one that
// holds a note, is a line the read would otherwise walk to its end for a
// caller who left at its start. Each stops where the caller left, with the
// caller's own error, short of the page it would have filled.
//
// Mutation: ask the caller only between lines, or not between the notes one
// value carries, or not between a record's values, and the read walks on and
// answers a page to a caller who left.
func TestAReadStopsBetweenTheNotesAndTheValuesOneLineHolds(t *testing.T) {
	t.Parallel()
	var many []string
	for i := range 1000 {
		many = append(many, note(fmt.Sprintf("ana-%d", i), "Keep going."))
	}
	values := strings.Repeat(jsonText(t, "x")+",", 1000) + jsonText(t, note("bo", "Stop here."))
	for name, content := range map[string]string{
		"a note on each of a value's lines":     jsonText(t, strings.Join(many, "\n\n")),
		"a thousand values before the one note": values,
	} {
		_, raw := claudeCaptureOf(t, []string{
			`{"type":"attachment","attachment":{"type":"hook_additional_context","hookEvent":"PostToolBatch",` +
				`"content":[` + content + `]},"timestamp":"2026-10-30T12:00:03.000Z"}`,
		}, nil)
		got, err := executor.ReadTranscript(leavingAfter(t.Context(), 64), executor.ClaudeCodeName,
			executor.ClaudeArchiveFormat, opener(bytes.NewReader(raw)), 0)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s, read for a caller who left part way through the line: (%d entries, %v), "+
				"want the caller's own error", name, len(got.Entries), err)
		}
	}
}

// THE TRANSCRIPT'S BOUNDS ARE THE ENGINE'S OWN, and hold to what their values
// argue: a page holds sixty-four entries at the entry's bound, a line is read to
// the bound every read of a run's record takes, the read budget is eight times
// the most an archive is stored as — gzip's ratio over text — so an ordinary
// session at its bound reads whole, and the reads a process runs at once are at
// least two, which at their worst — each a line at its bound, as much again
// while the slice gathering it grows, and its decoded copy — come to about
// 200 MiB.
//
// Mutation: raise the entry's bound past a sixty-fourth of the page's, read
// lines to a bound of their own, shrink the budget under eight archives, or let
// one read at a time, or a third, run at once, and a row goes red.
func TestTheTranscriptBoundsAreTheEnginesOwn(t *testing.T) {
	t.Parallel()
	bounds := executor.TranscriptReadBounds()
	if bounds.Entries != executor.TranscriptMaxEntries || bounds.Bytes != executor.TranscriptMaxBytes ||
		bounds.EntryBytes != executor.TranscriptEntryMaxBytes ||
		bounds.ReadBytes != executor.TranscriptReadBytes {
		t.Errorf("the bounds a read is held to are %+v, not the constants that state them", bounds)
	}
	if bounds.Bytes/bounds.EntryBytes < 64 {
		t.Errorf("a page of %d bytes holds %d entries at the entry's %d", bounds.Bytes,
			bounds.Bytes/bounds.EntryBytes, bounds.EntryBytes)
	}
	if bounds.LineBytes != executor.MaxLineBytes {
		t.Errorf("a transcript's line is read to %d, and a run's record to %d", bounds.LineBytes,
			executor.MaxLineBytes)
	}
	if bounds.ReadBytes < 8*executordomain.SessionMaxBytes {
		t.Errorf("the read budget %d is under eight archives at their bound of %d", bounds.ReadBytes,
			executordomain.SessionMaxBytes)
	}
	if n := executor.TranscriptReadsAtOnce; n < 2 || n*3*bounds.LineBytes > 200<<20 {
		t.Errorf("%d reads at once hold up to %d MiB at their worst, want at least two within 200 MiB",
			n, n*3*bounds.LineBytes>>20)
	}
}

// EVERY HARNESS THIS BUILD DRIVES CARRIES ITS SESSION, and the transcript is
// read for each in the one format its runner packs: the harnesses whose
// session is read are the harnesses [executor.Names] lists, from one list.
//
// Mutation: list a harness that carries no session, or read one in a format
// its runner does not pack, and a row goes red.
func TestEveryHarnessThisBuildDrivesCarriesItsSession(t *testing.T) {
	t.Parallel()
	formats := map[string]int{
		executor.ClaudeCodeName: executor.ClaudeArchiveFormat,
		executor.OpenCodeName:   executor.OpenCodeArchiveFormat,
	}
	for _, name := range executor.Names() {
		format, ok := formats[name]
		if !ok {
			t.Errorf("%s is a harness this build drives, and this case names no archive format of it", name)
			continue
		}
		if carried, why := executor.Carries(name, format); !carried {
			t.Errorf("%s's sessions are not read: %s", name, why)
		}
	}
}

// ---- how many transcripts a process reads at once ----------------------- //
//
// NONE OF THESE CASES IS PARALLEL, and each lets go of every read it holds and
// waits for it before it ends: the slots are the process's own
// ([executor.TranscriptReadsAtOnce]), shared by every read in this binary, so a
// parallel case reading beside one of these would hold a slot it counts on.
// Every case that is not parallel runs before the parallel ones are let go.

// slotWaitBound is how long a case here waits for what it is about to see
// before it fails: a bound on a hang, far past what any of these waits takes,
// and never a time a case depends on.
const slotWaitBound = time.Minute

// awaited waits for c, failing the case if it has not come within
// slotWaitBound.
func awaited(t *testing.T, c <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-c:
	case <-time.After(slotWaitBound):
		t.Fatalf("%s: not within %s", what, slotWaitBound)
	}
}

// heldRead is a read of an archive held inside its opener — its slot taken, its
// archive not yet opened — until it is let go, and how it ended.
type heldRead struct {
	let  func()
	done chan struct{}
	err  error
}

// holdSlot starts a read of raw that is held inside its opener until let, and
// returns once it is held there, with one of the process's slots. However the
// case ends, the read is let go and waited for, so no slot outlives the case.
func holdSlot(t *testing.T, raw []byte) *heldRead {
	t.Helper()
	ctx := t.Context()
	opened, gate := make(chan struct{}), make(chan struct{})
	h := &heldRead{let: sync.OnceFunc(func() { close(gate) }), done: make(chan struct{})}
	go func() {
		defer close(h.done)
		_, h.err = executor.ReadTranscript(ctx, executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			func(context.Context) (io.ReadCloser, error) {
				close(opened)
				<-gate
				return io.NopCloser(bytes.NewReader(raw)), nil
			}, 0)
	}()
	t.Cleanup(func() {
		h.let()
		<-h.done
	})
	awaited(t, opened, "a read given a free slot opens its archive")
	return h
}

// waitedContext is a read's context that says when the read first asks for its
// Done channel, which a read waiting for a slot does to leave with its caller:
// so a case ends the context of a read it knows is waiting, rather than of one
// that has yet to ask for a slot at all.
type waitedContext struct {
	context.Context
	asked chan struct{}
	once  sync.Once
}

// waitingOn is a waitedContext under parent, and what ends it.
func waitingOn(parent context.Context) (*waitedContext, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	return &waitedContext{Context: ctx, asked: make(chan struct{})}, cancel
}

// Done says the read asked for it, and is the context's own.
func (c *waitedContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.asked) })
	return c.Context.Done()
}

// readPastTheSlots starts a read of raw under ctx, with every slot held, and
// returns once it is waiting for one; opened is closed if it ever opens its
// archive, and done once it has ended, the transcript and the error it ended
// with in *got and *err.
func readPastTheSlots(t *testing.T, ctx *waitedContext, raw []byte, got *executor.Transcript,
	err *error) (opened, done chan struct{}) {
	t.Helper()
	opened, done = make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		*got, *err = executor.ReadTranscript(ctx, executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			func(context.Context) (io.ReadCloser, error) {
				close(opened)
				return io.NopCloser(bytes.NewReader(raw)), nil
			}, 0)
	}()
	select {
	case <-ctx.asked:
	case <-opened:
		t.Fatal("a read past every slot opened its archive")
	case <-time.After(slotWaitBound):
		t.Fatalf("a read past every slot neither waited for one nor opened its archive within %s",
			slotWaitBound)
	}
	return opened, done
}

// A READ PAST THE SLOTS OPENS NOTHING, AND LEAVES WITH ITS CALLER. With every
// slot held by a read whose archive is still opening, another read waits for
// one without opening its own — an opened object would hold its store's
// connection for as long as it waited — and once its caller leaves it returns
// the caller's own error, having opened nothing.
//
// Mutation: take no slot, or take it only once the archive is open, and the
// read past the slots opens its archive; stop waiting on the caller's context,
// and the read never ends.
func TestAReadPastTheSlotsOpensNothingAndLeavesWithItsCaller(t *testing.T) {
	raw := gzipped(t, briefs(1))
	for range executor.TranscriptReadsAtOnce {
		holdSlot(t, raw)
	}
	ctx, leave := waitingOn(t.Context())
	defer leave()
	var (
		got executor.Transcript
		err error
	)
	opened, done := readPastTheSlots(t, ctx, raw, &got, &err)
	leave()
	awaited(t, done, "a read waiting for a slot ends once its caller leaves")
	if !errors.Is(err, context.Canceled) || len(got.Entries) != 0 {
		t.Errorf("a read whose caller left while it waited: (%d entries, %v), want the caller's "+
			"own error", len(got.Entries), err)
	}
	select {
	case <-opened:
		t.Error("a read whose caller left while it waited for a slot opened its archive")
	default:
	}
}

// A WAITING READ TAKES A SLOT ONCE ONE IS GIVEN BACK. With every slot held,
// another read waits; once one of the reads holding them has read its archive
// and closed it, the waiting read opens its own and reads it whole.
//
// Mutation: never give a slot back, or give it back only from a read that
// failed, and the waiting read never opens its archive.
func TestAWaitingReadTakesASlotOnceOneIsGivenBack(t *testing.T) {
	raw := gzipped(t, briefs(1))
	held := make([]*heldRead, executor.TranscriptReadsAtOnce)
	for i := range held {
		held[i] = holdSlot(t, raw)
	}
	ctx, leave := waitingOn(t.Context())
	defer leave()
	var (
		got executor.Transcript
		err error
	)
	_, done := readPastTheSlots(t, ctx, raw, &got, &err)
	held[0].let()
	awaited(t, held[0].done, "a read holding a slot ends once its archive opens")
	if held[0].err != nil {
		t.Fatalf("the read that held a slot: %v", held[0].err)
	}
	awaited(t, done, "a read waiting for a slot ends once one is given back")
	if err != nil || len(got.Entries) != 1 {
		t.Errorf("the read that waited, once a slot was given back: (%d entries, %v), want the "+
			"archive's one brief", len(got.Entries), err)
	}
}

// closeSeen is an archive whose Close calls seen.
type closeSeen struct {
	io.Reader
	seen func()
}

func (c closeSeen) Close() error {
	c.seen()
	return nil
}

// A READ HOLDS ITS SLOT FROM BEFORE ITS ARCHIVE OPENS UNTIL AFTER IT IS CLOSED.
// Taken only once the object is open, a slot would leave an object open while
// its read waited for one, holding its store's connection; given back before
// the close, it would let another read open an archive beside one still open.
// And the read closes what it opened.
//
// Mutation: take the slot after the open, give it back before the close, or
// leave the archive open, and the read is seen opening or closing without its
// slot, or never closing.
func TestAReadHoldsItsSlotFromBeforeItOpensUntilAfterItCloses(t *testing.T) {
	if held := executor.TranscriptReadsHeld(); held != 0 {
		t.Fatalf("%d slots are held before the case reads anything", held)
	}
	raw := gzipped(t, briefs(1))
	atOpen, atClose := -1, -1
	_, err := executor.ReadTranscript(t.Context(), executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		func(context.Context) (io.ReadCloser, error) {
			atOpen = executor.TranscriptReadsHeld()
			return closeSeen{Reader: bytes.NewReader(raw), seen: func() {
				atClose = executor.TranscriptReadsHeld()
			}}, nil
		}, 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
	if atOpen != 1 || atClose != 1 {
		t.Errorf("the read held %d slots as its archive opened and %d as it closed (-1: never), "+
			"want its own throughout", atOpen, atClose)
	}
	if held := executor.TranscriptReadsHeld(); held != 0 {
		t.Errorf("%d slots are held once the read has ended, want none", held)
	}
}
