package builtin_test

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tracker"
)

// dashboardTree is the client source this gate reads. Relative, because a test
// runs in its own package's directory and the repository root is not a thing a
// Go test is handed.
const dashboardTree = "../../../dashboard/src"

// EVERY CALL THE SCREEN OFFERS IS ONE AN OPERATOR ACTUALLY HAS.
//
// The dashboard is READ-ONLY, so its answer to "how do I change this" is to
// show the call an operator's own assistant would make, pre-filled with the
// object's ids. That block is a table of tool names and argument names written
// in TypeScript — a documentation surface that starts lying on the first
// rename in Go, and lies in the worst way: a copied call that the surface
// refuses, on a screen whose whole promise is that this is what to send.
//
// So the table is read from ITS OWN SOURCE and checked against the REAL
// catalogue, the `rooms` idiom this repository already uses for cross-language
// claims: every tool it names exists, every argument it fills is one that tool
// takes, and none of them is a READ — `my_work` and `remove_work_item` are
// both operator tools and only one is an answer to the question the block asks.
func TestEveryToolCallTheScreenOffersIsOneAnOperatorHas(t *testing.T) {
	t.Parallel()
	served := operatorCatalogue()
	offered := offeredCalls(t)
	if len(offered) == 0 {
		t.Fatal("no calls were found at all, so this gate certifies nothing")
	}

	names := make([]string, 0, len(served))
	for name := range served {
		names = append(names, name)
	}
	sort.Strings(names)

	for tool, args := range offered {
		schema, has := served[tool]
		if !has {
			t.Errorf("the screen offers %q and the operator surface serves no "+
				"such tool — it serves %v", tool, names)
			continue
		}
		for _, arg := range args {
			if !slices.Contains(schema.fields, arg) {
				sort.Strings(schema.fields)
				t.Errorf("the screen fills %s(%s) and that tool takes %v — a "+
					"renamed argument leaves exactly this behind",
					tool, arg, schema.fields)
			}
		}
		if schema.readOnly {
			t.Errorf("the screen offers %q as a way to change something and it "+
				"is a READ — a block that opened on one would answer a "+
				"different question entirely", tool)
		}
	}
}

// servedTool is one operator tool as this gate compares against it.
type servedTool struct {
	fields   []string
	readOnly bool
}

// operatorCatalogue is the real operator surface, built with stub deps.
//
// THE REGISTRATION IS CONDITIONAL on which writers a deployment has, so every
// seam is non-nil here: a nil one silently drops its tools and turns this into
// a gate certifying a smaller catalogue than any real company runs.
func operatorCatalogue() map[string]servedTool {
	work := newFakeTracker()
	kb := &fakeKB{}
	out := map[string]servedTool{}
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:          work,
			Writer:          work.as,
			Merges:          func(builtin.Actor) builtin.WorkMerger { return nil },
			Search:          work,
			ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return &projectSpy{} },
			PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return &personSpy{} },
			SprintWriter:    func(builtin.Actor) builtin.SprintWriter { return &sprintSpy{} },
			ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return nil },
			GoalWriter:      func(builtin.Actor) builtin.GoalWriter { return nil },
			CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return nil },
			TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return nil },
			Inbox:           work,
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
		Pages: builtin.PageDeps{Reader: kb, Writer: kb},
	}) {
		out[tool.Name()] = servedTool{
			fields:   schemaFields(tool.Parameters()),
			readOnly: builtin.AnnotationsFor(tool.Name()).ReadOnly == mcp.Yes,
		}
	}
	return out
}

// schemaFields is the argument names a JSON Schema declares.
func schemaFields(schema map[string]any) []string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(props))
	for name := range props {
		out = append(out, name)
	}
	return out
}

// offeredCalls reads the screen's own table: tool name → the arguments it
// fills. Read from the SOURCE rather than restated here, so this gate cannot
// drift towards claiming the pair agree.
//
// BRACE-MATCHED rather than regex-delimited. An `args` object is written on
// one line when it is short and several when it is not, and a pattern ending
// at the first `\n },` runs straight past the short ones into the next entry —
// which reports `tool`, `label` and `destructive` as arguments the tool does
// not take. Counting braces is what actually answers "where does this object
// end".
func offeredCalls(t *testing.T) map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dashboardTree, "lib", "toolcall.ts"))
	if err != nil {
		t.Fatalf("the screen's call table could not be read, so this gate "+
			"certifies nothing: %v", err)
	}
	source := string(raw)
	named := regexp.MustCompile(`(?m)(?:^|[{,])\s*([a-z_]+):`)
	// A SHORTHAND PROPERTY carries no colon — `{ id, update: "…" }` — and
	// reading only the first form would quietly stop checking the one
	// argument that names the object.
	shorthand := regexp.MustCompile(`(?m)(?:^|[{,])\s*([a-z_]+)\s*(?:,|$)`)

	out := map[string][]string{}
	tool := regexp.MustCompile(`tool: "([a-z_]+)"`)
	for _, at := range tool.FindAllStringSubmatchIndex(source, -1) {
		name := source[at[2]:at[3]]
		// THE ARGS OF THIS CALL, which is the next `args: {` after its
		// name and not the one after that: a call with no args object is
		// a shape this table does not have, so the absence is a parse
		// failure rather than something to skip past.
		rest := source[at[1]:]
		open := strings.Index(rest, "args: {")
		if open < 0 {
			t.Fatalf("the entry for %s carries no args object, so this gate "+
				"cannot say what it fills", name)
		}
		body, ok := braced(rest[open+len("args: {")-1:])
		if !ok {
			t.Fatalf("the args object for %s is not closed, so this gate "+
				"cannot say what it fills", name)
		}
		top := topLevel(body)
		var fields []string
		for _, m := range named.FindAllStringSubmatch(top, -1) {
			fields = append(fields, m[1])
		}
		for _, m := range shorthand.FindAllStringSubmatch(top, -1) {
			fields = append(fields, m[1])
		}
		out[name] = append(out[name], fields...)
	}
	return out
}

// topLevel blanks out everything nested inside an args object, so only the
// object's OWN keys are read.
//
// `write_project` fills `sprints: {length_days: 14}`, and without this the
// gate reports `length_days` as an argument `write_project` does not take —
// which it does not, because it is an argument `sprints` takes. The limit is
// stated rather than hidden: this gate checks the TOP-LEVEL arguments of each
// call against the tool's top-level properties, and a wrong name nested inside
// one is not something it can see. Checking those would mean walking each
// schema's sub-objects, which is a parser rather than a gate.
func topLevel(body string) string {
	// THE OUTER BRACES ARE ALREADY OFF — [braced] hands back what is inside
	// them — so the args object's own keys are at depth ZERO and anything
	// nested is at one or more.
	out := []rune(body)
	depth := 0
	for i, r := range out {
		switch r {
		case '{', '[':
			depth++
			out[i] = ' '
		case '}', ']':
			depth--
			out[i] = ' '
		default:
			if depth > 0 {
				out[i] = ' '
			}
		}
	}
	return string(out)
}

// braced returns what is inside the object `in` opens with, counting nested
// braces. The table's values carry no braces inside strings, which is what
// makes counting sufficient — a value that did would need a parser, and this
// gate would rather fail loudly than read half an object.
func braced(in string) (string, bool) {
	depth := 0
	for i, r := range in {
		switch r {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return in[1:i], true
			}
		}
	}
	return "", false
}

// AND THE BLOCK SAYS WHO WOULD BE ATTRIBUTED, because the reason the dashboard
// does not write at all is that every change in this company carries a name.
func TestTheToolCallBlockNamesWhoWouldBeAttributed(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(dashboardTree, "components", "ToolCall.tsx"))
	if err != nil {
		t.Fatalf("the block could not be read: %v", err)
	}
	source := string(raw)
	if !strings.Contains(source, "operator") {
		t.Error("the block does not say the call would be attributed to an " +
			"operator — a reader who copies it has no way to know whose name " +
			"lands on the change")
	}
	// AND IT IS NOT A FORM. The one property that makes this a read-only
	// product's answer rather than an edit button is that nothing here sends
	// anything, so the shapes that would are named.
	for _, forbidden := range []string{"fetch(", "<form", "onSubmit", "query("} {
		if strings.Contains(source, forbidden) {
			t.Errorf("the block carries %q — it is meant to SHOW a call, never "+
				"to make one", forbidden)
		}
	}
}
