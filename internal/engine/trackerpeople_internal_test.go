package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE TRACKER READS AND WRITES A PERSON BY THEIR SEAT'S IDENTITY, against the
// chart this node runs now.
//
// internal/tracker keys every person column on the handle a seat was CREATED
// under and shows it as the handle the seat answers to (its people.go), and
// what answers both questions is this engine's live epoch. Three answers
// matter: every handle a renamed seat answers to is one identity; that
// identity is shown as the current handle; and a retired alias another seat
// has since taken names THAT seat, not the one that gave it up — a name nobody
// holds is kept as written. And the one post-apply signal the applier sends a
// screen, whose inbox moved, is named as the seat is called now, because a
// socket watches a seat by the handle it answers to.
//
// And a READING is held: a call names every person by the chart as it stood
// when it pinned, so a rename landing mid-answer cannot split one answer
// across two charts.
//
// Mutation: answer Identity with the handle as given, or Current with the
// identity, or hand the movements on unmapped, or read the live company per
// name again rather than once per Pin, or resolve a people field to the handle
// the seat answers to now.
func TestTheTrackerKnowsAPersonByTheirSeatsIdentity(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	e.epoch.current.Store(&Company{Org: &org.Organization{Name: "Acme",
		Roles: []*org.Role{
			// Created as `cto`, renamed `chief`, which retired `boss`
			// on the way.
			{Name: "Chief", DeclaredHandle: "chief", OriginHandle: "cto",
				FormerHandles: []string{"cto", "boss"}},
			// A newer seat that took the retired `boss`.
			{Name: "Boss", DeclaredHandle: "boss"},
			{Name: "Ada", DeclaredHandle: "ada", Kind: org.KindHuman},
		}}})
	people := livePeople{engine: e}.Pin()
	for _, tc := range []struct{ handle, identity, current string }{
		{"chief", "cto", "chief"},
		{"cto", "cto", "chief"},
		{"boss", "boss", "boss"},
		{"ada", "ada", "ada"},
		{"jane.doe", "jane.doe", "jane.doe"},
		{"token:ops", "token:ops", "token:ops"},
	} {
		if got := people.Identity(tc.handle); got != tc.identity {
			t.Errorf("Identity(%q) = %q, want %q", tc.handle, got, tc.identity)
		}
		if got := people.Current(tc.identity); got != tc.current {
			t.Errorf("Current(%q) = %q, want %q", tc.identity, got, tc.current)
		}
	}

	// A HANDLE AS SOMEBODY TYPED IT is the seat it names.
	if got := people.Identity(" chief "); got != "cto" {
		t.Errorf("Identity(\" chief \") = %q, want cto", got)
	}

	var pushed []string
	e.SetOnInboxMoved(func(moved []tracker.InboxMovement) {
		for _, m := range moved {
			pushed = append(pushed, m.Handle)
		}
	})
	written := []tracker.InboxMovement{{Handle: "cto"}, {Handle: "jane.doe"}}
	e.inboxMoved(written)
	if !slices.Equal(pushed, []string{"chief", "jane.doe"}) {
		t.Errorf("the movements were handed on as %v, want the seat named as "+
			"it is called now", pushed)
	}
	if written[0].Handle != "cto" {
		t.Error("the applier's own movements were rewritten under it")
	}

	// A PEOPLE FIELD'S VALUE IS RESOLVED TO THE IDENTITY by the reading that
	// found the seat, so no second reading is asked about a handle it may
	// never have seen.
	for _, typed := range []string{"chief", "Chief"} {
		if got, held := (liveSeats{engine: e}).ResolveSeat(typed); !held || got != "cto" {
			t.Errorf("a people field naming %q resolved to (%q, %t), want the "+
				"seat's identity cto", typed, got, held)
		}
	}

	// A NODE WITH NO COMPANY answers every name as given, and a people
	// field names nobody there.
	empty := livePeople{engine: &Engine{}}.Pin()
	if got := empty.Identity("chief"); got != "chief" {
		t.Errorf("a node with no chart answered Identity(chief) = %q", got)
	}
	if got, held := (liveSeats{engine: &Engine{}}).ResolveSeat("chief"); held {
		t.Errorf("a node with no chart resolved a people field to %q", got)
	}

	// A READING OUTLIVES THE RENAME AFTER IT: the chart moves `chief` on to
	// `head`, and the reading taken before goes on calling it `chief` while
	// the next one calls it `head`.
	e.epoch.current.Store(&Company{Org: &org.Organization{Name: "Acme",
		Roles: []*org.Role{
			{Name: "Chief", DeclaredHandle: "head", OriginHandle: "cto",
				FormerHandles: []string{"cto", "boss", "chief"}},
		}}})
	if got := people.Current("cto"); got != "chief" {
		t.Errorf("a reading taken before the rename answered %q, want chief — "+
			"one call named one seat two ways", got)
	}
	if got := (livePeople{engine: e}).Pin().Current("cto"); got != "head" {
		t.Errorf("a reading taken after the rename answered %q, want head", got)
	}
}
