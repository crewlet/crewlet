package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DONOR THAT STOPPED DIALS AGAIN, PROMPTLY AFTER IT HAD BEEN SERVING AND
// MORE SLOWLY WHILE IT CANNOT.
//
// The arithmetic apart from the loop, so it is checked without a clock: a
// refusal at the dial doubles towards the snapshot loop's own retry, and a
// connection closed under a donor that was serving starts again from the base
// however many refusals came before it — that close is a new outage, and the
// redial after it is the one a peer that needs a donor is waiting on.
//
// Mutation: drop the reset on ErrDonorConnectionClosed and the last row goes
// red; stop doubling and the middle rows do.
func TestADonorThatStoppedDialsAgainOnItsOwnSchedule(t *testing.T) {
	t.Parallel()
	refused := errors.New("statelog: open the donor's connection: refused")
	closed := fmt.Errorf("%w: nats: Maximum Payload Violation", statelog.ErrDonorConnectionClosed)
	policy := redialPolicy{base: time.Second, ceiling: 8 * time.Second}

	failures := 0
	for i, want := range []time.Duration{time.Second, 2 * time.Second,
		4 * time.Second, 8 * time.Second, 8 * time.Second} {
		var wait time.Duration
		failures, wait = policy.after(failures, refused)
		if wait != want {
			t.Errorf("refusal %d waits %v, want %v", i+1, wait, want)
		}
	}
	if failures, wait := policy.after(failures, closed); failures != 1 || wait != time.Second {
		t.Errorf("a connection closed under a serving donor after %d refusals "+
			"waits %v (failures %d), want the base %v: it is a new outage",
			5, wait, failures, time.Second)
	}
	if donorRedial.ceiling != snapshotSkipRetry || donorRedial.base != time.Second {
		t.Errorf("the donor redials from %v to %v, want a second to the snapshot "+
			"loop's own retry (%v)", donorRedial.base, donorRedial.ceiling, snapshotSkipRetry)
	}
}

// A DONOR WHOSE CONNECTION IS CLOSED UNDER IT SERVES AGAIN.
//
// The NATS client closes a connection for good on an error it does not retry —
// a chunk past a max_payload lowered under the node, a credential the server
// stopped accepting — and the donor's connection is the one that carries the
// largest messages this node sends. Served once, a donor whose connection
// closed was subscribed to nothing for the rest of the node's life while the
// snapshot register went on advertising its artefact. And it must not take the
// node with it, since a donor serves peers; so the loop that serves it is what
// brings it back.
//
// Staged on a real broker and a real donor: the case closes the connection the
// donor dialled, which is what the client does when it gives up on one, and
// asks for an offer again.
//
// Mutation: serve the donor once rather than in keepDonorServing's loop, or
// let Serve go on waiting on its context alone, and the second offer never
// comes.
func TestADonorWhoseConnectionIsClosedUnderItServesAgain(t *testing.T) {
	t.Parallel()
	q, err := jetstream.Open(t.Context(), jetstream.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })

	var (
		mu      sync.Mutex
		current *nats.Conn
		dials   atomic.Int32
	)
	donor, err := statelog.NewDonor(statelog.DonorDeps{
		NodeID: "redialled",
		Dial: func(context.Context) (*nats.Conn, error) {
			nc, err := q.DialOwned()
			if err == nil {
				mu.Lock()
				current = nc
				mu.Unlock()
				dials.Add(1)
			}
			return nc, err
		},
		Newest: func() (statelog.Manifest, bool) {
			return statelog.Manifest{NodeID: "redialled"}, true
		},
		Path: func(statelog.Manifest) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		keepDonorServing(ctx, donor, redialPolicy{
			base: 10 * time.Millisecond, ceiling: 100 * time.Millisecond})
	}()
	t.Cleanup(func() { stop(); <-done })

	offered(t, q.Conn(), "redialled", "the donor's first connection")

	mu.Lock()
	first := current
	mu.Unlock()
	first.Close()

	offered(t, q.Conn(), "redialled", "a fresh connection after the first was closed under it")
	if got := dials.Load(); got < 2 {
		t.Errorf("the donor answered again on %d dial(s): it is still on the "+
			"connection that was closed", got)
	}
}

// offered waits until the donor named node answers an offer request on nc's
// broker, asking again until it does: an ask published before the donor's
// subscription lands is lost rather than delivered late.
func offered(t *testing.T, nc *nats.Conn, node, what string) {
	t.Helper()
	ask, err := json.Marshal(statelog.OfferRequest{NodeID: "probe"})
	if err != nil {
		t.Fatalf("encode an offer request: %v", err)
	}
	// THE DIAL'S OWN BUDGET AND THEN SOME: an embedded broker's handshake
	// is given its accept budget on a loaded host, and the wait ends the
	// moment the donor answers.
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		msg, err := nc.Request(statelog.SubjectOffer, ask, 200*time.Millisecond)
		if err == nil {
			var offer statelog.Offer
			if json.Unmarshal(msg.Data, &offer) == nil &&
				offer.Fetch == statelog.SubjectFetchPrefix+node {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("donor %q never answered an offer on %s", node, what)
}
