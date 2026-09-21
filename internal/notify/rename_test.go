package notify_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// renamed is the fixture company with one seat that has been renamed: it is
// addressed as `head-reliability` today and was created as `sre-lead`.
func renamed() *org.Organization {
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			{
				Name: "Head of Reliability", DeclaredHandle: "head-reliability",
				OriginHandle: "sre-lead", FormerHandles: []string{"sre-lead"},
			},
			{Name: "Backend Engineer"},
		},
	}
	o.Normalize()
	return o
}

// A RENAMED SEAT'S PARTY CARRIES THE ID ITS MAILBOX IS ACTUALLY ON.
//
// The registry is how the notification spine turns a routed handle into
// something it can publish to, and the id it carries is the subject. Derived
// by hashing the handle in hand rather than by asking the organization, a
// renamed seat's party held a uuid that no mailbox, no seat lease and no
// ledger anywhere in the fleet is named after — so every notification the
// spine routed through it was published to a subject nothing consumes, and
// the send reported success.
func TestARenamedSeatsPartyCarriesTheIDItsMailboxIsOn(t *testing.T) {
	t.Parallel()
	o := renamed()
	r := notify.NewRegistry(o)

	p, ok := r.ByHandle("head-reliability")
	if !ok {
		t.Fatalf("the renamed seat is not in the registry")
	}
	want, ok := o.AgentIDFor(o.Role("head-reliability"))
	if !ok {
		t.Fatalf("the fixture seat derives no agent id")
	}
	if p.AgentID != want {
		t.Errorf("party id = %s, want %s — the id every durable thing this "+
			"seat owns is named by", p.AgentID, want)
	}
	// AND IT IS NOT THE LIVE HANDLE'S HASH, which is what it used to be.
	// Stated separately because the two are equal for every seat that was
	// never renamed, so a fixture that forgot to rename would satisfy the
	// check above and prove nothing.
	stale, _ := org.DeriveAgentID(o.Name, "head-reliability")
	if p.AgentID == stale {
		t.Errorf("party id is the hash of the seat's current address, which " +
			"names nothing in the fleet")
	}
}

// AND A REFERENCE SOMEBODY WROTE UNDER THE OLD NAME STILL REACHES IT.
//
// A Datadog monitor tagged `crewlet:sre-lead`, a channel topic, a runbook
// line: every one of those is a handle a person wrote down, which is what
// the chart's alias list is for. [org.Organization.Role] has resolved them
// since the rename gesture existed, so a registry that did not was the same
// string answering on one path and nobody on the other — and on this path
// "nobody" is an alert that wakes no one while the seat sits there working.
func TestAReferenceToARetiredHandleStillNamesTheSeat(t *testing.T) {
	t.Parallel()
	r := notify.NewRegistry(renamed())

	p, ok := r.ByHandle("sre-lead")
	if !ok {
		t.Fatalf("the retired handle resolves to nobody")
	}
	// THE PARTY IS THE CURRENT SEAT'S. An alias is another way to say one
	// seat, so what comes back must be addressed as the chart addresses it
	// now — a Party still calling itself `sre-lead` would put a retired
	// name back into a rendered prompt and a delivery receipt.
	if p.Handle != "head-reliability" {
		t.Errorf("party handle = %q, want the seat as it is addressed today", p.Handle)
	}
	// AND THE ROSTER IS UNCHANGED: an alias adds a way in, never a colleague.
	if n := r.Len(); n != 2 {
		t.Errorf("parties = %d, want the company's two seats", n)
	}
}

// A LIVE HANDLE ALWAYS WINS OVER SOMEBODY ELSE'S RETIRED ONE.
//
// The case the second pass exists for: a seat renamed AWAY from a name that
// another seat now holds. Resolved in org order, which seat answered would
// depend on which came first in the document — so the company's own roster
// would silently decide where a mention landed.
func TestALiveHandleOutranksAnotherSeatsAlias(t *testing.T) {
	t.Parallel()
	o := &org.Organization{
		Name: "nimbus",
		Roles: []*org.Role{
			// Listed FIRST, so a single pass in org order would let this
			// seat's retired name shadow the seat below that holds it now.
			{
				Name: "Head of Reliability", DeclaredHandle: "head-reliability",
				OriginHandle: "sre-lead", FormerHandles: []string{"sre-lead"},
			},
			{Name: "SRE Lead"},
		},
	}
	o.Normalize()

	p, ok := notify.NewRegistry(o).ByHandle("sre-lead")
	if !ok {
		t.Fatalf("sre-lead resolves to nobody")
	}
	if p.Name != "SRE Lead" {
		t.Errorf("sre-lead resolved to %q, want the seat that holds that "+
			"handle today", p.Name)
	}
}
