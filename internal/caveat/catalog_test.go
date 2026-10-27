package caveat_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/caveat"
)

// samples is every constructor in the catalog, called with the kind of facts
// it is called with in production: a handle, a project key, a field's slug
// and name. The walk below fails for a constructor this table does not call,
// so a new caveat cannot ship without its person phrasing being read.
var samples = map[string]caveat.Caveat{
	"UnaddressedComment":  caveat.UnaddressedComment("backend-engineer"),
	"DefaultAssigneeGone": caveat.DefaultAssigneeGone("ENG", "bo"),
	"BodyLinksCapped":     caveat.BodyLinksCapped(31, 25),
	"FieldDateTruncated": caveat.FieldDateTruncated("launch", "Launch",
		"2026-03-05T04:00:00Z", "2026-03-04", "America/Los_Angeles"),
	"TargetDateTruncated": caveat.TargetDateTruncated("2026-12-18T20:00:00Z",
		"2026-12-19", "Asia/Tokyo"),
	"TagNearDuplicate":    caveat.TagNearDuplicate("ENG", "apis", []string{"api"}),
	"FiledEarlierUnread":  caveat.FiledEarlierUnread("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"),
	"FiledEarlierRemoved": caveat.FiledEarlierRemoved("0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b"),
	"FileFinding": caveat.FileFinding("index.html:12",
		"the page has no <meta name=\"viewport\">, so it renders at desktop width on a phone"),
}

// What a person's words may never carry, each with why: every one of them is
// a model's vocabulary, which a person sees no control for.
var modelVocabulary = []struct {
	pattern *regexp.Regexp
	what    string
}{
	// `default_assignee`, `if_match`, `comment_id`, and every builtin
	// tool's name: an argument or a tool is spelled in snake case, and
	// nothing a person reads on a screen is.
	{regexp.MustCompile(`\b[a-z0-9]+(?:_[a-z0-9]+)+\b`), "a snake_case argument or tool name"},
	// `ask: ana`, `labels: [...]`: an argument as a model writes it.
	{regexp.MustCompile(`\b[a-z]+:\s`), "an argument written as `name: value`"},
	// The mark a tool's sentence puts around a value it names.
	{regexp.MustCompile("`"), "a backticked argument"},
	// A row's internal id, which no screen shows.
	{regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`), "an internal id"},
	// Process vocabulary a model is told about and a person never is.
	{regexp.MustCompile(`(?i)\b(operation|node|writer|tool|argument|call)\b`), "engine-internal vocabulary"},
}

// EVERY CAVEAT A PERSON CAN BE SHOWN IS WORDED FOR A PERSON.
//
// A caveat is constructed only here (its fields are unexported), so this walk
// over the catalog is a walk over every caveat the act route can answer. Each
// constructor's person phrasing must name no tool, argument or internal id,
// and end as a sentence; its model phrasing must exist. The incident this
// holds: a comment answered in the dashboard left a toast telling a person to
// "Use ask: backend-engineer", an argument only a model can pass.
func TestEveryCaveatAPersonCanBeShownIsWordedForAPerson(t *testing.T) {
	t.Parallel()
	tools := slices.Concat(builtin.WorkTools(), builtin.PageTools(),
		builtin.ArtifactTools(), builtin.ArtifactDataTools())
	if len(tools) == 0 {
		t.Fatal("no builtin tool names to hold the person phrasings against")
	}
	for name, c := range samples {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !c.Code().Valid() {
				t.Fatalf("%s makes a caveat of unknown code %q", name, c.Code())
			}
			if c.Agent() == "" || c.Person() == "" {
				t.Fatalf("%s is missing a phrasing: agent %q, person %q", name,
					c.Agent(), c.Person())
			}
			for _, text := range []string{c.Agent(), c.Person()} {
				if strings.ContainsRune(text, '\u2014') {
					t.Errorf("%s carries an em dash: %q", name, text)
				}
			}
			if c.Code() == caveat.CodeFileFinding {
				// THE ONE CAVEAT WHOSE WORDS ARE THE SAME FOR BOTH: a
				// finding about a file is about its text, which a
				// person who wrote the file reads as a model does. Its
				// words are the linter's, held by its own suite.
				return
			}
			person := c.Person()
			if !strings.HasSuffix(person, ".") {
				t.Errorf("%s's person phrasing is not a sentence: %q", name, person)
			}
			for _, tool := range tools {
				if strings.Contains(person, tool) {
					t.Errorf("%s tells a person to use the tool %s: %q", name, tool, person)
				}
			}
			for _, banned := range modelVocabulary {
				if found := banned.pattern.FindString(person); found != "" {
					t.Errorf("%s tells a person %q, %s: %q", name, found, banned.what, person)
				}
			}
		})
	}
}

// THE WALK ABOVE COVERS THE WHOLE CATALOG: every exported constructor this
// package declares is in [samples], and every code has a constructor there.
// Read from the source, so a constructor added without a sample fails here
// rather than shipping an unread person phrasing.
func TestTheWalkCoversEveryConstructorAndCode(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var constructors []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !fn.Name.IsExported() || fn.Type.Results == nil {
				continue
			}
			results := fn.Type.Results.List
			if len(results) != 1 {
				continue
			}
			if id, ok := results[0].Type.(*ast.Ident); ok && id.Name == "Caveat" {
				constructors = append(constructors, fn.Name.Name)
			}
		}
	}
	if len(constructors) == 0 {
		t.Fatal("found no constructors: the walk is reading the wrong source")
	}
	for _, name := range constructors {
		if _, sampled := samples[name]; !sampled {
			t.Errorf("the constructor %s has no sample, so its person phrasing "+
				"is never read: add it to samples", name)
		}
	}
	covered := map[caveat.Code]bool{}
	for _, c := range samples {
		covered[c.Code()] = true
	}
	for _, code := range caveat.Codes() {
		if !covered[code] {
			t.Errorf("no constructor in samples makes a caveat of code %q", code)
		}
	}
}

// WHETHER A PERSON MUST ACT IS THE CODE'S, and every code states one: the
// dashboard keeps a toast up for `required` and lets one go for `none`, so a
// code without a stated action would be a renderer's guess.
func TestEveryCodeStatesWhatItAsksOfAPerson(t *testing.T) {
	t.Parallel()
	for _, code := range caveat.Codes() {
		if !code.Action().Valid() {
			t.Errorf("%s asks %q of a person", code, code.Action())
		}
	}
	if got := caveat.Code("a_newer_peers_code").Action(); got != caveat.ActionNone {
		t.Errorf("an unknown code asks %q, want none: a toast this build "+
			"cannot explain must not demand dismissal", got)
	}
	if caveat.DefaultAssigneeGone("ENG", "bo").Action() != caveat.ActionRequired {
		t.Error("a default assignee nobody holds is left broken until somebody " +
			"acts, and is not marked required")
	}
	if caveat.UnaddressedComment("bo").Action() != caveat.ActionNone {
		t.Error("a remark that asks nothing demands dismissal")
	}
}

// A CAVEAT CROSSES THE ESTATE ROUTER WHOLE: a write routed to a data node
// answers its caveats to the node the caller is on, so the wire form round
// trips both phrasings, and a peer's code this build does not know is kept
// rather than failing the answer.
func TestACaveatCrossesTheEstateRouterWhole(t *testing.T) {
	t.Parallel()
	for name, c := range samples {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var back caveat.Caveat
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if back != c {
			t.Errorf("%s came back as %+v, want %+v", name, back, c)
		}
	}
	var newer caveat.Caveat
	if err := json.Unmarshal([]byte(`{"code":"later","agent":"a","person":"p."}`), &newer); err != nil {
		t.Fatalf("a newer peer's caveat failed the answer: %v", err)
	}
	if newer.Code() != "later" || newer.Person() != "p." {
		t.Errorf("a newer peer's caveat read as %+v", newer)
	}
	var partial caveat.Caveat
	if err := json.Unmarshal([]byte(`{"code":"later","agent":"a"}`), &partial); err == nil {
		t.Error("a caveat with no person phrasing was accepted: a person's " +
			"surface would answer nothing for it")
	}
}

// A PERSON'S SURFACE READS EACH CAVEAT IN A PERSON'S WORDS, always as a list.
func TestAPersonsSurfaceReadsThePersonsWords(t *testing.T) {
	t.Parallel()
	if got := caveat.ForPeople(nil); got == nil || len(got) != 0 {
		t.Errorf("no caveats answered %#v, want an empty list", got)
	}
	gone := caveat.DefaultAssigneeGone("ENG", "bo")
	got := caveat.ForPeople([]caveat.Caveat{gone, {}})
	want := []caveat.Shown{{Code: caveat.CodeDefaultAssigneeGone, Message: gone.Person(),
		Action: caveat.ActionRequired}}
	if !slices.Equal(got, want) {
		t.Errorf("answered %+v, want %+v", got, want)
	}
	if agents := caveat.Agents([]caveat.Caveat{gone}); !slices.Equal(agents, []string{gone.Agent()}) {
		t.Errorf("a model was told %q, want the agent phrasing", agents)
	}
}
