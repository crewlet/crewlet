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

// WITHOUT A BASE NOTHING WAS EVER REGISTERED, so there is nothing to
// withdraw and the disconnect finishes rather than failing. A teardown that
// errored here would hold a surface in Disconnecting over an integration that
// never had a hook at all.
func TestTeardownWithNoWebhookBaseIsANoOp(t *testing.T) {
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
	if len(inst.deleted) != 0 {
		t.Errorf("deleted = %v with no base configured", inst.deleted)
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
