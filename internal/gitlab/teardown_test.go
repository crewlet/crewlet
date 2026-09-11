package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/provision"
)

// tearDownAgainst runs a disconnect against the fake instance.
func tearDownAgainst(
	t *testing.T, f *adminInstance, tune func(*gitlab.TeardownOptions),
) (provision.Removed, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client, err := gitlab.NewClient(gitlab.ClientOptions{
		URL: srv.URL, Token: adminToken, HTTP: srv.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	cfg := enabledGitLab()
	cfg.Provisioning.Projects = []string{"nimbus/api"}
	opts := gitlab.TeardownOptions{Client: client, Config: cfg, Plan: &provision.Plan{}}
	if tune != nil {
		tune(&opts)
	}
	return gitlab.Teardown(context.Background(), opts)
}

// A DISCONNECT REMOVES THE HOOKS AT EVERY ADDRESS THIS ENGINE USED, not only
// the one in force.
//
// It compared each hook against the target built from the CURRENT public
// base, so every registration a previous base had left behind survived the
// disconnect that was supposed to remove them — and a deployment whose base
// had moved took none of them out while reporting the surface removed.
// Measured on a real deployment: a group hook and two project hooks still
// live, still signed, after the integration was disconnected.
func TestADisconnectRemovesTheHooksLeftAtEveryAddress(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		namedHook(1, "crewlet", "https://crewlet.example.com/webhooks/gitlab"),
		namedHook(2, "crewlet", "https://old-tunnel.example.com/webhooks/gitlab"),
		legacyHook(3, "https://older-tunnel.example.com/webhooks/gitlab"),
	}
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {namedHook(4, "crewlet", "https://old-tunnel.example.com/webhooks/gitlab")},
	}
	if _, err := tearDownAgainst(t, f, nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 0 {
		t.Errorf("group hooks = %+v, want every one this engine registered removed", f.hooks)
	}
	if len(f.projectHooks["nimbus/api"]) != 0 {
		t.Errorf("project hooks = %+v, want them removed too", f.projectHooks["nimbus/api"])
	}
}

// AND LEAVES EVERYTHING THAT IS NOT THIS DEPLOYMENT'S. An instance carries
// hooks other integrations registered, and another deployment of this same
// company carries its own under its own name — a disconnect that swept either
// would take down something nobody asked it to touch.
func TestADisconnectLeavesHooksThatAreNotThisDeploymentsAlone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		foreignHook(1, "https://someone-else.example.com/hook"),
		namedHook(2, "crewlet-staging", "https://staging.example.com/webhooks/gitlab"),
		namedHook(3, "crewlet", "https://crewlet.example.com/webhooks/gitlab"),
	}
	if _, err := tearDownAgainst(t, f, nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 2 {
		t.Fatalf("hooks = %+v, want the foreign one and staging's left in place", f.hooks)
	}
	for _, hook := range f.hooks {
		if hook.id == 3 {
			t.Errorf("this deployment's own hook survived the disconnect: %+v", hook)
		}
	}
}

// AN INSTANCE WITH NO GROUP HOOKS API IS NOT A FAILED DISCONNECT. Group hooks
// are a Premium feature and GitLab HIDES an unavailable endpoint rather than
// answering 402, so a Free instance says "not found" about a feature it does
// not serve — which is nothing to sweep, not a teardown that could not
// finish. Read as a failure it would hold the surface in Disconnecting and
// retry for the life of the deployment.
func TestADisconnectOnAnInstanceWithoutGroupHooksStillFinishes(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupHooks = true
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {namedHook(1, "crewlet", "https://old-tunnel.example.com/webhooks/gitlab")},
	}
	if _, err := tearDownAgainst(t, f, nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.projectHooks["nimbus/api"]) != 0 {
		t.Errorf("project hooks = %+v, want them removed", f.projectHooks["nimbus/api"])
	}
}
