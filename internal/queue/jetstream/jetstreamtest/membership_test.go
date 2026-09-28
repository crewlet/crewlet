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
//
// AND EVERY VOTER'S PEER ID IS THE HASH OF ITS NAME, [js.PeerIDOf] — the pin on
// a derivation written down from nats-server's source. Both halves are the
// server's own here: the id the group counts each voter by, and the node id
// the answering member reports for itself. An upgrade that changed how the
// server derives one fails this, rather than every removal of a voter nobody
// can name.
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
			if p.Peer != js.PeerIDOf(p.Name) {
				t.Errorf("member %d counts %s by peer id %q, and the name hashes to %q",
					i, p.Name, p.Peer, js.PeerIDOf(p.Name))
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
	peer := js.PeerIDOf(name)
	c.Servers[gone].Shutdown()

	if err := c.Servers[carrier].RemovePeer(t.Context(), peer); err != nil {
		t.Fatalf("remove %s through a survivor: %v", name, err)
	}
	if group, err := c.Servers[carrier].MetaGroup(); err != nil || group.Counts(peer) {
		t.Fatalf("the member that carried the removal still counts %s the moment it "+
			"returned: %+v (%v)", name, group, err)
	}
	awaitGroup(t, c.Servers[leader], func(g js.MetaGroup) bool {
		return !g.Counts(peer) && len(g.Peers) == 2
	})
	for _, other := range []string{name, "never-a-member"} {
		if err := c.Servers[carrier].RemovePeer(t.Context(), js.PeerIDOf(other)); !errors.Is(err, js.ErrNotAMetaPeer) {
			t.Errorf("removing %s answered %v, want the group to say it counts no such "+
				"voter", other, err)
		}
	}
}

// A MEMBER GONE FOR GOOD IS STILL REMOVABLE ONCE EVERY SURVIVOR HAS RESTARTED.
//
// nats-server learns a server's NAME only from that server, so survivors that
// restarted after it died — a rolling upgrade, or the restarts a capacity seal
// takes — list it as a voter whose name is unknown, and a removal asked by
// name is answered "no such server" while every election goes on counting it.
// Asked by the peer id its node id hashes to, the group drops it. The listing
// reports it nameless, by that id, rather than by the server's placeholder.
//
// The survivors restart one at a time, as a rolling upgrade does, so the group
// keeps the quorum each restarted member has to rejoin.
func TestAMemberGoneForGoodIsRemovedAfterEverySurvivorRestarted(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	leaderOf(t, c)
	const gone = 2
	peer := js.PeerIDOf(c.Configs[gone].ServerName)
	c.Servers[gone].Shutdown()
	for i := range gone {
		c.Servers[i].Shutdown()
		srv, err := js.StartServer(t.Context(), c.Configs[i])
		if err != nil {
			t.Fatalf("restart member %d: %v", i, err)
		}
		t.Cleanup(srv.Shutdown)
		c.Servers[i] = srv
	}

	group := awaitGroup(t, c.Servers[0], func(g js.MetaGroup) bool {
		return g.Leader != "" && g.Counts(peer)
	})
	for _, p := range group.Peers {
		if p.Peer == peer && p.Name != "" {
			t.Fatalf("the restarted survivors still know the dead member's name (%q), "+
				"so this case stages nothing: %+v", p.Name, group.Peers)
		}
	}
	if err := c.Servers[0].RemovePeer(t.Context(), peer); err != nil {
		t.Fatalf("remove the dead member by its peer id: %v", err)
	}
	awaitGroup(t, c.Servers[1], func(g js.MetaGroup) bool {
		return !g.Counts(peer) && len(g.Peers) == 2
	})
}

// A MEMBER NEVER CARRIES ITS OWN REMOVAL, and a gesture naming something that is
// not a peer id is refused before anything asks the group. A member removing
// itself could not see the group drop it — it goes on counting itself until
// its JetStream is switched off, which the commit does — so its answer would
// be lost with it.
func TestAMemberRefusesToCarryItsOwnRemoval(t *testing.T) {
	c := StartCluster(t, 3, js.Config{})
	leaderOf(t, c)
	self := js.PeerIDOf(c.Configs[0].ServerName)
	if err := c.Servers[0].RemovePeer(t.Context(), self); !errors.Is(err, js.ErrRemovingSelf) {
		t.Fatalf("a member asked to carry its own removal answered %v", err)
	}
	if err := c.Servers[0].RemovePeer(t.Context(), c.Configs[1].ServerName); err == nil ||
		errors.Is(err, js.ErrNotAMetaPeer) {
		t.Fatalf("a server NAME offered as a peer id answered %v, want a refusal of "+
			"its spelling before the group was asked", err)
	}
	if group, err := c.Servers[1].MetaGroup(); err != nil || !group.Counts(self) {
		t.Fatalf("a refused removal changed the group: %+v (%v)", group, err)
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
	if err := c.Servers[carrier].RemovePeer(ctx, js.PeerIDOf(name)); err != nil {
		t.Fatalf("remove the leader %s through a survivor: %v", name, err)
	}
	group, err := c.Servers[carrier].MetaGroup()
	if err != nil || group.Counts(js.PeerIDOf(name)) || len(group.Peers) != 2 {
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
