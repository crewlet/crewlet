package builtin_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY TOOL THIS BUILD REGISTERS HAS AN AUTHORITY RULE. internal/authz can
// hold its table only against itself, so a tool added without a row ships
// ungated and looking correct, [authz.Decide]'s unknown-action refusal the only
// backstop. The walk lives here because internal/authz cannot import the tools,
// and a hand-kept list of names is the drift it exists to catch.
func TestEveryRegisteredToolHasAnAuthorityRule(t *testing.T) {
	t.Parallel()
	names := everyToolName(t)
	if len(names) == 0 {
		t.Fatal("no tools registered, so this walk certifies nothing")
	}
	for _, name := range names {
		if _, known := authz.ClassOf(authz.Action(name)); !known {
			t.Errorf("the tool %q has no row in the authority table, so "+
				"internal/authz refuses it with unknown_action — which is the "+
				"backstop, not the gate", name)
		}
	}
}

// AND EVERY TOOL-SHAPED VERB IN THE TABLE IS A TOOL THIS BUILD REGISTERS: the
// direction that decays quietly, a row deciding nothing that internal/authz,
// consistent with itself, cannot see. Told apart by the dot: a verb a tool
// serves carries none, and one with no tool behind it (an HTTP-only surface, a
// route's store call) is spelled `pages.trash` (internal/authz's doc).
func TestEveryToolShapedActionIsARegisteredTool(t *testing.T) {
	t.Parallel()
	names := everyToolName(t)
	for _, action := range authz.Actions() {
		if strings.Contains(string(action), ".") || unwiredGrowthTool(string(action)) {
			continue
		}
		if !slices.Contains(names, string(action)) {
			t.Errorf("the authority table has a row for %q, which names no "+
				"tool this build registers — a rule deciding nothing. Either "+
				"the verb is gone and the row goes with it, or it is served "+
				"somewhere other than a tool and its name takes a dot",
				action)
		}
	}
}

// EVERY APP OPENS A TOOL A PERSON REACHES. A person uses an app through the
// operator catalogue (the act route, their assistant, the HTTP write surface),
// so an app no operator tool belongs to changes nothing they can do.
// internal/authz holds that some row names every app; this holds it against
// the tools a person is served, which that package cannot import.
func TestEveryAppOpensAPersonFacingTool(t *testing.T) {
	t.Parallel()
	opened := map[iam.Grant][]string{}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		app, known := authz.AppOf(authz.Action(tool.Name()))
		if known && app != "" {
			opened[app] = append(opened[app], tool.Name())
		}
	}
	for _, app := range iam.Apps {
		if len(opened[app]) == 0 {
			t.Errorf("the app %q opens no tool the operator catalogue serves, so a "+
				"person holding it can do nothing with it", app)
		}
	}
}

// NO SEAT-ONLY VERB IS OFFERED TO A PERSON. The table refuses `a2a_ask` (it
// spends a colleague seat's turn) to anybody but a seat, so a person's
// catalogue listing it would offer a tool every call to which is refused, and
// a screen a button that never works.
func TestNoSeatOnlyVerbIsOfferedToAPerson(t *testing.T) {
	t.Parallel()
	offered := map[string]bool{}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		offered[tool.Name()] = true
	}
	seatOnly := 0
	for _, action := range authz.Actions() {
		only, _ := authz.SeatOnlyOf(action)
		if !only {
			continue
		}
		seatOnly++
		if offered[string(action)] {
			t.Errorf("the operator catalogue offers %q to a person, and the table "+
				"refuses it to anybody but a seat", action)
		}
	}
	if seatOnly == 0 {
		t.Fatal("the table marks no verb seat-only, so this walk certifies nothing")
	}
}

// NO VERB MADE AS A SEAT IS OFFERED TO A CREDENTIAL ACTING AS ITSELF. A
// person's surface offers what [builtin.Admits] says (the dashboard's
// `viewer.acts`), so every seated verb the operator catalogue serves must be
// refused [authz.ReasonUnseated] to an all-grants credential bound to no seat,
// yet offered to a person bound to a seat holding the same, or a surface
// refusing everybody would pass. Mutation: drop `seated` from
// answer_knowledge's row and this goes red.
func TestNoSeatedVerbIsOfferedToACredentialActingAsItself(t *testing.T) {
	t.Parallel()
	authorize := builtin.Decide(chartLeads)
	unbound := iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee3"),
		Kind: iam.KindMachine, Login: "ci:release", Stage: iam.StageActive,
		Grants: iam.AllGrants,
	})
	bound := iam.WithPrincipal(context.Background(), iam.Principal{
		ID:   uuid.MustParse("018f3a9c-0000-7000-8000-00000000fee4"),
		Kind: iam.KindPerson, Login: "jane.doe", Seat: "jane", Stage: iam.StageActive,
		Grants: iam.AllGrants,
	})
	seated := 0
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		only, _ := authz.SeatedOf(authz.Action(tool.Name()))
		if !only {
			continue
		}
		seated++
		err := builtin.Admits(unbound, authorize, tool.Name())
		var refused *builtin.DecisionRefused
		if !errors.As(err, &refused) || refused.Decision.Reason != authz.ReasonUnseated {
			t.Errorf("%s is offered to a credential acting as no seat (%v), "+
				"and the table refuses its every call", tool.Name(), err)
		}
		if err := builtin.Admits(bound, authorize, tool.Name()); err != nil {
			t.Errorf("%s is not offered to a person bound to a seat: %v",
				tool.Name(), err)
		}
	}
	if seated == 0 {
		t.Fatal("the operator catalogue serves no seated verb, so this walk " +
			"certifies nothing")
	}
}

// EVERY APP ROW BELONGS TO ITS KIND'S APP. Nothing in the table pairs a verb
// with the KIND it acts on, so `list_pages` could carry the tracker with every
// internal/authz check passing. A tool's catalogue (the tracker's, the
// knowledge base's, chat's) is where the kind is stated, so each of their tools
// whose row names an app names that catalogue's (a row naming none, a seat's
// own or everybody's, is not an app row).
func TestEveryAppRowHasItsKindsApp(t *testing.T) {
	t.Parallel()
	for app, names := range map[iam.Grant][]string{
		iam.GrantWorklet: append(builtin.WorkTools(), worklet.OperatorOnlyTools()...),
		iam.GrantDoclet:  append(builtin.PageTools(), builtin.SearchKnowledgeTool),
		iam.GrantChatlet: append(chatlet.Tools(), chatlet.OperatorOnlyTools()...),
	} {
		for _, name := range names {
			got, known := authz.AppOf(authz.Action(name))
			if !known {
				t.Errorf("%s has no row in the authority table", name)
				continue
			}
			if got != "" && got != app {
				t.Errorf("%s is one of %s's tools and its row names the app %q",
					name, app, got)
			}
		}
	}
}

// everyToolName is every verb this build serves as a tool, over BOTH surfaces:
// a seat's registry and the operator's assistant each serve verbs the other
// does not, and either alone makes the walks above wrong in both directions.
// unwiredGrowthTool reports whether name is a tool of a growth catalog this
// test binary links and [fullDeps] does not wire: Google Play's, which its
// end-to-end test links. Only the fake product's tools are offered.
func unwiredGrowthTool(name string) bool {
	for _, c := range growth.Catalogs() {
		if _, listed := c.Tool(name); listed {
			return c.Kind != "fakeads"
		}
	}
	return false
}

func everyToolName(t *testing.T) []string {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(fullDeps(t))); err != nil {
		t.Fatalf("Register: %v", err)
	}
	names := reg.Names()
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		if !slices.Contains(names, tool.Name()) {
			names = append(names, tool.Name())
		}
	}
	return names
}

// operatorDeps wires EVERY operator tool on: OperatorTools drops a tool whose
// dependency is nil (a company on Jira gets no Worklet), so a nil here is a
// verb the walks silently stop covering.
func operatorDeps(t *testing.T) builtin.OperatorDeps {
	t.Helper()
	trk := newFakeTracker()
	person := &personSpy{}
	full := fullDeps(t)
	company := &org.Organization{Name: "Acme"}
	orgOf := func() *org.Organization { return company }
	answers := newAnswerRig()
	ledger := seededAtlasLedger()
	return builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Search: trk, Inbox: trk,
			Merges:        func(builtin.Actor) builtin.WorkMerger { return trk },
			Moves:         trk.moves,
			PersonWriter:  func(builtin.Actor) builtin.PersonWriter { return person },
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
			Placer:        func(builtin.Actor) builtin.WorkPlacer { return nil },
			// A constructor answering nil still registers the tool:
			// OperatorTools gates on the seam being wired, and the
			// walks only read names.
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			// The project files too, over the seat registry's seams.
			Files:          full.Work.Files,
			FileWriter:     full.Work.FileWriter,
			Objects:        full.Work.Objects,
			Seats:          func() []colleague.Seat { return nil },
			DefaultProject: func(string) string { return "ENG" },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator}, nil
			},
		},
		Pages:        full.Pages,
		Knowledge:    full.Knowledge,
		Artifacts:    full.Artifacts,
		ArtifactData: full.ArtifactData,
		// The atlas: the app's one person-facing tool, without which
		// holding atlas would open nothing here.
		Atlas: builtin.AtlasDeps{Settings: runsAcme, Records: atlasLogOnly(ledger)},
		// And its writes.
		AtlasWrite: builtin.AtlasWriteDeps{Log: ledger, Actor: builtin.PrincipalActor, Org: orgOf},
		// And the ask that one of its sources be read now.
		AtlasRefresh: builtin.AtlasRefreshDeps{Sources: stillSources{}, Actor: builtin.PrincipalActor},
		// And a person's Home board, on the coordination twin.
		HomeBoard: builtin.HomeBoardDeps{Boards: coordmem.NewFleet(), Actor: builtin.PrincipalActor},
		// And their own Ask Crewlet instructions.
		Instructions: builtin.InstructionsDeps{Store: coordmem.NewFleet(), Actor: builtin.PrincipalActor},
		// The code tools, over the seat registry's seams, read as the
		// request's principal.
		Code: builtin.CodeDeps{Code: full.Code.Code, Writer: full.Code.Writer, Holder: full.Code.Holder,
			Searcher: full.Code.Searcher, Requesters: full.Code.Requesters, Chart: full.Code.Chart,
			Actor: builtin.PrincipalActor},
		// Who read a unit repository: an operator-only verb, so left
		// nil no walk would see it.
		Readers: builtin.ReadersDeps{Readers: &readersFake{}},
		// The health tools, over the seat registry's seams (requester
		// ceilings and code_status's code half included).
		Health: builtin.HealthDeps{Leases: full.Health.Leases, Requesters: full.Health.Requesters,
			Chart: full.Health.Chart, Status: full.Health.Status,
			Code: builtin.CodeDeps{Code: full.Code.Code, Requesters: full.Code.Requesters,
				Chart: full.Code.Chart, Actor: builtin.PrincipalActor}},
		// The chat with its read-state store, which only this surface is
		// given: left nil, no walk sees mark_chat_read.
		Chat: func() builtin.ChatDeps {
			talk := full.Chat
			talk.Cursors = fakeCursors{}
			talk.Actor = builtin.PrincipalChatActor
			return talk
		}(),
		Org: orgOf,
		// The operator-only verbs (a parked run's answer, a seat's pause
		// and resume, a note to a running turn, a knowledge answer), each
		// its own seam: left nil, no walk would see it.
		Runs: builtin.RunDeps{Desk: &deskFake{}, Actor: builtin.PrincipalActor},
		Pauses: builtin.SeatPauseDeps{Pauses: coordmem.NewFleet(), Announce: &announced{},
			Org: orgOf, Actor: builtin.PrincipalActor},
		Steer: builtin.SteerDeps{Asker: &fleetAsker{}, Wire: steerWire, Actor: builtin.PrincipalActor},
		Answer: builtin.AnswerDeps{Models: answers.model, Budget: answers.budget,
			Actor: builtin.PrincipalActor},
		ScheduleRuns: builtin.ScheduleRunDeps{Runner: stillRunner{}, Org: orgOf,
			Actor: builtin.PrincipalActor},
		Authorize: builtin.Decide(chartLeads),
	}
}
