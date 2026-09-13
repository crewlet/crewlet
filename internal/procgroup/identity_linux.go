package procgroup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

// inspect reads /proc/<pid>/stat; see [parseProcStat] for what it takes from
// it and why.
//
// A missing entry is a process that does not exist, and so is ESRCH, which a
// read reports when the process is reaped between the open and the read.
func inspect(pid int) (Process, bool, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return Process{}, false, nil
	}
	if err != nil {
		return Process{}, false, err
	}
	boot, err := bootID()
	if err != nil {
		return Process{}, false, err
	}
	proc, err := parseProcStat(string(raw), boot)
	if err != nil {
		return Process{}, false, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	}
	return proc, true, nil
}

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
