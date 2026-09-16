package procgroup

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"unicode"
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
}

// inspect reads /proc/<pid>/stat; see [procTable.inspect].
func inspect(pid int) (Process, bool, error) { return procfs.inspect(pid) }

// groupLiving scans /proc for a member of pgid that has not exited; see
// [procTable.groupLiving].
func groupLiving(pgid int) (bool, error) { return procfs.groupLiving(pgid) }

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
