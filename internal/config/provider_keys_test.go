package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

const providerKeyDoc = `
name: Acme
providers:
  llm:
    default:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
    fast:
      type: anthropic
      model: claude-haiku-4-5
      api_keys: ["${ANTHROPIC_API_KEY}"]
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
	err := validateDoc(t, "    llm_plan: claude-sonet")
	if err == nil {
		t.Fatal("a role naming an unconfigured provider validated cleanly")
	}
	msg := err.Error()
	for _, want := range []string{"roles[0].llm_plan", "claude-sonet", "default", "fast"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %q:\n%s", want, msg)
		}
	}
}

func TestAConfiguredKeyValidates(t *testing.T) {
	t.Parallel()
	// The counterfactual. Without it the assertion above passes for a rule
	// that rejects every role.
	if err := validateDoc(t, "    llm_plan: [fast, default]"); err != nil {
		t.Errorf("a role naming configured providers was rejected: %v", err)
	}
}

func TestTheMappingFormIsCheckedTooAndNamedWhereItWasWritten(t *testing.T) {
	t.Parallel()
	// The flat field WINS over the mapping, so a typo inside llm.plan under
	// a role that also sets llm_plan never appears in the resolved chain —
	// and is still a typo, still in the file, and still what the operator
	// edits next. Validating the resolved value would hide it.
	err := validateDoc(t, "    llm_plan: fast\n    llm:\n      plan: [bigg]")
	if err == nil {
		t.Fatal("a shadowed typo in the mapping form validated cleanly")
	}
	if !strings.Contains(err.Error(), "roles[0].llm.plan") {
		t.Errorf("the error does not point at the mapping form:\n%s", err)
	}
}

func TestEveryPerPhaseFieldIsCovered(t *testing.T) {
	t.Parallel()
	// A field left out of the check is a field where the typo stays
	// invisible, and there is no way to notice that from the outside.
	for _, field := range []string{
		"llm", "llm_plan", "llm_execute", "llm_review",
		"llm_subagent", "llm_auxiliary", "llm_judge", "llm_sandbox",
	} {
		if err := validateDoc(t, "    "+field+": nope"); err == nil {
			t.Errorf("%s: an unconfigured provider validated cleanly", field)
		}
	}
	for _, key := range []string{
		"default", "plan", "execute", "review", "subagent", "auxiliary", "judge", "sandbox",
	} {
		if err := validateDoc(t, "    llm:\n      "+key+": nope"); err == nil {
			t.Errorf("llm.%s: an unconfigured provider validated cleanly", key)
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
    llm_plan: whatever
`))
	if err != nil {
		t.Fatalf("an org chart authored before its providers failed to parse: %v", err)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("an org chart authored before its providers was rejected: %v", err)
	}
}
