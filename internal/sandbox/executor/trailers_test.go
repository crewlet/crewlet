package executor_test

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// The trailer cases: the hook a run is handed ([executor.AttributionHook]),
// run by real gits — the pinned one and the client floor's — over every way a
// commit is made and rewritten. What the wrapper and Start do around it is
// attribution_test.go's.

// boxRun is one executor's attribution: the seat, the run (its turn) and the
// work item, as the launch asks its runner for them
// ([sandbox.CommitAttribution]).
type boxRun struct {
	seat, run string
	item      *types.WorkItem
}

// The runs the cases commit as: this run, on a native item, and another run
// of the same seat on no item at all. Both commit as the seat's one identity,
// so each credits the other's commits it rewrites.
var (
	thisRun  = boxRun{seat: "swe", run: "0190d8f0-1111-7000-8000-000000000001", item: &engItem}
	otherRun = boxRun{seat: "swe", run: "0190d8f0-2222-7000-8000-000000000002"}
	engItem  = types.WorkItem{Backend: types.WorkWorklet, ID: "task-7", Key: "ENG-7", Project: "ENG"}
)

// attribution is what r's launch asks its runner for.
func (r boxRun) attribution() sandbox.CommitAttribution {
	return sandbox.CommitAttribution{Run: r.run, Seat: r.seat, WorkItem: r.item}
}

// want is the trailer block a commit this run made carries.
func (r boxRun) want() []string {
	item := "none"
	if r.item != nil {
		item = string(r.item.Backend) + " " + r.item.Key
	}
	return []string{credit, executor.SeatTrailer + ": " + r.seat, executor.RunTrailer + ": " + r.run,
		executor.WorkItemTrailer + ": " + item}
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

// boxClone is a repository on main with one commit made outside any run, and
// the client that runs git there. It holds no hook: a run's is its own,
// reached through the run's environment ([in]).
func boxClone(t *testing.T, g boxGit) (*gittest.Client, string) {
	t.Helper()
	client := gittest.NewClientWith(t, g.path)
	dir := client.Init(t)
	client.Commit(t, dir, map[string]string{"README": "hello\n"}, "the first commit")
	return client, dir
}

// installed is a hooks directory holding the hook run is handed, linked as
// prepare-commit-msg the way Start links it.
//
// WRITTEN BY A SHELL, never by this process, as Start has the box's own
// process put it in place: a file this process opened for writing is held
// open by any child another case forks meanwhile until that child execs, and
// git's exec of the hook would fail ETXTBSY.
func installed(t *testing.T, run boxRun) string {
	t.Helper()
	hook, err := executor.AttributionHook(run.attribution())
	if err != nil {
		t.Fatalf("AttributionHook: %v", err)
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", "printf '%s' "+sandbox.ShellQuote(string(hook))+
		" > hook && chmod 700 hook && mkdir hooks && ln -s ../hook hooks/prepare-commit-msg")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("installing the hook: %v\n%s", err, out)
	}
	return filepath.Join(dir, "hooks")
}

// pointedAt is client with its git pointed at hooks, as the run's wrapper
// points the run's: one GIT_CONFIG_COUNT entry naming the directory.
func pointedAt(client *gittest.Client, hooks string) *gittest.Client {
	return client.Env("GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.hooksPath", "GIT_CONFIG_VALUE_0="+hooks)
}

// in is client running as run: its git pointed at the run's own hook.
func in(t *testing.T, client *gittest.Client, run boxRun) *gittest.Client {
	t.Helper()
	return pointedAt(client, installed(t, run))
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

// A RUN'S COMMIT CARRIES ITS TRAILERS: a commit the run makes with its message
// given carries exactly the co-author and the three Crewlet trailers, naming
// the seat, the run and the work item — a merge and a revert included, a
// message with no final newline or a markdown rule in it, a sign-off before
// them, and `--no-verify`, which does not skip the hook.
//
// Mutations: drop the hook's final-newline line — the merge fails on 2.40.4;
// drop --no-divider — the `---` case fails; write no work-item trailer for a
// run on none — the last case fails.
func TestARunsCommitCarriesItsTrailers(t *testing.T) {
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

// A REWRITTEN COMMIT OF THE SEAT'S CARRIES ONE OF EACH CREWLET TRAILER, naming
// the run that made the rewrite — as git itself replaces the committer — and
// the co-author once, never a second set beside the one it carried: an amend
// in the same run and in another, a reword, a cherry-pick, a rebase replaying
// another run's commit, a squash of two runs' commits, `commit -C`, and the
// hook run twice over one message.
//
// Mutations: add the Crewlet trailers `--if-exists addIfDifferent` — the
// cherry-pick, the rebase and the other run's amend each carry two
// Crewlet-Run; no work-item trailer for a run on none — the other run's amend
// keeps this run's item; add the co-author `--if-exists add` — every rewrite
// carries it twice.
func TestARewrittenCommitCarriesOneOfEachTrailer(t *testing.T) {
	t.Parallel()
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)
		hooks := installed(t, thisRun)
		run, other := pointedAt(client, hooks), in(t, client, otherRun)

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
		hook := filepath.Join(hooks, "prepare-commit-msg")
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
func TestTheAttributionHookLeavesGitsMessageRulesAlone(t *testing.T) {
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
	hooks := installed(t, thisRun)
	file := filepath.Join(t.TempDir(), "COMMIT_EDITMSG")
	const message = "fix the parser\n\nwith a body\n"
	if err := os.WriteFile(file, []byte(message), 0o600); err != nil {
		t.Fatal(err)
	}
	stderr := runHook(t, boxGit{"failing", failing}, pointedAt(client, hooks), dir,
		filepath.Join(hooks, "prepare-commit-msg"), file)
	if got, _ := os.ReadFile(file); string(got) != message {
		t.Errorf("a failed interpret-trailers left the message as %q, want it untouched", got)
	}
	if want := "crewlet: this commit goes ahead without Crewlet's credit: " +
		"git interpret-trailers could not add it\n"; stderr != want {
		t.Errorf("the hook said %q, want %q", stderr, want)
	}
}

// NO TRAILER VALUE CAN INJECT ANYTHING: a value is quoted into the hook and
// never run as shell, and the seat and the work item are encoded to one line
// of a safe set, so none writes a line past its own trailer — a sign-off
// forged in a tracker's key included — and the key comes back whole from it.
// What is the engine's own bug is refused before the run starts: an item with
// no backend, an item with no seat, a run that is not one line.
//
// Mutations: write a value into the hook unquoted — the raw run creates files;
// pass the key unencoded — the forged sign-off is a fifth trailer; drop either
// refusal — red.
func TestNoTrailerValueCanInject(t *testing.T) {
	t.Parallel()
	safe := regexp.MustCompile(`^[A-Za-z0-9._~/#!@+%-]+$`)
	keys := []string{"line\nbreak", "carriage\rreturn", "nul\x00byte", "tab\there", "two words", "100%",
		"a:b", "über", "\nSigned-off-by: Evil <e@x>"}
	eachGit(t, func(t *testing.T, g boxGit) {
		client, dir := boxClone(t, g)

		// A RUN NAMED BY SHELL, written as it is, and nothing runs.
		raw := boxRun{seat: "swe", run: "`touch y` $(touch x) ;touch z \" ' $HOME *"}
		in(t, client, raw).Run(t, dir, "commit", "--allow-empty", "-m", "a raw run")
		mustTrail(t, client, dir, "HEAD", "a raw run", raw.want())
		for _, name := range []string{"x", "y", "z"} {
			if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a raw value ran a command: %s exists (%v)", name, err)
			}
		}

		// HOSTILE KEYS: one safe word each, back whole, and four trailers on
		// the commit whatever the key held.
		prefix := executor.WorkItemTrailer + ": jira "
		for _, key := range keys {
			run := boxRun{seat: "swe", run: thisRun.run,
				item: &types.WorkItem{Backend: types.WorkJira, ID: "1", Key: key}}
			in(t, client, run).Run(t, dir, "commit", "--allow-empty", "-m", "a hostile key")
			got := trailersOf(t, client, dir, "HEAD")
			if len(got) != 4 || !strings.HasPrefix(got[3], prefix) {
				t.Errorf("a commit on key %q carries %q, want exactly the four", key, got)
				continue
			}
			encoded := strings.TrimPrefix(got[3], prefix)
			if !safe.MatchString(encoded) {
				t.Errorf("for %q the value %q leaves the safe set", key, encoded)
			}
			if back, err := url.PathUnescape(encoded); err != nil || back != key {
				t.Errorf("%q came back from %q as %q (%v)", key, encoded, back, err)
			}
		}
	})

	for name, a := range map[string]sandbox.CommitAttribution{
		"an item with no backend": {Seat: "swe", Run: thisRun.run, WorkItem: &types.WorkItem{Key: "ENG-7"}},
		"an item with no seat":    {Run: thisRun.run, WorkItem: &engItem},
		"a run of two lines":      {Seat: "swe", Run: thisRun.run + "\nSigned-off-by: Evil <e@x>"},
	} {
		if _, err := executor.AttributionHook(a); err == nil {
			t.Errorf("the attribution took %s", name)
		}
	}
	hook, err := executor.AttributionHook(sandbox.CommitAttribution{Seat: "swe",
		WorkItem: &types.WorkItem{Backend: types.WorkWorklet}})
	if want := sandbox.ShellQuote(executor.WorkItemTrailer + ": native"); err != nil ||
		!strings.Contains(string(hook), "--trailer "+want+" ") {
		t.Errorf("an item with no key is not named by its backend alone (%v):\n%s", err, hook)
	}
}

// NOTHING BUT THE ATTRIBUTION SPELLS A TRAILER KEY. The trailers are a claim
// a box can forge, so no reader of a pushed commit's trailers may exist in the
// engine: no string of a Go file the binary is built from, outside
// attribution.go, holds a Crewlet trailer key in any case (git matches keys
// without it), and the keys' constants are unexported, so no other package
// can name them either. A struct tag is no reader — a config field's
// description names the trailers to the person who sets it — and a comment is
// none, so neither counts. The binary is what `go list -deps ./cmd/crewlet`
// names; a test-support package, which may forge them to show the host
// ignores them, is outside it. Every non-test Go file of a listed directory is
// read, whatever its build constraints.
//
// Mutation: a file of internal/gitlet/host reading `Crewlet-Run:` from a pushed
// commit — red.
func TestNothingButTheAttributionSpellsATrailerKey(t *testing.T) {
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
	// format's `,` or `)`, a quote, a space or the string's end — never by a
	// word character, `-` or `.`, so a file named crewlet-seat.md (an agent
	// definition's example in harnessprofile) is not one.
	key := regexp.MustCompile(`(?i)crewlet-(?:seat|run|work-item)(?:[^.\w-]|$)`)
	own := filepath.Join(root, "internal", "sandbox", "executor", "attribution.go")
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
			read++
			if !spellsKey(t, path, key) {
				continue
			}
			if path == own {
				inOwn = true
				continue
			}
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s spells a Crewlet trailer key: nothing but the attribution may, since a box can forge one", rel)
		}
	}
	if read == 0 {
		t.Fatal("the walk read no file of the binary")
	}
	if !inOwn {
		t.Fatal("the walk did not find the keys in internal/sandbox/executor/attribution.go: " +
			"it is reading the wrong files")
	}
}

// spellsKey reports whether a string literal of the Go file at path — a
// struct tag aside — holds key.
func spellsKey(t *testing.T, path string, key *regexp.Regexp) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	tags := map[*ast.BasicLit]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		if f, ok := n.(*ast.Field); ok && f.Tag != nil {
			tags[f.Tag] = true
		}
		return true
	})
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING || tags[lit] {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			value = lit.Value
		}
		if key.MatchString(value) {
			found = true
		}
		return !found
	})
	return found
}
