package builtin

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tools"
)

// A TOOL ANSWER PAST ITS CEILING IS REFUSED AS PAST IT, its size stated rounded
// up: one byte over reads as the next KiB beside the ceiling, never as the
// ceiling itself, which is a refusal of an answer that seems to be AT its limit
// and says nothing about how much to narrow. One exactly at the ceiling is
// served whole.
func TestAnAnswerPastTheCeilingStatesItsSizeRoundedUp(t *testing.T) {
	t.Parallel()
	// A JSON string encodes as its characters and two quotes.
	over, err := jsonAnswer(strings.Repeat("a", ToolAnswerBytes-1), "Narrow it.")
	if err != nil {
		t.Fatal(err)
	}
	if !over.Failed || tools.RefusalOf(over) != tools.RefusalInvalid {
		t.Fatalf("an answer a byte past the ceiling = %+v, want it refused invalid", over)
	}
	for _, want := range []string{
		fmt.Sprintf("That answer is %d KiB", ToolAnswerBytes>>10+1),
		fmt.Sprintf("one tool answer is %d KiB", ToolAnswerBytes>>10),
	} {
		if !strings.Contains(over.Output, want) {
			t.Errorf("the refusal says %q, want it to say %q", over.Output, want)
		}
	}

	at, err := jsonAnswer(strings.Repeat("a", ToolAnswerBytes-2), "Narrow it.")
	if err != nil {
		t.Fatal(err)
	}
	if at.Failed || len(at.Output) != ToolAnswerBytes {
		t.Fatalf("an answer exactly at the ceiling = %d bytes, failed %v; want it served whole",
			len(at.Output), at.Failed)
	}
}
