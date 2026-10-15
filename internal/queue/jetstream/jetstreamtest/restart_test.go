package jetstreamtest

import (
	"context"
	"fmt"
	"slices"
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

	if got, want := c.Servers[member].ClusterPort(), c.Configs[member].ClusterPort; got != want {
		t.Errorf("the restarted member listens for routes on %d, its peers dial %d", got, want)
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

// routesTo reports whether peers names every one of want.
func routesTo(peers, want []string) bool {
	for _, w := range want {
		if !slices.Contains(peers, w) {
			return false
		}
	}
	return true
}
