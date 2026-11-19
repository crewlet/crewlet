package harnessprofile_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// AN IMAGE INPUT IS JUDGED WITH THE ARGV IT JOINS. Claude Code declares one
// whose web-denied run is still left with no tool of its own; an image input
// whose arguments bring a tool back fails that check, and one with no stdin
// prompt, an unknown format or no note is refused at load. Mutation: judge the
// argv without the image arguments and the tool server goes unseen.
func TestAnImageInputIsJudgedWithTheArgvItJoins(t *testing.T) {
	t.Parallel()
	claude, err := harnessprofile.Load("claude-code", map[string]any{"web_tools": "denied"})
	if err != nil {
		t.Fatal(err)
	}
	if claude.ImageInput == nil || claude.ImageInput.Format != harnessprofile.ImageClaudeStreamJSON {
		t.Fatalf("claude-code's image input = %+v", claude.ImageInput)
	}
	if problem := claude.ToolsOffProblem(); problem != "" {
		t.Errorf("claude-code's helper run with images has a tool of its own: %s", problem)
	}
	reopened, err := harnessprofile.Load("claude-code", map[string]any{"web_tools": "denied",
		"image_input": map[string]any{"args": []any{"--input-format", "stream-json",
			"--mcp-config", "servers.json"}, "format": "claude-stream-json", "note": "n"}})
	if err != nil {
		t.Fatal(err)
	}
	if problem := reopened.ToolsOffProblem(); !strings.Contains(problem, "with images") {
		t.Errorf("an image input that loads a tool server passed: %q", problem)
	}
	for name, input := range map[string]map[string]any{
		"no format": {"args": []any{"--input-format", "stream-json"}, "note": "n"},
		"no note":   {"args": []any{"--input-format", "stream-json"}, "format": "claude-stream-json"},
		"no args":   {"format": "claude-stream-json", "note": "n"},
	} {
		if _, err := harnessprofile.Load("pi", map[string]any{"image_input": input}); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	if _, err := harnessprofile.Load("claude-code", map[string]any{"prompt_mode": "argv",
		"prompt_args": []any{"-p"}}); err == nil {
		t.Error("an image input with its prompt on argv loaded")
	}
	if pi, _ := harnessprofile.Builtin("pi"); pi.ImageInput != nil {
		t.Error("pi declares an image input nobody measured")
	}
}
