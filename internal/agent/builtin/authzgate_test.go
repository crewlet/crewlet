package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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
// a tool whose dependency is nil by design — a company on Jira is shown no
// native tracker — so a nil here is a verb this walk silently stops covering.
func operatorDeps(t *testing.T) builtin.OperatorDeps {
	t.Helper()
	trk := newFakeTracker()
	person := &personSpy{}
	return builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Search: trk, Inbox: trk,
			Merges:        func(builtin.Actor) builtin.WorkMerger { return trk },
			PersonWriter:  func(builtin.Actor) builtin.PersonWriter { return person },
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
			// A CONSTRUCTOR THAT ANSWERS NIL still registers the tool,
			// because OperatorTools gates on whether the SEAM is wired
			// rather than on what it yields. That is what this walk
			// needs: it reads the catalogue's names and calls nothing.
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			Seats:           func() []colleague.Seat { return nil },
			DefaultProject:  func(string) string { return "ENG" },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}
}
