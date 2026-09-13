package procgroup

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// parseProcStat reads the two things this package takes from a Linux
// /proc/<pid>/stat line: whether the process is a zombie, and when it started.
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
// parenthesis. Field 3 is the state, where Z is a zombie and X the instant of
// release after a reap that some kernels expose: gone for every purpose a
// caller has.
func parseProcStat(stat, boot string) (Process, error) {
	paren := strings.LastIndexByte(stat, ')')
	if paren < 0 {
		return Process{}, errors.New("no parenthesised command field")
	}
	const stateField, startField = 3, 22
	fields := strings.Fields(stat[paren+1:])
	if len(fields) <= startField-stateField {
		return Process{}, fmt.Errorf("%d fields after the command, want at least %d",
			len(fields), startField-stateField+1)
	}
	ticks := fields[startField-stateField]
	if _, err := strconv.ParseUint(ticks, 10, 64); err != nil {
		return Process{}, fmt.Errorf("starttime %q is not a tick count", ticks)
	}
	state := fields[0]
	return Process{
		Start:  StartTime(boot + ":" + ticks),
		Zombie: state == "Z" || state == "X",
	}, nil
}
