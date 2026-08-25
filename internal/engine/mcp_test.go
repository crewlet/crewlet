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
	return e.Company().ToolsFor(handle).Names()
}

// describes returns the description a tool carries, which the helper server
// uses to report its own environment.
func describes(t *testing.T, e *engine.Engine, handle, tool string) string {
	t.Helper()
	entry, ok := e.Company().ToolsFor(handle).Lookup(tool)
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
