package gitproc_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestOnlyGitprocExecsGit fails the build when any Go file of the module
// outside cmd/ but the pool's own exec and gittest's starts git (ADR-0008): a
// second exec.Command is a place to forget one of the pool's six decisions
// (package doc, "Why one wrapper"), and nothing about it looks wrong. Test
// files are held too, by FILE (allowedGitExecs): a test's git needs the same
// hermetic environment, which gittest gives. The walk starts at the module
// root, so a new directory is read unlisted, and skips testdata and cmd/,
// whose `crewlet gitlet import` and `export` run the OPERATOR's git.
//
// What counts as git is execJudge's, as is the coverage boundary. A git-word
// name that runs another program is a hit: rename it. If the rule goes away,
// DELETE this guard rather than weaken it.
func TestOnlyGitprocExecsGit(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	found := gitExecs(t, root)

	if found.read == 0 || found.parsed == 0 || found.starting == 0 {
		t.Fatalf("read %d files, parsed %d and found %d that start a process — this guard is "+
			"certifying nothing. Check the module root and the walk", found.read, found.parsed, found.starting)
	}
	// A guard asserting an ABSENCE passes as well when its matcher has gone
	// inert, so these known verdicts fail it first; and every source it flags
	// must pass the prefilter, or the walk would skip that very file.
	for _, c := range []struct {
		src  string
		want bool
	}{
		{`package p; import "os/exec"; var _ = exec.Command("git", "x")`, true},
		{`package p; import "os/exec"; func f(ctx C) { exec.CommandContext(ctx, "/usr/bin/git") }`, true},
		{`package p; import ex "os/exec"; var _ = ex.Command(gitPath)`, true},
		{`package p; import "os/exec"; func f(c *C) { exec.Command(c.Git) }`, true},
		{`package p; import "os/exec"; var _ = &exec.Cmd{Path: "git"}`, true},
		{`package p; import "os/exec"; var _ = exec.Cmd{Args: nil, Path: realGit}`, true},
		{`package p; import "os"; var _, _ = os.StartProcess("git", nil, nil)`, true},
		{`package p; import "syscall"; var _ = syscall.Exec("/usr/bin/git", nil, nil)`, true},
		{`package p; import "syscall"; var _, _ = syscall.ForkExec(GitBinary, nil, nil)`, true},
		{`package p; import "golang.org/x/sys/unix"; var _ = unix.Exec(GITPath, nil, nil)`, true},
		{`package p; import ("os/exec"; "path/filepath"); var _ = exec.Command(filepath.Join("/usr/bin", "git"))`, true},
		{`package p; import "os/exec"; var _ = exec.Command(gitPath())`, true},
		{`package p; import "os/exec"; func f(o *O) { exec.Command(o.GitBinary()) }`, true},
		{`package p; import "os/exec"; func f(c *C) { exec.Command((*c.git)) }`, true},
		{`package p; import "os/exec"; var _ = exec.Command(my_git_2)`, true},
		{`package p; import "os/exec"; var _ = exec.Command(oldGit2)`, true},
		// What gittest hands out, and what a variable assigned it carries.
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest")
			func f(t T) { exec.Command(gittest.Require(t)) }`, true},
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest")
			func f(t T) { bin := gittest.Require(t); exec.Command(bin, "init") }`, true},
		{`package p; import ("os/exec"; gt "github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest")
			func f(t T) { bin, _ := gt.Recording(t); exec.Command(bin) }`, true},
		{`package p; import ("os/exec"; "path/filepath")
			func f(d string) { var bin string; bin = filepath.Join(d, "git"); exec.Command(bin) }`, true},
		{`package p; import "os/exec"
			func f() { p, _ := exec.LookPath("git"); exec.Command(p) }`, true},
		{`package p; import "os/exec"
			func f() { a := "git"; b := a; exec.Command(b) }`, true},
		// git's own programs, which are git processes: what an SSH command
		// line names and what net/http/cgi serves.
		{`package p; import "os/exec"; var _ = exec.Command("git-upload-pack", "--stateless-rpc", ".")`, true},
		{`package p; import "os/exec"; var _ = exec.Command("/usr/lib/git-core/git-receive-pack", ".")`, true},
		{`package p; import "syscall"; var _, _ = syscall.ForkExec("git-upload-archive", nil, nil)`, true},
		{`package p; import ("os/exec"; "path/filepath")
			func f(execPath string) { exec.Command(filepath.Join(execPath, "git-upload-pack")) }`, true},
		{`package p; import "net/http/cgi"; var _ = &cgi.Handler{Path: "/usr/lib/git-core/git-http-backend"}`, true},
		{`package p; import ("net/http/cgi"; "path/filepath")
			func f(execPath string) any { return cgi.Handler{Root: "/git", Path: filepath.Join(execPath, "git-http-backend")} }`, true},
		{`package p; import "os/exec"; var _ = []*exec.Cmd{{Path: "git"}}`, true},
		{`package p; import "os/exec"; var _ = map[string]exec.Cmd{"clone": {Dir: "/", Path: "/usr/bin/git"}}`, true},
		// Constants, collections and conversions given git in the same file.
		{`package p; import "os/exec"; const bin = "git"; var _ = exec.Command(bin)`, true},
		{`package p; import "os/exec"; func f() { const bin = "git"; exec.Command(bin, "init") }`, true},
		{`package p; import "os/exec"; const ( a = "git"; b ); var _ = exec.Command(b)`, true},
		{`package p; import "os/exec"
			func f() { argv := []string{"git", "init"}; exec.Command(argv[0], argv[1:]...) }`, true},
		{`package p; import "os/exec"
			func f(rest []string) { argv := append([]string{"git"}, rest...); exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"; func f() { for _, bin := range []string{"git"} { exec.Command(bin) } }`, true},
		{`package p; import "os/exec"
			func f() { bins := []string{"sh", "git"}; for _, bin := range bins { exec.Command(bin) } }`, true},
		{`package p; import "os/exec"
			func f() { for bin := range map[string]bool{"git": true} { exec.Command(bin) } }`, true},
		{`package p; import ("os/exec"; "path/filepath")
			func f(d string) { name := "git"; exec.Command(filepath.Join(d, name)) }`, true},
		{`package p; import "os/exec"; func f() { name := "git"; exec.Command(string(name)) }`, true},
		{`package p; import "os/exec"; func f(gitPath []byte) { exec.Command(string(gitPath)) }`, true},
		{`package p; import "os/exec"; func f(dir string) { exec.Command(dir + "/git") }`, true},
		{`package p; import "os/exec"; func f() { var v any = "git"; exec.Command(v.(string)) }`, true},
		// An element read as the program: a collection named for git, a key
		// that is git by its value, the element a constant index fixes, and
		// every element where nothing fixes which one is read.
		{`package p; import "os/exec"; func f(gitArgv []string) { exec.Command(gitArgv[0], gitArgv[1:]...) }`, true},
		{`package p; import "os/exec"; func f(o *O) { exec.Command(o.GitArgv[0], o.GitArgv[1:]...) }`, true},
		{`package p; import "os/exec"; func f(gitArgv []string) { exec.Command(gitArgv[1:][0]) }`, true},
		{`package p; import "os/exec"; func f(gitPath string) { exec.Command(gitPath[:]) }`, true},
		{`package p; import "os/exec"; func f(tools map[string]string) { exec.Command(tools["git"]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := []string{gitPath, "init"}; exec.Command(argv[0], argv[1:]...) }`, true},
		{`package p; import "os/exec"
			func f(o *O, rest []string) { argv := append([]string{o.git}, rest...); exec.Command(argv[0], argv[1:]...) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := append([]string{}, gitPath, "init"); exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := [...]string{1: "init", 0: gitPath}; exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { const first = 0; argv := []string{gitPath}; exec.Command(argv[first]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string, i int) { argv := []string{"sh", gitPath}; exec.Command(argv[i]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { for _, bin := range []string{"sh", gitPath} { exec.Command(bin) } }`, true},
		{`package p; import "os/exec"; func f(gitBins []string) { for _, bin := range gitBins { exec.Command(bin) } }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { cases := [][]string{{"sh"}, {gitPath, "init"}}; for _, c := range cases { exec.Command(c[0], c[1:]...) } }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argvs := [][]string{{"go"}, {gitPath}}; exec.Command(argvs[1][0]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := []string{"go", gitPath}; exec.Command(argv[1:][0]) }`, true},
		{`package p; import "os/exec"; func f(gitArgv []string) { p := &gitArgv; exec.Command((*p)[0]) }`, true},
		{`package p; import "os/exec"; func f(gitArgvs chan []string) { exec.Command((<-gitArgvs)[0]) }`, true},
		{`package p; import "os/exec"; func f() { exec.Command(gitArgv()[0]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := []string{"go"}; argv = append(argv, "x"); argv = append(argv, gitPath); exec.Command(argv[2]) }`, true},
		{`package p; import "os/exec"
			func f(c bool, gitPath string) { a := []string{"go"}; if c { a = []string{"sh", "-c"} }; argv := append(a, gitPath); exec.Command(argv[2]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { seen := map[string]bool{}; seen[gitPath] = true; for name := range seen { exec.Command(name) } }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := append([]string{"go"}, []string{"test", gitPath}...); exec.Command(argv[2]) }`, true},
		// By its value anywhere in the collection, whatever the index reads:
		// a wrapper (env, nice, timeout) runs the git beside it.
		{`package p; import "os/exec"; func f() { argv := []string{"env", "git", "status"}; exec.Command(argv[0], argv[1:]...) }`, true},
		{`package p; import "os/exec"; func f() { p := &[]string{"env", "git"}; exec.Command((*p)[0]) }`, true},
		{`package p; import "os/exec"
			func f() { argvs := [][]string{{"env", "git"}}; argv := argvs[0]; exec.Command(argv[0], argv[1:]...) }`, true},
		{`package p; import "os/exec"
			func f(rest []string) { argv := append([]string{"env", "git"}, rest...); exec.Command(argv[0], argv[1:]...) }`, true},
		{`package p; import ("os/exec"; "path/filepath")
			func f(dir string) { for name := range map[string]bool{"git": true} { exec.Command(filepath.Join(dir, name)) } }`, true},
		{`package p; import "os/exec"
			func f(argvs chan []string) { argvs <- []string{"env", "git"}; argv := <-argvs; exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"; func f(gitFns []func() string) { exec.Command(gitFns[0]()) }`, true},
		{`package p; import "os/exec"; func f() { exec.Command(findGit[string]()) }`, true},
		// A collection filled element by element, or copied into.
		{`package p; import "os/exec"
			func f() { argv := make([]string, 1); argv[0] = "git"; exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"
			func f(k string) { m := map[string]string{}; m[k] = "git"; exec.Command(m[k]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := make([]string, 2); argv[0] = gitPath; exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"
			func f() { seen := map[string]bool{}; seen["git"] = true; for name := range seen { exec.Command(name) } }`, true},
		{`package p; import "os/exec"
			func f() { argv := make([]string, 2); copy(argv, []string{"git", "init"}); exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"
			func f(gitPath string) { argv := make([]string, 3); copy(argv[1:], []string{gitPath}); exec.Command(argv[1]) }`, true},
		{`package p; import ("os/exec"; "path/filepath")
			func f() { parts := []string{"/usr/bin", "git"}; exec.Command(filepath.Join(parts...)) }`, true},
		// Through an address: a pointer and what it points at are one value,
		// whichever of the two is written.
		{`package p; import "os/exec"; func f() { bin := "git"; p := &bin; exec.Command(*p) }`, true},
		{`package p; import "os/exec"; func f() { bin := new(string); *bin = "git"; exec.Command(*bin) }`, true},
		{`package p; import "os/exec"; func f() { var bin string; p := &bin; *p = "git"; exec.Command(bin) }`, true},
		{`package p; import "os/exec"
			func f() { var argv [2]string; p := &argv[0]; *p = "git"; exec.Command(argv[0], argv[1]) }`, true},
		{`package p; import "os/exec"
			func f() { var argv []string; p := &argv; *p = append(*p, "git"); exec.Command(argv[0]) }`, true},
		{`package p; import "os/exec"; func f(bin string) { exec.Command(*(&bin)) }`, false},
		// A channel is a collection: a send puts an element in it, and a
		// receive reads one.
		{`package p; import "os/exec"; func f(ch chan string) { ch <- "git"; exec.Command(<-ch) }`, true},
		{`package p; import "os/exec"; func f(gitPaths chan string) { exec.Command(<-gitPaths) }`, true},
		{`package p; import "os/exec"
			func f(ch chan string) { ch <- "git"; for bin := range ch { exec.Command(bin) } }`, true},
		{`package p; import "os/exec"; func f(ch chan string) { ch <- "go"; exec.Command(<-ch) }`, false},
		// A callee held in a variable, or an element of a collection, is read
		// as the program it names; and a call is git by a collection argument
		// holding git, as by a plain one.
		{`package p; import "os/exec"; func f() { find := findGit; exec.Command(find()) }`, true},
		{`package p; import "os/exec"; func f() { fns := []func() string{findGit}; exec.Command(fns[0]()) }`, true},
		{`package p; import "os/exec"; func f() { exec.Command(findGit[string, int]()) }`, true},
		{`package p; import ("os/exec"; "strings")
			func f() { exec.Command(strings.Join([]string{"/usr/bin", "git"}, "/")) }`, true},
		{`package p; import "os/exec"; func f() { find := lookup; exec.Command(find()) }`, false},
		// Not git — other programs, other words, other packages.
		{`package p; import "os/exec"; var _ = exec.Command("gitlab-runner")`, false},
		{`package p; import "os/exec"; var _ = exec.Command(digit)`, false},
		{`package p; import "os/exec"; var _ = exec.Command(legit, GITLAB, gitproc)`, false},
		{`package p; import "os/exec"; var _, _ = exec.LookPath("git")`, false},
		{`package p; import "example.com/exec"; var _ = exec.Command("git")`, false},
		{`package p; import "os/exec"; func f(exec E) { exec.Command("git") }`, false},
		{`package p; import "os/exec"; var _ = exec.Command("sh", "-c", "git status")`, false},
		{`package p; import "os/exec"; var _ = exec.Command(script, "git")`, false},
		{`package p; import "os/exec"; var _ = exec.Cmd{Path: "sh", Args: []string{"git"}}`, false},
		{`package p; import "os"; var _ = os.Getenv("git")`, false},
		{`package p; import "os/exec"; var _ = exec.Command(".git/hooks/pre-commit")`, false},
		{`package p; import "net/http/cgi"; var _ = &cgi.Handler{Path: "/usr/lib/cgi-bin/app"}`, false},
		{`package p; import "example.com/cgi"; var _ = &cgi.Handler{Path: "git"}`, false},
		// A name's own words make a PROGRAM git, never a part of one: a path
		// under a repository's directory is not git, nor is a collection's
		// element because another element has a git word.
		{`package p; import ("os/exec"; "path/filepath")
			func f(gitDir string) { exec.Command(filepath.Join(gitDir, "hooks", "post-update")) }`, false},
		{`package p; import ("os/exec"; "path/filepath")
			func f(root string) { gitDir := filepath.Join(root, ".git"); exec.Command(gitDir + "/hooks/x") }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := []string{"go", "test", gitDir}; exec.Command(argv[0], argv[1:]...) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := []string{"go", "test"}; argv = append(argv, gitDir); exec.Command(argv[0], argv[1:]...) }`, false},
		{`package p; import "os/exec"
			func f(gitDirs []string) { argv := append([]string{"go", "test"}, gitDirs...); exec.Command(argv[0], argv[1:]...) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := append([]string{}, "go", gitDir); exec.Command(argv[0], argv[1:]...) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := append([]string{"go"}, []string{"test", gitDir}...); exec.Command(argv[1]) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := append(make([]string, 1), gitDir); exec.Command(argv[0]) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := append(append([]string{}, "go"), gitDir); exec.Command(argv[0]) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := make([]string, 3); argv[0] = "go"; argv[2] = gitDir; exec.Command(argv[0], argv[1:]...) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argv := [...]string{2: gitDir, 0: "go"}; exec.Command(argv[0]) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { const first = 0; argv := []string{"go", gitDir}; exec.Command(argv[first]) }`, false},
		{`package p; import "os/exec"
			func f(gitIdx int) { argv := make([]string, 2); argv[gitIdx] = "go"; exec.Command(argv[0]) }`, false},
		{`package p; import ("os/exec"; "path/filepath")
			func f(gitDir string) { for _, dir := range []string{gitDir} { exec.Command(filepath.Join(dir, "hooks", "x")) } }`, false},
		{`package p; import ("os/exec"; "path/filepath")
			func f(gitDir string) { p := &gitDir; exec.Command(filepath.Join(*p, "hooks", "x")) }`, false},
		{`package p; import "os/exec"; func f(k string) { m := map[string]string{}; m[k] = "go"; exec.Command(m[k]) }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { cases := [][]string{{"go", "test", gitDir}}; for _, c := range cases { exec.Command(c[0], c[1:]...) } }`, false},
		{`package p; import "os/exec"
			func f(gitDir string) { argvs := [][]string{{"go", gitDir}}; exec.Command(argvs[0][0]) }`, false},
		{`package p; import "os/exec"
			func f() { argv := []string{"go", "x"}; argv = append(argv[1:], "y"); exec.Command(argv[0]) }`, false},
		// The coverage boundary (execJudge), pinned so the doc cannot claim
		// less than the matcher sees.
		{`package p; import "os/exec"; func f(o *O) { o.bin = "git"; exec.Command(o.bin) }`, false},
		{`package p; import "os/exec"; func run(bin string) { exec.Command(bin) }; func f() { run("git") }`, false},
		{`package p; import "os/exec"; func bin() string { return "git" }; func f() { exec.Command(bin()) }`, false},
		{`package p; import ("os"; "os/exec"); func f() { exec.Command(os.Getenv("GIT")) }`, false},
		{`package p; import "os/exec"; func f(gitDirs map[string]string) { exec.Command("go", gitDirs["x"]) }`, false},
		// A variable is followed by its own object, never by its name: a
		// parameter called bin is not the bin assigned git elsewhere.
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest")
			func f(t T) { bin := gittest.Require(t); _ = bin }
			func g(bin string) { exec.Command(bin) }`, false},
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest")
			func f(t T) { bin := gittest.NewClient(t); exec.Command(bin) }`, false},
		// A cycle ends, and a forward reference at package level is followed.
		{`package p; import "os/exec"; var a = b; var b = a; var _ = exec.Command(a)`, false},
		{`package p; import "os/exec"; var a = b; var b = gitPath; var _ = exec.Command(a)`, true},
	} {
		if got := matchesInSource(t, c.src); got != c.want {
			t.Errorf("control: matcher returned %v for %q, want %v", got, c.src, c.want)
		}
		if !c.want {
			continue
		}
		if admitted, err := admit("control.go", []byte(c.src)); err != nil || !admitted {
			t.Errorf("control: the prefilter skips %q, which the matcher flags (%v)", c.src, err)
		}
	}

	outside, stale := judgeExecs(found.hits, allowedGitExecs)
	for _, h := range outside {
		t.Errorf("%s: %s starts git outside the pool and gittest.\n"+
			"\trun the engine's git through gitproc.Pool.Run, and a test's client git "+
			"through gitproc/gittest — the pool is where the environment, the "+
			"configuration, the slot, the deadline and the process group are decided, "+
			"and a second exec forgets one", h.pos, h.dir)
	}
	for _, file := range stale {
		t.Errorf("allowedGitExecs still excuses %s, but the walk no longer finds an exec "+
			"of git there. Delete the entry, or move it with the exec — it was: %s",
			file, allowedGitExecs[file])
	}

	t.Logf("read %d .go files outside cmd/, parsed the %d that spell a starter's name and import "+
		"its package, %d of which start a process: %d git execs, %d allowed",
		found.read, found.parsed, found.starting, len(found.hits), len(found.hits)-len(outside))
}

// judgeExecs splits the walk's hits into the execs no allowance excuses and
// the allowances no exec uses: a stale entry is where a future exec would hide.
func judgeExecs(hits []gitExec, allowed map[string]string) (outside []gitExec, stale []string) {
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.file] = true
		if _, known := allowed[h.file]; !known {
			outside = append(outside, h)
		}
	}
	for _, file := range slices.Sorted(maps.Keys(allowed)) {
		if !seen[file] {
			stale = append(stale, file)
		}
	}
	return outside, stale
}

// allowedGitExecs is the closed set of FILES that may exec git, with the
// reason. By file, not package: gitproc's own tests share the pool's
// directory and are held to gittest like every other suite.
var allowedGitExecs = map[string]string{
	"internal/gitlet/gitproc/run.go": "Pool.Run's exec.Command(p.git, …) — the one exec this " +
		"gate exists to keep the only one",
	"internal/gitlet/gitproc/gittest/gittest.go": "the client git every test drives: " +
		"readVersion's `git version` (the PATH's git and each floor's, once a binary), " +
		"Client.Try's exec.CommandContext(ctx, c.git, …) and Client.HTTPBackend's " +
		"cgi.Handler{Path: c.git} (the other host a mirror's case dials), hermetic for the reasons the pool's " +
		"environment is built",
}

// gittestPackage is the import path of the client git, whose binary-handing
// functions are git wherever they are called.
const gittestPackage = "github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"

// gittestBinaries are the gittest functions whose first result is a git
// binary: the PATH's git (Require, RequireNode), the floors' builds
// (NodeFloor, ClientFloor) and the stand-ins (Fake; Wrap and Recording exec
// the real git). The anchor below fails the build when one is renamed.
var gittestBinaries = map[string]bool{
	"Require": true, "RequireNode": true, "NodeFloor": true, "ClientFloor": true,
	"Fake": true, "Wrap": true, "Recording": true,
}

var _ = []any{gittest.Require, gittest.RequireNode, gittest.NodeFloor, gittest.ClientFloor,
	gittest.Fake, gittest.Wrap, gittest.Recording}

// starters are the calls that start a process, by the import path of their
// package, with the index of the argument that names the program.
var starters = map[string]map[string]int{
	"os/exec":               {"Command": 0, "CommandContext": 1},
	"os":                    {"StartProcess": 0},
	"syscall":               {"Exec": 0, "ForkExec": 0, "StartProcess": 0},
	"golang.org/x/sys/unix": {"Exec": 0},
}

// starterLiterals are the keyed composite literals that describe a process
// to start, by package import path and type, with the field naming the
// program: os/exec's Cmd, and net/http/cgi's Handler, which starts it for
// every request (how git-http-backend is served).
var starterLiterals = map[string]map[string]string{
	"os/exec":      {"Cmd": "Path"},
	"net/http/cgi": {"Handler": "Path"},
}

// starterPackage reports whether path is the import path of a package that
// starts a process.
func starterPackage(path string) bool {
	_, call := starters[path]
	_, literal := starterLiterals[path]
	return call || literal
}

// starterNames is the prefilter's names per package, built from the same
// tables so it cannot fall behind them. anyStarterName is their union, asked
// first so a file spelling none is never parsed even for its imports.
var starterNames, anyStarterName = func() (map[string]sourcetree.Identifiers, sourcetree.Identifiers) {
	byPackage := map[string][]string{}
	for path, calls := range starters {
		byPackage[path] = append(byPackage[path], slices.Collect(maps.Keys(calls))...)
	}
	for path, types := range starterLiterals {
		byPackage[path] = append(byPackage[path], slices.Collect(maps.Keys(types))...)
	}
	out := map[string]sourcetree.Identifiers{}
	var all []string
	for path, names := range byPackage {
		out[path] = sourcetree.MustIdentifiers(names...)
		all = append(all, names...)
	}
	slices.Sort(all)
	return out, sourcetree.MustIdentifiers(slices.Compact(all)...)
}()

// admit reports whether the walk parses src whole. It is EXACT: every
// construct judged is a selector on a starter package's import naming one of
// THAT package's names, so src must import one (imports parsed, not searched
// as text: a path can be spelled with escapes) and spell one of its names (an
// identifier has none). Per package, not the union: os is imported almost
// everywhere, and an Execute or CommandLine beside it starts nothing.
func admit(name string, src []byte) (bool, error) {
	if !anyStarterName.In(src) {
		return false, nil
	}
	imports, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
	if err != nil {
		return false, err
	}
	for _, path := range importNames(imports) {
		if names, ok := starterNames[path]; ok && names.In(src) {
			return true, nil
		}
	}
	return false, nil
}

// gitExec is one exec of git the walk found.
type gitExec struct {
	file string // relative to the root, slash-separated
	dir  string // the file's directory, slash-separated
	pos  string // file:line:column
}

// execWalk is what one walk found. read counts every .go file reached,
// before the prefilter, so the floor measures the walk rather than the
// filter; parsed the files parsed whole; starting those holding a process
// start, git or not, as the judge counted it.
type execWalk struct {
	read, parsed, starting int
	hits                   []gitExec
}

// gitExecs walks the module at root for execs of git.
func gitExecs(t *testing.T, root string) execWalk {
	t.Helper()
	var out execWalk
	fset := token.NewFileSet()
	operators := filepath.Join(root, "cmd")
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata is not compiled, and the module's own cmd/ runs
			// the operator's git: that directory only, not any named cmd.
			if d.Name() == "testdata" || path == operators {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		out.read++
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		admitted, err := admit(path, src)
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		if !admitted {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Errorf("parse %s: %v", path, err)
			return nil
		}
		out.parsed++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		judged := newExecJudge(file).judge()
		if judged.starts > 0 {
			out.starting++
		}
		for _, at := range judged.git {
			p := fset.Position(at)
			p.Filename = filepath.ToSlash(rel)
			out.hits = append(out.hits, gitExec{
				file: p.Filename,
				dir:  filepath.ToSlash(filepath.Dir(rel)),
				pos:  p.String(),
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestTheExecWalkReadsEveryFileImportingExec runs the walk on a tree whose
// verdict is known: git under a renamed import, in an exec.Cmd literal, in
// the pool's run.go, beside internal/ and under a directory named cmd below
// the root (hits); sh and a script's go (process starts, no hit); a starter
// spelled only in a comment; four files never parsed past the prefilter
// (u.go imports os, spelling only os/exec's names), three with bodies that
// are not Go so a deeper parse fails; and git in testdata and the module's
// cmd/, never reached. It then judges the hits against the tree's own
// allowance, exercising the stale direction the main gate shows only on a
// drifted tree.
func TestTheExecWalkReadsEveryFileImportingExec(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/p/p_test.go": "package p\n\nimport ex \"os/exec\"\n\nvar _ = ex.Command(\"git\")\n",
		"internal/p/cmd.go": "package p\n\nimport \"os/exec\"\n\n" +
			"var _ = &exec.Cmd{Path: gitBinary}\n",
		"internal/gitlet/gitproc/run.go": "package gitproc\n\nimport \"os/exec\"\n\n" +
			"func (p *Pool) run() { exec.Command(p.git) }\n",
		"static/serve_test.go": "package static_test\n\nimport \"os/exec\"\n\n" +
			"var _ = exec.Command(\"git-upload-pack\", \".\")\n",
		"internal/v/cmd/c.go": "package cmd\n\nimport \"os/exec\"\n\nconst bin = \"git\"\n\nvar _ = exec.Command(bin)\n",
		"internal/q/q.go":     "package q\n\nimport \"os/exec\"\n\nvar _ = exec.Command(\"sh\", \"-c\", \"git status\")\n",
		"scripts/notices/main.go": "package main\n\nimport \"os/exec\"\n\n" +
			"func main() { _ = exec.Command(\"go\", \"env\").Run() }\n",
		"internal/w/w.go":             "package w\n\nimport \"os/exec\"\n\n// Command is only mentioned here.\nvar _ = exec.ErrNotFound\n",
		"internal/r/r.go":             "package r\n\nimport \"strings\"\n\nthis is not Go, though it spells Command\n",
		"internal/s/s.go":             "package s\n\nimport \"os/exec\"\n\nthis is not Go and spells no starter\n",
		"internal/u/u.go":             "package u\n\nimport \"os\"\n\nthis is not Go, though it spells Command and Cmd\n",
		"internal/x/x.go":             "package x\n\nvar Answer = 42\n",
		"internal/p/testdata/t.go":    "package t\n\nimport \"os/exec\"\n\nvar _ = exec.Command(\"git\")\n",
		"cmd/crewlet/operator_git.go": "package main\n\nimport \"os/exec\"\n\nvar _ = exec.Command(\"git\")\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := gitExecs(t, root)
	files := func(hits []gitExec) []string {
		var out []string
		for _, h := range hits {
			out = append(out, h.file)
		}
		slices.Sort(out)
		return out
	}
	want := []string{"internal/gitlet/gitproc/run.go", "internal/p/cmd.go", "internal/p/p_test.go",
		"internal/v/cmd/c.go", "static/serve_test.go"}
	if found.read != 12 || found.parsed != 8 || found.starting != 7 || !slices.Equal(files(found.hits), want) {
		t.Errorf("read %d, parsed %d, %d starting a process, hits %+v; want 12 read (testdata and "+
			"cmd/ never), 8 parsed (r.go, s.go, u.go and x.go never past the prefilter), 7 starting "+
			"a process (w.go only mentions one) and %q",
			found.read, found.parsed, found.starting, found.hits, want)
	}

	// The pool's exec is excused, the other four are not, and gittest's
	// entry, whose file this tree lacks, is stale.
	outside, stale := judgeExecs(found.hits, allowedGitExecs)
	if got, want := files(outside), want[1:]; !slices.Equal(got, want) {
		t.Errorf("outside the allowance: %q, want %q", got, want)
	}
	if want := []string{"internal/gitlet/gitproc/gittest/gittest.go"}; !slices.Equal(stale, want) {
		t.Errorf("stale allowances: %q, want %q", stale, want)
	}
}

// importNames maps the local name of each starter or gittest import to its
// path, and is nil for a file importing no starter. A blank import names
// nothing; a dot import is past the coverage boundary (execJudge).
func importNames(file *ast.File) map[string]string {
	names := map[string]string{}
	starts := false
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		starter := starterPackage(path)
		if !starter && path != gittestPackage {
			continue
		}
		name := decltype.ImportName(path)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name == "_" || name == "." {
			continue
		}
		names[name] = path
		starts = starts || starter
	}
	if !starts {
		return nil
	}
	return names
}

// execJudge judges one parsed file: a starter (starters, starterLiterals),
// resolved against the file's own import so a renamed one counts and a local
// named exec does not, whose program is git (is). Coverage boundary: it
// follows only constants and variables the file declares, so it misses git
// reaching a starter through a parameter, a struct field, a function's result,
// another file's name, the environment, a collection built or filled by a call
// other than append, make or copy, or a cycle past once round; a dot import, a
// starter held as a function value, a Path set after the literal, a literal
// elided two collections deep; and a program another program runs (sh -c,
// ssh's remote command), which is the agent's or another machine's git.
type execJudge struct {
	file *ast.File
	// imports maps a local name to the import path it names.
	imports map[string]string
	// given is every value each constant and variable of the file is given.
	given map[variable][]given
	// following is the variables being followed, and as what, so a cycle
	// (var a = b; var b = a) ends rather than recursing.
	following map[followed]bool
}

// variable identifies one constant or variable of a file by its declaring
// node and name, as the parser's file-scoped resolution (Ident.Obj) reports
// them: a parameter called bin is never the bin another function assigned git.
type variable struct {
	decl any
	name string
}

// given is one value a constant or variable is given, and how it stands to
// the variable.
type given struct {
	expr ast.Expr
	rel  relation
	// index is where in the variable the element(s) land, nil where the
	// syntax fixes no place (an element of an element, a copy into a part, a
	// send). An expression, resolved when read: a constant index may be
	// declared further down the file.
	index ast.Expr
}

// relation is how a value given to a variable stands to it.
type relation int

const (
	// isValue: the variable IS the value: declared, defined or assigned it,
	// written through a pointer (*p = …), or pointed at (p := &bin gives bin
	// p as well as p &bin: a pointer and its target are one value, whichever
	// is written). copy(argv, src) is one too: argv holds src's elements at
	// src's indices.
	isValue relation = iota
	// inValue: the variable is an element or a key of the value — a range
	// clause's key or value.
	inValue
	// element: the value is an element of the variable, at index —
	// argv[0] = …, m[k] = …, ch <- ….
	element
	// elements: an element of the value is an element of the variable, at
	// index — copy(argv[1:], src), for argv[0] = range src.
	elements
	// key: the value is a key of the variable — m[k] = … gives m the key k
	// as well as the element.
	key
)

// followed is one variable being followed, and as what. The index path is
// deliberately NOT part of it: reaching the variable again is a cycle
// whatever is read, and a path in the key would let one that shifts it (argv =
// append(argv[1:], x)) recurse without end.
type followed struct {
	v variable
	r takenAs
}

// takenAs is what a judgement takes a value to be.
type takenAs int

const (
	// asProgram: the value is the program a starter runs, and a name's own
	// words count — exec.Command(gitPath) is git.
	asProgram takenAs = iota
	// asPart: the value is a part of the program's path (a call's argument,
	// an operand of +, an element read by its value), and only the value it
	// holds counts: filepath.Join(gitDir, "objects") is not git, and
	// filepath.Join(dir, name) is once name := "git".
	asPart
	// asValues: the value is a collection, and a value anywhere inside it
	// that is git by its value counts — argv[0] of argv := []string{"git",
	// "init"}.
	asValues
	// asElement: the value is a collection, and what an index path reads
	// from it is the program — argv[0] of argv := []string{gitPath,
	// "init"}, and never of argv := []string{"go", "test", gitDir}.
	asElement
	// asLength and asIndex are what a variable is followed as while its
	// length or its constant value is measured; they key the cycle guard
	// and are never a reading of a program.
	asLength
	asIndex
)

// variableOf is the constant or variable id names, if it names one this file
// declares.
func variableOf(id *ast.Ident) (variable, bool) {
	if id == nil || id.Obj == nil || id.Obj.Decl == nil || (id.Obj.Kind != ast.Var && id.Obj.Kind != ast.Con) {
		return variable{}, false
	}
	return variable{decl: id.Obj.Decl, name: id.Obj.Name}, true
}

func newExecJudge(file *ast.File) *execJudge {
	j := &execJudge{
		file:      file,
		imports:   importNames(file),
		given:     map[variable][]given{},
		following: map[followed]bool{},
	}
	// valueOf is the value the i'th of n names is given by values, if any.
	valueOf := func(i, n int, values []ast.Expr) (ast.Expr, bool) {
		switch {
		case len(values) == n:
			return values[i], true
		case len(values) == 1:
			// One call or comma-ok form assigning several: each name
			// gets the call, over-approximating an error or ok beside a
			// path but never missing the path.
			return values[0], true
		}
		return nil, false
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if value, ok := valueOf(i, len(n.Lhs), n.Rhs); ok {
					j.assign(lhs, value, isValue)
				}
			}
		case *ast.GenDecl:
			// A constant spec stating no values repeats the values of
			// the spec before it: const ( a = "git"; b ) gives b "git".
			var previous []ast.Expr
			for _, spec := range n.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				values := vs.Values
				if n.Tok == token.CONST && len(values) == 0 {
					values = previous
				}
				previous = values
				for i, name := range vs.Names {
					if value, ok := valueOf(i, len(vs.Names), values); ok {
						j.assign(name, value, isValue)
					}
				}
			}
		case *ast.RangeStmt:
			// A range clause's key and value are inside what it ranges
			// over. The parser declares them on a synthetic assignment
			// the tree does not hold, so they are recorded here.
			for _, e := range []ast.Expr{n.Key, n.Value} {
				if e != nil {
					j.assign(e, n.X, inValue)
				}
			}
		case *ast.SendStmt:
			// A channel is a collection: a send puts an element in it.
			j.assign(n.Chan, n.Value, element)
		case *ast.CallExpr:
			// copy is the one call that fills a collection it does not
			// return, from index 0 of what it is handed.
			if builtin(n.Fun, "copy") && len(n.Args) == 2 {
				j.assign(n.Args[0], n.Args[1], isValue)
			}
		}
		return true
	})
	return j
}

// assign records that value reaches lhs, standing to it as rel; *p = … gives
// p. A value written to an element (argv[0] = …, m[k] = …) or sent on a
// channel is given INSIDE the collection, at its index and with its key; one
// copied into a part (copy(argv[1:], src)) or ranged into an element gives it
// ELEMENTS. p := &bin and p := &argv[0] give the target p in turn, so a write
// through either reaches both. Any other target (a struct field, a call's
// result) is past the coverage boundary (execJudge).
func (j *execJudge) assign(lhs, value ast.Expr, rel relation) {
	if addr, ok := ast.Unparen(value).(*ast.UnaryExpr); ok && rel == isValue && addr.Op == token.AND {
		j.assign(addr.X, lhs, isValue)
	}
	inside, spread := rel == element, rel == inValue
	var index ast.Expr
	var keys []ast.Expr
	for {
		switch e := lhs.(type) {
		case *ast.ParenExpr:
			lhs = e.X
		case *ast.StarExpr:
			lhs = e.X
		case *ast.IndexExpr:
			// The outermost index is the element's own; one below it
			// is an element of an element, whose place is not fixed.
			if !inside {
				index = e.Index
			} else {
				index = nil
			}
			inside = true
			keys = append(keys, e.Index)
			lhs = e.X
		case *ast.SliceExpr:
			inside, spread, index = true, true, nil
			lhs = e.X
		case *ast.Ident:
			v, ok := variableOf(e)
			if !ok {
				return
			}
			switch {
			case !inside:
				j.given[v] = append(j.given[v], given{expr: value, rel: rel})
			case spread:
				j.given[v] = append(j.given[v], given{expr: value, rel: elements, index: index})
			default:
				j.given[v] = append(j.given[v], given{expr: value, rel: element, index: index})
			}
			for _, k := range keys {
				j.given[v] = append(j.given[v], given{expr: k, rel: key})
			}
			return
		default:
			return
		}
	}
}

// judged is what the judge found in one file: how many constructs start a
// process, and where each one that starts git is.
type judged struct {
	starts int
	git    []token.Pos
}

// judge visits every construct in the file that starts a process.
func (j *execJudge) judge() judged {
	var out judged
	literal := func(lit *ast.CompositeLit, field string) {
		out.starts++
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == field && j.git(kv.Value) {
				out.git = append(out.git, lit.Pos())
			}
		}
	}
	ast.Inspect(j.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			path, ok := j.imported(sel.X)
			if !ok {
				return true
			}
			at, starts := starters[path][sel.Sel.Name]
			if !starts {
				return true
			}
			out.starts++
			if at < len(n.Args) && j.git(n.Args[at]) {
				out.git = append(out.git, n.Pos())
			}
		case *ast.CompositeLit:
			if field, ok := j.literalField(n.Type); ok {
				literal(n, field)
				return true
			}
			// A collection of them with the element type elided
			// ([]*exec.Cmd{{Path: "git"}}): the inner literal has no
			// type of its own, so it is judged here.
			field, ok := j.literalField(elementType(n.Type))
			if !ok {
				return true
			}
			for _, elt := range n.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					elt = kv.Value
				}
				if inner, ok := elt.(*ast.CompositeLit); ok && inner.Type == nil {
					literal(inner, field)
				}
			}
		}
		return true
	})
	return out
}

// literalField is the field naming the program, if t is the type of a
// literal that starts a process.
func (j *execJudge) literalField(t ast.Expr) (string, bool) {
	sel, ok := t.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	path, ok := j.imported(sel.X)
	if !ok {
		return "", false
	}
	field, ok := starterLiterals[path][sel.Sel.Name]
	return field, ok
}

// elementType is the type of the elements of a slice, array or map type,
// dereferenced, and nil for any other type.
func elementType(t ast.Expr) ast.Expr {
	var elt ast.Expr
	switch t := t.(type) {
	case *ast.ArrayType:
		elt = t.Elt
	case *ast.MapType:
		elt = t.Value
	default:
		return nil
	}
	if star, ok := elt.(*ast.StarExpr); ok {
		return star.X
	}
	return elt
}

// imported is the import path x names, if x is an import's local name:
// ident.Obj is set for a name the file declares (a parameter called exec) and
// never for an import.
func (j *execJudge) imported(x ast.Expr) (string, bool) {
	ident, ok := x.(*ast.Ident)
	if !ok || ident.Obj != nil {
		return "", false
	}
	path, ok := j.imports[ident.Name]
	return path, ok
}

// builtin reports whether fun names the predeclared function name rather
// than anything the file declares.
func builtin(fun ast.Expr, name string) bool {
	id, ok := fun.(*ast.Ident)
	return ok && id.Name == name && id.Obj == nil
}

// git reports whether e, the program a starter runs, is git (is; the
// controls in TestOnlyGitprocExecsGit pin each shape).
func (j *execJudge) git(e ast.Expr) bool {
	return j.is(e, asProgram)
}

// is reports whether e is git, read as r: asProgram or asPart.
func (j *execJudge) is(e ast.Expr, r takenAs) bool {
	names := r == asProgram
	switch e := e.(type) {
	case *ast.BasicLit:
		return gitLiteral(e)
	case *ast.Ident:
		return names && gitWord(e.Name) || j.follow(e, r, nil)
	case *ast.SelectorExpr:
		return names && gitWord(e.Sel.Name)
	case *ast.ParenExpr:
		return j.is(e.X, r)
	case *ast.StarExpr:
		return j.is(e.X, r)
	case *ast.UnaryExpr:
		switch e.Op {
		case token.AND:
			return j.is(e.X, r)
		case token.ARROW:
			// A receive reads an element of the channel, at no place
			// the syntax fixes.
			return j.holds(e.X) || names && j.element(e.X, []int{-1})
		}
	case *ast.TypeAssertExpr:
		return j.is(e.X, r)
	case *ast.SliceExpr:
		// A part of a string is judged as the string (gitPath[:]).
		return j.is(e.X, r)
	case *ast.IndexListExpr:
		// A generic instance is the function it instantiates.
		return j.is(e.X, r)
	case *ast.IndexExpr:
		// An element: git when the collection holds git anywhere or its
		// key is git (tools["git"]), and, read as the program, when what
		// the index reads is.
		return j.holds(e.X) || j.is(e.Index, asPart) ||
			names && j.element(e.X, []int{j.index(e.Index)})
	case *ast.BinaryExpr:
		return e.Op == token.ADD && (j.is(e.X, asPart) || j.is(e.Y, asPart))
	case *ast.CallExpr:
		// string(name) is name; the parser cannot tell any other
		// conversion from a call, and judges it as one below.
		if builtin(e.Fun, "string") && len(e.Args) == 1 {
			return j.is(e.Args[0], r)
		}
		// The callee read as a program: a git-word name (gitPath()), or
		// a function a variable or collection holds (find := findGit;
		// find()).
		if names && j.is(e.Fun, asProgram) {
			return true
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if path, ok := j.imported(sel.X); ok && path == gittestPackage && gittestBinaries[sel.Sel.Name] {
				return true
			}
		}
		// Git by an argument's value, or a collection argument, spread
		// or whole, holding one (filepath.Join(parts...)).
		return slices.ContainsFunc(e.Args, j.within)
	}
	return false
}

// holds reports whether e is a collection holding a value git by its value
// anywhere inside it, as an element or a key, at any depth. Over-approximating
// on purpose (keys, values and nesting levels are not told apart): a false hit
// names itself, where a missed one says nothing.
func (j *execJudge) holds(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.CompositeLit:
		return slices.ContainsFunc(e.Elts, func(elt ast.Expr) bool {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				return j.within(kv.Key) || j.within(kv.Value)
			}
			return j.within(elt)
		})
	case *ast.Ident:
		return j.follow(e, asValues, nil)
	case *ast.ParenExpr:
		return j.holds(e.X)
	case *ast.StarExpr:
		return j.holds(e.X)
	case *ast.UnaryExpr:
		// &argv is argv, and <-ch an element of ch.
		return (e.Op == token.AND || e.Op == token.ARROW) && j.holds(e.X)
	case *ast.IndexExpr:
		return j.holds(e.X)
	case *ast.SliceExpr:
		return j.holds(e.X)
	case *ast.CallExpr:
		// append is the one call whose result holds what its arguments
		// do; any other is past the coverage boundary.
		if builtin(e.Fun, "append") {
			return slices.ContainsFunc(e.Args, j.within)
		}
	}
	return false
}

// within reports whether e, found inside a collection, is git by its value
// or holds such a value itself.
func (j *execJudge) within(e ast.Expr) bool {
	return j.is(e, asPart) || j.holds(e)
}

// element reports whether what the index path reads from the collection e
// (outermost index first, -1 where the syntax fixes none) is git read as the
// program; an empty path reads e itself. A literal the file writes FIXES what
// a constant index reads, through variables, append, make and element
// assignments, and only that element counts: argv[0] of []string{"go",
// "test", gitDir} is not git. Unfixed (a variable index, a range, a receive),
// every element is read; a git-word collection name is git whatever is read
// (gitArgv[0], <-gitPaths).
func (j *execJudge) element(e ast.Expr, path []int) bool {
	if len(path) == 0 {
		return j.is(e, asProgram)
	}
	at, rest := path[0], path[1:]
	switch e := e.(type) {
	case *ast.CompositeLit:
		if elt, fixed := j.positional(e, at); fixed {
			return elt != nil && j.element(elt, rest)
		}
		return slices.ContainsFunc(e.Elts, func(elt ast.Expr) bool {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				return j.element(kv.Key, rest) || j.element(kv.Value, rest)
			}
			return j.element(elt, rest)
		})
	case *ast.Ident:
		return gitWord(e.Name) || j.follow(e, asElement, path)
	case *ast.SelectorExpr:
		return gitWord(e.Sel.Name)
	case *ast.ParenExpr:
		return j.element(e.X, path)
	case *ast.StarExpr:
		return j.element(e.X, path)
	case *ast.TypeAssertExpr:
		return j.element(e.X, path)
	case *ast.UnaryExpr:
		switch e.Op {
		case token.AND:
			return j.element(e.X, path)
		case token.ARROW:
			return j.element(e.X, slices.Concat([]int{-1}, path))
		}
	case *ast.SliceExpr:
		low := 0
		if e.Low != nil {
			low = j.index(e.Low)
		}
		if at >= 0 && low >= 0 {
			return j.element(e.X, slices.Concat([]int{at + low}, rest))
		}
		return j.element(e.X, slices.Concat([]int{-1}, rest))
	case *ast.IndexExpr:
		// e is itself an element of e.X, so the path reads on from there.
		return j.element(e.X, slices.Concat([]int{j.index(e.Index)}, path))
	case *ast.CallExpr:
		if j.is(e.Fun, asProgram) {
			return true
		}
		if builtin(e.Fun, "append") && len(e.Args) > 0 {
			return j.appended(e, path)
		}
	}
	return false
}

// positional is the element of lit at index at (nil when none) and whether
// lit fixes it at all: not for a non-constant index, nor for a literal keyed
// by anything but constant integers.
func (j *execJudge) positional(lit *ast.CompositeLit, at int) (ast.Expr, bool) {
	if at < 0 {
		return nil, false
	}
	var found ast.Expr
	next := 0
	for _, elt := range lit.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			if next = j.index(kv.Key); next < 0 {
				return nil, false
			}
			elt = kv.Value
		}
		if next == at {
			found = elt
		}
		next++
	}
	return found, true
}

// appended reports whether what path reads from append(base, …) is git read
// as the program: the base's element below the base's fixed length, the
// appended argument at that index where the length is exact, else the base's
// element or any appended one.
func (j *execJudge) appended(call *ast.CallExpr, path []int) bool {
	at, rest := path[0], path[1:]
	base, args := call.Args[0], call.Args[1:]
	spread := call.Ellipsis.IsValid() && len(args) == 1
	n, exact := j.length(base)
	switch {
	case at >= 0 && at < n:
		return j.element(base, path)
	case at >= 0 && exact && spread:
		return j.element(args[0], slices.Concat([]int{at - n}, rest))
	case at >= 0 && exact:
		return at-n < len(args) && j.element(args[at-n], rest)
	}
	if j.element(base, path) {
		return true
	}
	if spread {
		return j.element(args[0], slices.Concat([]int{-1}, rest))
	}
	return slices.ContainsFunc(args, func(arg ast.Expr) bool { return j.element(arg, rest) })
}

// unbounded is a collection's length when measuring it reaches itself (argv =
// append(argv, x)): appending only lengthens, so that path bounds nothing.
const unbounded = math.MaxInt

// length is the fewest elements the collection e can hold, as far as the
// syntax fixes it, and whether it holds exactly that many: a literal's
// elements, make's length, what append adds to its base, and the least of a
// variable's values. It is 0 where nothing fixes it.
func (j *execJudge) length(e ast.Expr) (int, bool) {
	switch e := e.(type) {
	case *ast.CompositeLit:
		n, next := 0, 0
		for _, elt := range e.Elts {
			if kv, ok := elt.(*ast.KeyValueExpr); ok {
				if next = j.index(kv.Key); next < 0 {
					return 0, false
				}
			}
			next++
			n = max(n, next)
		}
		return n, true
	case *ast.Ident:
		v, ok := variableOf(e)
		if !ok {
			return 0, false
		}
		key := followed{v: v, r: asLength}
		if j.following[key] {
			return unbounded, false
		}
		j.following[key] = true
		defer delete(j.following, key)
		least, values, cyclic := unbounded, 0, false
		exact := true
		for _, g := range j.given[v] {
			switch g.rel {
			case isValue:
				n, ex := j.length(g.expr)
				if n == unbounded {
					cyclic = true
					continue
				}
				values++
				least, exact = min(least, n), exact && ex
			case inValue:
				// An element of a ranged collection: no fixed length.
				return 0, false
			}
		}
		if values == 0 {
			return 0, false
		}
		return least, exact && values == 1 && !cyclic
	case *ast.ParenExpr:
		return j.length(e.X)
	case *ast.StarExpr:
		return j.length(e.X)
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return j.length(e.X)
		}
	case *ast.CallExpr:
		switch {
		case builtin(e.Fun, "make") && len(e.Args) >= 2:
			if _, slice := e.Args[0].(*ast.ArrayType); slice {
				if n := j.index(e.Args[1]); n >= 0 {
					return n, true
				}
			}
		case builtin(e.Fun, "append") && len(e.Args) > 0:
			n, exact := j.length(e.Args[0])
			if n == unbounded || e.Ellipsis.IsValid() {
				return n, false
			}
			return n + len(e.Args) - 1, exact
		}
	}
	return 0, false
}

// index is the value of the constant integer index e — a literal, or a
// constant the file declares as one — and -1 where e is not one.
func (j *execJudge) index(e ast.Expr) int {
	switch e := ast.Unparen(e).(type) {
	case *ast.BasicLit:
		if e.Kind != token.INT {
			return -1
		}
		n, err := strconv.ParseInt(e.Value, 0, 0)
		if err != nil || n < 0 {
			return -1
		}
		return int(n)
	case *ast.Ident:
		v, ok := variableOf(e)
		key := followed{v: v, r: asIndex}
		if !ok || e.Obj.Kind != ast.Con || len(j.given[v]) != 1 || j.following[key] {
			return -1
		}
		j.following[key] = true
		defer delete(j.following, key)
		return j.index(j.given[v][0].expr)
	}
	return -1
}

// follow reports whether id names a constant or variable given git, read as
// r (as asElement, at the index path), anywhere in the file. Flow-insensitive
// on purpose: a variable that ever holds git is one a reader takes for git.
func (j *execJudge) follow(id *ast.Ident, r takenAs, path []int) bool {
	v, ok := variableOf(id)
	key := followed{v: v, r: r}
	if !ok || j.following[key] {
		return false
	}
	j.following[key] = true
	defer delete(j.following, key)
	return slices.ContainsFunc(j.given[v], func(g given) bool { return j.reads(g, r, path) })
}

// reads reports whether g, one value a variable is given, makes the variable
// git read as r (at the index path, read as asElement).
func (j *execJudge) reads(g given, r takenAs, path []int) bool {
	switch r {
	case asProgram, asPart:
		switch g.rel {
		case isValue:
			return j.is(g.expr, r)
		case inValue:
			// Inside a ranged collection: git by its value anywhere
			// there, and, read as the program, any element read as one
			// (range []string{"sh", gitPath}).
			return j.holds(g.expr) || r == asProgram && j.element(g.expr, []int{-1})
		}
	case asValues:
		switch g.rel {
		case isValue, inValue, elements:
			return j.holds(g.expr)
		case element, key:
			return j.within(g.expr)
		}
	case asElement:
		at, rest := path[0], path[1:]
		switch g.rel {
		case isValue:
			return j.element(g.expr, path)
		case inValue:
			return j.element(g.expr, slices.Concat([]int{-1}, path))
		case element, elements:
			if g.index != nil {
				if i := j.index(g.index); at >= 0 && i >= 0 && i != at {
					return false
				}
			}
			if g.rel == elements {
				return j.element(g.expr, slices.Concat([]int{-1}, rest))
			}
			return j.element(g.expr, rest)
		case key:
			// A key is what a range over a map reads, at no fixed
			// place, and never what an index reads.
			return at < 0 && j.element(g.expr, rest)
		}
	}
	return false
}

// gitLiteral reports whether lit is a string naming git or one of git's own
// programs.
func gitLiteral(lit *ast.BasicLit) bool {
	if lit.Kind != token.STRING {
		return false
	}
	s, err := strconv.Unquote(lit.Value)
	return err == nil && gitProgram(s)
}

// gitProgram reports whether the last element of the slash-separated path s
// is git or one of git's own programs, git-<name> (git-upload-pack,
// git-http-backend…), which read git's configuration as git does. Never
// gitlab-runner, nor a path inside a repository (.git/hooks/pre-commit).
func gitProgram(s string) bool {
	base := s[strings.LastIndex(s, "/")+1:]
	return base == "git" || strings.HasPrefix(base, "git-")
}

// gitWord reports whether name has a word equal to git, case folded, words
// split at case changes, at digits and at underscores: git, gitPath, realGit,
// GitBinary, GITPath and my_git_2 do; gitlab, digit, legit, gitproc and GITLAB
// do not.
func gitWord(name string) bool {
	return slices.ContainsFunc(words(name), func(w string) bool { return strings.EqualFold(w, "git") })
}

// words splits an identifier into its words: at an underscore, between a
// digit and a non-digit, before an upper-case letter that follows a
// lower-case one (realGit), and before the last capital of a run followed by a
// lower-case letter (GITPath is GIT and Path).
func words(name string) []string {
	var out []string
	rs := []rune(name)
	start := 0
	flush := func(end int) {
		if end > start {
			out = append(out, string(rs[start:end]))
		}
		start = end
	}
	for i, r := range rs {
		if r == '_' {
			flush(i)
			start = i + 1
			continue
		}
		if i == start {
			continue
		}
		prev := rs[i-1]
		switch {
		case unicode.IsDigit(r) != unicode.IsDigit(prev):
			flush(i)
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush(i)
		case unicode.IsUpper(r) && unicode.IsUpper(prev) && i+1 < len(rs) && unicode.IsLower(rs[i+1]):
			flush(i)
		}
	}
	flush(len(rs))
	return out
}

// matchesInSource runs the matcher over one snippet, for the controls.
func matchesInSource(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", src, 0)
	if err != nil {
		t.Fatalf("parse control %q: %v", src, err)
	}
	if len(importNames(file)) == 0 {
		return false
	}
	return len(newExecJudge(file).judge().git) > 0
}
