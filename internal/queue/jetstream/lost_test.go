package jetstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// lostWithin bounds a wait for a close the server has already decided: the
// client reads the server's -ERR, or fails its one reconnect a second later,
// and dispatches the handler on its own goroutine. Generous because it costs
// nothing when the close comes; a wait that times out is the defect.
const lostWithin = 30 * time.Second

// quietFor is how long a case that asserts NOTHING was lost waits for a report
// that should not come. The handler it would catch is dispatched on the
// client's own goroutine the moment the close is decided — microseconds, as
// every positive case here shows — so a quarter of a second is a wait a
// mistakenly installed handler has long since spent.
const quietFor = 250 * time.Millisecond

// oversized is an event larger than limit bytes on the wire and well inside
// the contract, so the client — which still reads the contract's figure after
// a live reload — sends it, and only the server can refuse it.
func oversized(t *testing.T, limit int) []byte {
	t.Helper()
	e := ev(1)
	e.Extra = map[string]json.RawMessage{
		"blob": json.RawMessage(`"` + strings.Repeat("x", limit+1024) + `"`),
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return body
}

// sendPast sends nc a message past limit, which the server has lowered under
// it by a live reload.
//
// A WRITE ERROR IS EXPECTED, not a failure of the case: the server refuses the
// message on its header and closes the connection while the client is still
// writing the body, so the write can meet a reset. What the case cannot go on
// from is the client refusing the message ITSELF — it would have learned the
// reloaded limit, and nothing would reach the server to refuse.
func sendPast(t *testing.T, nc *nats.Conn, limit int) {
	t.Helper()
	if err := nc.Publish("crewlet.lost.oversized", oversized(t, limit)); errors.Is(err, nats.ErrMaxPayload) {
		t.Fatalf("the client refused the message itself (%v): it has learned "+
			"the reloaded limit, and the case stages nothing", err)
	}
	_ = nc.Flush()
}

// lost waits for q to report a loss, and answers its cause.
func lost(t *testing.T, q *Queue) error {
	t.Helper()
	select {
	case <-q.Lost():
	case <-time.After(lostWithin):
		t.Fatalf("the server closed the connection for good and the queue "+
			"reported nothing within %s", lostWithin)
	}
	cause := q.LostCause()
	if cause == nil {
		t.Fatal("Lost is closed and LostCause is nil: a node would stop naming nothing")
	}
	return cause
}

// A CONNECTION NATS CLOSES FOR GOOD IS REPORTED, ONCE, WITH ITS CAUSE.
//
// The client stops reconnecting on its own in two ordinary shapes, each staged
// here as an operator produces it under a running node — a max_payload lowered
// by a LIVE RELOAD, and the server's credentials rotated, which it restarts
// for — and each closed the node's broker connection with nothing told: no
// publish, no consumer, no lease renewal, and a process that looked alive
// until a person restarted it. The cause is the sentence the node stops
// with, so each row holds it to the remedy its cause has, never to a single
// sentence for all of them.
//
// Mutation: drop watchClose from dial and every row goes red; drop the
// classification and the two rows each lose their remedy; record a second
// close over the first and the "once" half of the first row goes red.
func TestAConnectionClosedForGoodIsReportedOnceWithItsCause(t *testing.T) {
	t.Parallel()

	t.Run("a live reload of a lower max_payload and a message past it", func(t *testing.T) {
		t.Parallel()
		const limit = queue.MaxPayloadBytes / 4
		const token = "the-one-this-node-holds"
		srv := externaltest.Start(t, queue.MaxPayloadBytes, func(o *server.Options) {
			o.Authorization = token
		})
		q := newQueueWith(t, Config{URL: srv.URL(), Token: token})
		// A SECOND WATCHED CONNECTION, dialled before the reload, which
		// the case closes later for a DIFFERENT reason: the cause a node
		// stops for is the first, and a later close must not rewrite it.
		second, err := q.DialWatched()
		if err != nil {
			t.Fatalf("DialWatched: %v", err)
		}
		t.Cleanup(second.Close)

		srv.Reload(limit)
		sendPast(t, q.nc, limit)

		cause := lost(t, q)
		for _, want := range []string{"Maximum Payload Violation",
			"max_payload: 8MB", "stream.url", srv.HostPort(), "live reload",
			"refuses to start"} {
			if !strings.Contains(cause.Error(), want) {
				t.Errorf("the cause %q does not say %q", cause, want)
			}
		}
		if strings.Contains(cause.Error(), token) {
			t.Errorf("the cause %q carries the token the node signs in with", cause)
		}
		// AND IT IS THE SENTENCE THE DEPLOYMENT GUIDE PRINTS, which is what
		// an operator whose node exited greps for: the guide's example is
		// this reload, against a server at its own address.
		guide, err := os.ReadFile(filepath.Join(sourcetree.Root(t),
			"docs", "guides", "deployment.md"))
		if err != nil {
			t.Fatalf("read the deployment guide: %v", err)
		}
		printed := strings.ReplaceAll(cause.Error(), "nats://"+srv.HostPort(),
			"nats://nats-1.internal:4222")
		if !strings.Contains(string(guide), printed+"\n") {
			t.Errorf("docs/guides/deployment.md does not print the sentence a node "+
				"stops with; it should end a line with:\n%s", printed)
		}

		// AND ONCE: the second connection is closed for good too, by the
		// server refusing its credentials, and the cause stays the first.
		srv.RestartAs(func(o *server.Options) { o.Authorization = "rotated" })
		deadline := time.Now().Add(lostWithin)
		for !second.IsClosed() {
			if time.Now().After(deadline) {
				t.Fatalf("the second connection is %v, not closed", second.Status())
			}
			time.Sleep(20 * time.Millisecond)
		}
		time.Sleep(quietFor)
		if got := q.LostCause(); got == nil || got.Error() != cause.Error() {
			t.Errorf("a second close rewrote the cause a node stops for:\n"+
				"first:  %v\nnow:    %v", cause, got)
		}
	})

	t.Run("credentials the server refuses again on the reconnect", func(t *testing.T) {
		t.Parallel()
		const token = "the-one-this-node-holds"
		srv := externaltest.Start(t, queue.MaxPayloadBytes, func(o *server.Options) {
			o.Authorization = token
		})
		q := newQueueWith(t, Config{URL: srv.URL(), Token: token})

		srv.RestartAs(func(o *server.Options) { o.Authorization = "rotated" })

		cause := lost(t, q)
		for _, want := range []string{"Authorization Violation", "stream.token",
			"stream.credentials", srv.HostPort()} {
			if !strings.Contains(cause.Error(), want) {
				t.Errorf("the cause %q does not say %q", cause, want)
			}
		}
		for _, never := range []string{token, "max_payload"} {
			if strings.Contains(cause.Error(), never) {
				t.Errorf("the cause %q says %q", cause, never)
			}
		}
	})
}

// ONLY THE QUEUE'S OWN CONNECTION CLOSING LOSES THE ACKS.
//
// Every delivery the queue made is settled over its own connection, so that one
// closing for good is what makes a running turn's outcome unrecordable — and
// what a drain stops waiting on. A second watched connection closing (the
// coordination store's, on an embedded broker) stops the node too, but every
// ack still lands, so it must NOT read as lost acks: a drain cut there would
// cancel turns whose outcome the broker was about to record.
//
// Staged on an operator's server, through the one gesture that closes ONE
// connection and leaves the other: a max_payload lowered by a live reload and a
// message past it, sent first on the second connection and then on the queue's.
//
// Mutation: dial DialWatched's connection as the one that settles and the first
// half goes red; dial the queue's own without it, or record a close without
// asking which connection it was, and the second half does.
func TestOnlyTheQueuesOwnConnectionClosingLosesTheAcks(t *testing.T) {
	t.Parallel()
	const limit = queue.MaxPayloadBytes / 4
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	q := newQueueWith(t, Config{URL: srv.URL()})
	second, err := q.DialWatched()
	if err != nil {
		t.Fatalf("DialWatched: %v", err)
	}
	t.Cleanup(second.Close)

	srv.Reload(limit)
	sendPast(t, second, limit)
	first := lost(t, q)
	select {
	case <-q.AcksLost():
		t.Fatalf("a second watched connection closing for good (%v) reported the "+
			"queue's acks lost, so a drain would cancel turns whose outcome still "+
			"lands over the queue's own connection", first)
	case <-time.After(quietFor):
	}
	if q.nc.IsClosed() {
		t.Fatal("the queue's own connection closed with the second one: the case " +
			"staged nothing it can tell apart")
	}

	sendPast(t, q.nc, limit)
	select {
	case <-q.AcksLost():
	case <-time.After(lostWithin):
		t.Fatalf("the queue's own connection was closed for good (%v) and its "+
			"acks were not reported lost within %s, so a drain would go on "+
			"waiting for turns nothing can acknowledge", q.nc.Status(), lostWithin)
	}
	// And the cause a node stops for is still the first close's.
	if got := q.LostCause(); got == nil || got.Error() != first.Error() {
		t.Errorf("the queue's own close rewrote the cause:\nfirst: %v\nnow:   %v",
			first, got)
	}
}

// A START THAT FAILED ON A LOST CONNECTION CARRIES THE CAUSE, AND ONE THAT DID
// NOT IS LEFT ALONE.
//
// The step that meets a connection NATS closed for good fails in its own words —
// "connection closed" — while the sentence naming why and what to change is
// recorded beside it. The composition every start reads it through (this
// queue's own open and an engine's boot over it) has to put the cause FIRST,
// keep the step's own error in the chain, and touch no error at all when
// nothing was lost: a boot refused over a bad config is not a lost broker.
//
// Mutation: answer err unchanged with a cause recorded and the second half goes
// red; compose a nil cause into a failure that had nothing to do with the
// broker and the first does.
func TestAStartThatFailedOnALostConnectionCarriesTheCause(t *testing.T) {
	t.Parallel()
	step := fmt.Errorf("ensure stream CREWLET_AGENT: %w", nats.ErrConnectionClosed)
	l := newConnectionLoss()
	if got := l.during(step); got == nil || got.Error() != step.Error() {
		t.Errorf("nothing was lost and the step's error came back as %q", got)
	}
	if got := l.during(nil); got != nil {
		t.Errorf("no step failed and the composition answered %q", got)
	}

	cause := lostConnection(lostServer(false, "nats://nats-1.internal:4222"),
		nats.ErrAuthorization, false)
	l.record(cause, true)
	got := l.during(step)
	if got == nil || !strings.HasPrefix(got.Error(), cause.Error()) {
		t.Fatalf("a start that failed beside a recorded loss said %q; it should "+
			"open with the cause, %q", got, cause)
	}
	if !strings.Contains(got.Error(), step.Error()) {
		t.Errorf("%q drops the step's own error, %q", got, step)
	}
	for _, in := range []error{cause, nats.ErrConnectionClosed} {
		if !errors.Is(got, in) {
			t.Errorf("%q no longer carries %v in its chain", got, in)
		}
	}
}

// A CLOSE THIS NODE MADE ITSELF IS NOT A LOSS — and nor is a close of a
// connection whose owner is the one to notice it.
//
// The client calls a closed handler for its owner's own Close unless it is told
// not to, so a handler installed alone made every graceful stop a lost broker:
// the queue's Stop, and the coordination store's connection the engine closes
// itself on the way down. And a snapshot donor's connection, dialled through
// DialOwned, serves peers rather than this node — closed for good under it,
// the node still serves its own company, so that close is the donor's to
// redial and never a reason to stop.
//
// Mutation: drop NoCallbacksAfterClientClose from watchClose and the first two
// rows go red; give DialOwned the queue's loss and the third does.
func TestACloseThisNodeMadeIsNotALoss(t *testing.T) {
	t.Parallel()
	quiet := func(t *testing.T, q *Queue, what string) {
		t.Helper()
		select {
		case <-q.Lost():
			t.Fatalf("%s reported a lost broker: %v", what, q.LostCause())
		case <-time.After(quietFor):
		}
	}

	t.Run("the queue's own stop", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		q, err := Open(t.Context(), Config{URL: srv.URL()})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		quiet(t, q, "the queue's own Stop")
	})

	t.Run("an owner closing a watched connection", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		q := newQueueWith(t, Config{URL: srv.URL()})
		watched, err := q.DialWatched()
		if err != nil {
			t.Fatalf("DialWatched: %v", err)
		}
		watched.Close()
		quiet(t, q, "an owner closing what DialWatched gave it")
	})

	t.Run("a donor's own connection closed for good under it", func(t *testing.T) {
		t.Parallel()
		const limit = queue.MaxPayloadBytes / 4
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		q := newQueueWith(t, Config{URL: srv.URL()})
		owned, err := q.DialOwned()
		if err != nil {
			t.Fatalf("DialOwned: %v", err)
		}
		t.Cleanup(owned.Close)

		srv.Reload(limit)
		sendPast(t, owned, limit)
		deadline := time.Now().Add(lostWithin)
		for !owned.IsClosed() {
			if time.Now().After(deadline) {
				t.Fatalf("the owned connection is %v, not closed: the case "+
					"staged nothing", owned.Status())
			}
			time.Sleep(20 * time.Millisecond)
		}
		quiet(t, q, "a connection DialOwned handed a donor")
	})
}

// EVERY CONNECTION A NODE DEPENDS ON IS WATCHED, ON BOTH DIALS — and the one
// whose loss is its owner's is not.
//
// The embedded broker's connections cannot be closed for good from a test
// without stopping the broker, which ends the case's own connections with it,
// so this is the structural half: what each connection was dialled with. The
// behavioural half is the external rows above, through the dial an operator's
// server takes; the embedded one takes [embeddedServer.connect], and only a
// handler installed there makes the coordination store's connection — the one
// holding every lease on that topology — a loss the node hears of.
//
// Mutation: drop watchClose from connect and the embedded rows go red; hand
// DialOwned the queue's loss and the owned rows do.
func TestEveryConnectionANodeDependsOnIsWatched(t *testing.T) {
	t.Parallel()
	embedded, err := StartServer(t.Context(), Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	t.Cleanup(embedded.Shutdown)
	onEmbedded, err := embedded.Client(t.Context())
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	t.Cleanup(func() { _ = onEmbedded.Stop(context.WithoutCancel(t.Context())) })
	onExternal := newQueueWith(t, Config{URL: externaltest.Start(t, queue.MaxPayloadBytes).URL()})

	want := reflect.ValueOf(lossWatch{}.closed).Pointer()
	for _, q := range []struct {
		topology string
		queue    *Queue
	}{{"embedded", onEmbedded}, {"external", onExternal}} {
		watched, err := q.queue.DialWatched()
		if err != nil {
			t.Fatalf("%s: DialWatched: %v", q.topology, err)
		}
		t.Cleanup(watched.Close)
		owned, err := q.queue.DialOwned()
		if err != nil {
			t.Fatalf("%s: DialOwned: %v", q.topology, err)
		}
		t.Cleanup(owned.Close)

		for name, nc := range map[string]*nats.Conn{
			"the queue's own":          q.queue.Conn(),
			"a second DialWatched one": watched,
		} {
			opts := nc.Opts
			if opts.ClosedCB == nil || reflect.ValueOf(opts.ClosedCB).Pointer() != want {
				t.Errorf("%s: %s connection is not watched, so NATS closing it "+
					"for good leaves the node running on nothing", q.topology, name)
			}
			if !opts.NoCallbacksAfterClientClose {
				t.Errorf("%s: %s connection reports its owner's own Close as a "+
					"lost broker", q.topology, name)
			}
		}
		if owned.Opts.ClosedCB != nil {
			t.Errorf("%s: a DialOwned connection is watched, so a donor's "+
				"connection closing would stop the node it serves peers from",
				q.topology)
		}
	}
}

// THE CAUSE HAS FOUR REMEDIES, and each sentence names its own.
//
// Asserted on the sentence directly for the two shapes no case above can stage
// without stopping a broker under itself: the embedded server's, where no
// setting reaches the server and the remedy is the restart; and a cause the
// client does not recognise, where the server's own log is the only place that
// says why.
//
// Mutation: send the embedded sentence to stream.url, or drop the default's
// pointer at the server's log, and a row goes red.
func TestEachCauseNamesItsOwnRemedy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		cause    error
		embedded bool
		want     []string
		never    []string
	}{
		{"this node's own broker, out of reconnects", nats.ErrNoServers, true,
			[]string{"embedded", "no setting reaches it", "restart", "nats: no servers"},
			[]string{"stream.url", "max_payload: 8MB", "stream.token"}},
		{"an error the client does not recognise", &protoErr{"Unknown Protocol Operation"},
			false, []string{"Unknown Protocol Operation", "does not retry", "server's own log"},
			[]string{"max_payload: 8MB", "stream.token"}},
		{"no error recorded at all", nil, false,
			[]string{"no reason given"}, []string{"max_payload: 8MB", "stream.token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lostConnection(lostServer(tc.embedded, "nats://nats-1.internal:4222"),
				tc.cause, tc.embedded).Error()
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("%q does not say %q", got, want)
				}
			}
			for _, never := range tc.never {
				if strings.Contains(got, never) {
					t.Errorf("%q names %q, a remedy for another cause", got, never)
				}
			}
		})
	}
}

// protoErr is an -ERR the client closed on, as it reports one.
type protoErr struct{ description string }

func (e *protoErr) Error() string { return "nats: " + e.description }
