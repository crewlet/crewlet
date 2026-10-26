package authz_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// SEAT-ONLY AND HUMAN-ONLY ARE CHECKED BEFORE THE CLASS.
//
// Both are facts about the VERB, so the refusal has to say what it is before
// any class weighs a permission: a seat holding nothing takes a seat-only verb
// (an ask — the role's registry decides who is offered it), a
// person holding admin is refused one as [authz.ReasonSeatOnly] naming no
// permission (none would change it), and a seat is refused a human-only one as
// [authz.ReasonSeatRefused], again naming none — a seat holding admin, and a
// seat holding nothing whom the class would refuse. Each value has to be
// stated by some row, or this certifies nothing about it.
//
// THE HUMAN-ONLY ORDER IS PROVED BY THE SEAT THE CLASS REFUSES: decided after
// the class, it is refused naming a permission (no_grant, not_self, not_lead)
// instead of as a seat. Mutation: check humanOnly after the class and every
// human-only row goes red on that seat. A seat holding admin alone could not
// tell the two orders apart, since every human-only class admits it.
//
// THE SEAT-ONLY ORDER CANNOT BE OBSERVED while every seat-only row is
// [authz.ClassEverybody] naming no app: that class admits everybody, so the
// bar refuses the same principal the same way on either side of it. A
// seat-only row of any other class FAILS this walk, naming the case it needs
// — a person the class refuses, who must still be told seat_only — so the
// order is proved the moment it becomes observable rather than assumed.
func TestSeatOnlyAndHumanOnlyAreCheckedBeforeTheClass(t *testing.T) {
	t.Parallel()
	var seatOnly, humanOnly int
	for _, a := range authz.Actions() {
		object := everythingAbout(seat("sre"))
		if only, _ := authz.SeatOnlyOf(a); only {
			seatOnly++
			class, _ := authz.ClassOf(a)
			if app, _ := authz.AppOf(a); class != authz.ClassEverybody || app != "" {
				t.Errorf("%s is seat-only and decided by %s (app %q), so the order of "+
					"its bar is observable: add a case deciding it as a person that "+
					"class refuses, who must be refused seat_only naming nothing", a,
					class, app)
			}
			if d := authz.Decide(t.Context(), seat("sre"), a, object, nimbus(),
				decidedAt); !d.Allowed {
				t.Errorf("%s: a seat holding nothing decided %+v, want admitted — "+
					"a seat-only verb asks no permission", a, d)
			}
			d := authz.Decide(t.Context(), person("jane.doe", iam.GrantAdmin), a,
				object, nimbus(), decidedAt)
			if d.Allowed || d.Reason != authz.ReasonSeatOnly || len(d.Grants) != 0 {
				t.Errorf("%s: a person holding admin decided %+v, want refused "+
					"as seat-only naming nothing", a, d)
			}
		}
		if only, _ := authz.HumanOnlyOf(a); only {
			humanOnly++
			d := authz.Decide(t.Context(), seat("cto", iam.GrantAdmin), a, object,
				nimbus(), decidedAt)
			if d.Allowed || d.Reason != authz.ReasonSeatRefused || len(d.Grants) != 0 {
				t.Errorf("%s: a seat holding admin decided %+v, want refused as a "+
					"seat naming nothing", a, d)
			}
			// A SEAT THE CLASS REFUSES: holding nothing, about somebody else's
			// record, in a container nobody leads. On an artifact row that
			// seat holds a viewer share: one with no relation at all is told
			// the artifact is not found, whatever bar the verb carries
			// (TestABarOnTheVerbIsNoExistenceOracle).
			refused := authz.Object{Kind: authz.KindPerson, Owner: "someone.else",
				Container: "NOWHERE"}
			if class, _ := authz.ClassOf(a); class == authz.ClassShared {
				refused = authz.Object{Kind: authz.KindArtifact, Owner: "someone.else",
					Shares: []artifacts.Share{seatShare("sam", artifacts.RoleViewer)}}
			}
			d = authz.Decide(t.Context(), seat("sam"), a, refused, nimbus(), decidedAt)
			if d.Allowed || d.Reason != authz.ReasonSeatRefused || len(d.Grants) != 0 {
				t.Errorf("%s: a seat the class refuses decided %+v, want refused as a "+
					"seat naming nothing", a, d)
			}
		}
	}
	if seatOnly == 0 || humanOnly == 0 {
		t.Fatalf("%d seat-only and %d human-only rows: a walk over none of one "+
			"certifies nothing about it", seatOnly, humanOnly)
	}
}

// NO ROW IS BOTH SEAT-ONLY AND HUMAN-ONLY.
//
// Such a row admits nobody at all, and it reads in the table as two careful
// decisions rather than as the dead verb it is. Mutation: mark a2a_ask
// human-only and this goes red.
func TestNoRowIsBothSeatOnlyAndHumanOnly(t *testing.T) {
	t.Parallel()
	for _, a := range authz.Actions() {
		seatOnly, _ := authz.SeatOnlyOf(a)
		humanOnly, _ := authz.HumanOnlyOf(a)
		if seatOnly && humanOnly {
			t.Errorf("%s is both seat-only and human-only, so nobody may take it", a)
		}
	}
	if _, known := authz.SeatOnlyOf("no.such.verb"); known {
		t.Error("SeatOnlyOf knows a verb the table has no row for")
	}
	if _, known := authz.HumanOnlyOf("no.such.verb"); known {
		t.Error("HumanOnlyOf knows a verb the table has no row for")
	}
}

// EVERY DOTTED VERB IS ASKED BY A CALL SITE THAT IS NOT A TEST.
//
// A verb named after a tool is held to the tools this build registers by a
// walk in internal/agent/builtin; a DOTTED verb has no tool, so nothing but
// this holds it to anything. A dotted row nothing asks is a rule that decides
// nothing — `pages.container` sat in the table, asked by no surface, for as
// long as nobody happened to read it — and it reads to a reviewer as a gesture
// the engine gates. So the walk reads this package's own rules.go for the
// wire name of every `Action…` constant, then every non-test Go file in the
// tree outside this package for an `authz.<Constant>` selector, and fails on a
// dotted verb no such selector names.
//
// Mutation: delete `work.thread`'s one call site in internal/api/workapi and
// this goes red naming it.
func TestEveryDottedVerbIsAskedByANonTestCallSite(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	wire := actionWireNames(t, filepath.Join(root, "internal", "authz", "rules.go"))
	if len(wire) != len(authz.Actions()) {
		t.Fatalf("read %d Action constants out of rules.go and the table holds %d "+
			"verbs — the parse no longer sees the declarations", len(wire),
			len(authz.Actions()))
	}
	asked := map[string]bool{}
	files := 0
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "testdata", "static", "dashboard":
				return filepath.SkipDir
			}
			if path == filepath.Join(root, "internal", "authz") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// A FILE THAT NEVER SPELLS THE PACKAGE'S NAME HOLDS NO SELECTOR
		// INTO IT, so only the files that do are parsed.
		if !bytes.Contains(body, []byte("authz.")) {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, body, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "authz" {
				asked[sel.Sel.Name] = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if files < 100 {
		t.Fatalf("read %d Go files — the root is not this module's, so an "+
			"absence found here proves nothing", files)
	}
	dotted := 0
	for constant, verb := range wire {
		if !strings.Contains(verb, ".") {
			continue
		}
		dotted++
		if !asked[constant] {
			t.Errorf("%s (%q) is a dotted verb no non-test code asks, so its row "+
				"decides nothing — ask it where the gesture is decided, or delete it",
				constant, verb)
		}
	}
	if dotted == 0 {
		t.Fatal("no dotted verb was read, so this walk certifies nothing")
	}
}

// actionWireNames reads every `Action…` constant rules.go declares with an
// `Action` type and a string value, keyed by its Go name.
func actionWireNames(t *testing.T, path string) map[string]string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || len(value.Values) != 1 {
				continue
			}
			typ, ok := value.Type.(*ast.Ident)
			if !ok || typ.Name != "Action" {
				continue
			}
			lit, ok := value.Values[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				continue
			}
			verb, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: %v", value.Names[0].Name, err)
			}
			out[value.Names[0].Name] = verb
		}
	}
	return out
}
