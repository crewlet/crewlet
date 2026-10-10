package operator_test

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/clientsource"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// companyTools are the catalogue tools whose change rewrites what EVERY seat's
// tracker means rather than moving one piece of work: a project's own
// settings (its fields, its default assignee, its tags) and the company's
// catalogue of types and fields. The dashboard offers one only from a screen
// that says so, which is what its row's `scope: "company"` declares.
//
// Declared HERE, beside the gate that holds the client to it, because it is a
// statement about the dashboard's offer rather than about the tools — the
// BLAST RADIUS of a press. Who may make one is the other column, `reach`,
// which IS the engine's ([operator.Server.ReachOf]): a project's settings are
// its lead's to change whatever its blast radius, so a `company` tool is not
// therefore an admin's.
var companyTools = []string{"write_project", "write_work_catalogue"}

// EVERY CHANGE THE DASHBOARD MAKES IS ONE THE ACT TRANSPORT SERVES, WITH THE
// ARGUMENTS ITS TOOL TAKES, AT THE REACH THE ENGINE SERVES IT.
//
// `contract/actions.ts` is the dashboard's whole write vocabulary: a screen
// calls `act(tool, args)` with a key of it and only the arguments its row
// names. That table is tool names and argument names written in TypeScript —
// a surface that starts lying on the first rename in Go, and lies in the worst
// way: a button that is refused `invalid` the first time a person presses it,
// on a screen whose promise is that pressing it makes the change. So the table
// is read from ITS OWN SOURCE and held against the REAL catalogue:
//
//   - every tool is served by `/operator/act` — in the catalogue and not a
//     proven read, which the transport refuses `read_only_tool`;
//   - every argument is a property of that tool's schema, and every property
//     the schema requires is one the row lets a screen send;
//   - every row declares its scope, and `company` is exactly the tools that
//     rewrite what every seat's tracker means;
//   - every row declares its reach, and it is the reach the dispatch serves
//     the tool at — the column a screen reads to tell a member "only an
//     admin can" rather than "this engine does not", so a copy that drifted
//     would tell a person the wrong one.
func TestEveryActionTheDashboardTakesIsOneTheActTransportServes(t *testing.T) {
	t.Parallel()
	s := fullSurface(t)
	acts := s.Acts(auth.ReachAdmin)

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
	for _, name := range companyTools {
		if !slices.Contains(acts, name) {
			t.Errorf("%s is declared a company tool and the act transport "+
				"serves no such write — the list names a tool that is gone", name)
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
		}
		for _, need := range required {
			if !slices.Contains(args, need) {
				t.Errorf("%s requires %q and its row does not let a screen send "+
					"it — every press would be refused invalid", tool, need)
			}
		}
		scope := clientsource.Field(row, "scope")
		want := "person"
		if slices.Contains(companyTools, tool) {
			want = "company"
		}
		if len(scope) != 1 || scope[0] != want {
			t.Errorf("%s declares scope %q, want %q", tool, scope, want)
		}
		engine, declared := s.ReachOf(tool)
		if !declared {
			t.Errorf("%s is served with no reach — the catalogue refuses such a tool", tool)
			continue
		}
		if reach := clientsource.Field(row, "reach"); len(reach) != 1 ||
			reach[0] != string(engine) {

			t.Errorf("%s declares reach %q and the engine serves it at %q — a "+
				"screen would tell a person the wrong thing about who can press it",
				tool, reach, engine)
		}
	}
}

// EVERY TOOL THE CATALOGUE CAN SERVE NAMES ITS REACH, AND NO DECLARATION NAMES
// A TOOL THAT IS GONE.
//
// The first half is what [operator.New] refuses on, and the full surface
// building at all is the evidence. The second half is the one nothing else
// would notice: a row left behind by a renamed tool declares a reach for a
// name nobody serves, which reads as a deliberate decision and is the place
// the renamed tool's own row would then be missing from.
func TestEveryDeclaredReachNamesAToolTheCatalogueServes(t *testing.T) {
	t.Parallel()
	served := fullSurface(t).Tools()
	declared := operator.DeclaredReach()
	if len(declared) < 30 {
		t.Fatalf("the reach table declares %d tools; a reader that stopped "+
			"reading it would certify nothing", len(declared))
	}
	for name, reach := range declared {
		if !slices.Contains(served, name) {
			t.Errorf("a reach (%s) is declared for %q, which the fullest catalogue "+
				"does not serve — it serves %v", reach, name, served)
		}
		if !reach.Valid() || !reach.Covers(auth.ReachMember) {
			t.Errorf("%q is declared at %q; both transports are mounted at member "+
				"reach, so a tool below it is a tool nobody reaches past the route",
				name, reach)
		}
	}
	for _, name := range served {
		if _, ok := declared[name]; !ok {
			t.Errorf("the catalogue serves %q and the reach table does not name it", name)
		}
	}
	// THE ONE ADMIN TOOL, named, so a table that drifted to all-member —
	// the default this table exists to refuse — fails here rather than
	// serving the company's vocabulary to every teammate's key.
	if declared[tracker.WriteWorkCatalogueTool] != auth.ReachAdmin {
		t.Errorf("write_work_catalogue is declared at %q: the company's own "+
			"vocabulary is held by no seat in the chart, so it is an admin's",
			declared[tracker.WriteWorkCatalogueTool])
	}
}

// A TOOL THAT NAMES NO REACH IS REFUSED, never served at some default — which
// is the whole difference between a declaration and a list somebody has to
// remember to extend (ADR-0031).
func TestACatalogueToolWithNoReachIsRefused(t *testing.T) {
	t.Parallel()
	err := operator.NewCatalogue([]tools.Callable{unclassifiedTool{}})
	if !errors.Is(err, operator.ErrNoReach) || !strings.Contains(err.Error(), "unclassified_tool") {
		t.Fatalf("a tool with no reach built a catalogue (%v), want ErrNoReach naming it", err)
	}
	if err := operator.NewCatalogue(nil); err != nil {
		t.Errorf("an empty catalogue was refused: %v", err)
	}
}

// unclassifiedTool is a tool the reach table has never heard of.
type unclassifiedTool struct{}

func (unclassifiedTool) Name() string               { return "unclassified_tool" }
func (unclassifiedTool) Description() string        { return "nothing" }
func (unclassifiedTool) Parameters() map[string]any { return map[string]any{"type": "object"} }
func (unclassifiedTool) Call(context.Context, map[string]any) (tools.Result, error) {
	return tools.Result{}, nil
}

// THE DASHBOARD KNOWS EXACTLY THE REFUSALS THE ACT TRANSPORT ANSWERS.
//
// `contract/errors.ts` `ACT_ERRORS` is the sentence the dashboard shows for
// each `error` an act can come back with. It is held BOTH WAYS against the two
// sets the engine answers from — the transport's own codes and the tool
// refusal classes — because each direction fails differently: a code only the
// engine sends is a refusal the screen renders as an unexplained failure, and
// one only the dashboard knows is a sentence nobody will read.
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

// fullSurface is the operator surface with EVERY seam non-nil, so its
// catalogue is the largest any company runs.
//
// THE REGISTRATION IS CONDITIONAL on which writers a deployment has, and a nil
// seam silently drops its tools — which would turn the gate above into one
// certifying a smaller catalogue than a real company serves, and a tool the
// dashboard acts through reported "not served". Nothing here is ever CALLED:
// the gate reads names, hints and schemas, so each seam is an interface value
// with no implementation behind it.
//
// THE PROJECT FILES INCLUDED, which this once left out — so the four file
// tools a node with an object store serves were in no catalogue any gate
// here read, and a reach table missing them would have passed.
func fullSurface(t *testing.T) *operator.Server {
	t.Helper()
	actor := func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
		return builtin.Actor{}, nil
	}
	s := newSurface(t, operator.Options{
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
			Files:           struct{ builtin.FileReader }{},
			FileWriter:      func(builtin.Actor) builtin.FileWriter { return nil },
			Objects:         struct{ builtin.ObjectStore }{},
			Actor:           operator.WorkActor(nil, nil),
		},
		Pages: builtin.PageDeps{
			Reader: struct{ builtin.PageReader }{},
			Writer: struct{ builtin.PageWriter }{},
			Actor: func(context.Context, *turnctx.Turn) (pages.Actor, error) {
				return pages.Actor{}, nil
			},
		},
		Knowledge: struct{ builtin.KnowledgeSearcher }{},
		Org:       func() *org.Organization { return nil },
		Runs:      builtin.RunDeps{Desk: struct{ builtin.RunDesk }{}, Actor: actor},
		Pauses:    builtin.SeatPauseDeps{Pauses: struct{ builtin.SeatPauseStore }{}, Actor: actor},
		Steer:     builtin.SteerDeps{Asker: struct{ builtin.FleetAsker }{}, Actor: actor},
		Answer: builtin.AnswerDeps{
			Models: struct{ builtin.AnswerModels }{},
			Budget: struct{ builtin.AnswerBudget }{},
			Actor:  actor,
		},
	})
	if s == nil {
		t.Fatal("a surface with every seam wired serves nothing")
	}
	return s
}

// fullReader is every read seam the work tools test their reader for, so one
// value switches on the project and feed tools as well as the core ones.
type fullReader interface {
	builtin.WorkReader
	builtin.ProjectReader
	builtin.FeedReader
}
