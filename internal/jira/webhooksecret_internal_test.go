package jira

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/provision"
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
	_, err := webhookSecret(context.Background(), Options{
		Config: &config.Jira{WebhookSecret: secret},
		// A literal resolves to itself, which is why the early return
		// below is only skipped when the operator asked to recreate.
		Value:           func(v string) string { return v },
		RecreateWebhook: "-recreate-webhook",
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

// THE RECREATE SWITCH OVER A LITERAL IS REFUSED FOR WHAT IT IS. The literal
// resolves — to itself — and names no variable, so the refusal written for a
// slot that resolved to nothing ("set that variable") was wrong on both
// counts, and it never named the switch or the way on that needs no edit: a
// run without it registers the literal as it is.
//
// NAMED AS ITS CALLER SPELLS IT: `-recreate-webhook` for `crewlet jira
// provision`, `recreate_webhooks` for the setup route, whose caller has no
// flag, so a refusal naming `-recreate-webhook` would send them nowhere.
func TestRecreatingTheHookOverALiteralSecretNamesTheSwitchAndBothWaysOn(t *testing.T) {
	t.Parallel()
	for _, fresh := range []provision.FreshSwitch{"-recreate-webhook", "recreate_webhooks"} {
		_, err := webhookSecret(context.Background(), Options{
			Config:          &config.Jira{WebhookSecret: "whsec_aLiteralNobodyShouldEverSee"},
			Value:           func(v string) string { return v },
			RecreateWebhook: fresh,
		}, "the project hook")
		if err == nil {
			t.Errorf("%s over a literal webhook_secret went ahead", fresh)
			continue
		}
		for _, want := range []string{
			"integrations.jira.webhook_secret is a literal",
			string(fresh) + " has no variable to mint a fresh one into",
			"at a whole ${VAR} and re-run with " + string(fresh),
			"run without " + string(fresh) + " to register the value it holds as it is",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of %s does not say %q:\n%v", fresh, want, err)
			}
		}
		if fresh == "recreate_webhooks" && strings.Contains(err.Error(), "-recreate") {
			t.Errorf("the setup route's refusal names a command-line flag:\n%v", err)
		}
	}
}
