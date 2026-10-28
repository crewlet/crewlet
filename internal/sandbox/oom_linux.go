//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// selfOOMScoreAdjFile is where the kernel keeps the oom_score_adj of whichever
// process reads or writes it: the engine's for the engine, a shell's own for
// the shell.
const selfOOMScoreAdjFile = "/proc/self/oom_score_adj"

// boxOOMScore is the oom_score_adj every box on this host runs at
// ([boxOOMScoreAdj] of the engine's), and false where the engine's own could
// not be read, which is said once and leaves every box at the engine's.
//
// READ ONCE: whatever started the engine set its value before it ran — the
// kubelet for its pod's QoS class, systemd's OOMScoreAdjust=, a container
// runtime's --oom-score-adj — and nothing in the engine moves it.
var boxOOMScore = sync.OnceValues(func() (int, bool) {
	engine, err := readOOMScoreAdj(selfOOMScoreAdjFile)
	if err != nil {
		localLog.Warn("local_sandbox_oom_score_unread",
			"error", err.Error(),
			"consequence", "every box's processes run at the engine's own oom_score_adj, "+
				"so under memory pressure the kernel may kill the engine before a box")
		return 0, false
	}
	return boxOOMScoreAdj(engine), true
})

// readOOMScoreAdj is the oom_score_adj the kernel reports at path.
func readOOMScoreAdj(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading the engine's oom_score_adj: %w", err)
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, fmt.Errorf("reading the engine's oom_score_adj from %s: %w", path, err)
	}
	return value, nil
}

// oomFirst is argv started by a shell that first raises its own
// oom_score_adj to the box's ([boxOOMScore]) and then becomes argv, so argv
// and everything it ever starts run at it.
//
// WRITTEN BY THE PROCESS ITSELF, BEFORE IT EXECS: a value set on a running
// process reaches only what that process starts afterwards, so a write the
// engine made to the child's /proc entry after starting it would race the
// child's first fork. The exec keeps the pid and the kernel start time, which
// are what a direct job's record names it by.
//
// AHEAD OF ANY CONFINEMENT: a confined box's argv starts with the
// confinement's helper, and under Landlock the box may write nothing but its
// home, /proc included. So the raise is made by the one process that runs
// before the confinement does, and it runs nothing of the box's: a fixed
// script that execs argv's first word, an absolute path ([directBox.argv]),
// so no PATH the box's environment sets is searched.
//
// A WRITE THAT FAILS is said on the command's stderr and the command runs at
// the engine's value, as every box once did. Raising its own value is never
// refused by the kernel and the engine read its own from the same /proc, so
// only a host that took /proc away after the engine started gets there.
func oomFirst(argv []string) []string {
	score, ok := boxOOMScore()
	if !ok {
		return argv
	}
	raise := "echo " + strconv.Itoa(score) + " >" + selfOOMScoreAdjFile + "; exec \"$@\""
	return append([]string{"/bin/sh", "-c", raise, "sh"}, argv...)
}
