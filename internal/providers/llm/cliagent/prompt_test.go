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

// A PRIOR CALL IS SHOWN TO THE MODEL AS IT WROTE IT. The transcript is text
// the model reads as written, not through a JSON decoder, and it replies in
// the format it was shown: an HTML-safe encoder would hand back the call it
// made with `\u003c` and `\u0026` in place of what it typed. Mutation: put
// json.MarshalIndent back in renderPriorCalls and the escapes appear.
func TestAPriorCallIsShownToTheModelAsItWroteIt(t *testing.T) {
	t.Parallel()
	const code = "if a < b && c > d"
	const url = "https://example.com/?a=1&b=2"
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: "checking", ToolCalls: []llm.ToolCall{
			{ID: "cli_0", Name: "run", Arguments: map[string]any{"code": code, "url": url}},
		}},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, verbatim := range []string{code, url} {
		if !strings.Contains(got, verbatim) {
			t.Errorf("the prior call does not carry %q as written:\n%s", verbatim, got)
		}
	}
	for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, escape) {
			t.Errorf("the prior call carries %s, an escape the model never wrote:\n%s", escape, got)
		}
	}
	// And it is still the contract's envelope, read back as the call it was.
	env := ParseEnvelope(got)
	if !env.Parsed || len(env.ToolCalls) != 1 || env.ToolCalls[0].Arguments["code"] != code {
		t.Errorf("the rendered call does not parse back as written: %+v", env)
	}
}

// Thinking blocks are one provider's opaque structures, valid only when
// handed back to that provider on its own wire. Pasting them into a prompt
// for a different model is prose that looks like data.
func TestThinkingBlocksAreNotPastedIntoTheTranscript(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleAssistant, Content: "the answer", ThinkingBlocks: []llm.ThinkingBlock{
			{Type: "thinking", Thinking: "secret deliberation"},
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

// THE CATALOGUE CARRIES MARKUP AS WRITTEN. It is the one place a text-mode
// model learns a tool's grammar, and it reads the catalogue as text: a
// description saying `f.<slug>` or `>3` reached it as `\u003c` and `\u003e`
// escapes, a grammar nobody wrote. The block stays the indented catalogue it
// was. Mutation: put json.MarshalIndent back in renderTools and the escapes
// appear; pass it no indent and the layout assertion fails.
func TestTheToolCatalogueCarriesMarkupAsWritten(t *testing.T) {
	t.Parallel()
	const description = "Sort by `f.<slug>`; `>3` on a number & co."
	const property = "a field's `f.<slug>`"
	got, err := RenderPrompt(llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "go"}},
		Tools: []llm.ToolDef{{
			Name: "list_work", Description: description,
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"sort": map[string]any{"type": "string", "description": property},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	for _, verbatim := range []string{description, property} {
		if !strings.Contains(got, verbatim) {
			t.Errorf("the catalogue does not carry %q as written:\n%s", verbatim, got)
		}
	}
	for _, escape := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, escape) {
			t.Errorf("the catalogue carries %s, an escape nobody wrote:\n%s", escape, got)
		}
	}
	if !strings.Contains(got, "[\n  {\n    \"name\": \"list_work\"") {
		t.Errorf("the catalogue is no longer the indented list it was:\n%s", got)
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

// A USER MESSAGE AFTER TOOL RESULTS — a person's note to a running turn
// (internal/agent/steer) — renders under its own `## user` heading AFTER every
// result, so a CLI reading the transcript never reads the note as part of a
// tool's output.
func TestAUserMessageAfterToolResultsIsItsOwnSectionAfterThem(t *testing.T) {
	t.Parallel()
	got, err := RenderPrompt(llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "fix the build"},
		{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
			{ID: "cli_0", Name: "read_file"}, {ID: "cli_1", Name: "list_dir"},
		}},
		{Role: llm.RoleTool, Name: "read_file", ToolCallID: "cli_0", Content: "file contents"},
		{Role: llm.RoleTool, Name: "list_dir", ToolCallID: "cli_1", Content: "a b c"},
		{Role: llm.RoleUser, Content: "a note from the founder"},
	}})
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	last := strings.LastIndex(got, "## user")
	note := strings.Index(got, "a note from the founder")
	second := strings.Index(got, "a b c")
	if last < 0 || note < last || second < 0 || last < second {
		t.Fatalf("the note is not its own user section after both results:\n%s", got)
	}
	if strings.Count(got, "## user") != 2 {
		t.Errorf("want the ask and the note as two user sections:\n%s", got)
	}
}
