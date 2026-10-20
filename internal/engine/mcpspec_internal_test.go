package engine

import (
	"maps"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// specCompany parses a company document into the epoch a seat's specs are
// built from.
func specCompany(t *testing.T, doc string) *Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company
}

// A SEAT'S CREDENTIALS REACH A PER-ROLE SERVER THROUGH WHAT ITS TRANSPORT
// AUTHENTICATES WITH: the environment of a stdio child, the headers of an http
// connection.
//
// They went into the environment whatever the transport, and an http
// connection reads none, so the remote GitHub server a seat reaches with its
// own `Authorization: Bearer …` was dialled with the template's headers alone
// — for every seat. The stdio neighbour is here to hold the other half: its
// credential stays in the environment, and an http child carries none.
func TestASeatsCredentialsReachAnHTTPServerAsHeaders(t *testing.T) {
	t.Parallel()
	company := specCompany(t, `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-spec-key"]
mcp_servers:
  - name: github
    transport: http
    shared: false
    url: "https://mcp.example.com/"
    headers:
      X-Workspace: "acme"
  - name: tracker
    shared: false
    command: "tracker-server"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      github:
        Authorization: "Bearer ${CEO_GITHUB_TOKEN}"
      tracker:
        SEAT_TOKEN: "${CEO_TRACKER_TOKEN}"
`)
	env := config.NewResolver(config.MapSource{
		"CEO_GITHUB_TOKEN": "ghp-ceo", "CEO_TRACKER_TOKEN": "tracker-ceo",
	})
	specs, outcomes := seatSpecs(company, company.Org.AgentSeatByHandle("ceo"), env)
	if len(outcomes) != 0 || len(specs) != 2 {
		t.Fatalf("specs = %+v, outcomes = %+v: want both servers launchable", specs, outcomes)
	}
	byName := map[string]int{}
	for i, spec := range specs {
		byName[spec.Server()] = i
	}

	github := specs[byName["github"]]
	wantHeaders := map[string]string{"X-Workspace": "acme", "Authorization": "Bearer ghp-ceo"}
	if !maps.Equal(github.Headers, wantHeaders) {
		t.Errorf("the http child's headers are %v, want the template's with the seat's "+
			"credential over them, %v", github.Headers, wantHeaders)
	}
	if len(github.Env) != 0 {
		t.Errorf("the http child carries an environment it never reads: %v", github.Env)
	}

	tracker := specs[byName["tracker"]]
	if got := tracker.Env["SEAT_TOKEN"]; got != "tracker-ceo" {
		t.Errorf("the stdio child's SEAT_TOKEN is %q, want the seat's credential", got)
	}
	if len(tracker.Headers) != 0 {
		t.Errorf("the stdio child carries headers no pipe sends: %v", tracker.Headers)
	}
}
