package runner

import (
	"strings"
	"testing"
)

// descriptions is every description a submission contract carries: the
// tool's own and each property's, which is everything the model reads about
// the call besides the system prompt.
func descriptions(tool string, schema map[string]any) []string {
	out := []string{tool}
	props, _ := schema["properties"].(map[string]any)
	for _, p := range props {
		field, _ := p.(map[string]any)
		if d, ok := field["description"].(string); ok {
			out = append(out, d)
		}
	}
	return out
}

// THE REVIEW CONTRACT NEVER ASKS WHETHER THE ANSWER WAS DELIVERED.
//
// That is settled before the reviewer runs: the engine checks the executor's
// claims against its record, refuses a bad one in the loop, and overturns a
// `done` that reached nobody — and the review prompt says so. The schema kept
// asking anyway ("the answer was never delivered by a tool call" →
// self_iterate), in words the prompt had retired, so the reviewer read one
// contract in its system prompt and the opposite one beside the field it was
// filling in. Re-deriving that fact is how a real delivery was once read as
// none and posted twice.
func TestTheReviewContractNeverAsksWhetherTheAnswerWasDelivered(t *testing.T) {
	t.Parallel()
	for _, d := range descriptions(submitReviewDescription, reviewSchema) {
		if strings.Contains(strings.ToLower(d), "deliver") {
			t.Errorf("the review contract asks about delivery: %q", d)
		}
	}
}

// ONE COPY OF EACH RULE between the submission contracts and the system
// prompts. A fact about a field lives on the field — what `no_action` means,
// that `deliveries` is checked, what `completed_work` carries — and the
// tool's own description says only what the call is: the turn's rules (act
// with tool calls, search for what is missing) are the executor's header, and
// the reviewer's are its header. Each was written in two or three of these
// places once, and the copies drifted.
func TestEachSubmissionRuleLivesOnItsField(t *testing.T) {
	t.Parallel()
	for _, rule := range []string{"deliveries", "no_action", "list_mcp_server_tools",
		"activate_tool", "colleague", "Writing about an action"} {
		if strings.Contains(submitWorkDescription, rule) {
			t.Errorf("submit_work's description restates %q, which is a field's or the header's", rule)
		}
	}
	for _, rule := range []string{"self_iterate", "BEEN TOLD", "notes", "tool log"} {
		if strings.Contains(submitReviewDescription, rule) {
			t.Errorf("submit_review's description restates %q, which is a field's or the header's", rule)
		}
	}

	field := func(schema map[string]any, name string) string {
		t.Helper()
		props, _ := schema["properties"].(map[string]any)
		f, _ := props[name].(map[string]any)
		d, _ := f["description"].(string)
		if d == "" {
			t.Fatalf("%s has no description", name)
		}
		return d
	}
	// And the one copy each is where it must be.
	for _, want := range []struct{ got, phrase string }{
		{field(workSchema, "outcome"), "Declining something you were asked"},
		{field(workSchema, "outcome"), "report it as delivered"},
		{field(workSchema, "deliveries"), "checks them against its own record"},
		{field(reviewSchema, "notes"), "Required on self_iterate"},
		{field(reviewSchema, "completed_work"), "ALREADY landed"},
		{field(reviewSchema, "decision"), "self_iterate"},
	} {
		if !strings.Contains(want.got, want.phrase) {
			t.Errorf("%q is missing from the field that carries it: %q", want.phrase, want.got)
		}
	}
}
