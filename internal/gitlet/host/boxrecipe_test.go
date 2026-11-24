package host_test

import (
	"bytes"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// THE BOX RECIPES CLONE FROM GITLET on the pinned git, as the floors'
// arm holds them to on the client floor's own build (TestTheGitFloorsPassTheSuite).
func TestTheBoxRecipesCloneFromGitlet(t *testing.T) {
	t.Parallel()
	boxRecipes(t, gitlettest.SoloOptions{}, gittest.Require(t))
}

// boxRecipes is the box half of Gitlet's suite: the steps giving a coding box
// its repositories (the version check, the coding clone, a check's commit) run
// as a box runs them, `sh -c` in its working directory under its git (the pin,
// or the client floor's 2.40.4) with EXACTLY the engine's box environment
// (sandbox.BoxGit, ADR-0093), its token only in the file it names, over a
// global person's helper the reset keeps out.
//
// Mutations, each red: a commit hook in the coding recipe; the clone at HEAD,
// not its base; no --skip-checks (the cone naming [odd] and !bang refused); no
// --single-branch (every seat's work tracked); no reset of the helper list
// (every request refused); the helper back in GIT_CONFIG_COUNT (the harness's
// blank entries refuse the fetch), or reading the token from a variable.
func boxRecipes(t *testing.T, opts gitlettest.SoloOptions, git string) {
	t.Helper()
	ctx := t.Context()
	f := gitlettest.SoloNode(t, opts)
	f.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: gitlettest.Settings()})
	person := gitlettest.NewPerson(t, f, "ceo")
	base := f.Nodes()[0].URL() + "/git"
	url := base + "/" + gitlettest.RepoName

	work := person.Init()
	tip := person.Commit(work, map[string]string{
		"README": "the top level\n", "src/a.txt": "a\n", "src/lib/b.txt": "b\n", "docs/guide.md": "guide\n",
		"[odd]/o.txt": "odd\n", "!bang/n.txt": "bang\n",
	}, "the tree a box is given")
	outside := strings.TrimSpace(person.Git().Run(t, work, "rev-parse", tip+":docs/guide.md"))
	if res := person.Push(ctx, work, url, "main"); res.Err != nil {
		t.Fatalf("git push main: %s", res)
	}
	// THE OTHER SEATS' WORK, on the host before the run launches: a commit off
	// main on each of three seats' branches, Alice's own for another item among
	// them.
	ownRef := "refs/heads/seats/" + gitlettest.FixtureAlice + "/ENG-8"
	seatWork := map[string]string{}
	for _, ref := range []string{"refs/heads/seats/" + gitlettest.FixtureBob + "/ENG-1",
		"refs/heads/seats/" + gitlettest.FixtureLead + "/ENG-2", ownRef} {
		seatWork[ref] = pushOffMain(t, person, work, url, tip, ref)
	}
	// A REPOSITORY OF SEATS' BRANCHES ALONE, its default branch never
	// pushed: two seats' work, Alice's among it.
	const lone = "lone"
	f.Declare(t, gitlet.Declared{Name: lone, Settings: gitlettest.Settings()})
	loneOwn := "refs/heads/seats/" + gitlettest.FixtureAlice + "/ENG-9"
	loneOther := "refs/heads/seats/" + gitlettest.FixtureBob + "/ENG-4"
	loneWork := map[string]string{}
	for _, ref := range []string{loneOwn, loneOther} {
		loneWork[ref] = pushOffMain(t, person, work, base+"/"+lone, tip, ref)
	}
	loneRows, err := f.Rows(ctx, lone, gitlettest.Linearizable)
	if err != nil {
		t.Fatalf("%s's rows: %v", lone, err)
	}
	rows, err := f.Rows(ctx, gitlettest.RepoName, gitlettest.Linearizable)
	if err != nil {
		t.Fatalf("the repository's rows: %v", err)
	}
	const branch = "seats/" + gitlettest.FixtureAlice + "/ENG-7"
	run := f.Runs().Launch(gitlettest.FixtureAlice, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID,
		Name: gitlettest.RepoName, Access: gitlet.AccessWrite, Branch: "refs/heads/" + branch},
		host.BoxGrant{RepoID: loneRows.Repo.ID, Name: lone, Access: gitlet.AccessWrite, Branch: loneOwn})
	box := boxEnv(t, person.Git().Environ(), git, sandbox.BoxGit{Base: f.Nodes()[0].URL() + "/box/git",
		User: host.BoxUser, Token: f.Runs().Token(t, run), Name: "Alice", Email: "alice@seats.invalid"})

	runStep(t, t.TempDir(), box, sandbox.GitVersionStep())

	repo := sandbox.RunRepo{
		Repo: gitlettest.RepoName, RepoID: rows.Repo.ID, Base: tip, Branch: branch,
		Access: sandbox.RepoWrite, Paths: []string{"src", "[odd]", "!bang"},
	}
	coding, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	runStep(t, dir, box, coding)
	clone := filepath.Join(dir, gitlettest.RepoName)
	client := person.Git()
	if got := strings.TrimSpace(client.Run(t, clone, "symbolic-ref", "HEAD")); got != "refs/heads/"+branch {
		t.Errorf("the coding clone is on %s, want refs/heads/%s", got, branch)
	}
	if got := strings.TrimSpace(client.Run(t, clone, "rev-parse", "HEAD")); got != tip {
		t.Errorf("the coding clone is at %s, want the base %s", got, tip)
	}
	sameFiles(t, "the coding clone", clone, "README", "src/a.txt", "src/lib/b.txt", "[odd]/o.txt", "!bang/n.txt")
	held := heldObjects(t, client, clone)
	if held[outside] {
		t.Errorf("the coding clone holds %s, docs/guide.md's blob, outside its cone", outside)
	}
	// THE DEFAULT BRANCH, AND NONE OF THE OTHER SEATS' WORK: not a commit of
	// theirs is in the box, origin/main is the one branch it tracks, and its
	// refspec names main alone, so the agent's own `git fetch` or `git pull`
	// brings no other seat's next push either.
	for ref, commit := range seatWork {
		if held[commit] {
			t.Errorf("the coding clone holds %s, %s's commit: another branch's work, sent to every box", commit, ref)
		}
	}
	if got := trackedBranches(t, client, clone); !slices.Equal(got, []string{"refs/remotes/origin/main"}) {
		t.Errorf("the coding clone tracks %q, want origin/main alone", got)
	}
	if got := strings.Fields(client.Run(t, clone, "config", "--get-all", "remote.origin.fetch")); !slices.Equal(got,
		[]string{"+refs/heads/main:refs/remotes/origin/main"}) {
		t.Errorf("the coding clone's fetch refspec is %q, want main's alone", got)
	}
	if hooks := writtenHooks(t, clone); len(hooks) > 0 {
		t.Errorf("the coding clone wrote the hooks %q: a commit's attribution is its run's, never the checkout's",
			hooks)
	}

	// NO MONITOR: the coding clone leaves git's file-system monitor unset, and
	// no step turns it on (sandbox.CodingSteps), since its daemon would spend
	// the engine user's inotify budget beside every other seat's.
	if out, _, _ := client.Try(t, clone, nil, "config", "--get", "core.fsmonitor"); strings.TrimSpace(out) != "" {
		t.Errorf("the coding clone set core.fsmonitor to %q, want it unset", out)
	}

	// A COMMIT MADE IN THE BOX pushes AS THE RUN (the engine's helper answering
	// /box/git/ with the run's token), lands a push row naming the run, and
	// carries the seat's identity and no trailer of the recipe's: the push row,
	// stamped from the run's record, is its provenance, and attribution is the
	// executor runner's (internal/sandbox/executor's attribution.go), never a
	// checkout's hook.
	if err := os.WriteFile(filepath.Join(clone, "src", "a.txt"), []byte("a, changed in the box\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, box, "commit", "-q", "-a", "-m", "a box's change")
	pushed := strings.TrimSpace(runGit(t, clone, box, "rev-parse", "HEAD"))
	runGit(t, clone, box, "push", "-q", "origin", branch)
	if got := person.LsRemote(url, 2)["refs/heads/"+branch]; got != pushed {
		t.Fatalf("the host's %s is %s, want the box's commit %s", branch, got, pushed)
	}
	if !slices.ContainsFunc(pushRowsOf(t, f), func(row gitlet.PushRow) bool {
		return row.Run.RunID == run.LaunchID && slices.ContainsFunc(row.Commands, func(c gitlet.Command) bool {
			return c.Ref == "refs/heads/"+branch && c.New == pushed
		})
	}) {
		t.Errorf("no push row names run %s's push of %s", run.LaunchID, branch)
	}
	client.Run(t, work, "fetch", "-q", url, "refs/heads/"+branch)
	if got := strings.TrimSpace(client.Run(t, work, "log", "-1", "--format=%an <%ae> / %cn <%ce>",
		"FETCH_HEAD")); got != "Alice <alice@seats.invalid> / Alice <alice@seats.invalid>" {
		t.Errorf("the box's commit is %q, want the seat's identity as its author and committer", got)
	}
	if got := strings.TrimSpace(client.Run(t, work, "log", "-1", "--format=%(trailers:only,unfold)",
		"FETCH_HEAD")); got != "" {
		t.Errorf("the pushed commit carries the trailers %q, want none the recipe added", got)
	}

	// THE AGENT'S OWN FETCHES: a plain `git fetch` in the box brings nothing
	// of another seat's push since the clone, and another branch is fetched
	// by its name, as the brief tells the agent — into FETCH_HEAD, through
	// the box's own credential, never tracked.
	late := pushOffMain(t, person, work, url, tip, "refs/heads/seats/"+gitlettest.FixtureLead+"/ENG-3")
	runGit(t, clone, box, "fetch", "-q")
	if heldObjects(t, client, clone)[late] {
		t.Errorf("the box's own `git fetch` brought %s, another seat's push since its clone", late)
	}
	other := "seats/" + gitlettest.FixtureBob + "/ENG-1"
	runGit(t, clone, box, "fetch", "-q", "origin", other)
	if got := strings.TrimSpace(client.Run(t, clone, "rev-parse", "FETCH_HEAD")); got != seatWork["refs/heads/"+other] {
		t.Errorf("`git fetch origin %s` in the box left FETCH_HEAD at %s, want the branch's %s", other, got,
			seatWork["refs/heads/"+other])
	}
	if got := trackedBranches(t, client, clone); !slices.Equal(got, []string{"refs/remotes/origin/main"}) {
		t.Errorf("after fetching %s the coding clone tracks %q, want origin/main alone", other, got)
	}

	// UNDER A HARNESS'S BLANK ENTRIES, the box's own credential still answers:
	// Gemini CLI's shell appends GIT_CONFIG_COUNT entries emptying
	// credential.helper (resetting the helper list) and the rest, with no
	// prompt, and the engine's helper, in GIT_CONFIG_PARAMETERS, is read after
	// them, so a fetch and a push from that shell go through as the run.
	blanked := append(appendConfig(t, box, "credential.helper", "", "core.fsmonitor", "", "core.hooksPath", "",
		"core.sshCommand", "", "core.editor", "", "sequence.editor", "", "core.pager", "cat"),
		"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	runGit(t, clone, blanked, "fetch", "-q", "origin", other)
	runGit(t, clone, blanked, "commit", "-q", "--allow-empty", "-m", "from a harness's shell")
	runGit(t, clone, blanked, "push", "-q", "origin", branch)

	// A RUN CONTINUING THE SEAT'S OWN BRANCH starts at its tip, off the
	// default branch: fetched by its id, and the other seats' work still not.
	own := repo
	own.Base, own.Branch = seatWork[ownRef], strings.TrimPrefix(ownRef, "refs/heads/")
	continued, err := sandbox.CloneRecipe(own, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	runStep(t, dir, box, continued)
	if got := strings.TrimSpace(client.Run(t, filepath.Join(dir, gitlettest.RepoName), "rev-parse", "HEAD")); got !=
		own.Base {
		t.Errorf("a clone continuing %s is at %s, want its tip %s", ownRef, got, own.Base)
	}
	held = heldObjects(t, client, filepath.Join(dir, gitlettest.RepoName))
	for ref, commit := range seatWork {
		if ref != ownRef && held[commit] {
			t.Errorf("a clone continuing %s holds %s, %s's commit", ownRef, commit, ref)
		}
	}

	// A REPOSITORY OF SEATS' BRANCHES ALONE: its HEAD names a branch nobody
	// has pushed, so the clone wants no branch at all, and a run continuing
	// the seat's own branch there starts at its tip, fetched by its id,
	// holding nothing of the other seat's work.
	unborn := sandbox.RunRepo{Repo: lone, RepoID: loneRows.Repo.ID, Base: loneWork[loneOwn],
		Branch: strings.TrimPrefix(loneOwn, "refs/heads/"), Access: sandbox.RepoWrite}
	alone, err := sandbox.CloneRecipe(unborn, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	runStep(t, dir, box, alone)
	if got := strings.TrimSpace(client.Run(t, filepath.Join(dir, lone), "rev-parse", "HEAD")); got != unborn.Base {
		t.Errorf("a clone of %s continuing %s is at %s, want its tip %s", lone, loneOwn, got, unborn.Base)
	}
	if heldObjects(t, client, filepath.Join(dir, lone))[loneWork[loneOther]] {
		t.Errorf("a clone of %s continuing %s holds %s, %s's commit", lone, loneOwn, loneWork[loneOther], loneOther)
	}

	// A BASE NO REF REACHES is fetched by its id.
	gone := person.Commit(work, map[string]string{"src/gone.txt": "gone\n"}, "on a branch since deleted")
	if res := person.Push(ctx, work, url, "HEAD:refs/heads/gone"); res.Err != nil {
		t.Fatalf("git push gone: %s", res)
	}
	if res := person.Push(ctx, work, url, ":refs/heads/gone"); res.Err != nil {
		t.Fatalf("git push :gone: %s", res)
	}
	orphan := repo
	orphan.Base, orphan.Branch = gone, "seats/"+gitlettest.FixtureAlice+"/run-0123456789ab"
	again, err := sandbox.CloneRecipe(orphan, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	runStep(t, dir, box, again)
	if got := strings.TrimSpace(client.Run(t, filepath.Join(dir, gitlettest.RepoName), "rev-parse", "HEAD")); got != gone {
		t.Errorf("a clone at a deleted branch's commit is at %s, want %s", got, gone)
	}

	// THE CHECK'S CHECKOUT: one commit, detached, the cone alone.
	check := repo
	check.Branch, check.Access, check.Paths = "", sandbox.RepoRead, []string{"src"}
	single, err := sandbox.CloneRecipe(check, sandbox.CloneSingleSHA)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	runStep(t, dir, box, single)
	checkout := filepath.Join(dir, gitlettest.RepoName)
	if got := strings.TrimSpace(client.Run(t, checkout, "rev-parse", "HEAD")); got != tip {
		t.Errorf("the check's checkout is at %s, want %s", got, tip)
	}
	if _, _, err := client.Try(t, checkout, nil, "symbolic-ref", "-q", "HEAD"); err == nil {
		t.Error("the check's checkout is on a branch, want HEAD detached")
	}
	if got := strings.TrimSpace(client.Run(t, checkout, "rev-list", "--count", "HEAD")); got != "1" {
		t.Errorf("the check's checkout holds %s commits of history, want 1", got)
	}
	sameFiles(t, "the check's checkout", checkout, "README", "src/a.txt", "src/lib/b.txt")
	if hooks := writtenHooks(t, checkout); len(hooks) > 0 {
		t.Errorf("the check's checkout wrote the hooks %q: a check box commits nothing", hooks)
	}
}

// boxEnv is a box's environment over the client environment env, as the
// engine's launch makes it: git's directory first on PATH, the engine's box
// git environment (g) over env, its own git configuration kept, and g's token
// written to a file of its own that the environment names by its path; no
// variable holds the token.
func boxEnv(t testing.TB, env []string, git string, g sandbox.BoxGit) []string {
	t.Helper()
	run := map[string]string{}
	for _, kv := range env {
		name, value, _ := strings.Cut(kv, "=")
		run[name] = value
	}
	engine, err := g.Env(run)
	if err != nil {
		t.Fatalf("the box's git environment: %v", err)
	}
	maps.Copy(run, engine)
	access := filepath.Join(t.TempDir(), "git-access")
	if err := os.WriteFile(access, []byte(g.Token), 0o600); err != nil {
		t.Fatal(err)
	}
	run[sandbox.GitAccessFileVar] = access
	for name, value := range run {
		if strings.Contains(value, g.Token) {
			t.Fatalf("the box's variable %s holds its token, which only its file holds", name)
		}
	}
	run["PATH"] = filepath.Dir(git) + string(os.PathListSeparator) + os.Getenv("PATH")
	out := make([]string, 0, len(run))
	for _, name := range slices.Sorted(maps.Keys(run)) {
		out = append(out, name+"="+run[name])
	}
	return out
}

// runGit runs git in dir as a box runs it — through its shell, under the
// box's environment env, whose PATH names the box's git first — failing t
// where it exits non-zero, and answers its standard output. Through the
// shell, never an exec of the git itself: a box's git is the box's, not one
// this module starts (gitproc's TestOnlyGitprocExecsGit).
func runGit(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	words := make([]string, len(args))
	for i, arg := range args {
		words[i] = shellword.Quote(arg)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "git "+strings.Join(words, " "))
	cmd.Dir, cmd.Env = dir, env
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s in the box: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out.String()
}

// pushRowsOf is every push row of the fleet's repository, read linearizable.
func pushRowsOf(t *testing.T, f *gitlettest.SoloFleet) []gitlet.PushRow {
	t.Helper()
	var rows []gitlet.PushRow
	q := gitlet.ReadQuery{Asker: gitlettest.Reader, Kind: gitlet.ReadPushes, Repo: gitlettest.RepoName, Limit: 200,
		Fresh: gitlettest.Linearizable}
	for {
		a, err := f.Nodes()[0].Read(t.Context(), q)
		if err != nil {
			t.Fatalf("read the pushes: %v", err)
		}
		rows = append(rows, a.Pushes...)
		if a.Next == "" {
			return rows
		}
		q.Cursor = a.Next
	}
}

// runStep runs step's commands as a box does — each through a shell in the
// box's working directory dir, under env — failing t on the first that exits
// non-zero.
func runStep(t *testing.T, dir string, env []string, step sandbox.SetupStep) {
	t.Helper()
	for i, command := range step.Commands {
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
		cmd.Dir, cmd.Env = dir, env
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("step %q command %d: %v\n%s", step.Name, i+1, err, out.String())
		}
	}
}

// sameFiles fails t unless the work tree at dir holds exactly want.
func sameFiles(t *testing.T, what, dir string, want ...string) {
	t.Helper()
	var got []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			rel, _ := filepath.Rel(dir, path)
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("%s checked out %q, want %q", what, got, want)
	}
}

// pushOffMain pushes ref from p's work tree work as one commit on top of
// base, adding a file in a directory of its own (a new tree as well as a new
// commit), leaves work on main again, and answers the commit. Every git it
// runs is the person's, under the test's context.
func pushOffMain(t *testing.T, p *gitlettest.Person, work, url, base, ref string) string {
	t.Helper()
	p.Git().Run(t, work, "checkout", "-q", "-B", "seat-work", base)
	commit := p.Commit(work, map[string]string{"work/" + strings.ReplaceAll(ref, "/", "-") + ".txt": ref + "\n"},
		"work on "+ref)
	if res := p.Push(t.Context(), work, url, "HEAD:"+ref); res.Err != nil {
		t.Fatalf("git push %s: %s", ref, res)
	}
	p.Git().Run(t, work, "checkout", "-q", "main")
	return commit
}

// writtenHooks is every hook the checkout at dir holds that git's own template
// did not write: the entries of its .git/hooks with no ".sample" suffix, which
// is how git names each one it installs as an example and never runs.
func writtenHooks(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, ".git", "hooks"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s's hooks: %v", dir, err)
	}
	var written []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sample") {
			written = append(written, e.Name())
		}
	}
	return written
}

// trackedBranches is every remote-tracking branch of the repository at dir,
// the symbolic origin/HEAD left out.
func trackedBranches(t *testing.T, client *gittest.Client, dir string) []string {
	t.Helper()
	var out []string
	for _, ref := range strings.Fields(client.Run(t, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/")) {
		if ref != "refs/remotes/origin/HEAD" {
			out = append(out, ref)
		}
	}
	return out
}

// heldObjects is every object the repository at dir holds itself — a
// promisor's missing objects are not listed, and nothing is fetched to list
// them.
func heldObjects(t *testing.T, client *gittest.Client, dir string) map[string]bool {
	t.Helper()
	out := client.Run(t, dir, "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
	held := map[string]bool{}
	for _, line := range strings.Fields(out) {
		held[line] = true
	}
	return held
}
