//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// trapsUSR1 is a shell line that, sent SIGUSR1, writes "heard" into heard and
// goes on, and that writes its own pid into pid once its traps are set — a
// run's script's end protocol in miniature. Sent SIGUSR2, it writes heard's
// witness instead ([unsignalled]).
func trapsUSR1(heard, pid string) string {
	return `trap 'echo heard > ` + shQuote(heard) + `' USR1; trap 'echo witnessed > ` +
		shQuote(witnessOf(heard)) + `' USR2; echo $$ > ` + shQuote(pid) + `; while :; do sleep 0.05; done`
}

// witnessOf is the file a [trapsUSR1] shell writes when it is sent SIGUSR2.
func witnessOf(heard string) string { return heard + ".witness" }

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

// unsignalled fails the case if the [trapsUSR1] shell at pid, whose heard
// file is heard, was sent SIGUSR1 before now. It sends the shell SIGUSR2 and
// waits for the witness that writes: of two signals pending, the kernel
// delivers the lower-numbered first, and the shell runs the traps of the
// signals it caught in the same order, so an end request sent before the
// witness has written heard by the time the witness is written.
//
// NOT A QUIET WINDOW, of any length: a trap that has not run yet is no
// evidence that nothing was sent, and a loaded runner delays a trap past any
// window, so a window only sees a signal that is quick to show.
func unsignalled(t *testing.T, pid int, heard, what string) {
	t.Helper()
	witness := witnessOf(heard)
	if err := syscall.Kill(pid, syscall.SIGUSR2); err != nil {
		t.Fatalf("sending pid %d its witness signal: %v", pid, err)
	}
	waitFor(t, 10*time.Second, func() bool {
		_, err := os.Stat(witness)
		return err == nil
	}, fmt.Sprintf("pid %d never wrote its witness %s", pid, witness))
	if _, err := os.Stat(heard); err == nil {
		t.Fatal(what)
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
	unsignalled(t, script, heard, "the script was signalled for a job the record does not name")

	if err := box.EndJob(t.Context(), handle, script); err != nil {
		t.Fatalf("EndJob of the job's own script: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		_, err := os.Stat(heard)
		return err == nil
	}, "the job's script never heard its end request")
	unsignalled(t, stranger, strangerHeard, "the process outside the job was signalled")
}

// A DIRECT BOX KILLS EVERY PROCESS OF ITS JOB AND KEEPS THE BOX. The read that
// follows a kill still reads the box — its result, its session — so the kill
// is a teardown's signals without the teardown: a member that ignores SIGTERM
// outlives the grace and is taken by SIGKILL, nothing of the job's group is
// left running, and the box's files are still there to read.
//
// THE GRACE IS THE BUBBLE'S: the kill runs in a synctest bubble, where its
// wait for the group to go is timers that fire at exactly their times, and at
// the grace's last instant, the kill waiting on the timer that ends it, the
// member that ignores SIGTERM is read for a SIGKILL ([killSent]). A floor on
// how long the kill took on the machine's clock passed a kill that gave a
// shorter grace, and one that gave none whenever the kernel then took its
// time to reap the group.
//
// Mutation: kill through Close, and the box's files are gone; drop the
// SIGKILL, and the member that ignores SIGTERM is left running; send it before
// the grace runs out, and the member reads killed inside it.
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

	inBubble(t, func(t *testing.T) {
		killed := make(chan error, 1)
		go func() { killed <- box.KillJob(t.Context(), handle) }()
		// THE GRACE'S LAST INSTANT, every goroutine of the bubble blocked
		// again: the kill on the timer that ends the grace.
		time.Sleep(JobStopGrace - time.Nanosecond)
		synctest.Wait()
		if killSent(t, stubborn) {
			t.Fatalf("the member that ignores SIGTERM was killed inside its %v grace", JobStopGrace)
		}
		if err := <-killed; err != nil {
			t.Fatalf("KillJob: %v", err)
		}
	})
	if !procgrouptest.AwaitGone(t, stubborn, 5*time.Second) {
		t.Fatal("the member that ignores SIGTERM outlived the kill")
	}
	// THE KERNEL REAPS ON THE MACHINE'S CLOCK, and the kill's wait for it ran
	// on the bubble's, whose polls may outrun a loaded kernel: the group is
	// waited for here.
	waitFor(t, 5*time.Second, func() bool {
		current, err := job.Current()
		return err == nil && !current
	}, "the job's group is still current after the kill: a member was left running")
	got, err := box.ReadFile(t.Context(), kept)
	if err != nil || string(got) != "what the read reads" {
		t.Fatalf("the box's file reads %q (%v) after the kill, want it kept for the read", got, err)
	}
}

// killSent reports whether pid has been sent SIGKILL: it has exited — a
// zombie, or gone — or a SIGKILL waits on it. Linux keeps a process's pending
// signals where a test can read them (/proc/<pid>/status), and a SIGKILL sent
// to its group waits in its shared set from the moment kill(2) returns until
// it is reaped, so there the answer is the kernel's the instant it is read:
// never a kill the process has not yet acted on read as none. Elsewhere it is
// read off whether the process has exited alone.
func killSent(t *testing.T, pid int) bool {
	t.Helper()
	proc, found, err := procgroup.Inspect(pid)
	if err != nil {
		t.Fatalf("reading pid %d: %v", pid, err)
	}
	if !found || proc.Zombie {
		return true
	}
	if runtime.GOOS != "linux" {
		return false
	}
	status, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		t.Fatalf("reading pid %d's pending signals: %v", pid, err)
	}
	for line := range strings.Lines(string(status)) {
		name, mask, ok := strings.Cut(line, ":")
		if !ok || (name != "SigPnd" && name != "ShdPnd") {
			continue
		}
		pending, parseErr := strconv.ParseUint(strings.TrimSpace(mask), 16, 64)
		if parseErr != nil {
			t.Fatalf("pid %d's %s mask %q: %v", pid, name, mask, parseErr)
		}
		if pending&(1<<(syscall.SIGKILL-1)) != 0 {
			return true
		}
	}
	return false
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

// ownGroupEnv makes this test binary, re-executed, a process that leaves its
// parent's process group for a group of its own — no session's — and then
// becomes `/bin/sh -c` of the variable's value, its pid kept: a job's first
// process in a group no exec's own session leads, as a container runtime whose
// exec makes no session of its own would leave one ([startJobInOwnGroup]).
const ownGroupEnv = "CREWLET_SANDBOX_OWN_GROUP"

// AN init RATHER THAN TestMain's dispatch, which every platform's suite runs:
// leaving a process group is unix's alone, as this file is. It acts only in a
// binary re-executed with [ownGroupEnv] set, and never returns there.
func init() {
	script, ok := os.LookupEnv(ownGroupEnv)
	if !ok {
		return
	}
	if err := syscall.Setpgid(0, 0); err != nil {
		fmt.Fprintln(os.Stderr, "leaving the process group:", err)
		os.Exit(2)
	}
	_ = os.Unsetenv(ownGroupEnv)
	err := syscall.Exec("/bin/sh", []string{"sh", "-c", script}, os.Environ())
	fmt.Fprintln(os.Stderr, "becoming the job's shell:", err)
	os.Exit(2)
}

// startJobInOwnGroup starts script as a job in rig's container whose first
// process is in a group of its own that no exec's session leads, and leaves it
// with no group recorded, as an older build's start left one on such a
// runtime. It answers the job's handle and pid, and its group — the first
// process's own pid.
func startJobInOwnGroup(t *testing.T, rig containerRig, script string) (string, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	handle, err := rig.box.StartBackground(t.Context(),
		ownGroupEnv+"="+shQuote(script)+" "+shQuote(self)+" > /dev/null 2>&1", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	pid, _ := strconv.Atoi(handle)
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	waitFor(t, 10*time.Second, func() bool {
		proc, found, err := procgroup.Inspect(pid)
		return err == nil && found && proc.Group == pid
	}, "the job's first process never left its exec's group")
	if group, recorded := containerJobGroup(rig.box.layout, pid); recorded {
		t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	}
	if err := removeRecord(rig.box.layout, groupRecord); err != nil {
		t.Fatal(err)
	}
	return handle, pid
}

// startJobWithNoGroupRecorded starts script as a job in rig's container, its
// first process in the group its exec's own session leads as runc makes it,
// and leaves it with no group recorded, as an older build's start left one. It
// answers the job's handle and that group.
func startJobWithNoGroupRecorded(t *testing.T, rig containerRig, script string) (string, int) {
	t.Helper()
	handle, err := rig.box.StartBackground(t.Context(), script+" > /dev/null 2>&1", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	pid, _ := strconv.Atoi(handle)
	group, recorded := containerJobGroup(rig.box.layout, pid)
	if !recorded {
		raw, _ := os.ReadFile(rig.box.layout.groupFile())
		t.Fatalf("the premise: the job's start recorded no process group to take away (%q)", raw)
	}
	t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
	if err := removeRecord(rig.box.layout, groupRecord); err != nil {
		t.Fatal(err)
	}
	return handle, group
}

// awaitMembers waits for group to hold at least n living processes.
func awaitMembers(t *testing.T, group, n int) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		living, readable := livingIn(group)
		return readable && len(living) >= n
	}, fmt.Sprintf("the job's group never held %d living processes", n))
}

// A CONTAINER JOB THAT RECORDED NO GROUP IS KILLED BY THE GROUP ITS EXEC LEADS.
// A job an older build started recorded no process group, and its first
// process is not the job: it is the runner's wrapper, and the run's script and
// its harness are its children. So the kill reads, off that first process, the
// group the job's exec leads a session of its own with, and kills every
// process of it — the container's main process spared.
//
// Mutation: signal the first process alone, as the kill of a job with no group
// recorded once did, and the job's other processes outlive the kill.
func TestAContainerJobThatRecordedNoGroupIsKilledByTheGroupItsExecLeads(t *testing.T) {
	t.Parallel()
	rig := newContainerRig(t)
	main := startMain(t)
	handle, group := startJobWithNoGroupRecorded(t, rig,
		"sh -c "+shQuote("sleep 300 & while :; do sleep 0.05; done"))
	awaitMembers(t, group, 2)

	if err := rig.box.KillJob(t.Context(), handle); err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	waitFor(t, 10*time.Second, func() bool {
		living, readable := livingIn(group)
		return readable && len(living) == 0
	}, "a process of the job outlived the kill of a job that recorded no group")
	if proc, found, err := procgroup.Inspect(main); err != nil || !found || proc.Zombie {
		t.Fatalf("the container's main process went with the job (%+v, %v, %v)", proc, found, err)
	}
}

// A CONTAINER JOB WHOSE GROUP NO EXEC LEADS IS SIGNALLED NOTHING. A job that
// recorded no process group, whose first process is in a group no exec's own
// session leads — what a runtime whose exec makes no session leaves — has no
// group the box can name as the job's alone, and its first process alone is
// no kill of it: the kill signals nothing, says so, and the job still reads
// as running, never as a job that ended.
//
// Mutation: signal the first process alone where no group is named, as the
// kill once did, and the job reads as ended while its other processes run.
func TestAContainerJobWhoseGroupNoExecLeadsIsSignalledNothing(t *testing.T) {
	t.Parallel()
	rig := newContainerRig(t)
	// TWO PROCESSES THAT STAY, the shell waiting on its child: a member that
	// exits by itself would read as one the kill signalled.
	handle, pid := startJobInOwnGroup(t, rig, "sleep 300 & wait")
	awaitMembers(t, pid, 2)
	members, _ := livingIn(pid)

	if err := rig.box.KillJob(t.Context(), handle); !errors.Is(err, ErrNoJobGroup) {
		t.Fatalf("the kill of a job whose group no exec leads answered %v, want ErrNoJobGroup", err)
	}
	for _, member := range members {
		if running, err := procgrouptest.Running(member); err != nil || !running {
			t.Fatalf("process %d of the job was signalled by a kill that named no group of it (%v, %v)",
				member, running, err)
		}
	}
	if state, err := rig.box.Job(t.Context(), handle); err != nil || state != JobRunning {
		t.Fatalf("the job reads %q (%v) after a kill that signalled nothing, want it running", state, err)
	}
}

// boxPollRunner polls a job as internal/sandbox/executor's runner does — the
// done marker its wrapper writes as the job's script exits, and otherwise the
// box's own word on the job's first process — and its job's script answers no
// end request, as an older build's answers none.
type boxPollRunner struct {
	*FakeRunner
	done string
}

func (r boxPollRunner) Poll(ctx context.Context, box Sandbox, handle RunHandle) (JobState, error) {
	if _, err := os.Stat(r.done); err == nil {
		return JobEnded, nil
	}
	return box.Job(ctx, handle.CommandID)
}

// awaitBeat waits, within the bound the case gives every start, for the file
// a job's script rewrites every 50 ms to change from what it holds now: the
// script working. A beat is a fork and a write in the container's shell,
// which a loaded runner delays and never fakes, so the wait is as long as a
// beat needs rather than a window one has to land in.
func awaitBeat(t *testing.T, path, what string) {
	t.Helper()
	before, _ := os.ReadFile(path) //nolint:gosec // the case's own file
	waitFor(t, 10*time.Second, func() bool {
		now, _ := os.ReadFile(path) //nolint:gosec // the case's own file
		return len(now) > 0 && string(now) != string(before)
	}, what)
}

// A CONTAINER JOB WITH NO GROUP RECORDED IS READ OVER ONLY ONCE IT IS. The read
// that ends a stopped or lost job whose script answers no request — an older
// build's, which also recorded no process group in its container — kills it,
// and takes its session final only where the job is over: killed whole, by the
// group its exec leads, its script gone with its wrapper; or, where no exec
// leads its group, signalled nothing, read as still running and taken as it
// stands, not final — while its script works on, as the log says.
//
// THE SCRIPT IS READ BY WHAT EACH CLAIM NEEDS. That it works on is a beat
// awaited within the bound every start is given ([awaitBeat]): read over one
// fixed window instead, a runner that scheduled no beat in it failed the case
// though the script ran (CI run 41714027577). That it is gone is the group
// its exec leads holding no living process: a window with no beat in it is no
// evidence of that, and the same runner would pass a script that still runs.
//
// Mutation: kill a job with no group recorded by its first process alone, and
// the read takes the job for over, final, while its script still runs.
func TestAContainerJobWithNoGroupRecordedIsReadOverOnlyOnceItIs(t *testing.T) {
	for name, ownGroup := range map[string]bool{"its exec's group": false, "a group no exec leads": true} {
		t.Run(name, func(t *testing.T) {
			logs := captureLogs(t)
			rig := newContainerRig(t)
			beat, done := filepath.Join(rig.dir, "beat"), filepath.Join(rig.dir, "done")
			// THE RUNNER'S WRAPPER: the run's script its child, and the done
			// marker written once the script exits.
			wrapper := "sh -c " + shQuote("while :; do date +%s%N > "+shQuote(beat)+"; sleep 0.05; done") +
				"; echo $? > " + shQuote(done)
			var handle string
			group := 0
			if ownGroup {
				handle, _ = startJobInOwnGroup(t, rig, wrapper)
			} else {
				handle, group = startJobWithNoGroupRecorded(t, rig, "sh -c "+shQuote(wrapper))
				// THE GROUP HOLDS THE JOB, the wrapper and its script, so
				// the group's being empty after the read is the job's end.
				awaitMembers(t, group, 2)
			}
			awaitBeat(t, beat, "the job's script never beat")

			runner := boxPollRunner{FakeRunner: NewFakeRunner("older"), done: done}
			runner.Unheard = true
			coord := &Coordinator{endWait: time.Second, endPoll: 10 * time.Millisecond}
			over := coord.endJob(t.Context(), PendingRun{TurnID: "t1", SandboxID: "box1", CommandID: handle},
				rig.box, runner, nil)
			at := readCapture(BoxReadStop, Result{}, over)

			if ownGroup {
				if over || at.final || !at.pack {
					t.Fatalf("a job its kill signalled nothing of read over %v, its capture final %v and packed "+
						"now %v; want it still running, taken as it stands", over, at.final, at.pack)
				}
				awaitBeat(t, beat, "the premise: the job's script stopped working though nothing of it was signalled")
				for _, line := range []string{"sandbox_job_ungrouped", "sandbox_job_unended"} {
					if !strings.Contains(logs.String(), line) {
						t.Errorf("the read did not say %s:\n%s", line, logs.String())
					}
				}
				return
			}
			if !over || !at.final {
				t.Fatalf("a job killed whole read over %v, its capture final %v; want both", over, at.final)
			}
			waitFor(t, 10*time.Second, func() bool {
				living, readable := livingIn(group)
				return readable && len(living) == 0
			}, "the read took the job for over while a process of it, its script's or its wrapper's, still runs")
		})
	}
}
