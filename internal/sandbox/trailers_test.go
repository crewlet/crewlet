package sandbox_test

import (
	"bytes"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// boxRun is one coding run's trailer values: the seat, the job and the work
// item, as the engine hands them to the box ([sandbox.TrailerEnv]).
type boxRun struct {
	seat, launch string
	item         *types.WorkItem
}

// The runs the cases commit as: this run, on a native item, and another run
// of the same seat on no item at all.
var (
	thisRun  = boxRun{seat: "swe", launch: "0190d8f0-1111-7000-8000-000000000001", item: &engItem}
	otherRun = boxRun{seat: "swe", launch: "0190d8f0-2222-7000-8000-000000000002"}
	engItem  = types.WorkItem{Backend: types.WorkNative, ID: "task-7", Key: "ENG-7", Project: "ENG"}
)

// env is the run's three variables, as "NAME=value" pairs.
func (r boxRun) env(t *testing.T) []string {
	t.Helper()
	vars, err := sandbox.TrailerEnv(r.seat, r.launch, r.item)
	if err != nil {
		t.Fatalf("TrailerEnv: %v", err)
	}
	return []string{
		sandbox.TrailerSeatVar + "=" + vars[sandbox.TrailerSeatVar],
		sandbox.TrailerRunVar + "=" + vars[sandbox.TrailerRunVar],
		sandbox.TrailerWorkItemVar + "=" + vars[sandbox.TrailerWorkItemVar],
	}
}

// want is the trailer block a commit this run made carries.
func (r boxRun) want() []string {
	item := "none"
	if r.item != nil {
		item = string(r.item.Backend) + " " + r.item.Key
	}
	return []string{"Crewlet-Seat: " + r.seat, "Crewlet-Run: " + r.launch, "Crewlet-Work-Item: " + item}
}

// boxGit is one git a box may run: the pinned client, and the client floor's
// own build.
type boxGit struct {
	name string
	path string
}

// boxGits is every git the hook is held under: the pin, and 2.40.4.
func boxGits(t *testing.T) []boxGit {
	t.Helper()
	return []boxGit{
		{"pinned", gittest.Require(t)},
		{"client floor", gittest.ClientFloor(t)},
	}
}

// eachGit runs fn under every box git, each a subtest of its own.
func eachGit(t *testing.T, fn func(t *testing.T, g boxGit)) {
	t.Helper()
	for _, g := range boxGits(t) {
		t.Run(g.name, func(t *testing.T) {
			t.Parallel()
			fn(t, g)
		})
	}
}

// boxClone is a repository as the coding recipe leaves one — the hook
// installed — on main with one commit made outside any run, and the client
// that runs git there.
//
// THE HOOK IS WRITTEN AS THE RECIPE WRITES IT, by a shell, never by this
// process: a file this process opened for writing is held open by any child
// another case forks meanwhile until that child execs, and git's exec of the
// hook would fail ETXTBSY (TestEveryExecutableThisSuiteWritesIsSettled).
func boxClone(t *testing.T, g boxGit) (*gittest.Client, string) {
	t.Helper()
	client := gittest.NewClientWith(t, g.path)
	dir := client.Init(t)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "mkdir -p .git/hooks && printf '%s' "+
		sandbox.ShellQuote(sandbox.TrailerHook)+" > .git/hooks/prepare-commit-msg && "+
		"chmod 755 .git/hooks/prepare-commit-msg")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installing the hook: %v\n%s", err, out)
	}
	client.Commit(t, dir, map[string]string{"README": "hello\n"}, "the first commit")
	return client, dir
}

// in is client running as run: its three variables set.
func in(t *testing.T, client *gittest.Client, run boxRun) *gittest.Client {
	t.Helper()
	return client.Env(run.env(t)...)
}

// trailersOf is the trailer block of rev's message, as git's own reader reads
// it.
func trailersOf(t *testing.T, client *gittest.Client, dir, rev string) []string {
	t.Helper()
	out := strings.TrimSpace(client.Run(t, dir, "log", "-1", "--format=%(trailers:only,unfold)", rev))
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// mustTrail fails t unless rev carries exactly want.
func mustTrail(t *testing.T, client *gittest.Client, dir, rev, what string, want []string) {
	t.Helper()
	if got := trailersOf(t, client, dir, rev); !slices.Equal(got, want) {
		message := client.Run(t, dir, "log", "-1", "--format=%B", rev)
		t.Errorf("%s: the trailers are %q, want %q; the message:\n%s", what, got, want, message)
	}
}

// The editors the cases hand git, as the commands git runs them by — through a
// shell, the file to edit appended — so no case writes an executable of its
// own: the sequence editors turn a rebase's first pick into a reword and its
// second into a squash, and writes is an editor that writes a message from
// nothing.
const (
	rewordFirst  = "sed -i.orig -e '1s/^pick /reword /'"
	squashSecond = "sed -i.orig -e '2s/^pick /squash /'"
	writes       = "printf 'a message the editor wrote\\n' >"
)

// A CODING BOX'S COMMIT CARRIES ITS TRAILERS: a commit made in an engine clone
// with its message given carries exactly the three, naming the seat, the job
// and the work item — a merge and a revert included, a message with no final
// newline or a markdown rule in it, a sign-off before them, and `--no-verify`,
// which does not skip the hook.
//
// Mutations: drop the hook's final-newline line — the merge fails on 2.40.4;
// drop --no-divider — the `---` case fails; refuse the merge source — both
// merge cases fail.
func TestACodingBoxCommitCarriesItsTrailers(t *testing.T) {
	t.Parallel()
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)
		run := in(t, client, thisRun)

		run.Run(t, dir, "commit", "--allow-empty", "-m", "fix the parser")
		mustTrail(t, client, dir, "HEAD", "commit -m", thisRun.want())

		message := filepath.Join(t.TempDir(), "message")
		if err := os.WriteFile(message, []byte("from a file\n\nwith no final newline"), 0o600); err != nil {
			t.Fatal(err)
		}
		run.Run(t, dir, "commit", "--allow-empty", "-F", message)
		mustTrail(t, client, dir, "HEAD", "commit -F with no final newline", thisRun.want())

		run.Run(t, dir, "commit", "--allow-empty", "--no-verify", "-m", "skip the other hooks")
		mustTrail(t, client, dir, "HEAD", "commit --no-verify", thisRun.want())

		run.Run(t, dir, "commit", "--allow-empty", "-s", "-m", "signed")
		mustTrail(t, client, dir, "HEAD", "commit -s",
			append([]string{"Signed-off-by: Crewlet Test <test@example.com>"}, thisRun.want()...))

		run.Run(t, dir, "commit", "--allow-empty", "-m", "a subject\n\nnotes\n---\nmore notes")
		mustTrail(t, client, dir, "HEAD", "a message holding a markdown rule", thisRun.want())

		run.Run(t, dir, "checkout", "-q", "-b", "topic")
		run.Commit(t, dir, map[string]string{"topic.txt": "topic\n"}, "a topic commit")
		run.Run(t, dir, "checkout", "-q", "main")
		run.Run(t, dir, "merge", "--no-ff", "--no-edit", "topic")
		mustTrail(t, client, dir, "HEAD", "merge --no-ff --no-edit", thisRun.want())

		run.Run(t, dir, "checkout", "-q", "-b", "squashed", "HEAD~1")
		run.Run(t, dir, "merge", "--squash", "topic")
		run.Run(t, dir, "commit", "--no-edit")
		mustTrail(t, client, dir, "HEAD", "merge --squash then commit --no-edit", thisRun.want())

		run.Run(t, dir, "revert", "--no-edit", "HEAD")
		mustTrail(t, client, dir, "HEAD", "revert --no-edit", thisRun.want())

		in(t, client, otherRun).Run(t, dir, "commit", "--allow-empty", "-m", "on no work item")
		mustTrail(t, client, dir, "HEAD", "a run on no work item", otherRun.want())
	})
}

// A REWRITTEN BOX COMMIT CARRIES ONE OF EACH TRAILER, naming the run that made
// the rewrite — as git itself replaces the committer — never a second set
// beside the one it carried: an amend in the same run and in another, a
// reword, a cherry-pick, a rebase replaying another run's commit, a squash of
// two runs' commits, `commit -C`, and the hook run twice over one message.
//
// Mutations: `--if-exists addIfDifferent` — the cherry-pick, the rebase and the
// other run's amend each carry two Crewlet-Run; no work-item trailer for a run
// on none — the other run's amend keeps this run's item.
func TestARewrittenBoxCommitCarriesOneOfEachTrailer(t *testing.T) {
	t.Parallel()
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)
		run, other := in(t, client, thisRun), in(t, client, otherRun)

		run.Commit(t, dir, map[string]string{"amended.txt": "to amend\n"}, "to amend")
		run.Run(t, dir, "commit", "--amend", "--no-edit")
		mustTrail(t, client, dir, "HEAD", "amend --no-edit in the same run", thisRun.want())
		run.Run(t, dir, "commit", "--amend", "-m", "amended")
		mustTrail(t, client, dir, "HEAD", "amend -m in the same run", thisRun.want())
		other.Run(t, dir, "commit", "--amend", "--no-edit")
		mustTrail(t, client, dir, "HEAD", "amend in another run on no work item", otherRun.want())

		run.Env("GIT_SEQUENCE_EDITOR="+rewordFirst, "GIT_EDITOR=true").Run(t, dir, "rebase", "-q", "-i", "HEAD~1")
		mustTrail(t, client, dir, "HEAD", "rebase -i reword", thisRun.want())

		other.Run(t, dir, "checkout", "-q", "-b", "theirs")
		other.Commit(t, dir, map[string]string{"theirs.txt": "theirs\n"}, "another run's commit")
		run.Run(t, dir, "checkout", "-q", "main")
		run.Run(t, dir, "cherry-pick", "theirs")
		mustTrail(t, client, dir, "HEAD", "cherry-pick of another run's commit", thisRun.want())

		run.Run(t, dir, "checkout", "-q", "-b", "replayed", "theirs")
		run.Run(t, dir, "rebase", "-q", "--force-rebase", "main~1")
		mustTrail(t, client, dir, "HEAD", "rebase replaying another run's commit", thisRun.want())

		// THE OTHER RUN'S COMMIT LAST, so the squash's message ends on its
		// block: the one the hook must replace.
		run.Run(t, dir, "checkout", "-q", "-b", "two", "main")
		run.Commit(t, dir, map[string]string{"one.txt": "one\n"}, "this run's half")
		other.Commit(t, dir, map[string]string{"two.txt": "two\n"}, "the other run's half")
		run.Env("GIT_SEQUENCE_EDITOR="+squashSecond, "GIT_EDITOR=true").Run(t, dir, "rebase", "-q", "-i", "HEAD~2")
		mustTrail(t, client, dir, "HEAD", "rebase -i squash of two runs' commits", thisRun.want())

		run.Run(t, dir, "commit", "--allow-empty", "-C", "theirs")
		mustTrail(t, client, dir, "HEAD", "commit -C another run's commit", thisRun.want())

		// THE HOOK RUN TWICE over one message leaves it as the first run left
		// it.
		file := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
		if err := os.WriteFile(file, []byte("twice\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		hook := filepath.Join(dir, ".git", "hooks", "prepare-commit-msg")
		runHook(t, g, run, dir, hook, file)
		once, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		runHook(t, g, run, dir, hook, file)
		if twice, _ := os.ReadFile(file); !bytes.Equal(once, twice) {
			t.Errorf("the hook run twice changed the message:\n%s\nthen\n%s", once, twice)
		}
		if !strings.HasSuffix(string(once), "\n\n"+strings.Join(thisRun.want(), "\n")+"\n") {
			t.Errorf("the hook run directly wrote:\n%s", once)
		}
	})
}

// runHook runs the hook as git runs it for a message given whole — sh, the
// message file, `message` — under client's environment with g's git first on
// PATH, in dir, failing t unless it exits 0; it answers what it printed on
// standard error.
func runHook(t *testing.T, g boxGit, client *gittest.Client, dir, hook, file string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", hook, file, "message")
	cmd.Dir = dir
	cmd.Env = onPath(client.Environ(), filepath.Dir(g.path))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("the hook exited %v: %s", err, stderr.String())
	}
	return stderr.String()
}

// onPath is env with dir first on its PATH.
func onPath(env []string, dir string) []string {
	out := slices.Clone(env)
	for i, kv := range out {
		if rest, ok := strings.CutPrefix(kv, "PATH="); ok {
			out[i] = "PATH=" + dir + string(os.PathListSeparator) + rest
			return out
		}
	}
	return append(out, "PATH="+dir)
}

// THE HOOK LEAVES GIT'S MESSAGE RULES ALONE: git refuses exactly the messages it
// refuses without the hook, with its own sentence — an empty one given whole,
// with and without a sign-off; one an editor leaves empty, with and without a
// verbose diff; an untouched template — and a message an editor writes from
// nothing carries no trailer, nor does one given whole whose every line is a
// comment to the hook, which cannot tell it from an editor's.
//
// Mutations: drop the `template` case — the untouched template commits; drop
// the content check — the empty, signed-off empty and both editor cases
// commit; drop the cut-line sed — the verbose case commits; drop the
// Signed-off-by filter — the signed-off empty case commits.
func TestTheTrailerHookLeavesGitsMessageRulesAlone(t *testing.T) {
	t.Parallel()
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)
		run := in(t, client, thisRun)
		template := filepath.Join(t.TempDir(), "template")
		if err := os.WriteFile(template, []byte("A subject to fill in\n\n# Say why.\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			c    *gittest.Client
			args []string
			says string
		}{
			{"an empty message given whole", run, []string{"commit", "--allow-empty", "-m", ""},
				"empty commit message"},
			{"an empty message signed off", run, []string{"commit", "--allow-empty", "-s", "-m", ""},
				"empty commit message"},
			{"an editor that leaves the message empty", run.Env("GIT_EDITOR=true"),
				[]string{"commit", "--allow-empty"}, "empty commit message"},
			{"the same with a verbose diff", run.Env("GIT_EDITOR=true"),
				[]string{"commit", "-v"}, "empty commit message"},
			{"an untouched template", run.Env("GIT_EDITOR=true"),
				[]string{"commit", "--allow-empty", "-t", template}, "did not edit the message"},
		} {
			if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte(tc.name+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			client.Run(t, dir, "add", "staged.txt")
			head := strings.TrimSpace(client.Run(t, dir, "rev-parse", "HEAD"))
			_, stderr, err := tc.c.Try(t, dir, nil, tc.args...)
			if err == nil {
				t.Errorf("%s: committed, where git refuses it", tc.name)
				continue
			}
			if !strings.Contains(stderr, tc.says) {
				t.Errorf("%s: refused saying %q, want git's own %q", tc.name, stderr, tc.says)
			}
			if now := strings.TrimSpace(client.Run(t, dir, "rev-parse", "HEAD")); now != head {
				t.Errorf("%s: HEAD moved to %s", tc.name, now)
			}
		}
		client.Run(t, dir, "reset", "-q", "--hard")

		run.Env("GIT_EDITOR="+writes).Run(t, dir, "commit", "--allow-empty")
		mustTrail(t, client, dir, "HEAD", "a message an editor wrote from nothing", nil)
		if got := client.Run(t, dir, "log", "-1", "--format=%s"); got != "a message the editor wrote\n" {
			t.Errorf("the editor's message became %q", got)
		}

		run.Run(t, dir, "commit", "--allow-empty", "-m", "#42 fix the parser")
		mustTrail(t, client, dir, "HEAD", "a message whose every line starts with #", nil)
		if got := client.Run(t, dir, "log", "-1", "--format=%s"); got != "#42 fix the parser\n" {
			t.Errorf("the message given whole became %q", got)
		}
	})
}

// THE HOOK IS INERT OUTSIDE A RUN: a git that runs without all three of the
// run's variables — one another process in the box started — commits with no
// trailer, never an empty one.
//
// Mutation: drop the variable check — an empty Crewlet-Seat trailer, red.
func TestTheTrailerHookIsInertOutsideARun(t *testing.T) {
	t.Parallel()
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)
		all := thisRun.env(t)
		for i, missing := range []string{sandbox.TrailerSeatVar, sandbox.TrailerRunVar, sandbox.TrailerWorkItemVar} {
			env := slices.Delete(slices.Clone(all), i, i+1)
			client.Env(env...).Run(t, dir, "commit", "--allow-empty", "-m", "without "+missing)
			mustTrail(t, client, dir, "HEAD", "a commit without "+missing, nil)
		}
	})
}

// A TRAILER FAILURE NEVER STOPS A COMMIT: whatever interpret-trailers does,
// the hook exits 0, leaves the message as it was and says so in one line on
// standard error. Run directly — git puts its own exec path first on a hook's
// PATH, which no stand-in can precede — under a stand-in that fails
// interpret-trailers in front of the pinned git: what it holds is the
// script's, never a git version's.
//
// Mutation: end the hook with interpret-trailers' status — exit 1, red.
func TestATrailerFailureNeverStopsACommit(t *testing.T) {
	t.Parallel()
	failing := gittest.Wrap(t, `for arg in "$@"; do [ "$arg" = interpret-trailers ] && exit 1; done`)
	client, dir := boxClone(t, boxGit{"pinned", gittest.Require(t)})
	run := in(t, client, thisRun)
	file := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	const message = "fix the parser\n\nwith a body\n"
	if err := os.WriteFile(file, []byte(message), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr := runHook(t, boxGit{"failing", failing}, run, dir,
		filepath.Join(dir, ".git", "hooks", "prepare-commit-msg"), file)
	if got, _ := os.ReadFile(file); string(got) != message {
		t.Errorf("a failed interpret-trailers left the message as %q, want it untouched", got)
	}
	if stderr != "crewlet: this commit's Crewlet trailers could not be added\n" {
		t.Errorf("the hook said %q, want its one line", stderr)
	}
}

// NO TRAILER VALUE CAN INJECT ANYTHING: a value the hook is handed is never run
// as shell, and every value the engine hands it is one line of a safe set, so
// none writes a line past its own trailer — a sign-off forged in a tracker's
// key included — and the key comes back whole from it. TrailerEnv refuses what
// is the engine's own bug: no seat, a job that is not a canonical id, an item
// with no backend.
//
// Mutations: build the --trailer arguments through eval — the raw values create
// files; pass the key unencoded — the forged sign-off is a fourth trailer;
// accept any launch id — the refusals pass.
func TestNoTrailerValueCanInject(t *testing.T) {
	t.Parallel()
	safe := regexp.MustCompile(`^[A-Za-z0-9._~/#!@+%-]+$`)
	keys := []string{"line\nbreak", "carriage\rreturn", "nul\x00byte", "tab\there", "two words", "100%",
		"a:b", "über", "\nSigned-off-by: Evil <e@x>"}
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)

		// RAW VALUES, as a box could set them: verbatim, and nothing runs.
		raw := map[string]string{
			sandbox.TrailerSeatVar:     "$(touch x)",
			sandbox.TrailerRunVar:      "`touch y`",
			sandbox.TrailerWorkItemVar: `;touch z " ' $HOME *`,
		}
		client.Env(sandbox.TrailerSeatVar+"="+raw[sandbox.TrailerSeatVar],
			sandbox.TrailerRunVar+"="+raw[sandbox.TrailerRunVar],
			sandbox.TrailerWorkItemVar+"="+raw[sandbox.TrailerWorkItemVar]).
			Run(t, dir, "commit", "--allow-empty", "-m", "raw values")
		mustTrail(t, client, dir, "HEAD", "raw values", []string{
			"Crewlet-Seat: " + raw[sandbox.TrailerSeatVar], "Crewlet-Run: " + raw[sandbox.TrailerRunVar],
			"Crewlet-Work-Item: " + raw[sandbox.TrailerWorkItemVar],
		})
		for _, name := range []string{"x", "y", "z"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a raw value ran a command: %s exists (%v)", name, err)
			}
		}

		// THE ENGINE'S VALUES: one safe line each, the key back whole, and
		// three trailers on the commit whatever the key held.
		for _, key := range keys {
			run := boxRun{seat: "swe", launch: thisRun.launch,
				item: &types.WorkItem{Backend: types.WorkJira, ID: "1", Key: key}}
			vars, err := sandbox.TrailerEnv(run.seat, run.launch, run.item)
			if err != nil {
				t.Fatalf("TrailerEnv(%q): %v", key, err)
			}
			backend, encoded, ok := strings.Cut(vars[sandbox.TrailerWorkItemVar], " ")
			if !ok || backend != "jira" || strings.Contains(encoded, " ") {
				t.Errorf("the work item for %q is %q, want `jira <one word>`", key, vars[sandbox.TrailerWorkItemVar])
			}
			for _, value := range []string{vars[sandbox.TrailerSeatVar], vars[sandbox.TrailerRunVar], encoded} {
				if !safe.MatchString(value) {
					t.Errorf("for %q the value %q leaves the safe set", key, value)
				}
			}
			if back, err := url.PathUnescape(encoded); err != nil || back != key {
				t.Errorf("%q came back from %q as %q (%v)", key, encoded, back, err)
			}
			client.Env(run.env(t)...).Run(t, dir, "commit", "--allow-empty", "-m", "a hostile key")
			got := trailersOf(t, client, dir, "HEAD")
			if len(got) != 3 || !strings.HasPrefix(got[2], "Crewlet-Work-Item: jira ") {
				t.Errorf("a commit on key %q carries %q, want exactly the three", key, got)
			}
		}
	})

	for _, tc := range []struct {
		name         string
		seat, launch string
		item         *types.WorkItem
	}{
		{"no seat", "", thisRun.launch, nil},
		{"a launch id that is no UUID", "swe", "launch-1", nil},
		{"a launch id in upper case", "swe", strings.ToUpper(thisRun.launch), nil},
		{"an item with no backend", "swe", thisRun.launch, &types.WorkItem{Key: "ENG-7"}},
	} {
		if _, err := sandbox.TrailerEnv(tc.seat, tc.launch, tc.item); err == nil {
			t.Errorf("TrailerEnv took %s", tc.name)
		}
	}
	vars, err := sandbox.TrailerEnv("swe", thisRun.launch, &types.WorkItem{Backend: types.WorkNative})
	if err != nil || vars[sandbox.TrailerWorkItemVar] != "native" {
		t.Errorf("an item with no key gives %q (%v), want its backend alone", vars[sandbox.TrailerWorkItemVar], err)
	}
}

// NOTHING BUT THE HOOK SPELLS A TRAILER KEY. The trailers are a convention a
// box can forge, so no reader of a pushed commit's trailers may exist in the
// engine: no Go file the binary is built from, outside trailers.go, names a
// Crewlet trailer key in any case (git matches keys without it). The binary is
// what `go list -deps ./cmd/crewlet` names; a test-support package, which may
// forge them to show the host ignores them, is outside it. Every non-test Go
// file of a listed directory is read, whatever its build constraints.
//
// Mutation: a file of internal/code/host reading `Crewlet-Run:` from a pushed
// commit — red.
func TestNothingButTheHookSpellsATrailerKey(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-f",
		"{{if not .Standard}}{{.Dir}}{{end}}", "./cmd/crewlet")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps ./cmd/crewlet: %v: %s", err, stderr.String())
	}
	// A KEY AS A READER SPELLS ONE: followed by a separator (`:`, `=`), a
	// format's `,` or `)`, a quote, a space or the line's end — never by a
	// word character, `-` or `.`, so a file named crewlet-seat.md (an agent
	// definition's example in cliprofile) is not one.
	key := regexp.MustCompile(`(?i)crewlet-(?:seat|run|work-item)(?:[^.\w-]|$)`)
	own := filepath.Join(root, "internal", "sandbox", "trailers.go")
	read, inOwn := 0, false
	for dir := range strings.Lines(string(out)) {
		dir = strings.TrimSpace(dir)
		if dir == "" || !strings.HasPrefix(dir, root+string(filepath.Separator)) {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}
			read++
			if !key.Match(body) {
				continue
			}
			if path == own {
				inOwn = true
				continue
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s names a Crewlet trailer key: nothing but the hook may, since a box can forge one", rel)
		}
	}
	if read == 0 {
		t.Fatal("the walk read no file of the binary")
	}
	if !inOwn {
		t.Fatal("the walk did not find the keys in internal/sandbox/trailers.go: it is reading the wrong files")
	}
}
