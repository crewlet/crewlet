package gitletfault_test

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

	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// gitletfaultPath is the import path a hit site names the points through.
const gitletfaultPath = "github.com/crewlet/crewlet/internal/gitlet/gitletfault"

// hitDirs are the directories the points are hit in: the host pipeline (the
// engine write's build included), the suite's wrapping publisher and reserve
// (the committer's: a push's), the cache's sync, the compactor (compaction,
// publication, expunge), the check runner (a check's run), and the mirrors
// (import, pull mirror).
var hitDirs = []string{"internal/gitlet/host", "internal/gitlet/gitlettest", "internal/gitlet/cache",
	"internal/gitlet/compact", "internal/gitlet/checkrun", "internal/gitlet/mirror"}

// hitSites counts, over the non-test Go files under dirs, every call to a
// selector named `fault` or `Hit` whose second argument names a point of this
// package, under whatever name the file imports it.
func hitSites(dirs []string) (map[gitletfault.Point]int, error) {
	counts := map[gitletfault.Point]int{}
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
				if p, _ := strconv.Unquote(imp.Path.Value); p == gitletfaultPath {
					name = "gitletfault"
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
					counts[gitletfault.Point(arg.Sel.Name)]++
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

// pointNames maps each point's Go identifier, as a hit site spells it, to the
// point.
var pointNames = map[string]gitletfault.Point{
	"HostAfterCommands": gitletfault.HostAfterCommands, "HostAfterSync": gitletfault.HostAfterSync,
	"HostAfterScratch": gitletfault.HostAfterScratch, "HostHookAdmitted": gitletfault.HostHookAdmitted,
	"HostUpload": gitletfault.HostUpload, "HostAfterUpload": gitletfault.HostAfterUpload,
	"CommitterAfterAppend": gitletfault.CommitterAfterAppend, "HostAfterPropose": gitletfault.HostAfterPropose,
	"HostBeforeAdopt": gitletfault.HostBeforeAdopt, "HostHookClose": gitletfault.HostHookClose,
	"SyncDownload": gitletfault.SyncDownload, "SyncRefs": gitletfault.SyncRefs, "SyncState": gitletfault.SyncState,
	"SyncSwapBeforeRename": gitletfault.SyncSwapBeforeRename, "InstallAfterFetch": gitletfault.InstallAfterFetch,
	"CompactAfterBuild": gitletfault.CompactAfterBuild, "CompactUpload": gitletfault.CompactUpload,
	"CompactAfterUpload": gitletfault.CompactAfterUpload, "CompactAfterPropose": gitletfault.CompactAfterPropose,
	"PublishAfterBuild": gitletfault.PublishAfterBuild, "PublishUpload": gitletfault.PublishUpload,
	"PublishAfterUpload": gitletfault.PublishAfterUpload, "PublishAfterPropose": gitletfault.PublishAfterPropose,
	"WriteAfterBuild": gitletfault.WriteAfterBuild, "MergeAfterBuild": gitletfault.MergeAfterBuild,
	"ChainAfterBuild": gitletfault.ChainAfterBuild, "ExpungeAfterBuild": gitletfault.ExpungeAfterBuild,
	"ExpungeAfterBegin": gitletfault.ExpungeAfterBegin, "ExpungeAfterRescueUpload": gitletfault.ExpungeAfterRescueUpload,
	"ExpungeAfterCommit": gitletfault.ExpungeAfterCommit, "CheckAfterClaim": gitletfault.CheckAfterClaim,
	"ImportAfterBuild": gitletfault.ImportAfterBuild, "ImportAfterUpload": gitletfault.ImportAfterUpload,
	"ImportAfterPropose": gitletfault.ImportAfterPropose, "MirrorAfterBuild": gitletfault.MirrorAfterBuild,
	"MirrorAfterUpload": gitletfault.MirrorAfterUpload, "MirrorAfterPropose": gitletfault.MirrorAfterPropose,
	"CommitterBeforeAdmit": gitletfault.CommitterBeforeAdmit, "MaintainBeforeStep": gitletfault.MaintainBeforeStep,
	"WriteJoined": gitletfault.WriteJoined, "HostBeforeOffered": gitletfault.HostBeforeOffered,
}

// byPoint is counts keyed by identifier, read as points.
func byPoint(t *testing.T, counts map[gitletfault.Point]int) map[gitletfault.Point]int {
	t.Helper()
	out := map[gitletfault.Point]int{}
	for ident, n := range counts {
		p, ok := pointNames[string(ident)]
		if !ok {
			t.Fatalf("a hit names gitletfault.%s, which is no point this test knows", ident)
		}
		out[p] += n
	}
	if len(pointNames) != len(gitletfault.Points()) {
		t.Fatalf("this test names %d points and the package has %d: add the new one to pointNames",
			len(pointNames), len(gitletfault.Points()))
	}
	return out
}

// A fault point is ONE step, named once: cases kill a node or maintainer at
// every point of a path (F21 PushPath(), C6 CompactPath(), LY10 PublishPath(),
// F38 WritePath() with the landing's, the expunge suite ExpungePath()), and the
// cache cases hold or crash a sync at each of its points, copying the disk at
// the swap's rename as a crash leaves it. So a point hit at two sites fires at
// whichever runs first and one hit at none is a fault no case can make. Each
// upload point is hit in the one closure its caller hands gitlet.StoreBundle
// (the publication's also gitlet.StoreLayer).
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
	for _, p := range gitletfault.Points() {
		if sites[p] != 1 {
			t.Errorf("%s: hit at %d sites — a fault point is one step, named once, or a case faults at a "+
				"step nobody can find", p, sites[p])
		}
	}
}

// The walk counts each point by its sites, under the import's own name or a
// renamed one, and nothing that is not a hit: the control that keeps
// TestEveryPointHasOneHitSite able to fail.
func TestTheHitSiteWalkCountsEachPoint(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.go", `package a
import "`+gitletfaultPath+`"
func (s *S) run() {
	_ = s.fault(ctx, gitletfault.HostAfterSync, 0)
	_ = s.fault(ctx, gitletfault.HostAfterSync, 0)
	_ = s.script.Arm(gitletfault.HostUpload, gitletfault.Action{})
	_ = s.other(ctx, gitletfault.HostHookClose, 0)
}
`)
	write("b.go", `package a
import cf "`+gitletfaultPath+`"
func (p *P) Publish() { _ = p.faults.Hit(ctx, cf.CommitterAfterAppend, 0) }
`)
	write("b_test.go", `package a
import "`+gitletfaultPath+`"
func x() { _ = s.fault(ctx, gitletfault.HostHookClose, 0) }
`)
	counts, err := hitSites([]string{dir})
	if err != nil {
		t.Fatal(err)
	}
	sites := byPoint(t, counts)
	want := map[gitletfault.Point]int{gitletfault.HostAfterSync: 2, gitletfault.CommitterAfterAppend: 1}
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
