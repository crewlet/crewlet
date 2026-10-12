package statelog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

// AN ANSWER THAT ARRIVED WITH THE LAST ONE EXPECTED IS READ.
//
// The collection ends the moment the last donor it names has answered, and a
// wait whose context has ended answers that end even with a message in hand —
// so an answer already in the subscription when the last expected one was
// read was never read at all, and the only offer there was could be dropped.
// Here the donor the join names declines with an answer large enough that
// reading it takes a while, and a donor it does not name offers right behind
// it, into the subscription while that decline is read: the offer arrived in
// time, and is taken.
func TestAnAnswerThatArrivedWithTheLastExpectedOneIsRead(t *testing.T) {
	t.Parallel()
	// FOUR MEBIBYTES of a field no build reads: tens of milliseconds to
	// decode, against the microseconds the offer behind it takes to arrive.
	const padding = 4 << 20
	nc := offerBrokerOf(t, 2*padding)
	declined := make(chan struct{})
	decline, err := json.Marshal(struct {
		Offer
		Padding string `json:"padding"`
	}{Offer: Offer{Donor: "named"}, Padding: strings.Repeat("x", padding)})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	named, err := nc.Subscribe(SubjectOffer, func(msg *nats.Msg) {
		// AFTER THE EXPECTATION IS IN, so the decline is the answer that
		// leaves nobody named to wait for. And NOT FLUSHED before the
		// offer is let go: both are written on one connection, so the
		// offer follows the decline's last byte on the wire and reaches
		// the subscription while the decline is still being decoded.
		time.Sleep(100 * time.Millisecond)
		_ = msg.Respond(decline)
		close(declined)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = named.Unsubscribe() })
	offer, err := json.Marshal(Offer{Donor: "unnamed", Fetch: SubjectFetchPrefix + "unnamed"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	unnamed, err := nc.Subscribe(SubjectOffer, func(msg *nats.Msg) {
		<-declined
		_ = msg.Respond(offer)
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = unnamed.Unsubscribe() })
	if err := nc.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	offers, err := CollectOffers(t.Context(), nc, OfferRequest{NodeID: "joiner"},
		OfferWindow, "named")
	if err != nil {
		t.Fatalf("CollectOffers: %v", err)
	}
	if len(offers) != 1 || offers[0].Donor != "unnamed" {
		t.Fatalf("offers = %+v, want the unnamed donor's: it was in the "+
			"subscription when the collection ended, and was never read", offers)
	}
}

// offerBroker is an in-process broker and a connection to it.
func offerBroker(t *testing.T) *nats.Conn {
	t.Helper()
	return offerBrokerOf(t, 0)
}

// offerBrokerOf is [offerBroker] carrying messages up to maxPayload bytes, or
// the broker's own limit for zero.
func offerBrokerOf(t *testing.T, maxPayload int32) *nats.Conn {
	t.Helper()
	ns, err := server.NewServer(&server.Options{
		ServerName: "statelog-offers",
		Port:       -1,
		DontListen: true,
		MaxPayload: maxPayload,
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
