package engine

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/tools"
)

// The supervision's seams inside the engine, with the suite's helper tool
// server (mcpserver_test.go) really dying: which predicate gates a seat's
// restarts, that the runner and the bridge never both retry one child, and
// where a changed catalogue is refiled.

// helperCompany is a company whose one MCP server, `tracker`, is the suite's
// helper tool server launched with env (shared, or a per-role template for the
// CEO), and the directory its launches are counted in.
func helperCompany(t *testing.T, shared bool, env map[string]string) (*Company, string) {
	t.Helper()
	launches := t.TempDir()
	full := map[string]string{
		ToolServerModeEnv:     "server",
		ToolServerLaunchesEnv: launches,
		procgrouptest.GORACE:  procgrouptest.StandInRaceOptions(),
	}
	for k, v := range env {
		full[k] = v
	}
	var block strings.Builder
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range full {
			if !yield(k) {
				return
			}
		}
	}) {
		fmt.Fprintf(&block, "      %s: %q\n", k, full[k])
	}
	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: tracker
    shared: %t
    command: %q
    tool_prefix: "tracker_"
    env:
%sroles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-token"
`, shared, os.Args[0], block.String())))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company, launches
}

// launchesIn is how many helper processes have launched against a directory.
func launchesIn(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read launches: %v", err)
	}
	return len(entries)
}

// pollUntil polls cond until it holds or the deadline passes.
func pollUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", d, what)
}

// seatChild is the CEO's tracker child as its bridge sees it now.
func seatChild(s *seatChildren) mcp.Health {
	h, _ := s.bridge.Health(mcp.InstanceName("tracker", "CEO"))
	return h
}

// callSeatTool runs one of the CEO's tools as its turn would.
func callSeatTool(t *testing.T, e *Engine, name string) tools.Result {
	t.Helper()
	entry, ok := e.ToolsFor("ceo").Lookup(name)
	if !ok {
		t.Fatalf("the CEO has no tool %q: %v", name, e.ToolsFor("ceo").Names())
	}
	res, err := entry.Tool.Call(t.Context(), nil)
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}
	return res
}

// A SEAT'S CHILD THAT DIES IS RESTARTED ONLY WHILE THIS NODE CAN PROVE IT HOLDS
// THE SEAT, by the predicate the seat's own passes ask. The child IS the agent
// at the vendor, so a node whose lease lapsed unnoticed must not start a copy
// beside the seat's new holder. Without the proof the child stays down and
// refuses in the engine's words; with it back, so is the child.
func TestASeatsDeadChildIsRestartedOnlyWhileTheSeatIsProvable(t *testing.T) {
	t.Parallel()
	company, launches := helperCompany(t, false, map[string]string{ToolServerCrashLaunchesEnv: "1"})
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 50 * time.Millisecond}
	e.epoch.current.Store(company)
	var provable atomic.Bool
	provable.Store(true)
	s := heldSeatChildren(t, e, "ceo", provable.Load)
	e.convergeSeat(t.Context(), s, false)
	if !seatChild(s).Serving {
		t.Fatalf("precondition: the claim's pass did not start the child: %+v", seatOutcomes(e, "ceo"))
	}

	provable.Store(false)
	if res := callSeatTool(t, e, "tracker_probe"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}
	pollUntil(t, 30*time.Second, "the restart to be held back", func() bool {
		return strings.Contains(seatChild(s).Down, "held back")
	})
	time.Sleep(300 * time.Millisecond) // six of the schedule's bases
	if n := launchesIn(t, launches); n != 1 {
		t.Fatalf("the child was started %d times while this node could not prove the seat", n)
	}
	if res := callSeatTool(t, e, "tracker_probe"); !res.Failed || tools.RefusalOf(res) != tools.RefusalUnavailable {
		t.Errorf("a call while the child is held back = %+v, want the engine's refusal", res)
	}

	provable.Store(true)
	pollUntil(t, 30*time.Second, "the child to be restarted once the seat is provable", func() bool {
		return seatChild(s).Serving
	})
	if n := launchesIn(t, launches); n != 2 {
		t.Errorf("the child was started %d times, want once and once again", n)
	}
}

// A PASS LEAVES A CHILD THAT DIED TO ITS BRIDGE, AND AN APPLY'S PASS ASKS IT TO
// TRY NOW. Starting it would be a second retry loop racing the supervisor's,
// so a pass leaves it, reports it down and arms nothing; an apply's pass (what
// an operator does once a vendor is back) asks for an attempt now rather than
// at the schedule's next.
func TestAPassLeavesADeadSeatChildToItsBridgeAndAnApplyAsksItToTryNow(t *testing.T) {
	t.Parallel()
	// The immediate restart fails, so the bridge's next attempt is a base
	// away: ten seconds, spread to no less than eight.
	company, launches := helperCompany(t, false, map[string]string{
		ToolServerCrashLaunchesEnv: "1",
		ToolServerFailLaunchesEnv:  "2",
	})
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 10 * time.Second}
	e.epoch.current.Store(company)
	s := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), s, false)
	if res := callSeatTool(t, e, "tracker_probe"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}
	pollUntil(t, 30*time.Second, "the restart at once to fail", func() bool {
		return strings.Contains(seatChild(s).Down, "would not restart")
	})

	// A pass nobody's apply asked for: the runner's retry, or a claim.
	e.convergeSeat(t.Context(), s, false)
	time.Sleep(300 * time.Millisecond)
	if n := launchesIn(t, launches); n != 2 {
		t.Fatalf("a pass started the dead child (%d launches): a second loop over a child "+
			"its bridge is bringing back", n)
	}
	s.mu.Lock()
	armed := s.retry != nil
	s.mu.Unlock()
	if armed {
		t.Fatal("a pass armed the runner's retry for a child its bridge is restarting")
	}
	if got := seatOutcomes(e, "ceo"); len(got) != 1 || got[0].err != nil {
		t.Fatalf("the pass concluded %+v, want the child left to its bridge", got)
	}
	if rows := e.mcpStatus(t.Context()); len(rows) != 1 || rows[0].Failed != 1 ||
		!strings.Contains(rows[0].Error, "would not restart") {
		t.Errorf("status record = %+v, want the child reported down with why", rows)
	}

	e.convergeSeat(t.Context(), s, true)
	pollUntil(t, 4*time.Second, "the child to be back after an apply's pass asked", func() bool {
		return seatChild(s).Serving
	})
	if n := launchesIn(t, launches); n != 3 {
		t.Errorf("the child was launched %d times, want three", n)
	}
}

// BUT A PASS RESTARTS A DEAD CHILD WHOSE SPEC MOVED, INTO THE NEW SPEC: its
// bridge would bring it back from the spec it died with (a rotated-out
// credential, a replaced command). The new spec is told apart by the directory
// its launches are counted in.
func TestAPassRestartsADeadSeatChildWhoseSpecMovedIntoTheNewSpec(t *testing.T) {
	t.Parallel()
	// The immediate restart fails and the bridge's next attempt is a base
	// away, so the child is still down from its old spec when the spec moves.
	before, oldLaunches := helperCompany(t, false, map[string]string{
		ToolServerCrashLaunchesEnv: "1",
		ToolServerFailLaunchesEnv:  "2",
	})
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 10 * time.Second}
	e.epoch.current.Store(before)
	s := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), s, false)
	if res := callSeatTool(t, e, "tracker_probe"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}
	pollUntil(t, 30*time.Second, "the restart at once to fail", func() bool {
		return strings.Contains(seatChild(s).Down, "would not restart")
	})

	after, newLaunches := helperCompany(t, false, nil)
	e.epoch.current.Store(after)
	e.convergeSeat(t.Context(), s, false)
	if !seatChild(s).Serving {
		t.Fatalf("the pass left a dead child whose spec moved to its bridge: %+v", seatChild(s))
	}
	if n := launchesIn(t, newLaunches); n != 1 {
		t.Errorf("the new spec was launched %d times, want once", n)
	}
	time.Sleep(300 * time.Millisecond)
	if n := launchesIn(t, oldLaunches); n != 2 {
		t.Errorf("the old spec was launched %d times, want the first and the failed restart: "+
			"its supervision outlived the restart into the new spec", n)
	}
	if res := callSeatTool(t, e, "tracker_probe"); res.Failed {
		t.Errorf("the child restarted into the new spec does not answer: %q", res.Output)
	}
}

// A SEAT'S CHILD THAT COMES BACK WITH OTHER TOOLS IS REFILED INTO THE SEAT'S
// SURFACE. Nothing else would: no pass runs for the bridge's own restart, and
// the seat's registry is a clone, so its turns would keep a tool the child no
// longer lists.
func TestASeatChildThatComesBackWithOtherToolsIsRefiledIntoTheSeat(t *testing.T) {
	t.Parallel()
	company, _ := helperCompany(t, false, map[string]string{
		ToolServerCrashLaunchesEnv: "1",
		ToolServerToolEnv:          "probe_" + ToolServerLaunchToken,
	})
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 50 * time.Millisecond}
	e.epoch.current.Store(company)
	s := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), s, false)
	if res := callSeatTool(t, e, "tracker_probe_1"); res.Failed {
		t.Fatalf("the call before the crash failed: %q", res.Output)
	}

	pollUntil(t, 30*time.Second, "the restarted child's tool in the seat's surface", func() bool {
		_, ok := e.ToolsFor("ceo").Lookup("tracker_probe_2")
		return ok
	})
	if _, ok := e.ToolsFor("ceo").Lookup("tracker_probe_1"); ok {
		t.Error("the seat's surface still offers the tool the restarted child no longer lists")
	}
	if res := callSeatTool(t, e, "tracker_probe_2"); res.Failed {
		t.Errorf("the restarted child's tool does not answer: %q", res.Output)
	}
}

// AN EPOCH IS PUBLISHED WITH THE SHARED CATALOGUE AS IT STANDS THEN, not as it
// stood when the apply filed it: a shared server's restart while the epoch was
// built refiled only the epoch being replaced, so the publish refiles from the
// bridge, or it would offer a tool no server lists until the next apply.
func TestAPublishRefilesWhatTheSharedBridgeChangedWhileTheEpochWasBuilt(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		ToolServerCrashLaunchesEnv: "1",
		ToolServerToolEnv:          "probe_" + ToolServerLaunchToken,
	}
	current, _ := helperCompany(t, true, env)
	building, _ := helperCompany(t, true, env)
	e := &Engine{mcpRetryBase: 50 * time.Millisecond}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { e.stopSharedServers(context.Background()) })
	e.epoch.current.Store(current)

	e.startSharedServers(t.Context(), building)
	if _, ok := building.Tools.Lookup("tracker_probe_1"); !ok {
		t.Fatalf("precondition: the epoch being built lacks the server's tool: %v", building.Tools.Names())
	}
	tool, ok := e.mcp.Tool("tracker_probe_1")
	if !ok {
		t.Fatal("precondition: the bridge does not serve the tool")
	}
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	pollUntil(t, 30*time.Second, "the restart's tool refiled into the current epoch", func() bool {
		_, ok := current.Tools.Lookup("tracker_probe_2")
		return ok
	})

	e.installEpoch(t.Context(), building)
	names := building.Tools.Names()
	if !slices.Contains(names, "tracker_probe_2") || slices.Contains(names, "tracker_probe_1") {
		t.Fatalf("the published epoch lists %v, want the restarted server's tool and not the one "+
			"it no longer lists", names)
	}
}

// A CATALOGUE THE SHARED BRIDGE REFILED IS TOLD TO WHAT RENDERS IT: the
// dashboard's catalogue is pushed only on a published company, so a restarted
// server's new tools would wait for a reload. The hook runs only once the
// current epoch holds the new tools, so a surface reading the registry when
// told reads the new catalogue.
func TestARefiledSharedCatalogueIsToldToWhatRendersIt(t *testing.T) {
	t.Parallel()
	current, _ := helperCompany(t, true, map[string]string{
		ToolServerCrashLaunchesEnv: "1",
		ToolServerToolEnv:          "probe_" + ToolServerLaunchToken,
	})
	e := &Engine{mcpRetryBase: 50 * time.Millisecond}
	e.mcp = mcp.NewBridge(nil, e.sharedSupervision())
	t.Cleanup(func() { e.stopSharedServers(context.Background()) })
	var told, toldWithNewTools atomic.Int32
	e.SetOnCatalogueChanged(func(context.Context) {
		told.Add(1)
		if _, ok := current.Tools.Lookup("tracker_probe_2"); ok {
			toldWithNewTools.Add(1)
		}
	})
	e.epoch.current.Store(current)

	e.startSharedServers(t.Context(), current)
	if told.Load() != 0 {
		t.Fatalf("a start an apply made was told as a catalogue change %d times: the "+
			"publish that follows it re-sends the catalogue", told.Load())
	}
	tool, ok := e.mcp.Tool("tracker_probe_1")
	if !ok {
		t.Fatal("precondition: the bridge does not serve the tool")
	}
	if res, err := tool.Call(t.Context(), nil); err != nil || res.Failed {
		t.Fatalf("the call before the crash = %+v, %v", res, err)
	}
	pollUntil(t, 30*time.Second, "the restart's catalogue told to what renders it", func() bool {
		return told.Load() > 0
	})
	if toldWithNewTools.Load() != told.Load() {
		t.Fatalf("the hook ran before the current epoch held the restarted server's tools: "+
			"%d of %d calls saw them", toldWithNewTools.Load(), told.Load())
	}
}
