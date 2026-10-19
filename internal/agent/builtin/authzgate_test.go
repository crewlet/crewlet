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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY TOOL THIS BUILD REGISTERS HAS AN AUTHORITY RULE.
//
// internal/authz's own walks can only hold its table against ITSELF — every
// verb resolves to a class, every class is reached by a verb, every grant is
// asked for by something. None of them can see the surface the table is about,
// so a tool added without a row is a tool that ships ungated and ships looking
// correct, which is exactly what [authz.Decide]'s unknown-action refusal is
// the last line against rather than the first.
//
// THE WALK LIVES HERE because this is the package that owns the tools:
// internal/authz cannot import it without the cycle, and a gate written
// against a list of names somebody maintains by hand is the drift it was
// supposed to catch.
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

// AND EVERY TOOL-SHAPED VERB IN THE TABLE IS A TOOL THIS BUILD REGISTERS.
//
// THE DIRECTION THAT DECAYS QUIETLY, and it has already decayed once: the
// goal verbs left the tracker and `list_work_goals` and `write_work_goal` sat
// in the table for as long as it took somebody to read them, two rows deciding
// nothing. Nothing inside internal/authz could see it — the table was
// perfectly consistent with itself.
//
// TOLD APART BY THE DOT. A verb a tool serves is named after that tool, so it
// carries no separator; one with no tool behind it — an HTTP-only surface, a
// store call the route layer makes — is spelled `pages.trash`, `config.read`.
// That convention is what makes this walk possible at all, which is why
// internal/authz's own doc states it as load-bearing.
func TestEveryToolShapedActionIsARegisteredTool(t *testing.T) {
	t.Parallel()
	names := everyToolName(t)
	for _, action := range authz.Actions() {
		if strings.Contains(string(action), ".") {
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

// EVERY APP OPENS A TOOL A PERSON REACHES.
//
// An app is a permission somebody is given so they can USE something, and a
// person uses Worklet and Doclet through the operator
// catalogue — the dashboard's act route, their assistant, the HTTP write
// surface all dispatch into it. An app no operator tool belongs to is a
// permission nobody could be told what it opens: ticked on somebody's row, it
// would change nothing they can do. internal/authz's own walk holds that every
// app is named by SOME row; this one holds it against the tools a person is
// actually served, which that package cannot import.
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

// NO SEAT-ONLY VERB IS OFFERED TO A PERSON.
//
// `a2a_ask` and `run_sandbox` spend an agent's turn — a colleague seat's, or
// a coding run the seat's own loop suspends on — so the table refuses them to
// anybody but a seat before any class is read. A person's catalogue that still
// listed one would offer a tool every call to which is refused: the shape a
// model learns to distrust a whole catalogue from, and an act route a screen
// would draw a button for. And the table must say so for each: a seat-only row
// whose tool a seat's registry does not register would be a rule nobody
// reaches.
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

// NO VERB MADE AS A SEAT IS OFFERED TO A CREDENTIAL ACTING AS ITSELF.
//
// A person's surface offers what [builtin.Admits] says the table could admit
// before any object is named (the dashboard's `viewer.acts`), so a seated row
// is the difference between a screen that draws no control for an unbound
// credential and one whose every press of that control is refused. Held for
// every seated verb the operator catalogue serves, against a credential
// holding admin — what the table admits on — and beside a person bound to a
// seat holding the same, who must still be offered it, or a surface refusing
// everybody would pass. Mutation: drop `seated` from answer_knowledge's row
// and this goes red, which is the bug it was written for: viewer.acts offered
// the answer to a Tier A token's session and every press was refused.
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

// EVERY APP ROW BELONGS TO ITS KIND'S APP.
//
// A row names the app a verb belongs to, and nothing in the table pairs a verb
// with the KIND of thing it acts on — so `list_pages` could carry the tracker
// and every check inside internal/authz would still pass, while a person given
// the knowledge base alone was refused the pages and one given the tracker
// alone read them. Where the tool comes from is the one place the kind is
// stated: the tracker's own catalogue and the knowledge base's. Every one of
// their tools whose row names an app names that catalogue's app (one that
// names none — a seat's own, everybody's — is not an app row).
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

// everyToolName is every verb this build serves as a tool, over BOTH
// surfaces.
//
// THE UNION, because the authority table's whole premise is that a verb asked
// two ways is one verb: a seat's own registry and the operator's assistant
// each serve a set the other does not, and either one alone makes the walks
// above wrong in both directions. Measured with the seat's alone: nine rows
// the operator surface serves — set_priorities, work_inbox, remove_work_item
// and the rest — read as rules deciding nothing, and the tools they govern
// read as ungated.
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

// operatorDeps wires EVERY operator tool on, because a walk over a catalogue
// that omitted half of it would certify the half it saw. OperatorTools drops
// a tool whose dependency is nil by design (a company on Jira is shown no
// Worklet), so a nil here is a verb this walk silently stops covering.
func operatorDeps(t *testing.T) builtin.OperatorDeps {
	t.Helper()
	trk := newFakeTracker()
	person := &personSpy{}
	full := fullDeps(t)
	company := &org.Organization{Name: "Acme"}
	orgOf := func() *org.Organization { return company }
	answers := newAnswerRig()
	return builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Search: trk, Inbox: trk,
			Merges:        func(builtin.Actor) builtin.WorkMerger { return trk },
			Moves:         trk.moves,
			PersonWriter:  func(builtin.Actor) builtin.PersonWriter { return person },
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
			Placer:        func(builtin.Actor) builtin.WorkPlacer { return nil },
			// A CONSTRUCTOR THAT ANSWERS NIL still registers the tool,
			// because OperatorTools gates on whether the SEAM is wired
			// rather than on what it yields. That is what this walk
			// needs: it reads the catalogue's names and calls nothing.
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			// THE PROJECT FILES TOO, each over the seam the seat's own
			// registry is built with: a catalogue that left them off would
			// be one whose four file verbs no walk here ever reached.
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
		// THE CHAT, its read-state store included, which only this surface
		// is given — left nil, mark_chat_read is a verb no walk sees.
		Chat: func() builtin.ChatDeps {
			talk := full.Chat
			talk.Cursors = fakeCursors{}
			talk.Actor = builtin.PrincipalChatActor
			return talk
		}(),
		Org: orgOf,
		// The operator-only verbs: a parked run's answer, a seat's pause
		// and resume, a note to a running turn and an answer from the
		// company's knowledge. Each is a seam of its own, and each left
		// nil is a verb no seat holds and this walk would never see.
		Runs: builtin.RunDeps{Desk: &deskFake{}, Actor: builtin.PrincipalActor},
		Pauses: builtin.SeatPauseDeps{Pauses: coordmem.NewFleet(), Announce: &announced{},
			Org: orgOf, Actor: builtin.PrincipalActor},
		Steer: builtin.SteerDeps{Asker: &fleetAsker{}, Wire: steerWire, Actor: builtin.PrincipalActor},
		Answer: builtin.AnswerDeps{Models: answers.model, Budget: answers.budget,
			Actor: builtin.PrincipalActor},
		Authorize: builtin.Decide(chartLeads),
	}
}
