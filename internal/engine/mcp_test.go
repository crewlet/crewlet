package engine_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// The engine's MCP wiring, against real child processes.

// mcpCompany writes a company whose MCP servers are this test binary. echo
// names the environment variable each child reports in its tool's description,
// so a test reads the credential a child actually received.
//
// The race runtime's exit sleep is switched off
// ([procgrouptest.StandInRaceOptions]) in the server's own `env:`, the
// environment the engine launches a child with; otherwise a race-built helper
// sleeps a second on every stop a case makes.
func mcpCompany(shared bool, echo string) string {
	sharedFlag := "false"
	if shared {
		sharedFlag = "true"
	}
	return fmt.Sprintf(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: tracker
    shared: %s
    command: %q
    tool_prefix: "tracker_"
    env:
      %s: %q
      %s: "probe"
      %s: %q
      %s: %q
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
    mcp_env:
      tracker:
        SEAT_TOKEN: "cto-secret"
`, sharedFlag, os.Args[0],
		toolServerModeEnv, "server",
		toolServerToolEnv,
		toolServerEchoEnv, echo,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions())
}

// claimed starts the engine and waits for it to hold its seats, each with its
// acquisition finished ([waitEstablished]). A per-role child belongs to a
// seat's LEASE, not the epoch, so a test of per-role tools goes through the
// claim (OnAcquire starts the children before the mailbox opens).
func claimed(t *testing.T, e *engine.Engine, seats int) {
	t.Helper()
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitEstablished(t, e, seats)
}

// waitEstablished waits for this node to hold seats seats, every one of them
// ESTABLISHED, not merely held: a seat is held from the moment its lease is
// taken, BEFORE its acquisition has started its children, while
// [seat.Host.MayStart] admits a turn only once the surface is complete.
func waitEstablished(t *testing.T, e *engine.Engine, seats int) {
	t.Helper()
	host := e.Node().Host()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		held := host.Held()
		established := len(held) >= seats
		for _, handle := range held {
			if _, ok := host.MayStart(handle); !ok {
				established = false
			}
		}
		if established {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("held %v after 30s, want %d seats established", host.Held(), seats)
}

// surfaceOf is the tool names one seat's turns would be built against.
func surfaceOf(t *testing.T, e *engine.Engine, handle string) []string {
	t.Helper()
	return e.ToolsFor(handle).Names()
}

// describes returns the description a tool carries, which the helper server
// uses to report its own environment.
func describes(t *testing.T, e *engine.Engine, handle, tool string) string {
	t.Helper()
	entry, ok := e.ToolsFor(handle).Lookup(tool)
	if !ok {
		t.Fatalf("seat %q has no tool %q; surface = %v", handle, tool, surfaceOf(t, e, handle))
	}
	return entry.Tool.Description()
}

// A SHARED SERVER IS ONE CHILD FOR THE COMPANY, and its tools reach every
// seat: it carries no seat's credentials, so there is nothing to keep apart.
func TestASharedServersToolsReachEverySeat(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, mcpCompany(true, "PATH"))})

	for _, handle := range []string{"ceo", "cto"} {
		names := surfaceOf(t, e, handle)
		if !slices.Contains(names, "tracker_probe") {
			t.Errorf("seat %q cannot reach the shared server: %v", handle, names)
		}
	}
}

// THE IDENTITY INVARIANT, and why a seat needs its own registry: each role's
// child of a `shared: false` template holds that role's credentials and
// publishes the SAME tool name, so one flat registry would keep the last one
// registered and every seat would act under that seat's identity, invisibly.
func TestEachSeatCallsItsOwnChildAndNotAPeers(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, mcpCompany(false, "SEAT_TOKEN"))})
	claimed(t, e, 2)

	ceo := describes(t, e, "ceo", "tracker_probe")
	cto := describes(t, e, "cto", "tracker_probe")
	if !strings.Contains(ceo, "ceo-secret") {
		t.Errorf("the CEO's child holds %q, want its own credential", ceo)
	}
	if !strings.Contains(cto, "cto-secret") {
		t.Errorf("the CTO's child holds %q, want its own credential", cto)
	}
	if ceo == cto {
		t.Fatal("both seats resolved the same tool object: one child is serving " +
			"two identities, which is exactly what the per-seat surface prevents")
	}
}

// A SEAT A TEMPLATE IS NOT GRANTED TO GETS NO CHILD, whatever values it holds
// for it, and a seat it is granted to gets one, with none: the grant is the
// server's `access`, and an `mcp_env` block holds values only.
func TestASeatATemplateIsNotGrantedToGetsNoChild(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(mcpCompany(false, "SEAT_TOKEN"), `    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-secret"
`, "", 1)
	doc = strings.Replace(doc, `    tool_prefix: "tracker_"
`, `    tool_prefix: "tracker_"
    access: {seats: [ceo]}
`, 1)
	doc = strings.Replace(doc, `    mcp_env:
      tracker:
        SEAT_TOKEN: "cto-secret"
`, "", 1)
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	if names := surfaceOf(t, e, "cto"); slices.Contains(names, "tracker_probe") {
		t.Errorf("a seat the template is not granted to was given its tool: %v", names)
	}
	// And the seat it IS granted to has it with no block of its own, so the
	// case above is the grant at work rather than the absence of the wiring.
	if names := surfaceOf(t, e, "ceo"); !slices.Contains(names, "tracker_probe") {
		t.Errorf("the seat the template is granted to has no tool: %v", names)
	}
}

// EVERY SEAT KEEPS ITS BUILTINS: the per-seat surface is the company's
// catalogue PLUS this seat's children, never a replacement for it.
func TestASeatsOwnSurfaceStillCarriesTheBuiltins(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, mcpCompany(false, "SEAT_TOKEN"))})
	claimed(t, e, 2)

	names := surfaceOf(t, e, "ceo")
	for _, builtin := range []string{"lookup_colleague", "reflect_and_persist"} {
		if !slices.Contains(names, builtin) {
			t.Errorf("the seat lost the %s builtin to its own surface: %v", builtin, names)
		}
	}
}

// A SERVER THAT WILL NOT START COSTS ITS OWN TOOLS AND NOTHING ELSE. Failing
// the apply would take a working company offline over one missing binary, and
// the group being absent sends the operator to the right subsystem.
func TestAServerThatCannotStartLeavesTheSeatItsBuiltins(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(mcpCompany(false, "SEAT_TOKEN"),
		fmt.Sprintf("command: %q", os.Args[0]),
		`command: "/nonexistent/definitely-not-a-tool-server"`, 1)
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	names := surfaceOf(t, e, "ceo")
	if slices.Contains(names, "tracker_probe") {
		t.Error("a server that cannot start contributed a tool")
	}
	if !slices.Contains(names, "lookup_colleague") {
		t.Errorf("a failed server took the builtins with it: %v", names)
	}
}

// A SHARED SERVER SURVIVES A CONFIG APPLY. An apply builds a FRESH registry
// for the new epoch while the bridge's children keep running, so the registry
// must be told about them again: asking the bridge to ADD them is refused for
// a name it already runs, leaving the epoch with the builtins alone. Every
// `crewlet run` takes this path, applying its seeded config at boot.
func TestASharedServersToolsSurviveAConfigApply(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(true, "PATH")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})

	before := surfaceOf(t, e, "ceo")
	if !slices.Contains(before, "tracker_probe") {
		t.Fatalf("the boot epoch never had the shared tool: %v", before)
	}

	// The same document, re-applied — the rotation gesture an operator
	// makes, and what the reconciler does at boot straight after seeding.
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	after := surfaceOf(t, e, "ceo")
	if !slices.Contains(after, "tracker_probe") {
		t.Fatalf("the applied epoch lost the shared server's tools: %v\n"+
			"the child is still running; only the new epoch's registry was "+
			"never told about it", after)
	}
	for _, builtin := range []string{"lookup_colleague", "reflect_and_persist"} {
		if !slices.Contains(after, builtin) {
			t.Errorf("the applied epoch lost the %s builtin: %v", builtin, after)
		}
	}
}

// AND SO DO A HELD SEAT'S PER-ROLE TOOLS. A per-role registry is a CLONE of
// the epoch's surface, so unless an apply rebuilds it, every held seat falls
// back to the shared surface until re-claimed, its children still running
// with its credentials and unreachable. Re-activating an unchanged revision,
// the documented credential-rotation gesture, is such an apply.
func TestAHeldSeatsPerRoleToolsSurviveAConfigApply(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(false, "SEAT_TOKEN")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	before := describes(t, e, "ceo", "tracker_probe")
	if !strings.Contains(before, "ceo-secret") {
		t.Fatalf("the boot epoch never gave the seat its own child: %q", before)
	}

	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	names := surfaceOf(t, e, "ceo")
	if !slices.Contains(names, "tracker_probe") {
		t.Fatalf("the applied epoch lost the seat's per-role tools: %v\n"+
			"its children are still running and holding its credentials; only "+
			"the registry the turn is built against was never rebuilt", names)
	}
	// STILL ITS OWN child, not a peer's and not the shared one — a rebuild
	// that re-filed the wrong bridge would pass the check above while
	// handing one seat another's identity.
	if after := describes(t, e, "ceo", "tracker_probe"); !strings.Contains(after, "ceo-secret") {
		t.Errorf("the seat's tool now resolves to %q, want its own credential", after)
	}
	if cto := describes(t, e, "cto", "tracker_probe"); !strings.Contains(cto, "cto-secret") {
		t.Errorf("the peer seat's tool now resolves to %q, want its own credential", cto)
	}
	// AND THE BUILTINS ARE THE NEW EPOCH'S OBJECTS, not merely its names: an
	// apply re-registers them from that revision's numbers, so a seat's old
	// registry carried across would pass every check above while serving
	// the PREVIOUS revision's knobs. Hence identity, not membership.
	for _, builtin := range []string{"lookup_colleague", "reflect_and_persist"} {
		if !slices.Contains(names, builtin) {
			t.Errorf("the applied epoch lost the %s builtin from the seat's "+
				"own surface: %v", builtin, names)
			continue
		}
		want, ok := e.Company().Tools.Lookup(builtin)
		if !ok {
			t.Fatalf("the applied epoch has no %s builtin of its own", builtin)
		}
		got, _ := e.ToolsFor("ceo").Lookup(builtin)
		if got.Tool != want.Tool {
			t.Errorf("the seat's %s comes from the outgoing epoch, so its turns "+
				"run against the previous revision's builtins and knobs", builtin)
		}
	}
}

// AND THE PER-ROLE CHILD IS NOT RESTARTED BY AN APPLY THAT DID NOT MOVE IT.
// Every apply reconciles a held seat's children so a sealed or rotated
// credential reaches them, but one whose resolved spec is unchanged is left
// alone, or every config edit would re-handshake every seat's vendor
// credentials (the control-plane doc states the rule). Only the registry is
// rebuilt.
func TestAnApplyDoesNotRestartAHeldSeatsChild(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(false, "SEAT_TOKEN")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	// The tool OBJECT identifies the child behind it: a restart builds new
	// ones, so an unchanged pointer is proof the same process is serving.
	first, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
	if !ok {
		t.Fatal("the boot epoch gave the seat no per-role tool to compare")
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// The apply's reconcile of the children runs after it returns, so a
	// restart would show only once that reconcile is over.
	engine.SettleSeatServers(e)
	second, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
	if !ok {
		t.Fatal("the applied epoch has no per-role tool")
	}
	if first.Tool != second.Tool {
		t.Error("an apply that changed nothing replaced the seat's per-role tools, " +
			"which means it restarted a healthy child holding that seat's credentials")
	}
}

// AND THE CHILD IS NOT RESTARTED FOR AN UNCHANGED SPEC: restarting every
// server on every apply would leave every seat's tools absent for seconds on a
// config edit that touched none of them.
func TestAnUnchangedServerIsNotRestartedByAnApply(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(true, "PATH")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})

	// An unchanged tool object proves the same child is serving.
	first, ok := e.Company().Tools.Lookup("tracker_probe")
	if !ok {
		t.Fatal("the boot epoch has no shared tool to compare")
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	second, ok := e.Company().Tools.Lookup("tracker_probe")
	if !ok {
		t.Fatal("the applied epoch has no shared tool")
	}
	if first.Tool != second.Tool {
		t.Error("an apply that changed nothing replaced the server's tools, " +
			"which means it restarted a healthy child")
	}
}

// A CHANGED SPEC DOES RESTART IT, which is the other half: without it an
// operator can edit a shared server's environment, watch the apply report
// success, and keep being served by the child that holds the old value.
func TestAChangedServerSpecIsRestartedByAnApply(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{
		Company: parsedCompany(t, mcpCompany(true, "PATH")),
	})
	if _, ok := e.Company().Tools.Lookup("tracker_probe"); !ok {
		t.Fatal("the boot epoch has no shared tool")
	}

	// The echo variable is what the child reports in its tool description,
	// so a changed one is visible from outside only if the child was
	// actually replaced.
	next := strings.Replace(mcpCompany(true, "PATH"),
		toolServerEchoEnv+`: "PATH"`, toolServerEchoEnv+`: "SEAT_TOKEN"`, 1)
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, next), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := describes(t, e, "ceo", "tracker_probe")
	if !strings.HasPrefix(got, "SEAT_TOKEN=") {
		t.Errorf("the tool still reports %q: the edited spec never reached a "+
			"child, so the apply reported a change it did not make", got)
	}
}

// A SHARED SERVER A REVISION REMOVED IS RETIRED, not merely forgotten: the
// apply-time reconcile visits only the specs the CURRENT config names, so a
// deleted server's child, holding the company's credentials, would run until
// the engine stops (a rename, two).
func TestASharedServerARevisionRemovedIsStopped(t *testing.T) {
	t.Parallel()
	with := mcpCompany(true, "PATH")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, with)})

	if !slices.Contains(e.Company().Tools.Names(), "tracker_probe") {
		t.Fatal("the boot epoch never started the shared server")
	}

	// The whole mcp_servers block goes, AND the seats' mcp_env blocks for it:
	// a document keeping an `mcp_env` block for an undeclared server is
	// refused, so removing only the servers would test that rule instead.
	without := with[:strings.Index(with, "mcp_servers:")] +
		strings.NewReplacer(
			"    mcp_env:\n      tracker:\n        SEAT_TOKEN: \"ceo-secret\"\n", "",
			"    mcp_env:\n      tracker:\n        SEAT_TOKEN: \"cto-secret\"\n", "",
		).Replace(with[strings.Index(with, "roles:"):])
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, without), time.Now()); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The catalogue drops the tool.
	if names := e.Company().Tools.Names(); slices.Contains(names, "tracker_probe") {
		t.Errorf("the applied epoch still advertises the removed server: %v", names)
	}
	// And the child is GONE, by the bridge's own record of what it runs.
	if running := e.SharedServers(); slices.Contains(running, "tracker") {
		t.Errorf("the removed server is still running: %v\n"+
			"its process holds the company's credentials and would survive "+
			"until the engine stops", running)
	}
}
