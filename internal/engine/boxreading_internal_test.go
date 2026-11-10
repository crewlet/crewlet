package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// boxedCompany is the CEO seat running in a container box made from image.
func boxedCompany(t *testing.T, image string) *Company {
	t.Helper()
	return companyFor(t, `name: Acme
providers:
  sandbox:
    local:
      image: `+image+`
      run_args: ["--memory", "4g"]
    default_run_in: container
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`)
}

// boxedEngine is a node with a fleet store and no access log: a turn is told
// its access and its box, and no change.
func boxedEngine(t *testing.T) (*Engine, *coordmem.Fleet) {
	t.Helper()
	e, _ := engineOn(t)
	fleet := coordmem.NewFleet()
	e.backends.Fleet = fleet
	return e, fleet
}

// A SEAT IS TOLD ITS BOX AS A RUN LAST FOUND IT, AND ONLY WHILE THE DOCUMENT
// STILL MAKES THE BOX THAT WAY. Before any run, the turn is told the cell and
// the memory run_args hold it to; after a fresh box's read, what its image
// has; after the image changes, the old reading is not told of the new box.
//
// Mutation: tell a reading whatever its basis, keep none at a launch, read a
// box whose reading is kept, or drop the document's memory, and this goes red.
func TestASeatIsToldItsBoxAsARunLastFoundIt(t *testing.T) {
	t.Parallel()
	e, fleet := boxedEngine(t)
	ctx := t.Context()
	company := boxedCompany(t, "registry.example.com/coding:1")
	told := func(c *Company) string {
		t.Helper()
		return e.capabilityFor(ctx, c, "ceo", "turn-1").access
	}
	unread := "- Your box: a container on the engine host, held to 4 GB of memory. No run has read yet"
	if got := told(company); !strings.Contains(got, unread) {
		t.Fatalf("before any run the seat is told\n%s", got)
	}
	if !e.boxUnread(ctx, company, "ceo", sandbox.Container) {
		t.Fatal("a launch before any reading is not asked to read its box")
	}

	e.keepBoxReading(ctx, company, "ceo", sandbox.Container,
		sandbox.BoxFacts{MemoryBytes: 4 << 30, Browser: "chromium"})
	read := "- Your box: a container on the engine host, held to 4 GB of memory. It has a browser " +
		"(chromium) and no container runtime"
	if got := told(company); !strings.Contains(got, read) {
		t.Fatalf("after a run read the box the seat is told\n%s", got)
	}
	if e.boxUnread(ctx, company, "ceo", sandbox.Container) {
		t.Fatal("a launch is asked to read a box whose reading is kept under its basis")
	}
	kept, found, err := fleet.BoxReading(ctx, ceoSeat, string(sandbox.Container))
	if err != nil || !found {
		t.Fatalf("the reading was not kept: %v", err)
	}
	e.keepBoxReading(ctx, company, "ceo", sandbox.Container,
		sandbox.BoxFacts{MemoryBytes: 4 << 30, Browser: "chromium"})
	if again, _, _ := fleet.BoxReading(ctx, ceoSeat, string(sandbox.Container)); !again.At.Equal(kept.At) {
		t.Errorf("an unchanged reading was written again: %v then %v", kept.At, again.At)
	}

	rebuilt := boxedCompany(t, "registry.example.com/coding:2")
	if got := told(rebuilt); !strings.Contains(got, unread) {
		t.Fatalf("a box made from a new image is told the old image's reading\n%s", got)
	}
	if !e.boxUnread(ctx, rebuilt, "ceo", sandbox.Container) {
		t.Fatal("a box made from a new image is not read again")
	}
}

// A REMOVED SEAT'S BOX READINGS GO WITH IT, so a handle used again starts with
// none; another seat's stay.
//
// Mutation: skip the sweep, or sweep every seat, and this goes red.
func TestARemovedSeatLeavesNoBoxReading(t *testing.T) {
	t.Parallel()
	e, fleet := boxedEngine(t)
	ctx := t.Context()
	for _, seat := range []string{"ceo", "ghost"} {
		if err := fleet.PutBoxReading(ctx, coord.BoxReading{Seat: seatOf(seat), Cell: "container",
			Basis: "b", At: time.Now()}); err != nil {
			t.Fatalf("PutBoxReading: %v", err)
		}
	}
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman, Source: "api", CreatedAt: time.Now()}
	activated, err := fleet.Activate(ctx, coord.ActivationRequest{RevisionID: "rev-1", Payload: []byte("{}"),
		At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	next := companyFor(t, "name: Acme\nroles:\n  - name: CEO\n    handle: ceo\n")
	next.ActivatedAt = activated.At
	e.clearRemovedBoxReadings(ctx, next)
	all, err := fleet.ListBoxReadings(ctx)
	if err != nil || len(all) != 1 || all[0].Seat != ceoSeat {
		t.Errorf("after the ghost seat left the readings are %+v (%v), want ceo's alone", all, err)
	}
}

// A BOX CHANGE IS TOLD AND ASKS NO REVIEW OF OPEN WORK. A seat whose box grew
// is told so on its next turn, but nothing it asked a person was raised under
// a box, so neither that turn nor an apply asks it to review its open work;
// a change of its access beside the box still does.
//
// Mutation: let a box change owe a review, or keep a box change from being
// told, and this goes red.
func TestABoxChangeIsToldAndAsksNoReview(t *testing.T) {
	t.Parallel()
	company := capabilityCompany(t, "Ship small changes.")
	box := capability.Box{Cell: capability.CellContainer, MemoryBytes: 4 << 30}
	snap := func(c *Company, memory int64) capability.Snapshot {
		b := box
		b.MemoryBytes = memory
		return capability.Take(capability.Input{Company: c.Config, Handle: "ceo", Boxes: []capability.Box{b}})
	}
	before := snap(company, 4<<30)
	recent := accessdomain.Recent{Complete: true, Baseline: &accessdomain.ToldRow{Handle: "ceo",
		Turn: uuid.NewString(), Digest: before.Digest(), Snapshot: before.JSON(), Latest: true}}

	grown := snap(company, 8<<30)
	told := decideTold(grown, recent, "ceo", uuid.NewString(), time.Now().UTC(), accessdomain.SnapshotMaxBytes)
	if !strings.Contains(told.changes, "Your box is now held to 8 GB of memory (was 4 GB).") || told.review {
		t.Fatalf("a grown box tells %q, review %v: want it told and no review", told.changes, told.review)
	}
	if _, owed := measured(grown, recent); owed {
		t.Fatal("a grown box alone owes the seat a review of its open work")
	}

	both := snap(capabilityCompany(t, "Ship small changes.", "Only the SRE operates production."), 8<<30)
	told = decideTold(both, recent, "ceo", uuid.NewString(), time.Now().UTC(), accessdomain.SnapshotMaxBytes)
	if !told.review {
		t.Fatal("a policy changed beside the box asks no review")
	}
	if m, owed := measured(both, recent); !owed || len(m.seatLines) != 2 {
		t.Fatalf("a policy changed beside the box is measured %+v, %v: want both lines owed", m.seatLines, owed)
	}
}
