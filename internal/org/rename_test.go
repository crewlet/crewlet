package org

import (
	"slices"
	"testing"
)

// A RENAME KEEPS THE ID EVERYTHING DURABLE IS KEYED ON.
//
// The mailbox subject and its consumer group, the seat lease, the memory
// changelog's subjects and the schedule ledger's rows all address a seat by
// the id this derives. Deriving it from the CURRENT handle made every rename
// a brand-new seat: a fresh mailbox nobody had published to, a fresh lease,
// and an empty diary — with the old ones orphaned and unreachable.
func TestARenameKeepsTheIDTheLeaseTheMailboxAndTheDiary(t *testing.T) {
	t.Parallel()
	before := &Organization{Name: "Acme", Roles: []*Role{{Name: "Sarah Chen"}}}
	after := &Organization{Name: "Acme", Roles: []*Role{{
		Name:           "Sarah Okonkwo",
		DeclaredHandle: "sarah-okonkwo",
		OriginHandle:   "sarah-chen",
		FormerHandles:  []string{"sarah-chen"},
	}}}

	was, ok := before.AgentIDFor(before.Roles[0])
	if !ok {
		t.Fatal("the seat has no agent id before the rename")
	}
	is, ok := after.AgentIDFor(after.Roles[0])
	if !ok {
		t.Fatal("the seat has no agent id after the rename")
	}
	if was != is {
		t.Errorf("the rename moved the agent id from %s to %s. Everything "+
			"durable a seat owns is keyed on it, so a seat that moves it "+
			"wakes up with an empty diary and a mailbox nobody publishes to",
			was, is)
	}

	// THE CONTROL: without the frozen origin the id moves, which is what
	// makes the assertion above about the origin rather than about UUIDv5
	// being deterministic.
	naive := &Organization{Name: "Acme", Roles: []*Role{{
		Name: "Sarah Okonkwo", DeclaredHandle: "sarah-okonkwo"}}}
	if drifted, _ := naive.AgentIDFor(naive.Roles[0]); drifted == was {
		t.Error("a seat with no origin derives the same id as one that kept " +
			"it, so this case cannot tell the freeze from the derivation")
	}
}

// A RETIRED ADDRESS GOES ON RESOLVING, AND NEVER OVER A LIVE ONE.
//
// The chart moves the structure a rename touches, but it deliberately leaves
// the authored text of a `manages:` list alone — so the alias is the only
// thing keeping an entry somebody typed pointing at the person it named.
func TestARetiredHandleResolvesAndALiveOneAlwaysWins(t *testing.T) {
	t.Parallel()
	renamed := &Role{Name: "Sarah Okonkwo", DeclaredHandle: "sarah-okonkwo",
		FormerHandles: []string{"sarah-chen"}}
	reused := &Role{Name: "Sam Chen", DeclaredHandle: "sarah-chen"}
	o := &Organization{Name: "Acme", Roles: []*Role{renamed, reused}}

	if got := o.Role("sarah-okonkwo"); got != renamed {
		t.Errorf("the live handle resolves to %v", got)
	}
	// The LIVE seat wins the address, although the renamed one lists it as
	// retired and comes first in the walk.
	if got := o.Role("sarah-chen"); got != reused {
		t.Errorf("a retired handle outranked the live seat holding that "+
			"address: %v. A live handle must never lose to somebody else's "+
			"alias, or a rename silently redirects a colleague's mail", got)
	}
	o.Roles = []*Role{renamed}
	if got := o.Role("sarah-chen"); got != renamed {
		t.Errorf("with nothing else holding it the retired handle resolves to "+
			"%v, want the seat that used to answer to it", got)
	}
	if got := o.AgentSeatByHandle("sarah-chen"); got != renamed {
		t.Errorf("AgentSeatByHandle answers %v for a retired handle — it "+
			"resolves through Role so the two can never disagree", got)
	}
}

// A RENAMED UNIT STILL PLACES ITS SEATS AND STILL CASCADES ITS LEAD.
//
// Every `unit:` reference is a KEY, so a rename with no alias silently drops
// each referencing seat at the root: out of its unit's credential
// inheritance, invisible to the unit lead, and with no lead of its own.
func TestARenamedUnitStillResolvesEverySeatsUnitReferenceAndItsLeadCascade(t *testing.T) {
	t.Parallel()
	build := func(aliases []string) *Organization {
		return &Organization{
			Name: "Acme",
			Roles: []*Role{
				{Name: "Ada", DeclaredHandle: "ada", UnitRef: "platform"},
				// The lead has been renamed too, and the unit still
				// names them by the handle they retired.
				{Name: "Vee", DeclaredHandle: "vee", FormerHandles: []string{"lead"}},
			},
			Units: []*Unit{{
				Name: "Infrastructure", ID: "infrastructure",
				Lead: "lead", FormerKeys: aliases,
			}},
		}
	}

	o := build([]string{"platform"})
	o.Normalize()
	unit := o.Unit("infrastructure")
	if len(unit.Roles) != 1 || unit.Roles[0].Handle() != "ada" {
		t.Fatalf("the unit holds %d members, want the seat whose `unit:` "+
			"names its retired key", len(unit.Roles))
	}
	if lead := o.EffectiveLead(unit); lead == nil || lead.Handle() != "vee" {
		t.Errorf("the moved seat's unit has no effective lead: %v", lead)
	}
	if got := o.Manager(unit.Roles[0]); got == nil || got.Handle() != "vee" {
		t.Errorf("the unit lead does not auto-manage the seat placed through "+
			"the retired key: %v", got)
	}
	// AND THE DANGLING REPORT AGREES WITH THE LOOKUPS: the unit's `lead:`
	// is the handle that seat retired, and it resolves.
	if refs := o.DanglingRefs(); len(refs) != 0 {
		t.Errorf("a reference that resolves is reported as dangling: %v. This "+
			"report must answer exactly what Role and Unit answer, or an "+
			"operator is sent to fix a reference that works", refs)
	}

	// THE CONTROL: with no alias set the same document drops the seat at
	// the root, which is the failure the alias exists to prevent.
	control := build(nil)
	control.Normalize()
	if len(control.Roles) != 2 {
		t.Errorf("without the alias the seat was still placed, so this case " +
			"proves nothing about the alias")
	}
}

// AND A `manages:` ENTRY NAMING A RENAMED COLLEAGUE STILL MANAGES THEM.
//
// The entry is rewritten to the handle the seat answers to NOW, because that
// is what [Organization.Manager] compares against: it reads a live handle and
// resolves nothing, so an entry left at the retired spelling would leave the
// seat with no manager at all.
func TestAManagesEntryNamingARetiredHandleLandsOnTheLiveOne(t *testing.T) {
	t.Parallel()
	report := &Role{Name: "Sarah Okonkwo", DeclaredHandle: "sarah-okonkwo",
		FormerHandles: []string{"sarah-chen"}}
	boss := &Role{Name: "Vee", DeclaredHandle: "vee", Manages: []string{"sarah-chen"}}
	o := &Organization{Name: "Acme", Roles: []*Role{boss, report}}
	o.Normalize()

	if !slices.Equal(boss.Manages, []string{"sarah-okonkwo"}) {
		t.Errorf("manages = %v, want the handle the seat answers to now",
			boss.Manages)
	}
	if got := o.Manager(report); got != boss {
		t.Errorf("the renamed seat's manager is %v, want the seat whose "+
			"authored entry named them", got)
	}
	if got := o.Reports(boss); len(got) != 1 || got[0] != report {
		t.Errorf("reports = %v, want the renamed seat", got)
	}
}
