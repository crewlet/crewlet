package boxhelper_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// A PROCESS RUNS UNTIL ITS LAST THREAD HAS EXITED: the helper reads a stat
// line's state as its main thread's, as internal/procgroup does. Z is the
// process over, awaiting its reap, only where num_threads counts that thread
// alone; with more, its main thread has exited while another runs on, as
// Node's does through its exit, and it runs, its start kept, with no command
// line, the kernel holding none then. X is the reap, as is x on kernels 2.6.33
// to 3.13; R runs. Linux alone: elsewhere running asks ps.
//
// Mutations, each red: read every Z as gone (the row of a main thread exited
// beside another reads gone, as stop then answered while the supervisor ran
// on); answer an empty argv where the kernel gives none (that row has one);
// read x as running (its row runs).
func TestAProcessRunsUntilItsLastThreadHasExited(t *testing.T) {
	t.Parallel()
	const pid, ticks = 4242, 5000
	// stat is pid's stat line in the kernel's layout, as internal/procgroup's
	// suite writes one: state and threads at fields 3 and 20, its start at 22,
	// every other field plausible, so a field counted wrong reads wrong.
	stat := func(state string, threads int) string {
		return fmt.Sprintf("%d (node) %s 1 %d 31 34816 %d 4194560 118 0 0 0 7 3 0 0 20 0 %d 0 %d "+
			"4235264 164 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0\n",
			pid, state, pid, pid+5, threads, ticks)
	}
	argv := []string{"node", "/home/box/.crewlet/browser/helper.mjs", "supervise"}
	cases := []struct {
		name, stat, cmdline string
		alive               bool
		argv                []string
	}{
		{"running", stat("R", 11), strings.Join(argv, "\x00") + "\x00", true, argv},
		{"a main thread exited beside another", stat("Z", 2), "", true, nil},
		{"a zombie", stat("Z", 1), "", false, nil},
		{"reaped", stat("X", 1), "", false, nil},
		{"reaped, on kernels 2.6.33 to 3.13", stat("x", 1), "", false, nil},
	}
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		probe := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium}).Probe(t)
		for _, tc := range cases {
			var got struct {
				Alive   bool     `json:"alive"`
				Argv    []string `json:"argv"`
				Command string   `json:"command"`
				Start   string   `json:"start"`
			}
			probe.Do(map[string]any{"op": "proc", "pid": pid, "stat": tc.stat, "cmdline": tc.cmdline}, &got)
			if got.Alive != tc.alive || got.Command != "" || (got.Argv == nil) != (tc.argv == nil) ||
				!slices.Equal(got.Argv, tc.argv) {
				t.Errorf("%s: the helper reads alive %v with %s, want alive %v with %s",
					tc.name, got.Alive, heldLine(got.Argv, got.Command), tc.alive, heldLine(tc.argv, ""))
			}
			if tc.alive && !strings.HasSuffix(got.Start, fmt.Sprintf(":%d", ticks)) {
				t.Errorf("%s: the helper reads its start as %q, want the boot's and tick %d", tc.name, got.Start, ticks)
			}
		}
	})
}

// heldLine is a command line as the probe answers one, argv or command, in
// words: none where it answers neither, as for a process gone or one the
// kernel holds no command line for.
func heldLine(argv []string, command string) string {
	if argv == nil && command == "" {
		return "no command line"
	}
	return fmt.Sprintf("argv %q and command %q", argv, command)
}
