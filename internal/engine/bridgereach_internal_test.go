package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// hostOnlyBridge is the bridge a seats node builds from its own listener
// alone, as `crewlet run` builds it for the quickstart's company: an address
// on loopback for a box on this host, and none for any other.
func hostOnlyBridge(t *testing.T) *mcpbridge.Bridge {
	t.Helper()
	return bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), HostURL: "http://127.0.0.1:8080",
	})
}

// A DIRECT BOX DIALS THE NODE'S OWN LISTENER, AND A BOX ELSEWHERE IS REFUSED
// NAMING THE VARIABLE IT NEEDS. The quickstart's company is one `direct` cell
// and no CREWLET_MCP_BRIDGE_URL, and every turn of it used to be refused for
// the variable; a box on the host can always dial the host's own socket, so it
// launches on that. A box in any other cell cannot, so its refusal names the
// variable and the cell, never api.port, which is set, and is classed as the
// configuration it is: the variable is the operator's to set, and no wait sets
// it.
//
// Mutation: open every session off the host and the direct row is refused;
// open every session on it and the e2b row launches a box that dials a
// loopback it cannot reach; drop the cell refusal's class and the e2b row
// goes red.
func TestALaunchDialsTheBridgeWhereItsCellReachesIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		runIn  config.Placement
		refuse bool
	}{
		"a direct box":  {runIn: config.PlacementDirect},
		"an e2b box":    {runIn: config.PlacementE2B, refuse: true},
		"the default's": {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code",
				model: "claude-golden", runIn: tc.runIn})
			e, jobs := launchRig(t, c, hostOnlyBridge(t))
			l := launcherOn(e, c, seat, "t1")
			_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
			if tc.refuse {
				if err == nil {
					t.Fatal("a box off this host launched with only a loopback address to dial")
				}
				if !strings.Contains(err.Error(), mcpbridge.BaseURLVar) ||
					!strings.Contains(err.Error(), string(tc.runIn)) ||
					strings.Contains(err.Error(), "api.port") {
					t.Errorf("the refusal does not name the variable and the cell: %v", err)
				}
				if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
					t.Errorf("the refusal is classed %q (%v), want %q", why, ok, sandbox.WhyConfiguration)
				}
				refusedCleanly(t, e, "t1", jobs)
				return
			}
			if err != nil {
				t.Fatalf("a direct box was refused with the node's own listener to dial: %v", err)
			}
			started := jobs.Started()
			if len(started) != 1 {
				t.Fatalf("started %d jobs, want one", len(started))
			}
			var dialed string
			for _, server := range started[0].MCPServers {
				if strings.Contains(server.URL, mcpbridge.PathPrefix) {
					dialed = server.URL
				}
			}
			if !strings.HasPrefix(dialed, "http://127.0.0.1:8080"+mcpbridge.PathPrefix) {
				t.Errorf("the box dials the bridge at %q, want the node's own listener", dialed)
			}
		})
	}
}

// A CONTAINER CELL'S FIX SAYS WHAT A LOOPBACK ADDRESS NEEDS THERE. The line
// tells the operator to set the fleet's address, and that a loopback one
// reaches a container box on this host under its runtime's name for the host,
// which is so wherever the bridge hears that name: on Docker Desktop and
// Podman's machine it arrives on the host's loopback, which the bridge admits,
// and on Linux it arrives from the bridge network, which a loopback bind never
// hears. A line without that condition sent an operator on Linux to a value
// that leaves every executor without its tools.
//
// Mutation: drop the Linux clause from the fix and the bind goes unnamed.
func TestAContainerCellsFixSaysWhatALoopbackAddressNeeds(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(`name: Acme
providers:
  sandbox:
    local: {image: example.com/coding:1}
    default_run_in: container
  llm:
    main:
      model: claude-golden
roles:
  - name: SWE
    llm: main
`))
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	gaps := bridgeGaps(cfg, false, hostOnlyBridge(t))
	if len(gaps) != 1 || gaps[0].cell != config.PlacementContainer {
		t.Fatalf("gaps = %+v, want the container cell alone", gaps)
	}
	for _, want := range []string{mcpbridge.BaseURLVar, "runtime's name for the host",
		"on Linux", "beyond loopback", "api.host", "api.public.host"} {
		if !strings.Contains(gaps[0].fix, want) {
			t.Errorf("the container cell's fix does not say %q: %s", want, gaps[0].fix)
		}
	}
}

// bridgeReachCompany is a company with one seat whose executor can land in
// the direct cell (its head) and the e2b cell (its fallback), and one whose
// executor lands in direct alone.
const bridgeReachCompany = `name: Acme
providers:
  sandbox:
    local: {}
    e2b: {api_key: k}
    default_run_in: direct
  llm:
    main:
      model: claude-golden
    remote:
      model: sonnet
      run_in: e2b
roles:
  - name: SWE
    llm: [main, remote]
  - name: PM
    llm: main
`

// A NODE WITH NO BRIDGE AT ALL REFUSES AN EXECUTOR, naming the fleet's
// address, and opens nothing: a box with none of the seat's tools would burn
// its run producing prose nothing collects. Classed as configuration, which an
// operator's setting clears and no wait does.
//
// Mutation: drop the class from the refusal and this goes red.
func TestAnExecutorIsRefusedOnANodeWithNoBridge(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	e, jobs := launchRig(t, c, hostOnlyBridge(t))
	e.bridge = nil
	l := launcherOn(e, c, seat, "t1")
	_, err := l.LaunchExecutor(t.Context(), launchRequest(l, nil))
	if err == nil || !strings.Contains(err.Error(), mcpbridge.BaseURLVar) {
		t.Fatalf("a launch on a node with no bridge = %v, want it refused naming %s",
			err, mcpbridge.BaseURLVar)
	}
	if why, ok := sandbox.WhyOf(err); !ok || why != sandbox.WhyConfiguration {
		t.Errorf("the refusal is classed %q (%v), want %q", why, ok, sandbox.WhyConfiguration)
	}
	refusedCleanly(t, e, "t1", jobs)
}

// AN UNREACHABLE CELL IS SAID ONCE, AT APPLY, PER CELL. Every launch in it is
// refused naming the same fix, and the line that says so before the first turn
// is the one an operator needs — so each cell the company's executors launch
// in is weighed against the fleet's reach rule: nothing where this node serves
// the API on no listener, the fleet's address alone where it has no bridge, a
// direct cell on the node's own listener, any other cell on the variable
// alone. A fallback member's cell is a cell a launch reaches too.
//
// Mutation: weigh only the catalogue's default and the e2b row is never said;
// give every cell the host address and the e2b row reads as reachable.
func TestEveryCellAnExecutorCannotReachIsSaidOnceAtApply(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(bridgeReachCompany))
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}
	both := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), HostURL: "http://127.0.0.1:8080",
		BaseURL: "https://engine.example.com",
	})
	for name, tc := range map[string]struct {
		offline bool
		bridge  *mcpbridge.Bridge
		want    map[config.Placement]string
	}{
		"an ingress node on api.port 0, the variable set": {
			offline: true, bridge: bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}),
			want: map[config.Placement]string{config.PlacementDirect: "api.port",
				config.PlacementE2B: "api.port"},
		},
		"a node with no bridge": {
			bridge: nil,
			want: map[config.Placement]string{config.PlacementDirect: mcpbridge.BaseURLVar,
				config.PlacementE2B: mcpbridge.BaseURLVar},
		},
		"its own listener alone": {
			bridge: hostOnlyBridge(t),
			want:   map[config.Placement]string{config.PlacementE2B: mcpbridge.BaseURLVar},
		},
		"its own listener and the variable": {
			bridge: both, want: map[config.Placement]string{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			gaps := bridgeGaps(cfg, tc.offline, tc.bridge)
			got := map[config.Placement]string{}
			for _, gap := range gaps {
				if _, twice := got[gap.cell]; twice {
					t.Errorf("the %s cell is said twice", gap.cell)
				}
				got[gap.cell] = gap.fix
			}
			if len(got) != len(tc.want) {
				t.Fatalf("gaps %+v, want one for each of %v", gaps, tc.want)
			}
			for cell, names := range tc.want {
				if !strings.Contains(got[cell], names) {
					t.Errorf("the %s cell's fix is %q, want it to name %s", cell, got[cell], names)
				}
			}
			for _, gap := range gaps {
				if gap.cell == config.PlacementDirect && gap.seats != 2 {
					t.Errorf("the direct cell counts %d seats, want both", gap.seats)
				}
			}
		})
	}
}
