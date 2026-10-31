package host_test

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE BOX RECIPES CLONE FROM THE CODE HOST on the pinned git, as the floors'
// arm holds them to on the client floor's own build (TestTheGitFloorsPassTheSuite).
func TestTheBoxRecipesCloneFromTheCodeHost(t *testing.T) {
	t.Parallel()
	boxRecipes(t, codetest.SoloOptions{}, gittest.Require(t))
}

// boxRecipes is the box half of the code host's suite: on a git node serving a
// repository, the steps a coding box is given its repositories by — the
// version check, the coding clone and the check's single-commit checkout —
// run as a box runs them, `sh -c` in the box's working directory, under git
// (the box's git: the pin, or the client floor's 2.40.4) with EXACTLY the
// environment the engine gives a box (sandbox.BoxGit, ADR-0063): its run's
// credential on /box/git/, answered by the engine's credential helper, the
// code host's base as CREWLET_GIT_URL, the seat's commit identity, and the
// commit hook's values — over a git environment of the box's own whose
// global credential helper answers as a PERSON, which the engine's reset must
// keep from ever answering the code host.
//
//   - The version step passes.
//   - The coding clone leaves HEAD on the run's branch at its base, the cone's
//     files checked out — directories git's cone mode would read as patterns
//     among them — and not one blob outside the cone in the object store,
//     the commit-trailer hook in place; a commit made there pushes as the
//     run, lands a push row naming the run, and the commit the host holds
//     carries the seat's identity and the run's three trailers.
//   - The coding clone holds the default branch and nothing of the other
//     seats' branches already on the host — not one of their commits, no
//     remote-tracking ref but origin/main — and its fetch refspec names main
//     alone, so the agent's own later `git fetch` brings nothing of another
//     seat's next push either, and another branch it fetches by name, as its
//     brief says, lands in FETCH_HEAD untracked.
//   - A base off the default branch is fetched by id: the tip of the seat's
//     own branch a run continues, held with the other seats' work still not;
//     the same in a repository of seats' branches alone, whose HEAD names a
//     branch nobody pushed, where the clone wants no branch at all; and a
//     base no ref reaches any more (a deleted branch's commit).
//   - The single-commit checkout leaves HEAD detached at the commit, one
//     commit of history, the cone's files alone, and no commit hook.
//
// Mutations: drop the hook command from the coding recipe — the pushed commit
// carries no trailer, red; check the coding clone out at what its clone's
// HEAD names rather than its base — the base cases off the default branch,
// red; drop --skip-checks — the cone naming [odd] and !bang is refused, red;
// drop --single-branch — the clone holds every seat's work, tracks every
// branch and fetches another seat's next push, red; drop the engine's reset
// of the helper list — the person's helper answers the box route and every
// request is refused, red.
func boxRecipes(t *testing.T, opts codetest.SoloOptions, git string) {
	t.Helper()
	ctx := t.Context()
	f := codetest.SoloNode(t, opts)
	f.Declare(t, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	person := codetest.NewPerson(t, f, "ceo")
	base := f.Nodes()[0].URL() + "/git"
	url := base + "/" + codetest.RepoName

	work := person.Init()
	tip := person.Commit(work, map[string]string{
		"README": "the top level\n", "src/a.txt": "a\n", "src/lib/b.txt": "b\n", "docs/guide.md": "guide\n",
		"[odd]/o.txt": "odd\n", "!bang/n.txt": "bang\n",
	}, "the tree a box is given")
	outside := strings.TrimSpace(person.Git().Run(t, work, "rev-parse", tip+":docs/guide.md"))
	if res := person.Push(ctx, work, url, "main"); res.Err != nil {
		t.Fatalf("git push main: %s", res)
	}
	// THE OTHER SEATS' WORK, on the host before the run launches, as it is
	// on any repository its seats work in: a branch a commit off main under
	// each of three seats' namespaces, Alice's own branch for another item
	// among them.
	ownRef := "refs/heads/seats/" + codetest.FixtureAlice + "/ENG-8"
	seatWork := map[string]string{}
	for _, ref := range []string{"refs/heads/seats/" + codetest.FixtureBob + "/ENG-1",
		"refs/heads/seats/" + codetest.FixtureLead + "/ENG-2", ownRef} {
		seatWork[ref] = pushOffMain(ctx, t, person, work, url, tip, ref)
	}
	// A REPOSITORY OF SEATS' BRANCHES ALONE, its default branch never
	// pushed: two seats' work, Alice's among it.
	const lone = "lone"
	f.Declare(t, code.Declared{Name: lone, Settings: codetest.Settings()})
	loneOwn := "refs/heads/seats/" + codetest.FixtureAlice + "/ENG-9"
	loneOther := "refs/heads/seats/" + codetest.FixtureBob + "/ENG-4"
	loneWork := map[string]string{}
	for _, ref := range []string{loneOwn, loneOther} {
		loneWork[ref] = pushOffMain(ctx, t, person, work, base+"/"+lone, tip, ref)
	}
	loneRows, err := f.Rows(ctx, lone, codetest.Linearizable)
	if err != nil {
		t.Fatalf("%s's rows: %v", lone, err)
	}
	rows, err := f.Rows(ctx, codetest.RepoName, codetest.Linearizable)
	if err != nil {
		t.Fatalf("the repository's rows: %v", err)
	}
	const branch = "seats/" + codetest.FixtureAlice + "/ENG-7"
	run := f.Runs().Launch(codetest.FixtureAlice, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID,
		Name: codetest.RepoName, Access: code.AccessWrite, Branch: "refs/heads/" + branch},
		host.BoxGrant{RepoID: loneRows.Repo.ID, Name: lone, Access: code.AccessWrite, Branch: loneOwn})
	trailers, err := sandbox.TrailerEnv(codetest.FixtureAlice, run.LaunchID,
		&types.WorkItem{Backend: types.WorkNative, ID: "task-7", Key: "ENG-7"})
	if err != nil {
		t.Fatal(err)
	}
	box := boxEnv(t, person.Git().Environ(), git, sandbox.BoxGit{Base: f.Nodes()[0].URL() + "/box/git",
		User: host.BoxUser, Token: f.Runs().Token(run), Name: "Alice", Email: "alice@seats.invalid"}, trailers)

	runStep(t, t.TempDir(), box, sandbox.GitVersionStep())

	repo := sandbox.RunRepo{
		Repo: codetest.RepoName, RepoID: rows.Repo.ID, Base: tip, Branch: branch,
		Access: sandbox.RepoWrite, Paths: []string{"src", "[odd]", "!bang"},
	}
	coding, err := sandbox.CloneRecipe(repo, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	runStep(t, dir, box, coding)
	clone := filepath.Join(dir, codetest.RepoName)
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
	hook := filepath.Join(clone, ".git", "hooks", "prepare-commit-msg")
	if body, err := os.ReadFile(hook); err != nil || string(body) != sandbox.TrailerHook {
		t.Errorf("the coding clone's hook is %q (%v), want TrailerHook", body, err)
	}
	if info, err := os.Stat(hook); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("the coding clone's hook is not mode 0755: %v %v", info, err)
	}

	// GIT'S MONITOR is no clone's: the coding clone leaves it unset, and the
	// monitor's own step — which the engine runs only in a cell whose boxes
	// own their kernel (sandbox.CodingSteps) — turns it on in the checkout's
	// own configuration exactly where the box can keep it: a git of 2.55.0 or
	// newer, the node pin's here and never the client floor's 2.40.4, with
	// every checkout in the box taking at most half of the kernel's watches
	// and instances. Its daemon, which the box's first commit below starts,
	// is stopped before the case's directories go.
	if out, _, _ := client.Try(t, clone, nil, "config", "--get", "core.fsmonitor"); strings.TrimSpace(out) != "" {
		t.Errorf("the coding clone set core.fsmonitor to %q: the monitor is the cell's to decide", out)
	}
	runStep(t, dir, box, sandbox.MonitorStep([]sandbox.RunRepo{repo}))
	monitored := monitorable(t, box, dir)
	if out, _, _ := client.Try(t, clone, nil, "config", "--get", "core.fsmonitor"); (strings.TrimSpace(out) ==
		"true") != monitored {
		t.Errorf("the checkout's core.fsmonitor is %q, want it set exactly where the box can keep a monitor (%v)",
			strings.TrimSpace(out), monitored)
	}
	if monitored {
		t.Cleanup(func() {
			// Not under t's context, which is cancelled before a cleanup
			// runs: a daemon left running outlives the case.
			stop := exec.Command("/bin/sh", "-c", "git fsmonitor--daemon stop")
			stop.Dir, stop.Env = clone, box
			_ = stop.Run()
		})
	}

	// A COMMIT MADE IN THE BOX, under its run, pushes AS THE RUN — the
	// engine's helper answering /box/git/ with the run's token — lands a push
	// row naming the run, and carries the seat's identity and the run's
	// trailers on the host.
	if err := os.WriteFile(filepath.Join(clone, "src", "a.txt"), []byte("a, changed in the box\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, box, "commit", "-q", "-a", "-m", "a box's change")
	pushed := strings.TrimSpace(runGit(t, clone, box, "rev-parse", "HEAD"))
	runGit(t, clone, box, "push", "-q", "origin", branch)
	if got := person.LsRemote(url, 2)["refs/heads/"+branch]; got != pushed {
		t.Fatalf("the host's %s is %s, want the box's commit %s", branch, got, pushed)
	}
	if !slices.ContainsFunc(pushRowsOf(t, f), func(row code.PushRow) bool {
		return row.Run.RunID == run.LaunchID && slices.ContainsFunc(row.Commands, func(c code.Command) bool {
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
	got := strings.Split(strings.TrimSpace(client.Run(t, work, "log", "-1",
		"--format=%(trailers:only,unfold)", "FETCH_HEAD")), "\n")
	want := []string{"Crewlet-Seat: " + codetest.FixtureAlice, "Crewlet-Run: " + run.LaunchID,
		"Crewlet-Work-Item: native ENG-7"}
	if !slices.Equal(got, want) {
		t.Errorf("the pushed commit carries %q, want %q", got, want)
	}

	// THE AGENT'S OWN FETCHES: a plain `git fetch` in the box brings nothing
	// of another seat's push since the clone, and another branch is fetched
	// by its name, as the brief tells the agent — into FETCH_HEAD, through
	// the box's own credential, never tracked.
	late := pushOffMain(ctx, t, person, work, url, tip, "refs/heads/seats/"+codetest.FixtureLead+"/ENG-3")
	runGit(t, clone, box, "fetch", "-q")
	if heldObjects(t, client, clone)[late] {
		t.Errorf("the box's own `git fetch` brought %s, another seat's push since its clone", late)
	}
	other := "seats/" + codetest.FixtureBob + "/ENG-1"
	runGit(t, clone, box, "fetch", "-q", "origin", other)
	if got := strings.TrimSpace(client.Run(t, clone, "rev-parse", "FETCH_HEAD")); got != seatWork["refs/heads/"+other] {
		t.Errorf("`git fetch origin %s` in the box left FETCH_HEAD at %s, want the branch's %s", other, got,
			seatWork["refs/heads/"+other])
	}
	if got := trackedBranches(t, client, clone); !slices.Equal(got, []string{"refs/remotes/origin/main"}) {
		t.Errorf("after fetching %s the coding clone tracks %q, want origin/main alone", other, got)
	}

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
	if got := strings.TrimSpace(client.Run(t, filepath.Join(dir, codetest.RepoName), "rev-parse", "HEAD")); got !=
		own.Base {
		t.Errorf("a clone continuing %s is at %s, want its tip %s", ownRef, got, own.Base)
	}
	held = heldObjects(t, client, filepath.Join(dir, codetest.RepoName))
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
	orphan.Base, orphan.Branch = gone, "seats/"+codetest.FixtureAlice+"/run-0123456789ab"
	again, err := sandbox.CloneRecipe(orphan, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	runStep(t, dir, box, again)
	if got := strings.TrimSpace(client.Run(t, filepath.Join(dir, codetest.RepoName), "rev-parse", "HEAD")); got != gone {
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
	checkout := filepath.Join(dir, codetest.RepoName)
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
	if _, err := os.Lstat(filepath.Join(checkout, ".git", "hooks", "prepare-commit-msg")); err == nil {
		t.Error("the check's checkout has a commit hook: a check box commits nothing")
	}
}

// boxEnv is a box's environment over the client environment env, as the
// engine's launch makes it: git's directory first on PATH, and over env — its
// own git configuration kept — the engine's box git environment (g) and the
// commit hook's values.
func boxEnv(t testing.TB, env []string, git string, g sandbox.BoxGit, hook map[string]string) []string {
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
	maps.Copy(run, hook)
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
		words[i] = sandbox.ShellQuote(arg)
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

// monitorable reports whether a box under env, working in dir, could keep
// git's monitor for its checkouts: its git at gitproc.NodeMinVersion or
// newer, and the checkouts at dir's top together at most half of the
// kernel's watches (their directories) and of its instances (their count),
// each the least of the host's and the namespace's — what sandbox.MonitorStep
// asks before it turns the monitor on.
func monitorable(t *testing.T, env []string, dir string) bool {
	t.Helper()
	v, err := gitproc.ParseVersion(runGit(t, dir, env, "version"))
	if err != nil {
		t.Fatal(err)
	}
	least := func(paths ...string) int {
		n := -1
		for _, p := range paths {
			raw, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			if v, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && (n < 0 || v < n) {
				n = v
			}
		}
		return max(n, 0)
	}
	watches := least("/proc/sys/fs/inotify/max_user_watches", "/proc/sys/user/max_inotify_watches")
	instances := least("/proc/sys/fs/inotify/max_user_instances", "/proc/sys/user/max_inotify_instances")
	checkouts, dirs := 0, 0
	tops, err := filepath.Glob(filepath.Join(dir, "*", ".git"))
	if err != nil {
		t.Fatal(err)
	}
	for _, top := range tops {
		checkouts++
		if err := filepath.WalkDir(filepath.Dir(top), func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				dirs++
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return v.AtLeast(gitproc.NodeMinVersion) && dirs <= watches/2 && checkouts <= instances/2
}

// pushRowsOf is every push row of the fleet's repository, read linearizable.
func pushRowsOf(t *testing.T, f *codetest.SoloFleet) []code.PushRow {
	t.Helper()
	var rows []code.PushRow
	q := code.ReadQuery{Asker: codetest.Reader, Kind: code.ReadPushes, Repo: codetest.RepoName, Limit: 200,
		Fresh: codetest.Linearizable}
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

// pushOffMain pushes ref from p's work tree work as one commit on top of base,
// a file of its own in a directory of its own — a new tree as well as a new
// commit — leaves work on main again, and answers the commit.
func pushOffMain(ctx context.Context, t *testing.T, p *codetest.Person, work, url, base, ref string) string {
	t.Helper()
	p.Git().Run(t, work, "checkout", "-q", "-B", "seat-work", base)
	commit := p.Commit(work, map[string]string{"work/" + strings.ReplaceAll(ref, "/", "-") + ".txt": ref + "\n"},
		"work on "+ref)
	if res := p.Push(ctx, work, url, "HEAD:"+ref); res.Err != nil {
		t.Fatalf("git push %s: %s", ref, res)
	}
	p.Git().Run(t, work, "checkout", "-q", "main")
	return commit
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
