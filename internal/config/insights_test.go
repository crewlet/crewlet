package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// THE INSIGHTS CONSENT HOLDS ONLY ITS CLOSED VALUES: a mode the engine does not
// name, a negative schema, or a class that is no catalogue id is refused.
func TestTheInsightsConsentHoldsOnlyItsClosedValues(t *testing.T) {
	t.Parallel()
	const base = "name: Acme\nproviders:\n  llm:\n    p: {model: m}\n  sandbox: {fake: true}\n" +
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: p\ndiagnostics:\n  insights:\n"
	valid := base + "    share: review\n    accepted: 1\n    always: [wake.noop_share]\n"
	c, err := config.ParseCompany([]byte(valid))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a review consent was refused: %v", err)
	}
	for name, block := range map[string]string{
		"an unknown mode":      "    share: sometimes\n",
		"a negative schema":    "    accepted: -1\n",
		"words as a class":     "    always: [\"Acme payroll\"]\n",
		"a class listed twice": "    always: [wake.noop_share, wake.noop_share]\n",
	} {
		// REFUSED AT PARSE OR AT VALIDATION, whichever reads it first.
		c, err := config.ParseCompany([]byte(base + block))
		if err == nil {
			err = c.Validate()
		}
		if err == nil || !strings.Contains(err.Error(), "diagnostics.insights") ||
			!errors.Is(err, config.ErrUnknownValue) && !errors.Is(err, config.ErrOutOfRange) && !errors.Is(err, config.ErrConflict) {
			t.Errorf("%s: Validate = %v, want it refused at diagnostics.insights", name, err)
		}
	}
}
