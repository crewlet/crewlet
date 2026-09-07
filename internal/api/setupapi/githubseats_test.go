package setupapi_test

import (
	"net/http"
	"strings"
	"testing"
)

// A company whose agents are at every stage of getting their own GitHub App:
// one that has not started, one whose app exists and is installed nowhere, one
// that is finished, and one whose sealed key this node cannot read. The human
// seat is there to be left out.
const githubAppsDoc = `{
  "name": "Acme",
  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "Builder", "handle": "builder", "llm": "zulu",
     "integrations": {"github": {"tier": "full_access", "app_id": 41, "app_slug": "acme-builder"}}},
    {"name": "Reviewer", "handle": "reviewer", "llm": "zulu",
     "integrations": {"github": {"tier": "review", "app_id": 42, "app_slug": "acme-reviewer",
       "installation_id": 7, "private_key": "${REVIEWER_GITHUB_APP_KEY}"}}},
    {"name": "Stray", "handle": "stray", "llm": "zulu",
     "integrations": {"github": {"app_id": 43, "app_slug": "acme-stray",
       "installation_id": 8, "private_key": "${STRAY_GITHUB_APP_KEY}"}}},
    {"name": "Jane Founder", "handle": "founder", "kind": "human",
     "contact": {"github_login": "jane"}}
  ]
}`

// seedGitHubApps imports that company and seals the one key that resolves.
func (s *surface) seedGitHubApps(t *testing.T) {
	t.Helper()
	res := s.do(t, http.MethodPut, "/config", githubAppsDoc,
		map[string]string{"X-Summary": "a company mid-rollout"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "REVIEWER_GITHUB_APP_KEY",
		"-----BEGIN RSA PRIVATE KEY-----\nnot-a-real-key\n-----END RSA PRIVATE KEY-----",
		"test", "test", pinned); err != nil {
		t.Fatal(err)
	}
}

// githubSeats pulls the GitHub roster out of the setup answer, by handle.
func githubSeats(t *testing.T, s *surface) map[string]map[string]any {
	t.Helper()
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	rows, _ := state["seats"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		handle, _ := seat["handle"].(string)
		out[handle] = seat
	}
	return out
}

// THE ROSTER SAYS WHICH OF THE TWO CLICKS IS LEFT, PER AGENT.
//
// A GitHub App is one bot identity, so an agent gets its own or it acts as
// nobody, and building one takes two acts by a person that can be days apart:
// create it, then install it. An operator who has done the first must be told
// the second is outstanding rather than shown the same button again, which is
// why the step is a closed set the screen reads rather than a sentence it
// parses.
func TestTheGitHubRosterNamesTheStepEachAgentIsWaitingOn(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	seats := githubSeats(t, s)

	// EVERY AGENT AND ONLY THE AGENTS. The seat with no app is the row the
	// flow starts from, so leaving it out would leave an operator no way to
	// give it one; a person's GitHub account is not this engine's to create.
	if len(seats) != 4 {
		t.Fatalf("the roster holds %d seats, want one per agent: %v", len(seats), seats)
	}
	if _, listed := seats["founder"]; listed {
		t.Error("a human seat is on the roster, so the screen offers to create " +
			"a GitHub App for a person")
	}

	// NOTHING WRITTEN DOWN YET: create the app, and there is no link to
	// follow, because an app is created by POSTing a manifest from a page
	// carrying the operator's own GitHub session.
	fresh := seats["sre-lead"]
	if fresh["step"] != "create_app" {
		t.Errorf("a seat with no app is on step %v", fresh["step"])
	}
	if url, ok := fresh["action_url"]; ok {
		t.Errorf("create_app carries action_url %v, and there is no address to "+
			"send an operator to", url)
	}
	if fresh["present"] != false || fresh["satisfied"] != false {
		t.Errorf("a seat with no app reports present=%v satisfied=%v",
			fresh["present"], fresh["satisfied"])
	}
	// THE TIER IS ANSWERED BEFORE THE APP EXISTS, because it is what the
	// manifest will be built with. Silence is read_only, which is the least
	// this engine hands out.
	if fresh["tier"] != "read_only" {
		t.Errorf("a seat that says nothing about its tier reports %v", fresh["tier"])
	}

	// CREATED AND INSTALLED NOWHERE: install it, and the engine can address
	// that page directly.
	half := seats["builder"]
	if half["step"] != "install_app" {
		t.Fatalf("a seat whose app is installed nowhere is on step %v", half["step"])
	}
	action, _ := half["action_url"].(string)
	// THE APP'S OWN SETTINGS, not github.com/apps/{slug}: that public
	// route exists only for PUBLIC apps, and every app this engine creates
	// is private, so it answered 404 on the one click the flow depends on.
	//
	// This company has named no organization yet, so the link is the
	// personal-account form. The organization form is asserted in
	// TestAGitHubSeatWithWorkOutstandingHoldsTheCardOpen, which seeds one.
	if action != "https://github.com/settings/apps/acme-builder/installations" {
		t.Errorf("install_app points at %q, which is not this app's install page", action)
	}
	if strings.Contains(action, "github.com/apps/") {
		t.Errorf("the install link uses the public route, which 404s for a private app: %q", action)
	}
	// STARTED, and not finished: the app exists, so the roster must not
	// offer to create a second one, and it mints nothing until it is
	// installed.
	if half["present"] != true || half["satisfied"] != false {
		t.Errorf("a created but uninstalled app reports present=%v satisfied=%v",
			half["present"], half["satisfied"])
	}
	if half["tier"] != "full_access" {
		t.Errorf("tier = %v, want the one the manifest would carry", half["tier"])
	}

	// FINISHED: no step, and nothing for the screen to offer.
	done := seats["reviewer"]
	if step, ok := done["step"]; ok {
		t.Errorf("a finished seat is still on step %v", step)
	}
	if done["satisfied"] != true || done["present"] != true {
		t.Errorf("a finished seat reports present=%v satisfied=%v",
			done["present"], done["satisfied"])
	}
}

// A KEY WRITTEN DOWN IS NOT A KEY THIS NODE CAN READ.
//
// A `${VAR}` naming a secret the store does not hold is the state that reads
// as configured from every other surface while the agent mints no token at
// all, so the roster reports it as unfinished and names the variable to set.
// It names the REFERENCE and never a value: only a whole `${VAR}` can fail to
// resolve, so there is no path here for a private key to reach the wire.
func TestASeatWhoseAppKeyDoesNotResolveIsNotSatisfied(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	seats := githubSeats(t, s)

	stray := seats["stray"]
	if stray["satisfied"] != false {
		t.Fatal("a seat whose app key resolves to nothing reports itself finished, " +
			"which is the one state that looks configured while nothing works")
	}
	detail, _ := stray["detail"].(string)
	if !strings.Contains(detail, "${STRAY_GITHUB_APP_KEY}") {
		t.Errorf("detail = %q, and it does not name the variable to set", detail)
	}
	// AND THE ONE THAT DID RESOLVE SAYS SO WITHOUT CARRYING THE KEY.
	done, _ := seats["reviewer"]["detail"].(string)
	if strings.Contains(done, "BEGIN RSA PRIVATE KEY") {
		t.Fatal("the roster echoed a seat's private key")
	}
}

// AN UNFINISHED ROSTER DOES NOT MAKE THE CARD UNFINISHED.
//
// AN AGENT WITHOUT ITS OWN APP ACTS AS NOBODY, so the card must not read
// Connected over it.
//
// This asserted the opposite for one commit, on the reasoning that seats are
// informational everywhere but Slack. That is true where a seat credential is
// an upgrade on a working app, and false here: one GitHub App is one bot
// identity, so an agent with no app of its own has no way to act at all. An
// operator saw Connected on a card whose only agent could do nothing, and had
// to open the roster to find out.
func TestAGitHubSeatWithWorkOutstandingHoldsTheCardOpen(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.seedGitHub(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["satisfied"] != false {
		t.Fatalf("a GitHub whose agents still have apps to create reports "+
			"satisfied=%v", state["satisfied"])
	}
	if state["seats_required"] != true {
		t.Errorf("seats_required = %v: an agent without its own app acts as nobody",
			state["seats_required"])
	}
	// AND THE INSTALL LINK NAMES THE ORGANIZATION once the company has one:
	// an organization's app is installed from the organization's settings,
	// and a personal link opens somebody else's list.
	seats, _ := state["seats"].([]any)
	for _, entry := range seats {
		seat, _ := entry.(map[string]any)
		if seat["handle"] != "builder" {
			continue
		}
		want := "https://github.com/organizations/acme/settings/apps/acme-builder/installations"
		if got, _ := seat["action_url"].(string); got != want {
			t.Errorf("install_app points at %q, want %q", got, want)
		}
	}
}
