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
	"github.com/crewlet/crewlet/internal/coord/coordtest"
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

// AN MCP SERVER'S CONDITION IS THE FLEET'S, JOINED FROM EVERY NODE'S STATUS RECORD:
// its instances summed over the nodes; the instance down longest anywhere, with
// its node, its agent by handle, its class and what it said; the wait held
// longest likewise; and its refused names once across the fleet, counted as the
// node counting the most counts them. A server turned off has no condition, so
// whatever a node still runs of it raises nothing.
//
// Mutation: keep one node's outage rather than the earliest, name the agent by
// its id, sum the refusal counts, or judge a server turned off, and this fails.
func TestAnMCPServersConditionIsTheFleetsStatusRecords(t *testing.T) {
	t.Parallel()
	c := mcpAlarmFixture(t)
	backend, pm := seatID(t, c, "backend-dev"), seatID(t, c, "pm")
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	early, late := now.Add(-time.Hour), now.Add(-time.Minute)
	statuses := map[string]coord.NodeStatusRecord{
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

	got := mcpConditions(c, statuses, nil)
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

// flakyRecords is a status record store whose listing fails while fail is set.
type flakyRecords struct {
	innerFleet
	fail atomic.Bool
}

func (f *flakyRecords) NodeStatusRecords(ctx context.Context, read coord.NodeStatusRead) ([]coord.NodeStatusRecord,
	error) {
	if f.fail.Load() {
		return nil, errors.New("the coordination store did not answer")
	}
	return f.innerFleet.NodeStatusRecords(ctx, read)
}

// A FAILED LEASE READ CARRIES THE LAST GOOD ONE for the MCP servers too: a
// blip must not read as every server serving, which cleared every standing
// outage on the blip and raised it again a beat later, two log lines per server
// per blip and a health count that flickered. And so does a failed read of the
// status records the leases are joined to, for the same reason.
//
// Mutation: answer an unreadable lease table, or unreadable records, as an
// empty fleet, and the outage is lost on the blip.
func TestAFailedLeaseReadKeepsTheMCPConditions(t *testing.T) {
	t.Parallel()
	c := mcpAlarmFixture(t)
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	leases := &flakyLeases{Backend: coordmemory.New()}
	presence, _, err := leases.TryAcquire(t.Context(), coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute,
	})
	if err != nil || presence == nil {
		t.Fatalf("claim node-a's presence: %v, %v", presence, err)
	}
	fleet := &flakyRecords{innerFleet: coordmemory.NewFleet()}
	if err := coordtest.PutNodeStatus(t.Context(), fleet, coord.NodeStatusRecord{Node: "node-a", Epoch: presence.Epoch,
		MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Failed: 1, DownSince: now.Add(-time.Hour), Error: "exit status 1"},
		}}); err != nil {
		t.Fatal(err)
	}
	e := epochEngine(c)
	e.backends, e.testNode = &Backends{Coord: leases, Fleet: fleet}, "node-a"

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
	leases.fail.Store(false)
	fleet.fail.Store(true)
	var third statelog.Reading
	e.entriesReading(t.Context(), now.Add(2*statelog.AlarmInterval), &third)
	if len(third.MCPServers) != 2 || third.MCPServers[0].Failed != 1 ||
		!third.MCPServers[0].DownSince.Equal(first.MCPServers[0].DownSince) {
		t.Errorf("after a failed read of the status records the servers are %+v, want the last good %+v",
			third.MCPServers, first.MCPServers)
	}
}

// claimedAgain releases node's presence, held at epoch by the owner [liveNode]
// claims it under, and claims it again: the node restarted, and its new
// incarnation holds the presence at the epoch it returns.
func claimedAgain(t *testing.T, backend coord.Backend, node string, epoch int64) int64 {
	t.Helper()
	if released, err := backend.Release(t.Context(), coord.NodeResource(node), node+":1", epoch); err != nil ||
		!released {
		t.Fatalf("%s's release = (%v, %v)", node, released, err)
	}
	again := liveNode(t, backend, node)
	if again == epoch {
		t.Fatalf("the premise: %s claimed its presence again at its old epoch %d", node, epoch)
	}
	return again
}

// A LIVE NODE WHOSE STATUS RECORD IS NOT COUNTED KEEPS THE ONE THE MCP ALARMS
// LAST READ while its presence stands at the epoch it wrote it under: a record
// that aged out while its node lives is a node not saying, and read as one with
// no rows, an outage only that node held cleared for one evaluation and was
// raised again on the next, with its lines and its wakes. A node that claimed
// its presence again is a new incarnation, and keeps nothing its earlier one
// said.
//
// Mutation: read a live node whose record is not counted as one with no rows,
// or keep a node claimed again at a new epoch, and a step goes red.
func TestAnMCPOutageAnUncountedLiveNodeHoldsStandsOnItsLastRecord(t *testing.T) {
	t.Parallel()
	live := t.Context()
	c := mcpAlarmFixture(t)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	leases := coordmemory.New()
	fleet := coordmemory.NewFleet()
	epoch := liveNode(t, leases, "node-b")
	if err := coordtest.PutNodeStatus(live, fleet, coord.NodeStatusRecord{Node: "node-b", Epoch: epoch, At: now,
		MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Failed: 1, DownSince: now.Add(-time.Hour), Error: "exit status 1"},
		}}); err != nil {
		t.Fatalf("node-b's record: %v", err)
	}
	e := epochEngine(c)
	e.backends, e.testNode = &Backends{Coord: leases, Fleet: fleet}, "node-a"
	down := func(step string, want int) {
		t.Helper()
		var got statelog.Reading
		e.entriesReading(live, now, &got)
		for _, server := range got.MCPServers {
			if server.Server == "docs" {
				if server.Failed != want {
					t.Fatalf("%s: docs has %d instance(s) down, want %d", step, server.Failed, want)
				}
				return
			}
		}
		t.Fatalf("%s: no condition for docs in %+v", step, got.MCPServers)
	}
	down("node-b's record counted", 1)
	if err := fleet.DeleteNodeStatus(live, "node-b", epoch); err != nil {
		t.Fatalf("remove node-b's record: %v", err)
	}
	down("node-b's record aged out while it lives", 1)
	claimedAgain(t, leases, "node-b", epoch)
	down("node-b claimed its presence again", 0)
}

// THIS NODE'S OWN MCP ROWS ARE ITS MEMORY WHERE NO RECORD OF ITS OWN COUNTS,
// never a record of its own kept as a peer's is: a live peer whose record aged
// out keeps its last one because it is not saying ([coord.LiveNodeStatusesKept]),
// but this node holds what its record is written from, and kept, an outage its
// servers no longer had stood raised for as long as its record's writes failed.
//
// Mutation: keep this node's own last record as a peer's is kept, and the
// outage stays raised once its memory holds none.
func TestThisNodesOwnMCPRowsAreItsMemoryWhereNoRecordOfItsCounts(t *testing.T) {
	t.Parallel()
	live := t.Context()
	c := mcpAlarmFixture(t)
	now := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	leases := coordmemory.New()
	fleet := coordmemory.NewFleet()
	epoch := liveNode(t, leases, "node-a")
	if err := coordtest.PutNodeStatus(live, fleet, coord.NodeStatusRecord{Node: "node-a", Epoch: epoch, At: now,
		MCP: []coord.MCPServerStatus{
			{Server: "docs", Shared: true, Failed: 1, DownSince: now.Add(-time.Hour), Error: "exit status 1"},
		}}); err != nil {
		t.Fatalf("node-a's record: %v", err)
	}
	e := epochEngine(c)
	e.backends, e.testNode = &Backends{Coord: leases, Fleet: fleet}, "node-a"
	if rows := e.mcpStatus(live); len(rows) != 0 {
		t.Fatalf("the premise: this node's own servers say %+v", rows)
	}
	down := func(step string, want int) {
		t.Helper()
		var got statelog.Reading
		e.entriesReading(live, now, &got)
		for _, server := range got.MCPServers {
			if server.Server == "docs" {
				if server.Failed != want {
					t.Fatalf("%s: docs has %d instance(s) down, want %d", step, server.Failed, want)
				}
				return
			}
		}
		t.Fatalf("%s: no condition for docs in %+v", step, got.MCPServers)
	}
	down("this node's record counted", 1)
	if err := fleet.DeleteNodeStatus(live, "node-a", epoch); err != nil {
		t.Fatalf("remove node-a's record: %v", err)
	}
	down("this node's record aged out while it lives", 0)
}
