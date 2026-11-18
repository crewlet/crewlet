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

// THE REVIEW CONTRACT NEVER ASKS WHETHER THE ANSWER WAS DELIVERED: the engine
// settles that before the reviewer runs (it checks the executor's claims
// against its record and overturns a `done` that reached nobody), and the
// review prompt says so. A schema asking anyway contradicts the prompt beside
// the very field, and re-deriving delivery can read a real one as none and
// post it twice.
func TestTheReviewContractNeverAsksWhetherTheAnswerWasDelivered(t *testing.T) {
	t.Parallel()
	for _, d := range descriptions(submitReviewDescription, reviewSchema) {
		if strings.Contains(strings.ToLower(d), "deliver") {
			t.Errorf("the review contract asks about delivery: %q", d)
		}
	}
}

// ONE COPY OF EACH RULE between the submission contracts and the system
// prompts: a fact about a field lives on the field (what `no_action` means,
// that `deliveries` is checked, what `completed_work` carries), and the tool's
// own description says only what the call is; the turn's rules are each
// phase's header. Copies of a rule drift.
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
		{field(workSchema, "outcome"), "you changed nothing outside the engine"},
		{field(workSchema, "outcome"), "a read changes nothing"},
		{field(workSchema, "outcome"), "a call the engine cannot tell from a write counts as a change"},
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
