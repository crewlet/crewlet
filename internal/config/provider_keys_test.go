package config_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

const providerKeyDoc = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    default:
      model: claude-sonnet-5
    fast:
      model: claude-haiku-4-5
roles:
  - name: CEO
    handle: ceo
%s
`

func validateDoc(t *testing.T, roleBody string) error {
	t.Helper()
	// ParseCompany validates as it parses, so the rule can fire at either
	// stage. Which one is not the point of these tests — that the config is
	// refused is.
	c, err := config.ParseCompany([]byte(strings.Replace(providerKeyDoc, "%s", roleBody, 1)))
	if err != nil {
		return err
	}
	return c.Validate()
}

func TestARoleMayOnlyNameAConfiguredProvider(t *testing.T) {
	t.Parallel()
	// Without this rule the typo is invisible and permanent: the seat falls
	// back to another model, boots, thinks and bills on a model the
	// operator never chose. Nothing downstream can catch it — from inside
	// resolution a name that misses and a name never written are the same
	// absence.
	err := validateDoc(t, "    llm_review: claude-sonet")
	if err == nil {
		t.Fatal("a role naming an unconfigured provider validated cleanly")
	}
	msg := err.Error()
	for _, want := range []string{"roles[0].llm_review", "claude-sonet", "default", "fast"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestAConfiguredKeyValidates(t *testing.T) {
	t.Parallel()
	// The counterfactual. Without it the assertion above passes for a rule
	// that rejects every role.
	if err := validateDoc(t, "    llm_review: [fast, default]"); err != nil {
		t.Errorf("a role naming configured providers was rejected: %v", err)
	}
}

func TestTheMappingFormIsCheckedTooAndNamedWhereItWasWritten(t *testing.T) {
	t.Parallel()
	// The flat field WINS over the mapping, so a typo inside llm.review
	// under a role that also sets llm_review never appears in the resolved
	// chain — and is still a typo, still in the file, and still what the
	// operator edits next. Validating the resolved value would hide it.
	err := validateDoc(t, "    llm_review: fast\n    llm:\n      review: [bigg]")
	if err == nil {
		t.Fatal("a shadowed typo in the mapping form validated cleanly")
	}
	if !strings.Contains(err.Error(), "roles[0].llm.review") {
		t.Errorf("the error does not point at the mapping form:\n%s", err)
	}
}

func TestEveryPerPhaseFieldIsCovered(t *testing.T) {
	t.Parallel()
	// A field left out of the check is a field where the typo stays
	// invisible, and there is no way to notice that from the outside. So the
	// fields are READ OFF THE TYPES — every `llm`-prefixed seat key and every
	// key of the mapping form — rather than listed: a listed phase that was
	// removed kept passing here on the decoder's unknown-key refusal, and a
	// phase that was added was never asked about. Each refusal must name the
	// field, so the decoder's refusal cannot stand in for the rule.
	var flat []string
	role := reflect.TypeFor[config.Role]()
	for i := range role.NumField() {
		key, _, _ := strings.Cut(role.Field(i).Tag.Get("yaml"), ",")
		if strings.HasPrefix(key, "llm") {
			flat = append(flat, key)
		}
	}
	if len(flat) < 2 {
		t.Fatalf("found the seat keys %v, which is not the per-phase family", flat)
	}
	for _, field := range flat {
		err := validateDoc(t, "    "+field+": nope")
		if err == nil || !errors.Is(err, config.ErrUnknownValue) || !strings.Contains(err.Error(), "roles[0]."+field) {
			t.Errorf("%s: an unconfigured provider was not refused there: %v", field, err)
		}
	}
	mapping := reflect.TypeFor[config.PhaseLLM]()
	for i := range mapping.NumField() {
		key, _, _ := strings.Cut(mapping.Field(i).Tag.Get("yaml"), ",")
		err := validateDoc(t, "    llm:\n      "+key+": nope")
		if err == nil || !errors.Is(err, config.ErrUnknownValue) || !strings.Contains(err.Error(), "roles[0].llm") {
			t.Errorf("llm.%s: an unconfigured provider was not refused there: %v", key, err)
		}
	}
}

func TestAnEmptyProviderMapSkipsTheRuleEntirely(t *testing.T) {
	t.Parallel()
	// A company with no models is a documented authoring state — an org
	// chart written before the credentials exist. Rejecting every role's
	// key against an empty map turns that supported flow into a wall of
	// errors about models the author has not added yet.
	c, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: CEO
    handle: ceo
    llm_review: whatever
`))
	if err != nil {
		t.Fatalf("an org chart authored before its providers failed to parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("an org chart authored before its providers was rejected: %v", err)
	}
}
