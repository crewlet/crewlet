package node_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY TURN START IS ADMITTED BY THE NODE (ADR-0063).
//
// A turn runs in exactly two frames, [engine.Engine.runTurn] (a mailbox
// delivery's) and [engine.Engine.resumeTurn] (a suspended turn re-entered), and
// each is reachable only through a frame that holds the node's admission. This
// pins every edge of those two chains, so a new road to a turn fails here
// rather than in a drain:
//
//   - a mailbox delivery: node.runTurn admits ([node.Node.AdmitTurn]) and then
//     calls the configured Turn, which the engine sets to Engine.Dispatch and
//     nowhere else; the dispatcher's Turn is Engine.runTurn, installed in
//     buildDispatcher alone;
//   - a resume: Engine.resumeTurn is reached only from the engine's resumer,
//     whose Resume the sandbox coordinator calls only from resumeAndSettle,
//     which takes an admission no frame but admitTurn makes.
//
// A rule names the frames allowed to reach what it guards, as
// "<directory>.<Receiver>.<Function>", and each must still find what it
// guards, so a rename cannot leave it passing over nothing.
//
// Mutation: call turn.Run from a third frame, call the coordinator's resumer
// outside resumeAndSettle, mint an admission outside admitTurn, or take a slot
// outside AdmitTurn, and this names the frame.
func TestEveryTurnStartIsAdmitted(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	rules := admissionRules()
	found := make([]int, len(rules))
	fset := token.NewFileSet()
	err := sourcetree.Walk(filepath.Join(root, "internal"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(p))
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			frame := dir + "." + frameName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				for i, rule := range rules {
					if rule.dir != dir || !rule.reaches(n) {
						continue
					}
					found[i]++
					if !slices.Contains(rule.within, frame) {
						t.Errorf("%s: %s reaches %s, which only %v may: a turn started "+
							"there skips the node's admission (ADR-0063)",
							fset.Position(n.Pos()), frame, rule.what, rule.within)
					}
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, rule := range rules {
		if found[i] == 0 {
			t.Errorf("nothing in %s reaches %s any more: the rule guards nothing, and "+
				"the road it pinned has moved without it", rule.dir, rule.what)
		}
	}
}

// admissionRule is one edge of a chain to a turn: what in dir is reached, and
// the frames allowed to reach it.
type admissionRule struct {
	dir     string
	what    string
	reaches func(ast.Node) bool
	within  []string
}

func admissionRules() []admissionRule {
	return []admissionRule{
		{
			dir: "internal/engine", what: "turn.Run",
			reaches: selects(isIdent("turn"), "Run"),
			within:  []string{"internal/engine.Engine.runTurn", "internal/engine.Engine.resumeTurn"},
		},
		{
			dir: "internal/engine", what: "Engine.resumeTurn",
			reaches: selects(anyX, "resumeTurn"),
			within:  []string{"internal/engine.resumer.resume"},
		},
		{
			dir: "internal/engine", what: "Engine.runTurn",
			reaches: selects(anyX, "runTurn"),
			within:  []string{"internal/engine.Engine.buildDispatcher"},
		},
		{
			dir: "internal/engine", what: "the dispatcher's Dispatch",
			reaches: selects(func(x ast.Expr) bool {
				return isIdent("e")(x) || isSelector("dispatch")(x)
			}, "Dispatch"),
			within: []string{"internal/engine.Engine.Dispatch", "internal/engine.New"},
		},
		{
			dir: "internal/sandbox", what: "the coordinator's resumer",
			reaches: selects(isSelector("resume"), "Resume"),
			within:  []string{"internal/sandbox.Coordinator.resumeAndSettle"},
		},
		{
			dir: "internal/sandbox", what: "an admission",
			reaches: func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				return ok && isIdent("admitted")(lit.Type)
			},
			within: []string{"internal/sandbox.Coordinator.admitTurn"},
		},
		{
			dir: "internal/node", what: "a concurrency slot",
			reaches: selects(isSelector("turns"), "acquire"),
			within:  []string{"internal/node.Node.AdmitTurn"},
		},
		{
			dir: "internal/node", what: "the configured Turn",
			reaches: selects(isSelector("cfg"), "Turn"),
			within:  []string{"internal/node.Node.runTurn"},
		},
	}
}

// selects matches a selector naming sel on an operand x accepts.
func selects(x func(ast.Expr) bool, sel string) func(ast.Node) bool {
	return func(n ast.Node) bool {
		s, ok := n.(*ast.SelectorExpr)
		return ok && s.Sel.Name == sel && x(s.X)
	}
}

func anyX(ast.Expr) bool { return true }

func isIdent(name string) func(ast.Expr) bool {
	return func(x ast.Expr) bool {
		id, ok := x.(*ast.Ident)
		return ok && id.Name == name
	}
}

func isSelector(sel string) func(ast.Expr) bool {
	return func(x ast.Expr) bool {
		s, ok := x.(*ast.SelectorExpr)
		return ok && s.Sel.Name == sel
	}
}

// frameName is a function's name, behind its receiver's type for a method.
func frameName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	typ := fn.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// THE MAILBOX'S TURN IS ADMITTED BEFORE IT RUNS: node.runTurn asks
// [node.Node.AdmitTurn] before it calls the configured Turn. The rule above
// holds who may call that Turn; this holds what the one frame allowed to does
// first.
func TestTheMailboxTurnIsAdmittedBeforeItRuns(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "internal/node/node.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || frameName(fn) != "Node.runTurn" {
			continue
		}
		admitted, turned := token.NoPos, token.NoPos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch {
			case selects(anyX, "AdmitTurn")(n) && admitted == token.NoPos:
				admitted = n.Pos()
			case selects(isSelector("cfg"), "Turn")(n) && turned == token.NoPos:
				turned = n.Pos()
			}
			return true
		})
		if admitted == token.NoPos || turned == token.NoPos || admitted > turned {
			t.Fatal("Node.runTurn does not admit its turn (AdmitTurn) before it runs it (cfg.Turn)")
		}
		return
	}
	t.Fatal("internal/node/node.go has no Node.runTurn: the mailbox's road to a turn has moved")
}
