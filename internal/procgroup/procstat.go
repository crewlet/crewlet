package procgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
)

// parseProcStat reads the three things this package takes from a Linux
// /proc/<pid>/stat line: whether the process has exited, which process group
// it belongs to, and when it started.
//
// Portable, although only the Linux build reads a real one, so the parse is
// exercised on every platform the suite runs on rather than only in CI.
//
// The start time is field 22, `starttime`: clock ticks since boot, fixed at
// fork and kept until the process is reaped. It is paired with the kernel's
// boot id, because a tick count restarts at every boot and a box directory
// outlives one: a service that starts at the same pid and tick on every boot
// would otherwise match a job recorded before the reboot. Wall-clock forms
// (btime plus ticks) are refused for the opposite reason: btime is derived
// from the current clock, so a clock step would make a live job's own token
// stop matching it.
//
// Field 2, the command, is parenthesised and may itself contain spaces and
// parentheses, so the remaining fields are counted from the LAST closing
// parenthesis. A process sets its own command name, so a name built to look
// like the fields after it (") Z 1 7 7") is input, and counting from the first
// parenthesis would let a process forge its state and its group. Field 5 is
// `pgrp`, the process group id, which is how [procTable.groupLiving] tells the
// members of a group apart from every other entry under /proc.
//
// Field 3 is the state, and it is the state of the process's MAIN THREAD, not
// of the process. Z is an exited main thread awaiting a reap, X the instant of
// release after one (spelled x by the kernels between 2.6.33 and 3.13). A
// process whose main thread has called pthread_exit while its other threads
// run on reads Z for as long as they do, and that process is not over: its
// threads hold the job's work. Field 20, `num_threads`, tells the two apart.
// The kernel counts a thread there until it is released, and every thread but
// the main one is released the moment it exits, so a Z line counting one
// thread is the main thread alone (a real zombie) and one counting more still
// has a thread that has not exited. Reading every Z as a zombie would answer a
// running job as finished, and the sandbox deletes a finished job's checkout.
func parseProcStat(stat, boot string) (Process, error) {
	paren := strings.LastIndexByte(stat, ')')
	if paren < 0 {
		return Process{}, errors.New("no parenthesised command field")
	}
	const stateField, groupField, threadsField, startField = 3, 5, 20, 22
	fields := strings.Fields(stat[paren+1:])
	if len(fields) <= startField-stateField {
		return Process{}, fmt.Errorf("%d fields after the command, want at least %d",
			len(fields), startField-stateField+1)
	}
	ticks := fields[startField-stateField]
	if _, err := strconv.ParseUint(ticks, 10, 64); err != nil {
		return Process{}, fmt.Errorf("starttime %q is not a tick count", ticks)
	}
	group, err := strconv.Atoi(fields[groupField-stateField])
	if err != nil {
		return Process{}, fmt.Errorf("pgrp %q is not a process group id", fields[groupField-stateField])
	}
	threads, err := strconv.ParseUint(fields[threadsField-stateField], 10, 32)
	if err != nil {
		return Process{}, fmt.Errorf("num_threads %q is not a thread count", fields[threadsField-stateField])
	}
	var exited bool
	switch fields[0] {
	case "X", "x":
		exited = true
	case "Z":
		exited = threads <= 1
	}
	return Process{
		Start:  StartTime(boot + ":" + ticks),
		Group:  group,
		Zombie: exited,
	}, nil
}

// procTable is the Linux process table as this package reads it: a listing
// of /proc, one stat record per process, and the boot the records belong to.
//
// The reads are fields rather than direct calls so that what is done with
// them (which failure means "no such process", which means "could not ask",
// and which entries are members of a group) runs in every suite on every
// platform against a table the test builds, rather than only on a Linux CI
// runner. The Linux build wires it to the real /proc (see identity_linux.go).
type procTable struct {
	// list names the entries of /proc.
	list func() ([]string, error)
	// stat reads /proc/<pid>/stat.
	stat func(pid int) ([]byte, error)
	// boot is the kernel's boot id.
	boot func() (string, error)
}

// inspect reads one process's stat record; see [parseProcStat] for what it
// takes from it and why.
//
// A missing entry is a process that does not exist, and so is ESRCH, which a
// read reports when the process is reaped between the open and the read.
// Every other failure is "could not ask", which [Inspect] promises its caller
// never to report as absence.
func (t procTable) inspect(pid int) (Process, bool, error) {
	raw, err := t.stat(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNoProcess) {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	boot, err := t.boot()
	if err != nil {
		return Process{}, false, err
	}
	proc, err := parseProcStat(string(raw), boot)
	if err != nil {
		return Process{}, false, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	}
	return proc, true, nil
}

// groupLiving reports whether any member of process group pgid has not
// exited, reading every process's stat record.
//
// Linux has no per-group listing, so the members are found by scanning: each
// numeric entry under /proc is one process, read through [procTable.inspect]
// so its group, state and parse are exactly the ones every other answer in
// this package rests on. A member that has exited (see [parseProcStat]) does
// not count. The listing names processes only, never their other threads
// (those live under /proc/<pid>/task), so a thread cannot be counted as a
// member of its own, and a process whose main thread alone has exited is
// counted by its thread count rather than its state.
//
// The scan races the processes it reads, and that race is not an error. An
// entry that vanishes between the listing and its read is a process that has
// been reaped, which is no member of anything. /proc itself not listing IS an
// error, because false from here is definitive and an unread process table
// proves nothing. So is a record that does not parse: it is the kernel's own
// format, and a table this package cannot read is one it cannot vouch for.
//
// So is a record the kernel refuses to show, but only when it could change
// the answer. Under a hidepid=1 mount another user's entries are listed and
// unreadable, and any one of them may be a living member (a setuid helper
// started inside the job, for instance). Once a living member has been found
// they cannot matter; while none has, they are the difference between "over"
// and "could not tell", and the caller is told which it got. hidepid=2 omits
// those entries from the listing altogether, which is the kernel declaring
// them no business of this user, and there is nothing further to ask.
func (t procTable) groupLiving(pgid int) (bool, error) {
	names, err := t.list()
	if err != nil {
		return false, fmt.Errorf("listing /proc to find the members of the group: %w", err)
	}
	unreadable := 0
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		proc, found, err := t.inspect(pid)
		if errors.Is(err, fs.ErrPermission) {
			unreadable++
			continue
		}
		if err != nil {
			return false, err
		}
		if found && proc.Group == pgid && !proc.Zombie {
			return true, nil
		}
	}
	if unreadable > 0 {
		return false, fmt.Errorf("%d process records under /proc could not be read and any of them may be a "+
			"running member of the group: mount /proc without hidepid=1 (hidepid=2 hides them instead) or run "+
			"the engine with access to them", unreadable)
	}
	return false, nil
}
