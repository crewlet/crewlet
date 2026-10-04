package config_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// handleDoc is a company whose root seat and unit seat declare no handle, and
// whose third seat declares one its name would not derive.
const handleDoc = `
name: Acme
roles:
  - name: Software Engineer
  - name: Team Lead
    handle: lead
units:
  - name: Design
    roles:
      - name: Designer
`

// EVERY SEAT GETS ITS HANDLE WRITTEN DOWN WHEN THE DOCUMENT IS READ, and a
// handle it declares is kept.
//
// A handle is a seat's identity — its agent id, its mailbox, its memory and
// every reference to it derive from it — so one left to be derived from the
// display name would move the day somebody corrected a typo in that name: a
// removal and a creation nobody asked for. Written into the document at
// parse, every stored revision carries it, and editing the name of a stored
// seat is only ever an edit of its name.
//
// The control is the same edit on a document that declares no handle: its
// derived handle moves, which is exactly what minting exists to stop.
//
// Mutation: drop MintIdentities from the parse and the first assertions fail.
func TestEverySeatGetsAHandleAtParse(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(handleDoc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]string{}
	for role := range cfg.EachRole() {
		got[role.Name] = role.Handle
	}
	want := map[string]string{
		"Software Engineer": "software-engineer",
		"Team Lead":         "lead",
		"Designer":          "designer",
	}
	for name, handle := range want {
		if got[name] != handle {
			t.Errorf("%s carries handle %q in the parsed document, want %q written "+
				"down", name, got[name], handle)
		}
	}

	// IDEMPOTENT: the stored form read again mints nothing new.
	stored, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	again, err := config.ParseCompanyDocument(stored)
	if err != nil {
		t.Fatalf("parse the stored form: %v", err)
	}
	if !slices.Equal(handlesOf(again), handlesOf(cfg)) {
		t.Errorf("a second read moved the handles: %v, was %v",
			handlesOf(again), handlesOf(cfg))
	}

	// A NAME EDIT ON THE STORED FORM KEEPS THE HANDLE.
	again.Roles[0].Name = "Senior Software Engineer"
	edited, err := json.Marshal(again)
	if err != nil {
		t.Fatal(err)
	}
	read, err := config.ParseCompanyDocument(edited)
	if err != nil {
		t.Fatalf("parse the edited form: %v", err)
	}
	if got := read.Roles[0].Handle; got != "software-engineer" {
		t.Errorf("renaming a stored seat moved its handle to %q", got)
	}

	// THE CONTROL: the same rename on a document that never declared the
	// handle derives another, which is what the stored form protects.
	fresh, err := config.ParseCompany([]byte(
		"name: Acme\nroles:\n  - name: Senior Software Engineer\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := fresh.Roles[0].Handle; got == "software-engineer" {
		t.Errorf("the control derived %q, so this case cannot tell a kept "+
			"handle from a derived one", got)
	}
}

// handlesOf is every seat's handle, in document order.
func handlesOf(c *config.Company) []string {
	var out []string
	for role := range c.EachRole() {
		out = append(out, role.Handle)
	}
	return out
}

// A STORED REVISION CARRIES ITS SEATS AND UNITS: the organisation a node builds
// from the stored bytes is the one the authored document describes.
//
// The org chart is part of the company document, so every revision a node
// applies is the whole company — and a stored form that dropped a seat, a unit
// key or a lead would run a different company on every node that read it from
// the store rather than from the file.
func TestAStoredRevisionCarriesItsSeatsAndUnits(t *testing.T) {
	t.Parallel()
	authored, err := config.ParseCompany([]byte(`
name: Acme
roles:
  - name: CEO
    handle: ceo
    manages: [eng]
units:
  - name: Engineering
    id: eng
    lead: cto
    roles:
      - name: CTO
        handle: cto
      - name: Software Engineer
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	payload, err := json.Marshal(authored)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := config.DecodeCompany(payload)
	if err != nil {
		t.Fatalf("decode the stored form: %v", err)
	}
	if err := stored.ValidateRunnable(); err != nil {
		t.Fatalf("the stored form does not run: %v", err)
	}
	// THE CONTROL: the authored org holds what this case looks for, or an
	// empty company on both sides would pass the comparison below.
	if want, err := authored.Organization(); err != nil ||
		want.Role("software-engineer") == nil || len(want.Units) != 1 {
		t.Fatalf("the authored org is not the fixture's: %v", err)
	}
	describe := func(c *config.Company) []string {
		o, err := c.Organization()
		if err != nil {
			t.Fatalf("org: %v", err)
		}
		var out []string
		for role := range o.AllRoles() {
			unit := ""
			if u := o.UnitFor(role); u != nil {
				unit = u.Key() + " led by " + u.Lead
			}
			out = append(out, role.Handle()+" in "+unit)
		}
		for _, u := range o.Units {
			out = append(out, "unit "+u.Key())
		}
		return out
	}
	if g, w := describe(stored), describe(authored); !slices.Equal(g, w) {
		t.Errorf("the stored revision runs\n%v\nwhere the document describes\n%v", g, w)
	}
}
