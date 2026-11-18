package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// A local box records its job's witness: the engine process that last saw the
// job running — its starter, then whichever process first finds it running
// after a restart. A `direct` job outlives its engine process alone
// ([directBox.StartBackground]), but a container restart, a systemd stop or a
// reboot kills it with the engine, before its wrapper writes its end marker:
// the trace a crash of its own (an OOM kill) leaves, whose partial output IS
// its result. So a job not running whose witness still runs ended and is
// collected; one whose witness is gone too is lost ([JobLost]) and its round
// carried on in a fresh run. A crash before the next engine looks reads as lost
// too, the cheaper error: collecting a killed job costs the round. A witness is
// a process, not a node (the restart kills the job): its pid and kernel start
// time ([procgroup.StartTime]), since pids are reused and containers renumber.

// engineProcess is one process of this engine on its host.
type engineProcess struct {
	pid   int
	start procgroup.StartTime
}

// thisEngine is this process, read once: its identity cannot change while it
// runs.
var thisEngine = sync.OnceValues(func() (engineProcess, error) {
	pid := os.Getpid()
	proc, found, err := procgroup.Inspect(pid)
	switch {
	case err != nil:
		return engineProcess{}, fmt.Errorf("reading this engine process's start time: %w", err)
	case !found:
		return engineProcess{}, fmt.Errorf("the kernel has no record of this engine process (pid %d)", pid)
	}
	return engineProcess{pid: pid, start: proc.Start}, nil
})

// String is the process as one line, which [parseEngineProcess] reads back.
func (p engineProcess) String() string { return strconv.Itoa(p.pid) + " " + string(p.start) }

// parseEngineProcess reads an [engineProcess.String] back, refusing anything
// else: the record lies beside a box whose job runs as the engine's own user,
// so its content is input rather than a fact.
func parseEngineProcess(s string) (engineProcess, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return engineProcess{}, fmt.Errorf("%q is not an engine process: want \"<pid> <start>\"", s)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || pid <= 0 {
		return engineProcess{}, fmt.Errorf("%q does not name a process id", fields[0])
	}
	if strings.ContainsFunc(fields[1], unicode.IsControl) {
		return engineProcess{}, fmt.Errorf("%q is not a start time", fields[1])
	}
	return engineProcess{pid: pid, start: procgroup.StartTime(fields[1])}, nil
}

// running reports whether p is still running: its pid's kernel record still
// carries the start time p recorded, and it has not exited. An error is a
// kernel that could not be asked.
func (p engineProcess) running() (bool, error) {
	proc, found, err := procgroup.Inspect(p.pid)
	if err != nil {
		return false, fmt.Errorf("reading the kernel's record of engine process %d: %w", p.pid, err)
	}
	return found && !proc.Zombie && proc.Start == p.start, nil
}

// witnessJob records this engine process as the box's job's witness where the
// record names another, so a poll that finds the job running costs one small
// read. Never fatal: a box with no witness reads as ENDED ([jobEnd]), the
// answer that never starts a second run beside the first.
func witnessJob(l boxLayout) {
	self, err := thisEngine()
	if err != nil {
		localLog.Warn("local_sandbox_witness_unknown", "sandbox_id", l.id, "error", err.Error(),
			"detail", "this engine process could not be identified, so the box does not record "+
				"it as its job's witness; a job of this box that dies unseen is collected as ended")
		return
	}
	raw, err := readRecord(l, witnessRecord)
	if err == nil && strings.TrimSpace(string(raw)) == self.String() {
		return
	}
	if err := writeRecord(l, witnessRecord, []byte(self.String())); err != nil {
		localLog.Warn("local_sandbox_witness_unwritten", "sandbox_id", l.id, "error", err.Error(),
			"detail", "the box could not record this engine process as its job's witness; "+
				"a job of this box that dies unseen is collected as ended")
	}
}

// jobEnd is how a box's job that is not running went: [JobEnded] while the
// engine process that last saw it running is still here, a crash included, and
// [JobLost] once that process is gone too (the rule atop this file). A box with
// no witness, or one this engine did not write, ended: only a lost job is
// relaunched. An error (a kernel that could not be asked) is no answer at all.
func jobEnd(l boxLayout) (JobState, error) {
	raw, err := readRecord(l, witnessRecord)
	switch {
	case absent(err):
		return JobEnded, nil
	case err != nil:
		return "", fmt.Errorf("reading the job's witness %s: %w", l.witnessFile(), err)
	}
	witness, err := parseEngineProcess(strings.TrimSpace(string(raw)))
	if err != nil {
		localLog.Warn("local_sandbox_witness_unreadable", "sandbox_id", l.id,
			"path", l.witnessFile(), "error", err.Error(),
			"detail", "the box's record of the engine process that last saw its job running is "+
				"not one this engine wrote, so the job is read as ended and collected")
		return JobEnded, nil
	}
	running, err := witness.running()
	switch {
	case err != nil:
		return "", err
	case running:
		return JobEnded, nil
	}
	return JobLost, nil
}
