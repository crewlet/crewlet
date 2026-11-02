package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// AN OUTAGE IS DATED FROM WHEN IT BEGAN, across the retries and applies that
// replace an instance's outcome while it stays down: an alarm measures how
// long a server has been failing, and a clock reset by every retry would
// never let it fire. A start that succeeded carries no date of its own (its
// bridge says since when it serves), and an instance that was serving before
// begins its outage now.
func TestAnOutageIsDatedFromWhenItBeganAcrossRetries(t *testing.T) {
	t.Parallel()
	swe := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	pm := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	began := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	now := began.Add(5 * time.Minute)
	prev := []mcpOutcome{
		{server: "tracker", seat: swe, err: errors.New("401"), since: began},
		{server: "tracker", seat: pm},
	}
	next := []mcpOutcome{
		{server: "tracker", seat: swe, err: errors.New("403 this time")},
		{server: "tracker", seat: pm, err: errors.New("exited")},
		{server: "chat", seat: swe},
	}
	stampSince(prev, next, now)
	if !next[0].since.Equal(began) {
		t.Errorf("an instance still down is dated %s, want %s when it went down", next[0].since, began)
	}
	if !next[1].since.Equal(now) {
		t.Errorf("an instance that was serving is dated %s, want %s", next[1].since, now)
	}
	if !next[2].since.IsZero() {
		t.Errorf("a start that succeeded is dated %s", next[2].since)
	}
}

// THE HEARTBEAT'S ROW SAYS SINCE WHEN THE SERVER HAS BEEN DOWN ON THE NODE,
// AND WHY IN A CLASS: the earliest outage of its failed instances, and the
// class of the failure the row names (its first by seat id). A waiting
// instance is no outage, and a serving one has none.
func TestTheHeartbeatRowDatesTheOutageAndClassesItsFailure(t *testing.T) {
	t.Parallel()
	swe := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	pm := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	cto := uuid.MustParse("00000000-0000-7000-8000-000000000003")
	early := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	late := early.Add(10 * time.Minute)
	dns := fmt.Errorf("mcp: server %q failed to connect: %w", "tracker",
		&net.DNSError{Err: "no such host", Name: "tracker.example.invalid", IsNotFound: true})
	e := &Engine{mcpSeats: map[string][]mcpOutcome{
		"swe": {{server: "tracker", seat: swe, err: dns, since: late}},
		"pm":  {{server: "tracker", seat: pm, err: errors.New("exit status 1"), since: early}},
		"cto": {{server: "tracker", seat: cto, tools: 4}},
	}}
	rows := e.mcpStatus(t.Context())
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	row := rows[0]
	if row.Failed != 2 || row.Started != 1 || row.ErrorSeat != swe || row.ErrorClass != string(mcp.FailDNS) {
		t.Errorf("row = %+v, want two failures named by swe's, classed %q", row, mcp.FailDNS)
	}
	if !row.DownSince.Equal(early) {
		t.Errorf("down since %s, want %s, the earliest of its failed instances", row.DownSince, early)
	}

	healthy := (&Engine{mcpShared: []mcpOutcome{{server: "search", tools: 2}}}).mcpStatus(t.Context())
	if len(healthy) != 1 || !healthy[0].DownSince.IsZero() || healthy[0].ErrorClass != "" || healthy[0].Restarts != 0 {
		t.Errorf("a serving server's row = %+v, want no outage, class or restart", healthy)
	}
}

// A SERVER'S INSTANCES ARE READ FROM EVERY NODE HOLDING ONE, each node naming
// its own: the asker reads its own directly, a peer answers over the broker,
// and a node that does not answer is named as not having answered rather than
// read as holding nothing. Each node answers only for the server asked, and
// a failure's sentence crosses the broker redacted and bounded, since it
// carries what a server's process printed.
func TestAServersInstancesAreReadFromEveryNodeHoldingOne(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	client := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	ceo := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	cto := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	went := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	token := "ghp_" + strings.Repeat("b", 36)
	refused := fmt.Errorf("mcp: server %q failed to connect: the server refused %s: %w", "tracker", token,
		errors.New(strings.Repeat("x", 2*coord.MaxMCPErrorBytes)))
	asker := &Engine{testNode: "node-a", backends: &Backends{Queue: client()}, mcpSeats: map[string][]mcpOutcome{
		"ceo": {{server: "tracker", seat: ceo, err: refused, since: went}},
	}}
	peer := &Engine{testNode: "node-b", backends: &Backends{Queue: client()}, mcpSeats: map[string][]mcpOutcome{
		"cto": {
			{server: "tracker", seat: cto, tools: 7},
			{server: "chat", seat: cto, tools: 3},
		},
	}}
	peer.startMCPInstances(t.Context())
	t.Cleanup(func() { peer.stopMCPInstances(context.WithoutCancel(t.Context())) })

	read, err := asker.MCPInstances(t.Context(), "tracker", []string{"node-c", "node-b", "node-a", "node-b"})
	if err != nil {
		t.Fatalf("MCPInstances: %v", err)
	}
	if len(read.Nodes) != 3 {
		t.Fatalf("nodes = %+v, want each node asked once, in id order", read.Nodes)
	}
	if a := read.Nodes[0]; a.Node != "node-a" || !a.Answered || a.Count != 1 {
		t.Errorf("the asker = %+v, want its own read, one instance", a)
	}
	if b := read.Nodes[1]; b.Node != "node-b" || !b.Answered || b.Count != 1 {
		t.Errorf("the peer = %+v, want answered, one instance of tracker", b)
	}
	if c := read.Nodes[2]; c.Node != "node-c" || c.Answered || c.Reason == "" {
		t.Errorf("a node that serves nothing = %+v, want named as not answering, and why", c)
	}
	if len(read.Instances) != 2 {
		t.Fatalf("instances = %+v, want one from each node holding one", read.Instances)
	}
	failed, running := read.Instances[0], read.Instances[1]
	if failed.Node != "node-a" || failed.Seat != ceo || failed.State != coord.MCPInstanceFailed ||
		failed.Failure == nil || !failed.Since.Equal(went) {
		t.Errorf("the asker's instance = %+v, want ceo's, failed since it went down", failed)
	}
	if failed.Failure != nil && (strings.Contains(failed.Failure.Said, token) ||
		len(failed.Failure.Said) > coord.MaxMCPErrorBytes) {
		t.Errorf("a failure crossed the broker as %q: want it redacted and within %d bytes",
			failed.Failure.Said, coord.MaxMCPErrorBytes)
	}
	if running.Node != "node-b" || running.Server != "tracker" || running.Seat != cto ||
		running.State != coord.MCPInstanceRunning || running.Tools != 7 || running.Failure != nil {
		t.Errorf("the peer's instance = %+v, want cto's tracker, running with its seven tools", running)
	}
}
