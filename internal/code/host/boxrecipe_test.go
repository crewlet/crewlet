package host_test

import (
	"bytes"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
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
//   - A base no ref reaches any more (a deleted branch's commit) is fetched
//     by id.
//   - The single-commit checkout leaves HEAD detached at the commit, one
//     commit of history, the cone's files alone, and no commit hook.
//
// Mutations: drop the hook command from the coding recipe — the pushed commit
// carries no trailer, red; check the coding clone out at what its clone's
// HEAD names rather than its base — the deleted branch's base case, red; drop
// --skip-checks — the cone naming [odd] and !bang is refused, red; drop the
// engine's reset of the helper list — the person's helper answers the box
// route and every request is refused, red.
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
	rows, err := f.Rows(ctx, codetest.RepoName, codetest.Linearizable)
	if err != nil {
		t.Fatalf("the repository's rows: %v", err)
	}
	const branch = "seats/" + codetest.FixtureAlice + "/ENG-7"
	run := f.Runs().Launch(codetest.FixtureAlice, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID,
		Name: codetest.RepoName, Access: code.AccessWrite, Branch: "refs/heads/" + branch})
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
	if held := heldObjects(t, client, clone); held[outside] {
		t.Errorf("the coding clone holds %s, docs/guide.md's blob, outside its cone", outside)
	}
	hook := filepath.Join(clone, ".git", "hooks", "prepare-commit-msg")
	if body, err := os.ReadFile(hook); err != nil || string(body) != sandbox.TrailerHook {
		t.Errorf("the coding clone's hook is %q (%v), want TrailerHook", body, err)
	}
	if info, err := os.Stat(hook); err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("the coding clone's hook is not mode 0755: %v %v", info, err)
	}

	// A COMMIT MADE IN THE BOX, under its run, pushes AS THE RUN — the
	// engine's helper answering /box/git/ with the run's token — lands a push
	// row naming the run, and carries the seat's identity and the run's
	// trailers on the host.
	if err := os.WriteFile(filepath.Join(clone, "src", "a.txt"), []byte("a, changed in the box\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, clone, box, git, "commit", "-q", "-a", "-m", "a box's change")
	pushed := strings.TrimSpace(runGit(t, clone, box, git, "rev-parse", "HEAD"))
	runGit(t, clone, box, git, "push", "-q", "origin", branch)
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
	check.Paths = []string{"src"}
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
func boxEnv(t *testing.T, env []string, git string, g sandbox.BoxGit, hook map[string]string) []string {
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

// runGit runs git in dir under the box's environment env, failing t where it
// exits non-zero, and answers its standard output.
func runGit(t *testing.T, dir string, env []string, git string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), git, args...)
	cmd.Dir, cmd.Env = dir, env
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s in the box: %v\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return out.String()
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
