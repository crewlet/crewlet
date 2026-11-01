package executor_test

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A captured session read back as its transcript. Every archive here is
// PACKED BY THE RUNNER'S OWN SCRIPT — a run in a real box whose stand-in
// harness files its record where the pinned layout names, or the OpenCode
// run's script exporting through the fake opencode — so a case reads exactly
// the bytes a collection keeps, never a tar this file built by hand.

// plantedToken is a GitHub token's shape, which the redaction knows: what a
// harness's record carries when a run read a file holding one.
var plantedToken = "ghp_" + strings.Repeat("Qx7Rb2", 6)

// claudeTranscript is the record the stand-in Claude Code files for its
// session: a brief, the model's words, its thinking, a tool call answered as a
// failure that quotes a token, the harness's own bookkeeping, a compaction
// summary, a line that is no record, a subagent's task and its result, and a
// last message of a couple of kibibytes.
var claudeTranscript = []string{
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

// transcriptHarness is a stand-in `claude` that files transcript and sidecar
// as the session it was started in, where the pinned layout names them —
// beside a tool result spilled to a file and a task list, which a capture packs
// and a transcript does not read — and ends on a successful result.
func transcriptHarness(transcript, sidecar []string) string {
	return `#!/bin/sh
if [ "$1" = --version ]; then echo '2.1.310 (Claude Code)'; exit 0; fi
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
p=$CLAUDE_CONFIG_DIR/projects/$CLAUDE_CODE_PROJECT_DIR_NAME
mkdir -p "$p/$s/subagents" "$p/$s/tool-results" "$CLAUDE_CONFIG_DIR/tasks/$s"
cat > "$p/$s.jsonl" <<'TRANSCRIPT'
` + strings.Join(transcript, "\n") + `
TRANSCRIPT
cat > "$p/$s/subagents/agent-a1.jsonl" <<'SIDECAR'
` + strings.Join(sidecar, "\n") + `
SIDECAR
echo spilled > "$p/$s/tool-results/toolu_1.txt"
echo '[]' > "$CLAUDE_CONFIG_DIR/tasks/$s/1.json"
printf '{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"%s"}\n' "$s"
`
}

// claudeCaptureOf runs the stand-in in a real box and answers the archive the
// run packed, as the collection describes it, and its bytes.
func claudeCaptureOf(t *testing.T, transcript, sidecar []string) (sandbox.SessionArchive, []byte) {
	t.Helper()
	runner, b := realBox(t)
	writeBoxStandIn(t, b, executor.PathsFor(b).BinDir()+"/claude", transcriptHarness(transcript, sidecar))
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
	described, raw := claudeCaptureOf(t, claudeTranscript, claudeSidecar)
	got, err := executor.ReadTranscript(described.Harness, described.Format, bytes.NewReader(raw), 0)
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
	raw := openCodeCaptureOf(t, openCodeExportDoc())
	got, err := executor.ReadTranscript(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		bytes.NewReader(raw), 0)
	if err != nil {
		t.Fatalf("ReadTranscript: %v", err)
	}
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
		page, err := executor.ReadTranscriptWithin(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
			bytes.NewReader(raw), from, bounds)
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

// AN ENTRY IS REDACTED WHOLE BEFORE IT IS CUT. A tool's output longer than an
// entry's bound, a token straddling the bound, reads cut to the bound and
// marked, its whole size stated — and with no part of the token in it: cut
// first, the token's opening would no longer be a token the redaction knows.
//
// Mutation: cut the text before redacting it, and the opening of the token is
// handed out.
func TestAnEntryIsRedactedWholeBeforeItIsCut(t *testing.T) {
	t.Parallel()
	output := strings.Repeat("x", 50) + " " + plantedToken + " the rest of the output"
	export := `{"info":{},"messages":[{"info":{"role":"assistant"},"parts":[{"type":"tool","tool":"bash",` +
		`"state":{"status":"completed","input":{},"output":"` + output + `"}}]}]}`
	raw := openCodeCaptureOf(t, export)
	got, err := executor.ReadTranscriptWithin(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		bytes.NewReader(raw), 0, within(func(b *executor.TranscriptBounds) { b.EntryBytes = 64 }))
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
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
	first, err := executor.ReadTranscriptWithin(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		bytes.NewReader(raw), 0, bounds)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
	if first.Cut != executor.CutRead || first.More() || len(first.Entries) == 0 || len(first.Entries) >= 50 {
		t.Fatalf("the read held %d entries cut %q (more %v), want some of the fifty, cut at the budget",
			len(first.Entries), first.Cut, first.More())
	}
	again, err := executor.ReadTranscriptWithin(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		bytes.NewReader(raw), first.Next, bounds)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin from %d: %v", first.Next, err)
	}
	if len(again.Entries) != 0 || again.Cut != executor.CutRead {
		t.Errorf("a read from where the budget stopped one held %d entries cut %q, want none, "+
			"cut at the same budget", len(again.Entries), again.Cut)
	}
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
	described, raw := claudeCaptureOf(t, claudeTranscript, claudeSidecar)
	small := within(func(b *executor.TranscriptBounds) { b.LineBytes = 1 << 10 })
	got, err := executor.ReadTranscriptWithin(described.Harness, described.Format,
		bytes.NewReader(raw), 0, small)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
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
	open, err := executor.ReadTranscriptWithin(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		bytes.NewReader(openCodeCaptureOf(t, export)), 0, small)
	if err != nil {
		t.Fatalf("ReadTranscriptWithin: %v", err)
	}
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
// build does not pack is refused as unsupported before a byte is read; bytes
// that are no gzip, and a gzip that holds no tar, are a malformed archive; and
// a source that fails part way — an object the store stopped answering for, a
// seal that no longer opens — is that failure, wrapped, never a malformed
// archive, whatever the decompression made of the stream stopping.
//
// Mutation: report the source's failure as a malformed archive, or read an
// unknown format as one this build packs, and a row goes red.
func TestAnArchiveIsBlamedOnlyForItsOwnFaults(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		harness string
		format  int
	}{{"codex", 1}, {executor.ClaudeCodeName, executor.ClaudeArchiveFormat + 1}} {
		_, err := executor.ReadTranscript(c.harness, c.format, strings.NewReader(""), 0)
		if !errors.Is(err, executor.ErrUnsupportedArchive) {
			t.Errorf("%s's format %d: %v, want ErrUnsupportedArchive", c.harness, c.format, err)
		}
	}
	for name, raw := range map[string][]byte{
		"bytes that are no gzip":   []byte("not an archive"),
		"a gzip that holds no tar": gzipped(t, strings.Repeat("not a tar header ", 64)),
	} {
		_, err := executor.ReadTranscript(executor.ClaudeCodeName, executor.ClaudeArchiveFormat,
			bytes.NewReader(raw), 0)
		if !errors.Is(err, executor.ErrMalformedArchive) {
			t.Errorf("%s: %v, want ErrMalformedArchive", name, err)
		}
	}
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write([]byte(briefs(20))); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	stopped := errors.New("the store stopped answering")
	_, err := executor.ReadTranscript(executor.OpenCodeName, executor.OpenCodeArchiveFormat,
		&failingReader{data: packed.Bytes()[:packed.Len()/2], err: stopped}, 0)
	if !errors.Is(err, stopped) || errors.Is(err, executor.ErrMalformedArchive) {
		t.Errorf("a source that failed part way: %v, want its own failure and no malformed archive", err)
	}
}

// THE TRANSCRIPT'S BOUNDS ARE THE ENGINE'S OWN, and hold to what their values
// argue: a page holds sixty-four entries at the entry's bound, a line is read to
// the bound every read of a run's record takes, and the read budget is eight
// times the most an archive is stored as — gzip's ratio over text — so an
// ordinary session at its bound reads whole.
//
// Mutation: raise the entry's bound past a sixty-fourth of the page's, read
// lines to a bound of their own, or shrink the budget under eight archives, and
// a row goes red.
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
}
