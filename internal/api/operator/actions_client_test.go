package operator_test

import (
	"slices"
	"sort"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/iam"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/worklet"
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
	worklet.WriteProjectTool: authz.ActionProjectPolicy,
}

// scopeOf is the scope a row has to declare, read off the authority table: a
// write decided by the CONTAINER's lead or by admin rewrites what every seat's
// tracker means — a project's settings, the company's catalogue — and is
// `company`; every other is a person's change to the company's work in their
// own name, and is `person` — tidying one item included, whoever is admitted
// to it, its reporter or its project's lead ([authz.ClassTidy]).
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
	case authz.ClassContainer, authz.ClassAdmin:
		return "company"
	}
	return "person"
}

// appsOf is the apps a row has to declare, read off the authority table: the
// row's own app for a change one app opens, its set for one ANY OF SEVERAL
// apps opens ([authz.ClassAnyApp], [authz.AnyOf] — a question answered from
// whichever halves the caller holds), and none for a change no app holds.
// Read on the tool's OWN verb, the one [builtin.Admits] asks when the act
// transport offers it, and in the row's order, which is the order its refusal
// names them in.
func appsOf(t *testing.T, tool string) []string {
	t.Helper()
	action := authz.Action(tool)
	class, ok := authz.ClassOf(action)
	if !ok {
		t.Fatalf("%s has no row in the authority table", tool)
	}
	if class == authz.ClassAnyApp {
		set, _ := authz.AnyOf(action)
		out := make([]string, 0, len(set))
		for _, app := range set {
			out = append(out, string(app))
		}
		return out
	}
	app, _ := authz.AppOf(action)
	if app == "" {
		return []string{}
	}
	return []string{string(app)}
}

// attentionTools are the chat tools whose write is NOT a record on the chat
// log: a person's read cursors, mutes and do-not-disturb are a coordination
// record ([chatlet.MarkChatReadTool]), answered `applied` at no position, so
// there is no floor for a read to wait on.
var attentionTools = []string{chatlet.MarkChatReadTool}

// actionLog is the state log a tool's write lands on, by the family the tool
// is registered under — the work tools on the tracker's, the page tools on
// the knowledge base's, the chat tools on chat's, the code tools (a code
// inbox mark among them) on the code host's — or "" for a write that
// lands on none: a pause, a steer, a run's answer, a question answered, a
// person's attention.
//
// BY FAMILY RATHER THAN A TABLE OF TOOLS, so a tool added to a family is held
// to that family's log without anybody writing its name here; the one carve-
// out is declared above with its reason.
func actionLog(tool string) string {
	in := func(family ...[]string) bool {
		for _, names := range family {
			if slices.Contains(names, tool) {
				return true
			}
		}
		return false
	}
	switch {
	case slices.Contains(attentionTools, tool):
		return ""
	case in(worklet.Tools(), worklet.OperatorOnlyTools()):
		return "tracker"
	case in(builtin.PageTools()):
		return "pages"
	case in(chatlet.Tools(), chatlet.OperatorOnlyTools()):
		return "chat"
	case in(builtin.CodeToolNames(), builtin.CodeWrites(), []string{builtin.CodeMarkInboxTool}):
		return "code"
	}
	return ""
}

// orNull spells a row's domain as the table writes it.
func orNull(domain string) string {
	if domain == "" {
		return "null"
	}
	return strconv.Quote(domain)
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
//   - every row declares the scope its write's authority class gives it;
//   - every row's `apps` are the apps its authority row opens it to
//     ([appsOf]). A wrong set is a control held with the wrong remedy — or
//     with none: a reader holding no app was told the engine does not make
//     `answer_knowledge` for them while the table said either app would;
//   - every row's `domain` is the log its tool's write lands on ([actionLog]),
//     or null for one that lands on none. A wrong one is silent and costs
//     read-your-writes: a write filed under `null` raises no floor, so the
//     screen's next read may come from before the press.
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
		apps, err := clientsource.Property(row, "apps")
		if err != nil {
			t.Errorf("%s: %v", tool, err)
			continue
		}
		if got, want := clientsource.Strings(apps), appsOf(t, tool); !slices.Equal(got, want) {
			t.Errorf("%s declares apps %q, and its authority row opens it to %q",
				tool, got, want)
		}
		// A `null` domain carries no string, so it reads as no value at all.
		domain := ""
		if named := clientsource.Field(row, "domain"); len(named) == 1 {
			domain = named[0]
		}
		if log := actionLog(tool); domain != log {
			t.Errorf("%s declares domain %s and its write lands on %s — the "+
				"screen's read after the press would wait on the wrong log, or "+
				"on none, and could come from before it", tool, orNull(domain), orNull(log))
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
	code := &writingHost{}
	s := newSurface(t, operator.Options{
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
			Chat: builtin.ChatDeps{
				Reader:  struct{ builtin.ChatReader }{},
				Writer:  struct{ builtin.ChatWriter }{},
				Search:  struct{ builtin.ChatSearcher }{},
				Cursors: struct{ builtin.ChatCursors }{},
			},
			Knowledge: struct{ builtin.KnowledgeSearcher }{},
			// THE CODE HOST WITH ITS WRITES, so the change page's rows —
			// reviews, comments, asks, closes, applied suggestions — and the
			// Inbox's `code_mark_inbox`, which the catalogue serves only beside
			// the code host's writes, are held to the catalogue a company
			// hosting code serves.
			Code: builtin.CodeDeps{Code: code, Requesters: noRequesters{}, Chart: authz.NoChart{},
				Writer: code, Holder: code},
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
	// EVERY CHAT GESTURE A PERSON MAKES IS AMONG THEM, the read-state flush
	// included: the chat screen writes through nothing else (ADR-0024), so a
	// fixture that dropped them would refuse every chat row ACTIONS carries as
	// "serves no such tool" while a real company serves it.
	for _, name := range append(chatlet.Tools(), chatlet.OperatorOnlyTools()...) {
		if !slices.Contains(s.Tools(), name) {
			t.Fatalf("the full surface serves %v, without the chat tool %s",
				s.Tools(), name)
		}
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
