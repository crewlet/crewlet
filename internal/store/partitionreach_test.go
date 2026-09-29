package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// ONLY THE RUNTIME REACHES A PARTITION.
//
// Which partitions a node holds is the estate map's answer, and it moves: a
// partition is opened when this node joins it and closed when it leaves, and
// in a fleet most partitions are held somewhere else. So a caller that reaches
// for one by name — opens it, looks it up, builds a handle to it, finds its
// file — is a caller assuming this node holds it, and that assumption is
// right on a single node and wrong on every fleet. What reaches a partition is
// the RUNTIME that opened it, handing each one to the domains that apply and
// read it there; every other reader asks the node that holds it.
//
// `DB.Replicated()` was the whole of this rule while there was one file: it
// was deleted, and what replaced it is a family of exported methods the
// compiler lets any package call. This walk is what keeps the family to the
// list below.
//
// # What it reports
//
// Every call in a non-test file under internal/ or cmd/ to one of the store's
// partition methods — `OpenPartition`, `ClosePartition`, `DropPartition`,
// `OpenPartitions`, `PartitionDB`, `PartitionHandle`, `PartitionPath` — on a
// receiver that is not an imported package, and every call through the store
// package itself that names a partition's file: `PartitionPath`,
// `ReplicatedPath` (layout 0's), and `OpenEstate` of `EstatePartition`.
// Matching is on the name, as every gate in this package matches; the one name
// it trusts is an import's.
//
// # The allowance is two-sided
//
// An entry that matches no call FAILS, for the reason the applier gate's do.
func TestOnlyTheRuntimeReachesAPartition(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN.
	for _, positive := range []string{
		`node.OpenPartition(ctx, f)`,
		`s.db.ClosePartition(name)`,
		`node.DropPartition(ctx, f)`,
		`names := node.OpenPartitions()`,
		`part, err := node.PartitionDB(name)`,
		`r := node.PartitionHandle(name).Reader()`,
		`path := node.PartitionPath(f)`,
		`path := store.PartitionPath(dir, 1, name)`,
		`path := store.ReplicatedPath(boot.Store.Path, "")`,
		`db, err := store.OpenEstate(ctx, store.EstatePartition, path, store.Options{})`,
	} {
		if !reachesAPartition(t, positive) {
			t.Errorf("control: %q reaches a partition and the matcher did "+
				"not flag it", positive)
		}
	}
	for _, negative := range []string{
		`p := prompts.Partition(msg)`,
		`db, err := store.OpenEstate(ctx, store.EstateNode, path, store.Options{})`,
		`node, err := store.OpenNode(ctx, path, store.Options{})`,
		`err := h.Read(ctx, fn)`,
		`db := c.PartitionDB(t, i)`,
	} {
		if reachesAPartition(t, negative) {
			t.Errorf("control: %q does not reach a partition and the matcher "+
				"flagged it", negative)
		}
	}

	root := sourcetree.Root(t)
	files := parseTree(t, root, "internal", "cmd")
	var found []site
	for _, f := range files {
		ast.Inspect(f.file, func(n ast.Node) bool {
			if why, ok := partitionReachAt(n, f.names); ok {
				found = append(found, site{
					File: f.rel, Line: f.fset.Position(n.Pos()).Line, Why: why,
				})
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatal("found no call reaching a partition at all, not even the " +
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
		t.Errorf("%s:%d reaches a partition by name (%s).\n"+
			"\tWhich partitions this node holds is the estate map's answer and "+
			"moves with it, so nothing but the runtime that opened a partition "+
			"may reach for one: a domain is handed its partition by "+
			"internal/engine, and every other reader asks the node that holds "+
			"it. If this caller genuinely owns a partition's FILE — the way a "+
			"backup or an operator's migration does — add it to "+
			"allowedPartitionReach with the reason.", s.File, s.Line, s.Why)
	}
	for _, a := range allowedPartitionReach {
		if !used[a.prefix] {
			t.Errorf("%s is allowed to reach a partition and does not — the "+
				"reason on file is %q. Delete the entry", a.prefix, a.why)
		}
	}
	t.Logf("parsed %d files; %d call(s) reach a partition across %d allowance(s)",
		len(files), len(found), len(allowedPartitionReach))
}

// allowedPartitionReach is every file that may reach a partition by name, and
// why. Keyed and matched as [allowedPartitionWriter] is.
var allowedPartitionReach = []struct{ prefix, why string }{
	{"internal/store/", "The partitions' owner, and its test helpers."},
	{"internal/engine/", "THE RUNTIME: it opens and closes the partitions its " +
		"layout places on this node and hands each to the domains that apply " +
		"and read it there."},
	{"internal/statelog/", "The framework's own copies of a partition — the " +
		"adoption verifying a fetched artefact before it is renamed over the " +
		"live file, and the read of a copy's cursor."},
	{"internal/backup/", "Copies each partition's FILE, and reads a restored " +
		"one's object references."},
	{"cmd/crewlet/ops.go", "`crewlet migrate` opens every partition its layout " +
		"names on a stopped node, which is how an operator's migration reaches " +
		"each file."},
	{"cmd/crewlet/search.go", "`crewlet search eval` reads layout 0's " +
		"partition FILE on a stopped node, offline, as an operator's tool."},
}

// reachAllowanceFor reports which entry covers a file; the longest prefix
// wins, so a file-specific entry is never shadowed by a package-wide one.
func reachAllowanceFor(file string) (string, bool) {
	best, found := "", false
	for _, a := range allowedPartitionReach {
		p := filepath.FromSlash(a.prefix)
		if strings.HasPrefix(file, p) && len(p) > len(best) {
			best, found = a.prefix, true
		}
	}
	return best, found
}

// partitionReachMethods are the store's partition methods, with the argument
// count each takes: the count is what tells the store's `PartitionDB(name)`
// from a same-named method of another type the walk cannot resolve.
var partitionReachMethods = map[string]int{
	"OpenPartition":   2,
	"ClosePartition":  1,
	"DropPartition":   2,
	"OpenPartitions":  0,
	"PartitionDB":     1,
	"PartitionHandle": 1,
	"PartitionPath":   1,
}

// partitionReachAt reports whether one node is a call reaching a partition.
func partitionReachAt(n ast.Node, f fileNames) (string, bool) {
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
			case "PartitionPath", "ReplicatedPath":
				return "store." + name + " names a partition's file", true
			case "OpenEstate":
				for _, arg := range call.Args {
					if a, ok := arg.(*ast.SelectorExpr); ok && a.Sel.Name == "EstatePartition" {
						return "store.OpenEstate opens a partition's file", true
					}
				}
			}
			return "", false
		}
	}
	if want, ok := partitionReachMethods[name]; ok && len(call.Args) == want {
		return "." + name, true
	}
	return "", false
}

// reachesAPartition parses one snippet under the imports the controls name
// and reports whether the walk flags it.
func reachesAPartition(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", `package p

import (
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/store"
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
		if _, ok := partitionReachAt(n, names); ok {
			flagged = true
		}
		return true
	})
	return flagged
}
