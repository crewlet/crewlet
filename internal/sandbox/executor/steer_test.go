package executor_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// A NOTE IS MEASURED AS CLAUDE CODE MEASURES IT: JavaScript's length, in UTF-16
// code units, where a character outside the Basic Multilingual Plane is two. A
// budget counted in characters lets through an answer the harness moves into a file
// and previews, which reaches the model as a pointer to the note.
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

// AN ANSWER CARRIES THE OLDEST NOTES THAT FIT, and always at least one: the
// budget is counted on the notes joined as the answer joins them, and a note
// past the budget on its own is still carried rather than left to wait for ever.
func TestAHookAnswerCarriesTheOldestNotesThatFit(t *testing.T) {
	t.Parallel()
	half := strings.Repeat("a", executor.ClaudeContextBudget/2-1)
	if got := executor.ClaudeNotesThatFit([]string{half, half, "x"}); got != 2 {
		t.Errorf("two half-budget notes and a third: %d fit, want 2", got)
	}
	// ASTRAL CHARACTERS: in runes these fit, in UTF-16 units they do not.
	astral := strings.Repeat("🚀", executor.ClaudeContextBudget/4+1)
	if got := executor.ClaudeNotesThatFit([]string{astral, astral}); got != 1 {
		t.Errorf("two notes each over a quarter of the budget in astral characters: %d fit, "+
			"want 1", got)
	}
	whole := strings.Repeat("a", executor.ClaudeContextBudget+100)
	if got := executor.ClaudeNotesThatFit([]string{whole, "x"}); got != 1 {
		t.Errorf("a note past the budget on its own: %d carried, want it alone", got)
	}
	if got := executor.ClaudeNotesThatFit(nil); got != 0 {
		t.Errorf("no notes: %d", got)
	}
}

// THE ANSWER IS CONTEXT AND A TRANSCRIPT LINE, NEVER A DECISION: a hook answer
// that carried `decision`, `continue` or `reason` could stop the run's loop or
// block its step, and a note must only ever add to what the model reads.
func TestAHookAnswerIsContextAndNeverADecision(t *testing.T) {
	t.Parallel()
	got := executor.ClaudeHookAnswer("Stop", []executor.HookNote{
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
	if answer["systemMessage"] != "crewlet: 3 notes from ada, bob reached the run as it was finishing" {
		t.Errorf("the transcript line is %q", answer["systemMessage"])
	}
	if !strings.Contains(got, "<staging> & main") {
		t.Errorf("the note's own characters were escaped: %s", got)
	}
	one := executor.ClaudeHookAnswer("PostToolBatch", []executor.HookNote{{By: "ada", Message: "m"}})
	if !strings.Contains(one, `"systemMessage":"crewlet: a note from ada reached the run"`) {
		t.Errorf("one note after a batch answered %s", one)
	}
	if none := executor.ClaudeHookAnswer("Stop", nil); none != "" {
		t.Errorf("no notes answered %q, want nothing", none)
	}
}

// AN INBOX ID IS THE JOB'S AND THE NOTE'S, AND STABLE: OpenCode's message ids are
// unique across a box's whole database, which a relaunch in the same box
// shares, and a second admission under the same id is the first one.
func TestAnOpenCodeInboxIDIsStableAndTheJobsOwn(t *testing.T) {
	t.Parallel()
	id := executor.OpenCodeInboxID("job-1", "note-1")
	if id != executor.OpenCodeInboxID("job-1", "note-1") {
		t.Error("one note's id is not stable for its job")
	}
	if !strings.HasPrefix(id, "msg_") || len(id) != len("msg_")+32 {
		t.Errorf("the id %q is not msg_ and 32 hex digits", id)
	}
	for _, other := range []string{
		executor.OpenCodeInboxID("job-2", "note-1"),
		executor.OpenCodeInboxID("job-1", "note-2"),
		executor.OpenCodeInboxID("job-1note-1", ""),
	} {
		if other == id {
			t.Errorf("two notes or two jobs share the id %q", id)
		}
	}
}
