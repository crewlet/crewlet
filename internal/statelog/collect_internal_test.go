package statelog

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// A JOIN'S EXPECTATION IS READ BESIDE ITS COLLECTION, AND THE WINDOW BOUNDS IT.
//
// The donors a join expects come from a listing of the fleet's live data
// nodes, a coordination read. Read before the collection, a store slow to
// answer added all of its time in front of the offer window, on a node that
// refuses every read and write while it joins. Here the listing never answers
// at all: the collection ends with the window regardless, and the listing has
// returned by the time it does — nothing the collection started outlives it.
func TestAnExpectationSlowerThanTheWindowCostsOnlyTheWindow(t *testing.T) {
	t.Parallel()
	nc := offerBroker(t)
	silentDonor(t, nc)
	const window = 300 * time.Millisecond
	var listed atomic.Bool
	type result struct {
		offers []Offer
		err    error
	}
	done := make(chan result, 1)
	go func() {
		offers, err := collectOffers(t.Context(), nc, OfferRequest{NodeID: "joiner"}, window,
			func(ctx context.Context) ([]string, error) {
				<-ctx.Done()
				listed.Store(true)
				return nil, ctx.Err()
			})
		done <- result{offers, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(window + 2*time.Second):
		t.Fatalf("the collection was still running %v into a %v window: a "+
			"listing that never answered held it past the window", window+2*time.Second, window)
	}
	if got.err != nil {
		t.Fatalf("collectOffers: %v", got.err)
	}
	if len(got.offers) != 0 {
		t.Fatalf("offers = %+v from a donor that never answered", got.offers)
	}
	if !listed.Load() {
		t.Fatal("the collection returned with its listing still running")
	}
}

// AN EXPECTATION THAT ARRIVES AFTER EVERY DONOR IT NAMES HAS ANSWERED ENDS
// THE COLLECTION AS IT ARRIVES.
//
// The listing is slower than the donors here — the commonest order, since a
// donor answers from memory — so by the time it names them nothing more is
// coming, and a collection still waiting for a next answer would sit out the
// whole window. The answers that came before it count.
func TestAnExpectationArrivingAfterItsDonorsEndsTheCollection(t *testing.T) {
	t.Parallel()
	nc := offerBroker(t)
	answered := decliningDonor(t, nc, "a", 0)
	const window = 10 * time.Second
	started := time.Now()
	offers, err := collectOffers(t.Context(), nc, OfferRequest{NodeID: "joiner"}, window,
		func(ctx context.Context) ([]string, error) {
			select {
			case <-answered:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return []string{"a"}, nil
		})
	took := time.Since(started)
	if err != nil {
		t.Fatalf("collectOffers: %v", err)
	}
	if len(offers) != 0 {
		t.Fatalf("offers = %+v: a decline is not an offer", offers)
	}
	if took >= window/2 {
		t.Fatalf("the collection took %v: the expectation arrived after its one "+
			"donor had declined, and the collection waited for the %v window "+
			"rather than ending on it", took, window)
	}
}

// AN EXPECTATION THAT FAILS LEAVES THE WINDOW TO DECIDE.
//
// A listing that could not be read names nobody, which is not the same as
// naming an empty fleet: the collection goes on hearing whoever answers until
// the window closes, as it did before there was any expectation.
func TestAnExpectationThatFailsLeavesTheWindowToDecide(t *testing.T) {
	t.Parallel()
	nc := offerBroker(t)
	decliningDonor(t, nc, "empty", 0)
	donorAfter(t, nc, "slow", 200*time.Millisecond)
	offers, err := collectOffers(t.Context(), nc, OfferRequest{NodeID: "joiner"}, time.Second,
		func(context.Context) ([]string, error) {
			return nil, errors.New("the coordination store did not answer")
		})
	if err != nil {
		t.Fatalf("collectOffers: %v", err)
	}
	if len(offers) != 1 || offers[0].Donor != "slow" {
		t.Fatalf("offers = %+v, want the slow donor's: a failed listing ended the "+
			"collection before the window did", offers)
	}
}

// offerBroker is an in-process broker and a connection to it.
func offerBroker(t *testing.T) *nats.Conn {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "statelog-offers",
		Port:       -1,
		DontListen: true,
	})
	if err != nil {
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		t.Fatal("the broker did not become ready")
	}
	t.Cleanup(func() {
		ns.Shutdown()
		ns.WaitForShutdown()
	})
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// silentDonor subscribes to offer requests and never answers one: a donor
// that is there and says nothing.
func silentDonor(t *testing.T, nc *nats.Conn) {
	t.Helper()
	sub, err := nc.Subscribe(SubjectOffer, func(*nats.Msg) {})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// decliningDonor answers every offer request with a decline from id after
// delay, and returns a channel closed once it has answered the first.
func decliningDonor(t *testing.T, nc *nats.Conn, id string, delay time.Duration) <-chan struct{} {
	t.Helper()
	return answeringDonor(t, nc, Offer{Donor: id}, delay)
}

// donorAfter answers every offer request with an offer from id after delay.
func donorAfter(t *testing.T, nc *nats.Conn, id string, delay time.Duration) {
	t.Helper()
	answeringDonor(t, nc, Offer{Donor: id, Fetch: SubjectFetchPrefix + id}, delay)
}

func answeringDonor(t *testing.T, nc *nats.Conn, answer Offer, delay time.Duration) <-chan struct{} {
	t.Helper()
	body, err := json.Marshal(answer)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	answered := make(chan struct{})
	var once atomic.Bool
	sub, err := nc.Subscribe(SubjectOffer, func(msg *nats.Msg) {
		time.Sleep(delay)
		if err := msg.Respond(body); err != nil {
			return
		}
		if err := nc.Flush(); err != nil {
			return
		}
		if once.CompareAndSwap(false, true) {
			close(answered)
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	return answered
}
