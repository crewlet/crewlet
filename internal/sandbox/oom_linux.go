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
// ([boxOOMScoreAdj] of the engine's), or false where the engine's could not be
// read, said once and leaving every box at the engine's. READ ONCE: whatever
// started the engine set it (the kubelet's QoS class, systemd's
// OOMScoreAdjust=, a runtime's --oom-score-adj) and nothing in the engine
// moves it.
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

// oomFirst is argv started by a shell that first raises its own oom_score_adj
// to [boxOOMScore] and then execs argv, so argv and all it starts run at it.
//
// WRITTEN BY THE PROCESS ITSELF, BEFORE IT EXECS: a value set on a running
// process reaches only what it starts afterwards, so an engine write to the
// child's /proc entry would race its first fork; the exec keeps the pid and
// start time a direct job's record names it by. AHEAD OF ANY CONFINEMENT,
// since Landlock leaves a box nothing writable but its home, and running
// nothing of the box's: a fixed script exec'ing argv's absolute first word
// ([directBox.argv]), so no box PATH is searched. A failed write is said on the
// command's stderr and it runs at the engine's value; raising is never
// refused, so only a host that took /proc away since the engine started fails.
func oomFirst(argv []string) []string {
	score, ok := boxOOMScore()
	if !ok {
		return argv
	}
	raise := "echo " + strconv.Itoa(score) + " >" + selfOOMScoreAdjFile + "; exec \"$@\""
	return append([]string{"/bin/sh", "-c", raise, "sh"}, argv...)
}
