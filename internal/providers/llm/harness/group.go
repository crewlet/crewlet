package harness

import (
	"context"
	"errors"
	"os/exec"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// harnessGroup is the process group a harness child leads, as it was recorded
// the moment the child started: the leader's pid AND its start time
// ([procgroup.Leader]).
//
// A pid alone stops naming the group once the leader is reaped. Every member
// gone, the kernel frees the group's id with its last process and may hand it
// to a stranger, and a SIGKILL to "the group led by pid N" then reaches that
// stranger's tree. Three places signal a harness's group after its leader was
// reaped: a call's end killing what survived it, a serve lifecycle's server
// stopped once it exited, and a device sign-in stopped. Each now asks first
// whether the group is still the one recorded ([harnessGroup.signal]), which
// is [procgroup.Kill]'s own condition and gitproc's practice.
type harnessGroup struct {
	leader procgroup.Leader
	// current answers whether the group is still the one recorded:
	// leader.Current, which a test replaces to make the kernel's answer
	// one it chooses.
	current func() (bool, error)
}

// recordGroup reads the identity of the group the child cmd has just started
// leads ([procgroup.Set] made it its group's leader). Call it straight after
// Start, while nothing has waited on the child, so the pid is certainly the
// child's.
//
// A CHILD WHOSE IDENTITY CANNOT BE READ IS ENDED AT ONCE: killed with its
// group, and reaped, before the error returns. Unreaped, its pid still names
// its own group, so this one kill needs no identity; every later one would,
// and none could be checked. So the caller reports that no harness ran,
// rather than run one it could not safely stop.
func recordGroup(cmd *exec.Cmd) (harnessGroup, error) {
	leader, err := procgroup.Identify(cmd.Process.Pid)
	if err != nil {
		_ = procgroup.Kill(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return harnessGroup{}, err
	}
	return harnessGroup{leader: leader, current: leader.Current}, nil
}

// signal sends a signal to the group with send, named signal for the log, but
// only while the group is still the one recorded, and reports whether it was.
// reason says why the group is being signalled.
//
// THE KERNEL'S ANSWER DECIDES, in its three values. No: the group is gone or
// its id leads another's processes, and nothing is sent. An error: the process
// table could not be read (`/proc` mounted with hidepid), so the group cannot
// be shown to be the one recorded; the signal is withheld and logged as
// `harness_group_signal_withheld`, as the sandbox and gitproc do. The window
// left is procgroup's own: a reap may land between the check and the signal,
// but the kernel reuses a pid only after cycling through every pid.
func (g harnessGroup) signal(ctx context.Context, reason, signal string, send func(pid int) error) bool {
	live, err := g.current()
	if err != nil {
		log.WarnContext(ctx, "harness_group_signal_withheld", "pgid", g.leader.PID,
			"signal", signal, "reason", reason, "error", err)
		return false
	}
	if !live {
		return false
	}
	if err := send(g.leader.PID); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		log.WarnContext(ctx, "harness_group_signal_failed", "pgid", g.leader.PID,
			"signal", signal, "reason", reason, "error", err)
	}
	return true
}
