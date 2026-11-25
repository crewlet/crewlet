package config

import (
	"errors"
	"strings"
	"testing"
)

// THE COMPANY'S INSTRUCTIONS ARE ADMITTED BY THEIR RULES, AND A CREDENTIAL IS
// REFUSED WITHOUT BEING REPEATED: an unknown preset, a text past the cap and a
// credential each name their field, the text at the cap is admitted, and no
// refusal carries the credential into a log or the revision's record.
// Mutation: drop the credential check, or count the text in bytes, and this is red.
func TestTheCompanyInstructionsAreHeldToTheirRules(t *testing.T) {
	t.Parallel()
	secret := "ghp_" + strings.Repeat("a1B2", 9)
	for _, tc := range []struct {
		name  string
		in    AssistantInstructions
		field string
		kind  error
	}{
		{"an unknown length", AssistantInstructions{Length: "terse"}, "length", ErrUnknownValue},
		{"an unknown format", AssistantInstructions{Format: "essay"}, "format", ErrUnknownValue},
		{"a text past the cap", AssistantInstructions{Text: strings.Repeat("é", AssistantInstructionsMaxRunes+1)},
			"text", ErrOutOfRange},
		{"a credential", AssistantInstructions{Text: "Sign every answer with " + secret}, "text", ErrConflict},
	} {
		c := Company{Assistant: Assistant{Instructions: tc.in}}
		err := c.validateAssistant()
		if !errors.Is(err, tc.kind) || !strings.Contains(err.Error(), "assistant.instructions."+tc.field) {
			t.Errorf("%s: %v, want %v naming assistant.instructions.%s", tc.name, err, tc.kind, tc.field)
		}
		if err != nil && strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal repeats the credential", tc.name)
		}
	}
	atCap := AssistantInstructions{Length: AnswerBrief, Format: AnswerTLDR,
		Text: strings.Repeat("é", AssistantInstructionsMaxRunes)}
	if err := atCap.Validate(nil); err != nil {
		t.Errorf("instructions at the cap were refused: %v", err)
	}
	if got := ReadAssistantInstructions([]byte(`{"length":"terse"}`)); !got.IsZero() {
		t.Errorf("a stored layer this build cannot read read as %+v, want none", got)
	}
}

// THE SCHEMA BOUNDS THE TEXT WHERE THE VALIDATOR DOES: a struct tag cannot
// name the constant, so this holds the literal to it.
func TestTheSchemaBoundsInstructionsWhereTheValidatorDoes(t *testing.T) {
	t.Parallel()
	defs, _ := schemaDoc(t, TierCompany)["$defs"].(map[string]any)
	def, _ := defs["AssistantInstructions"].(map[string]any)
	props, _ := def["properties"].(map[string]any)
	text, _ := props["text"].(map[string]any)
	var bound any
	for _, branch := range append(anyOf(text), text) {
		if own, _ := branch.(map[string]any); own["maxLength"] != nil {
			bound = own["maxLength"]
		}
	}
	if got, _ := bound.(float64); int(got) != AssistantInstructionsMaxRunes {
		t.Fatalf("the schema bounds the text at %v, the validator at %d", bound, AssistantInstructionsMaxRunes)
	}
}
