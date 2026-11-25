package executor_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A NOTE IS MEASURED AS CLAUDE CODE MEASURES IT, in UTF-16 code units: a
// budget in characters lets through an answer the harness moves into a file.
func TestANoteIsMeasuredInUTF16Units(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{
		"":         0,
		"abc":      3,
		"é":        1,
		"日本":       2,
		"🚀":        2,
		"a🚀b":      4,
		"\xff\xfe": 2,
		"note: 🚀🚀": 10,
	} {
		if got := executor.UTF16Len(in); got != want {
			t.Errorf("UTF16Len(%q) = %d, want %d", in, got, want)
		}
	}
}

// AN ANSWER CARRIES THE OLDEST NOTES THAT FIT, counted as joined, and always
// at least one, or an oversized note would wait for ever.
func TestAHookAnswerCarriesTheOldestNotesThatFit(t *testing.T) {
	t.Parallel()
	half := strings.Repeat("a", executor.ClaudeContextBudget/2-1)
	if got := executor.HookNotesThatFit(executor.ClaudeCodeName, []string{half, half, "x"}); got != 2 {
		t.Errorf("two half-budget notes and a third: %d fit, want 2", got)
	}
	// ASTRAL CHARACTERS: in runes these fit, in UTF-16 units they do not.
	astral := strings.Repeat("🚀", executor.ClaudeContextBudget/4+1)
	if got := executor.HookNotesThatFit(executor.ClaudeCodeName, []string{astral, astral}); got != 1 {
		t.Errorf("two notes each over a quarter of the budget in astral characters: %d fit, "+
			"want 1", got)
	}
	whole := strings.Repeat("a", executor.ClaudeContextBudget+100)
	if got := executor.HookNotesThatFit(executor.ClaudeCodeName, []string{whole, "x"}); got != 1 {
		t.Errorf("a note past the budget on its own: %d carried, want it alone", got)
	}
	if got := executor.HookNotesThatFit(executor.ClaudeCodeName, nil); got != 0 {
		t.Errorf("no notes: %d", got)
	}
}

// THE ANSWER IS CONTEXT AND A TRANSCRIPT LINE, NEVER A DECISION: `decision`,
// `continue` or `reason` could stop the loop or block a step.
func TestAHookAnswerIsContextAndNeverADecision(t *testing.T) {
	t.Parallel()
	got := executor.HookAnswer(executor.ClaudeCodeName, "Stop", hookTake, []executor.HookNote{
		{By: "ada", Message: "## A note from ada\nuse <staging> & main"},
		{By: "bob", Message: "## A note from bob\ntag it"},
		{By: "ada", Message: "## A note from ada\nand push"},
	})
	var answer map[string]any
	if err := json.Unmarshal([]byte(got), &answer); err != nil {
		t.Fatalf("the answer is not JSON: %v\n%s", err, got)
	}
	for _, key := range []string{"decision", "continue", "reason", "stopReason"} {
		if _, ok := answer[key]; ok {
			t.Errorf("the answer carries %q: %s", key, got)
		}
	}
	out, _ := answer["hookSpecificOutput"].(map[string]any)
	if out["hookEventName"] != "Stop" || out["additionalContext"] !=
		"## A note from ada\nuse <staging> & main\n\n## A note from bob\ntag it\n\n## A note from ada\nand push" {
		t.Errorf("the hook output is %v", out)
	}
	if answer["systemMessage"] != "crewlet: 3 notes from ada, bob reached the run as it was finishing "+hookToken {
		t.Errorf("the transcript line is %q", answer["systemMessage"])
	}
	if !strings.Contains(got, "<staging> & main") {
		t.Errorf("the note's own characters were escaped: %s", got)
	}
	one := executor.HookAnswer(executor.ClaudeCodeName, "PostToolBatch", hookTake, []executor.HookNote{{By: "ada", Message: "m"}})
	if !strings.Contains(one, `"systemMessage":"crewlet: a note from ada reached the run `+hookToken+`"`) {
		t.Errorf("one note after a batch answered %s", one)
	}
	if none := executor.HookAnswer(executor.ClaudeCodeName, "Stop", hookTake, nil); none != "" {
		t.Errorf("no notes answered %q, want nothing", none)
	}
}

// hookTake is a take a hook's answer names, and hookToken the token it names
// it by.
const (
	hookTake  = "0b7e3c2a-5d41-4f6e-9a8b-1c2d3e4f5a6b"
	hookToken = "[crewlet take " + hookTake + "]"
)

// A HOOK'S ANSWER NAMES ITS TAKE, AND THE LIVE LINE LEAVES IT OUT: the answer's
// system message ends in the take's token, which the job's session then shows
// beside the note and a collection reads the serve's receipt by; a person
// reading the run's live transcript, or a captured session's, sees what the
// hook said without it. A token of any other shape, or one the line does not
// end in, is a person's words and stays.
//
// Mutation: leave the token on the live line (claudeSteerSaid without the cut)
// or on the session's (claudeHookSaid without it), and that transcript shows
// it; drop it from the answer, and the answer names no take.
func TestAHookAnswerNamesItsTakeAndTheLiveLineLeavesItOut(t *testing.T) {
	t.Parallel()
	for event, said := range map[string]string{
		"PostToolBatch": "a note from ada reached the run",
		"Stop":          "a note from ada reached the run as it was finishing",
	} {
		answer := executor.HookAnswer(executor.ClaudeCodeName, event, hookTake, []executor.HookNote{{By: "ada", Message: "m"}})
		var out struct {
			SystemMessage string `json:"systemMessage"`
		}
		if err := json.Unmarshal([]byte(answer), &out); err != nil {
			t.Fatalf("the answer is not JSON: %v\n%s", err, answer)
		}
		if !strings.HasSuffix(out.SystemMessage, " "+hookToken) {
			t.Errorf("%s: the answer's system message %q does not end in its take's token", event,
				out.SystemMessage)
		}
		line := `{"type":"system","subtype":"informational","content":` +
			jsonText(t, event+" says: "+out.SystemMessage) + `,"timestamp":"2026-10-30T12:00:03.100Z"}`
		if got := decodeClaude(line); got != "[steer] "+said {
			t.Errorf("%s: the live line reads %q, want %q", event, got, "[steer] "+said)
		}
		described, raw := claudeCaptureOf(t, []string{hookSaid(t, event, out.SystemMessage,
			"2026-10-30T12:00:03.100Z")}, nil)
		got, err := executor.ReadTranscript(t.Context(), described.Harness, described.Format,
			opener(bytes.NewReader(raw)), 0)
		if err != nil {
			t.Fatalf("ReadTranscript: %v", err)
		}
		sameEntries(t, got.Entries, []shown{{kind: executor.EntryNote, text: said}})
	}
	for _, kept := range []string{
		"a note from ada reached the run [crewlet take not-a-uuid-at-all-xxxxxxxxxxxxxxxxxx]",
		"a note from ada reached the run [crewlet take " + strings.ToUpper(hookTake) + "]",
		"a note from ada reached the run " + hookToken + " and more",
	} {
		line := `{"type":"system","subtype":"informational","content":` +
			jsonText(t, "PostToolBatch says: crewlet: "+kept) + `}`
		if got := decodeClaude(line); got != "[steer] "+kept {
			t.Errorf("the live line reads %q, want the hook's words whole: %q", got, kept)
		}
	}
}
