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

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestOnlyGitprocExecsGit fails the build when any Go file of the module
// outside cmd/ but the pool's own exec and gittest's starts git (ADR-0008).
//
// # Why it fails the build rather than reporting
//
// A git process is configured by six decisions — the environment, the
// configuration, the repository it may trust, the slot, the end of its
// process tree and what its failure means — and gitproc's package doc is the
// record of what each one cost when it was got wrong: a repository config that
// serves every object to anyone, a planted commondir that redirects refs, a
// WaitDelay that cuts the tail off a slow clone, a cancel that ends the leader
// and leaves the group running. A second exec.Command is a place to forget one
// of them, and nothing about it looks wrong: the process starts, the test
// passes, the push succeeds. So the pool is the one way in, and this is what
// makes it the one way rather than the recommended one.
//
// # Why test files too
//
// A test's git needs the same hermetic environment everywhere — a
// developer's own ~/.gitconfig signing commits, an init.defaultBranch, a
// url.insteadOf or a credential helper otherwise decides what a fixture
// builds — and every suite that drives git (the ref grammar's oracle, the
// cache's and the host's fixtures, the fleet cases) would otherwise write that
// rule for itself, which is ADR-0008's failure in test code. gittest is the
// one client, and the allowance below is keyed by FILE rather than by package
// so that gitproc's own tests are held to it like everybody else's.
//
// # Where it looks
//
// Every .go file of the module, production and test alike — internal/, and
// the Go beside it (static/'s tests, scripts/) — but testdata, which is not
// compiled, and cmd/, which is outside the walk on purpose: `crewlet code
// import` and `export` run the OPERATOR's git, with the operator's
// environment and credentials, which is the one git the pool must not
// configure. The walk starts at the module root and steps over cmd/ by name,
// so a directory that is added beside internal/ is read without anybody
// remembering to add it.
//
// # What it flags
//
// A construct that starts a process — os/exec's Command and CommandContext,
// os.StartProcess, syscall's Exec, ForkExec and StartProcess,
// golang.org/x/sys/unix's Exec, and a keyed literal (with or without &) of
// os/exec's Cmd or of net/http/cgi's Handler, which starts its Path for every
// request it serves and is the standard library's way of serving
// git-http-backend — whose PROGRAM is git. The construct is a selector
// resolved against the file's own import of its package, so a renamed import
// is covered and a local variable named exec is not. The program is git when
// it is:
//
//   - a string literal whose last path element is git or one of git's own
//     programs, git-<name>: "git", "/usr/bin/git", "git-upload-pack",
//     "/usr/lib/git-core/git-http-backend" — each a git process whose
//     configuration is exactly what the pool exists to own; never
//     "gitlab-runner" or ".git/hooks/pre-commit";
//   - a name with a WORD equal to git, words split at case changes, digits and
//     underscores — git, gitPath, realGit, GitBinary, o.Git; never gitlab,
//     digit or legit;
//   - a call whose callee, read as a program, is git — a name above
//     (gitPath(), o.GitBinary(), findGit[T]()), or a variable or an element
//     holding one (find := findGit; find()) — or any of whose arguments is
//     git by its VALUE: a literal above, a constant or variable the file gives
//     one, or a collection holding one, handed whole or spread
//     (filepath.Join(dir, "git"), filepath.Join(d, name) after name := "git",
//     filepath.Join(parts...)). A name's own words make a PROGRAM git and
//     never a part of one, so filepath.Join(gitDir, "objects") is not;
//   - a concatenation one of whose operands is git by its value (dir + "/git");
//   - a call of what gittest hands out as a git binary — Require and
//     RequireNode (the PATH's git, held to each floor), NodeFloor and
//     ClientFloor (the floors' own builds), and the stand-ins Fake, Wrap and
//     Recording, the last two of which exec the real git;
//   - a constant or variable given one of those anywhere in the same file —
//     declared, defined, assigned or written through a pointer (*p = …; a
//     pointer and what it points at are one value here, whichever of the two
//     is written), a constant repeating the spec before it (const ( a =
//     "git"; b )) — followed through the object the parser resolves it to,
//     never its name: bin := gittest.Require(t) then exec.Command(bin) is the
//     laundering a test reaches for without meaning to, and const bin = "git"
//     the one production code does. Flow-insensitively: a variable that ever
//     holds git is git wherever it is read;
//   - an ELEMENT of a collection — read by an index (argv[0], tools[k]), as a
//     range clause's key or value, or received from a channel — when the
//     collection holds a value that is git by its value anywhere inside it
//     (as an element or a key, at any depth: in a literal, built by append,
//     filled by copy, assigned element by element — argv[0] = …, m[k] = … —
//     or sent on), when the key is git by its value (tools["git"]), or when
//     what is read is git READ AS A PROGRAM: the collection's own name has a
//     git word (gitArgv[0], o.GitArgv[0], <-gitPaths), or the element read
//     does. Which element that is, the file's literals decide where they can:
//     a constant index into a literal the file writes — through the variables
//     it is given to, the appends that grow it, make and the elements
//     assigned one by one, at any depth — reads that element alone, so
//     argv[0] of []string{gitPath, "init"} is git and of []string{"go",
//     "test", gitDir} is not, nor is c[0] for each c of [][]string{{"go",
//     "test", gitDir}}. Where nothing decides it — an index that is not a
//     constant, a range clause, a receive — every element is read;
//   - a conversion (string(name)), a slice of a string, a type assertion, or
//     a parenthesised, dereferenced or addressed one of those.
//
// A literal of a collection of Cmds or Handlers whose element type is elided
// ([]*exec.Cmd{{Path: "git"}}) is judged like the literal it elides.
//
// # Coverage boundary
//
// It follows the constants and variables a file declares, through everything
// above, and nothing else. So it does not see:
//
//   - a git path that reaches the starter through anything else with no git
//     word, EVEN WHEN THE SAME FILE GIVES IT GIT: what a call passes to a
//     parameter (run("git") into func run(bin string)), what a struct field
//     is set to (o.bin = "git", T{bin: "git"}, an Options{Path: …} later
//     run as opts.Path), what a function returns (func bin() string { return
//     "git" }), or a package-level name declared in another file;
//   - a program named by the environment (os.Getenv("GIT"));
//   - a collection built by a call other than append and make, or filled by
//     one other than copy (strings.Fields("git status"), slices.Insert(argv,
//     0, "git")), and a range over one;
//   - a git-word name moved into place by a definition that reaches back to
//     itself (argv = argv[1:], then argv[0]): a cycle is followed once round
//     and no further, so only the values it holds are seen there;
//   - a program another program runs: a shell string (`sh -c "git …"`), which
//     is how a box runs its own git, and that git is the agent's rather than
//     the engine's; or ssh's remote command (`ssh host git-upload-pack …`),
//     which runs on another machine;
//   - a dot import; a function value (f := exec.Command); a Path assigned to
//     a Cmd or a Handler after it was built (new(exec.Cmd), then .Path = …);
//     a literal whose type is elided two collections deep;
//   - cmd/ (above).
//
// A name WITH a git word that does not run git — a GitHub CLI held as
// cfg.GitHub, an element of a collection named gitArgs — is a hit, and the
// remedy is a name that says what the program is.
//
// If the rule ever legitimately goes away, DELETE this guard rather than
// weakening it. A guard kept alive past its subject is how an allowance list
// quietly grows.
//
// # Cost
//
// It reads every .go file once and parses whole only those whose bytes spell
// one of a starter's names and that import that starter's package — measured
// when this was written, 43 of some 2,550 files, of which 34 hold a construct
// that starts a process (the rest name exec.Cmd as a type, or spell a starter
// only in a comment or a string; the test logs both counts on every run, so a
// figure and its label cannot drift apart) — in about 0.6 s under the race
// detector.
func TestOnlyGitprocExecsGit(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	found := gitExecs(t, root)

	if found.read == 0 || found.parsed == 0 || found.starting == 0 {
		t.Fatalf("read %d files, parsed %d and found %d that start a process — this guard is "+
			"certifying nothing. Check the module root and the walk", found.read, found.parsed, found.starting)
	}
	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the matcher has gone inert. The verdicts below are
	// known, so a matcher that stopped recognising a shape fails HERE rather
	// than certifying a clean tree — and every source it flags must pass the
	// prefilter, or the walk would skip the very file it would have caught.
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
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/code/gitproc/gittest")
			func f(t T) { exec.Command(gittest.Require(t)) }`, true},
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/code/gitproc/gittest")
			func f(t T) { bin := gittest.Require(t); exec.Command(bin, "init") }`, true},
		{`package p; import ("os/exec"; gt "github.com/crewlet/crewlet/internal/code/gitproc/gittest")
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
		// The coverage boundary, pinned so the doc cannot claim less than
		// the matcher sees: a struct field, a parameter, a function's result
		// and the environment are not followed.
		{`package p; import "os/exec"; func f(o *O) { o.bin = "git"; exec.Command(o.bin) }`, false},
		{`package p; import "os/exec"; func run(bin string) { exec.Command(bin) }; func f() { run("git") }`, false},
		{`package p; import "os/exec"; func bin() string { return "git" }; func f() { exec.Command(bin()) }`, false},
		{`package p; import ("os"; "os/exec"); func f() { exec.Command(os.Getenv("GIT")) }`, false},
		{`package p; import "os/exec"; func f(gitDirs map[string]string) { exec.Command("go", gitDirs["x"]) }`, false},
		// A variable is followed by its own object, never by its name: a
		// parameter called bin is not the bin assigned git elsewhere.
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/code/gitproc/gittest")
			func f(t T) { bin := gittest.Require(t); _ = bin }
			func g(bin string) { exec.Command(bin) }`, false},
		{`package p; import ("os/exec"; "github.com/crewlet/crewlet/internal/code/gitproc/gittest")
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
// the allowances no exec uses. The allowance is exact in BOTH directions: an
// entry whose site has moved or gone must be deleted, or the list becomes
// somewhere a future exec hides behind a stale excuse.
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

// allowedGitExecs is the closed set of FILES that may exec git, each with the
// reason. Keyed by file rather than by package: gitproc's own tests sit in the
// pool's directory and drive their client git through gittest like every
// other suite, so excusing the directory would excuse them.
var allowedGitExecs = map[string]string{
	"internal/code/gitproc/run.go": "Pool.Run's exec.Command(p.git, …) — the one exec this " +
		"gate exists to keep the only one",
	"internal/code/gitproc/gittest/gittest.go": "the client git every test drives: " +
		"readVersion's `git version` (the PATH's git and each floor's, once a binary), " +
		"Client.Try's exec.CommandContext(ctx, c.git, …) and Client.HTTPBackend's " +
		"cgi.Handler{Path: c.git} (the other host a mirror's case dials), hermetic for the reasons the pool's " +
		"environment is built",
}

// gittestPackage is the import path of the client git, whose binary-handing
// functions are git wherever they are called.
const gittestPackage = "github.com/crewlet/crewlet/internal/code/gitproc/gittest"

// gittestBinaries are the gittest functions whose (first) result is a git
// binary: Require and RequireNode are the real git on PATH, NodeFloor and
// ClientFloor the floors' own builds, Fake a stand-in, and Wrap and Recording
// stand-ins that exec the real git. The anchor below fails the build when one
// is renamed, so this list cannot silently stop naming anything.
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
// to start, by the import path of their package and their type, with the
// field that names the program: os/exec's Cmd, which runs it later, and
// net/http/cgi's Handler, which starts it for every request it serves.
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

// starterNames is the prefilter, per package: the identifiers a selector on
// that package is compared against — its calls and its literals' types,
// taken from the same tables, so the prefilter cannot fall behind them.
// anyStarterName is their union, asked first so a file spelling none of them
// is never parsed even for its imports.
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

// admit reports whether the walk parses src whole.
//
// THE PREFILTER IS EXACT. A file is parsed whole only if, for a starter
// package its imports name — parsed, never searched for as text, because an
// import path is a string literal and a string can be spelled with escapes
// its bytes never show — its bytes spell one of THAT package's names (an
// identifier has no escapes). Every construct this judges is a selector on
// such an import naming such a name, so a file failing it holds none. Per
// package rather than for the union, because os is imported almost
// everywhere and its one starter is rare: a file importing os and spelling
// Command — an Execute, a CommandLine — holds no process start for that.
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

// execWalk is what one walk found. read counts every .go file the walk
// reached, before the prefilter, so the floor measures the walk rather than
// the filter; parsed counts the files the prefilter admitted and that were
// parsed whole, and starting the ones of those holding a construct that
// starts a process, git or not — counted by the judge that visits each, so
// what the log reports is what was measured.
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
			// the operator's git (the coverage boundary above) — that
			// directory only, never one named cmd somewhere below.
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

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: a test file starting git under a
// renamed import and one starting it through an exec.Cmd literal (both hits,
// neither allowed), the pool's own exec (a hit, allowed), a test BESIDE
// internal/ starting git-upload-pack and one in a directory named cmd below
// the root (hits — the walk starts at the root and steps over the module's
// own cmd/ only), a file importing os/exec that runs sh and a script running
// go (parsed, each a process start, no hit), a file importing os/exec that
// spells a starter only in a comment (parsed, no process start), and four
// files the walk reads and never parses whole: one spelling a starter that
// imports none of their packages, one importing os/exec that spells none,
// one importing os that spells only os/exec's names — each with a body that
// is not Go, so a walk that parsed past its imports would fail — and one
// spelling nothing at all. And two execs of git the walk must never reach:
// one in testdata and one in the module's cmd/. Then THE VERDICT, against
// the tree's own allowance, so both of its directions are exercised: the
// main gate can only ever show the stale one failing on a tree that has
// drifted.
func TestTheExecWalkReadsEveryFileImportingExec(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/p/p_test.go": "package p\n\nimport ex \"os/exec\"\n\nvar _ = ex.Command(\"git\")\n",
		"internal/p/cmd.go": "package p\n\nimport \"os/exec\"\n\n" +
			"var _ = &exec.Cmd{Path: gitBinary}\n",
		"internal/code/gitproc/run.go": "package gitproc\n\nimport \"os/exec\"\n\n" +
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
	want := []string{"internal/code/gitproc/run.go", "internal/p/cmd.go", "internal/p/p_test.go",
		"internal/v/cmd/c.go", "static/serve_test.go"}
	if found.read != 12 || found.parsed != 8 || found.starting != 7 || !slices.Equal(files(found.hits), want) {
		t.Errorf("read %d, parsed %d, %d starting a process, hits %+v; want 12 read (testdata and "+
			"cmd/ never), 8 parsed (r.go, s.go, u.go and x.go never past the prefilter), 7 starting "+
			"a process (w.go only mentions one) and %q",
			found.read, found.parsed, found.starting, found.hits, want)
	}

	// Judged against the tree's own allowance, the pool's exec is excused,
	// the other four are not, and gittest's entry — whose file this tree
	// lacks — is reported stale rather than kept.
	outside, stale := judgeExecs(found.hits, allowedGitExecs)
	if got, want := files(outside), want[1:]; !slices.Equal(got, want) {
		t.Errorf("outside the allowance: %q, want %q", got, want)
	}
	if want := []string{"internal/code/gitproc/gittest/gittest.go"}; !slices.Equal(stale, want) {
		t.Errorf("stale allowances: %q, want %q", stale, want)
	}
}

// importNames maps the local name of each package the file imports by name
// that the judge reads — a starter's, or gittest's — to its import path, and
// is nil for a file importing no starter. A blank import names nothing, and a
// dot import would need identifier resolution rather than a selector — none
// exists in this tree, and the coverage boundary says the gate does not see
// one.
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

// execJudge judges one parsed file.
type execJudge struct {
	file *ast.File
	// imports maps a local name to the import path it names.
	imports map[string]string
	// given is every value each constant and variable of the file is given.
	given map[variable][]given
	// following is the variables being followed, each as what it is
	// followed for, so a cycle (var a = b; var b = a, argv = append(argv,
	// x)) ends rather than recursing.
	following map[followed]bool
}

// variable identifies one constant or variable of a file by the node that
// declares it and its name, as the parser's file-scoped resolution
// (Ident.Obj, which the tree's other syntax guards read too) reports them —
// so two variables sharing a name in two functions are two variables here,
// and a parameter called bin is never the bin another function assigned git.
type variable struct {
	decl any
	name string
}

// given is one value a constant or variable is given, and how it stands to
// the variable.
type given struct {
	expr ast.Expr
	rel  relation
	// index is where in the variable an element or elements land, and nil
	// where the syntax fixes no place: an element of an element, a copy
	// into a part of the collection, a send. Kept as the expression and
	// resolved when it is read, because a constant index may be declared
	// further down the file.
	index ast.Expr
}

// relation is how a value given to a variable stands to it.
type relation int

const (
	// isValue: the variable IS the value — declared, defined or assigned
	// it, written through a pointer to it (*p = …), or holding the address
	// of it (p := &bin gives bin the value p as well as p the value &bin,
	// because a pointer and what it points at are one value here, whichever
	// of the two is written). A copy into the whole of a collection is one
	// too: copy(argv, src) leaves argv holding src's elements at src's
	// indices.
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

// followed is one variable being followed, and as what. The index path a
// collection is read at is deliberately NOT part of it: to reach a variable
// again while following it is to follow its definition round a cycle,
// whatever is being read of it, and a key holding the path would let a cycle
// that shifts the path (argv = append(argv[1:], x)) recurse without end.
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
	// asPart: the value is a part of the program's path — an argument of
	// the call that builds it, an operand of +, an element of a collection
	// read by its value — and only the value it holds counts:
	// filepath.Join(gitDir, "objects") is not git, and filepath.Join(dir,
	// name) is once name := "git".
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
			// One call (or comma-ok form) assigning several: each name
			// is the call's, which over-approximates the error or ok
			// beside a path and never under-approximates the path.
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
			// A range clause's key and value are each something inside
			// what it ranges over. The parser declares them on a
			// synthetic assignment the tree does not hold, which is why
			// they are recorded here rather than as an AssignStmt.
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

// assign records that value reaches lhs, standing to it as rel. A name is
// given it; what a pointer points at is the pointer's, so *p = … gives p. A
// value written to an element of a collection is given INSIDE the
// collection, at its index and with its key (argv[0] = …, m[k] = …), as is
// one sent on a channel (rel element) at no index; a value copied into a part
// of one (copy(argv[1:], src)), or ranged into an element of one, gives the
// collection its ELEMENTS. And a name given the address of another (p :=
// &bin, p := &argv[0]) gives that other the name in turn, so a write through
// either reaches both. Any other target — a struct field, a call's result —
// is not followed (the coverage boundary).
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
			// A collection of them, whose elements' type is elided:
			// []*exec.Cmd{{Path: "git"}}. The inner literal has no type
			// of its own, so this is the one place it can be judged.
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

// imported is the import path x names, if x is the local name of an import
// rather than anything declared in the file. ident.Obj is set for a name the
// file declares — a variable or a parameter called exec — and never for an
// import's.
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

// git reports whether e, the program a starter runs, is git (the rules in
// TestOnlyGitprocExecsGit's doc).
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
		// An element of a collection: git by its value when the
		// collection holds a git value anywhere or its key is one
		// (tools["git"]), and — read as the program — when what the
		// index reads is git read as a program.
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
		// The callee read as a program: a name with a git word
		// (gitPath(), o.GitBinary()), and what a variable or a collection
		// holding a function gives it (find := findGit; find()).
		if names && j.is(e.Fun, asProgram) {
			return true
		}
		if sel, ok := e.Fun.(*ast.SelectorExpr); ok {
			if path, ok := j.imported(sel.X); ok && path == gittestPackage && gittestBinaries[sel.Sel.Name] {
				return true
			}
		}
		// Git by an argument's value: an argument that is one, or a
		// collection — spread into the call or handed whole — holding one
		// (filepath.Join(parts...), strings.Join(parts, "/")).
		return slices.ContainsFunc(e.Args, j.within)
	}
	return false
}

// holds reports whether e is a collection holding a value that is git by its
// value anywhere inside it — as an element or a key, at any depth.
// Over-approximating on purpose: a map's key and its value, or two levels of
// a nested slice, are not told apart, because a collection with git anywhere
// in it is one a reader would take an element of for git, and a false hit
// names itself where a missed one says nothing.
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
		// do; every other collection a call builds is the boundary's.
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

// element reports whether what the index path reads from the collection e —
// outermost index first, -1 for one the syntax does not fix — is git read as
// the program; an empty path reads e itself.
//
// A literal the file writes FIXES what a constant index reads — through the
// variables it is given to, the appends that grow it, make, and the elements
// assigned one by one — and then only that element counts: argv[0] of
// []string{"go", "test", gitDir} is not git, nor is c[0] for every c ranged
// over [][]string{{"go", "test", gitDir}}. Where nothing fixes it — an index
// that is not a constant, a range clause, a receive — every element the
// collection may hold is read; and a collection's own name with a git word
// is git whatever is read of it (gitArgv[0], <-gitPaths).
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

// positional is the element of lit at index at — nil when it has none there
// — and whether lit fixes that at all: not for an index that is not a
// constant, nor for a literal keyed by anything but constant integers (a map
// keyed by names).
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
// as the program: the base's own element below the length the syntax fixes
// for the base, the appended argument at that index where the base's length
// is exact, and otherwise the base's element or any appended one.
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

// unbounded is the length a collection measures at when measuring it reaches
// the collection itself (argv = append(argv, x)): appending only lengthens,
// so that path bounds nothing below what the collection's other values do.
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
				// An element of what a range clause ranges over: no
				// length the syntax fixes.
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
// r — and, read as asElement, at the index path — anywhere in the file.
// Flow-insensitive on purpose: a variable that ever holds git is one a reader
// would take for git, and an exec of it is the laundering this follows.
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
			// Something inside what a range clause ranges over: git by
			// its value anywhere there, and — read as the program — any
			// element read as one (for _, bin := range []string{"sh",
			// gitPath}).
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
			// A key is what a range clause reads of a map, which reads
			// at no place the syntax fixes, and never what an index does.
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
// is git or one of git's own programs, git-<name> — git-upload-pack,
// git-receive-pack, git-upload-archive, git-http-backend, git-shell — which
// are git processes reading git's configuration as much as git itself.
// Never gitlab-runner, whose name merely starts with the letters, nor a path
// inside a repository (.git/hooks/pre-commit).
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
