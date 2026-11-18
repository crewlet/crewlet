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
// that die (the bridge's restarts are internal/mcp's cases): a seat's turns
// reach the server again, a shared server whose start failed reaches every
// seat once it is up with no config apply in between, and the status record
// says what the server is doing now, not what its start concluded.

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

// statusOf is the status record's row for one server, if it has one.
func statusOf(e *engine.Engine, server string) (coord.MCPServerStatus, bool) {
	for _, row := range engine.MCPStatus(e) {
		if row.Server == server {
			return row, true
		}
	}
	return coord.MCPServerStatus{}, false
}

// within polls cond until it holds or 30s pass: long, because a restart is a
// real spawn and handshake under the race detector, and a passing case pays
// nothing for it.
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
// While it is down a seat's call is refused in the engine's words, classed
// unavailable (never a broken pipe), and the status record says it failed with
// the child's exit status; once back, the tool objects the registries already
// hold work again and the record says it started.
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
	within(t, "the status record to report the server down", func() bool {
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
		t.Errorf("status record after the restart = %+v, want one started instance serving its tool", row)
	}
	if now, _ := e.ToolsFor("cto").Lookup("tracker_probe"); now.Tool != held.Tool {
		t.Error("the restart replaced a tool whose listing did not change")
	}
}

// A SHARED SERVER WHOSE START FAILED REACHES EVERY SEAT WHEN IT COMES UP,
// without an apply. A seat with a surface of its own (a per-role server) gets
// the shared tool refiled into it too: that surface was cloned from the
// epoch's before the shared server was up.
//
// THE CASE DECIDES WHEN THE VENDOR COMES UP, not a timer: the server refuses
// every start until its ready file exists, written only once both seats are
// claimed and the CEO's surface lacks the shared tool. A retry schedule long
// enough to outlast the claims would race the engine's boot.
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
	// Short: every retry before the file appears is refused, so this only
	// bounds how soon after it the server comes up.
	e := newEngine(t, engine.WithMCPRetryBase(
		engine.Options{Company: parsedCompany(t, doc)}, 100*time.Millisecond))

	if row, ok := statusOf(e, "search"); !ok || row.Failed != 1 || !strings.Contains(row.Error, "would not start") {
		t.Fatalf("status record after the failed start = %+v, want it failed with why", row)
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
		t.Errorf("status record after the retry = %+v, want it started", row)
	}
}

// AN APPLY WHILE A SHARED SERVER IS DOWN KEEPS ITS TOOLS LISTED, IN THE ORDER
// AN APPLY WHILE IT IS UP WOULD. The reconcile reports the server down and
// hands back the tools it still lists, which the apply files beside the error
// in spec order; they refuse until it is back, and nothing refiles them then
// (same tools, no change). Left out, the new epoch would never offer them
// again; appended after publishing, the same revision would list its tools in
// another order, and a registry's order is the order a prompt lists them in.
func TestAnApplyWhileASharedServerIsDownKeepsItsToolsListed(t *testing.T) {
	t.Parallel()
	// The one that goes down is first in spec order, so a tool filed late
	// would land after the second's. Its vendor is taken away before the
	// call it crashes on and given back only after the apply, so every
	// restart, the apply's included, is refused throughout.
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
// wait is one seat heartbeat (when a node's proof that it holds a seat can next
// change, which a held-back per-role restart waits for), its ceiling the
// interval a seat's persistent failure is repeated at. Change both or neither.
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
