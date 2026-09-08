package setupapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A company with one agent and one person, on an engine that knows its own
// public address, which is what a Slack manifest's request URL is built from.
const slackDoc = `{
  "name": "Acme",
  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
  "integrations": {"public_base_url": "https://engine.example.com"},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "Jane Founder", "handle": "founder", "kind": "human",
     "contact": {"github_login": "jane"}}
  ]
}`

// slackSeats pulls the Slack roster out of the setup answer, by handle.
func slackSeatRows(t *testing.T, s *surface) map[string]map[string]any {
	t.Helper()
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/slack", "", nil))
	rows, _ := state["seats"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		handle, _ := seat["handle"].(string)
		out[handle] = seat
	}
	return out
}

// EVERY AGENT CARRIES THE APP IT IS BUILT FROM.
//
// Slack issues the credential that creates an app by hand, so an operator
// usually builds each agent's app themselves. Told only where to click, they
// were being asked to reproduce seventeen scopes and five event
// subscriptions from a documentation table, and the failure mode of getting
// one wrong is a bot that installs, reports success, and sees an empty
// workspace.
func TestASlackSeatCarriesTheManifestItsAppIsBuiltFrom(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	res := s.do(t, http.MethodPut, "/config", slackDoc,
		map[string]string{"X-Summary": "a company with an agent"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	seat := slackSeatRows(t, s)["sre-lead"]
	if seat == nil {
		t.Fatal("the roster does not list the agent")
	}
	text, _ := seat["manifest"].(string)
	if text == "" {
		t.Fatal("the seat carries no manifest, so its app has to be built by hand")
	}
	var manifest map[string]any
	if err := json.Unmarshal([]byte(text), &manifest); err != nil {
		t.Fatalf("the manifest is not JSON a person could paste: %v", err)
	}
	// THIS SEAT'S OWN ADDRESS, because a per-seat app has a route per agent
	// and a manifest carrying somebody else's delivers this agent's
	// mentions to another seat's inbox.
	if !strings.Contains(text, "https://engine.example.com/webhooks/slack/sre-lead") {
		t.Errorf("the manifest does not carry this seat's request URL:\n%s", text)
	}
	// AND THE SCOPES, which are the whole reason it is offered.
	if !strings.Contains(text, "app_mentions:read") || !strings.Contains(text, "chat:write") {
		t.Errorf("the manifest grants an app that could not hear or speak:\n%s", text)
	}
	// NO CREDENTIAL IN IT. It is a definition, not an install: everything
	// that authenticates comes back FROM Slack afterwards.
	for _, secret := range []string{"xoxb-", "client_secret", "signing_secret"} {
		if strings.Contains(text, secret) {
			t.Errorf("the manifest carries %q, which no definition should", secret)
		}
	}

	// A PERSON'S SEAT IS NOT AN AGENT'S. Nobody provisions a colleague a
	// Slack app.
	if _, ok := slackSeatRows(t, s)["founder"]; ok {
		t.Error("the roster lists a human seat")
	}
}

// NO ADDRESS, NO MANIFEST.
//
// The request URL is built from the company's public base, so a manifest
// written without one names no delivery address at all: the app installs, the
// agent is mentioned, and nothing ever arrives. Offering that to be pasted
// would be worse than offering nothing, because it looks finished.
func TestASlackSeatOffersNoManifestWithoutAPublicAddress(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	doc := strings.Replace(slackDoc,
		`"integrations": {"public_base_url": "https://engine.example.com"},`, "", 1)
	res := s.do(t, http.MethodPut, "/config", doc,
		map[string]string{"X-Summary": "a company with no public address"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	seat := slackSeatRows(t, s)["sre-lead"]
	if seat == nil {
		t.Fatal("the roster does not list the agent")
	}
	if text, _ := seat["manifest"].(string); text != "" {
		t.Errorf("a manifest was offered with no address to deliver to:\n%s", text)
	}
	// AND IT SAYS WHY, because the field help tells the operator to paste
	// one: a block with nothing in it and no reason given is an instruction
	// pointing at what is not there, and the cause is one edit away.
	note, _ := seat["manifest_note"].(string)
	if !strings.Contains(note, "public_base_url") {
		t.Errorf("manifest_note = %q, and it does not name the setting to fix", note)
	}
}

// A REFERENCE IS NOT AN ADDRESS, and the manifest is COPIED INTO SLACK.
//
// public_base_url is a Tier B field, so a whole ${VAR} is a legal way to
// write it and the document stores it verbatim. Read raw, the manifest an
// operator pastes carried "${PUBLIC_URL}/webhooks/slack/sre-lead" where an
// address belongs: Slack refuses the app, and nothing anywhere names the
// cause. The same mistake was measured on the Atlassian pass, which sent the
// literal ${ATLASSIAN_ORG_ID} to Atlassian.
func TestAnUnresolvedBaseProducesNoManifestRatherThanALiteralOne(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	doc := strings.Replace(slackDoc, `"public_base_url": "https://engine.example.com"`,
		`"public_base_url": "${PUBLIC_URL}"`, 1)
	res := s.do(t, http.MethodPut, "/config", doc,
		map[string]string{"X-Summary": "a company whose address is a reference"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	seat := slackSeatRows(t, s)["sre-lead"]
	if seat == nil {
		t.Fatal("the roster does not list the agent")
	}
	text, _ := seat["manifest"].(string)
	if strings.Contains(text, "${") {
		t.Errorf("the manifest carries a reference where an address belongs:\n%s", text)
	}
	if text != "" {
		t.Errorf("a manifest was built from an address this node cannot read:\n%s", text)
	}
	if note, _ := seat["manifest_note"].(string); note == "" {
		t.Error("nothing says why there is no manifest")
	}
	// AND THE BANNER TOO, which is the same value shown rather than pasted.
	if url, _ := seat["public_url"].(string); strings.Contains(url, "${") {
		t.Errorf("public_url = %q, which is a reference rather than an address", url)
	}
}

// A RESOLVED REFERENCE IS AN ADDRESS. The point is to read the value, not to
// refuse the shape.
func TestAResolvedBaseReferenceBuildsARealManifest(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	doc := strings.Replace(slackDoc, `"public_base_url": "https://engine.example.com"`,
		`"public_base_url": "${PUBLIC_URL}"`, 1)
	res := s.do(t, http.MethodPut, "/config", doc,
		map[string]string{"X-Summary": "a company whose address is a reference"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "PUBLIC_URL", "https://engine.example.com",
		"test", "test", pinned); err != nil {
		t.Fatal(err)
	}

	text, _ := slackSeatRows(t, s)["sre-lead"]["manifest"].(string)
	if !strings.Contains(text, "https://engine.example.com/webhooks/slack/sre-lead") {
		t.Errorf("the manifest does not carry the address the reference reads as:\n%s", text)
	}
}

// A NAME SLACK WOULD REFUSE SAYS SO, on the seat it belongs to.
//
// Slack caps an app name, so a long role name produces no manifest. Swallowed,
// that left one seat's block empty under a help line telling the operator to
// paste a manifest, with nothing anywhere naming the one-line fix.
func TestASeatWhoseNameSlackRefusesSaysSo(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	doc := strings.Replace(slackDoc, `"name": "SRE Lead"`,
		`"name": "Site Reliability Engineering Team Lead For Europe"`, 1)
	res := s.do(t, http.MethodPut, "/config", doc,
		map[string]string{"X-Summary": "a company with a long role name"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}

	seat := slackSeatRows(t, s)["sre-lead"]
	if seat == nil {
		t.Fatal("the roster does not list the agent")
	}
	if text, _ := seat["manifest"].(string); text != "" {
		t.Error("a manifest was offered for a name Slack would refuse")
	}
	note, _ := seat["manifest_note"].(string)
	// THE SENTENCE THE VENDOR PACKAGE WROTE, which names the field to edit
	// and the cap that was passed. Anything shorter is this layer inventing
	// a worse version of an error it already had.
	if !strings.Contains(note, "caps an app name") || !strings.Contains(note, "name") {
		t.Errorf("manifest_note = %q, and it does not say what to change", note)
	}
}
