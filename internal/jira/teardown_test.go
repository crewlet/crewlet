package jira_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jira"
)

// A TEARDOWN REMOVES THIS ENGINE'S HOOK AND NOBODY ELSE'S.
//
// A Jira instance carries hooks other integrations registered, and the one
// this engine made is identified by its TARGET — this deployment's own
// webhook address — rather than by its name, which an administrator may have
// changed. Matching on anything looser would delete a customer's unrelated
// integration on a disconnect, which is not recoverable from this side.
func TestTeardownRemovesOnlyThisEnginesHook(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-1"
	base := "https://engine.example.com"

	// Three hooks: ours, somebody else's, and one that merely shares the
	// host but points at another path.
	inst.hooks = []map[string]any{
		{"id": "1", "name": "crewlet", "url": base + "/webhooks/jira"},
		{"id": "2", "name": "someone else", "url": "https://other.example.com/hook"},
		{"id": "3", "name": "crewlet", "url": base + "/webhooks/gitlab"},
	}

	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.DataCenter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jira.Teardown(context.Background(), jira.Options{
		Client: client, Config: &config.Jira{}, WebhookBase: base,
	}); err != nil {
		t.Fatalf("teardown: %v", err)
	}

	if len(inst.deleted) != 1 || inst.deleted[0] != "1" {
		t.Fatalf("deleted = %v, want only the hook pointing at this engine", inst.deleted)
	}
}

// A NODE THAT CANNOT NAME ITS OWN ADDRESS STILL REMOVES THE HOOK.
//
// It did not: with no public base there was no address to match on, so the
// teardown walked away and left a live registration behind, delivering to
// this engine for ever after the operator disconnected the integration. A
// hook is identified by its NAME now, which is exactly the identity that does
// not depend on knowing where this deployment is reachable — see jira.ours.
func TestTeardownWithNoWebhookBaseStillRemovesTheHook(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-1"
	inst.hooks = []map[string]any{{"id": "1", "name": "crewlet", "url": "https://x/webhooks/jira"}}

	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.DataCenter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jira.Teardown(context.Background(), jira.Options{
		Client: client, Config: &config.Jira{},
	}); err != nil {
		t.Fatalf("teardown with no base: %v", err)
	}
	if len(inst.deleted) != 1 || inst.deleted[0] != "1" {
		t.Errorf("deleted = %v; a live hook survived the disconnect because "+
			"this node could not name its own address", inst.deleted)
	}
}

// AND SOMEBODY ELSE'S HOOK IS LEFT ALONE, which is what the delivery-path
// guard is for: a hook that shares the name and points anywhere but a
// /webhooks/jira path was never registered by this engine.
func TestTeardownLeavesAHookThatIsNotThisEnginesAlone(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-1"
	inst.hooks = []map[string]any{
		{"id": "1", "name": "crewlet", "url": "https://someone-else/hooks/theirs"},
	}

	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.DataCenter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := jira.Teardown(context.Background(), jira.Options{
		Client: client, Config: &config.Jira{}, WebhookBase: "https://x",
	}); err != nil {
		t.Fatal(err)
	}
	if len(inst.deleted) != 0 {
		t.Errorf("deleted %v, which this engine never registered", inst.deleted)
	}
}

// SAFE TO REPEAT. A failed teardown is retried, so the second run over an
// instance the first one already cleared must finish rather than fault.
func TestTeardownIsSafeToRepeat(t *testing.T) {
	t.Parallel()
	inst := newInstance(t)
	inst.accounts["Bearer org-token"] = "acct-1"
	base := "https://engine.example.com"
	inst.hooks = []map[string]any{{"id": "1", "name": "crewlet", "url": base + "/webhooks/jira"}}

	client, err := jira.NewClient(jira.ClientOptions{
		URL: inst.URL, Token: "org-token", Deployment: jira.DataCenter,
	})
	if err != nil {
		t.Fatal(err)
	}
	opts := jira.Options{Client: client, Config: &config.Jira{}, WebhookBase: base}
	if err := jira.Teardown(context.Background(), opts); err != nil {
		t.Fatalf("first teardown: %v", err)
	}
	inst.hooks = nil // the instance now reports what the first pass left
	if err := jira.Teardown(context.Background(), opts); err != nil {
		t.Fatalf("second teardown over an instance already cleared: %v", err)
	}
}
