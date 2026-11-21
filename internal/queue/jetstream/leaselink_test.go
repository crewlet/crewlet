package jetstream

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/linktest"
	"github.com/crewlet/crewlet/internal/seat"
)

// clientOf is a queue on srv, stopped when the case ends.
func clientOf(t *testing.T, srv testServer) *Queue {
	t.Helper()
	q, err := srv.Client(t.Context())
	if err != nil {
		t.Fatalf("a client of %s: %v", srv.cfg.ServerName, err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}

// leasesOf is q's lease connection, closed when the case ends.
func leasesOf(t *testing.T, q *Queue) *nats.Conn {
	t.Helper()
	nc, err := q.DialLeases(t.Context())
	if err != nil {
		t.Fatalf("DialLeases: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// memberLinks is a member's leaf connections from one leaf, as the member
// reports them: the data link, and the lease link, the one the leaf asked to
// isolate (a member's Leafz names no remote account, monitor.go v2.15.3);
// nil for one it does not hold, and how many it holds in all.
func memberLinks(t *testing.T, member *Server, subs bool) (data, lease *server.LeafInfo, n int) {
	t.Helper()
	leafz, err := member.embedded.ns.Leafz(&server.LeafzOptions{Subscriptions: subs})
	if err != nil {
		t.Fatalf("read the member's leaf connections: %v", err)
	}
	for _, l := range leafz.Leafs {
		if l.IsIsolated {
			lease = l
		} else {
			data = l
		}
	}
	return data, lease, len(leafz.Leafs)
}

// A LEAF READS EACH OF ITS LINKS BY ACCOUNT: with its lease link cut its
// lease connection reads down while its data path reads up, and with its data
// link cut the converse; and a leaf whose data link is cut is not ready
// however its lease link stands, its refusal naming the link it lacks.
//
// Mutant: LeaseLink reads the leaf's link count — the cut lease link reads up.
// Mutant: awaitLeafReady counts either link — the refusal says the link is up.
// Mutant: leafRemotes returns one remote — DialLeases never links.
func TestALeafReadsEachOfItsLinksByAccount(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, linktest.Lagging)
	q := clientOf(t, f.leaf)
	leasesOf(t, q)
	if err := q.LeaseLink(); err != nil {
		t.Fatalf("a linked leaf's lease connection reads %v", err)
	}
	if err := q.DataPath(); err != nil {
		t.Fatalf("a linked leaf's data path reads %v", err)
	}

	f.link.Cut(LeaseAccount)
	until(t, "the cut lease link read down", func() bool { return q.LeaseLink() != nil })
	if err := q.LeaseLink(); !strings.Contains(err.Error(), LeaseAccount) {
		t.Errorf("the lease link's refusal %q does not name its account", err)
	}
	if err := q.DataPath(); err != nil {
		t.Errorf("with only its lease link cut, the leaf's data path reads %v", err)
	}
	f.link.Mend(LeaseAccount)
	until(t, "the mended lease link read up", func() bool { return q.LeaseLink() == nil })

	f.link.Cut(server.DEFAULT_GLOBAL_ACCOUNT)
	until(t, "the cut data link read down", func() bool { return q.DataPath() != nil })
	if err := q.Link(); err == nil {
		t.Error("with its data link cut, the leaf's link reads up")
	}
	if err := q.LeaseLink(); err != nil {
		t.Errorf("with only its data link cut, the leaf's lease connection reads %v", err)
	}

	// NOT READY WHILE THE DATA LINK IS CUT, whatever the lease link does.
	f.leaf.embedded.ready = readiness{timeout: 2 * time.Second, poll: 20 * time.Millisecond,
		ask: 200 * time.Millisecond}
	again, err := newQueueOn(t.Context(), f.leaf.cfg, f.leaf.embedded, false, queue.Resolve())
	if err == nil {
		_ = again.Stop(context.WithoutCancel(t.Context()))
		t.Fatal("a leaf whose data link is cut opened a queue")
	}
	if !strings.Contains(err.Error(), "made no link (account $G)") {
		t.Errorf("a leaf whose data link is cut is refused for %v, want its data link named "+
			"as down", err)
	}
}

// A LEAF'S TWO LINKS TO ONE MEMBER STAND SIDE BY SIDE: the member holds both,
// the lease link isolated; the lease connection's own interest crosses the
// lease link and not the data link; and through three cycles of dropping each
// link in turn, each comes back without evicting the other. This holds the
// reading of nats-server v2.15.3's eviction rule (leafnode.go
// addLeafNodeConnection: same leaf, cluster and remote account) against the
// next bump.
//
// Mutant: the second remote's local account is the global one — the links
// evict each other on every connect.
// Mutant: the lease connection bound to the global account — its interest
// crosses the data link.
func TestALeafsTwoLinksToOneMemberStandSideBySide(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, linktest.Lagging)
	both := func() (data, lease *server.LeafInfo, ok bool) {
		data, lease, n := memberLinks(t, f.member.Server, false)
		return data, lease, n == 2 && data != nil && lease != nil
	}
	until(t, "both of the leaf's links on the member", func() bool { _, _, ok := both(); return ok })

	leases, err := f.leaf.LeaseConn()
	if err != nil {
		t.Fatalf("LeaseConn: %v", err)
	}
	t.Cleanup(leases.Close)
	if _, err := leases.Subscribe("leaselink.own", func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	if err := leases.Flush(); err != nil {
		t.Fatal(err)
	}
	until(t, "the lease connection's interest on the member", func() bool {
		data, lease, _ := memberLinks(t, f.member.Server, true)
		if data != nil && slices.Contains(data.Subs, "leaselink.own") {
			t.Fatal("the lease connection's interest crossed the data link")
		}
		return lease != nil && slices.Contains(lease.Subs, "leaselink.own")
	})

	for cycle := range 3 {
		for _, account := range []string{server.DEFAULT_GLOBAL_ACCOUNT, LeaseAccount} {
			data, lease, _ := both()
			dropped, kept := data.ID, lease.ID
			if account == LeaseAccount {
				dropped, kept = lease.ID, data.ID
			}
			f.link.Drop(account)
			until(t, fmt.Sprintf("cycle %d: the %s link back", cycle, account), func() bool {
				data, lease, ok := both()
				if !ok {
					return false
				}
				now := map[uint64]bool{data.ID: true, lease.ID: true}
				if !now[kept] {
					t.Fatalf("cycle %d: the %s link's return evicted the other link", cycle, account)
				}
				return !now[dropped]
			})
		}
	}
}

// otherLeaves and otherSubjects are the interest case's crowd: twenty leaves of
// fifty subscriptions each.
const (
	otherLeaves   = 20
	otherSubjects = 50
)

// A LEASE LINK IS TOLD NO OTHER LEAF'S INTEREST: with twenty other leaves of
// fifty subscriptions each joined to its member, the leaf's data link is told
// every one of them, and its lease link none — only the member's own. Decided
// by a fence: a subscription made on the member after the crowd's, which the
// lease link is told after anything sent ahead of it.
//
// Mutant: the lease remote not isolated — it is told the crowd's interest.
func TestALeaseLinkIsToldNoOtherLeafsInterest(t *testing.T) {
	t.Parallel()
	cfg := testTimings(Config{FleetAuth: testFleetAuth()})
	memberCfg := cfg
	memberCfg.ServerName = "member"
	memberCfg.LeafHost, memberCfg.LeafPort = "127.0.0.1", AnyPort
	memberCfg.StoreDir, memberCfg.StoreMaxBytes = brokertest.TempDir(t), brokertest.StoreMaxBytes
	member := startTestServer(t, memberCfg)
	leafCfg := cfg
	leafCfg.LeafURLs = []string{fmt.Sprintf("nats-leaf://127.0.0.1:%d", member.LeafPort())}
	leafCfg.ServerName = "leaf"
	leaf := startTestServer(t, leafCfg)

	for i := range otherLeaves {
		otherCfg := leafCfg
		otherCfg.ServerName = fmt.Sprintf("other-%d", i)
		other := startTestServer(t, otherCfg)
		nc, err := other.Conn()
		if err != nil {
			t.Fatalf("connect to %s: %v", otherCfg.ServerName, err)
		}
		t.Cleanup(nc.Close)
		for j := range otherSubjects {
			if _, err := nc.Subscribe(fmt.Sprintf("other.%d.%d", i, j), func(*nats.Msg) {}); err != nil {
				t.Fatal(err)
			}
		}
		if err := nc.Flush(); err != nil {
			t.Fatal(err)
		}
	}
	told := func(account string) []string {
		leafz, err := leaf.embedded.ns.Leafz(&server.LeafzOptions{Subscriptions: true, Account: account})
		if err != nil || len(leafz.Leafs) != 1 {
			return nil
		}
		return leafz.Leafs[0].Subs
	}
	crowd := func(subs []string) int {
		n := 0
		for _, s := range subs {
			if strings.HasPrefix(s, "other.") {
				n++
			}
		}
		return n
	}
	until(t, "the crowd's interest on the leaf's data link", func() bool {
		return crowd(told(server.DEFAULT_GLOBAL_ACCOUNT)) == otherLeaves*otherSubjects
	})

	down, err := member.Conn()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(down.Close)
	if _, err := down.Subscribe("leaselink.fence", func(*nats.Msg) {}); err != nil {
		t.Fatal(err)
	}
	if err := down.Flush(); err != nil {
		t.Fatal(err)
	}
	until(t, "the member's fence on the lease link", func() bool {
		return slices.Contains(told(LeaseAccount), "leaselink.fence")
	})
	if n := crowd(told(LeaseAccount)); n != 0 {
		t.Errorf("the lease link was told %d of the other leaves' %d subscriptions", n,
			otherLeaves*otherSubjects)
	}
}

// THE LEASE DIAL GIVES UP A DEAD SOCKET WITHIN A HEARTBEAT OF THE SHORTEST
// LEASE: its options name it, ping every 1.667 s with two out, and hold a
// write to five seconds; and live, an external server behind a link that
// blackholes the lease connection alone sees it go stale and dial again once
// the link carries it, while the queue's own connection beside it stands.
// The live half's bound is liveness: the queue dial's two-minute pings would
// take four minutes or more to notice.
//
// Mutant: the lease dial built from dialOptions — the stale socket is never
// noticed inside the bound, and the options are the queue's.
func TestTheLeaseDialGivesUpADeadSocketWithinAHeartbeatOfTheShortestLease(t *testing.T) {
	t.Parallel()
	opts, _, err := dialLeaseOptions(Config{})
	if err != nil {
		t.Fatal(err)
	}
	applied := nats.GetDefaultOptions()
	for _, opt := range opts {
		if err := opt(&applied); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat := config.MinLeaseTTL / seat.HeartbeatRatio
	if applied.Name != "crewlet-leases" || applied.MaxPingsOut != 2 ||
		applied.PingInterval != heartbeat/3 || applied.FlusherTimeout != heartbeat {
		t.Errorf("the lease dial is named %q, pings every %v with %d out and holds a write %v: "+
			"want crewlet-leases, %v, 2 and %v", applied.Name, applied.PingInterval,
			applied.MaxPingsOut, applied.FlusherTimeout, heartbeat/3, heartbeat)
	}
	if dead := time.Duration(applied.MaxPingsOut+1) * applied.PingInterval; dead > heartbeat {
		t.Errorf("the lease dial gives a dead socket up after %v, past a heartbeat %v", dead, heartbeat)
	}

	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	link := linktest.Lagging(t, srv.HostPort())
	q := newQueueWith(t, Config{URL: "nats://" + link.Addr(), Token: externaltest.Token})
	leases := leasesOf(t, q)
	dead := linktest.Client(leaseConnName)
	stale, back := make(chan error, 1), make(chan struct{}, 1)
	leases.SetDisconnectErrHandler(func(_ *nats.Conn, err error) {
		link.Mend(dead)
		select {
		case stale <- err:
		default:
		}
	})
	leases.SetReconnectHandler(func(*nats.Conn) {
		select {
		case back <- struct{}{}:
		default:
		}
	})
	link.Blackhole(dead)
	bound := time.NewTimer(60 * time.Second) //fixedwait:liveness
	defer bound.Stop()
	select {
	case err := <-stale:
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Errorf("the blackholed lease connection was dropped for %v, want stale", err)
		}
	case <-bound.C:
		t.Fatal("the lease connection never noticed its socket carried nothing")
	}
	select {
	case <-back:
	case <-bound.C:
		t.Fatal("the lease connection never dialled again once the link carried it")
	}
	if err := q.Link(); err != nil {
		t.Errorf("the queue's own connection, not blackholed, reads %v", err)
	}
}

// DIALLEASES WAITS FOR ITS LINK AND NAMES IT: a leaf whose lease link the relay
// refuses hands out no lease connection, and past its bound says which link it
// lacks and the key it presented; once the link is let through, it does.
//
// Mutant: DialLeases does not wait — it hands out a connection with no link.
func TestDialLeasesWaitsForItsLinkAndNamesIt(t *testing.T) {
	t.Parallel()
	f := newMeterFleet(t, linktest.Lagging)
	q := clientOf(t, f.leaf)
	f.link.Cut(LeaseAccount)
	until(t, "the cut lease link down", func() bool { return !f.leaf.embedded.linked(LeaseAccount) })
	f.leaf.embedded.ready = readiness{timeout: 2 * time.Second, poll: 20 * time.Millisecond,
		ask: 200 * time.Millisecond}
	nc, err := q.DialLeases(t.Context())
	if err == nil {
		nc.Close()
		t.Fatal("a leaf whose lease link is refused handed out a lease connection")
	}
	for _, want := range []string{"made no lease link", LeaseAccount, `secrets.keys id "jetstream-test"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q:\n%v", want, err)
		}
	}
	if err := q.LeaseLink(); err == nil {
		t.Error("a queue whose lease dial was refused reads a lease link")
	}

	f.link.Mend(LeaseAccount)
	f.leaf.embedded.ready = readiness{}
	leasesOf(t, q)
	if err := q.LeaseLink(); err != nil {
		t.Errorf("the lease connection handed out once its link was let through reads %v", err)
	}
}
