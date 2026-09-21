package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
)

// A SEAT IN A UNIT HAS A CROSS-LINK IN THE TOKEN ROLLUP.
//
// # What this is guarding
//
// The per-agent spend rollup is keyed on a seat's role NAME — that is what a
// phase record carries — and every row in it links to that seat's page, which
// is addressed by its HANDLE. The map from one to the other is this.
//
// It walked `company.Roles`, which is the seats belonging to NO unit. A
// company of any size puts its agents in units instead, so the map was short
// by exactly those seats and their rows linked nowhere — and once a stored
// revision stopped carrying seats at all it was short by every one of them,
// for every company, with nothing failing: an absent cross-link renders as a
// row you cannot click.
func TestEverySeatHasACrossLinkInTheTokenRollup(t *testing.T) {
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
units:
  - name: Engineering
    id: eng
    roles:
      - name: Staff Engineer
        handle: staff-eng
        llm: zulu
      - name: Site Reliability
        llm: zulu
`)
	handles := queries.Sources{Company: companySource(t, cfg)}.RoleHandles()

	for name, want := range map[string]string{
		// The seat at the root, whose handle is derived from its name.
		"Chief Executive": "chief-executive",
		// A seat in a unit that DECLARED a handle, and one that did not:
		// both are seats, and a map that held only the root would have
		// neither.
		"Staff Engineer":   "staff-eng",
		"Site Reliability": "site-reliability",
	} {
		if got := handles[name]; got != want {
			t.Errorf("%q links to %q, want %q — a row whose handle is missing "+
				"renders as one a reader cannot click", name, got, want)
		}
	}
	if len(handles) != 3 {
		t.Errorf("the map holds %d seats, want every seat in the company: %v",
			len(handles), handles)
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
