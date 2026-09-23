package stream_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD KNOWS EXACTLY THE FRAME KINDS THIS SOCKET SENDS.
//
// `LiveSocket.onMessage` switches on a frame's kind and falls silently through
// one its `PushKind` union does not name — which is right for a kind a newer
// peer added and wrong for one this build sends, where it is a push the store
// never applies and a screen that never updates, with nothing anywhere saying
// so. The other direction is a dispatch branch nothing can reach. So the union
// (`contract/wire.ts`) is held against every constant of type [stream.Kind] in
// this package's source, read from the source so a new kind is covered the
// moment it is declared rather than when somebody remembers to list it here.
//
// The engine used to have a fourth spelling of four of these: the config-apply
// hook in cmd/crewlet broadcast the roster, the org tree, the tool catalogue
// and the schedules under string literals. Kind is a named type now and those
// call sites name the constants, so the set this reads is every kind a frame
// can carry.
func TestTheDashboardKnowsExactlyThePushKindsTheEngineSends(t *testing.T) {
	t.Parallel()
	engine := engineKinds(t)
	client, err := clientsource.Union("../"+clientsource.Tree, "PushKind")
	if err != nil {
		// FAILS rather than skips: the dashboard source is committed, so a
		// missing declaration is a renamed one.
		t.Fatal(err)
	}
	if len(client) == 0 {
		t.Fatal("the PushKind union has no members, so this test could not fail")
	}
	for _, kind := range engine {
		if !slices.Contains(client, kind) {
			t.Errorf("the engine sends frames of kind %q and the dashboard's PushKind "+
				"does not name it, so the socket drops every one of them silently", kind)
		}
	}
	for _, kind := range client {
		if !slices.Contains(engine, kind) {
			t.Errorf("the dashboard's PushKind names %q and no stream.Kind constant "+
				"spells it — a renamed kind leaves exactly this behind, a dispatch "+
				"branch nothing can reach", kind)
		}
	}
	for i, kind := range client {
		if slices.Contains(client[:i], kind) {
			t.Errorf("the dashboard's PushKind names %q twice", kind)
		}
	}
}

// engineKinds is the value of every constant of type Kind in this package's
// source.
func engineKinds(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var kinds []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				typ, ok := value.Type.(*ast.Ident)
				if !ok || typ.Name != "Kind" {
					continue
				}
				for i, ident := range value.Names {
					if i >= len(value.Values) {
						t.Fatalf("%s is a Kind with no value of its own", ident.Name)
					}
					lit, ok := value.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s is a Kind whose value is not a string literal, "+
							"so this gate cannot read it", ident.Name)
					}
					kind, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("%s: %v", ident.Name, err)
					}
					kinds = append(kinds, kind)
				}
			}
		}
	}
	// A FLOOR, not a count: fourteen today, and a gate that read none — a
	// Kind moved to another file shape, or the type renamed — must not pass.
	if len(kinds) < 10 {
		t.Fatalf("found %d Kind constant(s) in this package, so this gate reads "+
			"less than the socket sends", len(kinds))
	}
	return kinds
}
