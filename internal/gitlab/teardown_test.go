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

// A REMOVED ACCOUNT NEVER KEEPS A WORKING TOKEN.
//
// GitLab's service-account delete BLOCKS rather than erases on the
// deployments this was measured against: after a `remove_seats` disconnect
// the account was `state: blocked`, out of the group, and holding one active
// token — while the engine had already deleted its own copy of that value. So
// the company lost the credential and GitLab kept a working one, on an
// account that one click restores. datadog's teardown names this hazard
// exactly ("a live key on a disabled account is a credential that works again
// the moment anybody re-enables it") and mattermost's argues the same
// ordering; this was the one that did not follow it.
func TestARemovedAccountKeepsNoWorkingToken(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.blocks = true
	f.join("crewlet-swe", 502, gitlabDeveloperLevel)
	f.pileTokens(502, "crewlet-swe", 1)

	removed, err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		o.RemoveSeats = true
		o.Plan = &provision.Plan{}
		o.Plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})
	})
	if err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	if len(removed.Accounts) != 1 {
		t.Fatalf("removed = %+v, want the seat reported", removed.Accounts)
	}
	if live := f.liveTokensOf(502); live != 0 {
		t.Errorf("%d live token(s) on an account this disconnect removed, and "+
			"the engine has just deleted the company's own copy: whoever "+
			"unblocks it gets a working credential nobody is tracking", live)
	}
}

// AND A TOKEN THAT COULD NOT BE REVOKED LEAVES THE ACCOUNT ALONE.
//
// The state an operator can then see is an account still listed holding a
// credential nobody could withdraw, rather than a blocked one quietly holding
// a working token. The seat is not reported removed either, so its sealed
// value stays where it is — deleting the company's only copy of a live
// credential is the one move nothing can undo.
func TestATokenThatCannotBeRevokedLeavesTheAccountIntact(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.blocks = true
	f.join("crewlet-swe", 502, gitlabDeveloperLevel)
	f.pileTokens(502, "crewlet-swe", 1)
	f.failTokenRevoke = true

	removed, err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		o.RemoveSeats = true
		o.Plan = &provision.Plan{}
		o.Plan.Add(provision.Seat{Handle: "swe", Role: "SWE", TokenVar: "GITLAB_TOKEN_SWE"})
	})
	if err == nil {
		t.Fatal("a teardown that could not revoke a live token reported success")
	}
	if len(removed.Accounts) != 0 {
		t.Errorf("removed = %+v: the seat was reported removed, so the engine "+
			"deletes the company's only copy of a token that still works",
			removed.Accounts)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blocked["crewlet-swe"] {
		t.Error("the account was blocked with a token nobody could revoke, " +
			"which is the state that hides the live credential")
	}
}

// A DISCONNECT REMOVES THIS ENGINE'S HOOKS FROM EVERY PROJECT IN THE GROUP.
//
// The config is not a record of where the hooks ARE: a run that established
// per-project hooks wrote them on the projects named AT THE TIME, so a
// project dropped from `provisioning.projects` since — or a company that
// connected with the group alone and never named one — left them unreachable,
// and a teardown that visited only the current list reported success having
// walked past them.
//
// Measured on a live disconnect: two hooks still on a project in the group,
// pointing at dead trycloudflare tunnels from earlier runs, which nothing
// would ever visit again. That hostname is re-issued to whoever asks next, so
// those deliveries go on leaving the customer's GitLab — signed with a secret
// the stranger does not have, so nothing can be forged INTO the engine, and
// the payloads still leave.
func TestADisconnectSweepsEveryProjectTheGroupHolds(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	// IN THE GROUP AND NOT IN THE CONFIG, which is the shape that leaked.
	f.groupProjects = []string{"nimbus/dropped", "nimbus/sub/deeper"}
	f.projectHooks = map[string][]hookRow{}
	f.projectHooks["nimbus/dropped"] = []hookRow{
		// NAMELESS, as an older build wrote them, at an address that is
		// gone. `ours` adopts a nameless hook at this path, so the name is
		// not what made these unreachable — nothing ever visited the
		// project.
		foreignHook(701, "https://dead-tunnel.example.com/webhooks/gitlab"),
	}
	f.projectHooks["nimbus/sub/deeper"] = []hookRow{
		namedHook(702, "crewlet", "https://also-dead.example.com/webhooks/gitlab"),
		// AND SOMEBODY ELSE'S, which must survive.
		foreignHook(703, "https://ci.example.com/hook"),
	}

	if _, err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		// NO PROJECTS NAMED AT ALL: the group alone, which is how this
		// company connected.
		o.Config.Provisioning.Projects = nil
	}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, project := range []string{"nimbus/dropped", "nimbus/sub/deeper"} {
		for _, hook := range f.projectHooks[project] {
			url, _ := hook.attrs["url"].(string)
			if strings.HasSuffix(url, "/webhooks/gitlab") {
				t.Errorf("%s still holds this deployment's hook at %s: the "+
					"address is re-issuable and the payloads keep leaving",
					project, url)
			}
		}
	}
	if len(f.projectHooks["nimbus/sub/deeper"]) != 1 {
		t.Errorf("nimbus/sub/deeper holds %v, want the stranger's hook alone",
			f.projectHooks["nimbus/sub/deeper"])
	}
}

// AND A GROUP LISTING THAT FAILS STILL SWEEPS WHAT THE CONFIG NAMES.
//
// The group listing needs a credential that can read it. A teardown that gave
// up on the whole project sweep because the group could not be enumerated
// would leave the hooks it CAN reach behind as well, which is the opposite of
// what walking the group was added for.
func TestAGroupListingThatFailsStillSweepsTheNamedProjects(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.noGroupProjects = true
	f.projectHooks = map[string][]hookRow{}
	f.projectHooks["nimbus/api"] = []hookRow{
		namedHook(801, "crewlet", "https://dead.example.com/webhooks/gitlab"),
	}

	_, err := tearDownAgainst(t, f, nil)
	if err == nil {
		t.Fatal("a teardown that could not enumerate the group reported success")
	}
	if !strings.Contains(err.Error(), "projects") {
		t.Errorf("the error does not say what it could not read:\n%v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, hook := range f.projectHooks["nimbus/api"] {
		url, _ := hook.attrs["url"].(string)
		if strings.HasSuffix(url, "/webhooks/gitlab") {
			t.Errorf("nimbus/api still holds %s, although the config names "+
				"that project and it was reachable", url)
		}
	}
}
