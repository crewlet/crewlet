package engine

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A JOIN NAMES EVERY NODE THAT CAN DONATE, AND ONLY THOSE.
//
// The collection ends on the last donor a join names, so a node that can offer
// and is not named is a node whose offer is dropped whenever it lands second.
// Named from presence alone, a DRAINING node — which gives its presence up
// first and serves its donor until its state log stops — was one of those. A
// node named that cannot answer costs the whole window instead, which is what
// this node's own name did at boot, where its donor has not started.
func TestAJoinNamesEveryNodeThatCanDonate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	holds := func(id string, age time.Duration) coord.NodePositions {
		return coord.NodePositions{NodeID: id, At: now.Add(-age), SnapshotBytes: 4096}
	}
	empty := func(id string, age time.Duration) coord.NodePositions {
		return coord.NodePositions{NodeID: id, At: now.Add(-age)}
	}
	cases := map[string]struct {
		ownServing bool
		live       []string
		rows       []coord.NodePositions
		want       []string
	}{
		"every live data node, whether or not its row names an artefact": {
			ownServing: true,
			live:       []string{"joiner", "peer"},
			rows:       []coord.NodePositions{empty("peer", 0)},
			want:       []string{"joiner", "peer"},
		},
		"a draining node, off its fresh row, with no presence": {
			ownServing: true,
			live:       []string{"joiner"},
			rows:       []coord.NodePositions{holds("draining", PositionHeartbeat)},
			want:       []string{"draining", "joiner"},
		},
		"a row naming an artefact by a log's snapshot instant alone": {
			ownServing: true,
			live:       []string{"joiner"},
			rows: []coord.NodePositions{{NodeID: "draining", At: now,
				Domains: map[string]coord.DomainPosition{
					"tracker": {SnapshotAt: now.Add(-time.Hour)},
				}}},
			want: []string{"draining", "joiner"},
		},
		"not a row older than four heartbeats": {
			ownServing: true,
			live:       []string{"joiner"},
			rows:       []coord.NodePositions{holds("gone", donorRowFresh+time.Second)},
			want:       []string{"joiner"},
		},
		"not a fresh row naming no artefact": {
			ownServing: true,
			live:       []string{"joiner"},
			rows:       []coord.NodePositions{empty("idle", 0)},
			want:       []string{"joiner"},
		},
		"not this node while its own donor is not serving, live or not": {
			live: []string{"joiner", "peer"},
			rows: []coord.NodePositions{holds("joiner", 0)},
			want: []string{"peer"},
		},
		"this node while its donor serves, with no presence and no row": {
			ownServing: true,
			want:       []string{"joiner"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			live := make([]statelog.Presence, 0, len(tc.live))
			for _, id := range tc.live {
				live = append(live, statelog.Presence{NodeID: id})
			}
			got := joinDonors(now, "joiner", tc.ownServing, live, tc.rows)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("joinDonors = %v, want %v", got, tc.want)
			}
		})
	}
}

// A JOIN COLLECTS A DRAINING NODE'S OFFER.
//
// The fleet a rolling upgrade makes: the joining node is the one live data
// node and its own donor declines at once, while the node being drained has
// given up its presence, still heartbeats a row naming its artefact, and
// answers a moment after the decline — the order a node busy finishing its
// turns, or one a zone away, answers in. Named from presence alone the
// collection ended on the decline and dropped the only offer there was; named
// from who can donate, it waits for that offer and takes it.
func TestAJoinCollectsADrainingNodesOffer(t *testing.T) {
	t.Parallel()
	broker := joinBroker(t)
	nc, err := nats.Connect("", nats.InProcessServer(broker))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	leases := coordmemory.New()
	claimPresences(t, leases, map[string][]string{"joiner": {"data"}})
	register := coordmemory.NewFleet()
	if err = register.PutPositions(t.Context(), coord.NodePositions{
		NodeID: "draining", At: time.Now().UTC(), SnapshotBytes: 4096,
	}); err != nil {
		t.Fatalf("PutPositions: %v", err)
	}
	e := &Engine{backends: &Backends{Coord: leases}}
	s := &stateLog{nodeID: "joiner", fleet: register}

	// THIS NODE'S OWN DONOR, serving and holding nothing.
	own, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "joiner",
		Dial: func(context.Context) (*nats.Conn, error) {
			return nats.Connect("", nats.InProcessServer(broker))
		},
		Newest: func() (statelog.Manifest, bool) { return statelog.Manifest{}, false },
		Path:   func(statelog.Manifest) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	serving, stopServing := context.WithCancel(t.Context())
	served := make(chan struct{})
	go func() { defer close(served); _ = own.Serve(serving) }()
	t.Cleanup(func() { stopServing(); <-served })
	s.donor.Store(own)
	deadline := time.Now().Add(10 * time.Second)
	for !own.Serving() {
		if time.Now().After(deadline) {
			t.Fatal("the premise: this node's own donor never began serving")
		}
		time.Sleep(time.Millisecond)
	}

	// THE DRAINING NODE, answering after the decline.
	const late = 200 * time.Millisecond
	offer, err := json.Marshal(statelog.Offer{
		Donor: "draining", Fetch: statelog.SubjectFetchPrefix + "draining",
		Manifest: statelog.Manifest{NodeID: "draining", Bytes: 4096},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	sub, err := nc.Subscribe(statelog.SubjectOffer, func(msg *nats.Msg) {
		time.Sleep(late)
		_ = msg.Respond(offer)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	named, err := e.joinExpectation(s)(t.Context())
	if err != nil {
		t.Fatalf("the join's expectation: %v", err)
	}
	if want := []string{"draining", "joiner"}; !slices.Equal(named, want) {
		t.Errorf("the join waits for %v, want %v: a draining node holding an "+
			"artefact is a donor whatever its presence says", named, want)
	}
	started := time.Now()
	offers, err := statelog.CollectOffers(t.Context(), nc,
		statelog.OfferRequest{NodeID: "joiner"}, statelog.OfferWindow, named...)
	took := time.Since(started)
	if err != nil {
		t.Fatalf("CollectOffers: %v", err)
	}
	if len(offers) != 1 || offers[0].Donor != "draining" {
		t.Fatalf("offers = %+v, want the draining node's: the collection ended "+
			"on this node's own decline, before the only offer there was", offers)
	}
	// AND ON THAT OFFER rather than the window: half of it separates the two.
	if took >= statelog.OfferWindow/2 {
		t.Fatalf("the collection took %v: it waited out the %v window rather "+
			"than ending on the last donor it named", took, statelog.OfferWindow)
	}
}

// joinBroker is an in-process broker with no listener, shut down with the
// test.
func joinBroker(t *testing.T) *server.Server {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "engine-join",
		Port:       -1,
		DontListen: true,
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	if !ns.ReadyForConnections(30 * time.Second) {
		t.Fatal("the broker did not become ready")
	}
	return ns
}
