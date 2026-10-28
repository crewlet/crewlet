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

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
		wrapped := fmt.Errorf("worklet: while doing the thing: %w", s.err)
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
// A page's bad parent is a field refusal ([doclet.ErrInvalid], which a tool
// renders as "refused that") AND the one fact [doclet.ErrParent] names, in the
// shape the pages store builds it. A stateless node's tool branches on either,
// so the wire has to carry both rather than whichever it met first.
func TestARefusalThatIsTwoSentinelsKeepsBoth(t *testing.T) {
	t.Parallel()
	original := fmt.Errorf("%w (%w)",
		fmt.Errorf("%w: parent_id: page x is in the trash", doclet.ErrInvalid),
		doclet.ErrParent)
	got := throughWire(t, original)
	if !errors.Is(got, doclet.ErrInvalid) || !errors.Is(got, doclet.ErrParent) {
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
	unvouchedStep := throughWire(t, fmt.Errorf("walk: %w", worklet.ErrStepUnvouched))
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
	original := &worklet.TagClash{Project: "ENG", Slug: "bug-fix", Label: "Bug fix",
		Other: worklet.Tag{Slug: "bugfix", Label: "Bugfix"}}
	var clash *worklet.TagClash
	if !errors.As(throughWire(t, original), &clash) {
		t.Fatal("errors.As(*worklet.TagClash) is false after the wire")
	}
	if !reflect.DeepEqual(clash, original) {
		t.Errorf("clash = %+v, want %+v", clash, original)
	}
}

// A REFUSAL'S WORDS FOR A PERSON CROSS WITH IT (ADR-0060): a write a data
// node refused answers the person on the node they are on in the words the
// writer chose, whether the writer put them beside its error or a typed
// refusal carries them. AND A ROLLING UPGRADE DEGRADES TO THE FLOOR: an older
// peer sends no phrasing, and the refusal reads as one with none, which the
// act route answers with its code's own sentence, never the model's message.
func TestARefusalsPersonWordsCrossTheRouter(t *testing.T) {
	t.Parallel()
	said := throughWire(t, fmt.Errorf("thread: %w", caveat.Saying(
		fmt.Errorf("worklet: the choice is not an option: %w", worklet.ErrInvalid),
		caveat.NotAnOption())))
	if got := caveat.RefusalOf(said); got != caveat.NotAnOption() {
		t.Errorf("a phrased refusal crossed as %q", got.Person())
	}
	if !errors.Is(said, worklet.ErrInvalid) {
		t.Error("the phrasing hid the refusal's class on the wire")
	}
	capped := throughWire(t, &worklet.TextCapError{Field: worklet.TextTitle,
		Size: 300, Limit: 256})
	if got := caveat.RefusalOf(capped); got != caveat.TitleTooLong() {
		t.Errorf("a title over its cap crossed as %q", got.Person())
	}
	older := decodeError(&wireError{Message: "worklet: the choice \"x\" is not an " +
		"option of the decision", Sentinels: []string{"worklet.ErrInvalid"}})
	if got := caveat.RefusalOf(older); !got.IsZero() {
		t.Errorf("an older peer's refusal read as phrased: %q", got.Person())
	}
}

// AN IDENTITY THIS BUILD DOES NOT KNOW IS DROPPED AND THE WORDS KEPT — a
// newer peer's sentinel mid-upgrade must not turn an answer into a failure to
// decode one.
func TestAnUnknownIdentityKeepsItsMessage(t *testing.T) {
	t.Parallel()
	got := decodeError(&wireError{Message: "something new went wrong",
		Sentinels: []string{"worklet.ErrFromTheFuture"},
		Typed:     []typedError{{Code: "worklet.FutureRefusal"}}})
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
	for _, pkg := range []string{"worklet", "doclet", "statelog", "estate",
		"iamdomain", "iam", "iam/session", "artifactsdomain", "artifacts",
		"artifacts/manifest", "artifacts/mediatype", "artifactdata", "artifactdata/canon",
		"artifactdata/query", "caveat"} {
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
// an `Error() string` method in one package directory, tests excluded.
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
				if ident, ok := recv.(*ast.Ident); ok && ident.IsExported() {
					types = append(types, ident.Name)
				}
			}
		}
	}
	slices.Sort(vars)
	slices.Sort(types)
	return slices.Compact(vars), slices.Compact(types)
}
