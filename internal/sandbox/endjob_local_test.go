//go:build unix

package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// trapsUSR1 is a shell line that, sent SIGUSR1, writes "heard" into heard and
// goes on, and that writes its own pid into pid once its trap is set — a run's
// script's end protocol in miniature.
func trapsUSR1(heard, pid string) string {
	return `trap 'echo heard > ` + shQuote(heard) + `' USR1; echo $$ > ` + shQuote(pid) +
		`; while :; do sleep 0.05; done`
}

// shQuote is s quoted for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// readPID waits for path to hold a pid, and returns it.
func readPID(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, 5*time.Second, func() bool {
		raw, err := os.ReadFile(path) //nolint:gosec // the case's own file
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil && pid > 1
	}, "no pid was written to "+path)
	return pid
}

// startStrangerTrapping starts a process in a group of its own — no box's —
// that writes "heard" into heard when it is sent SIGUSR1, and returns its pid
// once its trap is set.
func startStrangerTrapping(t *testing.T, dir string) (int, string) {
	t.Helper()
	heard, pidFile := filepath.Join(dir, "stranger-heard"), filepath.Join(dir, "stranger.pid")
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", trapsUSR1(heard, pidFile))
	procgroup.Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stranger: %v", err)
	}
	t.Cleanup(func() {
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Wait()
	})
	return readPID(t, pidFile), heard
}

// absentFor fails the case if path appears within a window a delivered signal
// would have shown in: a trap runs within the 50 ms of its shell's sleep.
func absentFor(t *testing.T, path, what string) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			t.Fatal(what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A DIRECT BOX ENDS ONLY A PROCESS OF ITS JOB. The pid an end request goes to
// is the job's own word — its script writes it in the box — and a direct box's
// job runs on the engine host as the engine's own user, so it could have
// written any pid of that user's. The request reaches the job's script, a
// member of the job's recorded group; a process outside the group is refused
// as no process of the job and sent nothing; and so is a pid below 2, and a
// handle the box's record does not name.
//
// Mutation: signal the pid as it was read, past the group check, and the
// stranger's trap writes its file.
func TestADirectBoxEndsOnlyAProcessOfItsJob(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	work := filepath.Join(box.Home(), WorkspaceSubdir)
	heard, scriptFile := filepath.Join(work, "heard"), filepath.Join(work, "script.pid")
	handle, err := box.StartBackground(t.Context(), "sh -c "+shQuote(trapsUSR1(heard, scriptFile)), ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	script := readPID(t, scriptFile)
	stranger, strangerHeard := startStrangerTrapping(t, t.TempDir())

	if err := box.EndJob(t.Context(), handle, stranger); !errors.Is(err, ErrNotJobProcess) {
		t.Fatalf("an end request to a process outside the job was answered %v, want ErrNotJobProcess", err)
	}
	if err := box.EndJob(t.Context(), handle, 1); !errors.Is(err, ErrNotJobProcess) {
		t.Fatalf("an end request to pid 1 was answered %v, want ErrNotJobProcess", err)
	}
	if err := box.EndJob(t.Context(), "2", script); !errors.Is(err, ErrNotJobProcess) {
		t.Fatalf("an end request for a job the box's record does not name was answered %v, "+
			"want ErrNotJobProcess", err)
	}
	absentFor(t, heard, "the script was signalled for a job the record does not name")

	if err := box.EndJob(t.Context(), handle, script); err != nil {
		t.Fatalf("EndJob of the job's own script: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(heard)
		return err == nil
	}, "the job's script never heard its end request")
	absentFor(t, strangerHeard, "the process outside the job was signalled")
}

// A DIRECT BOX KILLS EVERY PROCESS OF ITS JOB AND KEEPS THE BOX. The read that
// follows a kill still reads the box — its result, its session — so the kill
// is a teardown's signals without the teardown: a member that ignores SIGTERM
// outlives the grace and is taken by SIGKILL, nothing of the job's group is
// left running, and the box's files are still there to read.
//
// Mutation: kill through Close, and the box's files are gone; drop the
// SIGKILL, and the member that ignores SIGTERM is left running.
func TestADirectBoxKillsEveryProcessOfItsJobAndKeepsTheBox(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	work := filepath.Join(box.Home(), WorkspaceSubdir)
	stubbornFile := filepath.Join(work, "stubborn.pid")
	handle, err := box.StartBackground(t.Context(),
		"sh -c "+shQuote(`trap '' TERM; echo $$ > `+shQuote(stubbornFile)+`; while :; do sleep 0.05; done`)+
			" & sleep 300", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	stubborn := readPID(t, stubbornFile)
	kept := filepath.Join(box.Home(), ".crewlet", "kept")
	if err := box.WriteFile(t.Context(), kept, []byte("what the read reads")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	job := recordedLeader(t, box)

	began := time.Now()
	if err := box.KillJob(t.Context(), handle); err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	if took := time.Since(began); took < JobStopGrace/2 {
		t.Fatalf("the kill took %v: the member that ignores SIGTERM cannot have been given its grace", took)
	}
	if !procgrouptest.AwaitGone(t, stubborn, 5*time.Second) {
		t.Fatal("the member that ignores SIGTERM outlived the kill")
	}
	if current, err := job.Current(); err != nil || current {
		t.Fatalf("the job's group is current after the kill (%v, %v): a member was left running", current, err)
	}
	got, err := box.ReadFile(t.Context(), kept)
	if err != nil || string(got) != "what the read reads" {
		t.Fatalf("the box's file reads %q (%v) after the kill, want it kept for the read", got, err)
	}
}

// containerRig is a container box driven through a stand-in runtime that runs
// each `exec` on the engine host as the exec's own session leader — what runc
// makes an exec's first process (nsexec.c's setsid) — and lists the box's
// container as running. Every call the runtime is handed is logged in calls.
type containerRig struct {
	box   *containerBox
	dir   string
	calls string
}

func newContainerRig(t *testing.T) containerRig {
	t.Helper()
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	runtime := filepath.Join(dir, "runtime")
	name := ContainerPrefix + "box1"
	// THE EXEC BECOMES ITS COMMAND: runHost starts the runtime in a session
	// of its own, so the shell exec'd here leads that session, as runc makes
	// the first process of every `docker exec`.
	writeExecutable(t, runtime, "#!/bin/sh\necho \"$*\" >> "+shQuote(calls)+"\n"+
		"case \"$1\" in\n"+
		"exec) shift; while [ $# -gt 0 ]; do case \"$1\" in -w|--env-file) shift 2;; *) break;; esac; done; "+
		"shift; exec \"$@\";;\n"+
		"ps) printf '%s\\trunning\\n' "+shQuote(name)+";;\n"+
		"esac\n")
	layout := boxLayout{id: "box1", root: filepath.Join(dir, "box")}
	for _, d := range []string{layout.home(), layout.records()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return containerRig{box: &containerBox{layout: layout, cli: containerCLI{path: runtime}, container: name},
		dir: dir, calls: calls}
}

func (r containerRig) logged() string {
	raw, _ := os.ReadFile(r.calls)
	return string(raw)
}

// startMain starts the stand-in for the container's main process (`sleep
// infinity` under its init), in a group of its own, and returns its pid.
func startMain(t *testing.T) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "sleep", "300")
	procgroup.Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the container's main process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// livingIn lists the processes of group pgid that have not exited, read off
// /proc — false where there is none to read.
func livingIn(pgid int) ([]int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, false
	}
	var living []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		proc, found, err := procgroup.Inspect(pid)
		if err == nil && found && proc.Group == pgid && !proc.Zombie {
			living = append(living, pid)
		}
	}
	return living, true
}

// A CONTAINER BOX ENDS AND KILLS ITS JOB INSIDE THE CONTAINER. The job's start
// records, on the engine's side, the process group the exec that started it
// leads — the whole job, and nothing else of the container's. An end request
// reaches the job's script by its pid in the container, and one to a pid that
// is not there is refused as no process of the job; and the kill ends every
// process of the job's group and spares the container's main process, which
// the read after the kill still execs beside.
//
// Mutation: kill the job's first process alone, past its recorded group, and a
// member of the job is left running.
func TestAContainerBoxEndsAndKillsItsJobInsideTheContainer(t *testing.T) {
	t.Parallel()
	rig := newContainerRig(t)
	main := startMain(t)
	heard, scriptFile := filepath.Join(rig.dir, "heard"), filepath.Join(rig.dir, "script.pid")
	handle, err := rig.box.StartBackground(t.Context(),
		"sh -c "+shQuote("sleep 300 & "+trapsUSR1(heard, scriptFile))+" > /dev/null 2>&1", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	pid, _ := strconv.Atoi(handle)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	group, recorded := containerJobGroup(rig.box.layout, pid)
	if !recorded {
		raw, _ := os.ReadFile(rig.box.layout.groupFile())
		t.Fatalf("the job's start recorded no process group (%q)", raw)
	}
	t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	script := readPID(t, scriptFile)

	gone := exec.CommandContext(t.Context(), "true")
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	if err := rig.box.EndJob(t.Context(), handle, gone.Process.Pid); !errors.Is(err, ErrNotJobProcess) {
		t.Fatalf("an end request to a pid that is not there was answered %v, want ErrNotJobProcess", err)
	}
	if err := rig.box.EndJob(t.Context(), handle, 1); !errors.Is(err, ErrNotJobProcess) {
		t.Fatalf("an end request to pid 1 was answered %v, want ErrNotJobProcess", err)
	}
	if strings.Contains(rig.logged(), "kill -USR1 1 ") {
		t.Fatalf("pid 1 was sent the end request:\n%s", rig.logged())
	}
	if err := rig.box.EndJob(t.Context(), handle, script); err != nil {
		t.Fatalf("EndJob of the job's script: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(heard)
		return err == nil
	}, "the job's script never heard its end request")

	if err := rig.box.KillJob(t.Context(), handle); err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		living, readable := livingIn(group)
		return readable && len(living) == 0
	}, "a process of the job's group outlived the kill")
	if proc, found, err := procgroup.Inspect(main); err != nil || !found || proc.Zombie {
		t.Fatalf("the container's main process went with the job (%+v, %v, %v)", proc, found, err)
	}
}

// A CONTAINER JOB AN OLDER BUILD STARTED IS KILLED BY ITS HANDLE. Such a start
// recorded no process group, so the job's first process alone is told to go,
// then killed after the grace; the rest of the job is left to the box's
// reclaim, and nothing else of the container's is touched.
//
// Mutation: signal `-0`, the killing shell's own group, in place of the
// handle, and the job's first process is left running.
func TestAContainerJobStartedByAnOlderBuildIsKilledByItsHandle(t *testing.T) {
	t.Parallel()
	rig := newContainerRig(t)
	main := startMain(t)
	handle, err := rig.box.StartBackground(t.Context(),
		"sh -c "+shQuote("sleep 300 & while :; do sleep 0.05; done")+" > /dev/null 2>&1", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	pid, _ := strconv.Atoi(handle)
	if group, recorded := containerJobGroup(rig.box.layout, pid); recorded {
		t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	}
	// AS AN OLDER BUILD LEFT IT: no group recorded.
	if err := removeRecord(rig.box.layout, groupRecord); err != nil {
		t.Fatal(err)
	}

	if err := rig.box.KillJob(t.Context(), handle); err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
		t.Fatalf("the job's first process outlived the kill:\n%s", rig.logged())
	}
	if proc, found, err := procgroup.Inspect(main); err != nil || !found || proc.Zombie {
		t.Fatalf("the container's main process went with the job (%+v, %v, %v)", proc, found, err)
	}
	if !strings.Contains(rig.logged(), "kill -TERM "+handle+" ") {
		t.Fatalf("the job's handle was not what was signalled:\n%s", rig.logged())
	}
}
