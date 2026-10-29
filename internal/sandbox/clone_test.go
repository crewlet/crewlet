package sandbox_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// cloneBase is the commit the cases' repositories are given at.
const cloneBase = "0123456789abcdef0123456789abcdef01234567"

// cloned is a repository a coding run is given, valid by its grammar.
func cloned() sandbox.RunRepo {
	return sandbox.RunRepo{
		Repo: "web", RepoID: "0190d8f0-0000-7000-8000-00000000000a", Base: cloneBase,
		Branch: "seats/swe/ENG-7", Access: sandbox.RepoWrite, Paths: []string{"src", "docs"},
	}
}

// THE BOX'S GIT IS HELD TO THE CLIENT FLOOR: sandbox spells the floor as text,
// because it imports nothing of the code host, and this holds it to gitproc's.
//
// Mutation: spell the floor 2.39.0 — red.
func TestTheSandboxClientFloorIsGitprocs(t *testing.T) {
	t.Parallel()
	if got, want := sandbox.ClientGitFloor, gitproc.ClientMinVersion.String(); got != want {
		t.Fatalf("sandbox.ClientGitFloor is %s and gitproc.ClientMinVersion %s: a box would be held to "+
			"another floor than every other client of the code host", got, want)
	}
}

// A RUN REPOSITORY IS HELD TO ITS GRAMMAR, the one every reader of a run's
// grants takes: a repository name of either form, a canonical id, a base that
// is empty or a whole commit, a branch git and the code host both take, a
// known access, and a cone of clean directories inside the repository —
// relative, no "." or ".." component, no control character, no space at
// either end (git trims one), valid UTF-8, at most MaxConePathBytes each and
// MaxConePaths in all.
//
// Mutation: drop the space rule — " src" is taken, red.
func TestARunRepoIsHeldToItsGrammar(t *testing.T) {
	t.Parallel()
	with := func(edit func(*sandbox.RunRepo)) sandbox.RunRepo {
		r := cloned()
		edit(&r)
		return r
	}
	for _, r := range []sandbox.RunRepo{
		cloned(),
		with(func(r *sandbox.RunRepo) { r.Repo = "swe.notes" }),
		with(func(r *sandbox.RunRepo) { r.Base = "" }),
		with(func(r *sandbox.RunRepo) { r.Access = sandbox.RepoRead }),
		with(func(r *sandbox.RunRepo) { r.Paths = nil }),
		with(func(r *sandbox.RunRepo) {
			r.Paths = []string{"a/b/c", "dir with 'quote'", "[odd]", "!bang", "über", "a b",
				strings.Repeat("x", sandbox.MaxConePathBytes)}
		}),
		with(func(r *sandbox.RunRepo) { r.Paths = make([]string, sandbox.MaxConePaths); fill(r.Paths) }),
	} {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	for name, r := range map[string]sandbox.RunRepo{
		"a name in upper case":       with(func(r *sandbox.RunRepo) { r.Repo = "Web" }),
		"a name ending .git":         with(func(r *sandbox.RunRepo) { r.Repo = "web.git" }),
		"an id in upper case":        with(func(r *sandbox.RunRepo) { r.RepoID = strings.ToUpper(r.RepoID) }),
		"an id with no hyphens":      with(func(r *sandbox.RunRepo) { r.RepoID = strings.ReplaceAll(r.RepoID, "-", "") }),
		"the nil id":                 with(func(r *sandbox.RunRepo) { r.RepoID = "00000000-0000-0000-0000-000000000000" }),
		"an abbreviated base":        with(func(r *sandbox.RunRepo) { r.Base = cloneBase[:12] }),
		"a base in upper case":       with(func(r *sandbox.RunRepo) { r.Base = strings.ToUpper(cloneBase) }),
		"the zero base":              with(func(r *sandbox.RunRepo) { r.Base = strings.Repeat("0", 40) }),
		"a branch git refuses":       with(func(r *sandbox.RunRepo) { r.Branch = "a..b" }),
		"a branch that is an option": with(func(r *sandbox.RunRepo) { r.Branch = "-b" }),
		"no branch":                  with(func(r *sandbox.RunRepo) { r.Branch = "" }),
		"an unknown access":          with(func(r *sandbox.RunRepo) { r.Access = "admin" }),
		"an empty path":              with(func(r *sandbox.RunRepo) { r.Paths = []string{""} }),
		"an absolute path":           with(func(r *sandbox.RunRepo) { r.Paths = []string{"/src"} }),
		"a trailing slash":           with(func(r *sandbox.RunRepo) { r.Paths = []string{"src/"} }),
		"a doubled slash":            with(func(r *sandbox.RunRepo) { r.Paths = []string{"a//b"} }),
		"a dot component":            with(func(r *sandbox.RunRepo) { r.Paths = []string{"a/./b"} }),
		"the root":                   with(func(r *sandbox.RunRepo) { r.Paths = []string{"."} }),
		"a parent":                   with(func(r *sandbox.RunRepo) { r.Paths = []string{".."} }),
		"a path out":                 with(func(r *sandbox.RunRepo) { r.Paths = []string{"../up"} }),
		"a path out halfway":         with(func(r *sandbox.RunRepo) { r.Paths = []string{"a/../../up"} }),
		"a newline":                  with(func(r *sandbox.RunRepo) { r.Paths = []string{"a\nb"} }),
		"a NUL":                      with(func(r *sandbox.RunRepo) { r.Paths = []string{"a\x00b"} }),
		"a tab":                      with(func(r *sandbox.RunRepo) { r.Paths = []string{"a\tb"} }),
		"a DEL":                      with(func(r *sandbox.RunRepo) { r.Paths = []string{"a\x7fb"} }),
		"a leading space":            with(func(r *sandbox.RunRepo) { r.Paths = []string{" src"} }),
		"a trailing space":           with(func(r *sandbox.RunRepo) { r.Paths = []string{"src "} }),
		"invalid UTF-8":              with(func(r *sandbox.RunRepo) { r.Paths = []string{"a\xffb"} }),
		"a path past its bound": with(func(r *sandbox.RunRepo) {
			r.Paths = []string{strings.Repeat("x", sandbox.MaxConePathBytes+1)}
		}),
		"a cone past its bound": with(func(r *sandbox.RunRepo) { r.Paths = make([]string, sandbox.MaxConePaths+1); fill(r.Paths) }),
	} {
		if err := r.Validate(); err == nil {
			t.Errorf("%s was taken: %+v", name, r)
		}
	}
}

// fill names every path of paths a directory of its own.
func fill(paths []string) {
	for i := range paths {
		paths[i] = fmt.Sprintf("services/dir%02d", i)
	}
}

// THE CLONE RECIPES ARE THEIR SHAPES: the command each purpose writes, byte for
// byte, so a change to what a box's git is asked to do is a change this case
// is changed with.
func TestTheCloneRecipesAreTheirShapes(t *testing.T) {
	t.Parallel()
	hook := sandbox.ShellQuote(sandbox.TrailerHook)
	coding := `if [ ! -e 'web'/.git ]; then
  git clone --quiet --filter=blob:none --no-checkout -- "$CREWLET_GIT_URL"/'web.git' 'web' &&
  git -C 'web' sparse-checkout set --cone --skip-checks -- 'src' 'docs' &&
  git -C 'web' fetch --quiet --no-tags --filter=blob:none origin '` + cloneBase + `' &&
  git -C 'web' checkout --quiet -B 'seats/swe/ENG-7' '` + cloneBase + `' &&
  { [ -e 'web'/.git/hooks/prepare-commit-msg ] || [ -L 'web'/.git/hooks/prepare-commit-msg ] ||
    { mkdir -p 'web'/.git/hooks && printf '%s' ` + hook + ` > 'web'/.git/hooks/prepare-commit-msg &&
      chmod 755 'web'/.git/hooks/prepare-commit-msg; }; }
fi`
	empty := `if [ ! -e 'web'/.git ]; then
  git clone --quiet --filter=blob:none --no-checkout -- "$CREWLET_GIT_URL"/'web.git' 'web' &&
  git -C 'web' symbolic-ref HEAD 'refs/heads/seats/swe/ENG-7' &&
  { [ -e 'web'/.git/hooks/prepare-commit-msg ] || [ -L 'web'/.git/hooks/prepare-commit-msg ] ||
    { mkdir -p 'web'/.git/hooks && printf '%s' ` + hook + ` > 'web'/.git/hooks/prepare-commit-msg &&
      chmod 755 'web'/.git/hooks/prepare-commit-msg; }; }
fi`
	single := `if [ ! -e 'web'/.git ]; then
  git init --quiet 'web' && git -C 'web' remote add origin "$CREWLET_GIT_URL"/'web.git' &&
  git -C 'web' fetch --quiet --no-tags --depth=1 --filter=blob:none origin '` + cloneBase + `' &&
  git -C 'web' sparse-checkout set --cone --skip-checks -- 'src' 'docs' &&
  git -C 'web' checkout --quiet --detach '` + cloneBase + `'
fi`
	noCone := cloned()
	noCone.Paths, noCone.Base = nil, ""
	for _, tc := range []struct {
		name    string
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
		step    string
		want    string
	}{
		{"coding", cloned(), sandbox.CloneCoding, "clone web", coding},
		{"coding an empty repository", noCone, sandbox.CloneCoding, "clone web", empty},
		{"single-sha", cloned(), sandbox.CloneSingleSHA, "checkout web", single},
	} {
		step, err := sandbox.CloneRecipe(tc.r, tc.purpose)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if step.Name != tc.step || len(step.Commands) != 1 || len(step.Files) != 0 || len(step.Env) != 0 {
			t.Fatalf("%s: the step is %q with %d commands, %d files and %d variables; want %q with one "+
				"command and nothing else", tc.name, step.Name, len(step.Commands), len(step.Files), len(step.Env), tc.step)
		}
		if step.Commands[0] != tc.want {
			t.Errorf("%s: the command is\n%s\nwant\n%s", tc.name, step.Commands[0], tc.want)
		}
		syntax(t, tc.name, step.Commands[0])
	}
	syntax(t, "the version step", sandbox.GitVersionStep().Commands[0])
}

// syntax fails t unless a POSIX shell parses command.
func syntax(t *testing.T, name, command string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "/bin/sh", "-n", "-c", command).CombinedOutput()
	if err != nil {
		t.Errorf("%s: the shell refuses the command: %v\n%s", name, err, out)
	}
}

// A CLONE STEP'S VALUES ARRIVE AS LITERAL ARGUMENTS: run under a shell against
// a stand-in git that records every call, a branch holding a command
// substitution and a quote, and cone directories holding a quote, glob
// characters and a leading '!', reach git as one argument each, and nothing in
// them runs. The coding recipe writes the commit-trailer hook byte for byte
// with mode 0755 — only where no hook of that name exists, a file or a
// dangling link — and the single-sha recipe writes none. A box that already
// holds the checkout runs nothing at all.
//
// Mutations: interpolate the branch unquoted — the stand-in's arguments and a
// file named x, red; write the hook in the single-sha recipe — red; drop the
// hook's existence guard — the box's own hook is replaced, red; drop the
// checkout's guard — the second run clones again, red.
func TestACloneStepsValuesArriveAsLiteralArguments(t *testing.T) {
	t.Parallel()
	const url = "https://git.example.com/box/git"
	stub := gittest.Fake(t, "git version 2.56.0", `{ for arg in "$@"; do printf '%s\037' "$arg"; done; printf '\n'; } >> "$STUB_CALLS"
case "$1" in
clone|init)
  for dir in "$@"; do :; done
  mkdir -p "$dir/.git/hooks"
  case "$1:${STUB_HOOK-}" in
  clone:file) printf 'the box template' > "$dir/.git/hooks/prepare-commit-msg" ;;
  clone:link) ln -s nowhere "$dir/.git/hooks/prepare-commit-msg" ;;
  esac ;;
esac
exit 0`)
	r := cloned()
	r.Branch = `seats/swe/a$(touch${IFS}x)'b`
	r.Paths = []string{"src", "dir with 'quote'", "[odd]", "!bang"}
	cone := []string{"src", "dir with 'quote'", "[odd]", "!bang"}

	run := func(t *testing.T, box string, step sandbox.SetupStep, hook string) [][]string {
		t.Helper()
		calls := filepath.Join(t.TempDir(), "calls")
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", step.Commands[0])
		cmd.Dir = box
		cmd.Env = []string{"PATH=" + filepath.Dir(stub) + string(os.PathListSeparator) + os.Getenv("PATH"),
			"CREWLET_GIT_URL=" + url, "STUB_CALLS=" + calls, "STUB_HOOK=" + hook, "HOME=" + t.TempDir()}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", step.Name, err, out)
		}
		body, err := os.ReadFile(calls)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var out [][]string
		for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
			out = append(out, strings.Split(strings.TrimSuffix(line, "\x1f"), "\x1f"))
		}
		return out
	}

	t.Run("coding", func(t *testing.T) {
		t.Parallel()
		step, err := sandbox.CloneRecipe(r, sandbox.CloneCoding)
		if err != nil {
			t.Fatal(err)
		}
		box := t.TempDir()
		calls := run(t, box, step, "")
		want := [][]string{
			{"clone", "--quiet", "--filter=blob:none", "--no-checkout", "--", url + "/web.git", "web"},
			append([]string{"-C", "web", "sparse-checkout", "set", "--cone", "--skip-checks", "--"}, cone...),
			{"-C", "web", "fetch", "--quiet", "--no-tags", "--filter=blob:none", "origin", cloneBase},
			{"-C", "web", "checkout", "--quiet", "-B", r.Branch, cloneBase},
		}
		sameCalls(t, calls, want)
		if _, err := os.Stat(filepath.Join(box, "x")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a value of the step ran as a command: x exists (%v)", err)
		}
		hook := filepath.Join(box, "web", ".git", "hooks", "prepare-commit-msg")
		body, err := os.ReadFile(hook)
		if err != nil || string(body) != sandbox.TrailerHook {
			t.Errorf("the coding clone's hook is %q (%v), want TrailerHook byte for byte", body, err)
		}
		if info, err := os.Stat(hook); err != nil || info.Mode().Perm() != 0o755 {
			t.Errorf("the coding clone's hook is not mode 0755: %v %v", info, err)
		}
		if again := run(t, box, step, ""); len(again) != 0 {
			t.Errorf("a box already holding the checkout ran %q", again)
		}
	})
	t.Run("an empty repository", func(t *testing.T) {
		t.Parallel()
		empty := r
		empty.Base = ""
		step, err := sandbox.CloneRecipe(empty, sandbox.CloneCoding)
		if err != nil {
			t.Fatal(err)
		}
		box := t.TempDir()
		sameCalls(t, run(t, box, step, ""), [][]string{
			{"clone", "--quiet", "--filter=blob:none", "--no-checkout", "--", url + "/web.git", "web"},
			append([]string{"-C", "web", "sparse-checkout", "set", "--cone", "--skip-checks", "--"}, cone...),
			{"-C", "web", "symbolic-ref", "HEAD", "refs/heads/" + r.Branch},
		})
		if body, err := os.ReadFile(filepath.Join(box, "web", ".git", "hooks", "prepare-commit-msg")); err != nil ||
			string(body) != sandbox.TrailerHook {
			t.Errorf("the empty repository's hook is %q (%v)", body, err)
		}
	})
	t.Run("a hook the box's git put there", func(t *testing.T) {
		t.Parallel()
		step, err := sandbox.CloneRecipe(r, sandbox.CloneCoding)
		if err != nil {
			t.Fatal(err)
		}
		box := t.TempDir()
		run(t, box, step, "file")
		if body, _ := os.ReadFile(filepath.Join(box, "web", ".git", "hooks", "prepare-commit-msg")); string(body) !=
			"the box template" {
			t.Errorf("the box's own hook became %q, want it left as it was", body)
		}
		box = t.TempDir()
		run(t, box, step, "link")
		if target, err := os.Readlink(filepath.Join(box, "web", ".git", "hooks", "prepare-commit-msg")); err != nil ||
			target != "nowhere" {
			t.Errorf("the box's own hook link became %q (%v), want it left as it was", target, err)
		}
	})
	t.Run("single-sha", func(t *testing.T) {
		t.Parallel()
		step, err := sandbox.CloneRecipe(r, sandbox.CloneSingleSHA)
		if err != nil {
			t.Fatal(err)
		}
		box := t.TempDir()
		sameCalls(t, run(t, box, step, ""), [][]string{
			{"init", "--quiet", "web"},
			{"-C", "web", "remote", "add", "origin", url + "/web.git"},
			{"-C", "web", "fetch", "--quiet", "--no-tags", "--depth=1", "--filter=blob:none", "origin", cloneBase},
			append([]string{"-C", "web", "sparse-checkout", "set", "--cone", "--skip-checks", "--"}, cone...),
			{"-C", "web", "checkout", "--quiet", "--detach", cloneBase},
		})
		if _, err := os.Lstat(filepath.Join(box, "web", ".git", "hooks", "prepare-commit-msg")); !errors.Is(err,
			os.ErrNotExist) {
			t.Errorf("a check's checkout has a commit hook (%v): a check box commits nothing", err)
		}
	})
}

// sameCalls fails t unless the stand-in saw exactly want, in order.
func sameCalls(t *testing.T, got, want [][]string) {
	t.Helper()
	if !slices.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("git was called\n%q\nwant\n%q", got, want)
	}
}

// A STEP'S BRIEF TELLS THE AGENT WHAT THE STEP MADE TRUE: where the checkout
// is, on which branch from which commit, its cone, and how it pushes — or that
// a push is refused, for a repository given to read.
func TestACloneStepsBriefSaysWhereTheCheckoutIs(t *testing.T) {
	t.Parallel()
	read := cloned()
	read.Access = sandbox.RepoRead
	empty := cloned()
	empty.Base, empty.Paths = "", nil
	for _, tc := range []struct {
		name    string
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
		want    []string
		not     []string
	}{
		{"write", cloned(), sandbox.CloneCoding, []string{"`./web`", "branch `seats/swe/ENG-7`", cloneBase[:12],
			"`src`, `docs`", "git push origin seats/swe/ENG-7"}, []string{"refused"}},
		{"read", read, sandbox.CloneCoding, []string{"`./web`", "to read: a push to it is refused"},
			[]string{"git push"}},
		{"empty", empty, sandbox.CloneCoding, []string{"no commits yet", "git push origin seats/swe/ENG-7"},
			[]string{"sparse-checkout"}},
		{"single-sha", cloned(), sandbox.CloneSingleSHA, []string{"`./web`", "at commit " + cloneBase[:12],
			"no history"}, []string{"git push", "branch"}},
	} {
		step, err := sandbox.CloneRecipe(tc.r, tc.purpose)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for _, want := range tc.want {
			if !strings.Contains(step.Brief, want) {
				t.Errorf("%s: the brief does not say %q:\n%s", tc.name, want, step.Brief)
			}
		}
		for _, not := range tc.not {
			if strings.Contains(step.Brief, not) {
				t.Errorf("%s: the brief says %q:\n%s", tc.name, not, step.Brief)
			}
		}
	}
}

// A CLONE RECIPE IS REFUSED FOR WHAT IT CANNOT CLONE: a repository its grammar
// refuses — the recipe interpolates nothing a RunRepo cannot carry — a purpose
// this build does not know, and a check given no commit.
func TestCloneRecipeRefusesWhatItCannotClone(t *testing.T) {
	t.Parallel()
	with := func(edit func(*sandbox.RunRepo)) sandbox.RunRepo {
		r := cloned()
		edit(&r)
		return r
	}
	many := make([]string, sandbox.MaxConePaths+1)
	fill(many)
	for name, tc := range map[string]struct {
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
	}{
		"an invalid name":        {with(func(r *sandbox.RunRepo) { r.Repo = "../web" }), sandbox.CloneCoding},
		"an invalid branch":      {with(func(r *sandbox.RunRepo) { r.Branch = "a b" }), sandbox.CloneCoding},
		"an invalid base":        {with(func(r *sandbox.RunRepo) { r.Base = "main" }), sandbox.CloneCoding},
		"a path out":             {with(func(r *sandbox.RunRepo) { r.Paths = []string{"../etc"} }), sandbox.CloneCoding},
		"a cone past its bound":  {with(func(r *sandbox.RunRepo) { r.Paths = many }), sandbox.CloneCoding},
		"a check with no commit": {with(func(r *sandbox.RunRepo) { r.Base = "" }), sandbox.CloneSingleSHA},
		"a treeless clone":       {cloned(), sandbox.ClonePurpose("treeless")},
	} {
		if step, err := sandbox.CloneRecipe(tc.r, tc.purpose); err == nil {
			t.Errorf("%s: CloneRecipe wrote %q", name, step.Name)
		}
	}
	if !sandbox.CloneCoding.Valid() || !sandbox.CloneSingleSHA.Valid() || sandbox.ClonePurpose("treeless").Valid() {
		t.Error("ClonePurpose.Valid does not answer for exactly the two purposes")
	}
}

// THE WORST CLONE STEP IS ONE SHELL ARGUMENT. A box runs a setup command as one
// argument of its shell, and Linux refuses an argument past MAX_ARG_STRLEN —
// 32 pages, 131 072 bytes with its NUL — before the shell starts. The longest
// step a RunRepo can make — the longest name, a branch and MaxConePaths
// directories of MaxConePathBytes each, all of single quotes, which quoting
// makes four times their length — fits, and a shell takes it.
//
// Mutation: raise MaxConePathBytes to 1 024 — the step is past the limit, and
// the exec is refused, red.
func TestTheWorstCloneStepIsOneShellArgument(t *testing.T) {
	t.Parallel()
	const maxArgStrlen = 32 * 4096
	worst := sandbox.RunRepo{
		Repo: strings.Repeat("a", 63), RepoID: "0190d8f0-0000-7000-8000-00000000000a", Base: cloneBase,
		Branch: strings.Repeat("'", 244), Access: sandbox.RepoWrite,
		Paths: make([]string, sandbox.MaxConePaths),
	}
	for i := range worst.Paths {
		// Each a directory of its own, as a cone's are.
		worst.Paths[i] = strings.Repeat("'", sandbox.MaxConePathBytes-3) + string(rune('a'+i/26)) +
			string(rune('a'+i%26)) + "'"
	}
	if err := worst.Validate(); err != nil {
		t.Fatalf("the worst repository is not a RunRepo: %v", err)
	}
	empty := worst
	empty.Base = ""
	for _, tc := range []struct {
		name    string
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
	}{
		{"coding", worst, sandbox.CloneCoding},
		{"coding an empty repository", empty, sandbox.CloneCoding},
		{"single-sha", worst, sandbox.CloneSingleSHA},
	} {
		step, err := sandbox.CloneRecipe(tc.r, tc.purpose)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		command := step.Commands[0]
		if len(command)+1 > maxArgStrlen {
			t.Errorf("%s: the step is %d bytes, past the %d one argument may have", tc.name, len(command),
				maxArgStrlen)
		}
		syntax(t, tc.name, command)
	}
}

// THE VERSION STEP HOLDS A BOX'S GIT TO THE CLIENT FLOOR, read as gitproc reads
// a version: a git below it — Apple's 2.39.3 among them — fails the step with
// exit 3 and a sentence naming the git and the floor, and so does a box with no
// git, or one that does not say its version; the floor itself, a vendor's
// suffix and a wrapper's own line before git's pass, and so do the pinned git
// and the client floor's own build.
//
// Mutations: compare against 2.39.0 — Apple's git passes, red; read the first
// line whatever it is — the wrapped git fails, red.
func TestTheVersionStepHoldsABoxsGitToTheClientFloor(t *testing.T) {
	t.Parallel()
	command := sandbox.GitVersionStep().Commands[0]
	if name := sandbox.GitVersionStep().Name; name != "git-version" {
		t.Fatalf("the version step is named %q", name)
	}
	run := func(t *testing.T, path string) (int, string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
		cmd.Env = []string{"PATH=" + path, "HOME=" + t.TempDir()}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			return 0, stderr.String()
		case errors.As(err, &exit):
			return exit.ExitCode(), stderr.String()
		}
		t.Fatalf("the version step did not run: %v", err)
		return 0, ""
	}
	withPath := func(dir string) string { return dir + string(os.PathListSeparator) + os.Getenv("PATH") }
	for _, tc := range []struct {
		version string
		exit    int
		says    string
	}{
		{"git version 2.39.3 (Apple Git-145)", 3, "git 2.39.3 in this box is below 2.40.0"},
		{"git version 2.30.9", 3, "git 2.30.9 in this box is below 2.40.0"},
		{"git version 1.99.9", 3, "below 2.40.0"},
		{"git version 2.40.0", 0, ""},
		{"git version 2.40.0.windows.1", 0, ""},
		{"git version 2.45.GIT", 0, ""},
		{"git version 3.0.0", 0, ""},
		{"hub version 2.14.2\ngit version 2.40.1", 0, ""},
		{"", 3, "did not say its version"},
	} {
		code, stderr := run(t, withPath(filepath.Dir(gittest.Fake(t, tc.version, "exit 0"))))
		if code != tc.exit || !strings.Contains(stderr, tc.says) {
			t.Errorf("a box whose git says %q: exit %d, %q; want exit %d saying %q", tc.version, code, stderr,
				tc.exit, tc.says)
		}
	}
	if code, stderr := run(t, t.TempDir()); code != 3 || !strings.Contains(stderr, "this box has no git") {
		t.Errorf("a box with no git: exit %d, %q", code, stderr)
	}
	for _, g := range boxGits(t) {
		if code, stderr := run(t, withPath(filepath.Dir(g.path))); code != 0 {
			t.Errorf("the %s git fails the version step: exit %d, %q", g.name, code, stderr)
		}
	}
}
