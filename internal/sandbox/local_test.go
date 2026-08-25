//go:build unix

package sandbox

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newDirect mints a direct-mode provider rooted in a temp dir.
func newDirect(t *testing.T) *Local {
	t.Helper()
	local, err := NewLocal(LocalOptions{Containment: Direct, StateDir: t.TempDir()})
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
	_, err := NewLocal(LocalOptions{Containment: Container, StateDir: t.TempDir()})
	if err == nil {
		t.Fatal("a container box with no image fails only once an agent tries to use it — refuse at config time")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Fatalf("error does not name the missing field: %v", err)
	}
}

func TestAnUnknownContainmentIsRefused(t *testing.T) {
	if _, err := NewLocal(LocalOptions{Containment: "chroot", StateDir: t.TempDir()}); err == nil {
		t.Fatal("an unknown containment was accepted")
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
		t.Fatal("the engine's own API key reached the coding agent")
	}
	if !strings.HasPrefix(parts[3], box.Home()) {
		t.Fatalf("TMPDIR = %q, want it inside the box", parts[3])
	}
}

func TestThePerCommandEnvOverridesTheRunEnv(t *testing.T) {
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

// ---------------------------------------------------------------------
// the escape guard
// ---------------------------------------------------------------------

func TestDirectModeRefusesToWriteOutsideTheBox(t *testing.T) {
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

func TestDirectModeNamesTheContainerAlternativeWhenItRefuses(t *testing.T) {
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	err := box.WriteFile(t.Context(), "/usr/local/bin/tool", []byte("x"))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), string(Container)) {
		t.Fatalf("the refusal does not tell the operator what to do instead: %v", err)
	}
}

func TestDirectModeAcceptsAnAbsolutePathInsideTheBox(t *testing.T) {
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
	recorded, err := os.ReadFile(filepath.Join(box.Home(), ".crewlet", "box.pid"))
	if err != nil || strings.TrimSpace(string(recorded)) != pid {
		t.Fatalf("pid file = %q, %v; want %q — without it no later process can reach the job", recorded, err, pid)
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
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})

	pid, err := box.StartBackground(t.Context(), "exit 0", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	n, _ := strconv.Atoi(pid)
	waitFor(t, 5*time.Second, func() bool { return !groupExists(n) },
		"a finished job still answers the liveness probe — it was left a zombie")
}

// One killpg must reach the agent's children, not just the shell it started.
func TestClosingABoxKillsTheWholeProcessTree(t *testing.T) {
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
	waitFor(t, 10*time.Second, func() bool {
		return syscall.Kill(child, 0) != nil
	}, "the coding agent's own child survived teardown")
}

func TestClosingABoxRemovesItsDirectory(t *testing.T) {
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

func TestPauseStopsTheTreeAndConnectResumesIt(t *testing.T) {
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	counter := filepath.Join(box.Home(), WorkspaceSubdir, "ticks")

	if _, err := box.StartBackground(t.Context(),
		"while true; do echo x >> ticks; sleep 0.02; done", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > 0 }, "the job never started ticking")

	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Let anything already in flight land, then take the reading.
	time.Sleep(150 * time.Millisecond)
	stopped := fileSize(counter)
	time.Sleep(200 * time.Millisecond)
	if fileSize(counter) != stopped {
		t.Fatal("a paused box kept working")
	}

	resumed, err := local.Connect(t.Context(), box.ID())
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { resumed.Close(context.Background()) })
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > stopped },
		"Connect did not resume the paused box")
}

// Teardown of a PAUSED box is the case SIGCONT-first exists for: a stopped
// process never runs again to handle SIGTERM.
func TestAPausedBoxCanStillBeTornDown(t *testing.T) {
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	pid := readPID(boxLayout{id: box.ID(), root: box.Home()})
	if pid <= 0 {
		t.Fatal("no pid recorded")
	}
	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if err := box.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return !groupExists(pid) },
		"a paused box survived teardown — it never saw SIGTERM")
}

// Connect resumes, so the pause reaper must not use it: that would boot the
// work back up purely to stop it. Kill is the primitive that must not.
//
// SIGKILL needs no SIGCONT — it cannot be caught, blocked or ignored, so it
// reaches a stopped process directly. Waking the group first (which Close DOES
// have to do, because a stopped process really never runs to handle SIGTERM)
// lets a reaped clarification pause execute again on its way out.
//
// The tick file lives OUTSIDE the box, because Kill deletes the box: reading a
// counter inside it would measure the deletion, not the resume.
func TestKillReclaimsAPausedBoxWithoutResumingIt(t *testing.T) {
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	counter := filepath.Join(t.TempDir(), "ticks")
	// No sleep in the loop: any scheduling at all after a SIGCONT shows up.
	if _, err := box.StartBackground(t.Context(),
		"while true; do echo x >> "+counter+"; done", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return fileSize(counter) > 0 }, "the job never started")
	pid := readPID(boxLayout{id: box.ID(), root: box.Home()})

	if err := box.Pause(t.Context()); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// Let anything already in flight land before taking the reading.
	time.Sleep(200 * time.Millisecond)
	before := fileSize(counter)
	time.Sleep(100 * time.Millisecond)
	if fileSize(counter) != before {
		t.Fatal("the box was not actually paused")
	}

	if err := local.Kill(t.Context(), box.ID()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool { return !groupExists(pid) }, "Kill left the tree running")
	if after := fileSize(counter); after != before {
		t.Fatalf("Kill resumed the box before reclaiming it: %d bytes of work ran after the pause", after-before)
	}
	if _, err := os.Stat(box.Home()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Kill left the box directory behind")
	}
}

func TestKillingAVanishedBoxIsNotAnError(t *testing.T) {
	local := newDirect(t)
	if err := local.Kill(t.Context(), "never-existed"); err != nil {
		t.Fatalf("Kill of a box that is already gone: %v", err)
	}
}

func TestConnectingToAVanishedBoxSaysSo(t *testing.T) {
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
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	files := map[string]string{".claude/.credentials.json": shared}

	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{CredentialFiles: files})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seeded := filepath.Join(box.Home(), ".claude", ".credentials.json")
	if got, err := os.ReadFile(seeded); err != nil || string(got) != `{"token":"first"}` {
		t.Fatalf("the login was not seeded into the box: %q, %v", got, err)
	}
	// The coding agent rotates its token mid-run.
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
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seeded := filepath.Join(box.Home(), ".claude", ".credentials.json")
	if err := os.WriteFile(seeded, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// A fresh engine, holding nothing but the id.
	fresh, err := NewLocal(LocalOptions{Containment: Direct, StateDir: local.root})
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

// A torn-down box must never CREATE a login the operator has since removed.
func TestARemovedLoginIsNotRecreatedByATeardown(t *testing.T) {
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
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

// The keys are operator-overridable config.
func TestASeededCredentialCannotEscapeTheBox(t *testing.T) {
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

// Age alone does not mean orphaned. This is the bug the reaper had: it deleted
// the checkout and the pid file of every run that lasted longer than the TTL,
// leaving an unkillable job writing into a directory that no longer existed.
func TestTheReaperLeavesABoxWithALiveJobAlone(t *testing.T) {
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	if _, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{}); err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	ageBox(t, box.Home(), 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("the reaper deleted a box whose job is still running: %v", err)
	}
}

func TestTheReaperRemovesAnAbandonedBox(t *testing.T) {
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	ageBox(t, home, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an abandoned box survived the reaper: %v", err)
	}
}

// The keepalive is the "has it been abandoned?" half, and reading the
// directory's mtime instead would read a constant: a directory's mtime does
// not move when files are written inside its subdirectories.
func TestTheKeepaliveStampIsWhatSparesABoxTheReaper(t *testing.T) {
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	ageBox(t, box.Home(), 2*time.Hour)

	if err := box.SetTimeout(t.Context(), 300); err != nil {
		t.Fatalf("SetTimeout: %v", err)
	}
	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("the reaper deleted a box the waiter had just heart-beaten: %v", err)
	}
}

// A recycled pid pointing at some unrelated long-lived process would otherwise
// keep a dead box's directory alive forever.
func TestARecycledPidDoesNotKeepADeadBoxAlive(t *testing.T) {
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	// Pid 1 is a process that certainly predates the box.
	if err := os.MkdirAll(filepath.Join(home, ".crewlet"), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, ".crewlet", "box.pid"), []byte("1"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The two questions read two different stamps on purpose. Age the
	// keepalive so the box is a reap CANDIDATE, and leave the directory's
	// own mtime — the box's birth time — where it is, so pid 1, which
	// started before this box existed, reads as the recycled pid it is.
	ageKeepalive(t, home, 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a recycled pid kept a dead box's directory alive forever")
	}
}

func TestTheReaperCollectsCredentialsBeforeDeleting(t *testing.T) {
	store := t.TempDir()
	shared := filepath.Join(store, "credentials.json")
	if err := os.WriteFile(shared, []byte(`{"token":"first"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{
		CredentialFiles: map[string]string{".claude/.credentials.json": shared},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	seeded := filepath.Join(box.Home(), ".claude", ".credentials.json")
	if err := os.WriteFile(seeded, []byte(`{"token":"rotated"}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	ageBox(t, box.Home(), 2*time.Hour)

	local.reapOrphans(t.Context(), time.Minute)

	if got, _ := os.ReadFile(shared); string(got) != `{"token":"rotated"}` {
		t.Fatalf("shared login = %q — the reaper threw away a rotated credential", got)
	}
}

// A spec with a tiny or zero TTL must not make every box on the host instantly
// reapable, including one another engine just created.
func TestTheReaperFloorsItsCutoffRegardlessOfTheSpecTtl(t *testing.T) {
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	local.reapOrphans(t.Context(), 0)
	if _, err := os.Stat(box.Home()); err != nil {
		t.Fatalf("a zero TTL reaped a box that was just created: %v", err)
	}
}

func TestCreateReapsOrphansLeftByAPreviousEngine(t *testing.T) {
	local := newDirect(t)
	orphan, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	ageBox(t, orphan.Home(), 2*time.Hour)

	fresh := mustCreate(t, local, Spec{TimeoutSec: 60})
	if fresh.ID() == orphan.ID() {
		t.Fatal("Create reused an id")
	}
	if _, err := os.Stat(orphan.Home()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Create did not reap the orphan a previous engine left behind")
	}
}

// ---------------------------------------------------------------------
// container argv
// ---------------------------------------------------------------------

// The env carries the seat's LLM key, and argv is world-readable through
// /proc/<pid>/cmdline and every ps on the host.
func TestTheContainerEnvGoesInAFileNeverOnTheCommandLine(t *testing.T) {
	root := t.TempDir()
	box := &containerBox{
		layout:    boxLayout{id: "box", root: root},
		runtime:   "/usr/bin/docker",
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
	blob, err := os.ReadFile(filepath.Join(root, ".crewlet", "env"))
	if err != nil {
		t.Fatalf("env file: %v", err)
	}
	if !strings.Contains(string(blob), "ANTHROPIC_API_KEY=sk-ant-secret") {
		t.Fatalf("env file = %q", blob)
	}
	info, err := os.Stat(filepath.Join(root, ".crewlet", "env"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("env file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

// --env-file is line-oriented with no quoting, so a newline forges a variable.
func TestAnUnrepresentableEnvValueIsDroppedRatherThanForgingAVariable(t *testing.T) {
	root := t.TempDir()
	box := &containerBox{
		layout:  boxLayout{id: "box", root: root},
		runtime: "/usr/bin/docker", container: "c",
		env: map[string]string{
			"SAFE":     "fine",
			"INJECTED": "value\nADMIN_TOKEN=forged",
		},
	}
	if _, err := box.envArgs(nil); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	blob, err := os.ReadFile(filepath.Join(root, ".crewlet", "env"))
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
	root := t.TempDir()
	box := &containerBox{
		layout:  boxLayout{id: "box", root: root},
		runtime: "/usr/bin/docker", container: "c",
		env: map[string]string{"PHASE": "run"},
	}
	if _, err := box.envArgs(map[string]string{"SETUP_ONLY": "yes"}); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	if _, err := box.envArgs(nil); err != nil {
		t.Fatalf("envArgs: %v", err)
	}
	blob, _ := os.ReadFile(filepath.Join(root, ".crewlet", "env"))
	if strings.Contains(string(blob), "SETUP_ONLY") {
		t.Fatalf("the coding job inherited the setup phase's environment: %q", blob)
	}
}

func TestTheContainerMapsInBoxPathsOntoItsSideOfTheMount(t *testing.T) {
	root := t.TempDir()
	box := &containerBox{layout: boxLayout{id: "box", root: root}, runtime: "d", container: "c"}

	got, err := box.hostPath(DefaultHome + "/workspace/notes.md")
	if err != nil {
		t.Fatalf("hostPath: %v", err)
	}
	if want := filepath.Join(root, "workspace", "notes.md"); got != want {
		if resolved, _ := filepath.EvalSymlinks(root); got != filepath.Join(resolved, "workspace", "notes.md") {
			t.Fatalf("hostPath = %q, want %q", got, want)
		}
	}
	if got, err := box.hostPath(DefaultHome); err != nil || got != root {
		t.Fatalf("hostPath(home) = %q, %v; want %q", got, err, root)
	}
}

// A prefix test alone would pass this: it starts with the mount point and
// still resolves outside it, and setup-step file paths are operator config.
func TestTheContainerRefusesAPathThatResolvesOffTheMount(t *testing.T) {
	box := &containerBox{layout: boxLayout{id: "box", root: t.TempDir()}, runtime: "d", container: "c"}
	for _, path := range []string{
		DefaultHome + "/../../etc/cron.d/x",
		"/etc/passwd",
		"/usr/local/bin/tool",
	} {
		if _, err := box.hostPath(path); err == nil {
			t.Fatalf("hostPath(%q) succeeded — it would write to the engine host", path)
		}
	}
}

func TestTheBackgroundPidIsTheLastNumericLine(t *testing.T) {
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
	if _, err := ResolveContainerRuntime("lxc"); err == nil {
		t.Fatal("an unknown runtime was accepted")
	}
}

// ---------------------------------------------------------------------
// control commands
// ---------------------------------------------------------------------

func TestAControlCommandThatHangsIsAbandonedWithATimeoutCode(t *testing.T) {
	result, err := runHost(t.Context(), hostCommand{
		argv:    []string{"/bin/sh", "-c", "sleep 30"},
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

// `docker run` spawning a stuck child is precisely the case: killing the CLI
// alone would leave it behind.
func TestATimedOutControlCommandTakesItsChildrenWithIt(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	_, err := runHost(t.Context(), hostCommand{
		argv:    []string{"/bin/sh", "-c", "sh -c 'echo $$ > " + pidFile + "; sleep 300' & sleep 300"},
		timeout: time.Second,
		env:     map[string]string{"PATH": os.Getenv("PATH")},
	})
	if err != nil {
		t.Fatalf("runHost: %v", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Skip("the grandchild never recorded its pid before the timeout")
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("pid: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return syscall.Kill(child, 0) != nil },
		"a timed-out control command left its child running")
}

func TestControlOutputIsBoundedRatherThanTheEnginesMemory(t *testing.T) {
	var c capture
	for range 8 {
		c.Write(make([]byte, captureLimit/4))
	}
	if len(c.String()) > captureLimit+64 {
		t.Fatalf("captured %d bytes, want it bounded near %d", len(c.String()), captureLimit)
	}
	if !strings.Contains(c.String(), "truncated") {
		t.Fatal("truncation was silent")
	}
}

// os/exec reads a nil Env as "inherit the parent's", which is exactly what the
// allowlist exists to prevent — so no caller may reach that path by accident.
func TestFlattenEnvIsSortedAndNilOnlyWhenEmpty(t *testing.T) {
	if got := flattenEnv(nil); got != nil {
		t.Fatalf("flattenEnv(nil) = %v", got)
	}
	got := flattenEnv(map[string]string{"B": "2", "A": "1"})
	if len(got) != 2 || got[0] != "A=1" || got[1] != "B=2" {
		t.Fatalf("flattenEnv = %v, want it sorted", got)
	}
}

// ---------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------

func waitFor(t *testing.T, limit time.Duration, done func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if done() {
			return
		}
		time.Sleep(10 * time.Millisecond)
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

// ageKeepalive backdates only the "has it been abandoned?" stamp, leaving the
// box's birth time alone.
func ageKeepalive(t *testing.T, home string, by time.Duration) {
	t.Helper()
	when := time.Now().Add(-by)
	alive := filepath.Join(home, ".crewlet", "alive")
	if err := os.MkdirAll(filepath.Dir(alive), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if f, err := os.OpenFile(alive, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	if err := os.Chtimes(alive, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
}

// ageBox backdates a box's creation and keepalive stamps, standing in for time
// passing without making the suite wait for it.
func ageBox(t *testing.T, home string, by time.Duration) {
	t.Helper()
	when := time.Now().Add(-by)
	if err := os.Chtimes(home, when, when); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	alive := filepath.Join(home, ".crewlet", "alive")
	if _, err := os.Stat(alive); err == nil {
		if err := os.Chtimes(alive, when, when); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
}

// AN EMPTY ID NAMES NO BOX, and joining it gives the directory that holds
// EVERY box — so a "box" built from one would take the whole estate down with
// it on teardown. It is reachable: a run's row exists before its box is
// attached, so a poll landing in that window asks to connect to "".
func TestAnEmptyBoxIdIsRefusedRatherThanResolvingToTheWholeEstate(t *testing.T) {
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

// The ids are minted here and never come from config, but the join is the same
// one a traversal would exploit and the check costs one comparison on a path
// that is about to be deleted from.
func TestABoxIdThatIsAPathIsRefused(t *testing.T) {
	local := newDirect(t)
	for _, id := range []string{"..", ".", "../elsewhere", "nested/id"} {
		if _, err := local.Connect(t.Context(), id); err == nil {
			t.Fatalf("Connect(%q) was accepted", id)
		}
	}
}

// A box that survives its own teardown holds the seeded login and whatever the
// run wrote, and the only thing that will ever clean it up is the orphan
// reaper on some later create.
func TestKillWaitsForTheGroupBeforeRemovingTheBox(t *testing.T) {
	local := newDirect(t)
	box, err := local.Create(t.Context(), Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	home := box.Home()
	// A job that keeps writing into the box right up to the moment it dies,
	// which is what races the removal.
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
// after the run refreshed them, so losing it silently loses the refresh — and
// the fleet is logged out at the next token expiry with nothing in the log to
// say why.
//
// This is the case the previous shape could not report: the WriteFile error
// went into an `if err :=` binding that shadowed the variable being checked,
// so the warning never fired and no test could see it.
func TestAnUnwritableCredentialMapIsReported(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	layout := boxLayout{id: "box-1", root: root}

	// A FILE where the metadata directory belongs, so MkdirAll cannot win.
	if err := os.WriteFile(layout.meta(), []byte("in the way"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := recordCredentialMap(layout, map[string]string{".claude/.credentials.json": "/host/creds"})
	if err == nil {
		t.Fatal("an unwritable credential map was reported as recorded")
	}
	if !strings.Contains(err.Error(), layout.meta()) {
		t.Errorf("the error does not name the path: %v", err)
	}
}

// And it records cleanly when it can, or the check above proves nothing.
func TestTheCredentialMapRoundTrips(t *testing.T) {
	t.Parallel()
	layout := boxLayout{id: "box-2", root: t.TempDir()}
	files := map[string]string{".claude/.credentials.json": "/host/creds.json"}
	if err := recordCredentialMap(layout, files); err != nil {
		t.Fatalf("recordCredentialMap: %v", err)
	}
	got := readCredentialMap(layout)
	if got[".claude/.credentials.json"] != "/host/creds.json" {
		t.Errorf("the map did not round-trip: %v", got)
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
