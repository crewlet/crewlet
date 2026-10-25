package jetstreamtest

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// A MEMBER BROUGHT BACK IS THE SAME MEMBER: on its own route port and under its
// own name, routed to every peer again, counted by the metadata group as the
// voter it was, and serving the replicated records written while it was down —
// what a fleet's node restarting over its own files is, and what the code
// host's multi-node suite kills and restarts a member as.
//
// NOT PARALLEL, for the reason TestAMemberGoneForGoodIsRemovedAfterEverySurvivorRestarted
// gives: the member comes back on the route port it had, and a cluster another
// case reserved ports for in the same moment could take it while it is down.
func TestARestartedMemberRejoinsAsItself(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	leaderOf(t, c)
	const member = 2
	q := c.Client(t, 0)
	spec := js.DomainStream{Name: "CREWLET_RESTART_LOG", Subjects: []string{"crewlet.restart.>"},
		MaxBytes: 1 << 20, Duplicates: time.Minute}
	if err := q.EnsureDomainStream(t.Context(), spec); err != nil {
		t.Fatalf("provision a replicated log: %v", err)
	}
	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	appendN := func(from, to int) {
		t.Helper()
		for n := from; n < to; n++ {
			if _, _, err := log.Append(t.Context(), "crewlet.restart.a", fmt.Sprintf("m-%d", n), nil,
				[]byte(fmt.Sprintf("record %d", n))); err != nil {
				t.Fatalf("append record %d: %v", n, err)
			}
		}
	}
	appendN(0, 5)

	c.StopMember(member)
	// WRITTEN WHILE IT IS DOWN, so what it serves once back is what it
	// caught up on rather than what it held.
	appendN(5, 10)
	back := c.RestartMember(t, member)

	// ITS OWN ROUTE PORT, asked as a peer asks it: what answers on the port its
	// peers dial must be this member, or no peer would ever route to it.
	if got, want := routeListener(t, c.Configs[member].ClusterHost, c.Configs[member].ClusterPort),
		c.Configs[member].ServerName; got != want {
		t.Errorf("the route port its peers dial, %d, is answered by %q, not the restarted member %q",
			c.Configs[member].ClusterPort, got, want)
	}
	others := []string{c.Configs[0].ServerName, c.Configs[1].ServerName}
	deadline := time.Now().Add(30 * time.Second)
	for !routesTo(c.Servers[member].RoutePeers(), others) {
		if time.Now().After(deadline) {
			t.Fatalf("the restarted member routes to %v, want %v", c.Servers[member].RoutePeers(), others)
		}
		time.Sleep(50 * time.Millisecond)
	}
	group := awaitGroup(t, c.Servers[member], func(g js.MetaGroup) bool {
		return g.Leader != "" && len(g.Peers) == 3
	})
	if !group.Counts(js.PeerIDOf(c.Configs[member].ServerName)) {
		t.Errorf("the metadata group no longer counts the restarted member: %+v", group.Peers)
	}

	backLog, err := back.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("open the log through the restarted member: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, last, err := backLog.Bounds(ctx)
	if err != nil || last != 10 {
		t.Fatalf("through the restarted member the log ends at %d (%v), want 10", last, err)
	}
	for seq := uint64(1); seq <= last; seq++ {
		_, payload, _, ok, err := backLog.At(ctx, seq)
		if want := fmt.Sprintf("record %d", seq-1); err != nil || !ok || string(payload) != want {
			t.Errorf("record %d through the restarted member = %q, %v, %v; want %q", seq, payload, ok,
				err, want)
		}
	}
}

// routeListener dials a member's route port as a peer does and answers the
// server name in the INFO a route listener opens every connection with.
//
// THE PEER'S VIEW, never the member's own report of what it bound: a peer
// knows a member only by the port it dials, so that is the fact a member
// brought back has to hold.
func routeListener(t *testing.T, host string, port int) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("dial the route port %d: %v", port, err)
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetReadDeadline(deadline)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the INFO the route port %d opens with: %v", port, err)
	}
	payload, ok := strings.CutPrefix(strings.TrimSpace(line), "INFO ")
	if !ok {
		t.Fatalf("the route port %d opened with %q, not an INFO", port, line)
	}
	var info struct {
		Name string `json:"server_name"`
	}
	if err := json.Unmarshal([]byte(payload), &info); err != nil {
		t.Fatalf("decode the INFO the route port %d opened with: %v", port, err)
	}
	return info.Name
}

// routesTo reports whether peers names every one of want.
func routesTo(peers, want []string) bool {
	for _, w := range want {
		if !slices.Contains(peers, w) {
			return false
		}
	}
	return true
}
