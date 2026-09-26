package estate

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

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

// A REFUSAL CROSSES WITH ITS REASON AND ITS CAUSE. A tool branches on the
// reason (`op_reused` has a remedy of its own), and the cause is what still
// answers errors.Is — a stream recreated behind `wrong_stream` — so a
// refusal rebuilt as its message alone would lose both.
func TestATypedRefusalCrossesWithItsFieldsAndItsCause(t *testing.T) {
	t.Parallel()
	original := fmt.Errorf("write: %w", &statelog.Unavailable{
		Reason: statelog.ReasonOpReused, Detail: "the op wrote elsewhere",
		Position: statelog.Position{Stream: trackerStream, Generation: 2, Seq: 41},
		OpID:     "op-1", Cause: fmt.Errorf("because: %w", statelog.ErrStreamRecreated),
	})
	got := throughWire(t, original)
	var refusal *statelog.Unavailable
	if !errors.As(got, &refusal) {
		t.Fatalf("errors.As(*statelog.Unavailable) is false after the wire: %v", got)
	}
	if refusal.Reason != statelog.ReasonOpReused || refusal.OpID != "op-1" ||
		refusal.Position.Seq != 41 || refusal.Detail != "the op wrote elsewhere" {
		t.Errorf("the refusal's fields did not survive: %+v", refusal)
	}
	if !errors.Is(refusal.Cause, statelog.ErrStreamRecreated) {
		t.Errorf("the refusal's cause lost its identity: %v", refusal.Cause)
	}
	if !errors.Is(got, statelog.ErrUnavailable) || !errors.Is(got, statelog.ErrStreamRecreated) {
		t.Error("the chain's sentinels did not survive beside the typed value")
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
	for _, pkg := range []string{"tracker", "pages", "statelog"} {
		vars, types := declaredErrors(t, filepath.Join(root, "internal", pkg))
		for _, name := range vars {
			found++
			if !registeredSentinels[pkg+"."+name] {
				t.Errorf("%s.%s is an exported sentinel that does not cross the wire: "+
					"register it in estate's sentinels", pkg, name)
			}
		}
		for _, name := range types {
			found++
			if !registeredTypes[pkg+"."+name] {
				t.Errorf("%s.%s is an error type that does not cross the wire: "+
					"register it in estate's typedKinds", pkg, name)
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
