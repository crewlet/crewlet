package github_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
)

// A DISCONNECT WITHOUT THE OPTIONAL ORG TOKEN STILL FINISHES.
//
// `integrations.github.token` is optional on this host and nothing at runtime
// needs it — each agent acts through its own app — so the engine builds a nil
// client for a company that never set one, and says so in its own doc: "a nil
// client is a valid thing to hand the pass."
//
// Teardown treated it as a fault, and the block is dropped only AFTER the
// vendor step succeeds. So a company with no org token could not disconnect
// GitHub at all: the surface reported Disconnecting and the loop retried an
// error that no retry could change, for the life of the deployment.
func TestATeardownWithNoCredentialFinishesRatherThanFailing(t *testing.T) {
	t.Parallel()
	err := github.Teardown(t.Context(), github.Options{
		Client: nil,
		Config: &config.GitHub{
			Enabled: true,
			Provisioning: &config.GitHubProvisioning{
				Org:   "acme",
				Repos: []string{"acme/api"},
			},
		},
		WebhookBase: "https://engine.example.com",
	})
	if err != nil {
		t.Fatalf("Teardown = %v, want nil: no credential is a posture, not a fault", err)
	}
}

// AND IT NAMES WHAT IT COULD NOT REMOVE, because a hook left behind still
// delivers: the operator has to go and take it away by hand.
func TestATeardownWithNoCredentialNamesTheHooksItLeaves(t *testing.T) {
	if err := github.Teardown(t.Context(), github.Options{
		Config: &config.GitHub{
			Enabled: true,
			Provisioning: &config.GitHubProvisioning{
				Org: "leftbehind-org", Repos: []string{"leftbehind-org/api"},
			},
		},
		WebhookBase: "https://leftbehind.example.com",
	}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	for _, want := range []string{
		"github_hooks_not_removed",
		"leftbehind-org",
		"leftbehind-org/api",
		"leftbehind.example.com",
	} {
		if !logs.contains(want) {
			t.Errorf("the log does not name %q, so an operator is not told what "+
				"is still delivering", want)
		}
	}
}
