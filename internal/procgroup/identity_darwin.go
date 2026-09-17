package procgroup

import (
	"errors"
	"fmt"
	"strconv"

	"golang.org/x/sys/unix"
)

// inspect reads the process's kinfo_proc through sysctl kern.proc.pid.
//
// The start time is p_starttime, the wall-clock instant XNU stamped when the
// process was forked. It is stored in the process record rather than derived
// from the current clock, so it does not move when the clock is stepped, and
// being an absolute time it cannot repeat across reboots the way a tick count
// can. The token is its microseconds.
//
// A pid with no process answers the sysctl with an empty buffer, which the
// x/sys wrapper reports as EIO because the length is not a kinfo_proc's. No
// other outcome of this call produces EIO (the kernel returns either nothing or
// the whole record), so EIO is read as "no such process" rather than as a
// failure. A zombie keeps its record, and its p_stat says so.
func inspect(pid int) (Process, bool, error) {
	kinfo, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if errors.Is(err, unix.EIO) {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	started := kinfo.Proc.P_starttime
	micros := started.Sec*1_000_000 + int64(started.Usec)
	return Process{
		Start:  StartTime(strconv.FormatInt(micros, 10)),
		Group:  int(kinfo.Eproc.Pgid),
		Zombie: kinfo.Proc.P_stat == sZomb,
	}, true, nil
}

// groupLiving reports whether any member of process group pgid can still run,
// reading the group's members through sysctl kern.proc.pgrp.
//
// The same kinfo_proc record [inspect] reads, so "a zombie" means one thing in
// this package on this platform. The kernel filters by group itself and lists
// every user's processes, so there is no record here the engine may not read.
// The group id is compared again anyway: the answer licenses keeping or
// dropping a job's state, and a record that is not the group's must not
// decide it. An empty listing is a group with no members at all, which the
// wrapper reports as a nil slice rather than an error.
func groupLiving(pgid int) (bool, error) {
	members, err := unix.SysctlKinfoProcSlice("kern.proc.pgrp", pgid)
	if err != nil {
		return false, fmt.Errorf("sysctl kern.proc.pgrp for group %d: %w", pgid, err)
	}
	for i := range members {
		if int(members[i].Eproc.Pgid) == pgid && members[i].Proc.P_stat != sZomb {
			return true, nil
		}
	}
	return false, nil
}

// sZomb is XNU's SZOMB process state (sys/proc.h): exited and awaiting a
// wait from its parent. x/sys/unix does not export the proc states.
const sZomb = 5
