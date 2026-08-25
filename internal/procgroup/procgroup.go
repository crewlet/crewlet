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
// as one negative pid. That is the entire content of this package, and it
// lives here rather than beside either caller because two copies of a
// platform-conditional signal helper is how one of them quietly stops
// matching the other.
package procgroup

import (
	"os/exec"
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

// Exists reports whether the group led by pid has anything in it.
//
// A pid is not proof of identity — pids are reused — so a caller that needs
// "and it is MINE" has to establish that separately. This answers only
// whether something is there.
func Exists(pid int) bool { return exists(pid) }

// Terminate sends SIGTERM to the group led by pid, giving the tree a chance
// to flush and exit cleanly.
//
// "Nothing in the group" is not an error: it is the outcome the caller wanted.
func Terminate(pid int) error { return signal(pid, sigTerm) }

// Kill sends SIGKILL to the group led by pid.
//
// Callers must have evidence the group is not already reaped — signalling a
// group whose leader has been waited on is the one way to reach a recycled
// pid, and "the tree is definitely still there" is cheap to establish.
func Kill(pid int) error { return signal(pid, sigKill) }
