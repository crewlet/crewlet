//go:build !linux && !darwin

package gitproc

import (
	"errors"
	"fmt"
	"runtime"
)

// readNodeMemory refuses: this platform has neither Linux's /proc and cgroup
// files nor Darwin's sysctl hw.memsize, and a budget guessed from nothing is
// the OOM it exists to prevent.
//
// No release target is such a platform: internal/store's platform.go refuses
// to compile off linux and darwin and is the one place the supported set is
// named. This file lets another platform compile this package and reach that
// message, rather than stop here on an undefined function.
func readNodeMemory() (Memory, error) {
	return Memory{}, fmt.Errorf("%s has neither /proc/meminfo nor sysctl hw.memsize: %w",
		runtime.GOOS, errors.ErrUnsupported)
}
