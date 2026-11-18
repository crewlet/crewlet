package engine

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/search"
)

// A FAILED NATIVE START MUST LEAVE NOTHING RUNNING.
//
// [Engine.startNative] brings the state log up first and builds more after it,
// so a later failure must stop the log too: a bare `cancel()` reaches only this
// node's context, while the log's apply loops, position heartbeat and snapshot
// donor run under the LOG's, and a slice answerer's registration is detached
// inside the queue. Left running, they commit into a store [Engine.New]'s
// failure path closes, beside whatever a retry starts.

// nativeCleanupCompany is the smallest company that runs every native backend —
// the tracker, the knowledge base and chat, none of them on a vendor — which is
// what puts every construction below the state log on the path.
const nativeCleanupCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

// dialQueue is this node's REAL embedded broker, recording the second
// connection the state log opens for itself: the subject is what the log's own
// loops do when a failure lands, which needs a broker that provisions streams,
// serves consumers and takes a second connection.
type dialQueue struct {
	*jetstream.Queue

	mu     sync.Mutex
	dialed []*nats.Conn
}

// deafQueue is that broker refusing to register the search fan-out's answerer:
// Serve is the one verb a test can fail on demand AFTER the log is up. ONLY ON
// THE SEARCH SLICE: the estate server and the fleet history's answerer register
// before the native start, and refusing them fails the boot before the state
// log starts.
type deafQueue struct{ *dialQueue }

// Serve refuses the slice subject, which is where startNative's registration
// fails, and registers every other answerer for real.
func (q *deafQueue) Serve(ctx context.Context, subject string, answers int,
	fn queue.AnswerFunc) (queue.Unsubscribe, error) {

	if subject != search.SliceSubject {
		return q.dialQueue.Serve(ctx, subject, answers, fn)
	}
	return nil, errors.New("jetstream: this broker registers no answerer")
}

// DialOwned hands out the real second connection and REMEMBERS it: the state
// log's donor opens one and closes it when its context ends, so one still open
// after startNative failed is a state log still running. Not the goroutines:
// the runtime is never installed on a failure, and a goroutine count moves
// with every other test in this package.
func (q *dialQueue) DialOwned() (*nats.Conn, error) {
	nc, err := q.Queue.DialOwned()
	if err == nil {
		q.mu.Lock()
		q.dialed = append(q.dialed, nc)
		q.mu.Unlock()
	}
	return nc, err
}

// conns is what the log has dialled for itself so far.
func (q *dialQueue) conns() []*nats.Conn {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*nats.Conn(nil), q.dialed...)
}

// awaitDial waits for the state log to open its own connection. A WAIT, not a
// read: the donor dials inside its goroutine, so on a leaking build the error
// can return first and a single read would miss the leak.
func awaitDial(t *testing.T, q *dialQueue) []*nats.Conn {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if conns := q.conns(); len(conns) > 0 {
			return conns
		}
		if time.Now().After(deadline) {
			t.Fatal("the state log opened no connection of its own, so this " +
				"case cannot see whether its loops were stopped — if the " +
				"snapshot donor moved, move the observation with it rather " +
				"than deleting the case")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAFailedNativeStartStopsTheStateLogItStarted(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	real, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend this case needs",
			back.Queue)
	}
	deaf := &deafQueue{dialQueue: &dialQueue{Queue: real}}
	// BORROWED backends, so the failure path leaves them open for the
	// assertion, as an embedded caller sees it: there a leaked applier
	// outlives the failure by the life of the process.
	borrowed := *back
	borrowed.Queue = deaf

	if _, err := New(t.Context(), Options{
		Bootstrap: &b, Company: cfg, Backends: &borrowed,
	}); err == nil {
		t.Fatal("a broker that registers no answerer built an engine anyway")
	}

	for _, nc := range awaitDial(t, deaf.dialQueue) {
		if !nc.IsClosed() {
			t.Error("the native start failed and the state log is still " +
				"connected: its apply loops, its position heartbeat and its " +
				"donor are still running — against a store the caller is " +
				"free to close, and beside whatever a retry starts next")
		}
	}
}

// THE CLEANUP'S ORDER IS THE CONTRACT, not just that it runs: a withdrawal
// after the cancel kills an answerer mid-scan, which a coordinator counts as a
// silent empty slice, not a missing one; a cleanup that does not WAIT reports a node stopped while
// its loops still write. On a half-built runtime, the shape
// [Engine.startNative]'s failure path reaches it with.
func TestTheNativeCleanupWithdrawsBeforeItCancelsAndWaits(t *testing.T) {
	t.Parallel()
	runCtx, cancel := context.WithCancel(context.Background())
	n := &native{run: runCtx, stop: cancel}

	var (
		withdrawals int
		liveAtCall  bool
		handedLive  bool
	)
	n.stopSlices = func(ctx context.Context) error {
		withdrawals++
		liveAtCall = runCtx.Err() == nil
		handedLive = ctx.Err() == nil
		return nil
	}
	// A LOOP THAT TAKES A MOMENT TO UNWIND, as every real one does: the wait
	// is observable only against a goroutine not finished when told to.
	var unwound atomic.Bool
	n.done.Add(1)
	go func() {
		defer n.done.Done()
		<-runCtx.Done()
		time.Sleep(50 * time.Millisecond)
		unwound.Store(true)
	}()

	// AN ALREADY-CANCELLED CALLER, the ordinary case on both paths here
	// ([Engine.Stop] on a signal context, [Engine.New]'s failure path
	// unwinding a cancelled boot): the withdrawal calls the broker, so it
	// takes the caller's values without its cancellation.
	dead, kill := context.WithCancel(context.Background())
	kill()
	n.shutdown(dead)

	if withdrawals != 1 {
		t.Errorf("the answerer was withdrawn %d times, want exactly once — a "+
			"registration left behind keeps claiming buckets this node no "+
			"longer scans", withdrawals)
	}
	if !liveAtCall {
		t.Error("the answerer was withdrawn after the run context was " +
			"cancelled, so a scan in flight is killed rather than finished — " +
			"its coordinator counts an empty slice as an answer")
	}
	if !handedLive {
		t.Error("the withdrawal was handed a cancelled context, so the " +
			"unregistration itself cannot reach the broker — the caller's " +
			"context is passed for its values and must be stripped of its " +
			"cancellation on the way in")
	}
	if !unwound.Load() {
		t.Error("shutdown returned while a loop it was joining had not " +
			"finished — the caller is told this node stopped while it is " +
			"still writing")
	}
	if runCtx.Err() == nil {
		t.Error("shutdown returned without cancelling the runtime's context, " +
			"so every loop under it runs on")
	}
	if n.indexer != nil {
		t.Fatal("this case is meant to run on a half-built runtime")
	}
}
