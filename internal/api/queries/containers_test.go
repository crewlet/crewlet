package queries_test

import (
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/pages"
)

// ownersCompany has one space two units share, a unit whose space differs
// from its project, a unit under a lead it inherits, a unit with no lead and
// no project, and a unit that names no space at all.
const ownersCompany = `
name: Acme
roles:
  - name: Ana Diaz
    handle: ana
    kind: human
    contact:
      crewlet_operator_id: ops-1
units:
  - name: Engineering
    id: eng
    lead: Ana Diaz
    space: ENG
    project: ENG
    children:
      - name: Core platform
        space: ENG
        project: PLAT
  - name: Operations
    space: OPS
  - name: Design
`

// A CONTAINER NAMES THE UNITS THAT WRITE INTO IT, THE WAY A PROJECT NAMES ITS
// UNIT: resolved against the running chart, each with its effective lead and
// the project it files work in — so a member's knowledge page says which team a
// space is without reading the company document, which is an admin's
// (ADR-0031). A space two units share names both, in the chart's own order,
// and a container no unit names says so with an empty list, never an absent
// key.
func TestAContainerNamesTheUnitsThatWriteIntoIt(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(ownersCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	stub := &stubPages{containers: []pages.ContainerListing{
		{Container: pages.Container{Key: "ENG", Name: "Engineering"}, Pages: 3},
		{Container: pages.Container{Key: "OPS"}},
		{Container: pages.Container{Key: "HANDBOOK"}},
	}}
	sources := queries.Sources{Pages: stub, Company: func() *config.Company { return cfg }}
	got, err := askAs(t, sources, "containers", nil, asMember("ops-1"))
	if err != nil {
		t.Fatalf("containers as a member: %v", err)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var decoded struct {
		Containers []struct {
			Key    string          `json:"key"`
			Pages  int             `json:"pages"`
			Owners json.RawMessage `json:"owners"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	want := map[string]string{
		"ENG": `[{"unit":{"key":"eng","name":"Engineering","resolved":true},` +
			`"lead":{"handle":"ana","kind":"human"},"project":"ENG"},` +
			`{"unit":{"key":"Core platform","name":"Core platform","resolved":true},` +
			`"lead":{"handle":"ana","kind":"human"},"project":"PLAT"}]`,
		"OPS":      `[{"unit":{"key":"Operations","name":"Operations","resolved":true},"lead":{}}]`,
		"HANDBOOK": `[]`,
	}
	if len(decoded.Containers) != len(want) {
		t.Fatalf("containers = %s, want the three the reader listed", body)
	}
	for _, c := range decoded.Containers {
		if string(c.Owners) != want[c.Key] {
			t.Errorf("%s's owners are\n\t%s\nwant\n\t%s", c.Key, c.Owners, want[c.Key])
		}
	}
	if decoded.Containers[0].Pages != 3 {
		t.Errorf("the listing's own facts were lost: %s", body)
	}
}
