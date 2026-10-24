package confluence

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/provision"
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
		Recreate: "-recreate-webhooks",
	}, &Result{}, token, "webhook_token", "the token every delivery carries")
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

// THE RECREATE SWITCH OVER A LITERAL IS REFUSED FOR WHAT IT IS, on either
// half: the Cloud token and the Data Center secret go through the same mint.
// The literal resolves — to itself — and names no variable, so the refusal
// written for a slot that resolved to nothing ("set that variable") was wrong
// on both counts, and it never named the switch or the way on that needs no
// edit: a run without it registers the literal as it is.
//
// NAMED AS ITS CALLER SPELLS IT: `-recreate-webhooks` for `crewlet confluence
// provision`, `recreate_webhooks` for the setup route, whose caller has no
// flag, so a refusal naming one would send them nowhere.
func TestRecreatingHooksOverALiteralNamesTheSwitchAndBothWaysOn(t *testing.T) {
	t.Parallel()
	for _, fresh := range []provision.FreshSwitch{"-recreate-webhooks", "recreate_webhooks"} {
		for _, field := range []string{"webhook_token", "webhook_secret"} {
			_, _, err := mintInto(context.Background(), Options{
				Value:    func(v string) string { return v },
				Recreate: fresh,
			}, &Result{}, "aLiteralNobodyShouldEverSee", field, "what the hooks carry")
			if err == nil {
				t.Errorf("%s over a literal %s went ahead", fresh, field)
				continue
			}
			for _, want := range []string{
				"integrations.confluence." + field + " is a literal",
				string(fresh) + " has no variable to mint a fresh one into",
				"at a whole ${VAR} and re-run with " + string(fresh),
				"run without " + string(fresh) + " to register the value it holds as it is",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the %s refusal of %s does not say %q:\n%v",
						field, fresh, want, err)
				}
			}
			if fresh == "recreate_webhooks" && strings.Contains(err.Error(), "-recreate") {
				t.Errorf("the setup route's %s refusal names a command-line flag:\n%v",
					field, err)
			}
		}
	}
}
