package sandbox_test

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
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

// checked is the repository a check's box is given: its one commit, on no
// branch, to read.
func checked() sandbox.RunRepo {
	r := cloned()
	r.Branch, r.Access = "", sandbox.RepoRead
	return r
}

// borrowedAt is [checked] borrowing the objects mounted at objects in the box.
func borrowedAt(objects string) sandbox.RunRepo {
	r := checked()
	r.Objects = objects
	return r
}

// boxObjects is where the cases' borrowed checkouts read their objects.
const boxObjects = "/crewlet/borrowed/web/objects"

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
		if err := r.ValidateFor(sandbox.CloneCoding); err != nil {
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
		if err := r.ValidateFor(sandbox.CloneCoding); err == nil {
			t.Errorf("%s was taken: %+v", name, r)
		}
	}
}

// A RUN REPOSITORY IS HELD TO ITS PURPOSE: a coding run's works on a branch
// and borrows nothing; a check's is one commit — a base, no branch, read
// access, since its box's credential never pushes — and a borrowed checkout's
// names where its objects are in the box, an absolute, clean path a mount
// takes, outside the box's home; a single-commit checkout fetches its objects
// and names none. A value a purpose does not read is refused, never ignored,
// and so is a purpose this build does not know.
//
// Mutation: take a check's repository with a branch — "a check on a branch"
// is taken, red.
func TestARunRepoIsHeldToItsPurpose(t *testing.T) {
	t.Parallel()
	with := func(r sandbox.RunRepo, edit func(*sandbox.RunRepo)) sandbox.RunRepo {
		edit(&r)
		return r
	}
	for _, tc := range []struct {
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
	}{
		{cloned(), sandbox.CloneCoding},
		{checked(), sandbox.CloneSingleSHA},
		{with(checked(), func(r *sandbox.RunRepo) { r.Paths = nil }), sandbox.CloneSingleSHA},
		{borrowedAt(boxObjects), sandbox.CloneBorrowed},
		{borrowedAt("/srv/it's/objects"), sandbox.CloneBorrowed},
	} {
		if err := tc.r.ValidateFor(tc.purpose); err != nil {
			t.Errorf("%s: %+v: %v", tc.purpose, tc.r, err)
		}
	}
	for name, tc := range map[string]struct {
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
	}{
		"a coding run with no branch":         {with(cloned(), func(r *sandbox.RunRepo) { r.Branch = "" }), sandbox.CloneCoding},
		"a coding run borrowing objects":      {with(cloned(), func(r *sandbox.RunRepo) { r.Objects = boxObjects }), sandbox.CloneCoding},
		"a check with no commit":              {with(checked(), func(r *sandbox.RunRepo) { r.Base = "" }), sandbox.CloneSingleSHA},
		"a check on a branch":                 {with(checked(), func(r *sandbox.RunRepo) { r.Branch = "main" }), sandbox.CloneSingleSHA},
		"a check that may push":               {with(checked(), func(r *sandbox.RunRepo) { r.Access = sandbox.RepoWrite }), sandbox.CloneSingleSHA},
		"a single commit given objects":       {borrowedAt(boxObjects), sandbox.CloneSingleSHA},
		"a borrowed checkout naming none":     {checked(), sandbox.CloneBorrowed},
		"a borrowed checkout on a branch":     {with(borrowedAt(boxObjects), func(r *sandbox.RunRepo) { r.Branch = "main" }), sandbox.CloneBorrowed},
		"a borrowed checkout with no commit":  {with(borrowedAt(boxObjects), func(r *sandbox.RunRepo) { r.Base = "" }), sandbox.CloneBorrowed},
		"objects at a relative path":          {borrowedAt("crewlet/objects"), sandbox.CloneBorrowed},
		"objects at an unclean path":          {borrowedAt("/crewlet/../objects"), sandbox.CloneBorrowed},
		"objects at the root":                 {borrowedAt("/"), sandbox.CloneBorrowed},
		"objects in the box's home":           {borrowedAt(sandbox.DefaultHome + "/objects"), sandbox.CloneBorrowed},
		"objects at the box's home":           {borrowedAt(sandbox.DefaultHome), sandbox.CloneBorrowed},
		"objects at a path a -v splits":       {borrowedAt("/crewlet/a:b"), sandbox.CloneBorrowed},
		"objects at a path holding a newline": {borrowedAt("/crewlet/a\nb"), sandbox.CloneBorrowed},
		"a purpose this build does not know":  {checked(), sandbox.ClonePurpose("treeless")},
	} {
		if err := tc.r.ValidateFor(tc.purpose); err == nil {
			t.Errorf("%s was taken for %s: %+v", name, tc.purpose, tc.r)
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
	monitor := `  { git version | awk -v want=2.55.0 ` + sandbox.ShellQuote(`function n(s,  a) { split(s, a, "."); return a[1] * 1000000 + a[2] * 1000 + a[3] }
/^git version / { seen = 1; v = $3; sub(/[^0-9.].*$/, "", v); ok = n(v) >= n(want); exit }
END { exit !(seen && ok) }`) + ` &&
    [ "$(find 'web' -type d | wc -l)" -le "$(cat /proc/sys/fs/inotify/max_user_watches 2>/dev/null || echo 0)" ] &&
    git -C 'web' config core.fsmonitor true || :; }`
	coding := `if [ ! -e 'web'/.git ]; then
  git clone --quiet --filter=blob:none --no-checkout -- "$CREWLET_GIT_URL"/'web.git' 'web' &&
  git -C 'web' sparse-checkout set --cone --skip-checks -- 'src' 'docs' &&
  git -C 'web' fetch --quiet --no-tags --filter=blob:none origin '` + cloneBase + `' &&
  git -C 'web' checkout --quiet -B 'seats/swe/ENG-7' '` + cloneBase + `' &&
  { [ -e 'web'/.git/hooks/prepare-commit-msg ] || [ -L 'web'/.git/hooks/prepare-commit-msg ] ||
    { mkdir -p 'web'/.git/hooks && printf '%s' ` + hook + ` > 'web'/.git/hooks/prepare-commit-msg &&
      chmod 755 'web'/.git/hooks/prepare-commit-msg; }; } &&
` + monitor + `
fi`
	empty := `if [ ! -e 'web'/.git ]; then
  git clone --quiet --filter=blob:none --no-checkout -- "$CREWLET_GIT_URL"/'web.git' 'web' &&
  git -C 'web' symbolic-ref HEAD 'refs/heads/seats/swe/ENG-7' &&
  { [ -e 'web'/.git/hooks/prepare-commit-msg ] || [ -L 'web'/.git/hooks/prepare-commit-msg ] ||
    { mkdir -p 'web'/.git/hooks && printf '%s' ` + hook + ` > 'web'/.git/hooks/prepare-commit-msg &&
      chmod 755 'web'/.git/hooks/prepare-commit-msg; }; } &&
` + monitor + `
fi`
	single := `if [ ! -e 'web'/.git ]; then
  git init --quiet 'web' && git -C 'web' remote add origin "$CREWLET_GIT_URL"/'web.git' &&
  git -C 'web' fetch --quiet --no-tags --depth=1 --filter=blob:none origin '` + cloneBase + `' &&
  git -C 'web' sparse-checkout set --cone --skip-checks -- 'src' 'docs' &&
  git -C 'web' checkout --quiet --detach '` + cloneBase + `'
fi`
	borrowed := `if [ ! -e 'web'/.git ]; then
  git init --quiet 'web' && git -C 'web' remote add origin "$CREWLET_GIT_URL"/'web.git' &&
  printf '%s\n' '` + boxObjects + `' > 'web'/.git/objects/info/alternates &&
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
		{"single-sha", checked(), sandbox.CloneSingleSHA, "checkout web", single},
		{"borrowed", borrowedAt(boxObjects), sandbox.CloneBorrowed, "checkout web", borrowed},
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
// dangling link — and the two checks' recipes write none. The borrowed recipe
// fetches nothing, and names objects whose path holds a quote and a command
// substitution in its alternates file as the bytes they are. A box that
// already holds the checkout runs nothing at all.
//
// Mutations: interpolate the branch unquoted — the stand-in's arguments and a
// file named x, red; write the hook in the single-sha recipe — red; drop the
// hook's existence guard — the box's own hook is replaced, red; drop the
// checkout's guard — the second run clones again, red; write the objects
// unquoted — x is made and the alternates name another path, red.
func TestACloneStepsValuesArriveAsLiteralArguments(t *testing.T) {
	t.Parallel()
	const url = "https://git.example.com/box/git"
	stub := gittest.Fake(t, "git version 2.56.0", `{ for arg in "$@"; do printf '%s\037' "$arg"; done; printf '\n'; } >> "$STUB_CALLS"
case "$1" in
clone|init)
  for dir in "$@"; do :; done
  mkdir -p "$dir/.git/hooks" "$dir/.git/objects/info"
  case "$1:${STUB_HOOK-}" in
  clone:file) printf 'the box template' > "$dir/.git/hooks/prepare-commit-msg" ;;
  clone:link) ln -s nowhere "$dir/.git/hooks/prepare-commit-msg" ;;
  esac ;;
esac
exit 0`)
	watches(t, filepath.Dir(stub))
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
			"CREWLET_GIT_URL=" + url, "STUB_CALLS=" + calls, "STUB_HOOK=" + hook, "HOME=" + t.TempDir(),
			"STUB_WATCHES=1000000"}
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
			{"-C", "web", "config", "core.fsmonitor", "true"},
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
			{"-C", "web", "config", "core.fsmonitor", "true"},
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
	check := r
	check.Branch, check.Access = "", sandbox.RepoRead
	t.Run("single-sha", func(t *testing.T) {
		t.Parallel()
		step, err := sandbox.CloneRecipe(check, sandbox.CloneSingleSHA)
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
	// THE BORROWED CHECKOUT FETCHES NOTHING, and names its objects in its
	// alternates file whatever their path holds: written by the shell, the
	// path's quote and its command substitution arrive as the bytes named.
	t.Run("borrowed", func(t *testing.T) {
		t.Parallel()
		borrowed := check
		borrowed.Objects = `/crewlet/borrowed/it's $(touch${IFS}x)/objects`
		step, err := sandbox.CloneRecipe(borrowed, sandbox.CloneBorrowed)
		if err != nil {
			t.Fatal(err)
		}
		box := t.TempDir()
		sameCalls(t, run(t, box, step, ""), [][]string{
			{"init", "--quiet", "web"},
			{"-C", "web", "remote", "add", "origin", url + "/web.git"},
			append([]string{"-C", "web", "sparse-checkout", "set", "--cone", "--skip-checks", "--"}, cone...),
			{"-C", "web", "checkout", "--quiet", "--detach", cloneBase},
		})
		alternates, err := os.ReadFile(filepath.Join(box, "web", ".git", "objects", "info", "alternates"))
		if err != nil || string(alternates) != borrowed.Objects+"\n" {
			t.Errorf("the borrowed checkout's alternates are %q (%v), want its objects %q and a newline",
				alternates, err, borrowed.Objects)
		}
		if _, err := os.Stat(filepath.Join(box, "x")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a value of the step ran as a command: x exists (%v)", err)
		}
		if _, err := os.Lstat(filepath.Join(box, "web", ".git", "hooks", "prepare-commit-msg")); !errors.Is(err,
			os.ErrNotExist) {
			t.Errorf("a borrowed checkout has a commit hook (%v): a check box commits nothing", err)
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

// watches puts a stand-in cat in dir, a directory a case puts first on PATH:
// asked for how many directories the box's user may watch, it answers
// $STUB_WATCHES, so a case decides what the kernel says whatever kernel runs
// it; anything else it reads as cat does. It is written settled
// ([sandbox.WriteRuntime]), since the recipe execs it at once.
func watches(t *testing.T, dir string) {
	t.Helper()
	sandbox.WriteRuntime(t, filepath.Join(dir, "cat"),
		"if [ \"$1\" = /proc/sys/fs/inotify/max_user_watches ]; then\n"+
			"  printf '%s\\n' \"$STUB_WATCHES\"; exit 0\nfi\nexec /bin/cat \"$@\"\n")
}

// A CODING CLONE TURNS GIT'S MONITOR ON WHERE THE BOX CAN KEEP IT, and only
// there: in the checkout's own configuration where the box's git is 2.55.0
// or newer and the box's user may watch every directory the checkout holds;
// never with an older git or too few watches, and never a failure of the
// step either way — the monitor is speed, the checkout is the step. Run under
// a shell against stand-ins: a git of each version, and a cat answering the
// kernel's watch limit.
//
// Mutations: drop the version check — 2.54.0 turns it on, red; compare with
// `>` — 2.55.0 leaves it off, red; drop the watch check — two watches turn it
// on, red; drop the `|| :` — the step fails where it stays off, red.
func TestACodingCloneTurnsTheMonitorOnWhereTheBoxCanKeepIt(t *testing.T) {
	t.Parallel()
	r := cloned()
	r.Paths = nil
	step, err := sandbox.CloneRecipe(r, sandbox.CloneCoding)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, version, watches string
		on                     bool
	}{
		{"the node pin's git with watches to spare", "git version 2.56.0", "1000000", true},
		{"the floor's own release", "git version 2.55.0", "1000000", true},
		{"a vendor's build of the floor", "git version 2.55.0 (Apple Git-160)", "1000000", true},
		{"a git below the floor", "git version 2.54.0", "1000000", false},
		{"fewer watches than the checkout's directories", "git version 2.56.0", "2", false},
		{"no watch limit to read", "git version 2.56.0", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := gittest.Fake(t, tc.version, `{ for arg in "$@"; do printf '%s\037' "$arg"; done; printf '\n'; } >> "$STUB_CALLS"
case "$1" in
clone) for dir in "$@"; do :; done; mkdir -p "$dir/.git/hooks" "$dir/src/lib" ;;
esac
exit 0`)
			watches(t, filepath.Dir(stub))
			calls := filepath.Join(t.TempDir(), "calls")
			cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", step.Commands[0])
			cmd.Dir = t.TempDir()
			cmd.Env = []string{"PATH=" + filepath.Dir(stub) + string(os.PathListSeparator) + os.Getenv("PATH"),
				"CREWLET_GIT_URL=https://git.example.com/box/git", "STUB_CALLS=" + calls, "HOME=" + t.TempDir(),
				"STUB_WATCHES=" + tc.watches}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("the coding clone failed: %v\n%s", err, out)
			}
			body, err := os.ReadFile(calls)
			if err != nil {
				t.Fatal(err)
			}
			on := strings.Contains(string(body), "config\x1fcore.fsmonitor\x1ftrue\x1f")
			if on != tc.on {
				t.Errorf("the monitor turned on = %v, want %v; git saw\n%q", on, tc.on, body)
			}
		})
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
		{"single-sha", checked(), sandbox.CloneSingleSHA, []string{"`./web`", "at commit " + cloneBase[:12],
			"no history"}, []string{"git push", "branch"}},
		{"borrowed", borrowedAt(boxObjects), sandbox.CloneBorrowed, []string{"`./web`",
			"at commit " + cloneBase[:12], "this node's copy", "`" + boxObjects + "`", "read-only"},
			[]string{"git push", "branch", "fetched"}},
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

// A CLONE RECIPE IS REFUSED FOR WHAT IT CANNOT CLONE: a repository its
// purpose's grammar refuses — the recipe interpolates nothing a RunRepo cannot
// carry — a purpose this build does not know, a check given no commit, and a
// borrowed checkout given no objects.
func TestCloneRecipeRefusesWhatItCannotClone(t *testing.T) {
	t.Parallel()
	with := func(edit func(*sandbox.RunRepo)) sandbox.RunRepo {
		r := cloned()
		edit(&r)
		return r
	}
	many := make([]string, sandbox.MaxConePaths+1)
	fill(many)
	uncommitted := checked()
	uncommitted.Base = ""
	for name, tc := range map[string]struct {
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
	}{
		"an invalid name":                   {with(func(r *sandbox.RunRepo) { r.Repo = "../web" }), sandbox.CloneCoding},
		"an invalid branch":                 {with(func(r *sandbox.RunRepo) { r.Branch = "a b" }), sandbox.CloneCoding},
		"an invalid base":                   {with(func(r *sandbox.RunRepo) { r.Base = "main" }), sandbox.CloneCoding},
		"a path out":                        {with(func(r *sandbox.RunRepo) { r.Paths = []string{"../etc"} }), sandbox.CloneCoding},
		"a cone past its bound":             {with(func(r *sandbox.RunRepo) { r.Paths = many }), sandbox.CloneCoding},
		"a check with no commit":            {uncommitted, sandbox.CloneSingleSHA},
		"a check on a branch":               {with(func(r *sandbox.RunRepo) { r.Access = sandbox.RepoRead }), sandbox.CloneSingleSHA},
		"a borrowed checkout of no objects": {checked(), sandbox.CloneBorrowed},
		"a treeless clone":                  {cloned(), sandbox.ClonePurpose("treeless")},
	} {
		if step, err := sandbox.CloneRecipe(tc.r, tc.purpose); err == nil {
			t.Errorf("%s: CloneRecipe wrote %q", name, step.Name)
		}
	}
	for _, purpose := range sandbox.ClonePurposes() {
		if !purpose.Valid() {
			t.Errorf("ClonePurpose.Valid refuses %s, one of ClonePurposes", purpose)
		}
	}
	if got := sandbox.ClonePurposes(); !slices.Equal(got, []sandbox.ClonePurpose{sandbox.CloneCoding,
		sandbox.CloneSingleSHA, sandbox.CloneBorrowed}) || sandbox.ClonePurpose("treeless").Valid() {
		t.Errorf("ClonePurposes() = %v: want the three purposes, and Valid to refuse any other", got)
	}
}

// THE WORST CLONE STEP IS ONE SHELL ARGUMENT. A box runs a setup command as one
// argument of its shell, and Linux refuses an argument past MAX_ARG_STRLEN —
// 32 pages, 131 072 bytes with its NUL — before the shell starts. The longest
// step a RunRepo can make for each purpose — the longest name, a branch or a
// borrowed checkout's objects at MaxMountPathBytes, and MaxConePaths
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
	empty := worst
	empty.Base = ""
	check := worst
	check.Branch, check.Access = "", sandbox.RepoRead
	borrowed := check
	borrowed.Objects = "/" + strings.Repeat("'", sandbox.MaxMountPathBytes-1)
	for _, tc := range []struct {
		name    string
		r       sandbox.RunRepo
		purpose sandbox.ClonePurpose
	}{
		{"coding", worst, sandbox.CloneCoding},
		{"coding an empty repository", empty, sandbox.CloneCoding},
		{"single-sha", check, sandbox.CloneSingleSHA},
		{"borrowed", borrowed, sandbox.CloneBorrowed},
	} {
		if err := tc.r.ValidateFor(tc.purpose); err != nil {
			t.Fatalf("%s: the worst repository is not one: %v", tc.name, err)
		}
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

// THE BORROWED RECIPE FETCHES NOTHING. Run under a shell with a real git — the
// pin and the client floor's own build — against a git node's copy of a
// repository standing as the objects it borrows (packed, with a
// commit-graph), it checks the commit out detached, the cone's directories
// and the top level's files alone, with the copy's whole history read through
// its alternates — while the code host, its remote, is asked for nothing: a
// server standing in for it, which would answer, sees no request. And it
// writes nothing into the copy it borrows.
//
// Mutation: drop the alternates line — the checkout finds no such commit, red.
func TestTheBorrowedRecipeFetchesNothing(t *testing.T) {
	t.Parallel()
	eachGit(t, func(t *testing.T, g boxGit) {
		client := gittest.NewClientWith(t, g.path)
		node := client.Init(t)
		client.Commit(t, node, map[string]string{"README": "hello\n", "src/a.txt": "a\n", "docs/d.txt": "d\n",
			"other/o.txt": "o\n"}, "the first commit")
		tip := client.Commit(t, node, map[string]string{"src/lib/b.txt": "b\n"}, "the second commit")
		client.Run(t, node, "repack", "-a", "-d", "-q")
		client.Run(t, node, "commit-graph", "write", "--reachable")
		objects := filepath.Join(node, ".git", "objects")
		before := treeOf(t, objects)

		var asked atomic.Int64
		host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			asked.Add(1)
			http.Error(w, "the code host is asked for nothing by a borrowed checkout", http.StatusServiceUnavailable)
		}))
		t.Cleanup(host.Close)

		r := borrowedAt(objects)
		r.Base, r.Paths = tip, []string{"src"}
		step, err := sandbox.CloneRecipe(r, sandbox.CloneBorrowed)
		if err != nil {
			t.Fatal(err)
		}
		box := t.TempDir()
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", step.Commands[0])
		cmd.Dir = box
		cmd.Env = append(client.Environ(),
			"PATH="+filepath.Dir(g.path)+string(os.PathListSeparator)+os.Getenv("PATH"),
			"CREWLET_GIT_URL="+host.URL+"/box/git")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the borrowed recipe: %v\n%s", err, out)
		}

		checkout := filepath.Join(box, "web")
		if got := strings.TrimSpace(client.Run(t, checkout, "rev-parse", "HEAD")); got != tip {
			t.Errorf("the borrowed checkout is at %s, want %s", got, tip)
		}
		if _, _, err := client.Try(t, checkout, nil, "symbolic-ref", "-q", "HEAD"); err == nil {
			t.Error("the borrowed checkout is on a branch, want HEAD detached")
		}
		if got := strings.TrimSpace(client.Run(t, checkout, "rev-list", "--count", "HEAD")); got != "2" {
			t.Errorf("the borrowed checkout reads %s commits of history, want the copy's 2", got)
		}
		if got, want := strings.TrimSpace(client.Run(t, checkout, "remote", "get-url", "origin")),
			host.URL+"/box/git/web.git"; got != want {
			t.Errorf("the borrowed checkout's remote is %q, want the code host's %q", got, want)
		}
		var files []string
		if err := filepath.WalkDir(checkout, func(p string, d os.DirEntry, err error) error {
			switch {
			case err != nil:
				return err
			case d.IsDir() && d.Name() == ".git":
				return filepath.SkipDir
			case !d.IsDir():
				rel, err := filepath.Rel(checkout, p)
				files = append(files, filepath.ToSlash(rel))
				return err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		slices.Sort(files)
		if want := []string{"README", "src/a.txt", "src/lib/b.txt"}; !slices.Equal(files, want) {
			t.Errorf("the borrowed checkout holds %q, want the cone's and the top level's %q", files, want)
		}
		if n := asked.Load(); n != 0 {
			t.Errorf("the borrowed recipe asked the code host %d times", n)
		}
		if after := treeOf(t, objects); !maps.Equal(after, before) {
			t.Errorf("the borrowed recipe wrote into the copy it borrows:\nbefore %v\nafter  %v", before, after)
		}
	})
}

// treeOf is every entry under dir with its size, mode and modification time —
// what a writer into it would move.
func treeOf(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		out[rel] = fmt.Sprintf("%d %v %d", info.Size(), info.Mode(), info.ModTime().UnixNano())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}
