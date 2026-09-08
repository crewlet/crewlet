package setupapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// A company at every stage of giving its agents Mattermost bots: one opted in
// and sealed, one opted in and waiting for the sync, one that has not opted in
// at all, and a person, who is nobody's to provision.
const mattermostDoc = `{
  "name": "Acme",
  "providers": {"llm": {"zulu": {"type": "anthropic", "model": "claude-sonnet-5", "api_keys": ["${K}"]}}},
  "integrations": {
    "mattermost": {"enabled": true, "url": "https://chat.example.com", "team": "acme"}
  },
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu",
     "integrations": {"mattermost": {"bot_token": "${SRE_MATTERMOST_TOKEN}", "channel": "engineering"}}},
    {"name": "Builder", "handle": "builder", "llm": "zulu",
     "integrations": {"mattermost": {"bot_token": "${BUILDER_MATTERMOST_TOKEN}"}}},
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
	res := s.do(t, http.MethodPut, "/config", mattermostDoc,
		map[string]string{"X-Summary": "a company mid-rollout"})
	if res.Code != http.StatusCreated {
		t.Fatalf("import = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "SRE_MATTERMOST_TOKEN", "a-real-bot-token",
		"test", "test", pinned); err != nil {
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
	} else if detail, _ := sre["detail"].(string); detail != "Bot @sre-lead" {
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

	// AND THE SEAT THAT HAS NOT OPTED IN IS TOLD WHERE TO, at the address it
	// actually writes to: a Mattermost token lives on the seat's own block,
	// not in mcp_env, and pointing an operator at the wrong file is the
	// same dead end as saying nothing.
	reviewer := seats["reviewer"]
	if reviewer["present"] == true {
		t.Error("a seat with no mattermost block is reported holding a credential")
	}
	detail, _ := reviewer["detail"].(string)
	if !strings.Contains(detail, "integrations.mattermost.bot_token") {
		t.Errorf("reviewer detail = %q, want the address a bot token is named at", detail)
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
