package codefault_test

import (
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

	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// codefaultPath is this package's import path, which a hit site names its
// points through.
const codefaultPath = "github.com/crewlet/crewlet/internal/code/codefault"

// hitDirs are the directories the points are hit in: the code host's
// pipeline, its engine write's build among it, and the suite's wrapping
// publisher that hits the committer's — a push's — the cache, whose sync hits
// its own, and the compactor, which hits the compaction's.
var hitDirs = []string{"internal/code/host", "internal/code/codetest", "internal/code/cache",
	"internal/code/compact"}

// hitSites counts, over the non-test Go files under each of dirs, every call
// whose function is a selector named `fault` or `Hit` and whose second argument
// names a point of this package — under whatever name the file imports it.
func hitSites(dirs []string) (map[codefault.Point]int, error) {
	counts := map[codefault.Point]int{}
	for _, dir := range dirs {
		if _, err := os.Stat(dir); err != nil {
			return nil, fmt.Errorf("the hit sites' directory %s: %w", dir, err)
		}
		err := sourcetree.Walk(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			name := ""
			for _, imp := range file.Imports {
				if p, _ := strconv.Unquote(imp.Path.Value); p == codefaultPath {
					name = "codefault"
					if imp.Name != nil {
						name = imp.Name.Name
					}
				}
			}
			if name == "" {
				return nil
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				fn, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (fn.Sel.Name != "fault" && fn.Sel.Name != "Hit") {
					return true
				}
				arg, ok := call.Args[1].(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if pkg, ok := arg.X.(*ast.Ident); ok && pkg.Name == name {
					counts[codefault.Point(arg.Sel.Name)]++
				}
				return true
			})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return counts, nil
}

// pointNames maps each point's Go identifier to the point, as a hit site
// spells it.
var pointNames = map[string]codefault.Point{
	"HostAfterCommands": codefault.HostAfterCommands, "HostAfterSync": codefault.HostAfterSync,
	"HostAfterScratch": codefault.HostAfterScratch, "HostHookAdmitted": codefault.HostHookAdmitted,
	"HostUpload": codefault.HostUpload, "HostAfterUpload": codefault.HostAfterUpload,
	"CommitterAfterAppend": codefault.CommitterAfterAppend, "HostAfterPropose": codefault.HostAfterPropose,
	"HostBeforeAdopt": codefault.HostBeforeAdopt, "HostHookClose": codefault.HostHookClose,
	"SyncDownload": codefault.SyncDownload, "SyncRefs": codefault.SyncRefs, "SyncState": codefault.SyncState,
	"SyncSwapBeforeRename": codefault.SyncSwapBeforeRename, "InstallAfterFetch": codefault.InstallAfterFetch,
	"CompactAfterBuild": codefault.CompactAfterBuild, "CompactUpload": codefault.CompactUpload,
	"CompactAfterUpload": codefault.CompactAfterUpload, "CompactAfterPropose": codefault.CompactAfterPropose,
	"WriteAfterBuild": codefault.WriteAfterBuild,
}

// byPoint is counts keyed by identifier, read as points.
func byPoint(t *testing.T, counts map[codefault.Point]int) map[codefault.Point]int {
	t.Helper()
	out := map[codefault.Point]int{}
	for ident, n := range counts {
		p, ok := pointNames[string(ident)]
		if !ok {
			t.Fatalf("a hit names codefault.%s, which is no point this test knows", ident)
		}
		out[p] += n
	}
	if len(pointNames) != len(codefault.Points()) {
		t.Fatalf("this test names %d points and the package has %d: add the new one to pointNames",
			len(pointNames), len(codefault.Points()))
	}
	return out
}

// A fault point is ONE step, named once: F21 kills the node at every point
// of PushPath(), C6 the home at every point of CompactPath(), F37 the git node
// at every point of WritePath() — the write's own and the one landing's — and
// a cache case holds a sync — or crashes it — at each of the sync's, so a
// point hit at
// two sites fires at whichever runs first and a point hit at none is a fault
// no case can make — the swap's rename among them, which a case copies the
// disk at as a crash would leave it. The two upload points are each hit in
// the one closure their caller hands code.StoreBundle.
func TestEveryPointHasOneHitSite(t *testing.T) {
	root := sourcetree.Root(t)
	dirs := make([]string, len(hitDirs))
	for i, d := range hitDirs {
		dirs[i] = filepath.Join(root, d)
	}
	counts, err := hitSites(dirs)
	if err != nil {
		t.Fatal(err)
	}
	sites := byPoint(t, counts)
	for _, p := range codefault.Points() {
		if sites[p] != 1 {
			t.Errorf("%s: hit at %d sites — a fault point is one step, named once, or a case faults at a "+
				"step nobody can find", p, sites[p])
		}
	}
}

// The walk counts each point by its sites, under the import's own name or a
// renamed one, and nothing that is not a hit: the control that keeps the test
// above able to fail.
func TestTheHitSiteWalkCountsEachPoint(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", `package a
import "`+codefaultPath+`"
func (s *S) run() {
	_ = s.fault(ctx, codefault.HostAfterSync, 0)
	_ = s.fault(ctx, codefault.HostAfterSync, 0)
	_ = s.script.Arm(codefault.HostUpload, codefault.Action{})
	_ = s.other(ctx, codefault.HostHookClose, 0)
}
`)
	write("b.go", `package a
import cf "`+codefaultPath+`"
func (p *P) Publish() { _ = p.faults.Hit(ctx, cf.CommitterAfterAppend, 0) }
`)
	write("b_test.go", `package a
import "`+codefaultPath+`"
func x() { _ = s.fault(ctx, codefault.HostHookClose, 0) }
`)
	counts, err := hitSites([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	sites := byPoint(t, counts)
	want := map[codefault.Point]int{codefault.HostAfterSync: 2, codefault.CommitterAfterAppend: 1}
	if len(sites) != len(want) {
		t.Errorf("the walk counted %v, want %v", sites, want)
	}
	for p, n := range want {
		if sites[p] != n {
			t.Errorf("%s: counted %d, want %d", p, sites[p], n)
		}
	}
	if _, err := hitSites([]string{filepath.Join(dir, "missing")}); err == nil {
		t.Error("a walk of a directory that does not exist passed")
	}
}
