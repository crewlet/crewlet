package setup_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/setup"
)

// A FORM OPENS ON WHAT THE COMPANY ALREADY ANSWERED.
//
// Without the current value the settings dialog is a blank form over a live
// configuration, and saving it blanks every setting the operator did not
// retype. Observed: a Datadog dialog offering "Choose one" over a region and
// a fallback seat the document held.
func TestAPlainRequirementCarriesItsCurrentValue(t *testing.T) {
	t.Parallel()
	out, err := json.Marshal(setup.Requirement{
		Field: "route_to", Kind: setup.KindHandle, Stored: "sre-lead", Present: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["value"] != "sre-lead" {
		t.Errorf("value = %v, want the stored setting", got["value"])
	}
}

// AND A CREDENTIAL'S NEVER DOES. Stored can be a literal secret on a company
// that wrote one instead of a ${VAR}, which is the whole reason it is not
// serialised; the value field must not become the way it escapes.
func TestASecretRequirementNeverCarriesItsValue(t *testing.T) {
	t.Parallel()
	out, err := json.Marshal(setup.Requirement{
		Field: "api_key", Kind: setup.KindSecret, Stored: "a-literal-key", Present: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "a-literal-key") {
		t.Fatalf("a credential reached the wire: %s", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["value"]; ok {
		t.Errorf("a secret carried a value field: %v", got["value"])
	}
}

// AND EVERY OTHER FIELD SURVIVES THE CUSTOM MARSHALLER. An alias type is easy
// to get wrong in a way that silently drops half the form.
func TestMarshallingKeepsTheRestOfTheRequirement(t *testing.T) {
	t.Parallel()
	out, err := json.Marshal(setup.Requirement{
		Field: "site", Label: "Datadog region", Kind: setup.KindChoice,
		ConfigPath: "integrations.datadog.provisioning.site",
		Required:   true, Connect: true, Help: "h", Stored: "datadoghq.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	for field, want := range map[string]any{
		"field": "site", "label": "Datadog region", "kind": "choice",
		"config_path": "integrations.datadog.provisioning.site",
		"required":    true, "connect": true, "help": "h", "value": "datadoghq.com",
	} {
		if got[field] != want {
			t.Errorf("%s = %v, want %v", field, got[field], want)
		}
	}
}
