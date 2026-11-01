package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/chatlet"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
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
	declared := map[string]bool{}
	for _, props := range operatorArguments(t) {
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

// EVERY WRITE NAMES THE OBJECT IT WRITES — the other direction.
//
// A list of object arguments every one of which some tool takes can still be
// short: a write whose arguments name its object under a name the list lacks
// is answered, failed or unknown, with nothing but the tool's name beside it.
// write_page was that write — its page is addressed by its `container` and its
// title, and neither was a declared object — so a client holding a create
// whose outcome nobody could establish was told nothing about where to look.
// So every catalogue tool that is not a PROVEN read (the dispatch's own rule
// for what it audits) must take at least one declared object argument, or be
// named here with the reason it acts on no object of the company's records.
//
// Mutation: drop `container` from the pages domain's objects and write_page
// goes red; give a write no object argument and it is named.
func TestEveryWriteNamesTheObjectItWrites(t *testing.T) {
	t.Parallel()
	// namesNoObject are the writes that act on no object of a native domain,
	// each with why.
	namesNoObject := map[string]string{
		builtin.CreateArtifactTool: "makes a new artifact, which has no id to name " +
			"until it lands — an outcome nobody can vouch for is looked for with " +
			"list_artifacts, as its answer says",
		builtin.AnswerExecutorTool: "answers a parked run's question, named by its turn " +
			"— a run is no object of the company's records",
		builtin.SteerTurnTool: "hands a note to a running turn, named by its turn — " +
			"nothing is written to the company's records",
		chatlet.MarkChatReadTool: "records the caller's own read state in the " +
			"coordination store — no record of any domain's, and the caller is " +
			"its only object",
	}
	catalogue := operatorArguments(t)
	objects := builtin.ObjectArgs()
	writes := 0
	for name, props := range catalogue {
		if crewletmcp.ReadOnlyProven(builtin.AnnotationsFor(name)) {
			continue
		}
		writes++
		named := false
		for arg := range props {
			if slices.Contains(objects, arg) {
				named = true
				break
			}
		}
		why, exempt := namesNoObject[name]
		switch {
		case !named && !exempt:
			t.Errorf("%s is a write whose arguments name none of the declared objects "+
				"%v: declare the argument that names what it writes among its "+
				"domain's objects (writerclass.go), or name it here with why it "+
				"writes none", name, objects)
		case named && exempt:
			t.Errorf("%s names a declared object and is listed as naming none (%s): "+
				"drop it from the list", name, why)
		}
	}
	for name := range namesNoObject {
		if _, ok := catalogue[name]; !ok {
			t.Errorf("%s is listed as naming no object and the catalogue serves no "+
				"such tool: drop it from the list", name)
		}
	}
	// A GATE THAT READ NOTHING PASSES — so it says what it read.
	if writes < 10 {
		t.Fatalf("the catalogue serves %d writes; this is not reading it", writes)
	}
}

// operatorArguments is every operator catalogue tool's declared arguments, by
// tool name, with every seam wired so every tool is served.
func operatorArguments(t *testing.T) map[string]map[string]any {
	t.Helper()
	full := fullDeps(t)
	company := &org.Organization{Name: "Acme"}
	answers := newAnswerRig()
	actor := full.Work.Actor
	out := map[string]map[string]any{}
	talk := full.Chat
	talk.Cursors = fakeCursors{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: full.Work, Pages: full.Pages, Knowledge: full.Knowledge, Chat: talk,
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
		out[tool.Name()] = props
	}
	return out
}
