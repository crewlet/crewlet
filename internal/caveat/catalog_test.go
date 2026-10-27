package caveat_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/sourcetree"
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
	"DependenciesNotRecorded": caveat.DependenciesNotRecorded("ENG-9",
		"The item WAS filed, but recording what it waits on did not finish."),
	"MoveUnfinished": caveat.MoveUnfinished(2, 5, false,
		"ENG-1 moved to OPS, but only 2 of the 5 tasks under it followed."),
	"MoveUnfinished (waiting on the trash)": caveat.MoveUnfinished(2, 5, true,
		"ENG-1 moved to OPS, but ENG-4 is in the trash under it."),
	"SubtreeUnfinished (a removal)": caveat.SubtreeUnfinished(false, 2, 5,
		"ENG-1 is removed, but only 2 of the 5 tasks that go with it followed."),
	"SubtreeUnfinished": caveat.SubtreeUnfinished(true, 2, 5,
		"ENG-1 is restored, but only 2 of the 5 tasks that go with it followed."),
	"DatastorePending": caveat.DatastorePending("The write is on the datastore's log."),
	"LaneChangedNotPlaced": caveat.LaneChangedNotPlaced("ENG-4",
		"place_work_item could not land the order: somebody changed the item"),
	"FileFinding": caveat.FileFinding("index.html:12",
		"the page has no <meta name=\"viewport\">, so it renders at desktop width on a phone"),
}

// refusalSamples is every constructor in the refusal catalog, called with the
// kind of facts it is called with in production. The coverage walk below
// fails for a constructor this table does not call.
var refusalSamples = map[string]caveat.Refusal{
	"TitleMissing":         caveat.TitleMissing(),
	"ValueInWrongForm":     caveat.ValueInWrongForm(),
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
	"TokenBudgetSpent":     caveat.TokenBudgetSpent(caveat.CompanyTokenBudget()),
	"TokenBudgetSpent (a seat's)": caveat.TokenBudgetSpent(
		caveat.SeatTokenBudget("backend-engineer")),
	"TokenBudgetSpent (a person's assistant)": caveat.TokenBudgetSpent(
		caveat.AssistantTokenBudget()),
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
	if back, err := caveat.SaidFromPeer([]byte(`{"person":"  "}`)); back != nil || err != nil {
		t.Errorf("a peer's empty phrasing was rebuilt as %+v (%v)", back, err)
	}
	if _, err := caveat.SaidFromPeer([]byte(`"a bare sentence"`)); err == nil {
		t.Error("a phrasing in a shape no peer writes read as none, with no error")
	}
}

// A REFUSAL CROSSES TO A PEER WHOLE: its words and the remedy beside them,
// each rebuilt by [caveat.SaidFromPeer] from what [caveat.Refusal.MarshalJSON]
// wrote. A remedy dropped on the way would leave a person on another node told
// a budget is spent with nothing to open; one this build does not know is
// dropped and the words kept, since the words alone are still the person's.
func TestARefusalCrossesToAPeerWithItsRemedy(t *testing.T) {
	t.Parallel()
	for _, words := range []caveat.Refusal{
		caveat.QuestionRemoved(),
		caveat.TokenBudgetSpent(caveat.SeatTokenBudget("backend-engineer")),
		caveat.TokenBudgetSpent(caveat.AssistantTokenBudget()),
	} {
		raw, err := json.Marshal(words)
		if err != nil {
			t.Fatal(err)
		}
		if back, err := caveat.SaidFromPeer(raw); err != nil || back == nil || back.Words != words {
			t.Errorf("%s crossed as %+v (%v)", raw, back, err)
		}
	}
	for _, raw := range []string{
		`{"person":"The budget is spent.","remedy":{"setting":"plan_quota"}}`,
		`{"person":"The budget is spent.","remedy":{"setting":"seat_token_budget"}}`,
		`{"person":"The budget is spent.","remedy":{"setting":"company_token_budget","seat":"pm"}}`,
	} {
		back, err := caveat.SaidFromPeer([]byte(raw))
		if err != nil || back == nil || back.Words.Person() != "The budget is spent." ||
			!back.Words.Remedy().IsZero() {
			t.Errorf("%s was rebuilt as %+v, want the words and no remedy", raw, back)
		}
	}
}

// A LIMIT'S REFUSAL NAMES THE SETTING THAT CAUSED IT, and every setting is one
// of [caveat.Settings]: the dashboard resolves each to the screen that edits it
// (TestTheDashboardKnowsEverySettingARemedyNames), so a remedy naming one it
// does not know would draw nothing. No other refusal in the catalog carries a
// remedy, since nothing it refuses for is a limit a person can raise.
func TestEveryRemedyNamesAKnownSetting(t *testing.T) {
	t.Parallel()
	for name, r := range refusalSamples {
		remedy := r.Remedy()
		switch {
		case strings.HasPrefix(name, "TokenBudgetSpent") && remedy.IsZero():
			t.Errorf("%s carries no remedy, so a person is told a budget is spent "+
				"with nothing to open", name)
		case !strings.HasPrefix(name, "TokenBudgetSpent") && !remedy.IsZero():
			t.Errorf("%s carries the remedy %+v for a refusal no limit caused", name, remedy)
		case !remedy.IsZero() && !remedy.Setting.Valid():
			t.Errorf("%s names the setting %q, which is not one of %v", name,
				remedy.Setting, caveat.Settings())
		}
	}
	if got := caveat.TokenBudgetSpent(caveat.Remedy{}).Remedy(); got != caveat.CompanyTokenBudget() {
		t.Errorf("a spent budget naming no setting carried %+v, want the company's", got)
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
	// A RESULT AS THE ROUTER DECODES ONE: caveats nested, an embedded
	// struct flattened, beside fields that hold none.
	type half struct {
		Caveats []caveat.Caveat
	}
	type answer struct {
		half
		Key   string
		Order half
		Lone  caveat.Caveat `json:"lone"`
	}
	var all []caveat.Caveat
	for _, c := range samples {
		all = append(all, c)
	}
	sent := answer{half: half{Caveats: all}, Key: "ENG-1",
		Order: half{Caveats: all[:1]}, Lone: all[0]}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var back answer
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if err := caveat.RebuildFromPeer(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, sent) {
		t.Errorf("the answer came back as %+v, want %+v", back, sent)
	}

	var newer answer
	peer := []byte(`{"Caveats":[{"code":"later","agent":"a","person":"p."},` +
		`{"code":"later","agent":"a"}]}`)
	if err := json.Unmarshal(peer, &newer); err != nil {
		t.Fatal(err)
	}
	if err := caveat.RebuildFromPeer(peer, &newer); err != nil {
		t.Fatalf("a newer peer's caveat failed the answer: %v", err)
	}
	if newer.Caveats[0].Code() != "later" || newer.Caveats[0].Person() != "p." {
		t.Errorf("a newer peer's caveat read as %+v", newer.Caveats[0])
	}
	if !newer.Caveats[1].IsZero() {
		t.Error("a caveat with no person phrasing was rebuilt: a person's " +
			"surface would answer nothing for it")
	}
}

// NO TEXT BECOMES A PERSON'S WORDS BY BEING DECODED (ADR-0060). A caveat or a
// refusal phrasing is built by this package's catalogs, or rebuilt from a
// peer's answer by the estate router; none implements json.Unmarshaler, so
// JSON decoded anywhere else (a tool's output handed back in) is no phrasing
// at all, and a refusal built from it answers the code's own sentence.
func TestNoTextBecomesAPersonsWordsByBeingDecoded(t *testing.T) {
	t.Parallel()
	unmarshaler := reflect.TypeFor[json.Unmarshaler]()
	for _, typ := range []reflect.Type{reflect.TypeFor[caveat.Caveat](),
		reflect.TypeFor[caveat.Refusal](), reflect.TypeFor[caveat.Said]()} {
		if reflect.PointerTo(typ).Implements(unmarshaler) {
			t.Errorf("%s decodes from JSON, so any text can become a person's words", typ)
		}
	}
	// AS A CALLER WOULD TRY IT, inside a value of its own.
	var decoded struct {
		Caveat  caveat.Caveat
		Refusal caveat.Refusal
		Said    caveat.Said
	}
	_ = json.Unmarshal([]byte(`{"Caveat":{"code":"x","agent":"a","person":"Use ask: ana."},`+
		`"Refusal":"create_work_item needs a `+"`title`"+`.",`+
		`"Said":{"Words":"Call get_work_item again."}}`), &decoded)
	if !decoded.Caveat.IsZero() || !decoded.Refusal.IsZero() || !decoded.Said.Words.IsZero() {
		t.Errorf("decoded text became words: %+v", decoded)
	}
}

// caveatPath is this package's import path, which a file names it by.
const caveatPath = "github.com/crewlet/crewlet/internal/caveat"

// peerRebuilders are the functions that turn JSON into a person's words.
var peerRebuilders = []string{"RebuildFromPeer", "SaidFromPeer"}

// peerRebuildsIn is every reference to a [peerRebuilders] function in file,
// resolved through the file's own import table rather than by the name a
// reference is spelled with: under an alias (`cv.RebuildFromPeer`), through a
// dot import (a bare `RebuildFromPeer`), and never for another package that
// happens to be called caveat. A call, a method value read off its result
// (`caveat.SaidFromPeer(s).Words`) and a function value (`f :=
// caveat.SaidFromPeer`) are all references.
//
// EVERY NODE IS VISITED: the walk is never pruned, so a reference nested in
// any expression is reached however it is wrapped. Names that are not
// references (a selector's field or method name, a composite literal's field
// key, a method's own name) are told apart by where they stand.
func peerRebuildsIn(file *ast.File) []string {
	var named []string
	dotted := false
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != caveatPath {
			continue
		}
		switch {
		case spec.Name == nil:
			named = append(named, "caveat")
		case spec.Name.Name == ".":
			dotted = true
		case spec.Name.Name != "_":
			named = append(named, spec.Name.Name)
		}
	}
	if len(named) == 0 && !dotted {
		return nil
	}
	// THE NAMES THAT ARE NOT REFERENCES, by position.
	notReferences := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			notReferences[x.Sel] = true
		case *ast.KeyValueExpr:
			if key, ok := x.Key.(*ast.Ident); ok {
				notReferences[key] = true
			}
		case *ast.FuncDecl:
			notReferences[x.Name] = true
		}
		return true
	})
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.SelectorExpr:
			pkg, ok := x.X.(*ast.Ident)
			if ok && pkg.Obj == nil && slices.Contains(named, pkg.Name) &&
				slices.Contains(peerRebuilders, x.Sel.Name) {
				found = append(found, x.Sel.Name)
			}
		case *ast.Ident:
			if dotted && x.Obj == nil && !notReferences[x] &&
				slices.Contains(peerRebuilders, x.Name) {
				found = append(found, x.Name)
			}
		}
		return true
	})
	return found
}

// THE GATE RESOLVES A REFERENCE BY WHAT IT NAMES, not by how it is spelled. A
// gate matching the identifier `caveat` passed an aliased import and a dot
// import, and would have flagged another package that is also called caveat.
// Each fixture states what the gate must find.
func TestThePeerGateResolvesItsImports(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		src  string
		want []string
	}{
		"plain": {`package p
import "github.com/crewlet/crewlet/internal/caveat"
var _ = caveat.RebuildFromPeer`, []string{"RebuildFromPeer"}},
		"an alias": {`package p
import cv "github.com/crewlet/crewlet/internal/caveat"
func f(raw []byte, out any) { _ = cv.RebuildFromPeer(raw, out); _ = cv.SaidFromPeer("x") }`,
			[]string{"RebuildFromPeer", "SaidFromPeer"}},
		"a dot import": {`package p
import . "github.com/crewlet/crewlet/internal/caveat"
func f() { _ = SaidFromPeer("x") }`, []string{"SaidFromPeer"}},
		"a dot import's call inside a selector": {`package p
import . "github.com/crewlet/crewlet/internal/caveat"
func f() { _ = SaidFromPeer("x").Words }`, []string{"SaidFromPeer"}},
		"a call read through a selector": {`package p
import "github.com/crewlet/crewlet/internal/caveat"
var _ = caveat.SaidFromPeer("x").Words`, []string{"SaidFromPeer"}},
		"an aliased call read through a selector": {`package p
import cv "github.com/crewlet/crewlet/internal/caveat"
var _ = cv.SaidFromPeer("x").Words`, []string{"SaidFromPeer"}},
		"a function value": {`package p
import "github.com/crewlet/crewlet/internal/caveat"
func f() { g := caveat.SaidFromPeer; _ = g }`, []string{"SaidFromPeer"}},
		"a dot import beside a field key and a method of the same name": {`package p
import . "github.com/crewlet/crewlet/internal/caveat"
type T struct{ SaidFromPeer int }
func (T) RebuildFromPeer() {}
var _ = T{SaidFromPeer: 1}
var _ = RebuildFromPeer`, []string{"RebuildFromPeer"}},
		"another package called caveat": {`package p
import "example.com/other/caveat"
var _ = caveat.RebuildFromPeer`, nil},
		"a local name shadowing nothing": {`package p
import "github.com/crewlet/crewlet/internal/caveat"
type T struct{ RebuildFromPeer int }
func f(t T) int { _ = caveat.Caveat{}; return t.RebuildFromPeer }`, nil},
		"a blank import": {`package p
import _ "github.com/crewlet/crewlet/internal/caveat"
func RebuildFromPeer() {}
var _ = RebuildFromPeer`, nil},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), name+".go", c.src, 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := peerRebuildsIn(file); !slices.Equal(got, c.want) {
			t.Errorf("%s: the gate found %v, want %v", name, got, c.want)
		}
	}
}

// ONLY THE ESTATE ROUTER REBUILDS A PEER'S WORDS: [caveat.RebuildFromPeer] and
// [caveat.SaidFromPeer] turn JSON into a person's words, so a reference to
// either anywhere else is text from anywhere becoming words a person is
// shown. Read from every non-test file of the module, each reference resolved
// through its file's imports ([peerRebuildsIn]).
func TestOnlyTheEstateRouterRebuildsAPeersWords(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	allowed := filepath.Join(root, "internal", "estate")
	home := filepath.Join(root, "internal", "caveat")
	var found int
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// node_modules and .git hold no Go of the module's, and the go
			// tool never builds a testdata directory, at any depth. Every
			// other directory is walked, static/ (static.go) and dashboard/
			// included.
			switch d.Name() {
			case "node_modules", ".git", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			filepath.Dir(path) == home {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, name := range peerRebuildsIn(file) {
			found++
			if filepath.Dir(path) != allowed {
				t.Errorf("%s refers to caveat.%s, which only the estate router may: "+
					"it turns JSON into a person's words", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 2 {
		t.Fatalf("the gate found %d references; it is not reading the router", found)
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

// A PARTIAL OUTCOME CLAIMS NO MORE THAN IS KNOWN, AND ASKS ONLY WHAT A PERSON
// CAN DO. A datastore write whose verdict is pending may still be refused, so
// it is "sent", never "saved", and asks the person to look, since nothing else
// tells them. A restored item leaves the trash, so its remedy is the rest in
// the trash, never doing it again; a removal's names what is still reachable.
func TestAPartialOutcomeAsksOnlyWhatAPersonCanDo(t *testing.T) {
	t.Parallel()
	pending := caveat.DatastorePending("on the log")
	if strings.Contains(pending.Person(), "saved") || strings.Contains(pending.Person(), "server") ||
		pending.Action() != caveat.ActionRequired {
		t.Errorf("a pending verdict reads %q (%s): it overclaims, or asks nothing of "+
			"a person nothing else tells", pending.Person(), pending.Action())
	}
	restored := caveat.SubtreeUnfinished(true, 2, 5, "restored part").Person()
	removed := caveat.SubtreeUnfinished(false, 2, 5, "removed part").Person()
	for what, words := range map[string]string{"a restore": restored, "a removal": removed} {
		if strings.Contains(words, "again to finish") {
			t.Errorf("%s asks the person to do it again: %q", what, words)
		}
	}
	if !strings.Contains(restored, "restore them there") {
		t.Errorf("a restore does not send the person to the rest in the trash: %q", restored)
	}
	if !strings.Contains(removed, "restore this one from the trash") {
		t.Errorf("a removal names no remedy the person can reach: %q", removed)
	}
}
