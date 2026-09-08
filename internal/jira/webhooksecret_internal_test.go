package jira

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A LITERAL WEBHOOK SECRET IS NEVER QUOTED BACK.
//
// This refusal does not stay in the operator's terminal. It becomes the pass
// error, which `integration.Observe` writes into `State.LastError`, which the
// fleet's coordination store holds and the integrations query serves — a read
// that is anonymous unless the deployment turned that off. Quoting the value
// published the key every Jira delivery is signed with.
func TestARefusedWebhookSecretIsNotQuotedBack(t *testing.T) {
	t.Parallel()
	const secret = "whsec_aLiteralNobodyShouldEverSee"
	_, _, err := webhookSecret(context.Background(), Options{
		Config: &config.Jira{WebhookSecret: secret},
		// A literal resolves to itself, which is why the early return
		// below is only skipped when the operator asked to recreate.
		Value:           func(v string) string { return v },
		RecreateWebhook: true,
	}, "the project hook")
	if err == nil {
		t.Fatal("a literal webhook_secret was accepted for minting")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal repeats the secret:\n%s", err)
	}
	if !strings.Contains(err.Error(), "webhook_secret") {
		t.Errorf("the refusal does not name the field to change:\n%s", err)
	}
}
