package config

import (
	"errors"
	"strings"
	"testing"
)

// THE ADVISOR IS CONFIGURED BY ONE MODEL: absent is its model off and its
// rules on, a named entry is handed out, one the document does not hold is
// refused at admission and where it is handed out alike, and no other key
// under `advisor:` is read (the analyst's cadence, hour and ceiling are
// constants now).
func TestTheAdvisorIsConfiguredByOneModel(t *testing.T) {
	t.Parallel()
	doc := func(block string) string {
		return "name: Acme\nproviders:\n  llm:\n" + assistantEntry + "advisor:\n" + block
	}
	off := mustCompany(t, "name: Acme\n")
	if _, err := off.AdvisorModel(); !errors.Is(err, ErrAdvisorModelOff) {
		t.Errorf("an unassigned model answered %v, want off", err)
	}
	on := mustCompany(t, doc("  model: assistant\n"))
	if key, err := on.AdvisorModel(); err != nil || key != "assistant" {
		t.Errorf("an assigned model = %q (%v)", key, err)
	}
	gone, err := ParseCompanyDocument([]byte(doc("  model: gone\n")))
	if err != nil {
		t.Fatal(err)
	}
	if err := gone.ValidateAdmission(); err == nil || !strings.Contains(err.Error(), "advisor.model") {
		t.Errorf("admission answered %v, want a problem at advisor.model", err)
	}
	if _, err := gone.AdvisorModel(); err == nil {
		t.Error("AdvisorModel handed out an entry the document does not hold")
	}
	for _, old := range []string{"  analyst:\n    llm: [assistant]\n", "  cadence: daily\n", "  max_tokens: 200000\n"} {
		if _, err := ParseCompanyDocument([]byte(doc(old))); err == nil {
			t.Errorf("a document with advisor:\n%s parsed; only model is a setting", old)
		}
	}
}
