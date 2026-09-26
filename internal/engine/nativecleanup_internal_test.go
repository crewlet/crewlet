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
	"github.com/crewlet/crewlet/internal/search"
)

// A FAILED NATIVE START MUST LEAVE NOTHING RUNNING.
//
// [Engine.startNative] brings the state log up first and builds six things
// after it, four of which used to unwind with a bare `cancel()`. That cancels
// this node's own context and nothing else: the log's apply loops, its
// position heartbeat and its snapshot donor run under a context of the LOG's,
// and a slice answerer's registration is detached from the caller's inside the
// queue. So the failure returned, [Engine.New]'s failure path closed the store
// underneath appliers that were still committing into it, and a caller that
// retried ran a second runtime beside the first.

// nativeCleanupCompany is the smallest company that runs both native backends,
// which is what puts every construction below the state log on the path.
const nativeCleanupCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`

// dialQueue is this node's REAL broker, watching the second connection the
// state log opens for itself.
//
// The real one, embedded rather than faked, because the subject is what the
// state log's own loops are doing when a failure lands — and those need a
// broker that provisions streams, serves consumers and takes a second
// connection.
type dialQueue struct {
	*jetstream.Queue

	mu     sync.Mutex
	dialed []*nats.Conn
}

// deafQueue is that broker with one verb broken: it refuses to register the
// search fan-out's answerer.
//
// Serve is the one verb a test can fail on demand AFTER the log is up, and it
// is the exact failure the fan-out registration reports. ONLY that subject:
// the estate server registers before the native start, and a broker refusing
// it too would fail the boot before the state log existed — a case that could
// no longer see what it is about.
type deafQueue struct{ *dialQueue }

// Serve refuses the slice registration, which is where startNative fails.
func (q *deafQueue) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	if subject == search.SliceSubject {
		return nil, errors.New("jetstream: this broker registers no answerer")
	}
	return q.dialQueue.Serve(ctx, subject, h)
}

// DialOwned hands out the real second connection and REMEMBERS it.
//
// That connection is the observation: the state log's donor opens one of its
// own and closes it when its context ends, so a connection still open after
// startNative has returned an error is a state log still running. Watching the
// goroutines directly is not available from here — the runtime is never
// installed on the engine on a failure — and a goroutine count would be a
// number every other test in this package moves.
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

// awaitDial waits for the state log to open its own connection.
//
// A WAIT rather than a read, because the donor dials inside its goroutine: on
// the failing build the error is returned before that happens, and a test that
// read once would report no leak precisely because the leak had not got going
// yet.
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
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
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
	// assertion — and so this case sees what an embedded caller sees,
	// which is the deployment where a leaked applier outlives the failure
	// by the life of the process rather than by milliseconds.
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

// THE CLEANUP'S ORDER IS THE CONTRACT, not just that it runs.
//
// A withdrawal after the cancel is an answerer cancelled mid-scan, which a
// coordinator counts as a silent empty slice rather than a missing one; a
// cleanup that does not WAIT reports a node stopped while its loops are still
// writing. Asserted on a half-built runtime — no log, no index, nothing the
// engine installed — because that is exactly the shape [Engine.startNative]'s
// failure path reaches it with.
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
	// A LOOP THAT TAKES A MOMENT TO UNWIND, which is every real one: the
	// wait is only observable against a goroutine that has not finished
	// the instant it is told to.
	var unwound atomic.Bool
	n.done.Add(1)
	go func() {
		defer n.done.Done()
		<-runCtx.Done()
		time.Sleep(50 * time.Millisecond)
		unwound.Store(true)
	}()

	// AN ALREADY-CANCELLED CALLER, which is the ordinary case on both
	// paths into this: [Engine.Stop] is reached on a signal context and
	// [Engine.New]'s failure path unwinds a boot whose own cancellation is
	// frequently the failure. The withdrawal is a call to the broker, so
	// the caller's context travelling here for its values must not bring
	// its cancellation with it.
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
	if n.log != nil {
		t.Fatal("this case is meant to run on a half-built runtime")
	}
}
