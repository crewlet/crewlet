package setupapi_test

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A company at every stage of giving its agents Mattermost bots: one opted in
// and sealed, one opted in and waiting for the sync, one that has not opted in
// at all, and a person, who is nobody's to provision.
const mattermostDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "mattermost": {"enabled": true, "url": "https://chat.example.com", "team": "acme"}
  },
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
     "integrations": {"mattermost": {"enabled": false, "bot_token": "${SRE_MATTERMOST_TOKEN}", "channels": ["engineering"]}}},
    {"name": "Builder", "handle": "builder", "llm": "zulu",
     "integrations": {"mattermost": {"enabled": false, "bot_token": "${BUILDER_MATTERMOST_TOKEN}"}}},
    {"name": "Reviewer", "handle": "reviewer", "llm": "zulu"},
    {"name": "Jane Founder", "handle": "founder", "kind": "human",
     "contact": {"github_login": "jane"}}
  ]
}`

// mattermostSeats pulls the Mattermost roster out of the setup answer.
func mattermostSeats(t *testing.T, s *surface) map[string]map[string]any {
	t.Helper()
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/mattermost", "", nil))
	rows, _ := state["seats"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		seat, _ := row.(map[string]any)
		handle, _ := seat["handle"].(string)
		out[handle] = seat
	}
	return out
}

// THE ROSTER IS THE ANSWER TO WHETHER THE AGENTS GOT BOTS.
//
// Every agent posts as its own bot here, so the company block connects
// nobody: it holds the address, the team and the operator credential the
// provisioner works through, and none of that is what an agent authenticates
// with. Without the roster the card read Connected over a company whose
// agents had no accounts, which is the half an operator cannot act on, and
// the one they were asking about.
func TestTheMattermostRosterNamesEveryAgentsBot(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, mattermostDoc)
	if err := s.vault.Set(t.Context(), "SRE_MATTERMOST_TOKEN", "a-real-bot-token",
		testAuthor, secrets.SourceAPI, pinned); err != nil {
		t.Fatal(err)
	}
	// THE PASS THIS APP HAS, because what the roster tells a seat with no
	// credential to do differs by whether the engine can create the account.
	s.withPass(t, &recordingPass{kind: integration.KindMattermost})

	seats := mattermostSeats(t, s)

	// EVERY AGENT AND ONLY THE AGENTS. The seat that has not opted in is the
	// row the flow starts from, so leaving it out would leave an operator no
	// way to see who is missing; a person's chat account is not the engine's.
	if len(seats) != 3 {
		t.Fatalf("roster = %v, want the three agent seats", keysOf(seats))
	}
	if _, ok := seats["founder"]; ok {
		t.Error("the roster lists a human seat, whose account nobody here provisions")
	}

	if sre := seats["sre-lead"]; sre["satisfied"] != true || sre["present"] != true {
		t.Errorf("sre-lead = %v, want the seat whose token is sealed reported working", sre)
	} else if detail, _ := sre["detail"].(string); detail != "@sre-lead" {
		// WHO THIS AGENT IS AT MATTERMOST, in Mattermost's own words. It
		// said where the credential was kept, which is a fact about this
		// company's YAML: true, identical for every agent, and no help to
		// somebody reading Mattermost's own user list.
		t.Errorf("sre-lead detail = %q, want the bot it posts as", detail)
	}

	// OPTED IN AND WAITING is not a fault: naming the variable is the one
	// thing the pass cannot do for the seat, and it is what puts the seat in
	// the plan. Reported as a mistake, the state an operator reaches by
	// doing exactly the right thing reads as one.
	builder := seats["builder"]
	if builder["satisfied"] == true {
		t.Error("builder is reported working with nothing sealed behind its ${VAR}")
	}
	if detail, _ := builder["detail"].(string); !strings.Contains(detail, "waiting for the next sync") {
		t.Errorf("builder detail = %q, want the sync it is waiting on", detail)
	}

	// AND THE SEAT THAT HAS NOT OPTED IN IS TOLD HOW TO: turned on at
	// Mattermost, where the engine provisions the bots, it gets one; nobody
	// names its token, and pointing an operator at a field to fill is the
	// same dead end as saying nothing.
	reviewer := seats["reviewer"]
	if reviewer["present"] == true {
		t.Error("a seat with no mattermost block is reported holding a credential")
	}
	detail, _ := reviewer["detail"].(string)
	if !strings.Contains(detail, "Agents tab") {
		t.Errorf("reviewer detail = %q, want where the agent is turned on", detail)
	}
	if strings.Contains(detail, "mcp_env") {
		t.Errorf("reviewer detail = %q sends an operator to mcp_env, where a "+
			"Mattermost token has never lived", detail)
	}
}

// keysOf names a roster's rows for a failure message.
func keysOf(seats map[string]map[string]any) []string {
	out := []string{}
	for handle := range seats {
		out = append(out, handle)
	}
	return out
}

// A company whose agents are on at Mattermost: two turned on, each with the
// channels it was given, one turned off, and one with no block at all.
const mattermostOnDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "integrations": {
    "mattermost": {"enabled": true, "url": "https://chat.example.com", "team": "acme"}
  },
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
     "integrations": {"mattermost": {"enabled": true, "roles": ["system_user"], "channels": ["engineering"]}}},
    {"name": "Builder", "handle": "builder", "llm": "zulu",
     "integrations": {"mattermost": {"enabled": true, "roles": ["system_user"], "channels": ["town-square"]}}},
    {"name": "Writer", "handle": "writer", "llm": "zulu",
     "integrations": {"mattermost": {"enabled": false, "roles": ["system_user"]}}},
    {"name": "Reviewer", "handle": "reviewer", "llm": "zulu"}
  ]
}`

// A DISCONNECT FINISHES WHILE AGENTS ARE STILL TURNED ON THERE. The document
// refuses an agent turned on at an integration the company has not connected,
// so the block alone could not be removed: the engine's teardown retried the
// same refused revision for ever, and forcing answered 400, until somebody
// turned every agent off by hand, one by one. The disconnect's revision now
// turns those agents off with the block, as ONE revision: each keeps what it
// was given (its channels), so connecting again and turning it on restores it,
// and a seat that was already off, or had no block, is not touched.
//
// THE CONTROL is the bare patch a disconnect used to send, which the document
// still refuses: the rule is right, and it is the disconnect that owed it the
// seats. Mutation: drop TurnOffDisconnected from configapi.Service.Disconnect
// and the forced disconnect answers the control's refusal.
func TestADisconnectTurnsOffTheAgentsStillOnThere(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, mattermostOnDoc)
	before := s.active(t).ID

	bare := s.do(t, http.MethodPatch, "/config", `{"integrations":{"mattermost":null}}`,
		map[string]string{"Content-Type": "application/merge-patch+json", "X-Summary": "the block alone"})
	if bare.Code < 400 || !strings.Contains(bare.Body.String(), "turned on at mattermost") {
		t.Fatalf("the block alone = %d: %s; want the document's refusal of agents left on",
			bare.Code, bare.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/mattermost", `{"force": true}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("a forced disconnect with agents on = %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	off, _ := body["seats_off"].([]any)
	if body["removed"] != true || len(off) != 2 ||
		!slices.Contains(off, any("sre-lead")) || !slices.Contains(off, any("builder")) {
		t.Fatalf("the disconnect answered removed %v and seats_off %v, want the two agents that were on",
			body["removed"], off)
	}
	after := s.active(t)
	if after.ID == before || after.ParentID != before {
		t.Errorf("the disconnect wrote revision %s over %s, want ONE revision on the one it read",
			after.ID, before)
	}
	doc := decode(t, s.do(t, http.MethodGet, "/config", "", nil))
	config, _ := doc["config"].(map[string]any)
	if config == nil {
		config = doc
	}
	if in, _ := config["integrations"].(map[string]any); in["mattermost"] != nil {
		t.Errorf("integrations.mattermost is still %v", in["mattermost"])
	}
	seats := map[string]map[string]any{}
	roles, _ := config["roles"].([]any)
	for _, r := range roles {
		role, _ := r.(map[string]any)
		in, _ := role["integrations"].(map[string]any)
		block, _ := in["mattermost"].(map[string]any)
		seats[role["handle"].(string)] = block
	}
	for _, handle := range []string{"sre-lead", "builder", "writer"} {
		if seats[handle]["enabled"] != false {
			t.Errorf("%s is %v at mattermost after the disconnect, want off", handle, seats[handle]["enabled"])
		}
	}
	if channels, _ := seats["sre-lead"]["channels"].([]any); !slices.Contains(channels, any("engineering")) {
		t.Errorf("sre-lead's channels are %v, want what it was given kept for a reconnect", channels)
	}
	if seats["reviewer"] != nil {
		t.Errorf("the reviewer, who had no block, was given one: %v", seats["reviewer"])
	}
}
