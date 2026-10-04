package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/tokens"
)

// EVERY AGENT SEAT IS IN THE SPEND ROLLUP'S DIRECTORY, BY AGENT ID.
//
// # What this is guarding
//
// The per-agent spend rollup files a seat's records under its agent id — the
// one identifier a phase record carries that no namesake shares — and every
// row links to that seat's page, which is addressed by its HANDLE. The
// directory from one to the other is this.
//
// It was a map from role NAME to handle, which two seats sharing a name folded
// into one entry. And before that it walked `company.Roles`, the seats
// belonging to NO unit: a company of any size puts its agents in units, so the
// map was short by exactly those seats and their rows linked nowhere, with
// nothing failing — an absent cross-link renders as a row you cannot click.
func TestEveryAgentSeatIsInTheRollupDirectory(t *testing.T) {
	t.Parallel()
	cfg := parse(t, `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
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
	// THE HUMAN IS NOT HERE: a person runs no turn and bills nothing, and an
	// entry for them would be a directory row no record can ever name.
	if len(seats) != 3 {
		t.Errorf("the directory holds %d seats, want every agent seat and no person: %v",
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
