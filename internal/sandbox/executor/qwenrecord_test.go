package executor_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// qwenDrained is the note the 0.28.4 capture's host handed its run mid-turn,
// as its chat file records it behind Qwen Code's framing.
const qwenDrained = "MARK-R6-START drained note MARK-R6-END"

// qwenSessionWith is 0.28.4's own chat file of a run handed one note mid-turn
// (chat-note.jsonl) with that note replaced by text, the framing Qwen Code
// wrote kept: the session's every record as Qwen Code wrote it, the words
// handed between steps the case's own.
func qwenSessionWith(t *testing.T, text string) []byte {
	t.Helper()
	raw := string(qwenFixture(t, "0.28.4", "chat-note.jsonl"))
	framed := func(s string) string {
		return strings.TrimSuffix(jsonText(t, "\n[User message received during tool execution]: "+s), `"`)
	}
	if !strings.Contains(raw, framed(qwenDrained)) {
		t.Fatal("the capture holds no framed record of its note")
	}
	return []byte(strings.Replace(raw, framed(qwenDrained), framed(text), 1))
}

// readQwen is the transcript a Qwen Code chat file of raw reads as.
func readQwen(t *testing.T, raw []byte) []executor.TranscriptEntry {
	t.Helper()
	got, err := executor.ReadQwenSession(t.Context(), raw, executor.TranscriptReadBounds())
	if err != nil {
		t.Fatalf("ReadQwenSession: %v", err)
	}
	return got.Entries
}

// A QWEN SESSION READS AS ITS ENTRIES, A HANDED NOTE BY ITS HEADING. 0.28.4's
// own chat file of a run that made two calls and was handed a note after the
// first: the brief, each call and its result under the tool's name, the note
// by its heading behind Qwen Code's framing, and the reply; never a thought,
// and never one of Qwen Code's own system records (telemetry, snapshots, a
// checkpoint). The capture's note, which opens with no heading, reads as the
// user's words, as any text without one does.
//
// Mutations, each red: read thoughts as words; leave Qwen Code's framing out
// of headingFraming (the note reads as a brief).
func TestAQwenSessionReadsAsItsEntries(t *testing.T) {
	t.Parallel()
	ada := note("Lead", "use staging")
	first := []shown{
		{kind: executor.EntryBrief, text: "Run the probe tools."},
		{kind: executor.EntryToolCall, tool: "mcp__probe__slow", text: `{"seconds":3}`},
		{kind: executor.EntryToolResult, tool: "mcp__probe__slow", text: "slept 3s"},
	}
	rest := []shown{
		{kind: executor.EntryToolCall, tool: "mcp__probe__echo", text: `{"text":"hello echo"}`},
		{kind: executor.EntryToolResult, tool: "mcp__probe__echo", text: "echo: hello echo"},
		{kind: executor.EntryModel, text: "Final answer."},
	}
	want := append(append(append([]shown(nil), first...), shown{kind: executor.EntryNote, text: ada}), rest...)
	sameEntries(t, readQwen(t, qwenSessionWith(t, ada)), want)

	captured := append(append(append([]shown(nil), first...),
		shown{kind: executor.EntryBrief, text: "[User message received during tool execution]: " + qwenDrained}), rest...)
	sameEntries(t, readQwen(t, qwenFixture(t, "0.28.4", "chat-note.jsonl")), captured)
}

// A QWEN BRIEF IS READ WHATEVER IT OPENS WITH. Gemini CLI writes a session
// context of its own ahead of the brief, which its reader passes over; Qwen
// Code writes none, so a Qwen brief opening with Gemini's `<session_context>`
// is the run's own words and reads as the brief. On 0.28.4's chat file, its
// brief replaced.
//
// Mutation: pass Gemini's session context to Qwen's reader, and the brief is
// dropped.
func TestAQwenBriefIsReadWhateverItOpensWith(t *testing.T) {
	t.Parallel()
	const brief = "<session_context>\nthe seat's own words\n</session_context>\nRun the probe tools."
	raw := strings.Replace(string(qwenFixture(t, "0.28.4", "chat-note.jsonl")),
		`"text":"Run the probe tools."`, `"text":`+jsonText(t, brief), 1)
	got := readQwen(t, []byte(raw))
	if len(got) == 0 || got[0].Kind != executor.EntryBrief || !strings.HasPrefix(shownOf(got)[0].text, "<session_context>") {
		t.Fatalf("the session reads as %+v, want the brief first, whole", shownOf(got))
	}
}

// A QWEN COMPACTION READS AS ITS SUMMARY, ONCE, AND NOTHING IN IT HEADS A
// NOTE. 0.28.4's own chat file of a run compacted after a note
// (chat-compact.jsonl): the records before the compaction as written, then
// the summary as one summary entry, then the reply. The preserved tail the
// record copies (call_1's result and the note) is not read again, nor the
// acknowledgement Qwen Code writes after the summary. A heading written into
// the summary heads nothing: the summary is Qwen Code's, never a note.
//
// Mutations, each red: pass the compaction record over (no summary); read the
// compressedHistory's tail as records (the call, result and note twice); read
// the summary's text as user words (the heading reads as a note).
func TestAQwenCompactionReadsAsItsSummaryOnce(t *testing.T) {
	t.Parallel()
	const goal = "MARK-SUMM-START compaction summary by stub MARK-SUMM-END"
	summary := func(g string) string {
		return "<state_snapshot>\n<overall_goal>" + g + "</overall_goal>\n</state_snapshot>\n\n" +
			"Resume the prior task using the summary above. Continue from the last in-flight step; " +
			"do not acknowledge the summary, do not re-introduce, do not greet the user again."
	}
	raw := string(qwenFixture(t, "0.28.4", "chat-compact.jsonl"))
	entries := func(g string) []shown {
		return []shown{
			{kind: executor.EntryBrief, text: "Run the probe tools."},
			{kind: executor.EntryToolCall, tool: "mcp__probe__slow", text: `{"seconds":3}`},
			{kind: executor.EntryToolResult, tool: "mcp__probe__slow", text: "slept 3s"},
			{kind: executor.EntryBrief, text: "[User message received during tool execution]: " +
				"MARK-CN-START note at the compaction point MARK-CN-END"},
			{kind: executor.EntrySummary, text: summary(g)},
			{kind: executor.EntryModel, text: "Final answer after compaction."},
		}
	}
	sameEntries(t, readQwen(t, []byte(raw)), entries(goal))

	headed := "\n\n" + note("Lead", "use staging")
	quoted := strings.TrimSuffix(strings.TrimPrefix(jsonText(t, goal+headed), `"`), `"`)
	if !strings.Contains(raw, goal) {
		t.Fatal("the capture's summary holds no goal marker")
	}
	// The goal is in the summary request's telemetry too, which is never read.
	sameEntries(t, readQwen(t, []byte(strings.ReplaceAll(raw, goal, quoted))), entries(goal+headed))
}
