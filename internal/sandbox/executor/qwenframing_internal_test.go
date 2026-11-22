package executor

import "testing"

// A QWEN CODE NOTE IS READ AS ONE THROUGH QWEN CODE'S FRAMING. Qwen Code puts
// a drained message on a line after `[User message received during tool
// execution]: ` (its record of a note the measured run handed it,
// testdata/qwen/0.28.3/chat-note.jsonl), so a note it records opens with its
// heading after that framing and nothing else, and is filed as a note; a
// heading after any other words on its line still heads nothing.
//
// Mutations, each red: leave Qwen Code's framing out of headingFraming (the
// recorded note heads nothing); accept any bracketed words before a heading
// (the forged framing heads a note).
func TestAQwenCodeNoteIsReadThroughItsFraming(t *testing.T) {
	t.Parallel()
	note := noteHeadOpen + "eve" + noteHeadClose + "\nuse staging"
	recorded := "\n" + qwenNoteFraming + note
	if !opensWithHeading(recorded) {
		t.Errorf("Qwen Code's recorded note %q does not open with its heading", recorded)
	}
	var kinds []EntryKind
	var texts []string
	for kind, words := range headedIn(recorded) {
		kinds, texts = append(kinds, kind), append(texts, words)
	}
	if len(kinds) != 1 || kinds[0] != EntryNote || texts[0] != note {
		t.Errorf("the recorded note reads as %v %q, want one note %q", kinds, texts, note)
	}
	for _, forged := range []string{
		"\n[Tool output received during tool execution]: " + note,
		"\nsee [User message received during tool execution]: " + note,
	} {
		if opensWithHeading(forged) {
			t.Errorf("%q opens with a heading, though only Qwen Code's own framing may precede one", forged)
		}
	}
}
