package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// twoSeatCompany is a company whose one per-seat MCP server, `tracker`, is
// the suite's helper tool server granted to the CEO and the CTO, started with
// the extra environment env.
func twoSeatCompany(t *testing.T, env map[string]string) *Company {
	t.Helper()
	full := map[string]string{
		ToolServerModeEnv:     "server",
		ToolServerLaunchesEnv: t.TempDir(),
		procgrouptest.GORACE:  procgrouptest.StandInRaceOptions(),
	}
	for k, v := range env {
		full[k] = v
	}
	var block strings.Builder
	for k, v := range full {
		fmt.Fprintf(&block, "      %s: %q\n", k, v)
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
    shared: false
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
  - name: CTO
    handle: cto
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "cto-token"
`, os.Args[0], block.String())))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	return company
}

// childOf is one seat's tracker child as its bridge sees it now.
func childOf(s *seatChildren, role string) mcp.Health {
	h, _ := s.bridge.Health(mcp.InstanceName("tracker", role))
	return h
}

// A RESTART REQUEST NAMING A SEAT REACHES THE SEAT'S HOLDER ONLY. Every node
// hears the record, and only the node running the seat's child, which can
// prove it holds the seat, restarts it: another node's children of the same
// server are left as they are. A request for every instance restarts each
// node's own. And a record heard again restarts nothing more: it is a stamp,
// and the child came up after it.
func TestARestartRequestReachesTheSeatsHolderOnly(t *testing.T) {
	t.Parallel()
	company := twoSeatCompany(t, nil)
	holder := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 50 * time.Millisecond}
	holder.epoch.current.Store(company)
	ceo := heldSeatChildren(t, holder, "ceo", func() bool { return true })
	holder.convergeSeat(t.Context(), ceo, false)
	peer := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: 50 * time.Millisecond}
	peer.epoch.current.Store(company)
	cto := heldSeatChildren(t, peer, "cto", func() bool { return true })
	peer.convergeSeat(t.Context(), cto, false)
	if !childOf(ceo, "CEO").Serving || !childOf(cto, "CTO").Serving {
		t.Fatalf("precondition: both children serving: %+v, %+v", childOf(ceo, "CEO"), childOf(cto, "CTO"))
	}
	ceoID, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	ctoSince := childOf(cto, "CTO").Since

	asked := coord.MCPRestart{Server: "tracker", Seat: ceoID, At: time.Now(), By: "jane.doe",
		ByKind: iam.ActorHuman}
	holder.applyMCPRestart(t.Context(), asked)
	peer.applyMCPRestart(t.Context(), asked)
	pollUntil(t, 30*time.Second, "the CEO's child to be restarted", func() bool {
		h := childOf(ceo, "CEO")
		return h.Serving && h.Since.After(asked.At)
	})
	if h := childOf(cto, "CTO"); !h.Since.Equal(ctoSince) || h.Restarts != 0 {
		t.Errorf("a node that does not hold the seat restarted its own child: %+v", h)
	}

	// HEARD AGAIN, a record restarts nothing more.
	holder.restarts.mu.Lock()
	delete(holder.restarts.applied, asked.Entry())
	holder.restarts.mu.Unlock()
	holder.applyMCPRestart(t.Context(), asked)
	time.Sleep(200 * time.Millisecond)
	if h := childOf(ceo, "CEO"); h.Restarts != 1 {
		t.Errorf("the same request read twice restarted the child %d times, want once", h.Restarts)
	}

	every := coord.MCPRestart{Server: "tracker", At: time.Now(), By: "jane.doe", ByKind: iam.ActorHuman}
	peer.applyMCPRestart(t.Context(), every)
	pollUntil(t, 30*time.Second, "the CTO's child to be restarted by a request for every instance", func() bool {
		h := childOf(cto, "CTO")
		return h.Serving && h.Since.After(every.At)
	})
}

// A RESTART TRIES A SEAT'S CHILD WHOSE FIRST START FAILED NOW, AND STARTS ITS
// SCHEDULE AGAIN. No bridge keeps such a child (the seat's runner retries it),
// so the request asks the runner for a pass now. The schedule's base here is an
// hour: a child that comes up within the test came up because it was asked.
func TestARestartTriesAFailedSeatChildNowAndStartsItsScheduleAgain(t *testing.T) {
	t.Parallel()
	ready := filepath.Join(t.TempDir(), "vendor-up")
	company := twoSeatCompany(t, map[string]string{ToolServerReadyEnv: ready})
	e := &Engine{mcp: mcp.NewBridge(nil, mcp.Supervision{}), mcpRetryBase: time.Hour}
	e.epoch.current.Store(company)
	ceo := heldSeatChildren(t, e, "ceo", func() bool { return true })
	e.convergeSeat(t.Context(), ceo, false)
	outcomes := seatOutcomes(e, "ceo")
	if len(outcomes) != 1 || outcomes[0].err == nil || outcomes[0].since.IsZero() {
		t.Fatalf("precondition: the first start failed and is dated: %+v", outcomes)
	}
	if err := os.WriteFile(ready, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ceoID, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	e.applyMCPRestart(t.Context(), coord.MCPRestart{Server: "tracker", Seat: ceoID, At: time.Now(),
		By: "jane.doe", ByKind: iam.ActorHuman})
	pollUntil(t, 30*time.Second, "the child, tried now", func() bool { return childOf(ceo, "CEO").Serving })
}

// A RESTART IS REFUSED FOR WHAT IT CANNOT RESTART, naming why: a server the
// configuration does not declare or turns off, a seat named for a shared
// server, a seat nobody holds, and one the server is not granted to. A request
// it accepts is recorded with its stamp, for every node to hear.
func TestARestartIsRefusedForWhatItCannotRestart(t *testing.T) {
	t.Parallel()
	company := companyFor(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
mcp_servers:
  - name: search
    command: search-server
  - name: tracker
    shared: false
    command: tracker-server
  - name: wiki
    enabled: false
    command: wiki-server
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    mcp_env:
      tracker:
        SEAT_TOKEN: "ceo-token"
  - name: CTO
    handle: cto
    llm: zulu
`)
	fleet := coordmem.NewFleet()
	now := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	e := &Engine{backends: &Backends{Fleet: fleet}, testNode: "node-a", clock: func() time.Time { return now }}
	e.epoch.current.Store(company)
	jane := iam.Actor{Name: "jane.doe", Kind: iam.ActorHuman, OperatorID: "session:abc"}
	for _, tc := range []struct {
		server, seat string
		want         error
	}{
		{"nothing", "", ErrNoMCPServer},
		{"wiki", "", ErrMCPServerOff},
		{"search", "ceo", ErrMCPSeatOnShared},
		{"tracker", "nobody", ErrNoAgentSeat},
		{"tracker", "cto", ErrMCPSeatNotGranted},
	} {
		if _, err := e.RestartMCPServer(t.Context(), tc.server, tc.seat, jane); !errors.Is(err, tc.want) {
			t.Errorf("restart %s (seat %q) = %v, want %v", tc.server, tc.seat, err, tc.want)
		}
	}
	if all, _ := fleet.ListMCPRestarts(t.Context()); len(all) != 0 {
		t.Fatalf("a refused request was recorded: %+v", all)
	}

	got, err := e.RestartMCPServer(t.Context(), "tracker", "ceo", jane)
	if err != nil || got.Server != "tracker" || got.Seat != "ceo" || !got.At.Equal(now) || got.Node != "node-a" {
		t.Fatalf("restart = %+v, %v", got, err)
	}
	ceoID, _ := company.Org.AgentIDFor(company.Org.AgentSeatByHandle("ceo"))
	stored, found, err := fleet.MCPRestart(t.Context(), coord.MCPRestartEntry("tracker", ceoID))
	if err != nil || !found || stored.By != "jane.doe" || stored.OperatorID != "session:abc" || !stored.At.Equal(now) {
		t.Errorf("recorded %+v (found %v, %v), want jane's request at its stamp", stored, found, err)
	}
	if _, err := e.RestartMCPServer(t.Context(), "search", "", jane); err != nil {
		t.Errorf("a restart of every instance of a shared server: %v", err)
	}
}

// A RESTART GOES WITH WHAT IT RESTARTED. The record has no age, so without a
// published company clearing it a removed server's restart would stay for
// ever: the apply that removes a server, or a seat, deletes the requests
// naming it. Only the fleet's current company clears one, as it clears a
// removed seat's pause.
func TestARestartGoesWithTheServerOrSeatItNamed(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	e := &Engine{backends: &Backends{Fleet: fleet}}
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman, Source: "api", CreatedAt: time.Now()}
	current, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	ghost := uuid.MustParse("00000000-0000-7000-8000-0000000000ff")
	at := time.Now()
	for _, r := range []coord.MCPRestart{
		{Server: "tracker", At: at, By: "jane.doe", ByKind: iam.ActorHuman},
		{Server: "tracker", Seat: seatOf("ceo"), At: at, By: "jane.doe", ByKind: iam.ActorHuman},
		{Server: "tracker", Seat: ghost, At: at, By: "jane.doe", ByKind: iam.ActorHuman},
		{Server: "removed", At: at, By: "jane.doe", ByKind: iam.ActorHuman},
	} {
		if _, err := fleet.PutMCPRestart(t.Context(), r); err != nil {
			t.Fatal(err)
		}
	}
	next := companyFor(t, `
name: Acme
mcp_servers:
  - name: tracker
    shared: false
    command: tracker-server
roles:
  - name: CEO
    handle: ceo
`)
	e.clearRemovedMCPRestarts(t.Context(), next)
	if all, _ := fleet.ListMCPRestarts(t.Context()); len(all) != 4 {
		t.Fatalf("a company no activation named cleared restarts: %+v", all)
	}
	next.ActivatedAt = current.At
	e.clearRemovedMCPRestarts(t.Context(), next)
	all, err := fleet.ListMCPRestarts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, r := range all {
		kept = append(kept, r.Entry())
	}
	want := []string{"tracker", coord.MCPRestartEntry("tracker", seatOf("ceo"))}
	if fmt.Sprint(kept) != fmt.Sprint(want) {
		t.Errorf("restarts kept %v, want %v: the server's and the seat it still has", kept, want)
	}
}
