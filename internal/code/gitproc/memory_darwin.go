package gitproc

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// readNodeMemory reads this node's memory: the machine's, from sysctl
// hw.memsize. Darwin has no cgroups, so nothing below the machine limits a
// process; a container a git node runs in on a Mac runs Linux, and reads its
// limit there.
func readNodeMemory() (Memory, error) {
	n, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Memory{}, fmt.Errorf("reading sysctl hw.memsize: %w", err)
	}
	return Memory{Bytes: int64(min(n, math.MaxInt64)), From: "sysctl hw.memsize"}, nil
}
