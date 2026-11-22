package queue

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// answerDecl is what one Serve registration's answers are to the node's answer
// budget, exactly one of two: a payload answer that acquires (kind, in the
// package in, which may be below the registering one, as a box read's
// collection is), or a small answer the count alone bounds (small: its bound
// and where it is decided).
type answerDecl struct {
	kind  string
	in    string
	small string
}

// answerDecls is every Serve registration outside this package, by its
// package directory and its subject as written: the sweep below fails one it
// does not name, one it names that is gone, and a payload one whose package
// never acquires its kind. A NEW REGISTRATION IS DECLARED HERE, payload or
// small; there is no list of registrations the sweep passes over.
var answerDecls = map[string]answerDecl{
	"internal/engine|topics.SandboxBoxRead":            {kind: "AnswerBoxRead", in: "internal/sandbox/executor"},
	"internal/eventfan|Subject":                        {kind: "AnswerHistory", in: "internal/eventfan"},
	"internal/learning/memread|topics.HeldRead":        {kind: "AnswerMemory", in: "internal/learning/memread"},
	"internal/assistant/live|s.subject":                {kind: "AnswerReplay", in: "internal/assistant/live"},
	"internal/engine|liveRelaySubject(r.self)":         {kind: "AnswerLiveRelay", in: "internal/engine"},
	"internal/httprelay|nodeSubject(rl.name, rl.node)": {kind: "AnswerBridge", in: "internal/httprelay"},
	"internal/httprelay|exchangeSubject(rl.name, id)":  {kind: "AnswerBridge", in: "internal/httprelay"},
	"internal/sandbox|topics.ObserveExecutorTail":      {kind: "AnswerTail", in: "internal/sandbox"},
	"internal/engine|topics.CodeSearch":                {kind: "AnswerCodeSearch", in: "internal/codesearch"},
	"internal/search|SliceSubject":                     {kind: "AnswerSlice", in: "internal/search"},
	"internal/engine|topics.SeatSteer": {small: "one note's verdict: ids, a status and the keys a node " +
		"holds (internal/agent/steer's Reply), under 4 KiB"},
	"internal/engine|fleetBrokerSubject(e.id)": {small: "the broker's meta group, a row a cluster member " +
		"(jetstream.MetaGroup), a few hundred bytes a member"},
	"internal/engine|topics.HeldRelaunch": {small: "a target a session asked about (relaunchTargetReply, " +
		"a few short fields), no more rows than the request named"},
	"internal/engine|topics.ObserveMCPInstances": {small: "a row an MCP instance the node runs " +
		"(coord.MCPInstance), its failure and wait each held to coord.MaxMCPErrorBytes"},
	"internal/engine|topics.MCPCheck": {kind: "AnswerMCPCheck", in: "internal/engine"},
	"internal/engine|topics.CodeStatus": {small: "a node's cache status and its cache.HotRepos hottest " +
		"repositories (host.CodeStatus)"},
	"internal/engine|topics.HeldPark": {small: "whether a park holds a seat's inbox (heldParkReply), " +
		"under 1 KiB"},
	"internal/engine|topics.CodeHolds": {small: "which of the asked keys a running turn holds " +
		"(codeHoldsAnswer), no more than the request named"},
	"internal/presence|topics.PresenceAsk": {small: "the people of one room on this node, a name and a " +
		"place each (presence's state frame)"},
	// EVERY ESTATE LANE: a page or a capped list acquires by its declared
	// shape; one value and an entry per key asked are small, the first held
	// to its log's record cap where written, the second to the request
	// (internal/estate's answerShape.weight).
	"internal/estate|l.subject(opts.Self)": {kind: "AnswerEstate", in: "internal/estate"},
}

// answerKindNames is every kind by the name a declaration gives it.
var answerKindNames = map[string]AnswerKind{
	"AnswerBoxRead": AnswerBoxRead, "AnswerHistory": AnswerHistory, "AnswerMemory": AnswerMemory,
	"AnswerReplay": AnswerReplay, "AnswerLiveRelay": AnswerLiveRelay, "AnswerBridge": AnswerBridge,
	"AnswerTail": AnswerTail, "AnswerCodeSearch": AnswerCodeSearch, "AnswerSlice": AnswerSlice,
	"AnswerMCPCheck": AnswerMCPCheck, "AnswerEstate": AnswerEstate,
}

// EVERY SERVE REGISTRATION IS DECLARED AGAINST THE NODE'S ANSWER BUDGET: a
// payload answer acquires its kind in the package that reads it, a small one
// states the bound that keeps it small, and nothing registers unnamed. A
// registration is a call X.Serve(ctx, subject, answers, handler) in a file
// importing this package, outside this package's own tree (the backends and
// the suite).
//
// Mutations, each red: drop an acquire from a payload package; register a
// subject nobody declared; declare a registration that is gone; leave a kind
// no declaration uses.
func TestEveryAnswerRegistrationIsDeclaredAgainstTheBudget(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := map[string]string{}
	acquires := map[string]map[string]bool{}
	fset := token.NewFileSet()
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "internal/queue" || strings.HasPrefix(dir, "internal/queue/") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if !importsQueue(file) {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "Serve" && len(call.Args) == 4:
				found[dir+"|"+exprText(fset, call.Args[1])] = fset.Position(call.Pos()).String()
			case sel.Sel.Name == "Acquire" && len(call.Args) == 3:
				if kind, ok := call.Args[1].(*ast.SelectorExpr); ok {
					if x, ok := kind.X.(*ast.Ident); ok && x.Name == "queue" {
						if acquires[dir] == nil {
							acquires[dir] = map[string]bool{}
						}
						acquires[dir][kind.Sel.Name] = true
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) == 0 {
		t.Fatal("found no Serve registration anywhere, so this sweep would pass on nothing")
	}
	used := map[string]bool{}
	for key, at := range found {
		d, declared := answerDecls[key]
		switch {
		case !declared:
			t.Errorf("%s registers %s, which answerDecls does not declare: name its payload kind and the "+
				"package that acquires it, or the bound that keeps its answers small", at, key)
		case d.kind != "":
			used[d.kind] = true
			if _, ok := answerKindNames[d.kind]; !ok {
				t.Errorf("%s is declared to acquire %s, which is no AnswerKind", key, d.kind)
			} else if !acquires[d.in][d.kind] {
				t.Errorf("%s is a payload registration, and %s never acquires queue.%s", key, d.in, d.kind)
			}
		}
	}
	for key, d := range answerDecls {
		if _, ok := found[key]; !ok {
			t.Errorf("answerDecls declares %s, which no Serve registers any more: drop it", key)
		}
		if n := len(slices.DeleteFunc([]string{d.kind, d.small},
			func(s string) bool { return s == "" })); n != 1 || (d.kind != "") != (d.in != "") {
			t.Errorf("%s declares %+v: give it exactly one of a kind with its package or a small bound", key, d)
		}
	}
	for name, kind := range answerKindNames {
		if !kind.Valid() {
			t.Errorf("answerKindNames maps %s to %q, which AnswerKinds does not hold", name, kind)
		}
		if !used[name] {
			t.Errorf("no declaration acquires %s: a kind nobody holds is a meter nobody reads", name)
		}
	}
	if len(answerKindNames) != len(AnswerKinds) {
		t.Errorf("answerKindNames names %d kinds and AnswerKinds holds %d: name every kind", len(answerKindNames),
			len(AnswerKinds))
	}
}

// importsQueue reports whether file imports this package.
func importsQueue(file *ast.File) bool {
	return slices.ContainsFunc(file.Imports, func(spec *ast.ImportSpec) bool {
		return spec.Path.Value == `"github.com/crewlet/crewlet/internal/queue"`
	})
}

// exprText is an expression as its source writes it.
func exprText(fset *token.FileSet, e ast.Expr) string {
	var b strings.Builder
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}
