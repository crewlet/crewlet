package cliagent

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A CALL THAT OFFERED NO TOOLS GETS ITS ANSWER WHOLE, even when the answer is
// a JSON object carrying a key the envelope reader takes for the message.
// Auxiliary work asks for exactly such objects — the persist decider's
// `{"kind": ..., "content": ...}` — and an envelope read hands back the one
// field, so the caller's decode fails and its decision is dropped.
//
// The counterfactual is the same reply to a call that DID offer tools, which
// is read as the envelope the contract asked for: without it this passes for
// a backend that never parses an envelope at all.
//
// Mutation: parse the envelope whatever the request offered, and the first
// case comes back as "keep this".
func TestAnAnswerToACallWithoutToolsIsNeverReadAsAnEnvelope(t *testing.T) {
	t.Parallel()
	const reply = `{"kind": "LONG", "content": "keep this"}`
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": reply}, nil)

	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != reply {
		t.Errorf("a call that offered no tools came back as %q, want the answer verbatim %q",
			comp.Content, reply)
	}

	comp, err = ask(t, p, llm.Request{Tools: []llm.ToolDef{{
		Name: "note", Description: "Take a note.",
		Parameters: map[string]any{"type": "object"},
	}}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "keep this" {
		t.Errorf("a call that offered tools came back as %q, want the envelope's message", comp.Content)
	}
}
