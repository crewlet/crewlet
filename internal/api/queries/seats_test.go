package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/tokens"
)

// EVERY AGENT SEAT IS IN THE SPEND ROLLUP'S DIRECTORY, BY AGENT ID.
//
// The per-agent spend rollup files a seat's records under its agent id (the
// one identifier a phase record carries that no namesake shares), and every
// row links to that seat's page by its HANDLE. This is the directory between
// the two. Keyed by role NAME, namesakes fold into one entry; walking only
// `company.Roles` misses every seat in a unit. Either way rows link nowhere
// with nothing failing.
func TestEveryAgentSeatIsInTheRollupDirectory(t *testing.T) {
	t.Parallel()
	cfg := parse(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: Chief Executive
    llm: zulu
  - name: Founder
    kind: human
    contact: {slack_user_id: U0F}
units:
  - name: Engineering
    id: eng
    roles:
      - name: Engineer
        handle: staff-eng
        llm: zulu
      - name: Engineer
        handle: site-reliability
        llm: zulu
`)
	seats := queries.Sources{Company: companySource(t, cfg)}.Seats()
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatalf("organization: %v", err)
	}

	for handle, want := range map[string]tokens.Seat{
		// The seat at the root, whose handle is derived from its name and
		// which sits in no unit.
		"chief-executive": {Handle: "chief-executive", Name: "Chief Executive"},
		// Two seats in a unit that SHARE A NAME: two entries, each linked to
		// its own page, where a map keyed by name held one.
		"staff-eng":        {Handle: "staff-eng", Name: "Engineer", UnitKey: "eng", UnitName: "Engineering"},
		"site-reliability": {Handle: "site-reliability", Name: "Engineer", UnitKey: "eng", UnitName: "Engineering"},
	} {
		id, ok := organization.AgentIDFor(organization.AgentSeatByHandle(handle))
		if !ok {
			t.Fatalf("%s is no agent seat in this fixture", handle)
		}
		if got := seats[id.String()]; got != want {
			t.Errorf("%s = %+v, want %+v — a row missing from this renders as one "+
				"a reader cannot click", handle, got, want)
		}
	}
	// AND THE HUMAN UNDER THEIR PERSON KEY: a person runs no turn, but what
	// the operator surface spent for them is filed under their human seat's
	// handle, and a row the directory cannot name prints whatever its newest
	// day carried instead of the seat's own name.
	if got, want := seats[tokens.PersonKey("founder")], (tokens.Seat{Handle: "founder", Name: "Founder"}); got != want {
		t.Errorf("the founder's entry = %+v, want %+v under their person key", got, want)
	}
	if len(seats) != 4 {
		t.Errorf("the directory holds %d seats, want every agent seat and the person: %v",
			len(seats), seats)
	}
}

// parse reads an authored company, which is what a fixture holds.
func parse(t *testing.T, doc string) *config.Company {
	t.Helper()
	c, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}
	return c
}
