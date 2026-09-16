package procgroup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// A PID IS NOT AN IDENTITY.
//
// The kernel hands a pid out again once the process holding it is gone, so a
// pid written down and read back later (by another process, after a restart,
// hours on) may name a process that has nothing to do with the one it was
// recorded for. Signalling that pid's group signals a stranger's tree, and
// probing it keeps state alive on behalf of a job that ended long ago.
//
// What does not repeat is the pair of the pid and the moment its process
// started, read from the kernel rather than inferred from anything the caller
// can see. [Leader] is that pair, and [Leader.Current] is the one question a
// caller holding a recorded pid is allowed to ask before acting on it.
//
// The start time is read from the process's own kernel record on both release
// platforms (see [inspect]). It is deliberately NOT a file's timestamp: the
// mtime of /proc/<pid> on Linux is the moment the kernel first instantiated
// that directory's inode, which is whenever somebody first looked, and it
// resets when the inode is evicted, so an old process can read as brand new.

// StartTime identifies when a process started, as an opaque token.
//
// Only equality is meaningful, and only against a token this package produced
// on the same host: the encoding differs between platforms and carries the
// boot it was read in, so a process that happens to hold the same pid and the
// same tick count after a reboot does not compare equal. The zero value is
// "unknown", which [Leader.Current] refuses to match.
type StartTime string

// validStartTime reports whether a token could have come from [inspect]: not
// empty and free of whitespace, so a token always survives being written as
// one field of a line and read back.
func validStartTime(s StartTime) bool {
	return s != "" && !strings.ContainsFunc(string(s), unicode.IsSpace)
}

// Process is what the kernel reports about one process.
type Process struct {
	// Start is when the process started. Stable for the process's whole
	// life, including after it exits and until its parent reaps it.
	Start StartTime

	// Group is the id of the process group the process belongs to.
	Group int

	// Zombie is true for a process that has exited and not been reaped. It
	// runs nothing and holds nothing but its table entry, so a caller
	// waiting for a process to be gone treats it as gone.
	Zombie bool
}

// Inspect reads what the kernel reports about pid.
//
// Three answers: the process (found), no such process (not found, nil
// error), or an error meaning the kernel could not be asked. A caller must not
// read the last as absence: absence is permission to act on a pid, and an
// unreadable record grants none.
func Inspect(pid int) (Process, bool, error) {
	if pid <= 0 {
		return Process{}, false, nil
	}
	return inspect(pid)
}

// Leader names a process group by its leader's pid AND that leader's start
// time.
//
// Every group this package makes has a leader whose pid is the group id ([Set]
// and [Detach] both do that), so a recorded Leader is enough to find the group
// again and to tell whether it is still the same one.
type Leader struct {
	PID   int
	Start StartTime
}

// ErrNoIdentity reports a process whose start time the kernel did not give:
// the process is gone, or the pid cannot be a group this package addresses.
var ErrNoIdentity = errors.New("procgroup: no identity for this process")

// Identify reads the identity of the process pid.
//
// Call it while the process is certainly the one you started: before its
// parent has waited on it, when a zombie still keeps its record and its pid
// cannot have been handed to anybody else.
func Identify(pid int) (Leader, error) {
	if !addressable(pid) {
		return Leader{}, fmt.Errorf("%w: pid %d is not a process group this package addresses", ErrNoIdentity, pid)
	}
	proc, found, err := inspect(pid)
	if err != nil {
		return Leader{}, fmt.Errorf("procgroup: reading the start time of pid %d: %w", pid, err)
	}
	if !found {
		return Leader{}, fmt.Errorf("%w: pid %d has already been reaped", ErrNoIdentity, pid)
	}
	return Leader{PID: pid, Start: proc.Start}, nil
}

// Current reports whether the group this names still exists AND is still the
// group it was recorded for.
//
// False with a nil error is definitive: the group is gone, or its pid now
// leads a group of some later process. An error means the kernel could not be
// asked, and a caller must decide which way that fails for what it is about to
// do; see the callers in the sandbox package, which keep a directory on an
// unknown answer and withhold a signal on one.
//
// A GROUP IS CURRENT ONLY WHILE ONE OF ITS MEMBERS HAS NOT EXITED. A zombie
// keeps its place in its group until its parent reaps it, and kill(2) with
// signal 0 answers such a group as present: that is [Exists], and it is the
// wrong question here. A job whose last worker has exited is over, whether or
// not somebody has collected the corpse yet, and nothing guarantees anybody
// will do so promptly. An orphan is reaped by whatever the kernel reparents it
// to, which in a container is often a first process that never waits, so a
// finished run read through the probe alone would keep its box directory and
// its "running" status for as long as that corpse lingers. So the members are
// read from the kernel's own process record (see [groupLiving]) and only a
// member that has not exited counts.
//
// The leader is read first, because while it has not exited it answers the
// question alone: the pid and start time name this very process, and it still
// leads the group. That is every poll of a running job, which then costs one
// record rather than a scan of the process table.
//
// Otherwise a group whose leader has exited while a living member runs on is
// still current. The kernel does not hand out a pid while a process group
// still carries it as its id (Linux keeps the pid allocated for as long as
// anything references it as a group, and XNU's allocator skips every live
// group and session id), so the pid cannot have been reused while this group
// lives. The members are read BEFORE the leader is read again for that
// reason: a pid reissued between the two readings shows up in the later one as
// a leader with another start time. The one sequence this cannot see through
// is a recycled pid that became a group leader and then exited leaving members
// behind, which needs a full pid wrap, a setsid or setpgid by the stranger,
// and its early exit, all between two readings of the same record.
func (l Leader) Current() (bool, error) {
	if !addressable(l.PID) || !validStartTime(l.Start) {
		return false, nil
	}
	proc, found, err := inspect(l.PID)
	if err != nil {
		return false, fmt.Errorf("procgroup: reading the start time of pid %d: %w", l.PID, err)
	}
	if found && proc.Start == l.Start && !proc.Zombie && proc.Group == l.PID {
		return true, nil
	}
	// The probe is one system call and its "no" is definitive (an empty
	// group has no zombies either), so it spares the member scan for the
	// common case of a group that is plainly gone.
	if !exists(l.PID) {
		return false, nil
	}
	living, err := groupLiving(l.PID)
	if err != nil {
		return false, fmt.Errorf("procgroup: reading the members of process group %d: %w", l.PID, err)
	}
	if !living {
		return false, nil
	}
	proc, found, err = inspect(l.PID)
	if err != nil {
		return false, fmt.Errorf("procgroup: reading the start time of pid %d: %w", l.PID, err)
	}
	if !found {
		// A living member answered and its leader is gone: see the
		// paragraph above for why that group can only be this one.
		return true, nil
	}
	return proc.Start == l.Start, nil
}

// String is the leader as one line: the pid and the start token, separated by
// a space. [ParseLeader] reads it back.
func (l Leader) String() string {
	return strconv.Itoa(l.PID) + " " + string(l.Start)
}

// ParseLeader reads a [Leader.String] back, refusing anything else.
//
// A recorded leader often lives somewhere another program can write, so a
// value that is not exactly the two fields is refused rather than guessed at:
// a bare pid in particular would be an identity with no start time, which is
// the thing this type exists to rule out.
func ParseLeader(s string) (Leader, error) {
	fields := strings.Fields(s)
	if len(fields) != 2 {
		return Leader{}, fmt.Errorf("procgroup: %q is not a recorded leader: want \"<pid> <start>\"", s)
	}
	pid, err := strconv.Atoi(fields[0])
	if err != nil || !addressable(pid) {
		return Leader{}, fmt.Errorf("procgroup: %q does not name a process group pid", fields[0])
	}
	start := StartTime(fields[1])
	if !validStartTime(start) {
		return Leader{}, fmt.Errorf("procgroup: %q is not a start time", fields[1])
	}
	return Leader{PID: pid, Start: start}, nil
}
