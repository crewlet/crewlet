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

// Which engine process last saw a local box's job running, and whether it is
// still here.
//
// A JOB ON THE ENGINE HOST CAN GO WITH ITS ENGINE. A `direct` box's job is a
// process tree of the engine host, detached into a session of its own so that
// it outlives the engine process ([directBox.StartBackground]) — which it does
// when only that process dies. It does not survive what usually replaces an
// engine: its container restarted, which takes every process in it; its
// systemd unit stopped or restarted, which kills every process in the unit's
// control group by default; its host rebooted, which also takes a `container`
// box's job. Each kills the job in the same instant as the engine, mid-step,
// before its wrapper writes the marker that says how it ended.
//
// AND NOTHING ON THE JOB SAYS SO. The next engine finds its wrapper gone and no
// marker, which is exactly what a job that crashed on its own leaves — an OOM
// kill, a harness that died — and that one's partial output IS its result, read and
// judged as a failed run. Read the same way, a job its engine's restart killed
// resumed its turn with a truncated run as though the run had failed, or, once
// its box went too, ended the turn.
//
// THE BOX RECORDS ITS WITNESS: the engine process that last saw its job
// running — the one that started it, and then whichever process of this
// engine first finds it still running after a restart. A job that is not
// running ended in that witness's sight while the witness is still here, and
// is collected; a job whose witness is gone too went with it, or after it,
// before any engine could see it end — and is lost ([JobLost]). The second
// half is a window, not a point: a job that crashed on its own between the
// last engine's death and the next engine's first look reads as lost as well.
// Nothing can tell those two apart afterwards, and the loss is the answer that
// costs least when wrong: the round is carried on in a fresh run, where
// collecting a job its engine's restart killed costs the round.
//
// A PROCESS, NOT A NODE: a node is the same node across its restarts, and the
// restart is what kills the job. A process is its pid and the moment the
// kernel says it started ([procgroup.StartTime]), the pair that does not
// repeat — a pid alone is handed out again, and a restarted container begins
// its numbering anew.

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

// witnessJob records this engine process as the last to have seen the box's
// job running, where the record names another — once per process per job, so
// a poll that finds the job running costs one small read.
//
// NEVER FATAL. A box with no witness is read as a job that ENDED ([jobEnd]):
// collected, as every job was before a job could be lost, which is the answer
// that never starts a second run beside the first.
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
// engine process that last saw it running is still here — it went in that
// process's sight, a crash included — and [JobLost] once that process is gone
// too. See the package rule above.
//
// A box with no witness, or with one this engine did not write, ended: nothing
// says it was lost, and only a lost job is relaunched. A kernel that cannot be
// asked is the error a caller reads as no answer at all.
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
