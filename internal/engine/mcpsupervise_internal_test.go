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

// The supervision's seams inside the engine, against the suite's real helper
// tool server (mcpserver_test.go) dying for real: which predicate gates a
// seat's restarts, that a seat's runner and its bridge's supervisor never run
// two retry loops over one child, and where a changed catalogue is refiled.

// helperCompany is a company whose one MCP server, `tracker`, is the suite's
// helper tool server launched with env — shared, or a template only the CEO is
// granted — and the directory its launches are counted in.
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
// THE SEAT, by the predicate the seat's own passes ask. The child carries the
// seat's credentials — it IS the agent at the vendor — so a node whose lease
// lapsed before its heartbeat noticed must not start a second copy of it beside
// the node that took the seat over. While the proof is missing, the child stays
// down and refuses in the engine's words; once it is back, the child is.
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
// TRY NOW. The seat's runner retries a child whose start failed; its bridge's
// supervisor restarts one that died. A pass that took a dead child for one to
// start would be a second retry loop over it — a second start racing the
// supervisor's, and a timer of the runner's armed beside the supervisor's
// schedule. So a pass leaves it alone, reports it down, and arms nothing; and
// an apply's pass, the gesture an operator makes once a vendor is back, asks
// the supervisor for an attempt now rather than at its schedule's next.
func TestAPassLeavesADeadSeatChildToItsBridgeAndAnApplyAsksItToTryNow(t *testing.T) {
	t.Parallel()
	// The restart at once fails, so the next attempt of the bridge's own is
	// a base away: ten seconds, spread to no less than eight.
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
		t.Errorf("heartbeat = %+v, want the child reported down with why", rows)
	}

	e.convergeSeat(t.Context(), s, true)
	pollUntil(t, 4*time.Second, "the child to be back after an apply's pass asked", func() bool {
		return seatChild(s).Serving
	})
	if n := launchesIn(t, launches); n != 3 {
		t.Errorf("the child was launched %d times, want three", n)
	}
}

// BUT A PASS RESTARTS A DEAD CHILD WHOSE SPEC MOVED, INTO THE NEW SPEC. Its
// bridge would bring it back from the spec it died with — a credential the
// operator has just rotated out, a command a revision replaced — so leaving it
// to the bridge, as the pass leaves a child that died from the spec it would
// start, is exactly the wrong half of that rule. The new spec here is told
// apart by the directory its launches are counted in.
func TestAPassRestartsADeadSeatChildWhoseSpecMovedIntoTheNewSpec(t *testing.T) {
	t.Parallel()
	// The restart at once fails and the bridge's next attempt is a base
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
// SURFACE. Nothing else would: no pass runs for a restart the bridge made on
// its own, and the seat's registry is a clone filed from the bridge, so its
// turns would go on being offered the tool the child no longer lists.
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
// stood when the apply filed it. A shared server that came back with other
// tools while the epoch was being built told a refile that could only reach
// the epoch being replaced; the publish refiles the new one from the bridge,
// or it would offer a tool no server lists until the next apply.
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

// A CATALOGUE THE SHARED BRIDGE REFILED IS TOLD TO WHAT RENDERS IT. The
// dashboard's tool catalogue is a push re-sent on every published company and
// corrected by no event, so a shared server that came back from a crash with
// other tools was listed with the tools it had when the company was published
// until somebody reloaded. The hook runs once the current epoch holds the new
// tools — a surface reading the registry when told reads the new catalogue —
// and not before.
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
