//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// newDirect mints a direct-mode provider rooted in a temp dir.
func newDirect(t *testing.T) *Local {
	t.Helper()
	local, err := NewLocal(LocalOptions{Placement: Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	return local
}

func mustCreate(t *testing.T, local *Local, spec Spec) Sandbox {
	t.Helper()
	box, err := local.Create(t.Context(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(context.Background()) })
	return box
}

// ---------------------------------------------------------------------
// construction
// ---------------------------------------------------------------------

func TestContainerModeRefusesToStartWithoutAnImage(t *testing.T) {
	t.Parallel()
	_, err := NewLocal(LocalOptions{Placement: Container, StateDir: t.TempDir()})
	if err == nil {
		t.Fatal("a container box with no image fails only once an agent tries to use it — refuse at config time")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Fatalf("error does not name the missing field: %v", err)
	}
}

// A HOST THAT CANNOT RUN THE CONTAINER CELL IS REFUSED WHEN ITS PROVIDER IS
// BUILT (NewLocal, when the config is applied), not by Create at the first
// box, which would fail minutes into a turn after the config was accepted.
// Every spelling of the runtime is refused, as a [LocalError] that says what
// to change. A direct cell on the same host still builds: it drives no runtime.
func TestContainerModeRefusesToStartWithoutARuntimeOnThePath(t *testing.T) {
	// Not parallel: PATH is how the backend finds its runtime, and this case
	// empties it, as TestTheRuntimeGetsWhatItsRuntimeEnvDeclares fills it.
	t.Setenv("PATH", t.TempDir())
	for _, runtime := range []string{"", "auto", "docker", "podman"} {
		_, err := NewLocal(LocalOptions{
			Placement: Container, Image: "img", Runtime: runtime, StateDir: t.TempDir(),
		})
		var refusal *LocalError
		if !errors.As(err, &refusal) {
			t.Errorf("runtime %q with no container CLI on PATH: NewLocal = %v, want a "+
				"*LocalError, since a cell built here fails only at its first executor",
				runtime, err)
			continue
		}
		// A runtime named outright is a field whose command is missing; "auto"
		// found neither (the line the docs' failure modes quote) and points to
		// the cell that needs no runtime.
		want := []string{"providers.sandbox.local.runtime is " + strconv.Quote(runtime),
			"not on the engine host's PATH"}
		if runtime == "" || runtime == "auto" {
			want = []string{"neither docker nor podman is on the engine host's PATH",
				fmt.Sprintf("use run_in %q", Direct)}
			// AND NOT THE RUNTIME FIELD: every value of it is looked up on this
			// same PATH and refused above, so offering it leads to a second
			// refusal.
			if strings.Contains(refusal.Error(), "providers.sandbox.local.runtime") {
				t.Errorf("runtime %q: the refusal offers providers.sandbox.local.runtime, "+
					"though no value of it builds this cell on this PATH: %v", runtime, refusal)
			}
		}
		for _, phrase := range want {
			if !strings.Contains(refusal.Error(), phrase) {
				t.Errorf("runtime %q: the refusal does not say %q: %v", runtime, phrase, refusal)
			}
		}
	}
	if _, err := NewLocal(LocalOptions{Placement: Direct, StateDir: t.TempDir()}); err != nil {
		t.Fatalf("a direct cell drives no runtime, and was refused on a host without one: %v", err)
	}
}

func TestAPlacementTheLocalBackendDoesNotServeIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := NewLocal(LocalOptions{Placement: "elsewhere", StateDir: t.TempDir()}); err == nil {
		t.Fatal("a placement the local backend does not serve was accepted")
	}
}

func TestTheBoxRootHonoursItsEnvironmentOverride(t *testing.T) {
	t.Setenv(BoxRootEnv, "/somewhere/else")
	if got := DefaultBoxRoot(); got != "/somewhere/else" {
		t.Fatalf("DefaultBoxRoot = %q, want the override", got)
	}
	t.Setenv(BoxRootEnv, "   ")
	if got := DefaultBoxRoot(); got == "" || strings.TrimSpace(got) != got {
		t.Fatalf("a blank override was taken literally: %q", got)
	}
}

// ---------------------------------------------------------------------
// the box on disk
// ---------------------------------------------------------------------

func TestACreatedBoxIsPrivateToTheEngineUser(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	info, err := os.Stat(box.Home())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("box mode = %v, want 0700 — it holds a credential on a host that runs other services", info.Mode().Perm())
	}
}

func TestEachBoxGetsItsOwnHome(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	first := mustCreate(t, local, Spec{})
	second := mustCreate(t, local, Spec{})
	if first.Home() == second.Home() {
		t.Fatal("two boxes share a home — every run would read its neighbour's done marker")
	}
	if first.ID() == second.ID() {
		t.Fatal("two boxes share an id")
	}
}

func TestExecRunsInTheBoxWorkspaceByDefault(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	result, err := box.Exec(t.Context(), "pwd", ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	want := filepath.Join(box.Home(), WorkspaceSubdir)
	if got := strings.TrimSpace(result.Stdout); got != want {
		// The workspace may itself be reached through a symlinked temp root.
		if resolved, rerr := filepath.EvalSymlinks(want); rerr != nil || got != resolved {
			t.Fatalf("Exec cwd = %q, want %q", got, want)
		}
	}
}

func TestExecReportsANonZeroExitRatherThanFailing(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	result, err := box.Exec(t.Context(), "exit 3", ExecOptions{})
	if err != nil {
		t.Fatalf("a command that exits non-zero is a result, not an error: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want 3", result.ExitCode)
	}
}

// ---------------------------------------------------------------------
// the child environment
// ---------------------------------------------------------------------

func TestTheChildSeesTheRunEnvAndTheBoxHomeButNotTheEngineSecrets(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-engines-own-key")
	local := newDirect(t)
	box := mustCreate(t, local, Spec{Env: map[string]string{"GITHUB_TOKEN": "declared-by-config"}})

	result, err := box.Exec(t.Context(), `echo "$HOME|$GITHUB_TOKEN|$ANTHROPIC_API_KEY|$TMPDIR"`, ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	parts := strings.Split(strings.TrimSpace(result.Stdout), "|")
	if len(parts) != 4 {
		t.Fatalf("unexpected output %q", result.Stdout)
	}
	if parts[0] != box.Home() {
		t.Fatalf("HOME = %q, want the box's own home %q", parts[0], box.Home())
	}
	if parts[1] != "declared-by-config" {
		t.Fatalf("the run env did not reach the child: %q", parts[1])
	}
	if parts[2] != "" {
		t.Fatal("the engine's own API key reached the harness")
	}
	if !strings.HasPrefix(parts[3], box.Home()) {
		t.Fatalf("TMPDIR = %q, want it inside the box", parts[3])
	}
}

// EVERY TEMP ROOT A HOST BOX'S PROCESSES READ LIES INSIDE THE BOX, one
// variable at a time (Claude Code reads CLAUDE_CODE_TMPDIR, not TMPDIR), and
// as the box's own directory rather than anywhere under its home: a temp root
// left at its default is a write outside the box.
func TestEveryTempRootOfAHostBoxIsItsOwnTmp(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	want := filepath.Join(box.Home(), ".tmp")
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Fatalf("the box's temporary directory %s does not exist: %v", want, err)
	}
	for _, name := range []string{"TMPDIR", "TMP", "TEMP", "CLAUDE_CODE_TMPDIR"} {
		if !slices.Contains(TempRootEnv(), name) {
			t.Errorf("%s is missing from TempRootEnv", name)
		}
	}
	for _, name := range TempRootEnv() {
		result, err := box.Exec(t.Context(), `printf %s "$`+name+`"`, ExecOptions{})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if result.Stdout != want {
			t.Errorf("%s = %q, want the box's own %q", name, result.Stdout, want)
		}
	}
}

func TestThePerCommandEnvOverridesTheRunEnv(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{Env: map[string]string{"PHASE": "run"}})
	result, err := box.Exec(t.Context(), "echo $PHASE", ExecOptions{Env: map[string]string{"PHASE": "setup"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "setup" {
		t.Fatalf("PHASE = %q, want the per-command value", got)
	}
}

// A JOB LAUNCHED INTO A KEPT BOX CARRIES ITS OWN LAUNCH'S ENVIRONMENT, never
// the one the box was made with: the box Connect hands back holds no
// environment of its own. So a credential replaced between the launches
// reaches the second job, and one the new sign-in no longer names (a token
// swapped for a key) is gone, rather than lingering where a harness that
// prefers it would sign in with a credential the vendor already refused.
//
// Mutation: have a reattached box keep the env it was created with, and the
// old token reaches the follow-up job.
func TestAFollowUpInAKeptBoxCarriesItsOwnLaunchsEnv(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	first := mustCreate(t, local, Spec{Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-old"}})
	kept, err := local.Connect(t.Context(), first.ID())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	result, err := kept.Exec(t.Context(), `echo "${CLAUDE_CODE_OAUTH_TOKEN-unset}:${ANTHROPIC_API_KEY-unset}"`,
		ExecOptions{Env: map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-new"}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(result.Stdout); got != "unset:sk-ant-api03-new" {
		t.Fatalf("the follow-up job saw %q, want only its own launch's credential (unset:sk-ant-api03-new)", got)
	}

	// THE CONTAINER CELL'S TOO: its env file is the follow-up job's alone.
	container := &Local{opts: LocalOptions{Placement: Container}, root: t.TempDir()}
	layout, err := container.layout("box-1")
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if err := os.MkdirAll(layout.root, 0o700); err != nil {
		t.Fatal(err)
	}
	box, err := container.reattach("box-1")
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	reattached := box.(*containerBox)
	if _, err := reattached.envArgs(map[string]string{"ANTHROPIC_API_KEY": "sk-ant-api03-new"}); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	blob, _ := os.ReadFile(reattached.layout.envFile())
	if strings.Contains(string(blob), "CLAUDE_CODE_OAUTH_TOKEN") || !strings.Contains(string(blob), "sk-ant-api03-new") {
		t.Fatalf("the reattached container's env file = %q, want the follow-up job's credential alone", blob)
	}
}

// ---------------------------------------------------------------------
// the escape guard
// ---------------------------------------------------------------------

func TestDirectModeRefusesToWriteOutsideTheBox(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	outside := filepath.Join(t.TempDir(), "host-file")

	for _, path := range []string{outside, "../escape", "/usr/local/bin/tool", box.Home() + "/../escape"} {
		if err := box.WriteFile(t.Context(), path, []byte("x")); err == nil {
			t.Fatalf("WriteFile(%q) succeeded — direct mode has no filesystem virtualisation, so this hits the host", path)
		}
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused write still touched the host")
	}
}

// A TILDE IS THE BOX'S HOME, in a direct box as in a container, as a shell
// expands it, so a setup file lands in one place whatever the cell; never a
// directory named "~". Another account's home is refused, and so is "~\x",
// whose backslash is part of a file name, never a separator.
func TestADirectBoxWritesATildePathInItsHome(t *testing.T) {
	t.Parallel()
	box := mustCreate(t, newDirect(t), Spec{})
	if err := box.WriteFile(t.Context(), "~/.npmrc", []byte("registry=x\n")); err != nil {
		t.Fatalf("WriteFile(~/.npmrc): %v", err)
	}
	got, err := os.ReadFile(filepath.Join(box.Home(), ".npmrc"))
	if err != nil || string(got) != "registry=x\n" {
		t.Fatalf("the home's .npmrc reads %q, %v; want the file written as ~/.npmrc", got, err)
	}
	if _, err := os.Stat(filepath.Join(box.Home(), "~")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a directory named ~ was made in the home: %v", err)
	}
	for _, path := range []string{"~", "~bob/.npmrc", `~\.npmrc`} {
		if err := box.WriteFile(t.Context(), path, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) succeeded; it names the home itself, another account's, "+
				"or no home at all", path)
		}
	}
}

// A LOCAL BOX'S REFUSAL NAMES A REMEDY THAT WORKS IN EVERY LOCAL CELL, never
// another cell, which refuses the same path (a container box writes only under
// its home mount). What works in both is the home: a file named relative to it
// (a direct box's home is minted per box, so no absolute path names it), or a
// setup command writing under "$HOME".
func TestALocalBoxsRefusalNamesARemedyThatWorks(t *testing.T) {
	t.Parallel()
	const system = "/usr/local/bin/tool"
	boxes := map[Placement]Sandbox{
		Direct:    mustCreate(t, newDirect(t), Spec{}),
		Container: &containerBox{layout: boxLayout{id: "box", root: t.TempDir()}, cli: containerCLI{path: "d"}, container: "c"},
	}
	for cell, box := range boxes {
		err := box.WriteFile(t.Context(), system, []byte("x"))
		if err == nil {
			t.Fatalf("a %s box wrote %s, outside its home", cell, system)
		}
		msg := err.Error()
		if !strings.Contains(msg, "relative to the home") {
			t.Errorf("the %s box's refusal does not say to name the file relative to its "+
				"home: %v", cell, err)
		}
		if !strings.Contains(msg, `"$HOME"`) || !strings.Contains(msg, "command") {
			t.Errorf("the %s box's refusal does not say to have a setup command write the "+
				"file under its home: %v", cell, err)
		}
		for other := range boxes {
			if other != cell && strings.Contains(msg, string(other)) {
				t.Errorf("the %s box's refusal sends the operator to %s, which refuses the "+
					"same path: %v", cell, other, err)
			}
		}
	}
}

func TestDirectModeAcceptsAnAbsolutePathInsideTheBox(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	// A brief and a setup step both speak in the home the box reports.
	path := filepath.Join(box.Home(), WorkspaceSubdir, "notes.md")
	if err := box.WriteFile(t.Context(), path, []byte("hello")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := box.ReadFile(t.Context(), path)
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
}

// The runner polls for a done marker that does not exist yet on every tick.
func TestReadingAMissingFileIsEmptyRatherThanAnError(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	got, err := box.ReadFile(t.Context(), filepath.Join(box.Home(), ".crewlet", "done"))
	if err != nil {
		t.Fatalf("polling for a marker that has not been written is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ReadFile = %q, want empty", got)
	}
}

func TestReadingOutsideTheBoxIsEmptyRatherThanALeak(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("host secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, _ := box.ReadFile(t.Context(), secret)
	if len(got) != 0 {
		t.Fatalf("a read escaped the box and returned %q", got)
	}
}

// ---------------------------------------------------------------------
// the detached job
// ---------------------------------------------------------------------

func TestABackgroundJobOutlivesTheCallAndRecordsItsPid(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	marker := filepath.Join(box.Home(), WorkspaceSubdir, "done")

	pid, err := box.StartBackground(t.Context(), "sleep 0.2; echo finished > done", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if _, err := strconv.Atoi(pid); err != nil {
		t.Fatalf("StartBackground returned %q, want a pid", pid)
	}
	if leader := recordedLeader(t, box); strconv.Itoa(leader.PID) != pid {
		t.Fatalf("recorded group %d, want %s: without it no later process can reach the job", leader.PID, pid)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(marker)
		return err == nil
	}, "the background job never finished")
}

// Go reaps a child only when something calls Wait, and an unreaped process is
// a zombie that kill(0) reports as ALIVE — so the completion probe would hang
// on a job that already exited. This is the test for the Wait goroutine.
func TestAFinishedBackgroundJobStopsAnsweringTheLivenessProbe(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})

	pid, err := box.StartBackground(t.Context(), "exit 0", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	n, _ := strconv.Atoi(pid)
	waitFor(t, 5*time.Second, func() bool { return !procgroup.Exists(n) },
		"a finished job still answers the liveness probe — it was left a zombie")
}

// One killpg must reach the agent's children, not just the shell it started.
func TestClosingABoxKillsTheWholeProcessTree(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	childPidFile := filepath.Join(box.Home(), WorkspaceSubdir, "child.pid")
	if _, err := box.StartBackground(t.Context(),
		"sh -c 'echo $$ > child.pid; sleep 300' & sleep 300", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	var child int
	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(childPidFile)
		if err != nil {
			return false
		}
		child, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && child > 0
	}, "the grandchild never recorded its pid")

	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !procgrouptest.AwaitGone(t, child, 10*time.Second) {
		t.Fatal("the harness's own child survived teardown")
	}
}

func TestClosingABoxRemovesItsDirectory(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the box directory survived teardown: %v", err)
	}
}

// ---------------------------------------------------------------------
// pause / resume / kill
// ---------------------------------------------------------------------

// groupStates is the state ps reports for every process of group pgid, by
// pid: T for a stopped one and Z for one that has exited unreaped, on both
// release platforms.
func groupStates(t *testing.T, pgid int) map[int]string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "ps", "-A", "-o", "pid=,pgid=,stat=").Output()
	if err != nil {
		t.Fatalf("reading the processes' states through ps: %v", err)
	}
	states := map[int]string{}
	for line := range strings.Lines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		group, groupErr := strconv.Atoi(fields[1])
		if pidErr == nil && groupErr == nil && group == pgid {
			states[pid] = fields[2]
		}
	}
	return states
}

// stoppedTree reports whether the job whose group leader is leader, its
// group's states as [groupStates] reads them, runs nothing and can run nothing
// until something continues it: its leader stopped, and every other process of
// it stopped or exited.
func stoppedTree(states map[int]string, leader int) bool {
	if !strings.HasPrefix(states[leader], "T") {
		return false
	}
	for _, state := range states {
		if !strings.HasPrefix(state, "T") && !strings.HasPrefix(state, "Z") {
			return false
		}
	}
	return true
}

// awaitStopped waits for a paused job's tree to stop ([stoppedTree]), failing
// the case with what if it never does.
//
// WAITED FOR, NEVER GIVEN A HEAD START: a SIGSTOP takes effect once each
// process next runs, which a loaded runner can delay past any nap. A tree read
// stopped has no write in flight and does no work until continued, which no
// quiet window can say.
//
// ITS JOB FORKS NOTHING, running builtins alone: a stop landing while a shell
// waits in vfork for its child to exec leaves the shell in D, not T,
// indistinguishable from a shell blocked in a write.
func awaitStopped(t *testing.T, leader int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		states := groupStates(t, leader)
		if stoppedTree(states, leader) {
			return
		}
		//fixedwait:measure the poll's own deadline: it only ends a wait for a stop that must come, failing the case
		if time.Now().After(deadline) {
			t.Fatalf("%s: the group %d leads reads %v", what, leader, states)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// PAUSE STOPS THE TREE, read off the state the kernel keeps for each of its
// processes ([awaitStopped]), and Connect resumes it, which its work shows.
//
// Mutation: pause nothing, and the tree never stops; resume nothing on
// Connect, and the job never works again.
func TestPauseStopsTheTreeAndConnectResumesIt(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	counter := filepath.Join(box.Home(), WorkspaceSubdir, "ticks")

	// Builtins alone, so it forks nothing a stop could find it waiting on
	// ([awaitStopped]).
	if _, err := box.StartBackground(t.Context(),
		"while true; do echo x >> ticks; done", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > 0 }, "the job never started ticking")
	leader := recordedLeader(t, box).PID

	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	awaitStopped(t, leader, "a paused box kept working: its job's tree never stopped")
	stopped := fileSize(counter)

	resumed, err := local.Connect(t.Context(), box.ID())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { resumed.Close(context.Background()) })
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > stopped },
		"Connect did not resume the paused box")
}

// A READER NEVER WAKES A BOX. Attach hands a stopped direct box back STOPPED
// (its files and its probe are the host's, so neither needs the job to run),
// where Connect would SIGCONT it.
//
// READ AT ONCE, NOT OVER A WINDOW: a SIGCONT wakes the group inside the
// kill(2) that sends it, so a tree Attach continued no longer reads stopped
// when it returns. The pause before is waited for ([awaitStopped]).
//
// Mutation: resume in Attach, and the paused tree runs again.
func TestAttachingToAPausedBoxLeavesItPaused(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	counter := filepath.Join(box.Home(), WorkspaceSubdir, "ticks")
	// Builtins alone, as [awaitStopped] needs.
	handle, err := box.StartBackground(t.Context(),
		"while true; do echo x >> ticks; done", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > 0 }, "the job never started ticking")
	leader := recordedLeader(t, box).PID
	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	awaitStopped(t, leader, "the paused job's tree never stopped")

	attached, err := local.Attach(t.Context(), box.ID())
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	if state, err := attached.Job(t.Context(), handle); err != nil || state != JobRunning {
		t.Fatalf("the stopped job reads as %v, %v; want it running and readable", state, err)
	}
	if states := groupStates(t, leader); !stoppedTree(states, leader) {
		t.Fatalf("attaching resumed the paused box: its job's tree reads %v", states)
	}
}

// A PAUSED CONTAINER IS REFUSED, NEVER UNPAUSED. Its liveness probe is an exec
// a paused container refuses, so Attach reads the runtime's own state rather
// than unpausing to find out. The fake runtime logs every call and answers
// `inspect` from a file the test writes.
//
// Mutation: unpause in Attach, and the log shows it.
func TestAttachingToAPausedContainerRefusesItAndLeavesItPaused(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	calls, state := filepath.Join(dir, "calls"), filepath.Join(dir, "paused")
	runtime := filepath.Join(dir, "runtime")
	script := "#!/bin/sh\necho \"$@\" >> " + calls + "\n" +
		"if [ \"$1\" = inspect ]; then read s < " + state + "; echo \"$s\"; fi\n"
	writeExecutable(t, runtime, script)
	local := &Local{opts: LocalOptions{Placement: Container}, root: filepath.Join(dir, "boxes"), cli: containerCLI{path: runtime}}
	layout, err := local.layout("box1")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(layout.meta(), 0o700); err != nil {
		t.Fatal(err)
	}
	logged := func() string {
		raw, _ := os.ReadFile(calls)
		return string(raw)
	}

	if err := os.WriteFile(state, []byte("true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Attach(t.Context(), "box1"); !errors.Is(err, ErrBoxPaused) {
		t.Fatalf("attaching to a paused container = %v; want ErrBoxPaused", err)
	}
	if err := os.WriteFile(state, []byte("false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Attach(t.Context(), "box1"); err != nil {
		t.Fatalf("attaching to a running container: %v", err)
	}
	if strings.Contains(logged(), "unpause") {
		t.Fatalf("an attach unpaused the container: %q", logged())
	}
	if !strings.Contains(logged(), "inspect --format {{.State.Paused}} "+ContainerPrefix+"box1") {
		t.Fatalf("the runtime was never asked whether the container is paused: %q", logged())
	}
	// The log is real: Connect's unpause shows up in it.
	if _, err := local.Connect(t.Context(), "box1"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !strings.Contains(logged(), "unpause "+ContainerPrefix+"box1") {
		t.Fatalf("Connect's unpause is not in the log, so the log proves nothing: %q", logged())
	}
}

// Teardown of a PAUSED box is the case SIGCONT-first exists for: a stopped
// process never runs again to handle SIGTERM. The job traps SIGTERM, as a
// harness cleaning up does, and leaves a witness outside the box; the group
// being gone proves nothing, since the SIGKILL after the grace ends it anyway.
//
// Its job forks nothing ([awaitStopped]) and reads stopped before the
// teardown. It is paused only once a ready file (a builtin write after the
// trap) says its trap is set: paused before, SIGTERM is still at its default.
//
// Mutation: tear the box down without the SIGCONT, or without the SIGTERM,
// and the SIGKILL that follows the grace ends the job before its trap runs.
func TestAPausedBoxCanStillBeTornDown(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	dir := t.TempDir()
	witness, ready := filepath.Join(dir, "term"), filepath.Join(dir, "ready")
	if _, err := box.StartBackground(t.Context(),
		"trap 'echo term > "+witness+"; exit 0' TERM; : > "+ready+"; while true; do :; done", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	pid := recordedLeader(t, box).PID
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(ready)
		return err == nil
	}, "the job never set its SIGTERM trap")
	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	awaitStopped(t, pid, "the box was not actually paused: its job's tree never stopped")
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return !procgroup.Exists(pid) },
		"a paused box survived teardown")
	if got, err := os.ReadFile(witness); err != nil || string(got) != "term\n" {
		t.Fatalf("the paused job never handled the teardown's SIGTERM (its witness reads %q, %v)", got, err)
	}
}

// Connect resumes, so the pause reaper uses Kill, which must not: SIGKILL
// reaches a stopped process directly, while Close's SIGCONT-first teardown
// would let a reaped clarification pause run again on its way out.
//
// The tick file lives OUTSIDE the box, which Kill deletes. The job also ticks
// on SIGTERM, as a harness cleaning up does, so a Kill through the teardown's
// continue, terminate and grace shows in the counter; a SIGCONT that a
// SIGKILL overtakes runs nothing.
//
// Mutation: kill through the teardown's kill, continuing first, and the paused
// job works on its way out.
func TestKillReclaimsAPausedBoxWithoutResumingIt(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	counter := filepath.Join(t.TempDir(), "ticks")
	// No sleep in the loop: any scheduling at all after a SIGCONT shows up.
	if _, err := box.StartBackground(t.Context(),
		"trap 'echo term >> "+counter+"' TERM; while true; do echo x >> "+counter+"; done",
		ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > 0 }, "the job never started")
	pid := recordedLeader(t, box).PID

	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// THE PREMISE ([awaitStopped]): the whole tree stopped, so the reading
	// below is the paused job's last.
	awaitStopped(t, pid, "the box was not actually paused: its job's tree never stopped")
	before := fileSize(counter)

	if err := local.Kill(t.Context(), box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return !procgroup.Exists(pid) }, "Kill left the tree running")
	if after := fileSize(counter); after != before {
		t.Fatalf("Kill resumed the box before reclaiming it: %d bytes of work ran after the pause", after-before)
	}
	if _, err := os.Stat(box.Home()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Kill left the box directory behind")
	}
}

func TestKillingAVanishedBoxIsNotAnError(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	if err := local.Kill(t.Context(), "never-existed"); err != nil {
		t.Fatalf("Kill of a box that is already gone: %v", err)
	}
}

func TestConnectingToAVanishedBoxSaysSo(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	_, err := local.Connect(t.Context(), "never-existed")
	if err == nil {
		t.Fatal("Connect to a box that does not exist succeeded")
	}
	if !strings.Contains(err.Error(), "reaped") {
		t.Fatalf("the error does not explain what happened: %v", err)
	}
}

// ---------------------------------------------------------------------
// credentials
// ---------------------------------------------------------------------

func TestTheLoginIsSeededInAndARefreshedOneIsWrittenBack(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	files := map[string]string{".claude/.credentials.json": shared}

	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{CredentialFiles: files, CredentialRules: testRules(files)})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seeded := filepath.Join(box.Home(), ".claude", ".credentials.json")
	if got, err := os.ReadFile(seeded); err != nil || string(got) != `{"token":"first"}` {
		t.Fatalf("the login was not seeded into the box: %q, %v", got, err)
	}
	// The harness rotates its token mid-run.
	if err := os.WriteFile(seeded, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, err := os.ReadFile(shared); err != nil || string(got) != `{"token":"rotated"}` {
		t.Fatalf("shared login = %q, %v; a discarded rotation logs the whole fleet out at the next expiry", got, err)
	}
}

// Every production teardown goes through Connect or Kill, never the object
// Create returned — so the map has to be rebuilt from the box's own record.
func TestAReconnectedBoxStillWritesARefreshedLoginBack(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seeded := filepath.Join(box.Home(), ".claude", ".credentials.json")
	if err = os.WriteFile(seeded, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A fresh engine, holding nothing but the id.
	fresh, err := NewLocal(LocalOptions{Placement: Direct, StateDir: local.root})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	reconnected, err := fresh.Connect(t.Context(), box.ID())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := reconnected.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got, _ := os.ReadFile(shared); string(got) != `{"token":"rotated"}` {
		t.Fatalf("shared login = %q — a reconnected teardown discarded the rotation", got)
	}
}

// A torn-down box must never CREATE a login the operator has since removed —
// not even one its run refreshed.
func TestARemovedLoginIsNotRecreatedByATeardown(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	writeFile(t, shared, `{"token":"first"}`)
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, filepath.Join(box.Home(), ".claude", ".credentials.json"), `{"token":"rotated"}`)
	if err := os.Remove(shared); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(shared); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("teardown recreated a login the operator had removed")
	}
}

// Nor one the shared directory did not hold when the box was seeded: a login
// that appeared since is somebody else's, and whatever the box holds at that
// path is no refresh of it.
func TestALoginTheBoxWasNotSeededWithIsNeverWrittenBack(t *testing.T) {
	t.Parallel()
	shared := filepath.Join(t.TempDir(), "credentials.json")
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, shared, `{"token":"an operator's new login"}`)
	writeFile(t, filepath.Join(box.Home(), ".claude", ".credentials.json"), `{"token":"the box's own"}`)
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readFile(t, shared); got != `{"token":"an operator's new login"}` {
		t.Fatalf("shared login = %q; a box wrote over a login it was never given", got)
	}
}

// AN IDLE BOX NEVER WRITES ITS SEED BACK. A run that refreshed nothing holds
// exactly its seed (an OpenCode box always does), while the host may have
// refreshed the shared login meanwhile, rotating the seed's refresh token
// away: written back, the seed logs every seat out at the next expiry. Every
// teardown path is held to it: Close, Kill by id and the orphan reaper.
//
// Mutation: compare the box's copy with the shared login as it is now, and
// the host's refresh is reverted.
func TestAnIdleBoxNeverWritesItsSeedBackOverALoginTheHostRefreshed(t *testing.T) {
	t.Parallel()
	for path, teardown := range map[string]func(t *testing.T, local *Local, box Sandbox){
		"Close": func(t *testing.T, _ *Local, box Sandbox) {
			if err := box.Close(t.Context()); err != nil {
				t.Fatalf("Close: %v", err)
			}
		},
		"Kill": func(t *testing.T, local *Local, box Sandbox) {
			if err := local.Kill(t.Context(), box.ID()); err != nil {
				t.Fatalf("Kill: %v", err)
			}
		},
		"the orphan reaper": func(t *testing.T, local *Local, box Sandbox) {
			ageBox(t, box, 2*time.Hour)
			local.reapOrphans(t.Context(), time.Minute)
		},
	} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			shared := filepath.Join(t.TempDir(), "credentials.json")
			writeFile(t, shared, `{"refresh":"v1"}`)
			local := newDirect(t)
			box, err := local.Create(t.Context(), Spec{
				CredentialFiles: map[string]string{".claude/.credentials.json": shared},
				CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			// The host refreshes while the box runs; the box does not.
			writeFile(t, shared, `{"refresh":"v2"}`)
			teardown(t, local, box)
			if got := readFile(t, shared); got != `{"refresh":"v2"}` {
				t.Fatalf("shared login = %q; an idle box put its seed back over the host's refresh", got)
			}
		})
	}
}

// A REFRESH IS WRITTEN BACK ONLY OVER THE LOGIN IT DESCENDS FROM. When the
// host also refreshed, the box's copy is the older, so the shared login is
// kept and the dropped refresh is said, by path and never by value. A refresh
// of a login the operator removed meanwhile writes nothing, said in its own
// words: there is no shared login to have kept.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutations: write back whenever the box's copy changed, and the host's
// refresh is replaced; say both outcomes in one sentence, and the removed
// login's warning claims a login was kept.
func TestARefreshIsDroppedWhereTheSharedLoginMovedPastItsSeed(t *testing.T) {
	logs := captureLogs(t)
	shared := filepath.Join(t.TempDir(), "credentials.json")
	writeFile(t, shared, `{"refresh":"v1"}`)
	local := newDirect(t)

	// An idle box drops nothing, so says nothing: it refreshed no login, and
	// a warning for it would bury the one that matters.
	idle, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, shared, `{"refresh":"v1-host"}`)
	if err := local.Kill(t.Context(), idle.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if out := logs.String(); strings.Contains(out, "local_sandbox_credential_superseded") {
		t.Errorf("an idle box reported a refresh it never made:\n%s", out)
	}

	writeFile(t, shared, `{"refresh":"v1"}`)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, filepath.Join(box.Home(), ".claude", ".credentials.json"), `{"refresh":"box-refreshed"}`)
	writeFile(t, shared, `{"refresh":"host-refreshed"}`)
	if err := local.Kill(t.Context(), box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if got := readFile(t, shared); got != `{"refresh":"host-refreshed"}` {
		t.Fatalf("shared login = %q; want the host's refresh kept", got)
	}
	out := logs.String()
	if !strings.Contains(out, "local_sandbox_credential_superseded") ||
		!strings.Contains(out, shared) || !strings.Contains(out, "shared=changed") ||
		!strings.Contains(out, "the shared one changed after the box was seeded") ||
		!strings.Contains(out, "the shared one was kept") {
		t.Errorf("the dropped refresh was not said, naming the path and the newer login kept:\n%s", out)
	}
	if strings.Contains(out, "refreshed\"}") || strings.Contains(out, "box-refreshed") {
		t.Errorf("the log carries a login's value:\n%s", out)
	}

	// The operator removes the login while a box refreshes it.
	writeFile(t, shared, `{"refresh":"v1"}`)
	box, err = local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	writeFile(t, filepath.Join(box.Home(), ".claude", ".credentials.json"), `{"refresh":"box-refreshed"}`)
	if err := os.Remove(shared); err != nil {
		t.Fatal(err)
	}
	before := len(logs.String())
	if err := local.Kill(t.Context(), box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := os.Stat(shared); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a teardown recreated a login the operator removed")
	}
	removed := logs.String()[before:]
	if !strings.Contains(removed, "local_sandbox_credential_superseded") ||
		!strings.Contains(removed, "shared=removed") ||
		!strings.Contains(removed, "the shared one was removed after the box was seeded") ||
		!strings.Contains(removed, "a box never creates one") {
		t.Errorf("the refresh dropped over a removed login was not said in its own words:\n%s", removed)
	}
	if strings.Contains(removed, "was kept") {
		t.Errorf("the removed login's warning claims a login was kept:\n%s", removed)
	}
}

// THE COMPARISON AND THE WRITE ARE ONE STEP against every other writer of the
// shared directory: a write-back takes the directory's lock, as the harness
// backend's seats and `crewlet llm` commands do, rather than comparing against
// a login being replaced.
//
// READ ONCE THE TEARDOWN IS BLOCKED, in a bubble: Wait returns once every
// goroutine blocks, and the teardown's first wait is for the lock the case
// holds, so a write that skips the lock has landed by then, however late.
//
// Mutation: compare and write without the lock, and the write lands while
// another writer holds it.
func TestAWriteBackWaitsForTheSharedLoginsLock(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := t.TempDir()
		shared := filepath.Join(store, "credentials.json")
		writeFile(t, shared, `{"token":"first"}`)
		local := newDirect(t)
		box, err := local.Create(t.Context(), Spec{
			CredentialFiles: map[string]string{".claude/.credentials.json": shared},
			CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		writeFile(t, filepath.Join(box.Home(), ".claude", ".credentials.json"), `{"token":"rotated"}`)

		release, err := hostbox.LockCredentials(t.Context(), store)
		if err != nil {
			t.Fatalf("taking the shared login's lock: %v", err)
		}
		closed := make(chan error, 1)
		go func() { closed <- box.Close(t.Context()) }()
		synctest.Wait()
		if got := readFile(t, shared); got != `{"token":"first"}` {
			release()
			t.Fatalf("shared login = %q while another writer held its lock", got)
		}
		release()
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
		if got := readFile(t, shared); got != `{"token":"rotated"}` {
			t.Fatalf("shared login = %q; want the refresh written once the lock was free", got)
		}
	})
}

// The keys are operator-overridable config.
func TestASeededCredentialCannotEscapeTheBox(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	source := filepath.Join(store, "payload")
	if err := os.WriteFile(source, []byte("owned"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	victim := filepath.Join(t.TempDir(), "cron.d", "x")
	local := newDirect(t)
	box := mustCreate(t, local, Spec{CredentialFiles: map[string]string{
		"../../" + strings.TrimPrefix(victim, "/"): source,
	}})
	_ = box
	if _, err := os.Stat(victim); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a credential_paths key wrote a host file")
	}
}

// ---------------------------------------------------------------------
// the orphan reaper
// ---------------------------------------------------------------------

// Age alone does not mean orphaned: reaping a run that outlasted the TTL
// leaves an unkillable job writing into a deleted directory.
func TestTheReaperLeavesABoxWithALiveJobAlone(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	if _, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	ageBox(t, box, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("the reaper deleted a box whose job is still running: %v", err)
	}
}

func TestTheReaperRemovesAnAbandonedBox(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	ageBox(t, box, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an abandoned box survived the reaper: %v", err)
	}
}

// The keepalive is the "has it been abandoned?" half, and reading the
// directory's mtime instead would read a constant: a directory's mtime does
// not move when files are written inside its subdirectories.
func TestTheKeepaliveStampIsWhatSparesABoxTheReaper(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	ageBox(t, box, 2*time.Hour)

	if err := box.Touch(t.Context()); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("the reaper deleted a box the waiter had just heart-beaten: %v", err)
	}
}

// strangerGroup is a live process group that no box started: what a recycled
// pid names from a box's point of view ([startStranger]).
type strangerGroup struct {
	procgroup.Leader
	// heard is the file its trap writes when it is sent SIGCONT.
	heard string
}

// startStranger starts a stranger that keeps a record of what reaches it, for
// [assertUntouched] to read: a trap writes its heard file when it is sent
// SIGCONT, a SIGTERM or a SIGKILL ends it, and a SIGSTOP stops it; sent
// SIGWINCH, it writes heard's witness. It says it is ready once its traps are
// set, which this waits for, since a SIGCONT before them would leave no trace.
// It is killed when the test ends, so a failure leaks nothing.
func startStranger(t *testing.T) strangerGroup {
	t.Helper()
	heard := filepath.Join(t.TempDir(), "heard")
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c",
		`trap ': > "$1"' CONT; trap ': > "$1.witness"' WINCH; : > "$1.ready"; while :; do sleep 0.05; done`,
		"sh", heard)
	procgroup.Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stranger: %v", err)
	}
	t.Cleanup(func() {
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	leader, err := procgroup.Identify(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(heard + ".ready")
		return err == nil
	}, "the stranger never set its traps")
	return strangerGroup{Leader: leader, heard: heard}
}

// recordAsJob writes leader into a box's job record, as StartBackground does.
func recordAsJob(t *testing.T, box Sandbox, leader procgroup.Leader) {
	t.Helper()
	if err := recordLeader(layoutOf(t, box), leader); err != nil {
		t.Fatalf("recording the job: %v", err)
	}
}

// recycled is the stranger's pid under a start time that is not its own: the
// record a box would hold if its job had ended and the kernel had handed the
// pid to the stranger.
func recycled(stranger strangerGroup) procgroup.Leader {
	return procgroup.Leader{PID: stranger.PID, Start: stranger.Start + "0"}
}

// recordedLeader is the job record StartBackground wrote into a box.
func recordedLeader(t *testing.T, box Sandbox) procgroup.Leader {
	t.Helper()
	raw, err := os.ReadFile(layoutOf(t, box).pidFile())
	if err != nil {
		t.Fatalf("no job record: %v", err)
	}
	leader, err := procgroup.ParseLeader(string(raw))
	if err != nil {
		t.Fatalf("the job record %q is not a process group identity: %v", raw, err)
	}
	return leader
}

// A recycled pid pointing at some unrelated long-lived process would otherwise
// keep a dead box's directory alive forever.
//
// The stranger is a real, live process group: a pid that answers a liveness
// probe and is not the job. The reaper decides on the start time alone.
func TestARecycledPidDoesNotKeepADeadBoxAlive(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	stranger := startStranger(t)
	recordAsJob(t, box, recycled(stranger))
	ageBox(t, box, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a recycled pid kept a dead box's directory alive forever")
	}
	assertUntouched(t, stranger, "after the reap")
}

// The same record must not steer a signal either. A teardown, a kill and a
// pause all act on the group a box recorded, and a group that is no longer the
// job belongs to somebody else: SIGKILL there takes down a stranger's tree,
// SIGSTOP freezes it for as long as the pause lasts, and SIGCONT wakes one its
// owner stopped.
//
// Mutation: send any of them through a record whose start time is not the
// stranger's, and the stranger's own record of it fails the case.
func TestARecycledPidIsNeverSignalled(t *testing.T) {
	t.Parallel()
	stranger := startStranger(t)
	local := newDirect(t)
	for name, act := range map[string]func(Sandbox) error{
		"pause": func(box Sandbox) error { return box.Pause(t.Context()) },
		"close": func(box Sandbox) error { return box.Close(t.Context()) },
		"kill":  func(box Sandbox) error { return local.Kill(t.Context(), box.ID()) },
		"connect": func(box Sandbox) error {
			_, err := local.Connect(t.Context(), box.ID())
			return err
		},
	} {
		box, err := local.Create(t.Context(), Spec{})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		recordAsJob(t, box, recycled(stranger))
		if err := act(box); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		assertUntouched(t, stranger, "after "+name)
	}
}

// assertUntouched fails the test if anything a box sends reached the
// stranger's group before now: it sends SIGWINCH and waits for the witness, or
// for what an earlier signal did. THE WITNESS COMES AFTER EVERY SIGNAL THE
// CASE COULD HAVE SENT, so no quiet window passes a signal acted on late.
//
// A SIGKILL or SIGTERM leaves a zombie (this test's unreaped child) that never
// writes it; a SIGSTOP reads T in ps, the shell's or its sleep's
// ([awaitStopped]). Both are read off the kernel's record and ps, since
// [procgroup.Leader.Current] calls either current. A SIGCONT is trapped, and
// the shell runs caught traps in signal-number order, SIGCONT's before
// SIGWINCH's on both release platforms, so heard precedes the witness.
func assertUntouched(t *testing.T, stranger strangerGroup, when string) {
	t.Helper()
	witness := stranger.heard + ".witness"
	if err := os.Remove(witness); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("clearing the stranger's last witness: %v", err)
	}
	if err := syscall.Kill(stranger.PID, syscall.SIGWINCH); err != nil {
		t.Fatalf("%s sending the stranger its witness signal: %v", when, err)
	}
	waitFor(t, 10*time.Second, func() bool {
		if _, err := os.Stat(witness); err == nil {
			return true
		}
		proc, found, err := procgroup.Inspect(stranger.PID)
		if err != nil || !found || proc.Zombie || proc.Start != stranger.Start {
			t.Fatalf("%s the stranger is gone (record %+v, found %v, %v): a recycled pid was signalled",
				when, proc, found, err)
		}
		for pid, state := range groupStates(t, stranger.PID) {
			if strings.HasPrefix(state, "T") {
				t.Fatalf("%s the stranger's group is stopped (pid %d reads %s): a recycled pid was signalled",
					when, pid, state)
			}
		}
		return false
	}, when+" the stranger never wrote its witness")
	if _, err := os.Stat(stranger.heard); err == nil {
		t.Fatalf("%s the stranger was sent SIGCONT: a recycled pid was signalled", when)
	}
}

// THE COMPLETION POLL'S LIVENESS QUESTION, answered by the job's identity
// rather than its host pid, which the kernel may hand to anything once the
// wrapper is gone: a wrapper that died without its marker would hold its run
// open until that stranger exited.
func TestTheJobProbeFollowsTheJobAndNotItsPid(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})

	handle, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if state, err := box.Job(t.Context(), handle); err != nil || state != JobRunning {
		t.Fatalf("Job of a live job = %v, %v; want running", state, err)
	}

	// The job's pid now names a stranger: a live process that is not the job.
	stranger := startStranger(t)
	recordAsJob(t, box, recycled(stranger))
	if state, err := box.Job(t.Context(), strconv.Itoa(stranger.PID)); err != nil || state != JobEnded {
		t.Fatalf("Job of a recycled pid = %v, %v; want ended: a stranger held the run open", state, err)
	}
	// And a handle the record does not name is a job this box has replaced.
	if state, err := box.Job(t.Context(), handle); err != nil || state != JobEnded {
		t.Fatalf("Job of a handle the record no longer names = %v, %v; want ended", state, err)
	}
	if _, err := box.Job(t.Context(), "not-a-pid"); err == nil {
		t.Fatal("a handle that is not a pid was answered rather than refused")
	}
}

// An exited wrapper is not running, without waiting for the reap. It ENDED,
// not lost: the engine that started it is still here, so what it left is its
// result.
func TestAFinishedJobIsNotRunning(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	handle, err := box.StartBackground(t.Context(), "exit 0", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	var state JobState
	waitFor(t, 5*time.Second, func() bool {
		state, err = box.Job(t.Context(), handle)
		return err == nil && state != JobRunning
	}, "a finished job still reads as running")
	if state != JobEnded {
		t.Fatalf("a job that died while its engine ran reads as %v; want ended", state)
	}
}

// A job record that exists and cannot be read is the unknown answer, not an
// absent one. Deleting a live box's checkout is unrecoverable and a lingering
// directory is not, so the reaper keeps a box whose record it cannot read.
func TestAnUnreadableJobRecordKeepsTheBox(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	// A directory where the record belongs reads as an error for any user,
	// root included, which a permission bit would not.
	if err := os.MkdirAll(layoutOf(t, box).pidFile(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ageBox(t, box, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(home); err != nil {
		t.Fatalf("the reaper deleted a box whose job record it could not read: %v", err)
	}
}

// A record that is not a process group identity names no job (a direct box's
// job runs as the engine's user beside its records): a bare pid has no start
// time, so acting on it would reach whatever holds that pid now. It is neither
// signalled nor allowed to keep the box.
func TestAJobRecordWithoutAStartTimeNamesNoJob(t *testing.T) {
	t.Parallel()
	stranger := startStranger(t)
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	if err := os.WriteFile(layoutOf(t, box).pidFile(),
		[]byte(strconv.Itoa(stranger.PID)), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := local.Kill(t.Context(), box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	assertUntouched(t, stranger, "after a kill steered by a bare pid")
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Kill left the box directory behind")
	}
}

func TestTheReaperCollectsCredentialsBeforeDeleting(t *testing.T) {
	t.Parallel()
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
		CredentialRules: testRules(map[string]string{".claude/.credentials.json": ""}),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seeded := filepath.Join(box.Home(), ".claude", ".credentials.json")
	if err := os.WriteFile(seeded, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ageBox(t, box, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if got, _ := os.ReadFile(shared); string(got) != `{"token":"rotated"}` {
		t.Fatalf("shared login = %q — the reaper threw away a rotated credential", got)
	}
}

// A tiny or zero grace must not make every box on the host instantly
// reapable, including one another engine just created.
func TestTheReaperFloorsItsCutoffWhateverItsGrace(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	local.reapOrphans(t.Context(), 0)
	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("a zero grace reaped a box that was just created: %v", err)
	}
}

// CREATE REAPS BY THE BACKEND'S OWN GRACE ([LocalOptions.HostGrace]), one
// value for every box on its root: an orphan a previous engine left is taken
// once its stamp is older than the grace, and left while it is younger,
// whichever box's Create runs the sweep.
//
// Mutation: reap by a fixed cutoff, or by nothing the options set, and one
// case fails.
func TestCreateReapsOrphansLeftByAPreviousEngine(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		grace  time.Duration
		reaped bool
	}{
		{"older than the grace", time.Hour, true},
		{"within the grace", 3 * time.Hour, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			local, err := NewLocal(LocalOptions{Placement: Direct, StateDir: t.TempDir(), HostGrace: c.grace})
			if err != nil {
				t.Fatalf("NewLocal: %v", err)
			}
			orphan, err := local.Create(t.Context(), Spec{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			ageBox(t, orphan, 2*time.Hour)

			fresh := mustCreate(t, local, Spec{})
			if fresh.ID() == orphan.ID() {
				t.Fatal("Create reused an id")
			}
			_, err = os.Stat(orphan.Home())
			if reaped := errors.Is(err, os.ErrNotExist); reaped != c.reaped {
				t.Fatalf("an orphan two hours untouched under a %v grace: reaped %v, want %v (stat: %v)",
					c.grace, reaped, c.reaped, err)
			}
		})
	}
}

// ---------------------------------------------------------------------
// container argv
// ---------------------------------------------------------------------

// The env carries the seat's LLM key, and argv is world-readable through
// /proc/<pid>/cmdline and every ps on the host.
func TestTheContainerEnvGoesInAFileNeverOnTheCommandLine(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	box := &containerBox{
		layout:    boxLayout{id: "box", root: root},
		cli:       containerCLI{path: "/usr/bin/docker"},
		container: "crewlet-sbx-box",
		env:       map[string]string{"ANTHROPIC_API_KEY": "sk-ant-secret"},
	}
	argv, err := box.execArgv("true", ExecOptions{})
	if err != nil {
		t.Fatalf("execArgv: %v", err)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "sk-ant-secret") {
		t.Fatalf("a secret reached argv: %s", joined)
	}
	if !strings.Contains(joined, "--env-file") {
		t.Fatalf("no --env-file in %s", joined)
	}
	blob, err := os.ReadFile(box.layout.envFile())
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	if !strings.Contains(string(blob), "ANTHROPIC_API_KEY=sk-ant-secret") {
		t.Fatalf("env file = %q", blob)
	}
	info, err := os.Stat(box.layout.envFile())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	// AND NOT IN THE MOUNT: the runtime's client reads it on the host, and
	// the container's job could replace a file in its home between execs.
	if within, err := filepath.Rel(box.layout.home(), box.layout.envFile()); err != nil ||
		!strings.HasPrefix(within, "..") {
		t.Fatalf("the env file %s is under the box's home %s", box.layout.envFile(), box.layout.home())
	}
}

// --env-file is line-oriented with no quoting, so a newline forges a variable.
func TestAnUnrepresentableEnvValueIsDroppedRatherThanForgingAVariable(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	box := &containerBox{
		layout: boxLayout{id: "box", root: root},
		cli:    containerCLI{path: "/usr/bin/docker"}, container: "c",
		env: map[string]string{
			"SAFE":     "fine",
			"INJECTED": "value\nADMIN_TOKEN=forged",
		},
	}
	if _, err := box.envArgs(nil); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	blob, err := os.ReadFile(box.layout.envFile())
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	if strings.Contains(string(blob), "ADMIN_TOKEN") {
		t.Fatalf("a newline forged a variable: %q", blob)
	}
	if !strings.Contains(string(blob), "SAFE=fine") {
		t.Fatalf("the representable variables were lost too: %q", blob)
	}
}

// A stale file would hand one phase another's environment.
func TestTheEnvFileIsRewrittenPerCall(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	box := &containerBox{
		layout: boxLayout{id: "box", root: root},
		cli:    containerCLI{path: "/usr/bin/docker"}, container: "c",
		env: map[string]string{"PHASE": "run"},
	}
	if _, err := box.envArgs(map[string]string{"SETUP_ONLY": "yes"}); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	if _, err := box.envArgs(nil); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	blob, _ := os.ReadFile(box.layout.envFile())
	if strings.Contains(string(blob), "SETUP_ONLY") {
		t.Fatalf("the executor's launch inherited the setup phase's environment: %q", blob)
	}
}

func TestTheContainerMapsInBoxPathsOntoItsSideOfTheMount(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	box := &containerBox{layout: boxLayout{id: "box", root: root}, cli: containerCLI{path: "d"}, container: "c"}
	// The box's side of its directory, never the directory itself, which
	// also holds the engine's records about the box.
	home := box.layout.home()

	// NAMED IN THE HOME, which every access opens as a root
	// ([boxLayout.openHome]).
	want := filepath.Join("workspace", "notes.md")
	notes := func(got string) bool { return got == want }
	got, err := box.homeRel(DefaultHome + "/workspace/notes.md")
	if err != nil {
		t.Fatalf("homeRel: %v", err)
	}
	if !notes(got) || filepath.Join(home, got) != filepath.Join(home, "workspace", "notes.md") {
		t.Fatalf("homeRel = %q, want %q", got, want)
	}
	// The mount compared as a path, not a prefix: a doubled separator, a
	// climb that comes back and a tilde (the home in every cell) are under it.
	for _, path := range []string{DefaultHome + "//workspace/notes.md", DefaultHome + "/../user/workspace/notes.md",
		"~/workspace/notes.md"} {
		if got, err := box.homeRel(path); err != nil || !notes(got) {
			t.Errorf("homeRel(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	// The home itself is no file in the box, as in a direct box
	// ([hostbox.SafeJoin] refuses a root).
	if got, err := box.homeRel(DefaultHome); err == nil {
		t.Fatalf("homeRel(home) = %q; want a refusal: the home is no file in the box", got)
	}
}

// A prefix test alone would pass this: it starts with the mount point and
// still resolves outside it, and setup-step file paths are operator config.
func TestTheContainerRefusesAPathThatResolvesOffTheMount(t *testing.T) {
	t.Parallel()
	box := &containerBox{layout: boxLayout{id: "box", root: t.TempDir()}, cli: containerCLI{path: "d"}, container: "c"}
	for _, path := range []string{
		DefaultHome + "/../../etc/cron.d/x",
		"/etc/passwd",
		"/usr/local/bin/tool",
	} {
		if _, err := box.homeRel(path); err == nil {
			t.Fatalf("homeRel(%q) succeeded — it would write to the engine host", path)
		}
	}
}

func TestTheBackgroundPidIsTheLastNumericLine(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"12345\n":                 "12345",
		"[1] 12345\n12345\n":      "12345",
		"warning: something\n999": "999",
		"":                        "",
		"no pid here":             "",
		// The backgrounded job shares the exec's stdout, so its own output
		// can land after the echo. The LAST numeric line is still the pid.
		"12345\ntrailing output\n": "12345",
	}
	for output, want := range cases {
		if got := trailingPID(output); got != want {
			t.Fatalf("trailingPID(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestAnUnknownContainerRuntimeIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := ResolveContainerRuntime("lxc"); err == nil {
		t.Fatal("an unknown runtime was accepted")
	}
}

// ---------------------------------------------------------------------
// control commands
// ---------------------------------------------------------------------

func TestAControlCommandThatHangsIsAbandonedWithATimeoutCode(t *testing.T) {
	t.Parallel()
	result, err := runHost(t.Context(), hostCommand{
		argv:    []string{"/bin/sh", "-c", "sleep 30"},
		env:     map[string]string{"PATH": os.Getenv("PATH")},
		timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("runHost: %v", err)
	}
	if result.ExitCode != 124 {
		t.Fatalf("ExitCode = %d, want 124", result.ExitCode)
	}
	if !strings.Contains(result.Stderr, "timed out") {
		t.Fatalf("Stderr = %q", result.Stderr)
	}
}

// `docker run` spawning a stuck child is precisely the case: killing the harness
// alone would leave it behind.
//
// THE FIXTURE IS WAITED FOR, NOT RACED: a short timeout would let a loaded
// runner miss the pid file. The timeout never fires; the teardown comes from
// CANCELLING the context, the same path in [runHost], whose select has one
// `<-ctx.Done()` case for both. A grandchild that never records its pid fails
// the case: the fixture did not come up.
func TestATimedOutControlCommandTakesItsChildrenWithIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := runHost(ctx, hostCommand{
			// THE PATH TRAVELS IN THE ENVIRONMENT, quoted where used, never
			// pasted into the shell source: t.TempDir() can carry a space or a
			// metacharacter, and a quoting bug must not look like a process
			// tree that survived its teardown.
			argv: []string{"/bin/sh", "-c", `sh -c 'echo $$ > "$PIDFILE"; sleep 300' & sleep 300`},
			// Never what ends this command: the cancel below is, and a
			// timeout racing it would make the case flaky.
			timeout: time.Minute,
			env:     map[string]string{"PATH": os.Getenv("PATH"), "PIDFILE": pidFile},
		})
		done <- err
	}()

	waitFor(t, 30*time.Second, func() bool { return fileSize(pidFile) > 0 },
		"the grandchild never recorded its pid, so the fixture never came up")
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read the grandchild's pid: %v", err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid: %v", err)
	}

	cancel()
	// THE CALLER'S CANCELLATION IS ITS OWN ANSWER, never the timeout's 124,
	// which reads as a box that timed out or an image that would not start.
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("runHost cancelled by its caller answered %v; want the cancellation", err)
	}
	if !procgrouptest.AwaitGone(t, child, 5*time.Second) {
		t.Fatal("a cancelled control command left its grandchild running")
	}
}

func TestControlOutputIsBoundedRatherThanTheEnginesMemory(t *testing.T) {
	t.Parallel()
	var c capture
	for range 8 {
		c.Write(make([]byte, captureLimit/4))
	}
	if len(c.String()) > captureLimit+128 {
		t.Fatalf("captured %d bytes, want it bounded near %d", len(c.String()), captureLimit)
	}
	if !strings.Contains(c.String(), "more bytes of output not kept") {
		t.Fatal("the bound was silent")
	}
}

// ONE WRITE CROSSING THE LIMIT IS MARKED TOO, and what is kept ends on a
// whole line, never mid-line or mid-character.
func TestASingleWritePastTheLimitIsMarkedOnALine(t *testing.T) {
	t.Parallel()
	line := strings.Repeat("日", 20) + "\n" // 61 bytes: the limit lands inside a rune
	var c capture
	c.Write([]byte(strings.Repeat(line, captureLimit/len(line)+10)))
	got := c.String()
	kept, note, ok := strings.Cut(got, "\n(")
	if !ok || !strings.Contains(note, "more bytes of output not kept") {
		t.Fatalf("a single overflowing write was clipped silently: …%q", got[max(0, len(got)-120):])
	}
	if !strings.HasSuffix(kept, strings.TrimSuffix(line, "\n")) || !utf8.ValidString(kept) {
		t.Error("what was kept does not end on a whole line")
	}
	total := len(line) * (captureLimit/len(line) + 10)
	if want := fmt.Sprintf("(%d more bytes", total-len(kept)-1); !strings.HasPrefix("("+note, want) {
		t.Errorf("the note %q does not count what was left out (want %s)", note, want)
	}

	var short capture
	short.Write([]byte("an error\n"))
	if short.String() != "an error\n" {
		t.Errorf("output within the limit changed: %q", short.String())
	}
}

// A HOST COMMAND WITH NO ENVIRONMENT IS NOT STARTED: os/exec reads a nil Env
// as "inherit the parent's", handing the child the engine's keyring and Tier A
// tokens. The refusal comes before the fork, so the child never runs.
func TestAHostCommandWithNoEnvironmentIsNotStarted(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "ran")
	_, err := runHost(t.Context(), hostCommand{
		argv: []string{"/bin/sh", "-c", `: > "$0"`, marker},
	})
	if err == nil {
		t.Fatal("a host command with no environment of its own was run")
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatal("the child ran before the refusal, holding the engine's environment")
	}
}

// THE CONTAINER RUNTIME'S HARNESS SEES NONE OF THE ENGINE'S SECRETS, and every
// setting of its own: the runtime and every plugin and credential helper it
// runs would otherwise hold the session keyring. What it needs still arrives:
// its DOCKER_HOST (a rootless daemon's socket) and the HOME holding its config.
func TestTheContainerRuntimeSeesNoneOfTheEnginesSecrets(t *testing.T) {
	// Not parallel: it sets process-wide environment variables, which is the
	// only way to prove what a real child INHERITS.
	for name, value := range map[string]string{
		"CREWLET_SECRET_KEY_2026_01": "keyring-material-not-for-the-runtime",
		"CREWLET_OPS_TOKEN":          "tier-a-token-not-for-the-runtime",
		"OTEL_EXPORTER_OTLP_HEADERS": "authorization=Bearer%20otlp-not-for-the-runtime",
		"ANTHROPIC_API_KEY":          "sk-ant-not-for-the-runtime",
	} {
		t.Setenv(name, value)
	}
	t.Setenv("DOCKER_HOST", "unix:///run/user/1000/docker.sock")
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	// THE DUMP PATH IS IN THE SCRIPT, not the environment: the environment
	// is what is under test, so nothing the case needs can travel in it.
	runtime := filepath.Join(dir, "docker")
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/env > '%s'\n", dump)
	writeExecutable(t, runtime, script)
	// The rootless probe: the runtime call every container box makes first.
	runtimeIsRootless(t.Context(), containerCLI{path: runtime})

	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the runtime never ran: %v", err)
	}
	got := string(raw)
	for _, secret := range []string{
		"keyring-material-not-for-the-runtime", "tier-a-token-not-for-the-runtime",
		"client-secret-not-for-the-runtime", "otlp-not-for-the-runtime",
		"sk-ant-not-for-the-runtime",
	} {
		if strings.Contains(got, secret) {
			t.Errorf("the container runtime was handed the engine's %q:\n%s", secret, got)
		}
	}
	for _, want := range []string{
		"DOCKER_HOST=unix:///run/user/1000/docker.sock", "HOME=" + os.Getenv("HOME"), "PATH=",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the runtime's environment lacks %q, which it needs to reach "+
				"the daemon the operator configured:\n%s", want, got)
		}
	}
}

// WHAT AN OPERATOR DECLARES FOR THE RUNTIME REACHES IT, OVER THE HOST'S.
//
// The fixed rule cannot know what a runtime's helpers read (a credential
// helper pulling a private image wants its cloud's role and profile), so
// `providers.sandbox.local.runtime_env` declares it: it arrives on every
// runtime call, beats the host's value for the same name, and is copied, not
// read from the caller's map.
func TestTheRuntimeGetsWhatItsRuntimeEnvDeclares(t *testing.T) {
	// Not parallel: PATH is how the backend finds its runtime, and the
	// host's DOCKER_CONFIG is what the declared one must beat.
	dir := t.TempDir()
	dump := filepath.Join(dir, "env")
	runtime := filepath.Join(dir, "docker")
	script := fmt.Sprintf("#!/bin/sh\n/usr/bin/env > '%s'\n", dump)
	writeExecutable(t, runtime, script)
	t.Setenv("PATH", dir)
	t.Setenv("DOCKER_CONFIG", "/home/engine/.docker")
	t.Setenv("SSH_AUTH_SOCK", "/tmp/the-engines-own-agent.sock")

	declared := map[string]string{
		"SSH_AUTH_SOCK": "/run/user/1000/ssh-agent.sock",
		"AWS_PROFILE":   "registry-pull",
		"DOCKER_CONFIG": "/etc/crewlet/docker",
	}
	l, err := NewLocal(LocalOptions{
		Placement: Container, Image: "img", Runtime: "docker",
		StateDir: t.TempDir(), RuntimeEnv: declared,
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	declared["AWS_PROFILE"] = "changed-after-construction"

	// Any runtime call carries it; the listing is one every Create makes.
	if _, err := l.liveContainers(t.Context()); err != nil {
		t.Fatalf("liveContainers: %v", err)
	}
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the runtime never ran: %v", err)
	}
	got := string(raw)
	for _, want := range []string{
		"SSH_AUTH_SOCK=/run/user/1000/ssh-agent.sock",
		"AWS_PROFILE=registry-pull",
		"DOCKER_CONFIG=/etc/crewlet/docker",
	} {
		if !strings.Contains(got, want+"\n") {
			t.Errorf("the runtime's environment lacks the declared %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the-engines-own-agent") {
		t.Errorf("the host's own SSH_AUTH_SOCK reached the runtime:\n%s", got)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func waitFor(t *testing.T, limit time.Duration, done func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
	t.Fatal(msg)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// layoutOf is where a local box's directory, home and records are.
func layoutOf(t *testing.T, box Sandbox) boxLayout {
	t.Helper()
	switch b := box.(type) {
	case *directBox:
		return b.layout
	case *containerBox:
		return b.layout
	}
	t.Fatalf("%T is not a local box", box)
	return boxLayout{}
}

// ageBox backdates a box's creation and keepalive stamps, standing in for time
// passing without making the suite wait for it.
func ageBox(t *testing.T, box Sandbox, by time.Duration) {
	t.Helper()
	layout := layoutOf(t, box)
	when := time.Now().Add(-by)
	if err := os.Chtimes(layout.root, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	alive := layout.aliveFile()
	if _, err := os.Stat(alive); err == nil {
		if err := os.Chtimes(alive, when, when); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// AN EMPTY ID NAMES NO BOX: joined, it gives the directory holding EVERY box,
// which a teardown would delete. It is reachable: a run's row exists before its
// box is attached, so a poll in that window connects to "".
func TestAnEmptyBoxIdIsRefusedRatherThanResolvingToTheWholeEstate(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	alive := mustCreate(t, local, Spec{})

	if box, err := local.Connect(t.Context(), ""); err == nil {
		t.Fatalf("Connect(\"\") returned a box rooted at %q", box.Home())
	}
	// And a kill on one is a no-op rather than a recursive delete.
	if err := local.Kill(t.Context(), ""); err != nil {
		t.Fatalf("Kill(\"\"): %v", err)
	}
	if _, err := os.Stat(alive.Home()); err != nil {
		t.Fatalf("a live box was destroyed by a teardown of the empty id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(local.root, "boxes")); err != nil {
		t.Fatalf("the directory holding every box was destroyed: %v", err)
	}
}

// The ids are minted here, never config, but the join is the one a traversal
// would exploit, on a path about to be deleted from.
func TestABoxIdThatIsAPathIsRefused(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	for _, id := range []string{"..", ".", "../elsewhere", "nested/id"} {
		if _, err := local.Connect(t.Context(), id); err == nil {
			t.Fatalf("Connect(%q) was accepted", id)
		}
	}
}

// A box that survives its own teardown keeps the seeded login and the run's
// files until the orphan reaper runs on some later create.
func TestKillWaitsForTheGroupBeforeRemovingTheBox(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	// A job writing into the box until it dies races the removal.
	if _, err := box.StartBackground(t.Context(),
		"while true; do date >> churn.log; done", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return fileSize(filepath.Join(home, WorkspaceSubdir, "churn.log")) > 0
	}, "the job never started writing")

	if err := local.Kill(t.Context(), box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the box survived its own teardown: %v", err)
	}
}

// The credential map is how a RECONNECTED box knows which files to sync back
// after the run refreshed them: losing it silently loses the refresh, and the
// fleet is logged out at the next token expiry with nothing in the log to say
// why.
func TestAnUnwritableCredentialMapIsReported(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout := boxLayout{id: "box-1", root: root}

	// A FILE where the records directory belongs, so MkdirAll cannot win.
	if err := os.WriteFile(layout.records(), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := recordCredentialMap(layout, map[string]seededCredential{
		".claude/.credentials.json": {Source: "/host/creds", Seed: "0a1b"},
	})
	if err == nil {
		t.Fatal("an unwritable credential map was reported as recorded")
	}
	if !strings.Contains(err.Error(), layout.records()) {
		t.Errorf("the error does not name the path: %v", err)
	}
}

// And it records cleanly when it can, or the check above proves nothing.
func TestTheCredentialMapRoundTrips(t *testing.T) {
	t.Parallel()
	layout := boxLayout{id: "box-2", root: t.TempDir()}
	want := seededCredential{Source: "/host/creds.json", Seed: "0a1b", Rule: harnessprofile.FileRule{
		Name: "config.json", LoginKeys: []string{"authTokens"}, RefreshKeys: []string{"providers.meta.access_token"},
		IdentityKeys: []string{"tokens.account_id"},
	}}
	if err := recordCredentialMap(layout, map[string]seededCredential{".claude/.credentials.json": want}); err != nil {
		t.Fatalf("recordCredentialMap: %v", err)
	}
	got := readCredentialMap(layout)
	if rec := got[".claude/.credentials.json"]; !reflect.DeepEqual(rec, want) {
		t.Errorf("the map did not round-trip, with the seed a write-back is judged against: %v", got)
	}
}

// Nothing to record is not a failure: a seat with no subscription login is
// the ordinary case for an API-key company.
func TestAnEmptyCredentialMapIsNotAFailure(t *testing.T) {
	t.Parallel()
	layout := boxLayout{id: "box-3", root: t.TempDir()}
	if err := recordCredentialMap(layout, nil); err != nil {
		t.Errorf("recordCredentialMap(nil): %v", err)
	}
}

// A CONTAINER THAT WOULD NOT START IS THE CONFIGURATION'S ONLY WHEN ITS IMAGE IS
// HERE: an image the runtime holds and refused to run is a setting to fix, and
// one it does not hold failed to arrive, which a retry may outlast.
func TestAContainerThatWouldNotStartIsClassedByWhetherItsImageIsHere(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		inspect int
		want    Why
	}{
		{"the image is here", 0, WhyConfiguration},
		{"the image is not here", 1, WhyProviderUnavailable},
	} {
		runtime := filepath.Join(t.TempDir(), "docker")
		writeExecutable(t, runtime, fmt.Sprintf("#!/bin/sh\n[ \"$1 $2\" = \"image inspect\" ] && exit %d\nexit 125\n", c.inspect))
		if got := startFailure(t.Context(), containerCLI{path: runtime}, "img"); got != c.want {
			t.Errorf("%s: a failed start is classed %q, want %q", c.name, got, c.want)
		}
	}
}
