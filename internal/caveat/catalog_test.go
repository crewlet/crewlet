package caveat_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
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

// refusalSamples is every constructor in the refusal catalog, called with the
// kind of facts it is called with in production. The coverage walk below
// fails for a constructor this table does not call.
var refusalSamples = map[string]caveat.Refusal{
	"TitleMissing":         caveat.TitleMissing(),
	"TitleTooLong":         caveat.TitleTooLong(),
	"DescriptionTooLong":   caveat.DescriptionTooLong(),
	"CommentTooLong":       caveat.CommentTooLong(),
	"NotAQuestion":         caveat.NotAQuestion(),
	"QuestionRemoved":      caveat.QuestionRemoved(),
	"AskedSomebodyElse":    caveat.AskedSomebodyElse("backend-engineer"),
	"NoQuestionForChoice":  caveat.NoQuestionForChoice(),
	"QuestionHasNoOptions": caveat.QuestionHasNoOptions(),
	"NotAnOption":          caveat.NotAnOption(),
	"ArtifactBreaksPolicy": caveat.ArtifactBreaksPolicy(),
	"ArtifactQuotaReached": caveat.ArtifactQuotaReached(),
	"ArtifactTrashed":      caveat.ArtifactTrashed(),
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
			for _, problem := range caveattest.Problems(c.Person()) {
				t.Errorf("%s's person phrasing is not worded for a person: %s: %q",
					name, problem, c.Person())
			}
		})
	}
}

// EVERY REFUSAL A PERSON CAN BE SHOWN IS WORDED FOR A PERSON (ADR-0060).
//
// A refusal's person phrasing is constructed only here, so this walk over the
// catalog is a walk over every refusal sentence the act route can answer as
// `detail`: each must name no tool, argument or internal id, and end as a
// sentence. The class it holds: a decision answered in the dashboard Inbox
// was refused with "change the argument this names (`answers`, or `choice`)".
func TestEveryRefusalAPersonCanBeShownIsWordedForAPerson(t *testing.T) {
	t.Parallel()
	for name, r := range refusalSamples {
		if r.IsZero() {
			t.Errorf("%s makes no phrasing at all", name)
			continue
		}
		for _, problem := range caveattest.Problems(r.Person()) {
			t.Errorf("%s is not worded for a person: %s: %q", name, problem, r.Person())
		}
	}
}

// THE CHECK THE WALKS SHARE FAILS ON A MODEL'S REFUSAL: the sentence a tool
// answered a person's decision with before ADR-0060, verbatim, is refused for
// every mark of a model's vocabulary it carries. A check that passed it would
// pass every walk above while proving nothing.
func TestTheCheckRefusesAModelsRefusal(t *testing.T) {
	t.Parallel()
	model := "comment_on_work_item was refused: the choice \"ship\" is not an " +
		"option of the decision in 0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b on task " +
		"ENG-7. Nothing was posted, change the argument this names (`answers`, " +
		"or `choice`) and comment again."
	problems := caveattest.Problems(model)
	for _, want := range []string{"comment_on_work_item", "`", "internal id",
		"engine-internal vocabulary"} {
		if !slices.ContainsFunc(problems, func(p string) bool {
			return strings.Contains(p, want)
		}) {
			t.Errorf("the check missed %q in a model's refusal; it said %q", want, problems)
		}
	}
	if got := caveattest.Problems(caveat.NotAnOption().Person()); len(got) != 0 {
		t.Errorf("the person's phrasing of the same refusal was refused: %q", got)
	}
}

// A REFUSAL'S WORDS RIDE ITS ERROR: [caveat.Saying] keeps the error's message
// and every identity it answers, and [caveat.RefusalOf] reads the words back,
// through any wrapping a caller adds.
func TestARefusalsWordsRideItsError(t *testing.T) {
	t.Parallel()
	base := errors.New("worklet: the choice is not an option")
	err := fmt.Errorf("thread: %w", caveat.Saying(base, caveat.NotAnOption()))
	if !errors.Is(err, base) {
		t.Error("the phrasing hid the error it was put beside")
	}
	if err.Error() != "thread: "+base.Error() {
		t.Errorf("the phrasing changed the message: %q", err.Error())
	}
	if got := caveat.RefusalOf(err); got != caveat.NotAnOption() {
		t.Errorf("read back %q, want the phrasing it was given", got.Person())
	}
	if !caveat.RefusalOf(base).IsZero() {
		t.Error("an error carrying no phrasing read as one")
	}
	if caveat.Saying(nil, caveat.NotAnOption()) != nil {
		t.Error("a nil error became one")
	}
	//nolint:errorlint // Identity is the claim: a zero phrasing hands back err itself.
	if caveat.Saying(base, caveat.Refusal{}) != base {
		t.Error("a zero phrasing wrapped the error")
	}
	raw, err := json.Marshal(caveat.Said{Words: caveat.QuestionRemoved()})
	if err != nil {
		t.Fatal(err)
	}
	var back caveat.Said
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Words != caveat.QuestionRemoved() {
		t.Errorf("a phrasing crossed as %q", back.Words.Person())
	}
}

// THE WALKS ABOVE COVER THE WHOLE CATALOG: every exported constructor this
// package declares, of a caveat or of a refusal, is in [samples] or
// [refusalSamples], and every caveat code has a constructor there. Read from
// the source, so a constructor added without a sample fails here rather than
// shipping an unread person phrasing.
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
			// A READER OF ONE, not a constructor: [caveat.RefusalOf] takes
			// the error a phrasing rides and writes no words of its own.
			if readsAnError(fn) {
				continue
			}
			if id, ok := results[0].Type.(*ast.Ident); ok &&
				(id.Name == "Caveat" || id.Name == "Refusal") {
				constructors = append(constructors, fn.Name.Name)
			}
		}
	}
	if len(constructors) == 0 {
		t.Fatal("found no constructors: the walk is reading the wrong source")
	}
	for _, name := range constructors {
		_, sampled := samples[name]
		_, refusal := refusalSamples[name]
		if !sampled && !refusal {
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

// readsAnError reports whether fn takes an error, which no constructor does.
func readsAnError(fn *ast.FuncDecl) bool {
	for _, param := range fn.Type.Params.List {
		if id, ok := param.Type.(*ast.Ident); ok && id.Name == "error" {
			return true
		}
	}
	return false
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
