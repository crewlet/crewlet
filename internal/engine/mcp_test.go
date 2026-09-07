package engine_test

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
)

// The engine's MCP wiring, against real child processes.
//
// Every case here failed before this wiring existed, and not by erroring:
// `mcp_servers:` parsed, validated and appeared in the JSON Schema while
// nothing ever spawned a child, so a seat's whole surface was the builtins.
// A company could be configured to reach a tracker and simply could not.

// mcpCompany writes a company whose MCP servers are this test binary.
//
// echo names an environment variable each child reports back in its tool's
// description, which is how a test reads what credential a child actually
// received rather than what the engine meant to send.
func mcpCompany(shared bool, echo string) string {
	sharedFlag := "false"
	if shared {
		sharedFlag = "true"
	}
	return fmt.Sprintf(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
mcp_servers:
  - name: tracker
    shared: %s
    command: %q
    tool_prefix: "tracker_"
    env:
      %s: %q
      %s: "probe"
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
		toolServerEchoEnv, echo)
}

// claimed starts the engine and waits for it to hold its seats.
//
// A per-role child belongs to a seat's LEASE, not to the epoch: this node
// spawns one only for a seat it has claimed. So a test about per-role tools
// has to go through the claim, which is also the ordering OnAcquire exists to
// enforce — the children are up before the mailbox opens.
func claimed(t *testing.T, e *engine.Engine, seats int) {
	t.Helper()
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if len(e.Node().Host().Held()) >= seats {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("held %v after 10s, want %d seats", e.Node().Host().Held(), seats)
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

// A SHARED SERVER IS ONE CHILD FOR THE COMPANY, and its tools belong to
// everyone. It carries no seat's credentials, so there is nothing to keep
// apart — and spawning one per seat would run the same process N times for
// one catalogue.
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

// THE IDENTITY INVARIANT, and the reason a seat needs its own registry.
//
// A `shared: false` server is a template: each role gets its own child holding
// that role's credentials. Two children of one template publish the SAME tool
// name, so one flat registry would keep whichever registered last and every
// seat would call it — acting under another seat's identity in the tracker,
// invisibly, because the call looks identical from the engine's side.
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

// A SEAT THAT DECLARES NO CREDENTIALS FOR A TEMPLATE GETS NO CHILD.
//
// A template with nobody's identity in it is a server nobody can act through,
// and spawning one anyway would put a tool in the prompt whose every call
// fails on authentication.
func TestASeatWithoutCredentialsGetsNoChild(t *testing.T) {
	t.Parallel()
	doc := strings.Replace(mcpCompany(false, "SEAT_TOKEN"), `    mcp_env:
      tracker:
        SEAT_TOKEN: "cto-secret"
`, "", 1)
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	if names := surfaceOf(t, e, "cto"); slices.Contains(names, "tracker_probe") {
		t.Errorf("a seat that declares no credentials was given the tool anyway: %v", names)
	}
	// And the seat that DID declare them still has it, so the case above is
	// the absence of a child rather than the absence of the wiring.
	if names := surfaceOf(t, e, "ceo"); !slices.Contains(names, "tracker_probe") {
		t.Errorf("the seat that declared credentials lost its tool: %v", names)
	}
}

// EVERY SEAT KEEPS ITS BUILTINS. The per-seat surface is the company's
// catalogue PLUS this seat's children, never a replacement for it — a seat
// that gained a tracker and lost lookup_colleague would be a worse seat.
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

// A SERVER THAT WILL NOT START COSTS ITS OWN TOOLS AND NOTHING ELSE.
//
// The alternative — failing the apply — takes a working company offline
// because one vendor's binary is missing from an image. The operator sees the
// group absent, which is the reading that sends them to the right subsystem;
// builtins quietly shrinking is the reading that does not.
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

// A SHARED SERVER SURVIVES A CONFIG APPLY, and this is the case the suite
// above could not see because every one of them builds exactly one epoch.
//
// An apply builds a FRESH registry for the new epoch and equips it. The
// bridge's children are already running, so the engine has to be told about
// them again — and it asked the bridge to ADD them, which refuses a name it
// already runs. The refusal was logged as a failed server and the epoch went
// live with the builtins alone.
//
// It is not a rare path. `crewlet run` seeds its company config at boot and
// the reconciler applies it immediately, so on the Nimbus example the shared
// servers reached exactly one epoch — the one replaced a second later — and
// no seat ever saw a shared tool.
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
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc)); err != nil {
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

// AND SO DO A HELD SEAT'S PER-ROLE TOOLS, which is the harder half.
//
// A per-role registry is a CLONE of the epoch's surface, and an apply
// publishes a new epoch — so unless the apply rebuilds them, every seat this
// node already holds falls back to the shared surface and loses its entire
// per-role tool set. Silent, permanent until the seat is released and
// re-claimed, and it fires on the documented credential-rotation gesture of
// re-activating an unchanged revision. The children are still running the
// whole time, holding the seat's credentials, with no turn able to reach one.
func TestAHeldSeatsPerRoleToolsSurviveAConfigApply(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(false, "SEAT_TOKEN")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})
	claimed(t, e, 2)

	before := describes(t, e, "ceo", "tracker_probe")
	if !strings.Contains(before, "ceo-secret") {
		t.Fatalf("the boot epoch never gave the seat its own child: %q", before)
	}

	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc)); err != nil {
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
	// AND THE BUILTINS ARE THE NEW EPOCH'S OBJECTS, not merely its names.
	//
	// Every apply re-registers the builtins into the new epoch's registry
	// from that revision's own numbers, so the objects differ between
	// epochs while the names never do. Carrying the seat's old registry
	// across an apply would satisfy every check above and still serve the
	// PREVIOUS revision's builtins and knobs to turns running under the
	// new one — which is why this compares identity rather than membership.
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

// AND THE PER-ROLE CHILD IS NOT RESTARTED BY THE REBUILD.
//
// A per-role child belongs to the seat's LEASE rather than to the epoch, so
// an apply must leave it alone: restarting one would re-handshake every
// seat's vendor credentials on every config edit, and the control-plane doc
// states the rule. The registry is rebuilt; the process never learns an
// apply happened.
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
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc)); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	second, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
	if !ok {
		t.Fatal("the applied epoch has no per-role tool")
	}
	if first.Tool != second.Tool {
		t.Error("an apply that changed nothing replaced the seat's per-role tools, " +
			"which means it restarted a healthy child holding that seat's credentials")
	}
}

// AND THE CHILD IS NOT RESTARTED FOR AN UNCHANGED SPEC.
//
// The tempting fix is to restart every server on every apply, which would
// make the case above pass while tearing down every working child — on a
// company with several seats and real vendors, seconds of every seat's tools
// being absent for a config edit that touched none of them.
func TestAnUnchangedServerIsNotRestartedByAnApply(t *testing.T) {
	t.Parallel()
	doc := mcpCompany(true, "PATH")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, doc)})

	// The tool OBJECT identifies the child behind it: a restart builds new
	// ones, so an unchanged pointer is proof the same process is serving.
	first, ok := e.Company().Tools.Lookup("tracker_probe")
	if !ok {
		t.Fatal("the boot epoch has no shared tool to compare")
	}
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, doc)); err != nil {
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
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, next)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got := describes(t, e, "ceo", "tracker_probe")
	if !strings.HasPrefix(got, "SEAT_TOKEN=") {
		t.Errorf("the tool still reports %q: the edited spec never reached a "+
			"child, so the apply reported a change it did not make", got)
	}
}

// A SHARED SERVER A REVISION REMOVED IS RETIRED, not merely forgotten.
//
// The apply-time reconcile is driven by the specs the CURRENT config names,
// so a deleted server is never visited: its tools leave the catalogue with
// the new epoch's registry, but the CHILD — holding the company's
// credentials — keeps running until the engine stops. A rename runs two.
func TestASharedServerARevisionRemovedIsStopped(t *testing.T) {
	t.Parallel()
	with := mcpCompany(true, "PATH")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, with)})

	if !slices.Contains(e.Company().Tools.Names(), "tracker_probe") {
		t.Fatal("the boot epoch never started the shared server")
	}

	// The same document with the whole mcp_servers block gone — the
	// gesture an operator makes to take a leaking integration offline.
	without := with[:strings.Index(with, "mcp_servers:")] +
		with[strings.Index(with, "roles:"):]
	if _, _, err := e.Apply(t.Context(), parsedCompany(t, without)); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The catalogue losing the tool is the half that already worked.
	if names := e.Company().Tools.Names(); slices.Contains(names, "tracker_probe") {
		t.Errorf("the applied epoch still advertises the removed server: %v", names)
	}
	// The child being GONE is the half that did not. Servers() is the
	// bridge's own record of what it is still running.
	if running := e.SharedServers(); slices.Contains(running, "tracker") {
		t.Errorf("the removed server is still running: %v\n"+
			"its process holds the company's credentials and would survive "+
			"until the engine stops", running)
	}
}
