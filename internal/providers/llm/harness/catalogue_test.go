package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// NO REQUEST RENDERS A TOOL CATALOGUE (ADR-0049). A phase's tools reach its CLI
// as the tools of one MCP server and nowhere else, so the prompt the CLI reads
// names none of them, describes none and asks for no reply in which to call
// one: the CLI calls them the way it calls any tool, and the engine never reads
// a call out of a model's prose. A catalogue rendered back into the transcript
// would be a second tool channel the dispatcher's gates do not stand in front
// of, and a contract the vendor's model was never trained to follow.
func TestNoRequestRendersAToolCatalogue(t *testing.T) {
	t.Parallel()
	promptFile := filepath.Join(t.TempDir(), "prompt.txt")
	p := fakeClaude(t, fakeScript{PromptTo: promptFile, Responses: []fakeResponse{
		{Text: "done", Usage: fakeUsage{Input: 1, Output: 1}},
	}}, nil)
	if _, err := p.Complete(t.Context(), phase(newTestHost(t, "zz_catalogue_probe"))); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	raw, err := os.ReadFile(promptFile) //nolint:gosec // the test's own dir
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(raw)
	for _, needle := range []string{"zz_catalogue_probe", "the zz_catalogue_probe tool", "tool_calls", `"arguments"`} {
		if strings.Contains(prompt, needle) {
			t.Errorf("the prompt the CLI read carries %q — a catalogue or a reply contract:\n%s", needle, prompt)
		}
	}
}
