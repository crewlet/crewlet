package engine

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// mcpAlarmCompany runs a per-agent server, a shared one, and a third turned
// off.
const mcpAlarmCompany = `
name: Acme
roles:
  - name: Backend Dev
    handle: backend-dev
    mcp_env:
      github:
        GITHUB_TOKEN: ${GITHUB_TOKEN_BACKEND}
  - name: PM
    handle: pm
    mcp_env:
      github:
        GITHUB_TOKEN: ${GITHUB_TOKEN_PM}
mcp_servers:
  - name: github
    shared: false
    command: npx
    args: ["-y", "@modelcontextprotocol/server-github"]
  - name: docs
    command: docs-mcp
  - name: archive
    enabled: false
    command: archive-mcp
`

// mcpAlarmFixture is the company above, built.
func mcpAlarmFixture(t *testing.T) *Company {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(mcpAlarmCompany))
	if err != nil {
		t.Fatal(err)
	}
	organization, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	return &Company{Config: cfg, Org: organization}
}

// seatID is handle's agent id in c.
func seatID(t *testing.T, c *Company, handle string) uuid.UUID {
	t.Helper()
	id, ok := c.Org.AgentIDFor(c.Org.AgentSeatByHandle(handle))
	if !ok {
		t.Fatalf("no seat %s", handle)
	}
	return id
}

// AN MCP SERVER'S CONDITION IS THE FLEET'S, JOINED FROM EVERY NODE'S HEARTBEAT:
// its instances summed over the nodes; the instance down longest anywhere, with
// its node, its agent by handle, its class and what it said; the wait held
// longest likewise; and its refused names once across the fleet, counted as the
// node counting the most counts them. A server turned off has no condition, so
// whatever a node still runs of it raises nothing.
//
// Mutation: keep one node's outage rather than the earliest, name the agent by
// its id, sum the refusal counts, or judge a server turned off, and this fails.
func TestAnMCPServersConditionIsTheFleetsHeartbeats(t *testing.T) {
	t.Parallel()
	c := mcpAlarmFixture(t)
	backend, pm := seatID(t, c, "backend-dev"), seatID(t, c, "pm")
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	early, late := now.Add(-time.Hour), now.Add(-time.Minute)
	statuses := map[string]coord.NodeStatus{
		"node-a": {MCP: []coord.MCPServerStatus{
			{Server: "archive", Shared: true, Failed: 1, DownSince: early, Error: "gone"},
			{Server: "github", Started: 1, Failed: 1, DownSince: early, ErrorSeat: backend,
				ErrorClass: "credential_rejected", Error: "401 Bad credentials",
				Refused: []coord.MCPToolRefusal{{Tool: "gh_a", Reason: "too_long"}}, RefusedCount: 9},
		}},
		"node-b": {MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Started: 1, Tools: 3},
			{Server: "github", Failed: 1, DownSince: late, ErrorSeat: pm, ErrorClass: "exited",
				Error: "exit status 1", Waiting: 1, WaitingSince: early.Add(-time.Hour), WaitSeat: pm,
				Wait:         "not started: waiting for the github integration",
				Refused:      []coord.MCPToolRefusal{{Tool: "gh_b", Reason: "builtin"}, {Tool: "gh_a", Reason: "too_long"}},
				RefusedCount: 2},
		}},
		"node-c": {},
	}

	got := mcpConditions(c, statuses)
	if len(got) != 2 || got[0].Server != "docs" || got[1].Server != "github" {
		t.Fatalf("conditions = %+v, want docs and github: archive is turned off", got)
	}
	github := got[1]
	if github.Started != 1 || github.Failed != 2 || github.Waiting != 1 {
		t.Errorf("github counts %d started, %d failed, %d waiting; want 1, 2 and 1", github.Started,
			github.Failed, github.Waiting)
	}
	if want := (statelog.MCPInstanceNote{Node: "node-a", Seat: "backend-dev", Class: "credential_rejected",
		Said: "401 Bad credentials"}); !github.DownSince.Equal(early) || github.Down != want {
		t.Errorf("github down since %v as %+v, want %v as %+v, the outage that began first", github.DownSince,
			github.Down, early, want)
	}
	waited := statelog.MCPInstanceNote{Node: "node-b", Seat: "pm",
		Said: "not started: waiting for the github integration"}
	if !github.WaitingSince.Equal(early.Add(-time.Hour)) || github.Wait != waited {
		t.Errorf("github waits since %v as %+v, want the wait held longest", github.WaitingSince, github.Wait)
	}
	if !slices.Equal(github.Refused, []string{"gh_a", "gh_b"}) || github.RefusedCount != 9 {
		t.Errorf("github refused %v of %d, want gh_a and gh_b of 9", github.Refused, github.RefusedCount)
	}
	if docs := got[0]; docs.Started != 1 || docs.Failed+docs.Waiting+docs.RefusedCount != 0 {
		t.Errorf("docs = %+v, want one serving instance and nothing else", docs)
	}

	// AND THE TABLE JUDGES THEM: github serves somewhere while one instance
	// has been down an hour, an agent has waited two, and two names are
	// refused; docs raises nothing.
	kinds := map[statelog.Kind][]string{}
	for _, a := range statelog.Evaluate(statelog.Reading{MCPServers: got, MCPServersAt: now}) {
		for _, e := range a.Entries {
			kinds[a.Kind] = append(kinds[a.Kind], e.Key)
		}
	}
	for _, kind := range []statelog.Kind{statelog.KindMCPServerPartial, statelog.KindMCPServerWaitingLong,
		statelog.KindMCPToolsRefused} {
		if !slices.Equal(kinds[kind], []string{"github"}) {
			t.Errorf("the table raised %v, want %s on github", kinds, kind)
		}
	}
	if len(kinds) != 3 {
		t.Errorf("the table raised %v, want exactly three kinds", kinds)
	}
}

// flakyLeases is a lease table whose listing fails while fail is set.
type flakyLeases struct {
	coord.Backend
	fail atomic.Bool
}

func (f *flakyLeases) ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error) {
	if f.fail.Load() {
		return nil, errors.New("the coordination store did not answer")
	}
	return f.Backend.ListLive(ctx, class)
}

// A FAILED LEASE READ CARRIES THE LAST GOOD ONE for the MCP servers too: a
// blip must not read as every server serving, which cleared every standing
// outage on the blip and raised it again a beat later, two log lines per server
// per blip and a health count that flickered.
//
// Mutation: answer an unreadable lease table as an empty fleet, and the outage
// is lost on the blip.
func TestAFailedLeaseReadKeepsTheMCPConditions(t *testing.T) {
	t.Parallel()
	c := mcpAlarmFixture(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	leases := &flakyLeases{Backend: coordmemory.New()}
	status := coord.NodeStatus{MCP: []coord.MCPServerStatus{
		{Server: "docs", Shared: true, Failed: 1, DownSince: now.Add(-time.Hour), Error: "exit status 1"},
	}}
	if _, _, err := leases.TryAcquire(t.Context(), coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute, Meta: map[string]any{coord.StatusKey: status.Meta()},
	}); err != nil {
		t.Fatal(err)
	}
	e := epochEngine(c)
	e.backends, e.testNode = &Backends{Coord: leases}, "node-a"

	var first, second statelog.Reading
	e.entriesReading(t.Context(), now, &first)
	if len(first.MCPServers) != 2 || first.MCPServers[0].Failed != 1 || !first.MCPServersAt.Equal(now) {
		t.Fatalf("the first reading = %+v at %v, want docs down, read at %v", first.MCPServers,
			first.MCPServersAt, now)
	}
	leases.fail.Store(true)
	e.entriesReading(t.Context(), now.Add(statelog.AlarmInterval), &second)
	if len(second.MCPServers) != 2 || second.MCPServers[0].Failed != 1 ||
		!second.MCPServers[0].DownSince.Equal(first.MCPServers[0].DownSince) {
		t.Errorf("after a failed read the servers are %+v, want the last good %+v", second.MCPServers,
			first.MCPServers)
	}
}
