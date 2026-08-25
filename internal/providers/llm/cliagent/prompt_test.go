package cliagent

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The transcript is the only channel a CLI has, so every message must arrive
// under a heading naming its author: a model handed an unlabelled blob cannot
// tell the system prompt from the user's ask.
func TestTheTranscriptLabelsEveryAuthor(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "you are Dev"},
		{Role: llm.RoleUser, Content: "fix the build"},
		{Role: llm.RoleAssistant, Content: "on it"},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{"## system", "you are Dev", "## user", "fix the build",
		"## assistant", "on it"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "## system") > strings.Index(got, "## user") {
		t.Error("the transcript is out of order")
	}
}

// A tool result must name the call it answers, or a model reading a
// transcript with two parallel calls in it cannot pair them.
func TestAToolResultNamesTheCallItAnswers(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleTool, Name: "read_file", ToolCallID: "cli_0", Content: "file contents"},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{"tool result", "read_file", "cli_0", "file contents"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// A model shown its previous answer in one format and asked to reply in
// another produces the format it was shown, so prior calls render in the
// same shape the contract asks for.
func TestPriorToolCallsRenderInTheContractShape(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: "checking", ToolCalls: []llm.ToolCall{
			{ID: "cli_0", Name: "read_file", Arguments: map[string]any{"path": "go.mod"}},
		}},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if !strings.Contains(got, `"tool_calls"`) || !strings.Contains(got, `"read_file"`) {
		t.Errorf("a prior call did not render as an envelope:\n%s", got)
	}
	// And it round-trips through the parser that reads the model's own answers.
	env := ParseEnvelope(got)
	if !env.Parsed || len(env.ToolCalls) != 1 || env.ToolCalls[0].Name != "read_file" {
		t.Errorf("the rendered call does not parse back: %+v", env)
	}
}

// Thinking blocks are one provider's opaque structures, valid only when
// handed back to that provider on its own wire. Pasting them into a prompt
// for a different model is prose that looks like data.
func TestThinkingBlocksAreNotPastedIntoTheTranscript(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: "the answer", ThinkingBlocks: []llm.ThinkingBlock{
			{Type: "thinking", Thinking: "secret deliberation", Signature: "sig-abc"},
		}},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, leaked := range []string{"secret deliberation", "sig-abc"} {
		if strings.Contains(got, leaked) {
			t.Errorf("a provider's opaque block reached the transcript: %q", leaked)
		}
	}
	// Reasoning PROSE is kept, because it reads as prose to any model.
	withProse, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleAssistant, ReasoningContent: "I checked the log", Content: "done"},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	if !strings.Contains(withProse, "I checked the log") {
		t.Errorf("reasoning prose was dropped:\n%s", withProse)
	}
}

// The catalogue carries the schema, or the model guesses argument names.
func TestTheToolCatalogueCarriesTheSchema(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
		Tools: []llm.ToolDef{{
			Name: "read_file", Description: "Read a file.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		}},
	})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, want := range []string{"read_file", "Read a file.", `"path"`, `"required"`} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

// The estimate is the fallback budgets run on when a CLI reports nothing, so
// it has to be in the right order of magnitude rather than merely non-zero.
func TestTheTokenEstimateIsAboutFourCharactersAToken(t *testing.T) {
	t.Parallel()
	if got := EstimateTokens(""); got != 0 {
		t.Errorf("EstimateTokens(\"\") = %d", got)
	}
	if got := EstimateTokens(strings.Repeat("x", 4000)); got != 1000 {
		t.Errorf("EstimateTokens(4000 chars) = %d, want 1000", got)
	}
	if got := EstimateTokens("a"); got != 1 {
		t.Errorf("EstimateTokens(\"a\") = %d, want 1 — a short prompt must not round to nothing", got)
	}
}
