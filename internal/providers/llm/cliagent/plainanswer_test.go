package cliagent

import (
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A TEXT FUNCTION GETS ITS ANSWER WHOLE, even when the answer is a JSON object
// carrying a key some reader might take for a message. Auxiliary work asks for
// exactly such objects — the persist decider's `{"kind": ..., "content": ...}`
// — and a backend that read the answer for a field hands back the one field,
// so the caller's decode fails and its decision is dropped.
//
// Mutation: read a text profile's answer as a JSON document and take its
// `content`, and this comes back as "keep this".
func TestATextFunctionsAnswerComesBackWhole(t *testing.T) {
	t.Parallel()
	const reply = `{"kind": "LONG", "content": "keep this"}`
	p := fakeProvider(t, map[string]string{"FAKE_STDOUT": reply}, nil)

	comp, err := ask(t, p, llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != reply {
		t.Errorf("a text function's answer came back as %q, want it verbatim %q",
			comp.Content, reply)
	}
}
