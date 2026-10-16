package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/org"
)

// EVERY OBJECT ARGUMENT A DOMAIN DECLARES IS ONE A TOOL TAKES.
//
// The person's write surface names, beside a failed call, the object its
// arguments named — read through [builtin.ObjectArgs], each native domain's
// own list — so a client holding an unknown outcome knows which object to read.
// A name in that list that no tool declares is an argument renamed under it:
// the answer would name nothing, silently, for every call that took the new
// name. Read against the operator catalogue's own schemas, every seam wired.
//
// Mutation: rename any entry of a domain's objects and this goes red.
func TestEveryObjectArgumentIsOneAToolTakes(t *testing.T) {
	t.Parallel()
	full := fullDeps(t)
	company := &org.Organization{Name: "Acme"}
	answers := newAnswerRig()
	actor := full.Work.Actor
	declared := map[string]bool{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: full.Work, Pages: full.Pages, Knowledge: full.Knowledge,
		Org:  func() *org.Organization { return company },
		Runs: builtin.RunDeps{Desk: &deskFake{}, Actor: actor},
		Pauses: builtin.SeatPauseDeps{Pauses: coordmem.NewFleet(), Announce: &announced{},
			Org: func() *org.Organization { return company }, Actor: actor},
		Steer:  builtin.SteerDeps{Asker: &fleetAsker{}, Wire: steerWire, Actor: actor},
		Answer: builtin.AnswerDeps{Models: answers.model, Budget: answers.budget, Actor: actor},

		// AND THE ARTIFACTS' TOOLS, which name what they write by `artifact`.
		Artifacts: full.Artifacts, ArtifactData: full.ArtifactData,
	}) {
		props, _ := tool.Parameters()["properties"].(map[string]any)
		for name := range props {
			declared[name] = true
		}
	}
	objects := builtin.ObjectArgs()
	if len(objects) == 0 {
		t.Fatal("no domain declares an object argument, so this compares nothing")
	}
	if !slices.IsSorted(objects) {
		t.Errorf("ObjectArgs %v is not sorted", objects)
	}
	for _, name := range objects {
		if !declared[name] {
			t.Errorf("%q is a domain's object argument and no catalogue tool takes it", name)
		}
	}
}
