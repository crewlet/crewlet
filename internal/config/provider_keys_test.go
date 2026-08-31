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

// A SEAT IS A SEAT WHEREVER IT IS WRITTEN, and most of them are written
// inside units.
//
// The rule walked the root `roles:` list only, so a typo in any unit role —
// which is where a real company keeps nearly all of its seats — validated
// clean. The seat then booted, thought and billed on whatever the fallback
// resolved to, and this is the only place the typo can ever be seen.
func TestAProviderTypoIsCaughtAtEveryDepthOfTheOrgChart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		doc  string
		path string
	}{
		{
			name: "a root role",
			doc:  "roles:\n  - name: CEO\n    llm: claude-sonet\n",
			path: "roles[0].llm",
		},
		{
			name: "a role in a unit",
			doc: "units:\n  - name: Engineering\n    roles:\n" +
				"      - name: Dev\n        llm: claude-sonet\n",
			path: "units[0].roles[0].llm",
		},
		{
			name: "a role in a nested child unit",
			doc: "units:\n  - name: Engineering\n    children:\n" +
				"      - name: Backend\n        roles:\n" +
				"          - name: Dev\n            llm_plan: claude-sonet\n",
			path: "units[0].children[0].roles[0].llm_plan",
		},
		{
			name: "a role two children deep",
			doc: "units:\n  - name: Technology\n    children:\n" +
				"      - name: Engineering\n        children:\n" +
				"          - name: Backend\n            roles:\n" +
				"              - name: Dev\n                llm_review: claude-sonet\n",
			path: "units[0].children[0].children[0].roles[0].llm_review",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateOrgDoc(t, tc.doc)
			if err == nil {
				t.Fatal("a provider key that names no configured model was accepted; " +
					"the seat will fall back to another model and bill against it")
			}
			// Reported at the path the OPERATOR typed, which is the only
			// thing that makes the error actionable in a nested chart.
			if !strings.Contains(err.Error(), tc.path) {
				t.Errorf("error %q does not name the path %q", err, tc.path)
			}
		})
	}
}

// And a correct key at depth still validates, so the case above is the rule
// firing rather than the walk refusing every unit role it now reaches.
func TestAConfiguredKeyValidatesAtEveryDepth(t *testing.T) {
	t.Parallel()
	doc := "units:\n  - name: Engineering\n    roles:\n" +
		"      - name: Dev\n        llm: fast\n    children:\n" +
		"      - name: Backend\n        roles:\n" +
		"          - name: Junior\n            llm_plan: default\n"
	if err := validateOrgDoc(t, doc); err != nil {
		t.Fatalf("a document naming only configured providers was refused: %v", err)
	}
}

// validateOrgDoc parses a whole org body — roles and/or units — against the
// same provider map the rest of this file uses.
func validateOrgDoc(t *testing.T, orgBody string) error {
	t.Helper()
	doc := `
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
` + orgBody
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		return err
	}
	return c.Validate()
}
