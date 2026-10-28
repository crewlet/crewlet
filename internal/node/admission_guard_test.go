package node_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
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
//     calls the configured Turn, which the engine sets to its unexported
//     mailboxTurn, in New and nowhere else; mailboxTurn is the one frame that
//     enters the dispatcher, whose Turn is Engine.runTurn, installed in
//     buildDispatcher alone;
//   - a resume: Engine.resumeTurn is reached only from the engine's resumer,
//     whose Resume the sandbox coordinator reads only in resumeAndSettle,
//     which takes an admission no frame but admitTurn makes, and which is
//     called only from the two frames that claim under one
//     (completeAdmitted, resumeOwedAdmitted), each called only from the frame
//     that asked the gate (OnCompleted, attemptOwed).
//
// # What it reads, and what it cannot
//
// It is SYNTACTIC: it parses every non-test Go file in the module
// ([sourcetree.Walk] from the module root, so the root package and any new
// top-level directory are read too) and matches identifiers and selectors,
// never types. So each rule is written to survive the moves that defeat a name
// match. turn.Run is matched through whatever name the file imports the turn
// package under (an alias, a dot import, which is refused outright), in every
// package, at package scope as in a function. Every other edge is an unexported
// name matched on ANY operand, so a copy of the receiver (`eng := e`) or of the
// field (`d := e.dispatch`) reaches the same rule. The admission type is
// matched on ANY MENTION, in a signature as in a body (a declared variable,
// new, a conversion, an alias, a field), because its zero value assigned field
// by field is an admission as surely as a literal is. What it cannot see is a
// value that carries a guarded function out of the frames it pins (a method
// value stored and called later from elsewhere) or a call through reflection.
// Review holds those; the ADR says so.
//
// A rule names the frames allowed to reach what it guards, as
// "<directory>.<Receiver>.<Function>", and each must still find what it
// guards, so a rename cannot leave it passing over nothing.
//
// Mutation: call turn.Run from a third frame or another package, under any
// import name; reach mailboxTurn, the dispatcher's Dispatch or the coordinator's
// resumer outside their frames, through a copy of the receiver or not; mint an
// admission outside admitTurn, or take a slot outside AdmitTurn, and this names
// the frame. TestTheAdmissionGuardFindsEveryRoad makes each of those moves.
func TestEveryTurnStartIsAdmitted(t *testing.T) {
	t.Parallel()
	rules := admissionRules()
	report, err := scanAdmission(sourcetree.Root(t), rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range report.violations {
		t.Error(v)
	}
	for i, rule := range rules {
		if report.found[i] == 0 {
			t.Errorf("nothing reaches %s any more: the rule guards nothing, and the road "+
				"it pinned has moved without it", rule.what)
		}
	}
}

// THE GUARD FAILS ON EVERY ROAD IT CLAIMS TO CLOSE.
//
// Each case writes one file into an otherwise empty tree and scans it: a road
// is a violation naming its frame, and a file that only looks like one is not.
// The first three are the roads a name match let through: an aliased import of
// the turn package, turn.Run from a package no rule scanned, and a copy of the
// engine that a match on the identifier `e` did not recognise.
//
// Mutation: match turn.Run on the identifier `turn` again, scan only the
// rules' own directories, or match a rule's selector only on a named operand,
// and the case for that road passes with no violation.
func TestTheAdmissionGuardFindsEveryRoad(t *testing.T) {
	t.Parallel()
	const turnImport = `"github.com/crewlet/crewlet/internal/agent/turn"`
	for name, tc := range map[string]struct {
		path, src string
		// frame is the frame the violation must name; empty, the file is
		// not a road and must produce none.
		frame string
	}{
		"turn.Run under an aliased import": {
			path: "internal/engine/road.go",
			src: "package engine\nimport agentturn " + turnImport + "\n" +
				"func (e *Engine) sneak() { agentturn.Run(nil, agentturn.Phases{}, agentturn.Settings{}, agentturn.Input{}) }",
			frame: "internal/engine.Engine.sneak",
		},
		"turn.Run from a new package": {
			path: "internal/shortcut/road.go",
			src: "package shortcut\nimport " + turnImport + "\n" +
				"func Start() { turn.Run(nil, turn.Phases{}, turn.Settings{}, turn.Input{}) }",
			frame: "internal/shortcut.Start",
		},
		"turn.Run from the binary": {
			path:  "cmd/crewlet/road.go",
			src:   "package main\nimport " + turnImport + "\nfunc start() { turn.Run(nil, turn.Phases{}, turn.Settings{}, turn.Input{}) }",
			frame: "cmd/crewlet.start",
		},
		"turn.Run held at package scope": {
			path:  "internal/shortcut/road.go",
			src:   "package shortcut\nimport " + turnImport + "\nvar start = turn.Run",
			frame: "internal/shortcut.package scope",
		},
		"the turn package dot-imported": {
			path:  "internal/shortcut/road.go",
			src:   "package shortcut\nimport . " + turnImport + "\nfunc Start() { Run(nil, Phases{}, Settings{}, Input{}) }",
			frame: "internal/shortcut.package scope",
		},
		"Run called inside the turn package": {
			path:  "internal/agent/turn/road.go",
			src:   "package turn\nfunc again() { Run(nil, Phases{}, Settings{}, Input{}) }",
			frame: "internal/agent/turn.again",
		},
		"the mailbox's turn through a copy of the engine": {
			path:  "internal/engine/road.go",
			src:   "package engine\nfunc (e *Engine) sneak() { eng := e; eng.mailboxTurn(nil, \"swe\", nil) }",
			frame: "internal/engine.Engine.sneak",
		},
		"the dispatcher through a copy of the field": {
			path:  "internal/engine/road.go",
			src:   "package engine\nfunc (e *Engine) sneak() { d := e.dispatch; d.Dispatch(nil, \"swe\", nil) }",
			frame: "internal/engine.Engine.sneak",
		},
		"the dispatcher's inner turn": {
			path:  "internal/engine/road.go",
			src:   "package engine\nfunc (e *Engine) sneak() { e.dispatch.dispatch(nil, \"swe\", nil, nil) }",
			frame: "internal/engine.Engine.sneak",
		},
		"the coordinator's resumer through a copy": {
			path:  "internal/sandbox/road.go",
			src:   "package sandbox\nfunc (c *Coordinator) sneak() { r := c.resume; r.Resume(nil, ResumeRequest{}) }",
			frame: "internal/sandbox.Coordinator.sneak",
		},
		"the configured Turn through a copy of the config": {
			path:  "internal/node/road.go",
			src:   "package node\nfunc (n *Node) sneak() { cfg := n.cfg; cfg.Turn(nil, \"swe\", nil) }",
			frame: "internal/node.Node.sneak",
		},
		"a slot through a copy of the gate": {
			path:  "internal/node/road.go",
			src:   "package node\nfunc (n *Node) sneak() { g := n.turns; g.acquire(nil) }",
			frame: "internal/node.Node.sneak",
		},
		"turn.Run from the module's root package": {
			path:  "reference.go",
			src:   "package crewlet\nimport " + turnImport + "\nvar Hook = turn.Run",
			frame: moduleRoot + ".package scope",
		},
		"an admission declared and filled in": {
			path: "internal/sandbox/road.go",
			src: "package sandbox\nfunc (c *Coordinator) sneak(ctx context.Context) " +
				"{ var a admitted; a.ctx = ctx; _ = a }",
			frame: "internal/sandbox.Coordinator.sneak",
		},
		"a completion's claim from another frame": {
			path:  "internal/sandbox/road.go",
			src:   "package sandbox\nfunc (c *Coordinator) sneak() { c.completeAdmitted }",
			frame: "internal/sandbox.Coordinator.sneak",
		},
		"an admission from new": {
			path:  "internal/sandbox/road.go",
			src:   "package sandbox\nfunc (c *Coordinator) sneak() { a := *new(admitted); _ = a }",
			frame: "internal/sandbox.Coordinator.sneak",
		},
		"an admission through an alias": {
			path:  "internal/sandbox/road.go",
			src:   "package sandbox\ntype adm2 = admitted\nfunc (c *Coordinator) sneak() { _ = adm2{} }",
			frame: "internal/sandbox.package scope",
		},
		"an admission taken by another frame": {
			path:  "internal/sandbox/road.go",
			src:   "package sandbox\nfunc (c *Coordinator) sneak(adm admitted) {}",
			frame: "internal/sandbox.Coordinator.sneak",
		},
		"the resume called from another frame": {
			path:  "internal/sandbox/road.go",
			src:   "package sandbox\nfunc (c *Coordinator) sneak() { c.resumeAndSettle }",
			frame: "internal/sandbox.Coordinator.sneak",
		},
		"another package's Run named turn": {
			path:  "internal/shortcut/road.go",
			src:   "package shortcut\nimport turn \"example.com/other/turn\"\nfunc Start() { turn.Run() }",
			frame: "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			file := filepath.Join(root, filepath.FromSlash(tc.path))
			if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(tc.src), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err := scanAdmission(root, admissionRules())
			if err != nil {
				t.Fatal(err)
			}
			if tc.frame == "" {
				if len(report.violations) != 0 {
					t.Fatalf("a file that is no road was reported: %v", report.violations)
				}
				return
			}
			if !slices.ContainsFunc(report.violations, func(v string) bool {
				return strings.Contains(v, ": "+tc.frame+" reaches ")
			}) {
				t.Fatalf("the guard let the road through: no violation names %s (got %v)",
					tc.frame, report.violations)
			}
		})
	}
}

// turnPackage is the turn loop's import path, and turnDir its directory: the
// one package whose Run starts a turn.
const (
	turnPackage = "github.com/crewlet/crewlet/internal/agent/turn"
	turnDir     = "internal/agent/turn"
)

// moduleRoot is how a frame in the module's root package is named.
const moduleRoot = "(module root)"

// admissionReport is what a scan found: every reach outside its rule's frames,
// and how often each rule matched at all.
type admissionReport struct {
	violations []string
	found      []int
}

// scanAdmission reads every non-test Go file in the module at root against
// rules: the whole tree, as [sourcetree.Walk] defines it, less testdata and
// hidden directories.
func scanAdmission(root string, rules []admissionRule) (admissionReport, error) {
	report := admissionReport{found: make([]int, len(rules))}
	fset := token.NewFileSet()
	err := sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); p != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
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
		if dir == "." {
			dir = moduleRoot
		}
		parsed, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		report.scan(fset, sourceFile{dir: dir, imports: importNames(parsed)}, parsed, rules)
		return nil
	})
	return report, err
}

// scan checks one file's declarations against rules: a function's signature
// and body as the function's frame, and everything else (imports, types,
// package-level values) as the package's scope, which no rule allows.
func (r *admissionReport) scan(fset *token.FileSet, file sourceFile, parsed *ast.File, rules []admissionRule) {
	for _, decl := range parsed.Decls {
		parts := []ast.Node{decl}
		frame := file.dir + ".package scope"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			parts, frame = []ast.Node{fn.Type}, file.dir+"."+frameName(fn)
			if fn.Body != nil {
				parts = append(parts, fn.Body)
			}
		}
		for _, part := range parts {
			r.check(fset, file, frame, part, rules)
		}
	}
}

// check holds every node under part, in frame, to rules.
func (r *admissionReport) check(fset *token.FileSet, file sourceFile, frame string, part ast.Node,
	rules []admissionRule,
) {
	withParents(part, func(n, parent ast.Node) {
		at := site{node: n, parent: parent, file: file}
		for i, rule := range rules {
			if (rule.dir != "" && rule.dir != file.dir) || !rule.reaches(at) {
				continue
			}
			r.found[i]++
			if !slices.Contains(rule.within, frame) {
				r.violations = append(r.violations, fmt.Sprintf("%s: %s reaches %s, which only "+
					"%v may: a turn started there skips the node's admission (ADR-0063)",
					fset.Position(n.Pos()), frame, rule.what, rule.within))
			}
		}
	})
}

// sourceFile is what a rule may ask about the file a node is in.
type sourceFile struct {
	// dir is the file's directory, relative to the module root.
	dir string
	// imports maps each path the file imports to the name the file uses for
	// it: its alias, "." or "_", or its last element.
	imports map[string]string
}

// site is one node of a file, with the node that encloses it.
type site struct {
	node, parent ast.Node
	file         sourceFile
}

// admissionRule is one edge of a chain to a turn: what is reached, in dir (or
// in every package scanned, for an empty dir), and the frames allowed to reach
// it.
type admissionRule struct {
	dir     string
	what    string
	reaches func(site) bool
	within  []string
}

func admissionRules() []admissionRule {
	return []admissionRule{
		{
			what: "turn.Run", reaches: reachesTurnRun,
			within: []string{"internal/engine.Engine.runTurn", "internal/engine.Engine.resumeTurn"},
		},
		{
			dir: "internal/engine", what: "Engine.resumeTurn",
			reaches: selects("resumeTurn"),
			within:  []string{"internal/engine.resumer.resume"},
		},
		{
			dir: "internal/engine", what: "Engine.runTurn",
			reaches: selects("runTurn"),
			within:  []string{"internal/engine.Engine.buildDispatcher"},
		},
		{
			dir: "internal/engine", what: "Engine.mailboxTurn",
			reaches: selects("mailboxTurn"),
			within:  []string{"internal/engine.New"},
		},
		{
			// buildDispatcher reads the Options field of the same name,
			// the dispatcher it is building: the frame that installs
			// runTurn in it, and the one other frame the name may appear in.
			dir: "internal/engine", what: "the dispatcher's Dispatch",
			reaches: selects("Dispatch"),
			within:  []string{"internal/engine.Engine.mailboxTurn", "internal/engine.Engine.buildDispatcher"},
		},
		{
			// CALLED, because the name is also the engine's field that
			// holds the dispatcher, which other frames read for its
			// bookkeeping; a call can only be the method.
			dir: "internal/engine", what: "the dispatcher's inner dispatch",
			reaches: called("dispatch"),
			within:  []string{"internal/engine.Dispatcher.Dispatch"},
		},
		{
			// READ, NOT CALLED, because the name is also a method of the
			// local box's; reading the field is what any use of the
			// resumer starts with.
			dir: "internal/sandbox", what: "the coordinator's resumer",
			reaches: read("resume"),
			within:  []string{"internal/sandbox.Coordinator.resumeAndSettle"},
		},
		{
			dir: "internal/sandbox", what: "an admission",
			reaches: func(s site) bool {
				lit, ok := s.node.(*ast.CompositeLit)
				return ok && isIdent("admitted")(lit.Type)
			},
			within: []string{"internal/sandbox.Coordinator.admitTurn"},
		},
		{
			// ANY MENTION of the type but its own declaration: a variable
			// of it filled in field by field, new(admitted), a conversion,
			// an alias or a field holding one is an admission no gate
			// granted as surely as a literal is. The four frames are the one
			// that makes it and the three that take it.
			dir: "internal/sandbox", what: "the admission type",
			reaches: mentionsType("admitted"),
			within: []string{"internal/sandbox.Coordinator.admitTurn",
				"internal/sandbox.Coordinator.resumeAndSettle",
				"internal/sandbox.Coordinator.completeAdmitted",
				"internal/sandbox.Coordinator.resumeOwedAdmitted"},
		},
		{
			dir: "internal/sandbox", what: "the resume under an admission",
			reaches: selects("resumeAndSettle"),
			within: []string{"internal/sandbox.Coordinator.completeAdmitted",
				"internal/sandbox.Coordinator.resumeOwedAdmitted"},
		},
		{
			dir: "internal/sandbox", what: "a completion's claim under an admission",
			reaches: selects("completeAdmitted"),
			within:  []string{"internal/sandbox.Coordinator.OnCompleted"},
		},
		{
			dir: "internal/sandbox", what: "an owed answer's claim under an admission",
			reaches: selects("resumeOwedAdmitted"),
			within:  []string{"internal/sandbox.Coordinator.attemptOwed"},
		},
		{
			dir: "internal/node", what: "a concurrency slot",
			reaches: selects("acquire"),
			within:  []string{"internal/node.Node.AdmitTurn"},
		},
		{
			// ANY USE but New's check that it was set.
			dir: "internal/node", what: "the configured Turn",
			reaches: func(s site) bool { return selects("Turn")(s) && !comparedToNil(s) },
			within:  []string{"internal/node.Node.runTurn"},
		},
	}
}

// reachesTurnRun matches every way a file can name the turn loop's Run: through
// whatever name it imports the package under, a dot import of it (refused
// outright, since what it imports is then unqualified), and an unqualified Run
// inside the package itself.
func reachesTurnRun(s site) bool {
	switch n := s.node.(type) {
	case *ast.ImportSpec:
		path, err := strconv.Unquote(n.Path.Value)
		return err == nil && path == turnPackage && n.Name != nil && n.Name.Name == "."
	case *ast.SelectorExpr:
		name, ok := s.file.imports[turnPackage]
		return ok && name != "." && name != "_" && n.Sel.Name == "Run" && isIdent(name)(n.X)
	case *ast.Ident:
		if s.file.dir != turnDir || n.Name != "Run" {
			return false
		}
		switch p := s.parent.(type) {
		case *ast.SelectorExpr:
			return p.Sel != n
		case *ast.KeyValueExpr:
			return p.Key != n
		case *ast.Field, *ast.FuncDecl:
			return false
		}
		return true
	}
	return false
}

// mentionsType matches an identifier naming the type, anywhere but the name
// in its own declaration, a selector's field or method name, or a composite
// literal's key.
func mentionsType(name string) func(site) bool {
	return func(s site) bool {
		id, ok := s.node.(*ast.Ident)
		if !ok || id.Name != name {
			return false
		}
		switch p := s.parent.(type) {
		case *ast.TypeSpec:
			return p.Name != id
		case *ast.SelectorExpr:
			return p.Sel != id
		case *ast.KeyValueExpr:
			return p.Key != id
		}
		return true
	}
}

// importNames maps each path file imports to the name it is used under.
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		if spec.Name != nil {
			names[path] = spec.Name.Name
			continue
		}
		names[path] = path[strings.LastIndex(path, "/")+1:]
	}
	return names
}

// withParents visits every node under root with the node that encloses it.
func withParents(root ast.Node, visit func(n, parent ast.Node)) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		visit(n, parent)
		stack = append(stack, n)
		return true
	})
}

// selects matches a selector naming sel, on any operand.
func selects(sel string) func(site) bool {
	return func(s site) bool {
		x, ok := s.node.(*ast.SelectorExpr)
		return ok && x.Sel.Name == sel
	}
}

// called matches a selector naming sel that is the function of a call.
func called(sel string) func(site) bool {
	return func(s site) bool {
		call, ok := s.parent.(*ast.CallExpr)
		return selects(sel)(s) && ok && call.Fun == s.node
	}
}

// read matches a selector naming sel that is not the function of a call.
func read(sel string) func(site) bool {
	return func(s site) bool {
		return selects(sel)(s) && !called(sel)(s)
	}
}

// comparedToNil reports whether the node is one side of a comparison with nil.
func comparedToNil(s site) bool {
	bin, ok := s.parent.(*ast.BinaryExpr)
	return ok && (bin.Op == token.EQL || bin.Op == token.NEQ) &&
		(isIdent("nil")(bin.X) || isIdent("nil")(bin.Y))
}

func isIdent(name string) func(ast.Expr) bool {
	return func(x ast.Expr) bool {
		id, ok := x.(*ast.Ident)
		return ok && id.Name == name
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
		withParents(fn.Body, func(n, parent ast.Node) {
			at := site{node: n, parent: parent}
			switch {
			case selects("AdmitTurn")(at) && admitted == token.NoPos:
				admitted = n.Pos()
			case selects("Turn")(at) && turned == token.NoPos:
				turned = n.Pos()
			}
		})
		if admitted == token.NoPos || turned == token.NoPos || admitted > turned {
			t.Fatal("Node.runTurn does not admit its turn (AdmitTurn) before it runs it (cfg.Turn)")
		}
		return
	}
	t.Fatal("internal/node/node.go has no Node.runTurn: the mailbox's road to a turn has moved")
}
