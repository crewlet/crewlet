package jira_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/jira"
)

// A WEBHOOK IS THE COMPANY'S, NOT AN AGENT'S.
//
// Registration is what makes an instance deliver to this engine at all, and
// it depends on the company's own credential and address. A company with no
// agents yet still wants its instance wired up: the alternative is an
// integration that only starts listening once somebody adds a seat, which is
// the wrong way round.
func TestAHookIsRegisteredWithNoAgentsAtAll(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-org"
	res, err := run(t, inst, func(opts *jira.Options) {
		opts.Org = nil // no org chart at all: no seats, no agents
		opts.WebhookBase = "https://engine.example.com"
		opts.Value = func(v string) string {
			if v == "${JIRA_WEBHOOK_SECRET}" {
				return "s"
			}
			return v
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.created) != 1 {
		t.Fatalf("no hook was registered for a company with no agents: %v", inst.created)
	}
	if got := inst.created[0]["url"]; got != "https://engine.example.com/webhooks/jira" {
		t.Errorf("url = %v", got)
	}
	if res.Hooked == "" {
		t.Error("the result does not report the hook it registered")
	}
}
