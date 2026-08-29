package mcp

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestALeftoverTreeIsReaped is the process-group half of child supervision.
//
// An MCP server is very often launched through a package runner — uvx, npx —
// which forks the real server underneath itself. The transport's shutdown
// signals only the process it started, so the runner's child survives, holds
// the inherited stderr open, and goes on holding whatever the server held.
func TestALeftoverTreeIsReaped(t *testing.T) {
	t.Parallel()
	log, rec := recorder()
	spec := helperSpec(t, "runner", "spawn-grandchild", nil)
	c, err := connect(t.Context(), spec, log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}

	grandchild := waitForGrandchildPID(t, c)

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := c.stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	elapsed := time.Since(start)
	// Expect roughly: the child's own exit (see the harness floor noted on
	// TestMeasuredStdioTimings) + the full drain window, because the
	// grandchild holds the descriptor through it + a fast second drain once
	// the group is signalled. A second drain that ran the full grace instead
	// would mean the kill is not landing.
	t.Logf("stop with a leftover grandchild: %s (drain window %s + reap grace %s)",
		elapsed, stderrDrainTimeout, stderrReapGrace)

	if !rec.has("server_tree_reaped") {
		t.Fatal("the tree was never signalled: the grandchild outlives the engine")
	}
	// The pipe reaching EOF is what proves the grandchild actually died —
	// nothing else was holding the descriptor. If we had merely given up, the
	// pump would have been force-closed instead.
	if rec.has("server_stderr_reader_forced") {
		t.Fatal("the read end had to be closed from under the pump: the tree did not let go")
	}
	assertProcessGone(t, grandchild)

	if ceiling := stderrDrainTimeout + stderrReapGrace + 5*time.Second; elapsed > ceiling {
		t.Fatalf("stop took %s, ceiling %s", elapsed, ceiling)
	}
}

// TestAnOrdinaryStopDoesNotSignalTheGroup is the CONTROL for the test above.
//
// The group kill runs only on evidence that the tree is still there, because
// signalling a group whose leader has been reaped is a theoretical way to hit
// a recycled pid. A server that shuts down cleanly must produce no such
// evidence — if this ever starts logging, the "evidence" gate has become an
// unconditional kill and the test above proves nothing.
func TestAnOrdinaryStopDoesNotSignalTheGroup(t *testing.T) {
	t.Parallel()
	log, rec := recorder()
	c, err := connect(t.Context(), helperSpec(t, "tidy", "serve", nil), log)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := c.listTools(t.Context()); err != nil {
		t.Fatalf("listTools: %v", err)
	}
	if err := c.stop(t.Context()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if rec.has("server_tree_reaped") {
		t.Fatal("a clean shutdown signalled the process group anyway")
	}
	if rec.has("server_stderr_reader_forced") {
		t.Fatal("a clean shutdown had to force the stderr pump closed")
	}
}

// TestMeasuredStdioTimings records what the constants actually cost, so the
// numbers in timeouts.go stay attached to something real.
//
// It asserts only the ceilings it can defend; the values it prints are the
// evidence a future reader needs to re-tune them.
//
// # A FLOOR IN THE INSTRUMENT, not in the code under test
//
// The helper server is this test binary re-executed, so under -race it is
// RACE-INSTRUMENTED, and a race-instrumented Go binary takes ~1.00s from
// stdin-EOF to process exit. Measured against a control on the same box:
// `cat` exits in ~320µs, the helper in ~1.005s, with no MCP machinery in the
// path at all — a plain exec, a plain pipe, a plain Wait.
//
// So every "clean stop" figure printed under -race carries a ~1s floor that
// belongs to the harness. Without -race the same stop measures ~1ms. Do not
// tune shutdownGrace, stderrDrainTimeout or anything else against these
// numbers without subtracting it.
func TestMeasuredStdioTimings(t *testing.T) {
	t.Parallel()

	t.Run("a well-behaved server", func(t *testing.T) {
		t.Parallel()
		start := time.Now()
		c, err := connect(t.Context(), helperSpec(t, "quick", "serve", nil), discardLogger())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		connected := time.Since(start)

		start = time.Now()
		if _, err := c.listTools(t.Context()); err != nil {
			t.Fatalf("listTools: %v", err)
		}
		listed := time.Since(start)

		start = time.Now()
		if err := c.stop(t.Context()); err != nil {
			t.Fatalf("stop: %v", err)
		}
		stopped := time.Since(start)

		t.Logf("spawn+handshake %s, tools/list %s, stop %s (the stop carries the "+
			"harness's ~1s race-build exit floor; see the doc comment)",
			connected, listed, stopped)
		// A clean stop must not pay the shutdown ladder at all: the child
		// exits on stdin close. If this starts costing shutdownGrace, the
		// child has stopped watching its stdin and every drain in the fleet
		// just got slower. The instrument's ~1s floor sits comfortably under
		// that, so the assertion still means what it says.
		if stopped > shutdownGrace {
			t.Fatalf("a clean stop took %s, which is the whole first rung of the ladder", stopped)
		}
	})

	t.Run("a server that must be killed", func(t *testing.T) {
		t.Parallel()
		c, err := connect(t.Context(), helperSpec(t, "stubborn-solo", "stubborn", nil), discardLogger())
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		if _, err := c.listTools(t.Context()); err != nil {
			t.Fatalf("listTools: %v", err)
		}
		start := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.stop(ctx); err != nil {
			t.Fatalf("stop: %v", err)
		}
		elapsed := time.Since(start)
		t.Logf("stop of a server that ignores stdin-close AND SIGTERM: %s (one rung %s)",
			elapsed, shutdownGrace)
		// Two rungs: the wait after stdin closes, then the wait after the
		// ignored SIGTERM. A third would mean SIGKILL is not landing.
		if ceiling := 3 * shutdownGrace; elapsed > ceiling {
			t.Fatalf("stop took %s, ceiling %s: SIGKILL is not ending it", elapsed, ceiling)
		}
	})
}

func waitForGrandchildPID(t *testing.T, c *client) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range c.stderrTail() {
			if rest, ok := strings.CutPrefix(line, "GRANDCHILD "); ok {
				pid, err := strconv.Atoi(strings.TrimSpace(rest))
				if err != nil {
					t.Fatalf("unreadable grandchild pid %q: %v", rest, err)
				}
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the grandchild never announced itself on the inherited stderr")
	return 0
}

// assertProcessGone checks /proc where it exists.
//
// It deliberately does NOT use kill(pid, 0): a ZOMBIE answers that as alive,
// and a reparented grandchild is exactly the kind of process that can be one.
// Where /proc is absent the pipe-EOF evidence in the caller stands alone, and
// this says nothing rather than guessing.
func assertProcessGone(t *testing.T, pid int) {
	t.Helper()
	if _, err := os.Stat("/proc/self"); err != nil {
		t.Logf("no /proc: relying on the stderr-EOF evidence alone for pid %d", pid)
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		state, alive := procState(pid)
		if !alive || state == "Z" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	state, _ := procState(pid)
	t.Fatalf("grandchild %d is still running (state %q) after the group was reaped", pid, state)
}

func procState(pid int) (state string, exists bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "", false
	}
	// The comm field is parenthesised and may contain spaces; the state is
	// the first field after the closing paren.
	idx := strings.LastIndex(string(raw), ") ")
	if idx < 0 {
		return "", true
	}
	fields := strings.Fields(string(raw)[idx+2:])
	if len(fields) == 0 {
		return "", true
	}
	return fields[0], true
}
