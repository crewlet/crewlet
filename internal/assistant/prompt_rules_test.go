package assistant

import (
	"strings"
	"testing"
)

// THE STANDING RULES SAY WHAT THE QA OF prompt v4 FOUND MISSING: answer in
// words, say everything before a proposal's card, never describe what was not
// read, never give a page as the total, say times on the company's clock, and
// write for the person, never naming tools, routes or config keys and never
// quoting a refusal (the plain-language rules from the cards track). Mutation:
// drop any of them and this names it.
func TestTheStandingRulesCarryTheQualityRules(t *testing.T) {
	t.Parallel()
	for _, want := range []string{
		"never answer only by pointing at one",
		"everything you have to say comes before you call one",
		"a seat it involves is paused or stopped",
		"Never describe a setting, a secret, an integration or a screen you have not read",
		"never promise one is enforced",
		"is one page, never the total",
		"on the company's clock",
		"Never name a tool, a route, an HTTP method, a config key, a field in code or a file path",
		"Never write notes to yourself",
		"never quote the refusal",
		"Never say something does not exist unless you listed it",
	} {
		if !strings.Contains(identityRules, want) {
			t.Errorf("the standing rules do not say %q", want)
		}
	}
	if strings.Contains(identityRules, "—") {
		t.Error("the standing rules carry an em dash")
	}
}
