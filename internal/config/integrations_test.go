package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A MISTYPED typing_status IS REFUSED ON BOTH CHAT BLOCKS.
//
// integrations.slack had no validator at all, so `alwyas` validated clean and
// then degraded to the default inside Slack.Status — the indicator appears on
// some turns and not others, and the operator concludes the feature is flaky
// rather than that they typed it wrong. Mattermost's rule was inline; both
// now call the one on the type.
func TestAMistypedTypingStatusIsRefused(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"slack", "mattermost"} {
		t.Run(block, func(t *testing.T) {
			t.Parallel()
			err := validateIntegrationDoc(t, block, "    typing_status: alwyas")
			if err == nil {
				t.Fatal("a typing_status outside the closed set was accepted; " +
					"it degrades to the default with nothing to say so")
			}
			if !strings.Contains(err.Error(), "typing_status") {
				t.Errorf("error %q does not name the field", err)
			}
		})
	}
}

// And every value in the set is accepted, so the rule is a closed set rather
// than a refusal of everything.
func TestEveryDeclaredTypingStatusIsAccepted(t *testing.T) {
	t.Parallel()
	for _, status := range config.WorkingStatuses {
		t.Run(string(status), func(t *testing.T) {
			t.Parallel()
			if err := validateIntegrationDoc(t, "slack", "    typing_status: "+string(status)); err != nil {
				t.Errorf("%q was refused: %v", status, err)
			}
		})
	}
	// And omitting it, which is how a block takes the default.
	if err := validateIntegrationDoc(t, "slack", ""); err != nil {
		t.Errorf("an empty slack block was refused: %v", err)
	}
}

func validateIntegrationDoc(t *testing.T, block, body string) error {
	t.Helper()
	doc := `
name: Acme
providers:
  llm:
    fast:
      type: anthropic
      model: claude-golden
integrations:
  ` + block + `:
` + body + `
roles:
  - name: SWE
    llm: fast
`
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		return err
	}
	return c.Validate()
}
