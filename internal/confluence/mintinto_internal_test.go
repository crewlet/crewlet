package confluence

import (
	"context"
	"strings"
	"testing"
)

// A LITERAL WEBHOOK TOKEN IS NEVER QUOTED BACK.
//
// Same reasoning as Jira's, and the stakes are higher: on Cloud this token is
// the WHOLE authentication for `/webhooks/confluence/{event}`, and the
// refusal is persisted to the fleet store and served on a read that is
// anonymous by default. Anyone reading it could forge any delivery and drive
// a turn with content of their choosing.
func TestARefusedWebhookTokenIsNotQuotedBack(t *testing.T) {
	t.Parallel()
	const token = "aLiteralTokenNobodyShouldEverSee"
	_, _, err := mintInto(context.Background(), Options{
		Value:    func(v string) string { return v },
		Recreate: true,
	}, token, "webhook_token", "the token every delivery carries")
	if err == nil {
		t.Fatal("a literal webhook_token was accepted for minting")
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("the refusal repeats the token:\n%s", err)
	}
	if !strings.Contains(err.Error(), "webhook_token") {
		t.Errorf("the refusal does not name the field to change:\n%s", err)
	}
}
