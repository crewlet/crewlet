package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/engine"
)

// A server turned off, and a server's description, against real child
// processes: what the configuration says has to be what the engine runs and
// what a seat's turn is shown.

// trackerEntry adds lines to the tracker entry of [mcpCompany]'s document.
func trackerEntry(doc, lines string) string {
	return strings.Replace(doc, "  - name: tracker\n", "  - name: tracker\n"+lines, 1)
}

// turnedOff is [mcpCompany]'s document with the tracker turned off, its seats'
// mcp_env blocks left exactly where they were.
func turnedOff(shared bool) string {
	return trackerEntry(mcpCompany(shared, "SEAT_TOKEN"), "    enabled: false\n")
}

// A SERVER TURNED OFF IS NEVER STARTED AND GRANTED TO NOBODY, shared or one per
// seat, while every block that names it stays in the document: the seats'
// mcp_env blocks for it are declared, validated and kept, and start nothing.
func TestAServerTurnedOffIsNeverStartedOrGranted(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{true, false} {
		name := "per seat"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEngine(t, engine.Options{Company: parsedCompany(t, turnedOff(shared))})
			claimed(t, e, 2)

			if running := e.SharedServers(); slices.Contains(running, "tracker") {
				t.Errorf("the company bridge runs a server turned off: %v", running)
			}
			if names := e.Company().Tools.Names(); slices.Contains(names, "tracker_probe") {
				t.Errorf("the epoch's catalogue lists a server turned off: %v", names)
			}
			// NOT ON THE HEARTBEAT: nothing was started, failed or held
			// back for it, so the node has nothing to say, and the fleet's
			// answer reads the configuration to call it disabled.
			if row, reported := statusOf(e, "tracker"); reported {
				t.Errorf("the heartbeat reports a server turned off: %+v", row)
			}
			for _, handle := range []string{"ceo", "cto"} {
				if names := surfaceOf(t, e, handle); slices.Contains(names, "tracker_probe") {
					t.Errorf("seat %q is offered a server turned off: %v", handle, names)
				}
				seat := e.Company().Org.AgentSeatByHandle(handle)
				if _, kept := seat.MCPEnv["tracker"]; !kept {
					t.Errorf("seat %q lost its mcp_env block when the server was turned off", handle)
				}
				if e.Company().Config.MCPServers[0].Grants(seat) {
					t.Errorf("seat %q is granted a server turned off", handle)
				}
			}
		})
	}
}

// TURNING A SERVER OFF RETIRES WHAT IT RAN, on the apply, and turning it back
// on starts it again with the credentials its seats kept. Off is a change to
// the revision like any other: a child holding the company's or a seat's
// credentials must not outlive the decision to stop it.
func TestTurningAServerOffRetiresItsChildrenAndOnStartsThemAgain(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{true, false} {
		name := "per seat"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			on := mcpCompany(shared, "SEAT_TOKEN")
			e := newEngine(t, engine.Options{Company: parsedCompany(t, on)})
			claimed(t, e, 2)
			if !slices.Contains(surfaceOf(t, e, "ceo"), "tracker_probe") {
				t.Fatal("the boot epoch never started the server")
			}

			if _, _, err := e.Apply(t.Context(), parsedCompany(t, turnedOff(shared)), time.Now()); err != nil {
				t.Fatalf("Apply turning it off: %v", err)
			}
			waitSurface(t, e, "ceo", false)
			if running := e.SharedServers(); slices.Contains(running, "tracker") {
				t.Errorf("the server turned off is still running: %v", running)
			}
			if names := surfaceOf(t, e, "cto"); slices.Contains(names, "tracker_probe") {
				t.Errorf("the peer seat still has the server turned off: %v", names)
			}

			if _, _, err := e.Apply(t.Context(), parsedCompany(t, on), time.Now()); err != nil {
				t.Fatalf("Apply turning it back on: %v", err)
			}
			waitSurface(t, e, "ceo", true)
			if !shared {
				// ITS OWN CREDENTIAL, which the seat's block kept while
				// the server was off.
				if got := describes(t, e, "ceo", "tracker_probe"); !strings.Contains(got, "ceo-secret") {
					t.Errorf("turned back on, the seat's child runs with %q, want its own credential", got)
				}
			}
		})
	}
}

// waitSurface waits for one seat's surface to hold the tracker's tool, or not.
//
// A seat's children are reconciled by its own pass, which an apply asks for
// and does not wait on, so its surface follows the revision shortly after the
// apply returns.
func waitSurface(t *testing.T, e *engine.Engine, handle string, want bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if slices.Contains(surfaceOf(t, e, handle), "tracker_probe") == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("seat %q: holds tracker_probe = %v after 30s, want %v; surface = %v",
		handle, !want, want, surfaceOf(t, e, handle))
}

// A SERVER'S DESCRIPTION IS LISTED BESIDE ITS NAME, which is all a seat's turn
// is shown of a server before it asks for the server's tools. An edit to it
// reaches the next turn without restarting the server: what a server is for
// is nothing its process reads.
func TestAServerDescriptionIsListedBesideItsName(t *testing.T) {
	t.Parallel()
	for _, shared := range []bool{true, false} {
		name := "per seat"
		if shared {
			name = "shared"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			describe := func(text string) string {
				return trackerEntry(mcpCompany(shared, "SEAT_TOKEN"), "    description: "+text+"\n")
			}
			e := newEngine(t, engine.Options{Company: parsedCompany(t, describe("Reads and files tracker issues"))})
			claimed(t, e, 2)
			listed := func() string {
				return e.ToolsFor("ceo").Snapshot().Discoverable(nil)
			}
			const first = "- MCP server `tracker`: Reads and files tracker issues"
			if got := listed(); !strings.Contains(got, first) {
				t.Fatalf("the seat's server list is\n%s\nwant the line %q", got, first)
			}
			before, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
			if !ok {
				t.Fatal("the seat has no tracker tool to compare")
			}

			if _, _, err := e.Apply(t.Context(), parsedCompany(t, describe("Searches tracker issues")), time.Now()); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			const second = "- MCP server `tracker`: Searches tracker issues"
			deadline := time.Now().Add(30 * time.Second)
			for !strings.Contains(listed(), second) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if got := listed(); !strings.Contains(got, second) {
				t.Fatalf("after the edit the seat's server list is\n%s\nwant the line %q", got, second)
			}
			after, ok := e.ToolsFor("ceo").Lookup("tracker_probe")
			if !ok {
				t.Fatal("the edit took the seat's tracker tool away")
			}
			if after.Tool != before.Tool {
				t.Error("editing a description restarted the server: the tool object behind it changed")
			}
		})
	}
}
