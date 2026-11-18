package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// lostWithin bounds a wait for a close the server has already decided (the
// client reads its -ERR, or fails its one reconnect a second later). Generous
// because it costs nothing when the close comes; a timeout is the defect.
const lostWithin = 30 * time.Second

// reloadedLimit is the max_payload a case lowers the server to by a live
// reload before sending a message past it.
//
// SMALL ENOUGH THAT THE CLIENT WRITES THE WHOLE MESSAGE TO THE SOCKET AT ONCE,
// so a close for good is the only outcome. The server refuses on the header
// and closes without reading the body; a message still being written then
// fails part way, and since nats.go 1.55.0 the client drops the server's
// refusal unread and reconnects, staging nothing. A few KiB is far below any
// socket buffer and above anything the cases' connections send unprompted.
const reloadedLimit = 4 << 10

// oversized is an event larger than reloadedLimit bytes on the wire and well
// inside the contract, so the client (which still reads the contract's figure
// after a live reload) sends it, and only the server can refuse it.
func oversized(t *testing.T) []byte {
	t.Helper()
	e := ev(1)
	e.Extra = map[string]json.RawMessage{
		"blob": json.RawMessage(`"` + strings.Repeat("x", reloadedLimit+1024) + `"`),
	}
	body, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return body
}

// sendPast sends nc a message past reloadedLimit, which a live reload has
// lowered the server to.
//
// The Flush may fail, since the server closes the connection over the message;
// only the client refusing the message ITSELF stops the case, as it would then
// have learned the reloaded limit and nothing would reach the server.
func sendPast(t *testing.T, nc *nats.Conn) {
	t.Helper()
	if err := nc.Publish("crewlet.lost.oversized", oversized(t)); errors.Is(err, nats.ErrMaxPayload) {
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
	case <-time.After(lostWithin): //fixedwait:liveness
		t.Fatalf("the server closed the connection for good and the queue "+
			"reported nothing within %s", lostWithin)
	}
	cause := q.LostCause()
	if cause == nil {
		t.Fatal("Lost is closed and LostCause is nil: a node would stop naming nothing")
	}
	return cause
}

// closeHandled wraps nc's closed handler (the one it was dialled with, if any)
// with a close of the returned channel: the witness that whatever a close for
// good does about a loss is done.
//
// The client runs the handler later, on the connection's callback goroutine,
// so a loss read once the connection reads as closed races that goroutine.
// Read after this channel closes, the loss holds whatever the handler recorded,
// and nothing it lacks comes later.
//
// Only for a close the CLIENT makes: an owner's close calls no handler under
// [nats.NoCallbacksAfterClientClose], so the channel never closes;
// [clientGoroutines] witnesses that one.
func closeHandled(nc *nats.Conn) <-chan struct{} {
	dialled := nc.ClosedHandler()
	handled := make(chan struct{})
	nc.SetClosedHandler(func(nc *nats.Conn) {
		if dialled != nil {
			dialled(nc)
		}
		close(handled)
	})
	return handled
}

// awaitHandled waits for handled ([closeHandled]): the close for good the case
// staged of what, and that close's handler having run.
func awaitHandled(t *testing.T, handled <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-handled:
	case <-time.After(lostWithin): //fixedwait:liveness
		t.Fatalf("%s was not closed for good within %s, or its closed handler "+
			"never ran: the case staged nothing", what, lostWithin)
	}
}

// clientLabel is the profiler label [clientGoroutines] runs a dial under.
const clientLabel = "crewlet.lost.case"

// clientGoroutines runs dial under a profiler label naming this case, which
// every goroutine the NATS client starts for the connection inherits, and
// returns a count of the labelled goroutines running the client's code.
//
// THE WITNESS FOR A CLOSE THAT QUEUES NO HANDLER. The client calls a
// connection's handlers in order on one goroutine of its own, and a close
// queues its handlers and then that goroutine's return. An owner's close
// queues none under [nats.NoCallbacksAfterClientClose], so once none of the
// connection's goroutines is left, any handler it wrongly queued has run. A
// goroutine profile at debug level 1 is the one place that shows: it prints
// each goroutine's labels beside its stack.
func clientGoroutines(t *testing.T, dial func()) (running func() int) {
	t.Helper()
	pprof.Do(t.Context(), pprof.Labels(clientLabel, t.Name()), func(context.Context) { dial() })
	labelled := fmt.Sprintf("%q:%q", clientLabel, t.Name())
	// THE CLIENT'S PACKAGE AS A STACK NAMES IT, read off one of its functions:
	// the linker writes nats.go's dot as %2e there.
	client := strings.TrimSuffix(
		runtime.FuncForPC(reflect.ValueOf(nats.Connect).Pointer()).Name(), ".Connect")
	running = func() int {
		var profile bytes.Buffer
		if err := pprof.Lookup("goroutine").WriteTo(&profile, 1); err != nil {
			t.Fatalf("read the goroutine profile: %v", err)
		}
		n := 0
		for _, stack := range strings.Split(profile.String(), "\n\n") {
			if !strings.Contains(stack, labelled) || !strings.Contains(stack, "\t"+client+".") {
				continue
			}
			for _, line := range strings.Split(stack, "\n") {
				var count int
				if _, err := fmt.Sscanf(line, "%d @", &count); err == nil {
					n += count
					break
				}
			}
		}
		return n
	}
	if running() == 0 {
		t.Fatal("no goroutine of the NATS client's carries this case's label, so " +
			"the case could not tell when the ones it started for the connection " +
			"had returned")
	}
	return running
}

// awaitClientGone waits for every goroutine running counts ([clientGoroutines])
// to have returned, once the owner of what was dialled has closed it.
//
// Every 10 ms: the goroutines return within microseconds of the close, and
// each poll takes a stop-the-world goroutine profile.
func awaitClientGone(t *testing.T, running func() int, what string) {
	t.Helper()
	deadline := time.Now().Add(lostWithin)
	for running() > 0 {
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the NATS client's goroutines for %s were still running %s "+
				"after its owner closed it", what, lostWithin)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// A CONNECTION NATS CLOSES FOR GOOD IS REPORTED, ONCE, WITH ITS CAUSE.
//
// Staged in the two ordinary shapes an operator produces under a running node,
// where the client stops reconnecting on its own: a max_payload lowered by a
// LIVE RELOAD, and the server's credentials rotated. Unreported, either leaves
// a node that looks alive with no publish, consumer or lease renewal. The
// cause is the sentence the node stops with, so each row holds it to its own
// remedy.
//
// Mutation: drop watchClose from dial and every row goes red; drop the
// classification and the two rows each lose their remedy; record a second
// close over the first and the "once" half of the first row goes red.
func TestAConnectionClosedForGoodIsReportedOnceWithItsCause(t *testing.T) {
	t.Parallel()

	t.Run("a live reload of a lower max_payload and a message past it", func(t *testing.T) {
		t.Parallel()
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
		secondHandled := closeHandled(second)

		srv.Reload(reloadedLimit)
		sendPast(t, q.nc)

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

		// AND ONCE: the server's refusing the second connection's credentials
		// closes it for good too, and the cause stays the first, read once that
		// close's handler (where a second record would write) has run.
		srv.RestartAs(func(o *server.Options) { o.Authorization = "rotated" })
		awaitHandled(t, secondHandled, "the second connection")
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
// Every delivery settles over the queue's own connection, so only its close
// leaves a turn's outcome unrecordable and ends a drain's wait. A second
// watched connection (the coordination store's, on an embedded broker) closing
// stops the node, but acks still land: read as lost, a drain would cancel turns
// about to be recorded. Staged by a live reload and a message past it, on the
// second connection (read once HANDLED, [closeHandled]) and then the queue's.
//
// Mutation: dial DialWatched's connection as the one that settles and the first
// half goes red; dial the queue's own without it, or record a close without
// asking which connection it was, and the second half does.
func TestOnlyTheQueuesOwnConnectionClosingLosesTheAcks(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	q := newQueueWith(t, Config{URL: srv.URL(), Token: externaltest.Token})
	second, err := q.DialWatched()
	if err != nil {
		t.Fatalf("DialWatched: %v", err)
	}
	t.Cleanup(second.Close)
	handled := closeHandled(second)

	srv.Reload(reloadedLimit)
	sendPast(t, second)
	awaitHandled(t, handled, "the second watched connection")
	first := lost(t, q)
	if closedYet(q.AcksLost()) {
		t.Fatalf("a second watched connection closing for good (%v) reported the "+
			"queue's acks lost, so a drain would cancel turns whose outcome still "+
			"lands over the queue's own connection", first)
	}
	if q.nc.IsClosed() {
		t.Fatal("the queue's own connection closed with the second one: the case " +
			"staged nothing it can tell apart")
	}

	sendPast(t, q.nc)
	select {
	case <-q.AcksLost():
	case <-time.After(lostWithin): //fixedwait:liveness
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

// A READER THAT HAS SEEN THE LOSS SEES WHETHER IT LOST THE ACKS.
//
// `crewlet run` wakes on Lost and asks AcksLost without waiting, to say before
// the drain whether running turns finish, so record closes AcksLost no later
// than Lost, or that reader promises turns the drain is cancelling.
//
// THE WINDOW IS HELD OPEN, as a race hit it under one run in twenty: the
// case enters Lost's close via the sync.Once record uses, and holds it. A
// correct record has closed AcksLost by then; one closing Lost first never
// reaches the acks close. Held at the acks close, it would wait in vain.
//
// Mutation: close done before acks in record and this goes red on every run.
func TestAReaderThatHasSeenTheLossSeesWhetherItLostTheAcks(t *testing.T) {
	t.Parallel()
	cause := lostConnection(lostServer(false, "nats://nats-1.internal:4222"),
		nats.ErrAuthorization, false)
	l := newConnectionLoss()

	entered, release := make(chan struct{}), make(chan struct{})
	go l.once.Do(func() {
		close(entered)
		<-release
		l.cause = cause
		close(l.done)
	})
	<-entered
	recorded := make(chan struct{})
	go func() {
		defer close(recorded)
		l.record(cause, true)
	}()

	select {
	case <-l.acks:
	case <-time.After(lostWithin): //fixedwait:liveness
		// The Lost close is held, and record had not closed the acks on its
		// way to it.
		t.Fatal("record did not close AcksLost before it reached Lost's close, so " +
			"a reader woken by Lost could find the queue's own connection lost " +
			"and its acks still standing, and the drain line would promise the " +
			"running turns finish while the drain cancels them")
	}

	close(release)
	select {
	case <-recorded:
	case <-time.After(lostWithin): //fixedwait:liveness
		t.Fatalf("record did not return within %s of Lost's close being let go", lostWithin)
	}
	if !closedYet(l.done) || !closedYet(l.acks) {
		t.Fatalf("record returned with Lost closed %v and AcksLost closed %v, "+
			"want both", closedYet(l.done), closedYet(l.acks))
	}
}

// closedYet reports whether ch is closed, without waiting for it.
func closedYet(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A START THAT FAILED ON A LOST CONNECTION CARRIES THE CAUSE, AND ONE THAT DID
// NOT IS LEFT ALONE.
//
// The step that meets a connection NATS closed for good fails as "connection
// closed", while the sentence naming why and what to change is recorded beside
// it. The composition every start reads it through (the queue's open, an
// engine's boot) puts the cause FIRST, keeps the step's error in the chain, and
// touches no error when nothing was lost: a bad config is not a lost broker.
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

// A START WAITS FOR THE CAUSE OF A CONNECTION THE CLIENT HAS ALREADY CLOSED —
// and waits for nothing where no watched connection is closed.
//
// The client queues the closed handler under its lock and runs it on its own
// goroutine, so the failed step reaches the composition before the cause is
// recorded; the composition waits while the connection reads as closed. Here
// the handler is the case's (nc is dialled unwatched), so the case orders it.
// Each composition runs in a bubble, where [synctest.Wait] returns once it has
// answered or parked; the connection stays outside, on a real server.
//
// Mutation: drop the settle from during and the second half goes red; settle
// on a connection that is still open and the first half does.
func TestAStartWaitsForTheCauseOfAConnectionAlreadyClosed(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	nc, err := dial(Config{URL: srv.URL(), Token: externaltest.Token}, watched{}, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(nc.Close)
	step := fmt.Errorf("ensure stream CREWLET_AGENT: %w", nats.ErrConnectionClosed)

	// composing starts a start's composition over l, which tracks nc, and
	// returns once it has answered (into the channel) or parked. Called inside
	// the bubble l was made in.
	composing := func(l *connectionLoss) <-chan error {
		watched{loss: l}.track(nc)
		out := make(chan error, 1)
		go func() { out <- l.during(step) }()
		synctest.Wait()
		return out
	}

	// OPEN: nothing to wait for, so the step's own error, at once.
	synctest.Test(t, func(t *testing.T) {
		l := newConnectionLoss()
		select {
		case got := <-composing(l):
			if got == nil || got.Error() != step.Error() {
				t.Errorf("nothing is closed and the step's error came back as %q", got)
			}
		default:
			// Let go, so the bubble's goroutine ends with it.
			l.record(errors.New("the case letting the composition go"), false)
			t.Fatal("a start whose connections are all open waited for a loss " +
				"nothing will record")
		}
	})

	// CLOSED BY THE CLIENT, its handler not yet run: the connection is
	// unwatched, so no handler records anything, and the one the case gives it
	// only says the close was made.
	closed := closeHandled(nc)
	srv.Reload(reloadedLimit)
	sendPast(t, nc)
	awaitHandled(t, closed, "the connection")
	synctest.Test(t, func(t *testing.T) {
		l := newConnectionLoss()
		composed := composing(l)
		select {
		case got := <-composed:
			t.Fatalf("the start composed %q while the connection it failed on was "+
				"closed and its cause not yet recorded: an operator reads the bare "+
				"step and not what to change", got)
		default:
		}
		cause := lostConnection(lostServer(false, srv.URL()), nc.LastError(), false)
		l.record(cause, true)
		if got := <-composed; got == nil || !strings.HasPrefix(got.Error(), cause.Error()) ||
			!errors.Is(got, nats.ErrConnectionClosed) {
			t.Errorf("the start said %q once the cause was recorded; it should "+
				"open with %q and keep the step's own error", got, cause)
		}
	})
}

// A STOPPED QUEUE IS ANSWERED FROM WHAT IS RECORDED, AND NEVER WAITED ON.
//
// Its own Stop closed its connection, and under NoCallbacksAfterClientClose no
// handler comes for an owner's close, so a wait would be for ever. Likewise an
// open that fails for a reason of its own (a cancelled context) reads the cause
// before it closes its connection: closed first, the read would wait for a
// handler its own close suppressed.
//
// Mutation: drop the stopped-queue branch from LostDuring and the first half
// hangs; close the connection before the read in abandonOpen and the second
// does.
func TestAStartIsNeverWaitedOnForItsOwnClose(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes)
	step := fmt.Errorf("record a node's admission: %w", nats.ErrConnectionClosed)

	q := newQueueWith(t, Config{URL: srv.URL(), Token: externaltest.Token})
	if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	answered := make(chan error, 1)
	go func() { answered <- q.LostDuring(step) }()
	select {
	case got := <-answered:
		if got == nil || got.Error() != step.Error() {
			t.Errorf("a stopped queue that lost nothing answered %q", got)
		}
	case <-time.After(lostWithin): //fixedwait:liveness
		t.Fatalf("LostDuring on a stopped queue waited %s for a handler its own "+
			"Stop suppressed", lostWithin)
	}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	stop := context.WithoutCancel(cancelled)
	opened := make(chan error, 1)
	go func() {
		q, err := Open(cancelled, Config{URL: srv.URL(), Token: externaltest.Token})
		if err == nil {
			_ = q.Stop(stop)
		}
		opened <- err
	}()
	select {
	case err := <-opened:
		if err == nil {
			t.Fatal("an open under a cancelled context succeeded: the case " +
				"staged nothing")
		}
	case <-time.After(lostWithin): //fixedwait:liveness
		t.Fatalf("an open that failed for a reason of its own waited %s for a "+
			"handler its own close suppressed", lostWithin)
	}
}

// A CLOSE THIS NODE MADE ITSELF IS NOT A LOSS — and nor is a close of a
// connection whose owner is the one to notice it.
//
// Without NoCallbacksAfterClientClose the client calls the closed handler on an
// owner's own Close, making every graceful stop (the queue's, the coordination
// store's) a lost broker. A donor's DialOwned connection serves peers, so its
// close is the donor's to redial. Each row is read once anything that could
// record a loss is done: the donor's CLIENT close once handled ([closeHandled]),
// the owners' closes once the client's goroutines are gone ([clientGoroutines]).
//
// Mutation: drop NoCallbacksAfterClientClose from watchClose and the first two
// rows go red; give DialOwned the queue's loss and the third does.
func TestACloseThisNodeMadeIsNotALoss(t *testing.T) {
	t.Parallel()
	quiet := func(t *testing.T, q *Queue, what string) {
		t.Helper()
		if closedYet(q.Lost()) {
			t.Fatalf("%s reported a lost broker: %v", what, q.LostCause())
		}
	}

	t.Run("the queue's own stop", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		var q *Queue
		running := clientGoroutines(t, func() {
			var err error
			if q, err = Open(t.Context(), Config{URL: srv.URL(), Token: externaltest.Token}); err != nil {
				t.Fatalf("Open: %v", err)
			}
		})
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		awaitClientGone(t, running, "the queue's own connection")
		quiet(t, q, "the queue's own Stop")
	})

	t.Run("an owner closing a watched connection", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		q := newQueueWith(t, Config{URL: srv.URL(), Token: externaltest.Token})
		var conn *nats.Conn
		running := clientGoroutines(t, func() {
			var err error
			if conn, err = q.DialWatched(); err != nil {
				t.Fatalf("DialWatched: %v", err)
			}
		})
		conn.Close()
		awaitClientGone(t, running, "the watched connection")
		quiet(t, q, "an owner closing what DialWatched gave it")
	})

	t.Run("a donor's own connection closed for good under it", func(t *testing.T) {
		t.Parallel()
		srv := externaltest.Start(t, queue.MaxPayloadBytes)
		q := newQueueWith(t, Config{URL: srv.URL(), Token: externaltest.Token})
		owned, err := q.DialOwned()
		if err != nil {
			t.Fatalf("DialOwned: %v", err)
		}
		t.Cleanup(owned.Close)
		handled := closeHandled(owned)

		srv.Reload(reloadedLimit)
		sendPast(t, owned)
		awaitHandled(t, handled, "the owned connection")
		quiet(t, q, "a connection DialOwned handed a donor")
	})
}

// EVERY CONNECTION A NODE DEPENDS ON IS WATCHED, ON BOTH DIALS — and the one
// whose loss is its owner's is not.
//
// The structural half: what each connection was dialled with, since an
// embedded broker's connection cannot be closed for good without stopping the
// broker under the case. The external rows above are the behavioural half; on
// the embedded topology only [embeddedServer.connect]'s handler makes the
// coordination store's connection, holding every lease, a loss the node hears
// of. This is the gate ADR-0001 names.
//
// Mutation: drop watchClose from connect and the embedded rows go red; hand
// DialOwned the queue's loss and the owned rows do.
func TestEveryConnectionANodeDependsOnIsWatched(t *testing.T) {
	t.Parallel()
	embedded, err := StartServer(t.Context(), Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatalf("StartServer: %v", err)
	}
	t.Cleanup(embedded.Shutdown)
	onEmbedded, err := embedded.Client(t.Context())
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	t.Cleanup(func() { _ = onEmbedded.Stop(context.WithoutCancel(t.Context())) })
	onExternal := newQueueWith(t, Config{URL: externaltest.Start(t, queue.MaxPayloadBytes).URL(),
		Token: externaltest.Token})

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
// Asserted on the sentence for the two shapes no case above can stage without
// stopping a broker under itself: the embedded server's, where no setting
// reaches it and the remedy is the restart; and a cause the client does not
// recognise, where only the server's own log says why.
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
