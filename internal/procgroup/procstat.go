package procgroup

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"time"
)

// parseProcStat reads the three things this package takes from a Linux
// /proc/<pid>/stat line: whether the process has exited, which process group
// it belongs to, and when it started.
//
// Portable, so the parse runs on every platform the suite runs on.
//
// The start time is field 22, `starttime`: ticks since boot, fixed at fork,
// paired with the boot id because ticks restart at every boot and a box
// directory outlives one. Wall-clock forms (btime plus ticks) are refused:
// btime derives from the current clock, so a clock step would unmatch a live
// job's token. Field 5, `pgrp`, is how [procTable.groupLiving] tells a
// group's members from other entries.
func parseProcStat(stat, boot string) (Process, error) {
	const groupField, threadsField, startField = 5, 20, 22
	fields, err := statFields(stat, startField)
	if err != nil {
		return Process{}, err
	}
	ticks := fields[startField-stateField]
	if _, err = strconv.ParseUint(ticks, 10, 64); err != nil {
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
	// Field 3 is the state of the MAIN THREAD, not of the process. Z is an
	// exited main thread awaiting a reap, X the instant of release after one
	// (x between kernels 2.6.33 and 3.13). A main thread that called
	// pthread_exit while other threads run on reads Z as long as they do, and
	// that job is not over. Field 20, num_threads, tells them apart: every
	// thread but the main one is released as it exits, so a Z line counting one
	// thread is a real zombie. Reading every Z as a zombie would answer a
	// running job as finished, and the sandbox deletes a finished job's checkout.
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

// stateField is the number proc(5) gives the state, the first field after the
// command: field n of a stat line is element n-stateField of [statFields].
const stateField = 3

// statFields is the fields of a /proc/<pid>/stat line after its command,
// counted from the LAST closing parenthesis, refusing a line too short to
// hold field last.
//
// Field 2, the command, is parenthesised and may hold spaces and parentheses.
// A process sets its own command name, so one built to look like the fields
// after it (") Z 1 7 7") is input, and counting from the first parenthesis
// would let a process forge its state and its group.
func statFields(stat string, last int) ([]string, error) {
	paren := strings.LastIndexByte(stat, ')')
	if paren < 0 {
		return nil, errors.New("no parenthesised command field")
	}
	fields := strings.Fields(stat[paren+1:])
	if len(fields) <= last-stateField {
		return nil, fmt.Errorf("%d fields after the command, want at least %d", len(fields), last-stateField+1)
	}
	return fields, nil
}

// parseProcCPU reads the CPU a /proc/<pid>/stat line records, in clock ticks,
// and the process group it belongs to (field 5, as [parseProcStat] reads it).
//
// The CPU is fields 14 to 17: utime and stime, the process's own threads
// (an exited thread's folded in), and cutime and cstime, every child it has
// waited for, which carry their own reaped children's. So a sum over a group
// counts each process once: a child running, or exited and unreaped, is in
// its own record and not yet its parent's; one reaped is in its parent's and
// has no record. cutime and cstime print signed but are never negative, so a
// line saying otherwise is not the kernel's and is refused.
func parseProcCPU(stat string) (group int, ticks uint64, err error) {
	const groupField, utimeField, cstimeField = 5, 14, 17
	fields, err := statFields(stat, cstimeField)
	if err != nil {
		return 0, 0, err
	}
	group, err = strconv.Atoi(fields[groupField-stateField])
	if err != nil {
		return 0, 0, fmt.Errorf("pgrp %q is not a process group id", fields[groupField-stateField])
	}
	for f := utimeField; f <= cstimeField; f++ {
		n, err := strconv.ParseUint(fields[f-stateField], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("field %d, %q, is not a tick count", f, fields[f-stateField])
		}
		ticks += n
	}
	return group, ticks, nil
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
	// hz is how many clock ticks make a second, the unit a stat record's
	// times are counted in.
	hz func() (uint64, error)
}

// record reads one process's raw stat record.
//
// A missing entry is a process that does not exist, and so is ESRCH, which a
// read reports when the process is reaped between the open and the read.
// Every other failure is "could not ask", which [Inspect] promises its caller
// never to report as absence.
func (t procTable) record(pid int) ([]byte, bool, error) {
	raw, err := t.stat(pid)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, errNoProcess) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return raw, true, nil
}

// inspect reads one process's stat record (see [procTable.record] for which
// failure is absence); see [parseProcStat] for what it takes from it and why.
func (t procTable) inspect(pid int) (Process, bool, error) {
	raw, found, err := t.record(pid)
	if err != nil || !found {
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
// Linux has no per-group listing, so each numeric /proc entry is read through
// [procTable.inspect]; an exited member (see [parseProcStat]) does not count.
//
// An entry vanishing between listing and read is a reaped process, not an
// error. An unlistable /proc, an unparsable record, and an unreadable one that
// could change the answer ARE: false from here is definitive. Under hidepid=1
// another user's entries are unreadable and may be a living member (a setuid
// helper); once one is found they cannot matter, until then they separate
// "over" from "could not tell". hidepid=2 omits them, so nothing is asked.
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

// groupCPU is the CPU the members of process group pgid have spent so far,
// summed over every process's stat record (see [parseProcCPU]). Members are
// found as [procTable.groupLiving] finds them, an exited one awaiting its reap
// included: its time is in no parent's record yet.
//
// A FIGURE THAT IS SHORT, NEVER ONE THAT IS LONG. A record that cannot be
// read or parsed is reported beside the sum of every one that could, not
// ending the scan: the caller keeps the most any reading found. A member
// reaped mid-scan moves its time into its reaper's record, which may already
// have been read, so one process's figure can be missed. A table that cannot
// be listed, or a clock with no tick rate, gives nothing.
func (t procTable) groupCPU(pgid int) (time.Duration, error) {
	hz, err := t.hz()
	if err != nil {
		return 0, err
	}
	if hz == 0 {
		return 0, errors.New("the kernel gave a clock of no ticks a second to count a stat record's times in")
	}
	names, err := t.list()
	if err != nil {
		return 0, fmt.Errorf("listing /proc to find the members of the group: %w", err)
	}
	var (
		ticks      uint64
		unreadable int
		failed     error
	)
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		raw, found, err := t.record(pid)
		switch {
		case errors.Is(err, fs.ErrPermission):
			unreadable++
			continue
		case err != nil:
			failed = cmp.Or(failed, err)
			continue
		case !found:
			continue
		}
		group, n, err := parseProcCPU(string(raw))
		if err != nil {
			failed = cmp.Or(failed, fmt.Errorf("/proc/%d/stat: %w", pid, err))
			continue
		}
		if group == pgid {
			ticks += n
		}
	}
	spent := time.Duration(ticks/hz)*time.Second + time.Duration(ticks%hz)*time.Second/time.Duration(hz)
	if failed != nil {
		return spent, fmt.Errorf("reading the CPU of process group %d's members: %w", pgid, failed)
	}
	if unreadable > 0 {
		return spent, fmt.Errorf("%d process records under /proc could not be read and any of them may be a "+
			"member of process group %d, whose CPU is then not counted: mount /proc without hidepid=1 "+
			"(hidepid=2 hides them instead) or run the engine with access to them", unreadable, pgid)
	}
	return spent, nil
}
