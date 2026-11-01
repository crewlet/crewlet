package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// ---------------------------------------------------------------------
// a local job's witness: was it seen to end, or lost with its engine?
// ---------------------------------------------------------------------

// goneEngine is an engine process that has come and gone: a process that ran
// on this host and has exited and been reaped, as the engine process before a
// restart has.
func goneEngine(t *testing.T) engineProcess {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the stand-in engine: %v", err)
	}
	proc, found, err := procgroup.Inspect(cmd.Process.Pid)
	if err != nil || !found {
		t.Fatalf("reading the stand-in engine's start time: found=%v err=%v", found, err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the stand-in engine: %v", err)
	}
	return engineProcess{pid: cmd.Process.Pid, start: proc.Start}
}

// witnessedBy writes p into a box's record of its job's witness, as the
// engine process p would have.
func witnessedBy(t *testing.T, l boxLayout, p engineProcess) {
	t.Helper()
	if err := writeRecord(l, witnessRecord, []byte(p.String())); err != nil {
		t.Fatalf("writing the witness: %v", err)
	}
}

// killJob takes a direct box's job group down, as the restart that replaces
// its engine does, and waits until it is gone.
func killJob(t *testing.T, box Sandbox) {
	t.Helper()
	leader := recordedLeader(t, box)
	if err := procgroup.Kill(leader.PID); err != nil {
		t.Fatalf("killing the job: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		current, err := leader.Current()
		return err == nil && !current
	}, "the killed job is still running")
}

// jobOver waits for a box's job to stop running and answers how it went.
func jobOver(t *testing.T, box Sandbox, handle string) JobState {
	t.Helper()
	var state JobState
	waitFor(t, 5*time.Second, func() bool {
		var err error
		state, err = box.Job(t.Context(), handle)
		return err == nil && state != JobRunning
	}, "the job still reads as running")
	return state
}

// A JOB KILLED WITH ITS ENGINE IS LOST: the engine process that last saw it
// running is gone too, so nothing saw it end — it went with that process, as a
// direct box's job does when its container restarts or its systemd unit is
// stopped with its control group. What it left is no result.
//
// Mutation: answer every job that is not running as ended, and a job its
// engine's restart killed is collected as though it had crashed on its own.
func TestAJobKilledWithItsEngineIsLost(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	handle, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	witnessedBy(t, layoutOf(t, box), goneEngine(t))
	killJob(t, box)

	if state := jobOver(t, box, handle); state != JobLost {
		t.Fatalf("a job killed with the engine that last saw it reads as %v; want lost", state)
	}
}

// A JOB FOUND RUNNING AFTER ITS ENGINE RESTARTED IS WATCHED BY THE NEW ONE: the
// job outlived the process that started it — only that process died — and this
// process found it running, so it is the job's witness from then on. A job that
// then dies dies in this engine's sight: a crash of its own, ended and
// collected, never taken for a loss and started again beside its own result.
//
// Mutation: leave the witness as the process that started the job, and a job
// that survived its engine's crash and failed on its own an hour later is
// relaunched as though the crash had killed it.
func TestAJobFoundRunningAfterARestartIsWatchedByTheNewEngine(t *testing.T) {
	t.Parallel()
	local := newDirect(t)
	box := mustCreate(t, local, Spec{})
	handle, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{})
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	l := layoutOf(t, box)
	witnessedBy(t, l, goneEngine(t))

	if state, err := box.Job(t.Context(), handle); err != nil || state != JobRunning {
		t.Fatalf("Job = %v, %v; want the surviving job running", state, err)
	}
	self, err := thisEngine()
	if err != nil {
		t.Fatalf("thisEngine: %v", err)
	}
	if raw, err := readRecord(l, witnessRecord); err != nil || strings.TrimSpace(string(raw)) != self.String() {
		t.Fatalf("the witness reads %q (%v); want this process, %q", raw, err, self.String())
	}

	killJob(t, box)
	if state := jobOver(t, box, handle); state != JobEnded {
		t.Fatalf("a job that died while this engine watched it reads as %v; want ended", state)
	}
}

// A BOX WITH NO WITNESS ENDED ITS JOB: nothing says the job was lost, and only
// a lost job is started again — so a box whose record of its witness is gone,
// or is not one this engine wrote, is collected as every job was before a job
// could be lost, which never starts a second run beside the first.
//
// Mutation: read a missing or unreadable witness as a gone one, and a box that
// cannot say starts its round again.
func TestABoxWithNoWitnessEndedItsJob(t *testing.T) {
	t.Parallel()
	for name, spoil := range map[string]func(l boxLayout) error{
		"missing":    func(l boxLayout) error { return os.Remove(l.witnessFile()) },
		"unreadable": func(l boxLayout) error { return writeRecord(l, witnessRecord, []byte("a stranger")) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			local := newDirect(t)
			box := mustCreate(t, local, Spec{})
			handle, err := box.StartBackground(t.Context(), "sleep 300", ExecOptions{})
			if err != nil {
				t.Fatalf("StartBackground: %v", err)
			}
			if err := spoil(layoutOf(t, box)); err != nil {
				t.Fatalf("spoiling the witness: %v", err)
			}
			killJob(t, box)
			if state := jobOver(t, box, handle); state != JobEnded {
				t.Fatalf("a job whose box names no witness reads as %v; want ended", state)
			}
		})
	}
}

// THE WITNESS IS READ BACK AS WRITTEN, AND NOTHING ELSE IS: the record sits
// beside a box whose job runs as the engine's own user, so its content is input
// rather than a fact.
func TestAWitnessIsReadBackAsWrittenAndNothingElseIs(t *testing.T) {
	t.Parallel()
	self, err := thisEngine()
	if err != nil {
		t.Fatalf("thisEngine: %v", err)
	}
	back, err := parseEngineProcess(self.String())
	if err != nil || back != self {
		t.Fatalf("parseEngineProcess(%q) = %+v, %v; want %+v", self.String(), back, err, self)
	}
	for _, refused := range []string{"", "123", "x 123", "0 123", "-4 123", "1 2 3"} {
		if _, err := parseEngineProcess(refused); err == nil {
			t.Errorf("parseEngineProcess(%q) was accepted", refused)
		}
	}
}

// ---------------------------------------------------------------------
// a container box's job: its runtime asked before its process
// ---------------------------------------------------------------------

// containerJobRuntime is a stand-in container runtime whose answers a case
// sets through files in its own directory: what `ps` lists for the box's
// container ("down" for a daemon that does not answer, "absent" for none, or
// the container's state), and the exit status of `kill -0` inside it. The
// listing also names a container whose name merely contains the box's, which
// must not be read as the box's.
func containerJobRuntime(t *testing.T, container string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "docker")
	script := fmt.Sprintf(`#!/bin/sh
dir='%s'
case "$1" in
ps)
  state=$(cat "$dir/state")
  [ "$state" = down ] && { echo 'Cannot connect to the Docker daemon' >&2; exit 1; }
  printf '%%s\texited\n' '%s-shadow'
  [ "$state" = absent ] || printf '%%s\t%%s\n' '%s' "$state"
  exit 0 ;;
exec) exit "$(cat "$dir/kill")" ;;
*) exit 0 ;;
esac
`, dir, container, container)
	writeExecutable(t, path, script)
	return path, dir
}

// A CONTAINER JOB IS ASKED OF ITS RUNTIME BEFORE ITS PROCESS: an exec into the
// container fails the same way whether the job is gone or the daemon is, so
// read alone a daemon that was briefly away ended a running job — and, with an
// engine that had just restarted as no witness to it, had its round started
// again beside it. The listing tells the two apart.
//
// Mutation: answer from the exec alone, and a daemon that does not answer is a
// job that is over.
func TestAContainerJobIsAskedOfItsRuntimeFirst(t *testing.T) {
	t.Parallel()
	const container = ContainerPrefix + "box"
	gone := goneEngine(t)
	for _, tc := range []struct {
		name, state, kill string
		witness           func(t *testing.T) engineProcess
		want              JobState
		wantErr           bool
	}{
		{name: "a daemon that does not answer is no answer", state: "down", kill: "1",
			witness: func(*testing.T) engineProcess { return gone }, wantErr: true},
		{name: "a paused container cannot be asked", state: "paused", kill: "0",
			witness: func(*testing.T) engineProcess { return gone }, wantErr: true},
		{name: "a running job in a running container runs", state: "running", kill: "0",
			witness: func(*testing.T) engineProcess { return gone }, want: JobRunning},
		{name: "a container gone with its host lost its job", state: "absent", kill: "0",
			witness: func(*testing.T) engineProcess { return gone }, want: JobLost},
		{name: "a job gone from a running container under a gone engine is lost", state: "running",
			kill: "1", witness: func(*testing.T) engineProcess { return gone }, want: JobLost},
		{name: "a container that stopped while this engine watched ended", state: "exited", kill: "0",
			witness: func(t *testing.T) engineProcess {
				self, err := thisEngine()
				if err != nil {
					t.Fatalf("thisEngine: %v", err)
				}
				return self
			}, want: JobEnded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime, dir := containerJobRuntime(t, container)
			for name, value := range map[string]string{"state": tc.state, "kill": tc.kill} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			l := boxLayout{id: "box", root: t.TempDir()}
			if err := os.MkdirAll(l.records(), 0o700); err != nil {
				t.Fatal(err)
			}
			witnessedBy(t, l, tc.witness(t))
			box := &containerBox{layout: l, cli: containerCLI{path: runtime}, container: container}

			state, err := box.Job(t.Context(), "42")
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("Job = %v; want no answer", state)
			case !tc.wantErr && (err != nil || state != tc.want):
				t.Fatalf("Job = %v, %v; want %v", state, err, tc.want)
			}
			if tc.want == JobRunning {
				self, _ := thisEngine()
				if raw, _ := readRecord(l, witnessRecord); strings.TrimSpace(string(raw)) != self.String() {
					t.Errorf("a running container job did not make this process its witness: %q", raw)
				}
			}
		})
	}
}
