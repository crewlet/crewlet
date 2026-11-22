package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// geminiTwinNote is the note the runner's own capture blocked its finish with
// twice (session-twin-end), as the take's stand-in bridge rendered it.
const geminiTwinNote = "## A note from Lead, sent while this turn was running: CAP-TWIN-END"

// geminiSessionWith is the runner's own capture of a whole 0.70.0 run
// (session-twin-end) with each delivery of its note replaced by text: the
// session's every shape as Gemini CLI wrote it, the words a finish was handed
// the case's own.
func geminiSessionWith(t *testing.T, text string) []byte {
	t.Helper()
	raw := string(geminiFixture(t, "session-twin-end.jsonl"))
	if !strings.Contains(raw, jsonText(t, geminiTwinNote)) {
		t.Fatal("the capture holds no delivery of its note")
	}
	return []byte(strings.ReplaceAll(raw, jsonText(t, geminiTwinNote), jsonText(t, text)))
}

// readGemini is the transcript a Gemini CLI session of raw reads as.
func readGemini(t *testing.T, raw []byte) []executor.TranscriptEntry {
	t.Helper()
	got, err := executor.ReadGeminiSession(t.Context(), raw, executor.TranscriptReadBounds())
	if err != nil {
		t.Fatalf("ReadGeminiSession: %v", err)
	}
	return got.Entries
}

// A GEMINI SESSION READS AS ITS ENTRIES, EACH RESPONSE ONCE. The runner's own
// capture of a 0.70.0 run that made two calls three times over, its finish
// blocked twice by a note: the brief (never the harness's session context),
// each call and its result under the tool's name, each reply, and each note
// by its heading. 0.70.0 wrote every response up to three times (its words,
// its calls scheduled, its calls run), and each call reads once.
//
// Mutations, each red: read every copy of a response (each call three times);
// read the session context as a brief; tell a note from a brief by anything
// but its heading.
func TestAGeminiSessionReadsAsItsEntries(t *testing.T) {
	t.Parallel()
	ada := note("Lead", "CAP-TWIN-END")
	got := readGemini(t, geminiSessionWith(t, ada))
	round := []shown{
		{kind: executor.EntryToolCall, tool: "mcp_crewlet_slow", text: `{"seconds":1}`},
		{kind: executor.EntryToolResult, tool: "mcp_crewlet_slow", text: "<untrusted_context>\nslept 1s\n</untrusted_context>"},
		{kind: executor.EntryToolCall, tool: "mcp_crewlet_echo", text: `{"text":"hello"}`},
		{kind: executor.EntryToolResult, tool: "mcp_crewlet_echo", text: "<untrusted_context>\necho: hello\n</untrusted_context>"},
		{kind: executor.EntryModel, text: "STUB-ANSWER: done after mcp_crewlet_echo."},
	}
	want := []shown{{kind: executor.EntryBrief, text: "Call the slow tool, then echo, then answer."}}
	want = append(want, round...)
	want = append(want, shown{kind: executor.EntryNote, text: ada})
	want = append(want, round...)
	want = append(want, shown{kind: executor.EntryNote, text: ada})
	want = append(want, round...)
	sameEntries(t, got, want)
}

// A GEMINI COMPRESSION READS AS ITS SUMMARY, AS CLAUDE CODE'S COMPACTION DOES:
// every entry before it stays, the `<state_snapshot>` entry is one summary
// (a note's heading inside it heads nothing), and nothing is read twice. On
// 0.70.0's own session of two compressions (r6), the second of which folded
// a note sent plain before it and kept a copy of it and of the call after it:
// the harness's "Got it" and the copies are its restatement, never read, and
// the call's result after them is still named by the call read before.
//
// Mutations, each red: read the snapshot as an ordinary user entry; read the
// restatement up to the `$patch`; never end a restatement.
func TestAGeminiCompressionReadsAsItsSummaryAndNothingTwice(t *testing.T) {
	t.Parallel()
	const delivered = "NOTE-END ## A note from Lead, sent while this turn was running: R6-NOTE-second"
	lead := note("Lead", "R6-NOTE-second")
	raw := string(geminiFixture(t, "session-compressed.jsonl"))
	if strings.Count(raw, jsonText(t, delivered)) != 2 || strings.Count(raw, "SUMMARY-MARK sawNote=false") != 2 {
		t.Fatal("the capture holds no note with its kept copy, or not two compressions")
	}
	inner := jsonText(t, "SUMMARY-MARK\n"+lead)
	raw = strings.ReplaceAll(raw, jsonText(t, delivered), jsonText(t, lead))
	raw = strings.ReplaceAll(raw, "SUMMARY-MARK sawNote=false", inner[1:len(inner)-1])
	summary := "<state_snapshot>SUMMARY-MARK\n" + lead + "</state_snapshot>"
	sameEntries(t, readGemini(t, []byte(raw)), []shown{
		{kind: executor.EntryBrief, text: "go"},
		{kind: executor.EntryToolCall, tool: "mcp_probe_slow", text: `{"seconds":1}`},
		{kind: executor.EntryToolResult, tool: "mcp_probe_slow", text: "<untrusted_context>\nslept 1s\n</untrusted_context>"},
		{kind: executor.EntryToolCall, tool: "mcp_probe_echo", text: `{"text":"hello"}`},
		{kind: executor.EntryToolResult, tool: "mcp_probe_echo", text: "<untrusted_context>\necho: hello\n</untrusted_context>"},
		{kind: executor.EntryModel, text: "STUB-ANSWER: done after mcp_probe_echo."},
		{kind: executor.EntrySummary, text: summary},
		{kind: executor.EntryNote, text: lead},
		{kind: executor.EntryToolCall, tool: "mcp_probe_echo", text: `{"text":"after-note"}`},
		{kind: executor.EntrySummary, text: summary},
		{kind: executor.EntryToolResult, tool: "mcp_probe_echo",
			text: "<untrusted_context>\necho: after-note\n</untrusted_context>"},
		{kind: executor.EntryModel, text: "STUB-ANSWER: done after the note turn."},
	})
}
