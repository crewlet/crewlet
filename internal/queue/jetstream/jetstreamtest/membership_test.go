package jetstreamtest

import (
	"context"
	"errors"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
)

// EVERY MEMBER READS THE WHOLE METADATA GROUP, itself included, by the names
// the engine knows its nodes by — so the fleet's broker listing can hold what
// the nodes advertise against what the broker counts, from whichever member it
// asks.
func TestEveryMemberReadsTheWholeMetadataGroup(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	want := []string{c.Configs[0].ServerName, c.Configs[1].ServerName, c.Configs[2].ServerName}
	for i, srv := range c.Servers {
		group := awaitGroup(t, srv, func(g js.MetaGroup) bool {
			return g.Leader != "" && len(g.Peers) == 3
		})
		var names []string
		leaders := 0
		for _, p := range group.Peers {
			names = append(names, p.Name)
			if p.Self != (p.Name == c.Configs[i].ServerName) {
				t.Errorf("member %d marks %s self=%v", i, p.Name, p.Self)
			}
			if p.Leader {
				leaders++
			}
		}
		if len(names) != 3 || names[0] != want[0] || names[1] != want[1] || names[2] != want[2] {
			t.Fatalf("member %d reads the group as %v, want %v", i, names, want)
		}
		if leaders != 1 {
			t.Fatalf("member %d reads %d leaders in %+v", i, leaders, group.Peers)
		}
	}
}

// A MEMBER THAT IS GONE FOR GOOD IS REMOVED THROUGH ANY SURVIVOR'S SYSTEM
// ACCOUNT, and stops being a voter every election and every create waits on.
// The removal returns once the member that carried it has APPLIED it — carried
// here by a follower, which learns of the leader's commit a heartbeat later —
// so the group it reports next no longer counts the server. Asking again, or
// naming a server the group never had, is refused as naming nothing — which is
// what the fleet's remove verb reports rather than a failure.
func TestAMemberGoneForGoodIsRemovedFromTheMetadataGroup(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	leader := leaderOf(t, c)
	gone, carrier := -1, -1
	for i := range c.Servers {
		switch {
		case i == leader:
		case gone < 0:
			gone = i
		default:
			carrier = i
		}
	}
	name := c.Configs[gone].ServerName
	c.Servers[gone].Shutdown()

	if err := c.Servers[carrier].RemovePeer(t.Context(), name); err != nil {
		t.Fatalf("remove %s through a survivor: %v", name, err)
	}
	if group, err := c.Servers[carrier].MetaGroup(); err != nil || counts(group, name) {
		t.Fatalf("the member that carried the removal still counts %s the moment it "+
			"returned: %+v (%v)", name, group, err)
	}
	awaitGroup(t, c.Servers[leader], func(g js.MetaGroup) bool {
		return !counts(g, name) && len(g.Peers) == 2
	})
	for _, other := range []string{name, "never-a-member"} {
		if err := c.Servers[carrier].RemovePeer(t.Context(), other); !errors.Is(err, js.ErrNotAMetaPeer) {
			t.Errorf("removing %s answered %v, want the group to say it lists no such "+
				"server", other, err)
		}
	}
}

// THE LEADER GONE FOR GOOD IS REMOVED ONCE THE SURVIVORS ELECT ANOTHER — the
// case an operator meets most, since a dead leader is what sends one looking.
// Only a leader answers a removal and a member that is not one drops it
// unanswered, so an ask made while the survivors are still electing is lost:
// asked once, the removal waited out its whole budget for an answer nobody was
// going to send, and was reported as a group with no leader.
//
// The wait is bounded at a minute: several of nats-server's election timeouts
// (four to nine seconds each), and half the budget a single lost ask would
// spend.
func TestTheLeaderGoneForGoodIsRemovedOnceTheSurvivorsElectAnother(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	leader := leaderOf(t, c)
	carrier := (leader + 1) % len(c.Servers)
	name := c.Configs[leader].ServerName
	c.Servers[leader].Shutdown()

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	if err := c.Servers[carrier].RemovePeer(ctx, name); err != nil {
		t.Fatalf("remove the leader %s through a survivor: %v", name, err)
	}
	group, err := c.Servers[carrier].MetaGroup()
	if err != nil || counts(group, name) || len(group.Peers) != 2 {
		t.Fatalf("after the removal the survivor reads %+v (%v)", group, err)
	}
}

// leaderOf waits for the whole group to be counted under one leader and
// returns that leader's index.
func leaderOf(t *testing.T, c *Cluster) int {
	t.Helper()
	group := awaitGroup(t, c.Servers[0], func(g js.MetaGroup) bool {
		return g.Leader != "" && len(g.Peers) == len(c.Servers)
	})
	for i, cfg := range c.Configs {
		if cfg.ServerName == group.Leader {
			return i
		}
	}
	t.Fatalf("the leader %s is none of the cluster's servers", group.Leader)
	return -1
}

// counts reports whether a group counts the named server.
func counts(g js.MetaGroup, name string) bool {
	for _, p := range g.Peers {
		if p.Name == name {
			return true
		}
	}
	return false
}

// awaitGroup reads the metadata group until it is the one the case needs, and
// fails the case with the last reading if it never is.
func awaitGroup(t *testing.T, srv *js.Server, done func(js.MetaGroup) bool) js.MetaGroup {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		group, err := srv.MetaGroup()
		if err == nil && done(group) {
			return group
		}
		if time.Now().After(deadline) {
			t.Fatalf("the metadata group never reached the state this case needs: "+
				"%+v (%v)", group, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
