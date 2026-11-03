//go:build !linux && !darwin

package gitproc

import (
	"errors"
	"fmt"
	"runtime"
)

// readNodeMemory refuses: this platform has neither the Linux /proc and
// cgroup files nor Darwin's sysctl hw.memsize to read a node's memory from,
// and a budget guessed from nothing is the OOM it exists to prevent.
//
// No release target is on such a platform — internal/store refuses to compile
// off linux and darwin (its platform.go), and that refusal is the one place
// the supported set is named. This file exists so a build for another
// platform COMPILES THIS PACKAGE and reaches that message, rather than
// stopping here on an undefined function, which says nothing about why.
func readNodeMemory() (Memory, error) {
	return Memory{}, fmt.Errorf("%s has neither /proc/meminfo nor sysctl hw.memsize: %w",
		runtime.GOOS, errors.ErrUnsupported)
}
