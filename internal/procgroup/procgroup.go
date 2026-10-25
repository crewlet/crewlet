// Package procgroup addresses a child process TREE with one signal.
//
// Every long-running child this engine starts is launched through something
// that forks: an MCP server behind `uvx` or `npx`, a coding CLI behind a Node
// or Bun runtime, a sandbox command behind a shell. Signalling the process
// Go started reaches the launcher and nothing beneath it, so the real worker
// survives its parent, keeps the inherited stderr descriptor open, and goes
// on holding whatever it held.
//
// Putting the child in its own process group makes the whole tree addressable
// as one negative pid. That is most of this package, and it lives here rather
// than beside any one caller because two copies of a platform-conditional
// signal helper is how one of them quietly stops matching the other.
//
// The rest is the question a caller has to answer before it signals a pid it
// did not just start: is that group still the one it recorded? Pids are
// reused, so a pid read back from disk may lead a stranger's group, and the
// answer ([Leader]) is the pid together with its leader's start time as the
// kernel reports it. It is here for the same reason as the signals: the start
// time is read differently on every platform, and a second reader is a second
// definition of "the same process".
//
// And one figure about a group: the CPU its members have spent ([GroupCPU]),
// read from the same process records, because a group signalled as one dies
// without its leader reaping the rest, and the leader's own usage then counts
// none of them.
package procgroup

import (
	"fmt"
	"os/exec"
	"time"
)

// Set puts cmd's child in its own process group, so [Terminate] and [Kill]
// reach everything it forks.
//
// Call it before Start. On a platform without process groups it does nothing,
// and the signalling functions say so rather than reporting a success they
// did not achieve.
func Set(cmd *exec.Cmd) { set(cmd) }

// Detach puts cmd's child in its own SESSION, which is [Set] plus two more
// properties a long-lived detached job needs:
//
//   - No controlling terminal, so a Ctrl-C in the operator's shell does not
//     reach into a running job.
//   - Reparenting to init if the engine dies, which is what lets a detached
//     coding run survive a restart and be collected by the next engine.
//
// A session leader also leads its own process group, so everything [Kill],
// [Terminate], [Stop] and [Continue] do for a Set child works here too.
func Detach(cmd *exec.Cmd) { detach(cmd) }

// Stop suspends every process in the group led by pid (SIGSTOP).
//
// Uncatchable by design: it is the pause a sandbox takes while it waits for a
// human to answer a coding agent's question, and a job that could decline to
// pause would go on spending the seat's budget.
func Stop(pid int) error { return signal(pid, sigStop) }

// Continue resumes a group suspended by [Stop] (SIGCONT).
func Continue(pid int) error { return signal(pid, sigCont) }

// Exists reports whether the group led by pid has anything in it, zombies
// included.
//
// It is the signal-0 probe, so it answers exactly what a signal to the group
// would reach: a group whose only members have exited and wait to be reaped is
// still there. That is the question a caller asks when it wants to know that a
// process was REAPED rather than merely that it exited. A caller asking whether
// a job is still running asks [Leader.Current], which counts only members that
// can run and also checks that the group is still the one recorded: pids are
// reused, and this answers only whether something is there.
func Exists(pid int) bool { return exists(pid) }

// Terminate sends SIGTERM to the group led by pid, giving the tree a chance
// to flush and exit cleanly.
//
// "Nothing in the group" is not an error: it is the outcome the caller wanted.
func Terminate(pid int) error { return signal(pid, sigTerm) }

// GroupCPU is the CPU the members of the process group led by pid have spent
// so far: every member's user and system time, and that of every child each
// has waited for.
//
// It is the figure a caller reads BEFORE it signals a group whose cost it
// counts. wait4 hands a parent the time of the children it reaped, and a
// leader killed with its group dies without reaping the children dying beside
// it: upload-pack's pack-objects, receive-pack's index-pack, orphaned and
// reaped by whatever the kernel hands them to. So the leader's own usage at
// its reap is the leader alone, and what the tree spent until the signal is
// only to be had here, from the members while they are still members.
//
// Read from the kernel's own process record on Linux, the members found by a
// scan of the process table. A record that cannot be read is reported beside
// the sum of every one that could, so the figure is only ever short and the
// caller may keep it as a floor. Elsewhere it is [errors.ErrUnsupported]: XNU
// keeps a process's CPU where only libproc reads it, and the engine builds
// without cgo.
func GroupCPU(pid int) (time.Duration, error) {
	if !addressable(pid) {
		return 0, fmt.Errorf("procgroup: pid %d is not a process group this package addresses", pid)
	}
	return groupCPU(pid)
}

// Kill sends SIGKILL to the group led by pid.
//
// Callers must have evidence the group is not already reaped: signalling a
// group whose leader has been waited on is the one way to reach a recycled
// pid. A caller holding the process it started has that evidence already; one
// holding a pid it read back has it only from [Leader.Current].
func Kill(pid int) error { return signal(pid, sigKill) }

// addressable reports whether kill(2) reads -pid as ONE process group.
//
// Two values do not, and both are refused rather than signalled. Zero and
// below would address the caller's own group, which is every process in the
// engine's session. One is the sharper case: kill(-1, sig) is the BROADCAST
// form, delivered to every process the caller has permission to signal, so a
// SIGKILL "to the group led by pid 1" takes down the engine, every coding job
// and, on a workstation, the operator's whole login. No child this package
// addresses can be pid 1: init holds it for the life of the pid namespace,
// the engine's own included when it runs as a container's first process.
func addressable(pid int) bool { return pid > 1 }
