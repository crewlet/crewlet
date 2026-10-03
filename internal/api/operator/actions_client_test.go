package operator_test

import (
	"slices"
	"sort"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/tracker"
)

// facetActions names the rows whose write is decided by a verb OTHER than its
// tool's own: the tool admits on one rule and then asks another, from inside,
// for the arguments the row sends. `write_project` admits on the colleague
// write (adding a label is anybody's) and asks [authz.ActionProjectPolicy] —
// the project lead's — for every policy field, and the only field the
// dashboard's row sends beside the project is one of those, its target date.
//
// Declared HERE, beside the gate that holds the client to it, because it is a
// statement about what the dashboard's row SENDS rather than about the tool.
var facetActions = map[string]authz.Action{
	tracker.WriteProjectTool: authz.ActionProjectPolicy,
}

// scopeOf is the scope a row has to declare, read off the authority table: a
// write decided by the CONTAINER's lead or by a deployment GRANT rewrites what
// every seat's tracker means — a project's settings, the company's catalogue —
// and is `company`; every other is a person's change to the company's work in
// their own name, and is `person`.
func scopeOf(t *testing.T, tool string) string {
	t.Helper()
	action, ok := facetActions[tool]
	if !ok {
		action = authz.Action(tool)
	}
	class, ok := authz.ClassOf(action)
	if !ok {
		t.Fatalf("%s is decided by %s, which the authority table has no row for",
			tool, action)
	}
	switch class {
	case authz.ClassContainer, authz.ClassOperator:
		return "company"
	}
	return "person"
}

// EVERY CHANGE THE DASHBOARD MAKES IS ONE THE ACT TRANSPORT SERVES, WITH THE
// ARGUMENTS ITS TOOL TAKES.
//
// `contract/actions.ts` is the dashboard's whole write vocabulary: a screen
// calls `act(tool, args)` with a key of it and only the arguments its row
// names. That table is tool names and argument names written in TypeScript —
// a surface that starts lying on the first rename in Go, and lies in the worst
// way: a button that is refused the first time a person presses it, on a
// screen whose promise is that pressing it makes the change. So the table is
// read from ITS OWN SOURCE and held against the REAL catalogue:
//
//   - every tool is served by `/operator/act` — in the catalogue and not a
//     proven read, which the transport refuses `read_only_tool`;
//   - every argument is a property of that tool's schema, and every property
//     the schema requires is one the row lets a screen send — an argument the
//     tool does not declare is refused `invalid_body` by the tools' own gate,
//     which is what a row still naming `save_work_view`'s retired `owner`, or
//     a `viewer` no tool takes, would meet on every press;
//   - every row declares the scope its write's authority class gives it.
func TestEveryActionTheDashboardTakesIsOneTheActTransportServes(t *testing.T) {
	t.Parallel()
	s := fullSurface(t)
	acts, err := s.Acts(t.Context(), administrator)
	if err != nil {
		t.Fatalf("Acts: %v", err)
	}

	body, err := clientsource.Literal(clientsource.Tree(t), "ACTIONS")
	if err != nil {
		t.Fatal(err)
	}
	tools, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	// A FLOOR, because a table read as empty passes every check below.
	if len(tools) < 3 {
		t.Fatalf("ACTIONS names %d tools; a reader that stopped reading it "+
			"would certify nothing", len(tools))
	}
	for tool := range facetActions {
		if !slices.Contains(acts, tool) {
			t.Errorf("%s is declared a facet row and the act transport serves "+
				"no such write — the list names a tool that is gone", tool)
		}
	}

	for _, tool := range tools {
		schema, served := s.Parameters(tool)
		switch {
		case !served:
			t.Errorf("the dashboard acts through %q and the operator catalogue "+
				"serves no such tool — it serves %v", tool, s.Tools())
			continue
		case crewletmcp.ReadOnlyProven(s.Annotations(tool)):
			t.Errorf("the dashboard acts through %q, which is a READ: the act "+
				"transport refuses it read_only_tool on every press", tool)
			continue
		case !slices.Contains(acts, tool):
			t.Errorf("%q is in the catalogue and not among what the act "+
				"transport serves (%v)", tool, acts)
			continue
		}
		row, err := clientsource.Property(body, tool)
		if err != nil {
			t.Errorf("%s: %v", tool, err)
			continue
		}
		list, err := clientsource.Property(row, "args")
		if err != nil {
			t.Errorf("%s: %v", tool, err)
			continue
		}
		args := clientsource.Strings(list)
		if len(args) == 0 {
			t.Errorf("%s lets a screen send no argument at all", tool)
		}
		properties, required := schemaOf(schema)
		for _, arg := range args {
			if !slices.Contains(properties, arg) {
				t.Errorf("the dashboard sends %s(%s) and that tool takes %v — a "+
					"renamed argument leaves exactly this behind", tool, arg, properties)
			}
			if arg == operator.OperationArg {
				t.Errorf("the dashboard sends %s(%s), which the act transport "+
					"refuses: the operation is the request's key", tool, arg)
			}
		}
		for _, need := range required {
			if !slices.Contains(args, need) {
				t.Errorf("%s requires %q and its row does not let a screen send "+
					"it — every press would be refused", tool, need)
			}
		}
		scope := clientsource.Field(row, "scope")
		if want := scopeOf(t, tool); len(scope) != 1 || scope[0] != want {
			t.Errorf("%s declares scope %q, and its authority class makes it %q",
				tool, scope, want)
		}
	}
}

// THE DASHBOARD KNOWS EXACTLY THE REFUSALS THE ACT TRANSPORT ANSWERS.
//
// `contract/errors.ts` `ACT_ERRORS` is the sentence the dashboard shows for
// each `error` an act can come back with. It is held BOTH WAYS against the two
// sets the engine answers from — the transport's own codes and the tool
// refusal classes, whose spelling IS the code — because each direction fails
// differently: a code only the engine sends is a refusal the screen renders
// as an unexplained failure, and one only the dashboard knows is a sentence
// nobody will read.
func TestTheDashboardKnowsExactlyTheActRefusals(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree(t), "ACT_ERRORS")
	if err != nil {
		t.Fatal(err)
	}
	client, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	engine := make([]string, 0, len(operator.ActTransportCodes)+len(crewletmcp.Refusals))
	for _, code := range operator.ActTransportCodes {
		engine = append(engine, string(code))
	}
	// A REFUSAL CLASS IS ANSWERED UNDER ITS OWN SPELLING (held by
	// TestEveryRefusalCodeMapsToOneStatus), so the class is the code.
	for _, class := range crewletmcp.Refusals {
		engine = append(engine, string(class))
	}
	if len(engine) < 20 || len(client) < 20 {
		t.Fatalf("the engine answers %d codes and the dashboard names %d; a "+
			"reader that stopped reading either certifies nothing",
			len(engine), len(client))
	}
	for _, code := range engine {
		if !slices.Contains(client, code) {
			t.Errorf("the act transport answers %q and ACT_ERRORS has no "+
				"sentence for it", code)
		}
	}
	for _, code := range client {
		if !slices.Contains(engine, code) {
			t.Errorf("ACT_ERRORS names %q, which the act transport never answers", code)
		}
	}
}

// schemaOf is a JSON Schema's top-level property names and its required list,
// sorted. The catalogue writes `required` as either []any or []string.
func schemaOf(schema map[string]any) (properties, required []string) {
	if props, ok := schema["properties"].(map[string]any); ok {
		for name := range props {
			properties = append(properties, name)
		}
	}
	switch list := schema["required"].(type) {
	case []string:
		required = append(required, list...)
	case []any:
		for _, name := range list {
			if s, ok := name.(string); ok {
				required = append(required, s)
			}
		}
	}
	sort.Strings(properties)
	sort.Strings(required)
	return properties, required
}

// administrator is a person bound to a seat holding every grant, so what the
// act transport offers them is everything it serves.
var administrator = person("ada.admin", "jane-founder", iam.AllGrants...)

// fullSurface is the operator surface with EVERY seam non-nil, so its
// catalogue is the largest any company runs.
//
// THE REGISTRATION IS CONDITIONAL on which writers a deployment has, and a nil
// seam silently drops its tools — which would turn the gate above into one
// certifying a smaller catalogue than a real company serves, and a tool the
// dashboard acts through reported "not served". Nothing here is ever CALLED:
// the gate reads names, hints and schemas, so each seam is an interface value
// with no implementation behind it.
func fullSurface(t *testing.T) *operator.Server {
	t.Helper()
	return newSurface(t, operator.Options{
		Halves: fixed(operator.Halves{
			Work: builtin.WorkDeps{
				Reader:          struct{ fullReader }{},
				Writer:          func(builtin.Actor) builtin.WorkWriter { return nil },
				Dependencies:    func(builtin.Actor) builtin.WorkDepender { return nil },
				Merges:          func(builtin.Actor) builtin.WorkMerger { return nil },
				Moves:           func(builtin.Actor) builtin.WorkMover { return nil },
				Search:          struct{ builtin.WorkSearcher }{},
				ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
				CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
				PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return nil },
				Inbox:           struct{ builtin.InboxReader }{},
				ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return nil },
				TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
				Placer:          func(builtin.Actor) builtin.WorkPlacer { return nil },
			},
			Pages: builtin.PageDeps{
				Reader: struct{ builtin.PageReader }{},
				Writer: struct{ builtin.PageWriter }{},
			},
			Knowledge: struct{ builtin.KnowledgeSearcher }{},
		}),
		Org:    func() *org.Organization { return nil },
		Runs:   builtin.RunDeps{Desk: struct{ builtin.RunDesk }{}},
		Pauses: builtin.SeatPauseDeps{Pauses: struct{ builtin.SeatPauseStore }{}},
		Steer:  builtin.SteerDeps{Asker: struct{ builtin.FleetAsker }{}},
		Answer: builtin.AnswerDeps{
			Models: struct{ builtin.AnswerModels }{},
			Budget: struct{ builtin.AnswerBudget }{},
		},
	})
}

// fullReader is every read seam the work tools test their reader for, so one
// value switches on the project and feed tools as well as the core ones.
type fullReader interface {
	builtin.WorkReader
	builtin.ProjectReader
	builtin.FeedReader
}
