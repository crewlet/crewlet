package config

import (
	"strings"
	"testing"
)

// THE DOCUMENT WALK STILL REACHES EVERY SEAT IN THE FILE.
//
// # Why this is worth its own case now that the walk is unexported
//
// `crewlet validate` reads an authored file whole, and what its cross-field
// rules check IS the document — a seat's model chain against the providers
// declared beside it, a `manages:` entry against the units, a per-seat
// credential against the integration block it belongs to. Every one of those
// rules is applied by walking this.
//
// The walk was unexported because outside this package it answers the wrong
// question: a company's seats are the org chart's own log, and a stored
// revision carries no `roles:` and no `units:` at all, so the same call there
// walks an empty list and exempts every seat with no error and no symptom.
//
// What that unexport must NOT have done is narrow the walk itself. A rule
// that holds for a seat in `roles:` and not for the identical seat one level
// down is not a rule, and the seats it misses are exactly the ones whose
// mistakes have no run-time symptom to find them by — which is the failure
// this walk was introduced to fix in the first place.
func TestTheDocumentWalkReachesEverySeatAtEveryDepth(t *testing.T) {
	t.Parallel()
	c, err := ParseCompany([]byte(strings.TrimSpace(`
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
        llm: zulu
    children:
      - name: Infrastructure
        id: infra
        roles:
          - name: Site Reliability
            llm: zulu
`)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	seen := map[string]string{}
	for role, path := range c.eachRole() {
		seen[role.Name] = path.String()
	}
	for name, want := range map[string]string{
		"Chief Executive":  "roles[0]",
		"Staff Engineer":   "units[0].roles[0]",
		"Site Reliability": "units[0].children[0].roles[0]",
	} {
		if got, held := seen[name]; !held || got != want {
			t.Errorf("%q walked at %q (held=%t), want %q — a seat the walk "+
				"misses is one every cross-field rule silently exempts",
				name, got, held, want)
		}
	}
	if len(seen) != 3 {
		t.Errorf("the walk reached %d seats, want every seat in the file: %v",
			len(seen), seen)
	}
}
