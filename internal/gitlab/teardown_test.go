package gitlab_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
)

// tearDownAgainst runs a disconnect against the fake instance.
func tearDownAgainst(
	t *testing.T, f *adminInstance, tune func(*gitlab.TeardownOptions),
) error {
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
	opts := gitlab.TeardownOptions{Client: client, Config: cfg}
	if tune != nil {
		tune(&opts)
	}
	removed, err := gitlab.Teardown(context.Background(), opts)
	if len(removed.Accounts) != 0 {
		t.Errorf("a teardown reported removing accounts: %+v", removed)
	}
	return err
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
		namedHook(3, "crewlet", "https://older-tunnel.example.com/webhooks/gitlab"),
	}
	f.projectHooks = map[string][]hookRow{
		"nimbus/api": {namedHook(4, "crewlet", "https://old-tunnel.example.com/webhooks/gitlab")},
	}
	if err := tearDownAgainst(t, f, nil); err != nil {
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
// hooks other integrations registered — a nameless one at a `/webhooks/gitlab`
// path included, since this engine names every hook it registers — and
// another deployment of this same company carries its own under its own name.
// A disconnect that swept any of them would take down something nobody asked
// it to touch.
func TestADisconnectLeavesHooksThatAreNotThisDeploymentsAlone(t *testing.T) {
	t.Parallel()
	f := newAdminInstance()
	f.hooks = []hookRow{
		foreignHook(1, "https://someone-else.example.com/hook"),
		namedHook(2, "crewlet-staging", "https://staging.example.com/webhooks/gitlab"),
		namedHook(3, "crewlet", "https://crewlet.example.com/webhooks/gitlab"),
		foreignHook(4, "https://somebody.example.com/webhooks/gitlab"),
	}
	if err := tearDownAgainst(t, f, nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.hooks) != 3 {
		t.Fatalf("hooks = %+v, want the two foreign ones and staging's left in place", f.hooks)
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
	if err := tearDownAgainst(t, f, nil); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.projectHooks["nimbus/api"]) != 0 {
		t.Errorf("project hooks = %+v, want them removed", f.projectHooks["nimbus/api"])
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
	//
	// ONE OF THEM ARCHIVED, because an archived project keeps its webhooks
	// and GitLab's listing takes an `archived` parameter this sweep has to
	// pass correctly — `true` means "do not filter", where `only` would
	// narrow to this one project and walk past the two live ones. Both
	// states in one fixture is what makes either mistake fail here.
	f.groupProjects = []string{
		"nimbus/dropped", "nimbus/sub/deeper", "nimbus/shut-archived",
	}
	f.projectHooks = map[string][]hookRow{}
	f.projectHooks["nimbus/dropped"] = []hookRow{
		// THIS DEPLOYMENT'S, at an address that is gone. It carries the
		// name, so the name is not what made it unreachable — nothing ever
		// visited the project.
		namedHook(701, "crewlet", "https://dead-tunnel.example.com/webhooks/gitlab"),
	}
	f.projectHooks["nimbus/sub/deeper"] = []hookRow{
		namedHook(702, "crewlet", "https://also-dead.example.com/webhooks/gitlab"),
		// AND SOMEBODY ELSE'S, which must survive.
		foreignHook(703, "https://ci.example.com/hook"),
	}
	// AN ARCHIVED PROJECT'S HOOK IS AS LIVE AS ANY OTHER: GitLab goes on
	// delivering from one, so a sweep that never lists it leaves the
	// payloads leaving.
	f.projectHooks["nimbus/shut-archived"] = []hookRow{
		namedHook(704, "crewlet", "https://long-gone.example.com/webhooks/gitlab"),
	}

	if err := tearDownAgainst(t, f, func(o *gitlab.TeardownOptions) {
		// NO PROJECTS NAMED AT ALL: the group alone, which is how this
		// company connected.
		o.Config.Provisioning.Projects = nil
	}); err != nil {
		t.Fatalf("Teardown: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, project := range []string{
		"nimbus/dropped", "nimbus/sub/deeper", "nimbus/shut-archived",
	} {
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

	err := tearDownAgainst(t, f, nil)
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
