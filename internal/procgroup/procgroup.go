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
