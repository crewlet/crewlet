package procgroup

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
)

// A PID IS NOT AN IDENTITY.
//
// The kernel hands a pid out again once its process is gone, so a recorded pid
// read back later may name an unrelated process: signalling its group hits a
// stranger's tree. What does not repeat is the pair of the pid and its
// process's start time, read from the kernel's own record on both release
// platforms (see [inspect]). [Leader] is that pair, and [Leader.Current] is
// the one question a caller holding a recorded pid may ask before acting.
//
// The start time is deliberately NOT a file's timestamp: the mtime of
// /proc/<pid> is when the kernel first instantiated the inode, and resets on
// eviction, so an old process can read as brand new.

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
// group it was recorded for. False with a nil error is definitive. An error
// means the kernel could not be asked; the caller decides which way that fails
// (the sandbox keeps a directory, withholds a signal).
//
// A GROUP IS CURRENT ONLY WHILE ONE OF ITS MEMBERS HAS NOT EXITED. A zombie
// keeps its place in its group until reaped, and kill(2) with signal 0 answers
// such a group as present ([Exists], the wrong question here); an orphan's
// reaper may never collect it, leaving a finished run "running". So members
// are read from the kernel's record (see [groupLiving]) and only one that has
// not exited counts.
func (l Leader) Current() (bool, error) {
	if !addressable(l.PID) || !validStartTime(l.Start) {
		return false, nil
	}
	proc, found, err := inspect(l.PID)
	if err != nil {
		return false, fmt.Errorf("procgroup: reading the start time of pid %d: %w", l.PID, err)
	}
	// The leader is read first: while it has not exited it answers alone (pid
	// and start time name this very process, still leading the group), so every
	// poll of a running job costs one record, not a scan of the process table.
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
	// A group whose leader exited while a living member runs on is still
	// current: the kernel does not hand out a pid while a group carries it as
	// its id (Linux keeps it allocated while referenced, XNU skips every live
	// group and session id), so it cannot have been reused. The members are
	// read BEFORE the leader is read again: a pid reissued between the readings
	// shows as a leader with another start time. Not seen through: a recycled
	// pid that became a group leader and exited leaving members, which needs a
	// full pid wrap, a setsid or setpgid, and an early exit between readings.
	proc, found, err = inspect(l.PID)
	if err != nil {
		return false, fmt.Errorf("procgroup: reading the start time of pid %d: %w", l.PID, err)
	}
	if !found {
		return true, nil
	}
	return proc.Start == l.Start, nil
}

// ErrNotMember is a signal [Leader.SignalMember] refused unsent: the pid is no
// living member of the group the leader names, or that group is no longer the
// one recorded.
var ErrNotMember = errors.New("procgroup: not a living member of the recorded process group")

// SignalMember sends sig to ONE process, pid, only while it is a living member
// of the group l names and that group is still the one recorded.
//
// THE PROCESS IS HELD BEFORE IT IS CHECKED, and the signal goes to what was
// held: on Linux a pidfd ([os.FindProcess], from 5.3), which names the very
// process it opened on, so a pid reissued meanwhile reaches nobody. Without
// one (darwin, older Linux, seccomp refusing pidfd_open) the residual is a pid
// reissued in that instant.
//
// Answers as [Leader.Current]'s: sent; [ErrNotMember], definitive; or an
// unreadable record, which licenses no signal. A pid of one or below is
// refused unsent: init, or the engine as a container's first process.
func (l Leader) SignalMember(pid int, sig os.Signal) error {
	if !addressable(pid) {
		return fmt.Errorf("%w: pid %d is no process this package signals", ErrNotMember, pid)
	}
	return signalMember(l, pid, sig)
}

// memberOf reports whether pid, which the caller holds, is a living member of
// the group l names and that group is still the one recorded: [ErrNotMember]
// where it is not, and the kernel's failure where it could not be asked.
//
// IT SERVES A PID THE CALLER DID NOT START. A job's process can tell its
// engine which of its processes to address, so the pid is the job's word and
// can name any process of the engine's user, the engine included. The group
// it is held to is the engine's own record, read from the kernel at start,
// which the job cannot rewrite; a pid not the job's own reaches nobody.
func (l Leader) memberOf(pid int) error {
	proc, found, err := inspect(pid)
	switch {
	case err != nil:
		return fmt.Errorf("procgroup: reading the kernel's record of pid %d: %w", pid, err)
	case !found:
		return fmt.Errorf("%w: pid %d is no process", ErrNotMember, pid)
	case proc.Zombie:
		return fmt.Errorf("%w: pid %d has exited", ErrNotMember, pid)
	case proc.Group != l.PID:
		return fmt.Errorf("%w: pid %d is in process group %d, not %d", ErrNotMember, pid, proc.Group, l.PID)
	}
	// AND THE GROUP IS STILL THE ONE RECORDED: a group id is a pid too, and a
	// group the recorded one ended and a later process now leads under the
	// same id is somebody else's.
	current, err := l.Current()
	if err != nil {
		return err
	}
	if !current {
		return fmt.Errorf("%w: process group %d is no longer the one recorded", ErrNotMember, l.PID)
	}
	return nil
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
