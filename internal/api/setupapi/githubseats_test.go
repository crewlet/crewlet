package setupapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/setupapi"
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

// THE DELETE LINK OPENS THE PAGE THE DELETE CONTROL IS ON.
//
// GitHub has no API for deleting an App at any permission, so a disconnect
// uninstalls each agent's App — which is what revokes its access — and hands
// the operator a link for the rest. That link is the whole of what the engine
// can do about it, so landing on the wrong tab wastes the one gesture it
// offers: the settings root opens on General, and the delete control lives
// only under Advanced, at the bottom, under a heading named nothing like
// "delete".
//
// PINNED HERE, at the seam, and not only at [github.ManageURL]. The builder
// has had its own test since the page was corrected; what had none was this —
// which of the builders this roster calls. Nothing would have caught
// `InstallURL` here, or a bare settings root, and the failure is silent: the
// link works, opens, and shows the operator a page without the control they
// came for.
func TestTheAppDeleteLinkOpensTheAdvancedPage(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.seedGitHub(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	seats, _ := state["seats"].([]any)
	var found bool
	for _, entry := range seats {
		seat, _ := entry.(map[string]any)
		if seat["handle"] != "builder" {
			continue
		}
		found = true
		want := "https://github.com/organizations/acme/settings/apps/acme-builder/advanced"
		got, _ := seat["manage_url"].(string)
		if got != want {
			t.Errorf("manage_url = %q, want %q: the delete control is only on "+
				"the Advanced page, so every other page is a wasted trip",
				got, want)
		}
	}
	if !found {
		t.Fatalf("no seat named builder in %v", seats)
	}
	// AND WHAT IS LEFT TO CLICK IS SAID, because the control is at the
	// bottom of that page under a heading that does not say delete.
	if state["manage_path"] != "Delete GitHub App" {
		t.Errorf("manage_path = %v, so the link lands them somewhere with no "+
			"word about what to press", state["manage_path"])
	}

	// AND THE LISTING CARRIES IT, which is the route that matters: the
	// disconnect dialog is drawn from GET /setup/integrations, not from the
	// single-kind read above. The two share one builder today and this is
	// what keeps that true — a listing that dropped its seats, or built
	// them its own way, would take the link out of the one screen that
	// offers it while every single-kind test went on passing.
	listing := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))
	tools, _ := listing["tools"].([]any)
	var linked string
	for _, entry := range tools {
		tool, _ := entry.(map[string]any)
		if tool["key"] != "github" {
			continue
		}
		rows, _ := tool["seats"].([]any)
		for _, row := range rows {
			seat, _ := row.(map[string]any)
			if seat["handle"] == "builder" {
				linked, _ = seat["manage_url"].(string)
			}
		}
	}
	if want := "https://github.com/organizations/acme/settings/apps/acme-builder/advanced"; linked != want {
		t.Errorf("the listing the disconnect dialog reads carries %q, want %q",
			linked, want)
	}
}

// fakeGitHub answers the one call the app flow makes: the manifest
// conversion. It records what it was asked to convert so a test can assert
// the code reached it, and answers with what GitHub answers with: a private
// key and a webhook secret, both returned exactly once.
func fakeGitHub(t *testing.T, app map[string]any) *httptest.Server {
	t.Helper()
	// UNDER /api/v3, which is where an Enterprise Server actually serves its
	// REST API and therefore the only path a conversion may arrive on. The
	// fake answered at the root, so it agreed with a caller that passed the
	// BROWSER base as the API base — and the one deployment shape that
	// distinguishes them is the one this fake stands in for.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/v3/app-manifests/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(app)
	}))
	t.Cleanup(server.Close)
	return server
}

// convertOneApp drives a whole creation for one seat, from the begin route
// through GitHub's redirect, against a fake GitHub.
func (s *surface) convertOneApp(t *testing.T, handle string, app map[string]any) {
	t.Helper()
	server := fakeGitHub(t, app)
	// THE CONVERSION GOES WHERE THE COMPANY SAYS GITHUB IS, which is what
	// makes an Enterprise Server work and what lets this test answer.
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"public_base_url":"https://engine.example.com",
		  "github":{"enabled":true,"url":"`+server.URL+`","webhook_secret":"org",
		  "provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "point at a github",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("point at github = %d: %s", res.Code, res.Body)
	}

	flow := setupapi.NewAppFlow(s.setup, []string{"test-material"}, nil)
	s.setup.AttachAppFlow(flow)

	// THROUGH THE BEGIN ROUTE, so the state the callback validates is one
	// this engine actually minted rather than one the test forged.
	begin := decode(t, s.do(t, http.MethodPost, "/setup/integrations/github/app",
		`{"seat":"`+handle+`"}`, nil))
	state, _ := begin["state"].(string)
	if state == "" {
		t.Fatalf("the begin route minted no state: %v", begin)
	}
	if seat, err := flow.Complete(t.Context(), "one-time-code", state); err != nil {
		t.Fatalf("complete %s: %v", seat, err)
	}
}

// seatDoc reads one seat back through the entity route the flow writes it on.
func (s *surface) seatDoc(t *testing.T, handle string) []byte {
	t.Helper()
	res := s.do(t, http.MethodGet, "/config/roles/"+handle, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("read the seat %s = %d: %s", handle, res.Code, res.Body)
	}
	return res.Body.Bytes()
}

// AN APP'S OWN WEBHOOK SECRET IS SEALED AND POINTED AT.
//
// GitHub returns the signing secret in the conversion response, once, and it
// is the ONLY thing an agent's deliveries can be verified against: the
// organization's belongs to a different app, or to no app at all in a company
// that only ever created per-agent ones. Sealed without a pointer written
// onto the seat, the value was durable and unreachable, and the route checked
// every agent's delivery against the organization's secret and answered 401.
func TestAnAppsWebhookSecretIsSealedAndReachable(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.convertOneApp(t, "sre-lead", map[string]any{
		"id": 91, "slug": "acme-sre-lead", "name": "Acme sre-lead",
		"pem":            "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----",
		"webhook_secret": "the-apps-own-secret",
	})

	// SEALED under the seat's own name, beside its key.
	if got, ok := s.vault.get("GITHUB_APP_WEBHOOK_SECRET_SRE_LEAD"); !ok || got != "the-apps-own-secret" {
		t.Fatalf("the sealed webhook secret is %q (found=%v)", got, ok)
	}
	// AND POINTED AT from the seat, which is the half that was missing.
	body := s.seatDoc(t, "sre-lead")
	var seat struct {
		Integrations struct {
			GitHub struct {
				WebhookSecret string `json:"webhook_secret"`
				PrivateKey    string `json:"private_key"`
			} `json:"github"`
		} `json:"integrations"`
	}
	if err := json.Unmarshal(body, &seat); err != nil {
		t.Fatalf("decode the seat: %v", err)
	}
	if got := seat.Integrations.GitHub.WebhookSecret; got != "${GITHUB_APP_WEBHOOK_SECRET_SRE_LEAD}" {
		t.Errorf("the seat points at %q, so nothing can verify this app's deliveries", got)
	}
	// THE POINTER, NEVER THE VALUE. A secret in the document is one the
	// dashboard renders and a config export carries.
	if strings.Contains(string(body), "the-apps-own-secret") {
		t.Fatal("the app's webhook secret was written into the company document")
	}
	if strings.Contains(string(body), "BEGIN RSA PRIVATE KEY") {
		t.Fatal("the app's private key was written into the company document")
	}
}

// A GITHUB THAT RETURNS NO SECRET LEAVES NO POINTER.
//
// A `${VAR}` naming a secret nothing sealed resolves to nothing, and the
// route reads an empty secret as "cannot verify" and answers 503. So writing
// the pointer unconditionally would turn a seat that should fall back to the
// organization's secret into one whose every delivery is refused.
func TestASeatWithNoWebhookSecretHoldsNoPointerToOne(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.convertOneApp(t, "sre-lead", map[string]any{
		"id": 92, "slug": "acme-sre-lead", "name": "Acme sre-lead",
		"pem": "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----",
	})

	body := s.seatDoc(t, "sre-lead")
	if strings.Contains(string(body), "webhook_secret") {
		t.Fatalf("the seat points at a webhook secret nothing sealed: %s", body)
	}
	// AND THE KEY IS STILL THERE, because the conversion did happen: the
	// absent secret is one field missing, not a failed creation.
	if !strings.Contains(string(body), "${GITHUB_APP_KEY_SRE_LEAD}") {
		t.Fatalf("the seat lost its app key: %s", body)
	}
}

// A BLOCK IS THE OPT-IN, AND AN OPTED-IN SEAT WITH NO APP IS UNFINISHED.
//
// The two acts that build an agent's app are days apart, and before the first
// there is nothing on the seat but the tier it will run at. Read as a
// company's choice not to give this agent GitHub, that state left the card
// reporting the integration finished over an agent that acts as nobody, while
// the reconcile, which reports on exactly the seats that have a block, said
// action was needed. Two surfaces, two answers, one seat.
func TestASeatEnrolledInGitHubWithNoAppHoldsTheCardOpen(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", `{
	  "name": "Acme",
	  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
	  "integrations": {"public_base_url": "https://engine.example.com",
	    "github": {"enabled": true, "webhook_secret": "org", "provisioning": {"org": "acme"}}},
	  "roles": [
	    {"name": "Coder", "handle": "coder", "llm": "zulu",
	     "integrations": {"github": {"tier": "review"}}},
	    {"name": "Writer", "handle": "writer", "llm": "zulu"}
	  ]
	}`, map[string]string{"X-Summary": "one agent on GitHub, one not"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["satisfied"] != false {
		t.Fatalf("a GitHub whose enrolled agent has no app reports satisfied=%v",
			state["satisfied"])
	}

	seats := githubSeats(t, s)
	// THE ONE WITH A BLOCK IS ENROLLED, and holds nothing yet.
	coder := seats["coder"]
	if coder["enrolled"] != true || coder["present"] != false {
		t.Errorf("coder reports enrolled=%v present=%v", coder["enrolled"], coder["present"])
	}
	if coder["step"] != "create_app" {
		t.Errorf("coder is on step %v", coder["step"])
	}
	// AND THE ONE WITHOUT IS LISTED AND NOT ENROLLED. It is on the roster
	// because that is where an operator starts the flow; it is not work
	// outstanding, because a company running GitHub for some of its agents
	// chose that.
	writer := seats["writer"]
	if _, listed := seats["writer"]; !listed {
		t.Fatal("a seat with no GitHub block is missing from the roster, so " +
			"there is no way to start one for it")
	}
	if writer["enrolled"] == true {
		t.Error("a seat with no GitHub block reports itself enrolled, which " +
			"makes every company with one unfinished for ever")
	}
}

// A CARD WITH NOTHING LEFT TO TYPE OFFERS NO FORM.
//
// Both acts that produce an agent's app happen at GitHub, from that agent's
// own row: a manifest POSTed from a page carrying the operator's session, and
// an install. A GitHub seat's requirements are empty for exactly that reason,
// so a company whose only outstanding work is those two clicks has every box
// answered, and a Continue button beside it opened a form that could not
// create an app or install one.
func TestAGitHubCardWithOnlySeatWorkHasNoBoxLeftToFill(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.seedGitHub(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["satisfied"] != false {
		t.Fatalf("satisfied = %v, and agents still have apps to create", state["satisfied"])
	}
	if state["form_complete"] != true {
		t.Errorf("form_complete = %v: every box this dialog draws is answered, "+
			"and what is left is two clicks at GitHub", state["form_complete"])
	}
}

// AND A COMPANY MISSING A CREDENTIAL STILL OFFERS THE FORM.
//
// The two are independent: an unanswered box is what Continue fixes, whether
// or not an agent also has an app to create.
func TestAGitHubCardMissingACompanyAnswerStillHasABoxToFill(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	// Enabled, with the webhook secret left as a reference nothing resolves.
	res := s.do(t, http.MethodPatch, "/config", `{"integrations":{
		"public_base_url":"https://engine.example.com",
		"github":{"enabled":true,"webhook_secret":"${GH_NOT_SET}",
		"provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "github with an unresolved secret",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", res.Code, res.Body)
	}

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["form_complete"] != false {
		t.Errorf("form_complete = %v, and the webhook secret resolves to nothing",
			state["form_complete"])
	}
}

// DATADOG'S SEATS SAY HOW MUCH THEY MAY DO, in the same words as everyone
// else's.
//
// Datadog grades an account by putting it in a ROLE, and every account this
// engine creates goes into the one named on the provisioning block. That is
// still the answer to what an agent may do there, and it differs between
// companies, so the roster carries it exactly as a code host's tier is
// carried: a tag on the name rather than a second status beside it.
func TestADatadogSeatCarriesTheRoleItsAccountHolds(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", `{
	  "name": "Acme",
	  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
	  "integrations": {"datadog": {"enabled": true, "route_to": "sre-lead",
	    "webhook_token": "EXAMPLEDATADOGTOKEN0000000",
	    "provisioning": {"api_key": "dd-api", "app_key": "dd-app", "site": "datadoghq.com", "role": "Datadog Standard Role"}}},
	  "roles": [{"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"}]
	}`, map[string]string{"X-Summary": "datadog on a standard role"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil))
	rows, _ := state["seats"].([]any)
	if len(rows) == 0 {
		t.Fatal("datadog lists no seats")
	}
	seat, _ := rows[0].(map[string]any)
	// THE ENGINE'S OWN VOCABULARY, so an agent's Datadog access reads
	// beside its GitHub access rather than in a second grammar.
	if seat["tier"] != "review" {
		t.Errorf("the standard role reads as tier %v", seat["tier"])
	}
	// AND DATADOG'S PREFIX AND SUFFIX DROPPED. "Datadog Standard Role" on a
	// row that has already said which app it is about is noise.
	if seat["tier_label"] != "Standard" {
		t.Errorf("the role renders as %v", seat["tier_label"])
	}
	if hint, _ := seat["tier_hint"].(string); !strings.Contains(hint, "monitors") {
		t.Errorf("the role's hint is %q, and it does not say what it grants", hint)
	}
}

// A ROLE AN ORGANIZATION MADE IS PASSED THROUGH WHOLE.
//
// Datadog's roles are whatever its admins have made, so there is no closed
// set: shortening somebody's own name is how a reader stops recognising it,
// and claiming a tier for it would be this engine inventing how much a role
// it has never seen grants.
func TestACustomDatadogRoleKeepsItsOwnName(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", `{
	  "name": "Acme",
	  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
	  "integrations": {"datadog": {"enabled": true, "route_to": "sre-lead",
	    "webhook_token": "EXAMPLEDATADOGTOKEN0000000",
	    "provisioning": {"api_key": "dd-api", "app_key": "dd-app", "site": "datadoghq.com", "role": "Acme On-Call"}}},
	  "roles": [{"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"}]
	}`, map[string]string{"X-Summary": "datadog on a role of our own"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil))
	rows, _ := state["seats"].([]any)
	seat, _ := rows[0].(map[string]any)
	if seat["tier_label"] != "Acme On-Call" {
		t.Errorf("the custom role renders as %v", seat["tier_label"])
	}
	// NO TIER, because this engine has never seen the role and has no
	// basis for saying how much it grants. The screen draws it neutral.
	if tier, listed := seat["tier"]; listed && tier != "" {
		t.Errorf("a role this engine does not know was graded as %v", tier)
	}
}

// A FINISHED SEAT SAYS WHO IT IS ON GITHUB.
//
// The login is what appears on every commit, comment and review the agent
// writes, and what an operator matches against GitHub's own pages. The row
// said which repositories the seat reaches, which is a real difference
// between two finished seats and not the question a roster of agents raises
// first.
func TestAFinishedGitHubSeatNamesItsLogin(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)

	seat := githubSeats(t, s)["reviewer"]
	if seat == nil {
		t.Fatal("the roster does not list the finished seat")
	}
	detail, _ := seat["detail"].(string)
	if detail != "acme-reviewer" {
		t.Errorf("detail = %q, want the login this agent acts as", detail)
	}
	// AND NOT THE SCOPE AN OPERATOR ALREADY CHOSE. "Every repository the
	// installation covers" is the answer they gave when they installed the
	// app; repeating it after the login pushes the name off a narrow row to
	// say nothing.
	if strings.Contains(detail, "Every repository") {
		t.Errorf("detail = %q repeats the scope the install already settled", detail)
	}
}

// A GITHUB CARD IS NOT SATISFIED WHILE NO AGENT CAN ACT.
//
// One GitHub App is one bot identity, so an agent without its own app acts as
// nobody there. The satisfaction check asked only about seats that had
// STARTED — which is what lets a company running GitHub for three of its ten
// agents be finished when those three are — and a company where NOBODY had
// started passed it vacuously.
//
// Measured on a live connect: one agent seat with no app, `satisfied: true`
// and `seats_required: true` on the same answer, the card reading Connected,
// and the seat's own row saying "no app of its own yet, so this agent acts as
// nobody on GitHub".
func TestAGitHubToolWithNoWorkingAgentIsNotSatisfied(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	// A COMPANY WHERE NOBODY HAS STARTED: the org block is complete and no
	// seat has an app.
	res := s.do(t, http.MethodPut, "/config", `{
	  "name": "Acme",
	  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
	  "integrations": {
	    "public_base_url": "https://engine.example.com",
	    "github": {"enabled": true, "webhook_secret": "${GH_SIGN}",
	               "provisioning": {"org": "crewbed"}}
	  },
	  "roles": [{"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"}]
	}`, map[string]string{"X-Summary": "github connected for nobody"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "GH_SIGN", "s", "test", "test", pinned); err != nil {
		t.Fatal(err)
	}

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["seats_required"] != true {
		t.Fatalf("the premise is wrong: seats_required = %v", state["seats_required"])
	}
	if state["satisfied"] == true {
		t.Error("a GitHub card with no agent able to act reported satisfied, " +
			"which is what a person reads as done")
	}
}

// AND A COMPANY RUNNING GITHUB FOR SOME OF ITS AGENTS IS STILL FINISHED.
//
// The clause above must not turn the opt-in model into a permanent block: a
// company whose three enrolled agents are done is done, whatever the other
// seven chose.
func TestAGitHubToolWithOneWorkingAgentIsSatisfied(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"public_base_url":"https://engine.example.com",
		  "github":{"enabled":true,"webhook_secret":"${GH_SIGN}"}}}`,
		map[string]string{"X-Summary": "connect github"})
	if res.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "GH_SIGN", "s", "test", "test", pinned); err != nil {
		t.Fatal(err)
	}

	// The fixture's `reviewer` seat is installed with a key that resolves,
	// and `sre-lead` has no app at all.
	seats := githubSeats(t, s)
	if seats["reviewer"]["satisfied"] != true {
		t.Fatalf("the premise is wrong: reviewer = %v", seats["reviewer"])
	}
	if seats["sre-lead"]["satisfied"] == true {
		t.Fatalf("the premise is wrong: sre-lead = %v", seats["sre-lead"])
	}
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	// NOT satisfied, because `builder` and `stray` are ENROLLED and
	// unfinished — which is the pre-existing clause, still working.
	if state["satisfied"] == true {
		t.Error("a company with enrolled unfinished agents reported satisfied")
	}
}

// THE ORGANIZATION-WIDE ANSWER IS REFUSED WITHOUT ITS TOKEN, AT THE ROUTE.
//
// The form checks the same thing and that is a courtesy; this is the
// boundary. A submission choosing coverage of the whole organization and
// sending no token would otherwise store a choice the engine cannot carry
// out, leaving a company one apply later demanding an organization hook with
// nothing able to register it.
//
// AGAINST THE SUBMITTED VALUES, never the stored ones — this request is what
// changes the answer, so every gate read against the document would see the
// value being replaced and wave it through.
func TestTheOrgWideAnswerIsRefusedWithoutItsToken(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/inputs",
		`{"values":{"provisioning.org_webhook":"true"}}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	// NAMING THE FIELD, because a refusal that says only "invalid" leaves
	// somebody guessing which of the answers they gave was the problem.
	if body := res.Body.String(); !strings.Contains(body, "token") ||
		!strings.Contains(body, "provisioning.org_webhook") {
		t.Errorf("the refusal names neither the field nor the answer asking "+
			"for it:\n%s", body)
	}
}

// AND THE ANSWER THAT NEEDS NOTHING IS ACCEPTED WITH NOTHING.
//
// The whole point of the gate: a company taking the recommendation connects
// without a credential it will never use. Required in general, this was a
// personal access token demanded before anything worked.
func TestTheRecommendedAnswerNeedsNoToken(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/inputs",
		`{"values":{"provisioning.org_webhook":"false"}}`, nil)
	if res.Code < 200 || res.Code >= 300 {
		t.Fatalf("the recommended answer was refused: %d %s", res.Code, res.Body)
	}
}
