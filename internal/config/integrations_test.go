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

// A CHAT SURFACE SHOWS THAT IT IS WORKING UNLESS TOLD OTHERWISE, on both
// backends and by the same word.
//
// A turn takes minutes, and a reader who sees nothing cannot tell an agent
// working from an agent that is dead. The two defaults used to disagree
// (Slack addressed, Mattermost off), which made "what does an unset block
// do" a question with two answers.
func TestBothChatSurfacesShowTheIndicatorByDefault(t *testing.T) {
	t.Parallel()
	if got := (&config.Slack{}).Status(); got != config.StatusAlways {
		t.Errorf("slack default = %q, want always", got)
	}
	if got := (&config.Mattermost{}).Status(); got != config.StatusAlways {
		t.Errorf("mattermost default = %q, want always", got)
	}
}

// AND THERE IS NO WAY TO TURN IT OFF FOR A WHOLE DEPLOYMENT.
//
// `off` was a company whose agents think in silence for minutes at a time,
// which is the state the indicator exists to remove. `addressed` is the same
// judgement made per message rather than once, and it is what an operator who
// finds the indicator noisy actually wants.
func TestTheIndicatorCannotBeSwitchedOff(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"slack", "mattermost"} {
		t.Run(block, func(t *testing.T) {
			t.Parallel()
			body := "    typing_status: off"
			if block == "mattermost" {
				body = "    url: https://chat.example.com\n    team: acme\n" + body
			}
			err := validateIntegrationDoc(t, block, body)
			if err == nil {
				t.Fatal("`off` was accepted")
			}
			// AND THE REFUSAL NAMES WHAT IS LEFT, because an operator
			// carrying a config that used to work needs the replacement
			// rather than the news that their value is gone.
			for _, want := range []string{"always", "addressed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not offer %q: %v", want, err)
				}
			}
		})
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
