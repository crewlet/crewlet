package queries_test

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// A QUESTION CANNOT BE DECIDED BY A VERB THAT ASKS A PROOF OR A PERSON.
//
// Every question is answered over the dashboard's socket as well as over REST,
// and the socket can say neither `step_up_required` nor `token_refused` — the
// dashboard replays a step-up over REST alone — so a question decided by such a
// verb would be refused on the socket as a permission nobody could grant. The
// registration refuses it at wiring time, naming the verb and what it asks;
// a verb that asks neither still registers, which is what tells this case from
// a registry that panicked on every DecidedBy.
//
// Mutation: drop the clause from Posture.Valid and the registration's own
// check and both verbs register.
func TestAQuestionCannotBeDecidedByAVerbThatAsksAProofOrAPerson(t *testing.T) {
	t.Parallel()
	for _, verb := range []authz.Action{authz.ActionConfigWrite, authz.ActionSessionEnd} {
		t.Run(string(verb), func(t *testing.T) {
			t.Parallel()
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatalf("a question decided by %s registered", verb)
				}
				if said := fmt.Sprint(recovered); !strings.Contains(said, string(verb)) ||
					!strings.Contains(said, "socket") {
					t.Errorf("the registration's panic = %q, want it to name %s and "+
						"the socket that cannot carry the refusal", said, verb)
				}
			}()
			queries.NewRegistry().Register("q", queries.DecidedBy(verb), answersWith(nil))
		})
	}
	if !queries.DecidedBy(authz.ActionSeatTrailRead).Valid() ||
		queries.DecidedBy(authz.ActionConfigWrite).Valid() {
		t.Error("Posture.Valid does not tell a verb that asks a proof from one that asks none")
	}
	queries.NewRegistry().Register("q", queries.DecidedBy(authz.ActionSeatTrailRead),
		answersWith(nil))
}

// A REFUSAL CARRIES ITS DECISION WHOLE, AND ONLY A DECISION THAT REFUSED.
//
// Both transports answer a refusal through the authority table's one mapping,
// which reads the decision's reason, grants and window — so the refusal keeps
// all of it, a step-up's window included. And a decision that admitted, or one
// this node could not reach, is no refusal at all: built into one it would tell
// a reader holding the authority that they lack it, so NewRefusal panics.
//
// Mutation: copy only the reason and the grants into the refusal and the
// window is lost; drop the panic and the admitted decision becomes a refusal.
func TestARefusalCarriesTheDecisionThatMadeIt(t *testing.T) {
	t.Parallel()
	d := authz.Decision{Reason: authz.ReasonStepUp, Recency: iam.RecencyStepUp,
		Grants: []iam.Grant{iam.GrantAdmin}}
	refusal := queries.NewRefusal("config", d)
	if refusal.Reason != d.Reason || refusal.Recency != d.Recency ||
		len(refusal.Grants) != 1 || refusal.Grants[0] != iam.GrantAdmin {
		t.Errorf("the refusal carries %+v, want the decision %+v whole", refusal.Decision, d)
	}
	if !errors.Is(refusal, queries.ErrUnauthorized) {
		t.Error("a refusal is not ErrUnauthorized to errors.Is")
	}
	for name, d := range map[string]authz.Decision{
		"an admission":            {Allowed: true, Reason: authz.ReasonGrant},
		"a decision nobody could": {Err: errors.New("the chart is not built yet")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("%s was built into a refusal", name)
				}
			}()
			queries.NewRefusal("q", d)
		})
	}
}

// NEWREFUSAL IS THE ONE WAY A REFUSAL IS BUILT.
//
// Its panic is what keeps a refusal that refused nothing off the wire, and a
// composite literal — `queries.Refusal{...}` or, inside the package, a bare
// `Refusal{...}` — or a `new(Refusal)` would step round it. So this walks
// every Go file of the module, tests included, and refuses each such
// construction outside NewRefusal's own body, under whatever name a file
// imports the package as.
//
// Mutation: build the registry's stage refusal as a literal again and this
// goes red naming queries.go; skip the test files and the floor of files read
// still holds, so add `_ = queries.Refusal{}` to a test and it goes red too.
func TestARefusalIsBuiltOnlyByNewRefusal(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	const self = "github.com/crewlet/crewlet/internal/api/queries"
	home := filepath.Join(root, "internal", "api", "queries")
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
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		files++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(body, []byte("Refusal")) {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, body, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// WHICH NAME THE TYPE GOES BY in this file: bare inside the
		// package's own directory, and through the import elsewhere.
		names := func(expr ast.Expr) bool { return false }
		if filepath.Dir(path) == home && file.Name.Name == "queries" {
			names = func(expr ast.Expr) bool {
				id, ok := expr.(*ast.Ident)
				return ok && id.Name == "Refusal"
			}
		} else {
			for _, spec := range file.Imports {
				if imported, _ := strconv.Unquote(spec.Path.Value); imported != self {
					continue
				}
				as := "queries"
				if spec.Name != nil {
					as = spec.Name.Name
				}
				names = func(expr ast.Expr) bool {
					sel, ok := expr.(*ast.SelectorExpr)
					if !ok || sel.Sel.Name != "Refusal" {
						return false
					}
					pkg, ok := sel.X.(*ast.Ident)
					return ok && pkg.Name == as
				}
			}
		}
		for _, decl := range file.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil &&
				fn.Name.Name == "NewRefusal" && filepath.Dir(path) == home {
				continue
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				built := false
				switch n := n.(type) {
				case *ast.CompositeLit:
					built = names(n.Type)
				case *ast.CallExpr:
					if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "new" && len(n.Args) == 1 {
						built = names(n.Args[0])
					}
				}
				if built {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s:%d builds a queries.Refusal itself — build it with "+
						"NewRefusal, which refuses a decision that refused nothing",
						filepath.ToSlash(rel), fset.Position(n.Pos()).Line)
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	if files < 500 {
		t.Fatalf("read %d Go files — the root is not this module's, so an absence "+
			"found here proves nothing", files)
	}
}
