package operator_test

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/clientsource"
	crewletmcp "github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/tracker"
)

// companyTools are the catalogue tools whose change rewrites what EVERY seat's
// tracker means rather than moving one piece of work: a project's own
// settings (its fields, its default assignee, its tags) and the company's
// catalogue of types and fields. The dashboard offers one only from a screen
// that says so, which is what its row's `scope: "company"` declares.
//
// Declared HERE, beside the gate that holds the client to it, because it is a
// statement about the dashboard's offer rather than about the tools: both are
// on the operator surface for a person's assistant, and the tracker's own
// authority rules (the project lead, the operator) decide who may call them.
var companyTools = []string{"write_project", "write_work_catalogue"}

// EVERY CHANGE THE DASHBOARD MAKES IS ONE THE ACT TRANSPORT SERVES, WITH THE
// ARGUMENTS ITS TOOL TAKES.
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
//   - every row's `domain` is the log its tool's write lands on ([actionLog]),
//     or null for one that lands on none. A wrong one is silent and costs
//     read-your-writes: a post filed under `null` raises no chat floor, so the
//     room's next read may come from before the post.
func TestEveryActionTheDashboardTakesIsOneTheActTransportServes(t *testing.T) {
	t.Parallel()
	s := fullSurface(t)
	acts := s.Acts()

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
		// A `null` domain carries no string, so it reads as no value at all.
		domain := ""
		if named := clientsource.Field(row, "domain"); len(named) == 1 {
			domain = named[0]
		}
		if log := actionLog(tool); domain != log {
			t.Errorf("%s declares domain %s and its write lands on %s — the "+
				"floor a press raises is the wrong log's, so the next read can "+
				"come from before it", tool, orNull(domain), orNull(log))
		}
	}
}

// orNull spells a row's domain as the table writes it.
func orNull(domain string) string {
	if domain == "" {
		return "null"
	}
	return strconv.Quote(domain)
}

// attentionTools are the chat tools whose write is NOT a record on the chat
// log: a person's read cursors, mutes and do-not-disturb are a coordination
// record ([chat.MarkChatReadTool]), answered `applied` at no position, so
// there is no floor for a read to wait on.
var attentionTools = []string{chat.MarkChatReadTool}

// actionLog is the state log a tool's write lands on, by the family the tool
// is registered under — the work tools on the tracker's, the page tools on
// the knowledge base's, the chat tools on chat's — or "" for a write that
// lands on none: a pause, a steer, a run's answer, a person's attention.
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
	case in(tracker.Tools(), tracker.OperatorOnlyTools()):
		return "tracker"
	case in(builtin.PageTools()):
		return "pages"
	case in(chat.Tools(), chat.OperatorOnlyTools()):
		return "chat"
	}
	return ""
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
			Actor:           operator.WorkActor(nil, nil),
		},
		Pages: builtin.PageDeps{
			Reader: struct{ builtin.PageReader }{},
			Writer: struct{ builtin.PageWriter }{},
			Actor: func(context.Context, *turnctx.Turn) (pages.Actor, error) {
				return pages.Actor{}, nil
			},
		},
		Chat: builtin.ChatDeps{
			Reader:  struct{ builtin.ChatReader }{},
			Writer:  struct{ builtin.ChatWriter }{},
			Search:  struct{ builtin.ChatSearcher }{},
			Cursors: struct{ builtin.ChatCursors }{},
			Actor:   operator.ChatActor(nil, nil),
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
	// EVERY CHAT GESTURE A PERSON MAKES IS AMONG THEM, the read-state flush
	// included: the chat screen writes through nothing else (ADR-0024), so a
	// fixture that dropped them would refuse every chat row ACTIONS gains as
	// "serves no such tool" while a real company serves it.
	for _, name := range append(chat.Tools(), chat.OperatorOnlyTools()...) {
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
