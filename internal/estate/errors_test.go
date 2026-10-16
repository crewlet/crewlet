package estate

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// throughWire is err as the asking node rebuilds it.
func throughWire(t *testing.T, err error) error {
	t.Helper()
	raw, marshalErr := json.Marshal(encodeError(err))
	if marshalErr != nil {
		t.Fatalf("encode %v: %v", err, marshalErr)
	}
	var w wireError
	if unmarshalErr := json.Unmarshal(raw, &w); unmarshalErr != nil {
		t.Fatalf("decode %s: %v", raw, unmarshalErr)
	}
	return decodeError(&w)
}

// EVERY SENTINEL ANSWERS errors.Is ON THE FAR SIDE, wrapped as a caller
// wraps it, and the words cross unchanged. A tool that asked "is this no such
// task" of a remote answer and was told no would tell a model its item does
// not exist in the wrong words — or, worse, file it again.
func TestEverySentinelKeepsItsIdentityAcrossTheWire(t *testing.T) {
	t.Parallel()
	for _, s := range sentinels {
		wrapped := fmt.Errorf("tracker: while doing the thing: %w", s.err)
		got := throughWire(t, wrapped)
		if !errors.Is(got, s.err) {
			t.Errorf("%s: errors.Is is false after the wire", s.code)
		}
		if got.Error() != wrapped.Error() {
			t.Errorf("%s: the message became %q, want %q", s.code, got, wrapped)
		}
		for _, other := range sentinels {
			if other.code != s.code && errors.Is(got, other.err) && !errors.Is(wrapped, other.err) {
				t.Errorf("%s: the rebuilt error also claims to be %s", s.code, other.code)
			}
		}
	}
}

// A REFUSAL THAT IS TWO SENTINELS AT ONCE KEEPS BOTH.
//
// A page's bad parent is a field refusal ([pages.ErrInvalid], which a tool
// renders as "refused that") AND the one fact [pages.ErrParent] names, in the
// shape the pages store builds it. A stateless node's tool branches on either,
// so the wire has to carry both rather than whichever it met first.
func TestARefusalThatIsTwoSentinelsKeepsBoth(t *testing.T) {
	t.Parallel()
	original := fmt.Errorf("%w (%w)",
		fmt.Errorf("%w: parent_id: page x is in the trash", pages.ErrInvalid),
		pages.ErrParent)
	got := throughWire(t, original)
	if !errors.Is(got, pages.ErrInvalid) || !errors.Is(got, pages.ErrParent) {
		t.Fatalf("a parent refusal lost an identity across the wire: %v", got)
	}
	if got.Error() != original.Error() {
		t.Fatalf("the message became %q, want %q", got, original)
	}
}

// A REFUSAL CROSSES WITH ITS REASON AND ITS CAUSE. A tool branches on the
// reason (`op_reused` has a remedy of its own), and the cause is what still
// answers errors.Is — a stream recreated behind `wrong_stream` — so a
// refusal rebuilt as its message alone would lose both. And the writer of a
// copy it names crosses too: whether the reason is the serving node's standing
// or another node's is what its remedy turns on.
func TestATypedRefusalCrossesWithItsFieldsAndItsCause(t *testing.T) {
	t.Parallel()
	original := fmt.Errorf("write: %w", &statelog.Unavailable{
		Reason: statelog.ReasonOpReused, Detail: "the op wrote elsewhere",
		Position: statelog.Position{Stream: trackerStream, Generation: 2, Seq: 41},
		OpID:     "op-1", CopyWriter: "node-b",
		Cause: fmt.Errorf("because: %w", statelog.ErrStreamRecreated),
	})
	got := throughWire(t, original)
	var refusal *statelog.Unavailable
	if !errors.As(got, &refusal) {
		t.Fatalf("errors.As(*statelog.Unavailable) is false after the wire: %v", got)
	}
	if refusal.Reason != statelog.ReasonOpReused || refusal.OpID != "op-1" ||
		refusal.Position.Seq != 41 || refusal.Detail != "the op wrote elsewhere" ||
		refusal.CopyWriter != "node-b" {
		t.Errorf("the refusal's fields did not survive: %+v", refusal)
	}
	if !errors.Is(refusal.Cause, statelog.ErrStreamRecreated) {
		t.Errorf("the refusal's cause lost its identity: %v", refusal.Cause)
	}
	if !errors.Is(got, statelog.ErrUnavailable) || !errors.Is(got, statelog.ErrStreamRecreated) {
		t.Error("the chain's sentinels did not survive beside the typed value")
	}
}

// THE ROUTER'S OWN WORDS CROSS TOO, and so does every answer the router's
// failover turns on. An estate nobody served says who was asked on the far
// side of a second hop; and an unvouched step rebuilt from the wire must still
// read as one another node's ledger may answer, or a router that met it on
// another node would stop at it rather than ask the next, under its operation
// id.
func TestTheRouterReadsARefusalTheWayItWasSent(t *testing.T) {
	t.Parallel()
	var unserved *ErrUnserved
	if !errors.As(throughWire(t, &ErrUnserved{Detail: "data-a: no answer"}), &unserved) ||
		unserved.Detail != "data-a: no answer" {
		t.Fatalf("an unserved estate crossed as %+v", unserved)
	}
	unvouchedStep := throughWire(t, fmt.Errorf("walk: %w", tracker.ErrStepUnvouched))
	if !unvouched(opCreateTask.spec, nil, unvouchedStep) {
		t.Errorf("an unvouched step crossed as %v, which the router reads as final", unvouchedStep)
	}
	if unvouched(opCommentPage.spec, nil, unvouchedStep) {
		t.Error("a page write — never repeated — was read as one to ask again")
	}
}

// A TAG CLASH CROSSES AS A TAG CLASH, whose other tag is what the tool names
// back to the model.
func TestAStructuredRefusalKeepsItsValue(t *testing.T) {
	t.Parallel()
	original := &tracker.TagClash{Project: "ENG", Slug: "bug-fix", Label: "Bug fix",
		Other: tracker.Tag{Slug: "bugfix", Label: "Bugfix"}}
	var clash *tracker.TagClash
	if !errors.As(throughWire(t, original), &clash) {
		t.Fatal("errors.As(*tracker.TagClash) is false after the wire")
	}
	if !reflect.DeepEqual(clash, original) {
		t.Errorf("clash = %+v, want %+v", clash, original)
	}
}

// A REFUSAL A PERSON IS SHOWN CROSSES AS THE SENTENCE THEY ARE SHOWN.
//
// Each package's Sentence gives a person the writer's own words without the
// Go prefix — and it finds them by unwrapping the package's refusal TYPE. A
// refusal decided on the data node that answered and printed on the node whose
// caller asked has crossed the wire, so the type has to cross with its fields
// or Sentence finds nothing and prints "tracker: …" or "pages: invalid: …" to
// the person. The class must survive beside it, since a surface picks the
// refusal's class by it — and the rebuilt refusal must answer for every
// sentinel it carries, the one a refusal holds of its own beside its class
// included, since that is what a caller acting on that refusal in particular
// tests.
func TestARefusalsSentenceSurvivesTheWire(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		refusal  error
		marks    []error
		sentence func(error) string
		// typed is the refusal's own value out of an error, or nil.
		typed func(error) error
	}{
		{"tracker invalid", &tracker.Refusal{Mark: tracker.ErrInvalid,
			Err: errors.New("a title is required — give the item one")},
			[]error{tracker.ErrInvalid}, tracker.Sentence, trackerRefusal},
		{"tracker forbidden", &tracker.Refusal{Mark: tracker.ErrForbidden,
			Err: errors.New("only ana may close this — ask her")},
			[]error{tracker.ErrForbidden}, tracker.Sentence, trackerRefusal},
		{"tracker forbidden with a sentinel of its own", &tracker.Refusal{
			Mark: tracker.ErrForbidden, Also: tracker.ErrRestoresOthers,
			Err: errors.New("2 other tasks went to the trash with it — ask ana")},
			[]error{tracker.ErrForbidden, tracker.ErrRestoresOthers},
			tracker.Sentence, trackerRefusal},
		{"pages", &pages.InvalidError{Field: "title",
			Why: "is empty — give the page one"},
			[]error{pages.ErrInvalid}, pages.Sentence, pagesRefusal},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			want := c.sentence(c.refusal)
			if want == c.refusal.Error() {
				t.Fatalf("the original %q has no sentence apart from its message, so "+
					"this case would pass whatever the wire did", want)
			}
			got := throughWire(t, c.refusal)
			if said := c.sentence(got); said != want {
				t.Errorf("a person is shown %q across the wire, want %q", said, want)
			}
			if got.Error() != c.refusal.Error() {
				t.Errorf("the message became %q, want %q", got, c.refusal)
			}
			rebuilt := c.typed(got)
			if rebuilt == nil {
				t.Fatalf("the refusal's type did not cross: %v", got)
			}
			for _, mark := range c.marks {
				if !errors.Is(got, mark) {
					t.Errorf("the error lost %v across the wire: %v", mark, got)
				}
				if !errors.Is(rebuilt, mark) {
					t.Errorf("the rebuilt refusal no longer answers %v: %#v", mark, rebuilt)
				}
			}
		})
	}
}

func trackerRefusal(err error) error {
	var r *tracker.Refusal
	if errors.As(err, &r) {
		return r
	}
	return nil
}

func pagesRefusal(err error) error {
	var r *pages.InvalidError
	if errors.As(err, &r) {
		return r
	}
	return nil
}

// AN IDENTITY THIS BUILD DOES NOT KNOW IS DROPPED AND THE WORDS KEPT — a
// newer peer's sentinel mid-upgrade must not turn an answer into a failure to
// decode one.
func TestAnUnknownIdentityKeepsItsMessage(t *testing.T) {
	t.Parallel()
	got := decodeError(&wireError{Message: "something new went wrong",
		Sentinels: []string{"tracker.ErrFromTheFuture"},
		Typed:     []typedError{{Code: "tracker.FutureRefusal"}}})
	if got == nil || got.Error() != "something new went wrong" {
		t.Fatalf("got %v", got)
	}
}

// THE REGISTRY IS COMPLETE, and this is the gate that keeps it so: every
// exported sentinel and every error type of the packages whose errors cross
// is registered — parsed from their source, because the list of what a tool
// compares grows in another package and nothing else would tell this one.
func TestEveryErrorOfTheCrossingPackagesIsRegistered(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	registeredSentinels := map[string]bool{}
	for _, s := range sentinels {
		registeredSentinels[s.code] = true
	}
	registeredTypes := map[string]bool{}
	for _, k := range typedKinds {
		registeredTypes[k.code] = true
	}
	var found int
	// THIS PACKAGE'S OWN TOO: a router refuses with them, and a node that
	// answers on another's behalf carries them back across a second hop.
	//
	// AND THE IDENTITY DIRECTORY'S, whose reads a node holding no data asks
	// of a data node, beside the identity vocabulary's and the session's,
	// whose answers a person verb is decided on. A code is the identifier
	// as Go code spells it, so a nested package is named by its last
	// element.
	for _, pkg := range []string{"tracker", "pages", "statelog", "estate",
		"iamdomain", "iam", "iam/session"} {
		vars, types := declaredErrors(t, filepath.Join(root, "internal", filepath.FromSlash(pkg)))
		name := path.Base(pkg)
		for _, v := range vars {
			found++
			if !registeredSentinels[name+"."+v] {
				t.Errorf("%s.%s is an exported sentinel that does not cross the wire: "+
					"register it in estate's sentinels", name, v)
			}
		}
		for _, ty := range types {
			found++
			if name == "estate" && ty == "remoteError" {
				// THE WIRE'S OWN REBUILT ERROR, which is what every
				// other value here crosses AS — it never crosses itself.
				continue
			}
			if !ast.IsExported(ty) {
				// AN UNEXPORTED ERROR TYPE CANNOT CROSS: the wire
				// rebuilds a type from its exported fields, so one of
				// these arrives as its message alone. That is how a
				// refusal's sentence was lost on every routed write —
				// the tracker's and the knowledge base's refusal types
				// were both unexported, so each package's Sentence on
				// the asking node found no refusal and handed a person
				// the Go error's prefix.
				t.Errorf("%s.%s is an unexported error type, which the wire "+
					"rebuilds as its message alone: export it with its fields "+
					"and register it in estate's typedKinds", name, ty)
				continue
			}
			if !registeredTypes[name+"."+ty] {
				t.Errorf("%s.%s is an error type that does not cross the wire: "+
					"register it in estate's typedKinds", name, ty)
			}
		}
	}
	// A GATE THAT READ NOTHING PASSES — so it says what it read.
	if found < 30 {
		t.Fatalf("the gate found only %d declarations; it is not reading the packages", found)
	}
}

// declaredErrors is every exported Err* package variable and every type with
// an `Error() string` method — exported or not — in one package directory,
// tests excluded.
func declaredErrors(t *testing.T, dir string) (vars, types []string) {
	t.Helper()
	fset := token.NewFileSet()
	matches, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.VAR {
					continue
				}
				for _, spec := range d.Specs {
					for _, name := range spec.(*ast.ValueSpec).Names {
						if name.IsExported() && strings.HasPrefix(name.Name, "Err") {
							vars = append(vars, name.Name)
						}
					}
				}
			case *ast.FuncDecl:
				if d.Recv == nil || d.Name.Name != "Error" || len(d.Type.Params.List) != 0 {
					continue
				}
				recv := d.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if ident, ok := recv.(*ast.Ident); ok {
					types = append(types, ident.Name)
				}
			}
		}
	}
	slices.Sort(vars)
	slices.Sort(types)
	return slices.Compact(vars), slices.Compact(types)
}
