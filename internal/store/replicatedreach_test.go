package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ONLY THE RUNTIME REACHES THE REPLICATED ESTATE.
//
// A node without the `data` role holds no replicated estate at all — no file,
// nothing open — and a tool, a reader or an operator surface runs on every
// kind of node. So a caller that reaches for the estate through the node's own
// store — opens it, looks it up, builds a handle to it, finds its file — is a
// caller assuming this node holds it, and that assumption is right on a data
// node and wrong on every node without `data`. What reaches the estate is the
// RUNTIME that opened it, handing it to the domains that apply and read it
// there; every other reader is handed what the runtime gives it, or asks a
// node that holds the estate through the estate router (internal/estate).
//
// The exported methods that reach it are a family the compiler lets any
// package call. This walk is what keeps the family to the list below.
//
// # What it reports
//
// Every call in a non-test file under internal/ or cmd/ to one of the store's
// replicated-estate methods — `OpenReplicated`, `CloseReplicated`,
// `ReplicatedDB`, `Replicated`, `ReplicatedFile` — on a receiver that is not
// an imported package, and every call through the store package itself that
// names the replicated estate's file: `ReplicatedPath` and `OpenEstate` of
// `EstateReplicated`. Matching is on the name, as every gate in this package
// matches; the one name it trusts is an import's.
//
// # The allowance is two-sided
//
// An entry that matches no call FAILS, for the reason the applier gate's do.
func TestOnlyTheRuntimeReachesTheReplicatedEstate(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN.
	for _, positive := range []string{
		`node.OpenReplicated(ctx, logs)`,
		`s.db.CloseReplicated()`,
		`db, err := node.ReplicatedDB()`,
		`r := node.Replicated().Reader()`,
		`path := node.ReplicatedFile()`,
		`path := store.ReplicatedPath(boot.Store.Path, "")`,
		`db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})`,
	} {
		if !reachesTheReplicatedEstate(t, positive) {
			t.Errorf("control: %q reaches the replicated estate and the matcher "+
				"did not flag it", positive)
		}
	}
	for _, negative := range []string{
		`db, err := store.OpenEstate(ctx, store.EstateNode, path, store.Options{})`,
		`node, err := store.OpenNode(ctx, path, store.Options{})`,
		`err := h.Read(ctx, fn)`,
		`db := storetest.ReplicatedDB(t, h)`,
		`db := c.ReplicatedDB(t, i)`,
		`n := view.Replicated(stream)`,
	} {
		if reachesTheReplicatedEstate(t, negative) {
			t.Errorf("control: %q does not reach the replicated estate and the "+
				"matcher flagged it", negative)
		}
	}

	files := moduleTree(t)
	var found []site
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			if why, ok := replicatedReachAt(n, f.names); ok {
				found = append(found, site{
					File: f.rel, Line: f.fset.Position(n.Pos()).Line, Why: why,
				})
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatal("found no call reaching the replicated estate at all, not even the " +
			"runtime's own — so this guard is watching for a shape nobody " +
			"writes. Fix the walk")
	}

	used := map[string]bool{}
	var unexpected []site
	for _, s := range found {
		if prefix, ok := reachAllowanceFor(s.File); ok {
			used[prefix] = true
			continue
		}
		unexpected = append(unexpected, s)
	}
	slices.SortFunc(unexpected, func(a, b site) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	for _, s := range unexpected {
		t.Errorf("%s:%d reaches the replicated estate through the node's own "+
			"store (%s).\n"+
			"\tA node without `data` holds no replicated estate, so nothing but "+
			"the runtime that opened it may reach for it: a domain is handed the "+
			"estate by internal/engine, and every other reader asks a node that "+
			"holds it through the estate router. If this caller genuinely owns "+
			"the estate's FILE — the way a backup or an operator's migration "+
			"does — add it to allowedReplicatedReach with the reason.",
			s.File, s.Line, s.Why)
	}
	for _, a := range allowedReplicatedReach {
		if !used[a.prefix] {
			t.Errorf("%s is allowed to reach the replicated estate and does not — "+
				"the reason on file is %q. Delete the entry", a.prefix, a.why)
		}
	}
	t.Logf("parsed %d files; %d call(s) reach the replicated estate across %d allowance(s)",
		len(files), len(found), len(allowedReplicatedReach))
}

// allowedReplicatedReach is every file that may reach the replicated estate
// through the node's own store, and why. Keyed and matched as
// [allowedReplicatedWriter] is.
var allowedReplicatedReach = []struct{ prefix, why string }{
	{"internal/store/", "The replicated estate's owner, and its test helpers."},
	{"internal/engine/", "THE RUNTIME: it opens the replicated estate on a data " +
		"node and hands it to the domains that apply and read it there — and it " +
		"is the one package that knows whether this node holds it at all."},
	{"internal/statelog/", "The framework's own copies of the estate — the " +
		"adoption verifying a fetched artefact before it is renamed over the " +
		"live file, and the read of a copy's cursor."},
	{"internal/backup/", "Copies the replicated estate's FILE, and reads a " +
		"restored one's object references."},
	{"cmd/crewlet/ops.go", "`crewlet migrate` opens the replicated estate beside " +
		"the node's own file on a stopped node, which is how an operator's " +
		"migration reaches it."},
	{"cmd/crewlet/search.go", "`crewlet search eval` reads the replicated " +
		"estate's FILE on a stopped node, offline, as an operator's tool."},
}

// reachAllowanceFor reports which entry covers a file; the longest prefix
// wins, so a file-specific entry is never shadowed by a package-wide one.
func reachAllowanceFor(file string) (string, bool) {
	best, found := "", false
	for _, a := range allowedReplicatedReach {
		p := filepath.FromSlash(a.prefix)
		if strings.HasPrefix(file, p) && len(p) > len(best) {
			best, found = a.prefix, true
		}
	}
	return best, found
}

// replicatedReachMethods are the store's replicated-estate methods, with the
// argument count each takes: the count is what tells the store's
// `ReplicatedDB()` from a same-named method of another type the walk cannot
// resolve — a test helper's `ReplicatedDB(t, h)` among them.
var replicatedReachMethods = map[string]int{
	"OpenReplicated":  2,
	"CloseReplicated": 0,
	"ReplicatedDB":    0,
	"Replicated":      0,
	"ReplicatedFile":  0,
}

// replicatedReachAt reports whether one node is a call reaching the replicated
// estate.
func replicatedReachAt(n ast.Node, f fileNames) (string, bool) {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	name := sel.Sel.Name
	if id, isIdent := sel.X.(*ast.Ident); isIdent {
		if _, isPkg := f.imports[id.Name]; isPkg {
			if f.store == "" || id.Name != f.store {
				return "", false
			}
			switch name {
			case "ReplicatedPath":
				return "store." + name + " names the replicated estate's file", true
			case "OpenEstate":
				for _, arg := range call.Args {
					if a, ok := arg.(*ast.SelectorExpr); ok && a.Sel.Name == "EstateReplicated" {
						return "store.OpenEstate opens the replicated estate's file", true
					}
				}
			}
			return "", false
		}
	}
	if want, ok := replicatedReachMethods[name]; ok && len(call.Args) == want {
		return "." + name, true
	}
	return "", false
}

// reachesTheReplicatedEstate parses one snippet under the imports the controls
// name and reports whether the walk flags it.
func reachesTheReplicatedEstate(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", `package p

import (
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

func f() {
`+src+`
}
`, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", src, err)
	}
	names := namesOf(file, "internal/p")
	flagged := false
	ast.Inspect(file, func(n ast.Node) bool {
		if _, ok := replicatedReachAt(n, names); ok {
			flagged = true
		}
		return true
	})
	return flagged
}
