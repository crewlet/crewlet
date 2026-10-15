package procgroup

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/sys/unix"
)

// procfs is the kernel's own process table; [procTable] holds everything
// done with what it reads, so that runs on every platform's suite too.
var procfs = procTable{
	list: func() ([]string, error) {
		dir, err := os.Open("/proc")
		if err != nil {
			return nil, err
		}
		defer func() { _ = dir.Close() }()
		// Unsorted names and no per-entry stat: the scan runs on every
		// poll of a teardown and needs nothing but the pids.
		return dir.Readdirnames(-1)
	},
	stat: func(pid int) ([]byte, error) {
		return os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	},
	boot: bootID,
	hz:   clockTicks,
}

// inspect reads /proc/<pid>/stat; see [procTable.inspect].
func inspect(pid int) (Process, bool, error) { return procfs.inspect(pid) }

// groupLiving scans /proc for a member of pgid that has not exited; see
// [procTable.groupLiving].
func groupLiving(pgid int) (bool, error) { return procfs.groupLiving(pgid) }

// groupCPU sums the CPU of pgid's members from /proc; see
// [procTable.groupCPU].
func groupCPU(pgid int) (time.Duration, error) { return procfs.groupCPU(pgid) }

// atClockTicks is AT_CLKTCK, the auxiliary vector's entry for the clock ticks
// a second the kernel counts a process's times in for userspace (elf.h; x/sys
// does not export it).
const atClockTicks = 17

// clockTicks is how many ticks make a second in a /proc stat record: the
// kernel's USER_HZ, which it hands every process in its auxiliary vector and
// which is what sysconf(_SC_CLK_TCK) reads. Read rather than assumed to be
// 100: it is 100 on every architecture this engine is released for, and 1024
// on others the kernel supports, and a figure off by ten would be read as a
// node ten times busier than it is. Read once: it is fixed for the kernel's
// life.
var clockTicks = sync.OnceValues(func() (uint64, error) {
	auxv, err := unix.Auxv()
	if err != nil {
		return 0, fmt.Errorf("reading the auxiliary vector for the clock ticks a process's CPU is counted in: %w", err)
	}
	for _, kv := range auxv {
		if kv[0] == atClockTicks && kv[1] > 0 {
			return uint64(kv[1]), nil
		}
	}
	return 0, errors.New("the auxiliary vector holds no clock tick rate (AT_CLKTCK) to count a process's CPU in")
})

// bootID is this boot's identifier, read once: it cannot change while the
// process that read it is running.
var bootID = sync.OnceValues(func() (string, error) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", fmt.Errorf("reading the kernel boot id, which a process start time is only unique within: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	// The token is "<boot>:<ticks>", so the boot half may hold neither the
	// separator nor whitespace.
	if id == "" || strings.ContainsFunc(id, func(r rune) bool { return r == ':' || unicode.IsSpace(r) }) {
		return "", fmt.Errorf("the kernel boot id %q is not a single token", id)
	}
	return id, nil
})
