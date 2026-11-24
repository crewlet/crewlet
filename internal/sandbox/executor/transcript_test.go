package executor_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A captured session read back as its transcript. Every archive a run can pack
// is PACKED BY THE RUNNER'S OWN SCRIPT (a real box's stand-in harness filing
// where the pinned layout names, or the OpenCode script exporting through the
// fake opencode), so a case reads exactly the bytes a collection keeps. The
// archives no pack makes (no gzip, a gzip of no tar, a sparse member) are built
// here: a box's own agent can leave them in place of what its run packed.

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

// A CLAUDE CODE SESSION READS AS THE TRANSCRIPT ITS HARNESS RECORDED, in
// order: the brief, the model's words, each tool call (input as JSON) and the
// result answering it (named by the call's tool, a failure marked), the
// compaction summary, a non-record line said where it was, then the subagent's
// transcript, each entry naming it. The harness's bookkeeping (meta record,
// file snapshot, system notice, session title), the thinking, a spilled tool
// result and the task list are no entry. Entries carry the harness's instant,
// the page is whole, and a quoted token reads redacted.
//
// Mutation: read the thinking or the meta record as entries, drop the
// sidecars, name a result by nothing, or skip the redaction, and it goes red.
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
	if len(got.Entries) > 3 && got.Entries[3].Bytes != len(redacted) {
		t.Errorf("the redacted result states %d bytes, want the %d its redaction shows, never the %d the "+
			"harness recorded", got.Entries[3].Bytes, len(redacted), len(failure))
	}
}

// openCodeDoc is an OpenCode export holding messages, each one message of
// OpenCode 2's `SessionMessage.Info` as JSON: the document `opencode session
// export` prints, `{"info": …, "messages": […]}`.
func openCodeDoc(messages ...string) string {
	return `{"info":{"id":"ses_0123456789abcdef0123456789abcdef","title":"crewlet"},"messages":[` +
		strings.Join(messages, ",") + `]}`
}

// openCodeUser is a person's message saying text, as OpenCode 2 exports one.
func openCodeUser(text string) string {
	// json.Marshal cannot fail on a string.
	quoted, _ := json.Marshal(text)
	return `{"id":"msg_1","type":"user","text":` + string(quoted) + `,"time":{"created":1793361600000}}`
}

// openCodeFixtureVersions are the OpenCode releases a real export is kept of,
// testdata/opencode/<version>/export.json: the one release this build drives.
var openCodeFixtureVersions = []string{"2.3.2"}

// openCodeExportFixture is the export OpenCode version printed for one session:
// a REAL `opencode session export` of a session the release ran (@opencode/cli
// in a container, against an OpenAI-compatible stub model), byte for byte but for
// the container's paths (run directory /work, home /home/crewlet). It holds a
// brief and its steps (a completed call, a failed read with a note steered in
// meanwhile, a background shell command and a subagent, each with its notice
// and the step it woke); then an API notice, a person's shell command and
// skill, the agent switched to plan and back, the model switched, a compaction;
// then, on a server restarted with changed instructions, a run that answered,
// one its provider refused, one interrupted mid-call and one mid-write; and
// last a directory move.
func openCodeExportFixture(t *testing.T, version string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "opencode", version, "export.json"))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return string(raw)
}

// openCodeMinted is an id the fixture's session minted as it ran, in what its
// tools answered — a shell's, a subagent's session, the project's hash — which
// a case reads past: no two runs of one session mint the same.
var openCodeMinted = regexp.MustCompile(`\b(?:sh|ses)_[0-9A-Za-z]+|\b[0-9a-f]{40}\b`)

// openCodeBackground is what OpenCode's shell answered a call it ran in the
// background, beside the process it started, the ids it minted read past
// ([openCodeMinted]): two text items, a line apart, as 2.3.2 writes them.
func openCodeBackground(pid string) string {
	return "Command moved to the background (shell ID: <id>).\nPID: " + pid + "\n" +
		"Output is streaming to: /work/xdg/data/opencode/shell/<id>/<id>.out\n" +
		"You will be notified automatically when the command finishes. The notification will include the " +
		"command's output. Unless the user explicitly asks otherwise, DO NOT poll for completion, even if you " +
		"need the final result to continue. Repeatedly sleeping and reading or searching the output file is " +
		"polling, not useful work. You may read the current output if it lets you do useful work now, but do " +
		"not repeatedly check it while waiting for the command to finish. Keep working on anything that does " +
		"not depend on the result. If you have nothing else to do, end your response; you will be resumed " +
		"automatically when the command finishes."
}

// openCodeSubagent is what OpenCode's subagent tool answered a call it started
// in the background, the session it minted read past, as 2.3.2 writes it.
const openCodeSubagent = "The subagent is working in the background (sessionID: <id>). You will receive a " +
	"<subagent> message when it goes idle, and again after each later round of work.\n" +
	"DO NOT sleep, poll, proactively check on its progress, or duplicate this subagent's work.\n" +
	"Work on non-overlapping tasks, or briefly tell the user what you launched and end your response.\n" +
	"To send it follow-ups or change its instructions while it runs, call subagent again with this sessionID."

// openCodeFixtureTranscript is the transcript the fixture's session reads as
// on the release version, each entry beside the message it was read from
// (OpenCode's own messages, which read as none, are named where they fall).
func openCodeFixtureTranscript(version string) []shown {
	// THE READ'S FAILURE is OpenCode's own words, and the shell's pids the
	// processes the two runs of the steps started.
	notFound, pids := map[string]string{
		"2.3.2": "File not found: /nonexistent/missing.go\n\nThe folder /nonexistent does not exist. " +
			"The nearest folder that does is /.",
	}[version], map[string][2]string{"2.3.2": {"329", "485"}}[version]
	fixed := shown{kind: executor.EntryModel, text: "Fixed: the cache takes its lock."}
	steps := func(note []shown, pid string) []shown {
		return slices.Concat([]shown{
			// A step: its reasoning none, its words, and a call that completed.
			{kind: executor.EntryModel, text: "Running the tests first."},
			{kind: executor.EntryToolCall, tool: "shell", text: `{"command":"echo ok-from-shell"}`},
			{kind: executor.EntryToolResult, tool: "shell", text: "ok-from-shell\n"},
			// A step whose read failed.
			{kind: executor.EntryToolCall, tool: "read", text: `{"path":"/nonexistent/missing.go"}`},
			{kind: executor.EntryToolResult, tool: "read", failed: true, text: notFound},
		}, note, []shown{
			// A call run in the background, and a subagent.
			{kind: executor.EntryToolCall, tool: "shell",
				text: `{"command":"sleep 2; echo bg-done","background":true}`},
			{kind: executor.EntryToolResult, tool: "shell", text: openCodeBackground(pid)},
			{kind: executor.EntryToolCall, tool: "subagent",
				text: `{"agent":"general","description":"find the race","prompt":"Look for the race."}`},
			{kind: executor.EntryToolResult, tool: "subagent", text: openCodeSubagent},
			fixed,
		})
	}
	return slices.Concat(
		[]shown{{kind: executor.EntryBrief, text: "Fix the flaky test in ENG-7."}},
		steps([]shown{{kind: executor.EntryNote,
			text: note("ada", "Target the release branch.")}}, pids[0]),
		// The execution's end; the subagent's notice and the step it woke;
		// the end; the background call's notice and the step it woke; the end.
		[]shown{fixed, fixed},
		// A person's shell command, a skill, the agent switched to plan and
		// back, the model switched — and the compaction.
		[]shown{{kind: executor.EntrySummary, text: "## Objective\n- Fix the flaky cache test.\n\n" +
			"## Work State\n### Completed\n- The cache takes its lock.\n\n## Next Move\n1. (none)"}},
		// Four notices (the API's, the shell command's, plan mode's on and
		// off), and the steps the session ran again, its history compacted;
		// the end, the subagent's notice and the step it woke.
		steps(nil, pids[1]),
		[]shown{fixed},
		// The execution's end; the agent and the model the next run names;
		// the instructions updated, read by the new server; and the next
		// runs, each naming its agent and ending its execution: one answered,
		// one its provider refused, which wrote nothing, one interrupted as a
		// call ran and one as the model wrote a call.
		[]shown{
			{kind: executor.EntryBrief, text: "Carry on."},
			{kind: executor.EntryModel, text: "Carried on: nothing is left to do."},
			{kind: executor.EntryBrief, text: "Fail now."},
			{kind: executor.EntryBrief, text: "Wait for it."},
			{kind: executor.EntryToolCall, tool: "shell", text: `{"command":"sleep 30; echo woke"}`},
			{kind: executor.EntryToolResult, tool: "shell", failed: true,
				text: "[no output; output file: /work/xdg/data/opencode/shell/<id>/<id>.out]\n\n" +
					"Command interrupted by the user (shell ID: <id>)."},
			{kind: executor.EntryBrief, text: "Write slowly."},
			{kind: executor.EntryToolCall, tool: "write", text: `{}`},
			{kind: executor.EntryToolResult, tool: "write", failed: true, text: "Tool execution interrupted"},
		},
		// The session moved, and the execution's end.
	)
}

// AN OPENCODE SESSION READS AS THE TRANSCRIPT ITS EXPORT HOLDS: each driven
// release's real export (every kind of message OpenCode 2 writes), packed by
// the run's script, reads as the briefs, the model's words, each call (input
// as JSON) and its answer, a failure marked, a steered-in note as the note, and
// the compaction's summary. OpenCode's own messages (notices, instruction
// updates, a person's skill or shell command, an execution's end, a switched
// agent, model or directory), the reasoning and a refused step are no entry.
// Every entry is dated, as every exported message says when.
//
// Mutation: read by OpenCode 1.x's `info.role` and `parts`, read a notice,
// update, skill, shell command or reasoning as an entry, drop a call's result,
// or read a failed call's answer without its failure, and the case goes red.
func TestAnOpenCodeSessionReadsAsTheTranscriptItsExportHolds(t *testing.T) {
	t.Parallel()
	for _, version := range openCodeFixtureVersions {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			got := readOpenCode(t, openCodeCaptureOf(t, openCodeExportFixture(t, version)),
				executor.TranscriptReadBounds())
			if got.Cut != "" || got.More() {
				t.Errorf("the read is cut %q, want the whole transcript", got.Cut)
			}
			read := slices.Clone(got.Entries)
			for i := range read {
				read[i].Text = openCodeMinted.ReplaceAllString(read[i].Text, "<id>")
				if read[i].At.IsZero() {
					t.Errorf("entry %d (%s) is not dated, and its message says when", i, read[i].Kind)
				}
			}
			sameEntries(t, read, openCodeFixtureTranscript(version))
		})
	}
}

// openCodeStep is an assistant's step holding items, as OpenCode 2
// exported one: the step's agent, model, finish, cost and tokens beside its
// content.
func openCodeStep(items ...string) string {
	return `{"id":"msg_2","time":{"created":1793361601000,"streamed":1793361601100,"completed":1793361609000},` +
		`"type":"assistant","agent":"build","model":{"id":"stub-model","providerID":"stub","variant":"default"},` +
		`"content":[` + strings.Join(items, ",") + `],"finish":"tool-calls","rawFinish":"tool_calls","cost":0,` +
		`"tokens":{"input":1001,"output":30,"reasoning":0,"cache":{"read":200,"write":0}}}`
}

// openCodeCall is a tool item of a step: the call named name, as it stands.
func openCodeCall(name, state string) string {
	return `{"type":"tool","id":"call_1","name":"` + name + `","executed":false,"state":` + state +
		`,"time":{"created":1793361602000,"ran":1793361603000,"completed":1793361604000}}`
}

// EACH KIND OF OPENCODE MESSAGE READS AS WHAT IT IS, OR AS NOTHING. A person's
// message is the brief, or a note where it opens with one. A step is the
// model's words and its calls, never its reasoning: a completed call is the
// call and its answer (text items a line apart, a file said as a file), a
// failed one the call and its failure, marked, with what it answered first,
// and one stopped running or mid-write the call alone, its input so far. A
// completed compaction is its summary. Every other kind is OpenCode's own and
// no entry, a notice quoting a note's heading (as a job's output can) included.
//
// Mutation: read any OpenCode kind as an entry, search a notice for notes, read
// a failed or running compaction as a summary, give an unsettled call a result,
// drop a failure's message or prior answer, or read reasoning: that row is red.
func TestEachKindOfOpenCodeMessageReadsAsWhatItIsOrAsNothing(t *testing.T) {
	t.Parallel()
	// Shapes are the fixture's (testdata/opencode) but where it has no example,
	// written from OpenCode 2's schema: a file answer
	// (packages/schema/src/tool.ts:73-79), a streaming or running call
	// (packages/schema/src/session-message.ts:126-137; the interrupted session
	// settled its calls as failed), a failed compaction (:269-276) and a running
	// one (:247-254), which an export omits
	// (packages/core/src/session/transfer.ts:165-169) but a box's agent can write.
	for name, c := range map[string]struct {
		message string
		want    []shown
	}{
		"a person's message is the brief": {
			`{"id":"msg_1","time":{"created":1793361600000},"text":"Fix the flaky test in ENG-7.","files":[],"type":"user"}`,
			[]shown{{kind: executor.EntryBrief, text: "Fix the flaky test in ENG-7."}}},
		"a person's message opening with a note is the note": {openCodeUser(note("ada", "Target main.")),
			[]shown{{kind: executor.EntryNote, text: note("ada", "Target main.")}}},
		"a step's text is the model's, its reasoning none": {openCodeStep(
			`{"type":"reasoning","text":"the race is in the cache","state":{"reasoningField":"reasoning_content"},`+
				`"time":{"created":1793361601200,"completed":1793361601300}}`,
			`{"type":"text","text":"Running the tests first."}`,
			`{"type":"citation","text":"an item of a type this build does not know"}`),
			[]shown{{kind: executor.EntryModel, text: "Running the tests first."}}},
		"a completed call is the call and its answer": {openCodeStep(openCodeCall("shell",
			`{"status":"completed","input":{"command":"go test ./cache/..."},"content":[`+
				`{"type":"text","text":"ok cache 0.2s"},{"type":"text","text":"exit 0"}],`+
				`"metadata":{"status":"completed","truncated":false,"exit":0}}`)),
			[]shown{{kind: executor.EntryToolCall, tool: "shell", text: `{"command":"go test ./cache/..."}`},
				{kind: executor.EntryToolResult, tool: "shell", text: "ok cache 0.2s\nexit 0"}}},
		"a file a call answered with is said as a file": {openCodeStep(openCodeCall("read",
			`{"status":"completed","input":{"path":"logo.png"},"content":[`+
				`{"type":"file","uri":"data:image/png;base64,iVBORw0KGgo=","mime":"image/png","name":"logo.png"},`+
				`{"type":"file","uri":"file:///work/spec.pdf","mime":"application/pdf"}]}`)),
			[]shown{{kind: executor.EntryToolCall, tool: "read", text: `{"path":"logo.png"}`},
				{kind: executor.EntryToolResult, tool: "read", text: "[an image]\n[a file]"}}},
		"a failed call is its failure, marked, and what it answered first": {openCodeStep(openCodeCall("shell",
			`{"status":"error","input":{"command":"go vet ./..."},`+
				`"error":{"type":"tool.execution","message":"the command timed out"},`+
				`"content":[{"type":"text","text":"vet: checking cache"}]}`)),
			[]shown{{kind: executor.EntryToolCall, tool: "shell", text: `{"command":"go vet ./..."}`},
				{kind: executor.EntryToolResult, tool: "shell", failed: true,
					text: "the command timed out\nvet: checking cache"}}},
		"a failed call whose failure has no message is its class": {openCodeStep(openCodeCall("shell",
			`{"status":"error","input":{"command":"true"},"error":{"type":"tool.aborted","message":""}}`)),
			[]shown{{kind: executor.EntryToolCall, tool: "shell", text: `{"command":"true"}`},
				{kind: executor.EntryToolResult, tool: "shell", failed: true, text: "tool.aborted"}}},
		"a call stopped while it ran is the call alone": {openCodeStep(openCodeCall("shell",
			`{"status":"running","input":{"command":"sleep 600"},"metadata":{}}`)),
			[]shown{{kind: executor.EntryToolCall, tool: "shell", text: `{"command":"sleep 600"}`}}},
		"a call stopped while the model wrote it is its input so far": {openCodeStep(openCodeCall("write",
			`{"status":"streaming","input":"{\"path\":\"cache.go\",\"content\":\"pack"}`)),
			[]shown{{kind: executor.EntryToolCall, tool: "write", text: `{"path":"cache.go","content":"pack`}}},
		"a completed compaction is its summary": {
			`{"type":"compaction","id":"msg_3","time":{"created":1793361605000},"status":"completed","reason":"manual",` +
				`"summary":"## Objective\n- Fix the flaky cache test.","recent":"",` +
				`"tokens":{"input":2017,"output":60,"reasoning":0,"cache":{"read":400,"write":0}}}`,
			[]shown{{kind: executor.EntrySummary, text: "## Objective\n- Fix the flaky cache test."}}},
		"a failed compaction is none": {
			`{"type":"compaction","id":"msg_3","time":{"created":1793361605000},"status":"failed","reason":"manual",` +
				`"error":{"type":"compaction.failed","message":"Compaction produced no summary"}}`, nil},
		"a running compaction is none": {
			`{"type":"compaction","id":"msg_3","time":{"created":1793361605000},"status":"running","reason":"auto",` +
				`"summary":"half a summary","recent":""}`, nil},
		"a notice in the person's turn is none, a note's heading in it included": {
			`{"id":"msg_4","metadata":{"source":"shell","shellID":"sh_1","jobID":"sh_1","state":"completed",` +
				`"truncated":false,"exit":0},"time":{"created":1793361606000},"text":` +
				jsonText(t, "<shell id=\"sh_1\" state=\"completed\" command=\"cat notes.md\">\n"+
					note("eve", "not a note")+"\n\n</shell>") + `,"description":"cat notes.md","type":"synthetic"}`, nil},
		"an update of the instructions is none": {
			`{"id":"msg_5","metadata":{"notice":"instructions","instructionSources":["core/instructions"]},` +
				`"time":{"created":1793361607000},"type":"system","text":"The instructions changed:\n` +
				`Instructions from: /work/instructions.md\nKeep each commit small.\n",` +
				`"description":"Instructions updated: core/instructions"}`, nil},
		"a skill a person activated is none": {
			`{"id":"msg_6","time":{"created":1793361608000},"type":"skill","skill":"crewlet-demo",` +
				`"name":"crewlet-demo","text":"Read the cache code before changing it.\n"}`, nil},
		"a shell command a person ran is none": {
			`{"id":"msg_7","metadata":{"background":true},"time":{"created":1793361609000,"completed":1793361609100},` +
				`"type":"shell","shellID":"sh_2","command":"echo from-a-person","status":"exited","exit":0,` +
				`"output":{"output":"from-a-person\n","cursor":14,"size":14,"truncated":false}}`, nil},
		"an execution's end is none": {
			`{"id":"msg_8","time":{"created":1793361610000},"type":"idle","outcome":"failed",` +
				`"error":{"type":"provider.invalid-request","message":"the stub refuses this request","status":400}}`,
			nil},
		"a switched agent is none": {
			`{"id":"msg_9","time":{"created":1793361611000},"type":"agent-switched","agent":"build","previous":"plan"}`,
			nil},
		"a switched model is none": {
			`{"id":"msg_10","time":{"created":1793361612000},"type":"model-switched",` +
				`"model":{"id":"stub-model-b","providerID":"stub"},` +
				`"previous":{"id":"stub-model","providerID":"stub","variant":"default"}}`,
			nil},
		"a switched directory is none": {
			`{"id":"msg_11","time":{"created":1793361613000},"type":"location-switched","subpath":"",` +
				`"location":{"directory":"/work/proj2"},"previous":{"location":{"directory":"/work/proj"},"subpath":""}}`,
			nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := readOpenCode(t, openCodeCaptureOf(t, openCodeDoc(c.message)), executor.TranscriptReadBounds())
			sameEntries(t, got.Entries, c.want)
		})
	}
}

// AN OPENCODE ENTRY IS DATED WHEN ITS HARNESS RECORDED IT: a person's message
// when it arrived, the model's words when their step began, a call when it ran
// — one that never ran, when the model began it — and its answer when it
// settled, where the record says when; and otherwise when the step began.
//
// Mutation: date a call by when the model began it though it ran, or by its
// step; date an answer by its call; or date the model's words by when their
// step settled, and a row goes red.
func TestAnOpenCodeEntryIsDatedWhenItsHarnessRecordedIt(t *testing.T) {
	t.Parallel()
	at := func(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
	export := openCodeDoc(
		`{"id":"msg_1","time":{"created":1793361600000},"text":"Fix it.","type":"user"}`,
		openCodeStep(`{"type":"text","text":"Running the tests first."}`,
			openCodeCall("shell", `{"status":"completed","input":{"command":"true"},"content":[{"type":"text","text":"ok"}]}`),
			`{"type":"tool","id":"call_2","name":"write","state":{"status":"streaming","input":"{\"pa"},`+
				`"time":{"created":1793361605000}}`,
			`{"type":"tool","id":"call_3","name":"read","state":{"status":"completed","input":{"path":"a"},`+
				`"content":[{"type":"text","text":"a"}]},"time":{}}`))
	got := readOpenCode(t, openCodeCaptureOf(t, export), executor.TranscriptReadBounds())
	want := []time.Time{
		at(1793361600000), // the brief, when it arrived
		at(1793361601000), // the model's words, when their step began
		at(1793361603000), // the call, when it ran
		at(1793361604000), // its answer, when it settled
		at(1793361605000), // a call that never ran, when the model began it
		at(1793361601000), // a call that says neither, when its step began
		at(1793361601000), // an answer that says not when, when its step began
	}
	if len(got.Entries) != len(want) {
		t.Fatalf("the transcript has %d entries, want %d: %+v", len(got.Entries), len(want), shownOf(got.Entries))
	}
	for i, e := range got.Entries {
		if !e.At.Equal(want[i]) {
			t.Errorf("entry %d (%s %q) is dated %s, want %s", i, e.Kind, e.Text, e.At, want[i])
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
	export := openCodeDoc(
		`{"type":"user","text":"past the year 9999","time":{"created":253402300800000}}`,
		`{"type":"user","text":"in range","time":{"created":`+strconv.FormatInt(recorded.UnixMilli(), 10)+`}}`)
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

// A NOTE A PERSON SENT THE RUN READS AS A NOTE, ONCE, WHERE ITS HOOK HANDED IT:
// off the firing's `hook_additional_context`, at a batch and at the finish (two
// in one step as two), never off the copies beside it (#1054); the hook's own
// line (the live `[steer]`) as a note saying whose reached the run. OpenCode's
// note, a person's message, reads as a note, not a brief. A brief carrying
// notes further down (a launch's) is still the brief, and a tool result quoting
// a note's heading is the tool's: tool text is never a note.
//
// Mutation: read a note off any record's strings, or off another attachment,
// drop the hook's line, take a heading anywhere in a brief for a note, or read
// OpenCode's note as a brief, and a row goes red.
func TestANoteAPersonSentTheRunReadsAsANote(t *testing.T) {
	t.Parallel()
	batch := note("ada", "Target the release branch.") + "\n\n" + note("cy", "And keep the old flag.")
	stop := note("bob", "Stop after the fix.")
	quoted := "notes.md: " + note("eve", "not a note") // a file a tool read
	// A firing's records as 2.1.310 and 2.1.322 write them (holdings/s1): its
	// success, whose stdout carries the answer; its context, with the form it
	// is rendered in; and, at Stop, the summary; then the request's blob.
	firing := func(event, at, context string) []string {
		answer := `{"hookSpecificOutput":{"hookEventName":"` + event + `","additionalContext":` +
			jsonText(t, context) + `}}`
		framed := "<system-reminder>\n" + event + " hook additional context: " + context + "\n</system-reminder>"
		out := []string{
			`{"type":"attachment","attachment":{"type":"hook_success","hookEvent":"` + event + `","content":"",` +
				`"stdout":` + jsonText(t, answer) + `},"timestamp":"` + at + `"}`,
			`{"type":"attachment","attachment":{"content":[` + jsonText(t, context) + `],"hookEvent":"` + event +
				`","type":"hook_additional_context"},"rendered":[{"content":` + jsonText(t, framed) + `}],` +
				`"renderedRole":"system","timestamp":"` + at + `"}`,
		}
		if event == "Stop" {
			out = append(out, `{"type":"system","subtype":"stop_hook_summary","hookAdditionalContext":[`+
				jsonText(t, context)+`],"timestamp":"`+at+`"}`)
		}
		return append(out, `{"type":"api-request-blob","message":{"role":"user","content":[{"type":"text",`+
			`"text":`+jsonText(t, framed)+`}]}}`)
	}
	record := []string{
		`{"type":"user","message":{"role":"user","content":` +
			jsonText(t, "Fix the flake.\n\n"+note("ada", "use main")) + `},"timestamp":"2026-10-30T12:00:00.000Z"}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"notes.md"}}]},"timestamp":"2026-10-30T12:00:01.000Z"}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":` +
			jsonText(t, quoted) + `}]},"timestamp":"2026-10-30T12:00:02.000Z"}`,
	}
	// An attachment of another type, its content quoting a note, hands none.
	record = append(record, `{"type":"attachment","attachment":{"content":[`+jsonText(t, note("eve", "not handed"))+
		`],"type":"queued_command"},"timestamp":"2026-10-30T12:00:02.500Z"}`)
	record = append(record, firing("PostToolBatch", "2026-10-30T12:00:03.000Z", batch)...)
	record = append(record, `{"type":"system","subtype":"informational","content":"PostToolBatch says: `+
		`crewlet: 2 notes from ada, cy reached the run","timestamp":"2026-10-30T12:00:03.100Z"}`)
	record = append(record, firing("Stop", "2026-10-30T12:00:04.000Z", stop)...)
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

	open := readOpenCode(t, openCodeCaptureOf(t, openCodeDoc(openCodeUser("Fix the flake."),
		openCodeUser(note("ada", "Target main.")))), executor.TranscriptReadBounds())
	sameEntries(t, open.Entries, []shown{
		{kind: executor.EntryBrief, text: "Fix the flake."},
		{kind: executor.EntryNote, text: note("ada", "Target main.")},
	})
}

// EACH NOTE A HOOK HANDED A MEASURED RUN READS ONCE: the probe sessions 2.1.310
// and 2.1.322 wrote (holdings/s1: a note in the brief, one handed at a batch and
// one at the finish, each with every copy its release writes beside it) read as
// the brief and one note entry a firing, in the firing's order (#1054).
//
// Mutation: read a note off every record's strings, and each firing reads three
// or four times.
func TestEachNoteAHookHandedAMeasuredRunReadsOnce(t *testing.T) {
	t.Parallel()
	for release, session := range map[string]string{
		"2.1.310": "5e55100a-0000-4000-8000-000000000106", "2.1.322": "5e55100a-0000-4000-8000-000000000113",
	} {
		t.Run(release, func(t *testing.T) {
			t.Parallel()
			raw := fixture(t, release, "holdings/s1/projects/crewlet/"+session+".jsonl")
			got := readClaudeRecords(t, strings.Split(strings.TrimSpace(raw), "\n"))
			var briefs int
			var handed []string
			for _, e := range got {
				switch e.Kind {
				case executor.EntryBrief:
					if strings.HasPrefix(e.Text, "SCENARIO=s1") {
						briefs++
					}
				case executor.EntryNote:
					handed = append(handed, e.Text)
				}
			}
			want := []string{"PROBE NOTE ctx-00000000-0000-4000-8000-000000000020",
				"PROBE NOTE ctx-00000000-0000-4000-8000-000000000028"}
			ok := briefs == 1 && len(handed) == len(want)
			for i := 0; ok && i < len(want); i++ {
				ok = strings.Contains(handed[i], want[i])
			}
			if !ok {
				t.Errorf("the session reads as %d briefs and the notes %q, want its brief once and one note "+
					"a firing, %q", briefs, handed, want)
			}
		})
	}
}

// THE LINE THE RUN'S HOOK PRINTED IS A NOTE AS THE HARNESS RECORDED IT. The
// line saying whose notes reached the run names every sender, and five run
// past the hundred and sixty bytes the live transcript shows of a line: its
// entry is the line whole, its size its own and nothing cut, with nothing the
// harness did not write after it — and a token it quotes is redacted, as every
// entry's text is, its size the redaction's.
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
		if e := got.Entries[1]; strings.Contains(e.Text, plantedToken) || e.Bytes != len(redact.Secrets(quoted)) {
			t.Errorf("the hook's line quoting a token reads %q stating %d bytes, want it redacted, "+
				"stating the %d its redaction shows", e.Text, e.Bytes, len(redact.Secrets(quoted)))
		}
	}
}

// A RECORD OF NO SHAPE THIS READ KNOWS IS SAID, NEVER READ AS SILENCE: a line
// of Claude Code's record that is JSON but no object; a person's or model's
// record whose message is no object, or whose content is neither text nor a
// list; an OpenCode message that is no object, a step whose content is no
// list, and a message naming no kind or one OpenCode 2 does not write (a later
// release's, or 1.x's, which names none). Each is an entry where it was. A
// retyped field inside a record is skipped, the record read.
//
// Mutation: tolerate any of these and its entry vanishes; refuse a retyped
// field and the record that held it does.
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

	export := openCodeDoc(`"not a message"`,
		`{"type":"assistant","content":{"type":"text","text":"content of an object"}}`,
		openCodeUser("read"),
		`{"type":"assistant","time":{"created":"soon"},"content":[{"type":"text","text":"retyped"}]}`,
		`{"text":"a message naming no kind"}`,
		`{"type":7,"text":"a kind that is no string"}`,
		`{"type":"todo-updated","text":"a kind a later release adds"}`,
		`{"info":{"role":"user"},"parts":[{"type":"text","text":"OpenCode 1.x's message"}]}`)
	open := readOpenCode(t, openCodeCaptureOf(t, export), executor.TranscriptReadBounds())
	noKind := "a message of the export of no kind this read knows was not read"
	sameEntries(t, open.Entries, []shown{
		{kind: executor.EntryUnread, text: "a message of the export is not a message, and was not read"},
		{kind: executor.EntryUnread, text: "a message of the export whose content is not a list was not read"},
		{kind: executor.EntryBrief, text: "read"},
		{kind: executor.EntryModel, text: "retyped"},
		{kind: executor.EntryUnread, text: noKind},
		{kind: executor.EntryUnread, text: noKind},
		{kind: executor.EntryUnread, text: noKind},
		{kind: executor.EntryUnread, text: noKind},
	})
}

// briefs is an export of n person's messages, `message 0` to `message n-1`.
func briefs(n int) string {
	messages := make([]string, n)
	for i := range n {
		messages[i] = fmt.Sprintf(`{"id":"msg_%d","type":"user","text":"message %d","time":{"created":%d}}`,
			i, i, 1793361600000+int64(i)*1000)
	}
	return openCodeDoc(messages...)
}

// within is the engine's bounds with the given ones lowered.
func within(change func(*executor.TranscriptBounds)) executor.TranscriptBounds {
	bounds := executor.TranscriptReadBounds()
	change(&bounds)
	return bounds
}

// pageAll reads raw, an archive harness packed in format, a page at a time
// under bounds, from the first entry until a page says nothing follows, and
// answers every page.
func pageAll(t *testing.T, harness string, format int, raw []byte,
	bounds executor.TranscriptBounds) []executor.Transcript {
	t.Helper()
	var pages []executor.Transcript
	for from := 0; ; {
		page, err := executor.ReadTranscriptWithin(t.Context(), harness, format,
			opener(bytes.NewReader(raw)), from, bounds)
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
			pages := pageAll(t, executor.OpenCodeName, executor.OpenCodeArchiveFormat, raw, c.bounds)
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

// A CLAUDE CODE TRANSCRIPT READ A PAGE AT A TIME IS THE WHOLE READ, tools and
// threads included: at one to five entries a page, the pages of a session whose
// calls and results straddle pages, its subagent after the run, join to the
// whole read entry for entry. A page is read from the archive's start and
// remembers the calls before it, so a result names its call's tool on any page.
//
// Mutation: remember a call only where its entry lands on the page, or forget
// a page's calls when it starts, and a later page's result names no tool.
func TestAClaudeCodeTranscriptReadAPageAtATimeIsTheWholeRead(t *testing.T) {
	t.Parallel()
	_, raw := claudeCaptureOf(t, claudeSessionRecord, claudeSidecar)
	whole := readClaude(t, raw, executor.TranscriptReadBounds())
	if len(whole.Entries) < 6 {
		t.Fatalf("the whole read holds %d entries, too few to page", len(whole.Entries))
	}
	for n := 1; n <= 5; n++ {
		var read []executor.TranscriptEntry
		for _, page := range pageAll(t, executor.ClaudeCodeName, executor.ClaudeArchiveFormat, raw,
			within(func(b *executor.TranscriptBounds) { b.Entries = n })) {
			read = append(read, page.Entries...)
		}
		if got, want := shownOf(read), shownOf(whole.Entries); !slices.Equal(got, want) {
			t.Errorf("read %d entries a page, the pages join as\n%+v\nwant the whole read's\n%+v", n,
				got, want)
		}
	}
}

// AN ENTRY IS REDACTED BEFORE IT IS CUT. A tool output past the entry's bound,
// a token straddling it, reads cut to the bound and marked, stating the size
// its whole text redacts to, with no part of the token: cut first, the token's
// opening is no longer one the redaction knows. A private key whose body begins
// inside the bound and whose END lies many KiB past it (prose straight after
// its BEGIN, so only reading back from the END reaches the body) is redacted
// whole: the redaction reaches a key block's length past the bound.
//
// Mutation: cut before redacting, redact only the bound's own bytes, or read a
// window short of a key block past the bound, and the token's opening or the
// key's body is handed out; state the recorded size, and it counts the bytes
// the redaction took out.
func TestAnEntryIsRedactedBeforeItIsCut(t *testing.T) {
	t.Parallel()
	small := within(func(b *executor.TranscriptBounds) { b.EntryBytes = 64 })
	tool := func(output string) string {
		return openCodeDoc(`{"type":"assistant","content":[{"type":"tool","id":"call_1","name":"shell",` +
			`"state":{"status":"completed","input":{},"content":[{"type":"text","text":` + jsonText(t, output) +
			`}]}}]}`)
	}
	output := strings.Repeat("x", 50) + " " + plantedToken + " the rest of the output"
	got := readOpenCode(t, openCodeCaptureOf(t, tool(output)), small)
	if len(got.Entries) != 2 {
		t.Fatalf("the transcript has %d entries, want the call and its result: %+v",
			len(got.Entries), got.Entries)
	}
	result := got.Entries[1]
	if !result.Cut || len(result.Text) > 64 || result.Bytes != len(redact.Secrets(output)) {
		t.Errorf("the result is %d bytes cut %v stating %d, want at most 64, cut, stating the %d its "+
			"redaction shows", len(result.Text), result.Cut, result.Bytes, len(redact.Secrets(output)))
	}
	if strings.Contains(result.Text, "ghp_") {
		t.Errorf("the cut result hands out the token's opening: %q", result.Text)
	}
	if !strings.HasPrefix(result.Text, strings.Repeat("x", 50)+" "+redact.Marker) {
		t.Errorf("the cut result is %q, want the output up to the token's redaction", result.Text)
	}

	// THE KEY'S BODY BEGINS INSIDE THE BOUND; its END, the only thing reaching
	// the body past the prose closing the BEGIN's block, is 200 lines (~13 KiB)
	// past it: further than any window short of a key block reads.
	body := strings.Repeat("MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun\n", 200)
	key := "-----BEGIN RSA PRIVATE KEY-----\nsee below\n" + body + "-----END RSA PRIVATE KEY-----\ndone\n"
	got = readOpenCode(t, openCodeCaptureOf(t, tool(key)), small)
	if len(got.Entries) != 2 {
		t.Fatalf("the transcript has %d entries, want the call and its result", len(got.Entries))
	}
	result = got.Entries[1]
	if strings.Contains(result.Text, "MIIEow") || !result.Cut || result.Bytes != len(redact.Secrets(key)) {
		t.Errorf("the key's result reads %q (cut %v, %d bytes), want it cut with the key's body "+
			"redacted, stating the %d its redaction shows", result.Text, result.Cut, result.Bytes,
			len(redact.Secrets(key)))
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
	got := readClaude(t, executor.SparseBomb(t, 1<<62), within(func(b *executor.TranscriptBounds) {
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

// A VALUE OF ANY LENGTH IS READ, AND THE RECORD READS ON PAST IT. A Claude Code
// line and an OpenCode message are read however long they run, a value past
// [executor.TranscriptValueMaxBytes] shown as the opening of its whole
// redaction, its whole size stated, and the line or message after it read.
//
// Mutation: say a line past a line bound in its place, or end a read at a value
// past the bound, and the entry or the record after it goes.
func TestAValueOfAnyLengthIsReadAndTheRecordReadsOn(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("y", executor.TranscriptValueMaxBytes+1<<10)
	record := []string{
		`{"type":"user","message":{"role":"user","content":"first"}}`,
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"` + long + `"}]}}`,
		`{"type":"user","message":{"role":"user","content":"after"}}`,
	}
	// BUILT HERE, as no pack's script takes a line this long as its argument.
	var member bytes.Buffer
	tw := tar.NewWriter(&member)
	body := strings.Join(record, "\n") + "\n"
	if err := tw.WriteHeader(&tar.Header{Name: "projects/" + executor.ClaudeProjectDir + "/ses.jsonl",
		Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	export := openCodeDoc(openCodeUser("first"),
		`{"type":"assistant","content":[{"type":"text","text":"`+long+`"}]}`, openCodeUser("after"))
	bounds := executor.TranscriptReadBounds()
	for name, got := range map[string]executor.Transcript{
		"claude code": readClaude(t, gzipped(t, member.String()), bounds),
		"opencode":    readOpenCode(t, gzipped(t, export), bounds),
	} {
		if len(got.Entries) != 3 || got.Entries[0].Text != "first" || got.Entries[1].Kind != executor.EntryModel ||
			got.Entries[1].Bytes != len(long) || !got.Entries[1].Cut ||
			!strings.HasPrefix(got.Entries[1].Text, strings.Repeat("y", 1<<10)) ||
			got.Entries[2].Text != "after" || got.Cut != "" {
			t.Errorf("%s read as %+v cut %q, want the first, the long value's opening sized %d, and the one "+
				"after it", name, shownOf(got.Entries), got.Cut, len(long))
		}
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
	}{{"custom", 1}, {executor.ClaudeCodeName, executor.ClaudeArchiveFormat + 1}} {
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
// archive is not what its format describes (no gzip, bytes that do not inflate,
// a stream cut short, a gzip of no tar, no export document) is a malformed
// archive whose reason is this package's one sentence for that way, and nothing
// the archive holds reaches them: a string export with a planted token and a
// MiB of a box's words is no export document, carrying neither. An export
// ending before it closes is cut short wherever it ends (in its list's opening,
// after a message, after a comma), whatever the JSON decoder calls that end.
//
// Mutation: say it in the decoder's words or name the planted token, and the
// token or the MiB reaches the reason; read every syntax error an export's walk
// meets as no export document, and one cut between messages is said as that.
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
			gzipped(t, `{"info":{},"messages":[{"type":"user","text":"fix"}`+"\n"), executor.WhyCutShort},
		{"an export cut after the comma before a message", executor.OpenCodeName,
			executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[{"type":"user","text":"fix"},`), executor.WhyCutShort},
		{"an export cut inside a message", executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			gzipped(t, `{"info":{},"messages":[{"type":"us`), executor.WhyCutShort},
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

// A READ STOPS WHEN ITS CALLER IS GONE. A caller leaving as its archive opens
// stops the read at the next line or message, with the context's own error,
// never a transcript or a malformed archive; an open failing as its caller
// leaves is answered as left, whatever the store made of it; and a caller gone
// before the read began has it open nothing, a slot free or not.
//
// Mutation: drop the check between lines and a departed caller gets the whole
// transcript; answer a failed open as failed and the store's words reach it;
// take a slot for a caller already gone and the read opens its archive for
// nobody, at random (a select between a free slot and a done context picks),
// hence the thirty-two tries.
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
		// THE CALLER LEAVES AS THE ARCHIVE OPENS: one gone before the read
		// began opens nothing (below), so never reaches the check between lines.
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
// argue: a page holds sixty-four entries at the entry's bound; a read frames
// the lines its constant states; and at least two reads run at once, within
// 200 MiB at their measured worst ([executor.TranscriptReadHeldBytes]).
//
// Mutation: raise the entry's bound past a sixty-fourth of the page's, state a
// lines bound of another figure, or let one read at a time, or a fifth, run at
// once, and a row goes red.
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
	if bounds.ReadLines != executor.TranscriptReadLines {
		t.Errorf("a read frames %d lines and elements, not the constant that states it", bounds.ReadLines)
	}
	if n := executor.TranscriptReadsAtOnce; n < 2 || n*executor.TranscriptReadHeldBytes > 200<<20 {
		t.Errorf("%d reads at once hold up to %d MiB at their worst, want at least two within 200 MiB",
			n, n*executor.TranscriptReadHeldBytes>>20)
	}
}

// EVERY HARNESS THIS BUILD DRIVES CONTINUES AND CARRIES ITS SESSIONS, and the
// transcript is read for each in the one format its runner packs: the
// harnesses whose session is read are the harnesses [executor.Names] lists,
// from one list.
//
// Mutation: list a harness that carries none, let a runner start every launch
// fresh, or read one in a format its runner does not pack, and a row goes red.
func TestEveryHarnessThisBuildDrivesCarriesItsSession(t *testing.T) {
	t.Parallel()
	formats := map[string]int{
		executor.ClaudeCodeName:  executor.ClaudeArchiveFormat,
		executor.CodexName:       executor.Codex{}.ArchiveFormat(),
		executor.OpenCodeName:    executor.OpenCodeArchiveFormat,
		executor.GeminiCLIName:   executor.GeminiArchiveFormat,
		executor.QwenCodeName:    executor.QwenArchiveFormat,
		executor.CopilotName:     executor.CopilotArchiveFormat,
		executor.GrokName:        executor.GrokArchiveFormat,
		executor.KimiName:        executor.KimiArchiveFormat,
		executor.MuseName:        executor.MuseArchiveFormat,
		executor.HermesName:      executor.HermesArchiveFormat,
		executor.PiName:          executor.PiArchiveFormat,
		executor.CursorAgentName: executor.CursorAgentArchiveFormat,
	}
	for _, name := range executor.Names() {
		if executor.RunnerNamed(name).StartsFresh() {
			t.Errorf("%s starts every launch on a session of its own, so it continues none", name)
			continue
		}
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
// NONE OF THESE CASES IS PARALLEL, and each lets go of and waits for every read
// it holds before it ends: the slots ([executor.TranscriptReadsAtOnce]) are the
// process's own, shared by every read in this binary, so a parallel read beside
// one would hold a slot it counts on. Non-parallel cases all run before the
// parallel ones are let go.

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

// stalledArchive is an archive whose bytes never come: a read of it says so
// on inside and waits for its caller to leave.
type stalledArchive struct {
	ctx    context.Context
	inside func()
}

func (a stalledArchive) Read([]byte) (int, error) {
	a.inside()
	<-a.ctx.Done()
	return 0, a.ctx.Err()
}

func (stalledArchive) Close() error { return nil }

// A READ ITS CALLER ABANDONS PART WAY GIVES ITS SLOT BACK. With every slot held
// by a read inside an archive whose bytes never come, the callers leave (a
// dashboard's query its client stopped waiting for, whose context the socket
// ends): each read ends with its caller's own error and gives its slot back, so
// the next read reads at once.
//
// Mutation: read the archive under a context the caller cannot end, or give
// the slot back only from a read that ended on its own, and the reads never end
// or the next read waits behind them.
func TestAReadItsCallerAbandonsPartWayGivesItsSlotBack(t *testing.T) {
	type stalled struct {
		inside, done chan struct{}
		err          error
	}
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	reads := make([]*stalled, executor.TranscriptReadsAtOnce)
	for i := range reads {
		r := &stalled{inside: make(chan struct{}), done: make(chan struct{})}
		reads[i] = r
		seen := sync.OnceFunc(func() { close(r.inside) })
		go func() {
			defer close(r.done)
			_, r.err = executor.ReadTranscript(ctx, executor.OpenCodeName, executor.OpenCodeArchiveFormat,
				func(ctx context.Context) (io.ReadCloser, error) { return stalledArchive{ctx, seen}, nil }, 0)
		}()
		awaited(t, r.inside, "a read given a free slot reads its archive")
	}
	if held := executor.TranscriptReadsHeld(); held != executor.TranscriptReadsAtOnce {
		t.Fatalf("%d slots are held by the reads inside their archives, want every one", held)
	}
	leave()
	for _, r := range reads {
		awaited(t, r.done, "a read inside its archive ends once its caller leaves")
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("a read its caller abandoned ended %v, want its caller's own error", r.err)
		}
	}
	if held := executor.TranscriptReadsHeld(); held != 0 {
		t.Fatalf("%d slots are held once every abandoned read has ended, want none", held)
	}
	got, err := executor.ReadTranscript(t.Context(), executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		opener(bytes.NewReader(gzipped(t, briefs(1)))), 0)
	if err != nil || len(got.Entries) != 1 {
		t.Errorf("the read after the abandoned ones: (%d entries, %v), want the archive's one brief",
			len(got.Entries), err)
	}
}

// leavingArchive is an archive handing out r's bytes whose reader's caller
// leaves once it has handed after of them; handed counts all it handed.
type leavingArchive struct {
	r      io.Reader
	after  int
	leave  context.CancelFunc
	handed int
}

func (a *leavingArchive) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if a.handed += n; a.handed >= a.after {
		a.leave()
	}
	return n, err
}

func (*leavingArchive) Close() error { return nil }

// A READ ITS CALLER LEAVES INSIDE ONE LONG VALUE STOPS THERE. The slow read a
// dashboard gives up on is one value streamed past the value bound, which the
// export walk asks its context after only between records: a person's read of
// a brief 4 MiB past the value bound whose caller leaves an eighth of the way through the archive
// ends with its caller's own error, having read the archive no further than a
// piece past where its caller left, and gives its slot back.
//
// Mutation: ask the context only between records, and the read streams the
// value to its end.
func TestAReadItsCallerLeavesInsideOneLongValueStopsThere(t *testing.T) {
	// Words a box might write that gzip packs only so far, so the archive is
	// read a piece at a time all through the value.
	rng := rand.New(rand.NewChaCha8([32]byte{}))
	var text strings.Builder
	for text.Len() < executor.TranscriptValueMaxBytes+4<<20 {
		for range 3 + rng.IntN(6) {
			text.WriteByte(byte('a' + rng.IntN(26)))
		}
		text.WriteByte(' ')
	}
	raw := gzipped(t, openCodeDoc(openCodeUser(text.String())))
	ctx, leave := context.WithCancel(t.Context())
	defer leave()
	archive := &leavingArchive{r: bytes.NewReader(raw), after: len(raw) / 8, leave: leave}
	_, err := executor.ReadTranscriptWithin(ctx, executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		func(context.Context) (io.ReadCloser, error) { return archive, nil }, 0,
		executor.TranscriptReadBounds())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("a read whose caller left inside a long value ended %v, want its caller's own error", err)
	}
	if archive.handed > len(raw)/2 {
		t.Errorf("a read whose caller left %d bytes into an archive of %d read %d of it, want it stopped a "+
			"piece past where its caller left", archive.after, len(raw), archive.handed)
	}
	if held := executor.TranscriptReadsHeld(); held != 0 {
		t.Errorf("%d slots are held once the read its caller left has ended, want none", held)
	}
}
