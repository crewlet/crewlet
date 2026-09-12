package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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

// A GROUP THAT DOES NOT RESOLVE IS NOT A GROUP THAT WAS DELETED, AND THE
// ACCOUNTS ARE STILL ASKED ABOUT.
//
// This arm used to fabricate a full provision.Removed for every planned seat
// without one request — "the group is gone, so everything in it went with it"
// — and Engine.forgetRemoved then deleted those seats' sealed tokens.
// GroupByPath maps ANY 404 to not-found, and GitLab answers 404 for a renamed
// group, for a typo, and for a group the credential cannot see; an instance-
// owned account survives its group being deleted outright. So the disconnect
// destroyed live agents' credentials and reported success.
func TestAGroupThatDoesNotResolveDoesNotReportLiveAccountsAsRemoved(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroup = true
	// The account is THERE, on the instance, whatever the group says.
	f.join("crewlet-swe", 501, gitlabDeveloperLevel)

	removed, err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		o.RemoveSeats = true
		o.Plan = &provision.Plan{}
		o.Plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})
	})
	if err == nil {
		t.Fatal("a group that does not resolve, over an account that is still " +
			"there, reported a clean teardown")
	}
	if len(removed.Accounts) != 0 {
		t.Fatalf("reported %+v as removed, so the engine would delete a live "+
			"agent's sealed token", removed.Accounts)
	}
	if len(removed.Secrets()) != 0 {
		t.Errorf("named %v for deletion", removed.Secrets())
	}
}

// AND A SEAT WHOSE ACCOUNT REALLY IS ABSENT IS STILL REPORTED, on the
// instance's own authority: UserByUsername needs no group, so the honest
// answer is available even when the group is not. Without this the fix would
// have traded a destructive teardown for one that can never finish.
func TestAGroupThatDoesNotResolveStillReportsAnAccountThatIsGone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroup = true

	removed, err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		o.RemoveSeats = true
		o.Plan = &provision.Plan{}
		o.Plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(removed.Accounts) != 1 || removed.Accounts[0].Handle != "swe" {
		t.Fatalf("removed = %+v, want the absent seat reported", removed.Accounts)
	}
	if got := removed.Secrets(); len(got) != 1 || got[0] != "GITLAB_TOKEN_SWE" {
		t.Errorf("secrets = %v, want the dead credential named", got)
	}
}

// AND AN INSTANCE-OWNED ACCOUNT IS NEVER SENT DOWN THE GROUP ROUTE WITH NO
// GROUP. The group delete reads 404 as success, so addressing group 0 would
// report every account removed and leave it live — the same trap
// TeardownOptions.Mode exists to close, by a different road.
func TestAGroupOwnedAccountIsNotDeletedThroughAGroupThatIsNotThere(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroup = true
	f.join("crewlet-swe", 502, gitlabDeveloperLevel)

	_, err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		o.RemoveSeats = true
		o.Plan = &provision.Plan{}
		o.Plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})
	})
	if err == nil {
		t.Fatal("the delete was reported as done")
	}
	if !strings.Contains(err.Error(), "provisioning.group") {
		t.Errorf("error %q does not name the field to fix", err)
	}
}
