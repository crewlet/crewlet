package engine_test

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/seat"
	"github.com/crewlet/crewlet/internal/tools"
)

// The engine's half of keeping an MCP server alive, against real children
// that die: the bridge restarts them (internal/mcp has those cases), and
// what is the engine's is that a seat's turns reach the server again, that a
// shared server whose start failed reaches every seat once the bridge brings
// it up — with no config apply in between — and that the heartbeat says what
// the server is doing now rather than what its start concluded.

// withServerEnv adds variables to every `env:` block of an mcpCompany
// document, which is the environment the engine launches the server with.
func withServerEnv(doc string, env map[string]string) string {
	keys := slices.Sorted(func(yield func(string) bool) {
		for k := range env {
			if !yield(k) {
				return
			}
		}
	})
	var lines strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&lines, "      %s: %q\n", k, env[k])
	}
	return strings.ReplaceAll(doc, "    env:\n", "    env:\n"+lines.String())
}

// statusOf is the heartbeat's row for one server, if it has one.
func statusOf(e *engine.Engine, server string) (coord.MCPServerStatus, bool) {
	for _, row := range engine.MCPStatus(e) {
		if row.Server == server {
			return row, true
		}
	}
	return coord.MCPServerStatus{}, false
}

// within polls cond until it holds or the deadline passes. Long, because a
// restart is a real spawn and handshake under the race detector; a case that
// is fast when it works pays nothing for it.
func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// call runs one of a seat's tools as its turn would.
func call(t *testing.T, e *engine.Engine, handle, tool string) tools.Result {
	t.Helper()
	entry, ok := e.ToolsFor(handle).Lookup(tool)
	if !ok {
		t.Fatalf("seat %q has no tool %q; surface = %v", handle, tool, surfaceOf(t, e, handle))
	}
	res, err := entry.Tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("calling %s: %v", tool, err)
	}
	return res
}

// A SHARED SERVER THAT DIES IS BROUGHT BACK, with nobody editing the config.
// While it is down, every seat that calls it is refused in the engine's own
// words, classed unavailable — never a broken pipe — and the heartbeat says
// it failed, with the child's exit status; once it is back the seats' calls
// work again through the very tool objects their registries already hold, and
// the heartbeat says it started.
func TestASharedServerThatDiesIsBroughtBackAndReportedWhileDown(t *testing.T) {
	t.Parallel()
	starts := t.TempDir()
	// The restart at once, and its first retry, fail: launch 4 is the one
	// that comes back, so there is an outage long enough to read.
	doc := withServerEnv(mcpCompany(true, "PATH"), map[string]string{
		toolServerLaunchesEnv:      starts,
		toolServerCrashLaunchesEnv: "1",
		toolServerFailLaunchesEnv:  "2,3",
	})
	e := newEngine(t, engine.WithMCPRetryBase(
		engine.Options{Company: parsedCompany(t, doc)}, 300*time.Millisecond))

	held, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
	if !ok {
		t.Fatal("the boot epoch has no shared tool")
	}
	if res := call(t, e, "ceo", "tracker_probe"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}

	crash := "exit status " + strconv.Itoa(toolServerCrashCode)
	within(t, "the heartbeat to report the server down", func() bool {
		row, ok := statusOf(e, "tracker")
		return ok && row.Failed == 1 && row.Started == 0 && strings.Contains(row.Error, crash)
	})
	for _, handle := range []string{"ceo", "cto"} {
		res := call(t, e, handle, "tracker_probe")
		if !res.Failed {
			// Back already: the outage was shorter than this read.
			continue
		}
		if tools.RefusalOf(res) != tools.RefusalUnavailable || strings.Contains(res.Output, "broken pipe") {
			t.Errorf("seat %q was answered %+v, want the engine's refusal classed unavailable",
				handle, res)
		}
	}

	within(t, "the server to be back", func() bool {
		return !call(t, e, "ceo", "tracker_probe").Failed
	})
	if n := launches(t, starts); n != 4 {
		t.Errorf("the server was started %d times, want the first, two that failed and one that came up", n)
	}
	if row, _ := statusOf(e, "tracker"); row.Started != 1 || row.Failed != 0 || row.Tools != 1 {
		t.Errorf("heartbeat after the restart = %+v, want one started instance serving its tool", row)
	}
	if now, _ := e.ToolsFor("cto").Lookup("tracker_probe"); now.Tool != held.Tool {
		t.Error("the restart replaced a tool whose listing did not change")
	}
}

// A SHARED SERVER WHOSE START FAILED REACHES EVERY SEAT WHEN IT COMES UP —
// without an apply. Before, the next config apply was the only thing that ever
// asked again, so a vendor that was down at boot cost the company that
// server's tools until somebody edited the config. A seat that holds a
// surface of its own (it has a per-role server) gets the shared tool refiled
// into it too, because that surface was cloned from the epoch's before the
// shared server was up.
//
// THE CASE DECIDES WHEN THE VENDOR COMES UP, not a timer: the shared server
// refuses every start until its ready file exists, which the case writes only
// once both seats are claimed and the CEO's surface is known to lack the
// shared tool. A retry schedule long enough to outlast the claims would be a
// race against the engine's boot, which a loaded runner loses.
func TestASharedServerWhoseStartFailedReachesEverySeatWhenItComesUp(t *testing.T) {
	t.Parallel()
	starts := t.TempDir()
	ready := filepath.Join(t.TempDir(), "vendor-is-up")
	doc := fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    shared: true
    command: %[1]q
    tool_prefix: "search_"
    env:
      %[2]s: "server"
      %[3]s: %[4]q
      %[5]s: %[6]q
      %[7]s: %[8]q
  - name: tracker
    shared: false
    command: %[1]q
    tool_prefix: "tracker_"
    env:
      %[2]s: "server"
      %[7]s: %[8]q
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-secret"
  - name: CTO
    handle: cto
    llm: zulu
`, os.Args[0], toolServerModeEnv, toolServerLaunchesEnv, starts, toolServerReadyEnv, ready,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
	// Short: every retry before the file appears is refused, so the schedule
	// only bounds how soon after it the server comes up.
	e := newEngine(t, engine.WithMCPRetryBase(
		engine.Options{Company: parsedCompany(t, doc)}, 100*time.Millisecond))

	if row, ok := statusOf(e, "search"); !ok || row.Failed != 1 || !strings.Contains(row.Error, "would not start") {
		t.Fatalf("heartbeat after the failed start = %+v, want it failed with why", row)
	}
	claimed(t, e, 2)
	if names := surfaceOf(t, e, "ceo"); slices.Contains(names, "search_probe") ||
		!slices.Contains(names, "tracker_probe") {
		t.Fatalf("precondition: the CEO's own surface should hold its per-role tool and not "+
			"yet the shared one: %v", names)
	}
	within(t, "a retry the vendor refused", func() bool { return launches(t, starts) >= 2 })

	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatalf("bring the vendor up: %v", err)
	}
	within(t, "the retried start to reach the CEO's own surface", func() bool {
		return slices.Contains(surfaceOf(t, e, "ceo"), "search_probe")
	})
	if !slices.Contains(e.Company().Tools.Names(), "search_probe") {
		t.Error("the epoch's registry never got the retried server's tool")
	}
	if !slices.Contains(surfaceOf(t, e, "cto"), "search_probe") {
		t.Error("a seat on the epoch's surface never got the retried server's tool")
	}
	if !slices.Contains(surfaceOf(t, e, "ceo"), "tracker_probe") {
		t.Error("refiling the CEO's surface lost its own per-role tool")
	}
	if res := call(t, e, "ceo", "search_probe"); res.Failed {
		t.Errorf("the retried server's tool does not answer: %q", res.Output)
	}
	if row, _ := statusOf(e, "search"); row.Started != 1 || row.Failed != 0 {
		t.Errorf("heartbeat after the retry = %+v, want it started", row)
	}
}

// AN APPLY WHILE A SHARED SERVER IS DOWN KEEPS ITS TOOLS LISTED, IN THE ORDER
// AN APPLY WHILE IT IS UP WOULD. The apply builds a fresh registry and the
// server it reconciles is down; the reconcile says so, and hands back the tools
// the server still lists, which the apply files beside the error in spec order.
// They refuse honestly until the server is back, and when it is, nothing
// refiles them, because a restart that lists the same tools is no change. Left
// out, the new epoch would never offer them again; appended once the epoch is
// published, the same revision would render its catalogue in another order
// whenever a server happened to be down at the apply — and a registry's order
// is the order a prompt lists its tools in.
func TestAnApplyWhileASharedServerIsDownKeepsItsToolsListed(t *testing.T) {
	t.Parallel()
	// Two shared servers, the first in spec order the one that goes down,
	// so a tool filed late would land after the second's. The one that goes
	// down fronts a vendor the case takes away before the call it crashes
	// on and gives back only once the apply is done, so the server is down
	// through the whole apply however long it takes: every restart, the one
	// the apply asks for included, is refused until then.
	ready := filepath.Join(t.TempDir(), "vendor-is-up")
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatalf("bring the vendor up for the boot: %v", err)
	}
	doc := fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: tracker
    shared: true
    command: %[1]q
    tool_prefix: "tracker_"
    env:
      %[2]s: "server"
      %[3]s: %[4]q
      %[5]s: "1"
      %[6]s: %[7]q
      %[8]s: %[9]q
  - name: search
    shared: true
    command: %[1]q
    tool_prefix: "search_"
    env:
      %[2]s: "server"
      %[8]s: %[9]q
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: CTO
    handle: cto
    llm: zulu
`, os.Args[0], toolServerModeEnv, toolServerLaunchesEnv, t.TempDir(),
		toolServerCrashLaunchesEnv, toolServerReadyEnv, ready,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
	e := newEngine(t, engine.WithMCPRetryBase(
		engine.Options{Company: parsedCompany(t, doc)}, 100*time.Millisecond))
	if err := os.Remove(ready); err != nil {
		t.Fatalf("take the vendor away: %v", err)
	}
	if res := call(t, e, "ceo", "tracker_probe"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}
	within(t, "the server to be down", func() bool {
		row, ok := statusOf(e, "tracker")
		return ok && row.Failed == 1
	})

	before := e.Company()
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if e.Company() == before {
		t.Fatal("the apply published no new epoch")
	}
	if row, _ := statusOf(e, "tracker"); row.Failed != 1 {
		t.Fatalf("the server is not down after the apply (%+v), though its vendor is", row)
	}
	if got, want := e.Company().Tools.Names(), before.Tools.Names(); !slices.Equal(got, want) {
		t.Fatalf("the epoch an apply built while the server was down lists\n%v\nwhere the "+
			"same revision applied while it was up listed\n%v", got, want)
	}
	if res := call(t, e, "cto", "tracker_probe"); !res.Failed || tools.RefusalOf(res) != tools.RefusalUnavailable {
		t.Errorf("a call while it is down = %+v, want the engine's refusal", res)
	}

	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatalf("bring the vendor back: %v", err)
	}
	within(t, "the server to be back, through the new epoch", func() bool {
		return !call(t, e, "cto", "tracker_probe").Failed
	})
}

// A SEAT'S OWN CHILD THAT DIES IS BROUGHT BACK FOR THAT SEAT, carrying that
// seat's credential and nobody else's, and the tool its registry holds works
// again.
func TestASeatsOwnChildThatDiesIsBroughtBackForThatSeat(t *testing.T) {
	t.Parallel()
	// Launches 1 and 2 are the two seats' first children, in whichever
	// order they were claimed; each crashes after its first call, and the
	// CEO's replacement is launch 3.
	doc := withServerEnv(mcpCompany(false, "SEAT_TOKEN"), map[string]string{
		toolServerLaunchesEnv:      t.TempDir(),
		toolServerCrashLaunchesEnv: "1,2",
	})
	e := newEngine(t, engine.WithMCPRetryBase(
		engine.Options{Company: parsedCompany(t, doc)}, 100*time.Millisecond))
	claimed(t, e, 2)

	held, _ := e.ToolsFor("ceo").Lookup("tracker_probe")
	if res := call(t, e, "ceo", "tracker_probe"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}
	within(t, "the CEO's child to be back", func() bool {
		return !call(t, e, "ceo", "tracker_probe").Failed
	})
	if got := describes(t, e, "ceo", "tracker_probe"); !strings.Contains(got, "ceo-secret") {
		t.Errorf("the CEO's restarted child holds %q, want its own credential", got)
	}
	if now, _ := e.ToolsFor("ceo").Lookup("tracker_probe"); now.Tool != held.Tool {
		t.Error("the restart replaced a tool whose listing did not change")
	}
	if got := describes(t, e, "cto", "tracker_probe"); !strings.Contains(got, "cto-secret") {
		t.Errorf("the CTO's child now holds %q", got)
	}
}

// THE RESTART SCHEDULE KEEPS THE SEAT CADENCE it is anchored to: its first
// wait is one seat heartbeat — when a node's proof that it holds a seat can
// next change, which is what a held-back per-role restart is waiting for —
// and its ceiling the interval a seat's persistent failure is repeated at. A
// change to either side without the other breaks the argument both rest on.
func TestTheMCPRestartScheduleKeepsTheSeatCadence(t *testing.T) {
	t.Parallel()
	if mcp.RestartBase != seat.HeartbeatInterval {
		t.Errorf("mcp.RestartBase = %s, seat.HeartbeatInterval = %s", mcp.RestartBase, seat.HeartbeatInterval)
	}
	if mcp.RestartCeiling != seat.UndeadAlarmInterval {
		t.Errorf("mcp.RestartCeiling = %s, seat.UndeadAlarmInterval = %s",
			mcp.RestartCeiling, seat.UndeadAlarmInterval)
	}
}
