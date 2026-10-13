package kv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/period"
)

// A COUNTER WRITE OUTLASTS ANY NUMBER OF LOST RACES.
//
// A lost compare-and-swap says another node's write landed, which is a store
// answering, so the count of them is how many peers wrote rather than whether
// the counter works. The bound this replaced was sixteen attempts, and a write
// that lost a seventeenth race failed its round as an outage while every
// request it made was answered — and when the write was the company's, the
// round the vendor had billed was recorded nowhere. Three times that many
// losses here, on the company's counter, with the patience long enough that
// only the count could stop it.
func TestACounterWriteOutlastsAnyNumberOfLostRaces(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	store.patience = time.Minute
	seat := coord.AgentScope("x")
	charge(t, store, seat, nil) // the record exists, so every write is an update

	const races = 48
	losing := loseRaces{KeyValue: store.budgets, key: encodeKey(coord.OrgScope)}
	losing.remaining.Store(races)
	store.budgets = &losing

	if got := charge(t, store, seat, coord.Caps{period.Day: 100}); !got.OK {
		t.Fatalf("Charge = %+v after %d lost races, want it admitted", got, races)
	}
	if lost := losing.lost.Load(); lost != races {
		t.Fatalf("the charge lost %d races, want the %d injected: the fault did not "+
			"reach the company's write", lost, races)
	}
	if used := orgSpent(t, store); used != [3]int{20, 20, 20} {
		t.Errorf("org day/week/month = %v, want both rounds counted once", used)
	}
}

// AND GIVES UP WHEN ITS PATIENCE RUNS OUT, AS AN OUTAGE AND NEVER A REFUSAL.
//
// A write no lost race may stop still has to answer its caller: a round's
// charge runs on a context that outlives the turn, so the counter's patience is
// the only thing between a counter written faster than it can serve and a turn
// that waits for ever. What it answers is the contract's unknown — the round
// stops, and nobody is told the company is out of budget — and it records
// nothing, because every attempt it made completed and lost.
func TestACounterWriteGivesUpWhenItsPatienceRunsOut(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	charge(t, store, seat, nil)

	const patience = 200 * time.Millisecond
	store.patience = patience
	losing := loseRaces{KeyValue: store.budgets, key: encodeKey(coord.OrgScope), forever: true}
	store.budgets = &losing

	type answer struct {
		got coord.Spend
		err error
	}
	answered := make(chan answer, 1)
	began := time.Now()
	go func() {
		got, err := store.Charge(t.Context(), coord.ChargeRequest{
			Seat: seat, Tokens: 10, Windows: testWindows(), OrgCaps: coord.Caps{period.Day: 100},
		})
		answered <- answer{got, err}
	}()
	var a answer
	select {
	case a = <-answered:
	case <-time.After(30 * time.Second):
		t.Fatal("a charge that lost every race was still racing after 30s: nothing bounds its patience")
	}
	took := time.Since(began)

	if !errors.Is(a.err, coord.ErrUnavailable) || !strings.Contains(a.err.Error(), "did not land within") {
		t.Fatalf("Charge = (%+v, %v), want an unavailable store naming the contention", a.got, a.err)
	}
	if a.got.OK || a.got.RefusedScope != "" {
		t.Fatalf("Charge = %+v: contention reported as a decision about budget", a.got)
	}
	if took < patience {
		t.Errorf("the charge gave up after %v, before its %v patience ran out", took, patience)
	}
	if losing.lost.Load() < 2 {
		t.Errorf("the charge lost %d races, want it to have retried", losing.lost.Load())
	}
	store.budgets = losing.KeyValue
	if used := orgSpent(t, store); used != [3]int{10, 10, 10} {
		t.Errorf("org day/week/month = %v, want only the first round: a write that "+
			"never landed was counted", used)
	}
}

// A NODE NEVER RACES ITSELF.
//
// A node's concurrent writes to one counter are combined into one
// compare-and-swap at a time, so with no other node writing, not one of them
// can lose a race — however many there are. Racing each other, 32 of them
// through a leaf link on a busy machine failed in every run: the broker served
// on the order of N² requests to land N, and the slowest charges ran out of
// attempts having lost every race to their own node.
func TestANodeNeverRacesItself(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	counting := countRaces{KeyValue: store.budgets}
	store.budgets = &counting
	seat := coord.AgentScope("x")

	const callers = 64
	var (
		wg       sync.WaitGroup
		admitted atomic.Int32
	)
	errs := make(chan error, callers)
	for range callers {
		wg.Go(func() {
			got, err := store.Charge(t.Context(), coord.ChargeRequest{
				Seat: seat, Tokens: 10, Windows: testWindows(), OrgCaps: coord.Caps{period.Day: 200},
			})
			switch {
			case err != nil:
				errs <- err
			case got.OK:
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a concurrent charge failed: %v", err)
	}
	if lost := counting.lost.Load(); lost != 0 {
		t.Errorf("%d writes lost a race with nobody but this node writing: its own "+
			"writes raced each other at the broker", lost)
	}
	if got := admitted.Load(); got != 20 {
		t.Errorf("%d of %d charges of 10 admitted against a ceiling of 200, want exactly 20",
			got, callers)
	}
	for _, scope := range []string{coord.OrgScope, seat} {
		if used := spentBy(t, store, scope); used != [3]int{callers * 10, callers * 10, callers * 10} {
			t.Errorf("%s day/week/month = %v, want all %d rounds", scope, used, callers*10)
		}
	}
}

// WRITES QUEUED BEHIND ONE IN FLIGHT ARE SENT AS ONE, AND JUDGED IN ORDER.
//
// The combining is what keeps a node to one write in flight, and it must judge
// each charge exactly as if it had been sent alone after the ones before it: a
// ceiling admits what it holds and nothing more, and every round is counted.
// Held here at the broker so the queue is known before it is sent: one charge
// in flight, ten behind it, and the ten are one write.
func TestWritesQueuedBehindOneInFlightAreSentAsOne(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	charge(t, store, seat, nil)
	// A write held at the door is a write in flight, and the patience is
	// what bounds one: long, so the hold rather than the clock ends it.
	store.patience = time.Minute

	held := newHoldWrites(store.budgets, encodeKey(coord.OrgScope))
	store.budgets = held
	results := make(chan coord.Spend, 11)
	errs := make(chan error, 11)
	send := func() {
		got, err := store.Charge(t.Context(), coord.ChargeRequest{
			Seat: seat, Tokens: 10, Windows: testWindows(), OrgCaps: coord.Caps{period.Day: 60},
		})
		if err != nil {
			errs <- err
			return
		}
		results <- got
	}
	go send()
	held.waitHeld(t)
	for range 10 {
		go send()
	}
	waitQueued(t, store, coord.OrgScope, 10)
	writesBefore := held.writes.Load()
	close(held.release)

	admitted := 0
	for range 11 {
		select {
		case err := <-errs:
			t.Fatalf("a charge failed: %v", err)
		case got := <-results:
			if got.OK {
				admitted++
			}
		}
	}
	if sent := held.writes.Load() - writesBefore; sent != 1 {
		t.Errorf("the ten queued charges were %d writes to the company's counter, want one", sent)
	}
	// 10 already spent and a ceiling of 60: five of the eleven fit.
	if admitted != 5 {
		t.Errorf("%d of 11 charges admitted with room for 5, want 5: a combined write "+
			"judged its charges on a counter the ones before them had not moved", admitted)
	}
	if used := orgSpent(t, store); used != [3]int{120, 120, 120} {
		t.Errorf("org day/week/month = %v, want all twelve rounds", used)
	}
}

// A CALLER WHO HANGS UP WHILE ITS WRITE IS QUEUED IS TOLD SO, AT ONCE, AND
// NOTHING OF IT IS RECORDED.
//
// Queued, nothing of the write has landed, so it is withdrawn rather than sent
// for a caller no longer listening — and the caller is not kept waiting for a
// batch that is not its own.
func TestAWriteWithdrawnWhileQueuedIsNeverSent(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	charge(t, store, seat, nil)
	// A write held at the door is a write in flight, and the patience is
	// what bounds one: long, so the hold rather than the clock ends it.
	store.patience = time.Minute

	held := newHoldWrites(store.budgets, encodeKey(coord.OrgScope))
	store.budgets = held
	first := make(chan error, 1)
	go func() {
		_, err := store.Charge(t.Context(), coord.ChargeRequest{
			Seat: seat, Tokens: 10, Windows: testWindows(),
		})
		first <- err
	}()
	held.waitHeld(t)

	ctx, cancel := context.WithCancel(t.Context())
	queued := make(chan error, 1)
	go func() {
		_, err := store.Charge(ctx, coord.ChargeRequest{
			Seat: seat, Tokens: 1000, Windows: testWindows(),
		})
		queued <- err
	}()
	waitQueued(t, store, coord.OrgScope, 1)
	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("the hung-up charge answered %v, want the store unavailable for its "+
				"cancelled context", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the hung-up charge waited for the write in flight ahead of it")
	}

	close(held.release)
	if err := <-first; err != nil {
		t.Fatalf("the charge in flight failed: %v", err)
	}
	for _, scope := range []string{coord.OrgScope, seat} {
		if used := spentBy(t, store, scope); used != [3]int{20, 20, 20} {
			t.Errorf("%s day/week/month = %v, want the two rounds that were sent: the "+
				"withdrawn charge was recorded", scope, used)
		}
	}
}

// A CALLER WHO HANGS UP BETWEEN LOST RACES IS TOLD SO, AND NOTHING IS RECORDED.
//
// Every attempt before the hang-up lost, so nothing of the write has landed:
// it is withdrawn at the next attempt rather than raced on for the rest of the
// patience for a caller no longer listening — the one that sends a batch is
// often the caller itself, so nothing else would notice.
func TestAWriteWithdrawnBetweenLostRacesIsNeverRecorded(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	charge(t, store, seat, nil)
	store.patience = time.Minute
	losing := loseRaces{KeyValue: store.budgets, key: encodeKey(coord.OrgScope), forever: true}
	store.budgets = &losing

	ctx, cancel := context.WithCancel(t.Context())
	answered := make(chan error, 1)
	go func() {
		_, err := store.Charge(ctx, coord.ChargeRequest{Seat: seat, Tokens: 1000, Windows: testWindows()})
		answered <- err
	}()
	deadline := time.Now().Add(30 * time.Second)
	for losing.lost.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("the charge did not lose two races within 30s")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-answered:
		if !errors.Is(err, context.Canceled) || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("the hung-up charge answered %v, want the store unavailable for its "+
				"cancelled context", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the hung-up charge went on racing for a caller no longer listening")
	}
	store.budgets = losing.KeyValue
	if used := orgSpent(t, store); used != [3]int{10, 10, 10} {
		t.Errorf("org day/week/month = %v, want only the first round: the withdrawn "+
			"charge was recorded", used)
	}
}

// A CALLER WHO HANGS UP IN A LOSING BATCH IS TOLD SO AT ONCE, WHOEVER HOLDS
// THE TURN, AND THE OTHERS' WRITES STILL LAND.
//
// A batch is sent on its holder's goroutine, for every caller in it. A holder
// that hung up used to go on racing another node for the rest of its batch's
// patience before it could return — two seconds in a probe whose patience was
// two, a whole request timeout in production — because nothing let it hand
// the other writes on. Run both ways round, and the order is not left to the
// scheduler: the turn passes to the write queued FIRST, so queuing the
// caller that hangs up first makes it the holder, and queuing it second makes
// it a waiter in another caller's batch.
func TestACallerWhoHangsUpInALosingBatchIsAnsweredAtOnce(t *testing.T) {
	t.Parallel()
	for _, holder := range []string{"hangs-up", "listens"} {
		t.Run("the caller that "+holder+" holds the turn", func(t *testing.T) {
			t.Parallel()
			store := openFleet(t, embeddedNATS(t))
			seat := coord.AgentScope("x")
			charge(t, store, seat, nil)
			// Long, so the clock never answers anybody here: a holder
			// that kept racing for the listening caller would hold the
			// caller that hung up for a minute.
			store.patience = time.Minute
			staged := newStagedWrites(store.budgets, encodeKey(coord.OrgScope))
			staged.losing.Store(true)
			store.budgets = staged

			send := func(ctx context.Context, name string) chan error {
				answered := make(chan error, 1)
				go func() {
					_, err := store.Charge(context.WithValue(ctx, senderKey{}, name), coord.ChargeRequest{
						Seat: seat, Tokens: 10, Windows: testWindows(),
					})
					answered <- err
				}()
				return answered
			}
			ahead := send(t.Context(), "ahead")
			staged.waitHeld(t)
			hangUpCtx, hangUp := context.WithCancel(t.Context())
			var hungUp, listening chan error
			if holder == "hangs-up" {
				hungUp = send(hangUpCtx, "hangs-up")
				waitQueued(t, store, coord.OrgScope, 1)
				listening = send(t.Context(), "listens")
			} else {
				listening = send(t.Context(), "listens")
				waitQueued(t, store, coord.OrgScope, 1)
				hungUp = send(hangUpCtx, "hangs-up")
			}
			waitQueued(t, store, coord.OrgScope, 2)
			close(staged.release)
			if err := <-ahead; err != nil {
				t.Fatalf("the charge ahead of the batch failed: %v", err)
			}
			staged.waitLost(t, 2)
			if got := staged.firstLoser(); got != holder {
				t.Fatalf("the batch was sent by %q, want %q: the turn did not pass to the "+
					"write queued first", got, holder)
			}

			hangUp()
			select {
			case err := <-hungUp:
				if !errors.Is(err, context.Canceled) || !errors.Is(err, coord.ErrUnavailable) {
					t.Fatalf("the caller that hung up was answered %v, want the store "+
						"unavailable for its cancelled context", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the caller that hung up was still waiting 20s later: its batch went on " +
					"racing for the caller still listening")
			}
			staged.losing.Store(false)
			if err := <-listening; err != nil {
				t.Fatalf("the caller still listening was answered %v, want its round counted", err)
			}
			for _, scope := range []string{coord.OrgScope, seat} {
				if used := spentBy(t, store, scope); used != [3]int{30, 30, 30} {
					t.Errorf("%s day/week/month = %v, want the three rounds whose callers "+
						"listened, and not the one that hung up", scope, used)
				}
			}
		})
	}
}

// THE PATIENCE NEVER CUTS AN ATTEMPT IN FLIGHT.
//
// It decides whether to START an attempt. A compare-and-swap the broker
// applied but had not acknowledged when the patience ran out used to be cut
// there and answered "did not land" — while the company's counter held the
// round, and the seat's, never written, did not. Here the third attempt
// starts well inside the patience and lands, and its acknowledgement arrives
// after the patience has run out: the charge is the attempt's own answer.
func TestThePatienceNeverCutsAnAttemptInFlight(t *testing.T) {
	t.Parallel()
	store := openFleet(t, embeddedNATS(t))
	seat := coord.AgentScope("x")
	charge(t, store, seat, nil)
	const patience = time.Second
	store.patience = patience
	late := &lateAck{KeyValue: store.budgets, key: encodeKey(coord.OrgScope), losses: 2,
		ackAfter: 2 * patience}
	store.budgets = late

	got, err := store.Charge(t.Context(), coord.ChargeRequest{
		Seat: seat, Tokens: 10, Windows: testWindows(),
	})
	if err != nil || !got.OK {
		t.Fatalf("Charge = (%+v, %v), want the round admitted: the attempt that landed was "+
			"answered by the clock rather than by the broker", got, err)
	}
	if n := late.attempts.Load(); n != 3 {
		t.Fatalf("%d attempts reached the company's counter, want two lost and one landed", n)
	}
	store.budgets = late.KeyValue
	for _, scope := range []string{coord.OrgScope, seat} {
		if used := spentBy(t, store, scope); used != [3]int{20, 20, 20} {
			t.Errorf("%s day/week/month = %v, want both rounds", scope, used)
		}
	}
}

// THE LANE'S TURN PASSES TO THE WRITE QUEUED FIRST, AND IS NEVER STRANDED.
//
// Every unanswered write belongs to a caller that holds the turn or is waiting
// to be handed it, so whenever a write is queued somebody is going to send
// it. A turn handed to a caller that withdraws before taking it, or a batch's
// leftovers given back behind writes queued after them, would break that —
// the first leaves a queue nobody sends, the second sends writes out of the
// order they queued in. Neither can be staged reliably through Charge, where
// the scheduler picks between a caller's hang-up and its turn, so the lane is
// driven directly.
func TestTheLaneTurnPassesToTheWriteQueuedFirst(t *testing.T) {
	t.Parallel()
	write := func() *counterWrite {
		return &counterWrite{turn: make(chan struct{}), done: make(chan struct{})}
	}
	handed := func(w *counterWrite) bool {
		select {
		case <-w.turn:
			return true
		default:
			return false
		}
	}
	lane := &counterLane{}
	a, b, c, d, e := write(), write(), write(), write(), write()

	lane.enqueue(a)
	if !handed(a) {
		t.Fatal("a write queued on an idle lane was not handed the turn")
	}
	lane.enqueue(b)
	lane.enqueue(c)
	if handed(b) || handed(c) {
		t.Fatal("a write queued behind the turn's holder was handed the turn too")
	}

	// a's caller hangs up before it takes its turn.
	if !lane.withdraw(a) {
		t.Fatal("a queued write could not be withdrawn")
	}
	if !handed(b) || handed(c) {
		t.Fatal("the turn a withdrawn write had been handed did not pass to the next " +
			"write queued: the queue is stranded")
	}

	// b sends [b, c]; d queues meanwhile; b's write leaves the batch and c
	// goes back IN FRONT of d.
	if batch := lane.take(); len(batch) != 2 || batch[0] != b || batch[1] != c {
		t.Fatalf("the batch taken is %d writes, want b and c in the order they queued", len(batch))
	}
	lane.enqueue(d)
	if handed(d) {
		t.Fatal("a write queued while a batch was in flight was handed the turn")
	}
	lane.giveBack([]*counterWrite{c})
	if !handed(c) || handed(d) {
		t.Fatal("a write a batch gave back was not handed the turn ahead of one queued after it")
	}
	if batch := lane.take(); len(batch) != 2 || batch[0] != c || batch[1] != d {
		t.Fatal("the writes given back were not sent ahead of the writes queued after them")
	}

	// c's batch lands with nothing queued: the turn is let go, and the next
	// write finds the lane idle.
	lane.giveBack(nil)
	if lane.sending {
		t.Fatal("the turn was kept with nothing queued")
	}
	lane.enqueue(e)
	if !handed(e) {
		t.Fatal("a write queued after the turn was let go was not handed it")
	}
	if lane.withdraw(b) {
		t.Fatal("a write a batch already took was withdrawn from the queue")
	}
}

// CHARGES FROM MANY NODES ADD UP, AND A CEILING ADMITS EXACTLY WHAT IT HOLDS.
//
// Between nodes a write still races, and every race has to settle: four
// stores on one set of buckets, each the counter of a node of its own, each
// charging the company concurrently. The patience is long because what this
// case pins is the arithmetic of writes that race, not how long one may.
func TestChargesFromManyNodesAddUp(t *testing.T) {
	t.Parallel()
	client := jsOf(embeddedNATS(t))
	prefix := fmt.Sprintf("f%d", bucketSeq.Add(1))
	const nodes, perNode = 4, 16
	stores := make([]*FleetStore, nodes)
	counted := make([]*countRaces, nodes)
	for i := range stores {
		stores[i] = openFleetVia(t, client, prefix)
		stores[i].patience = time.Minute
		counted[i] = &countRaces{KeyValue: stores[i].budgets}
		stores[i].budgets = counted[i]
	}

	var (
		wg       sync.WaitGroup
		admitted atomic.Int32
	)
	errs := make(chan error, nodes*perNode)
	for i := range nodes * perNode {
		wg.Go(func() {
			got, err := stores[i%nodes].Charge(t.Context(), coord.ChargeRequest{
				Seat: coord.AgentScope(fmt.Sprintf("seat-%d", i%nodes)), Tokens: 10,
				Windows: testWindows(), OrgCaps: coord.Caps{period.Day: 300},
			})
			switch {
			case err != nil:
				errs <- err
			case got.OK:
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("a charge failed: %v", err)
	}
	if got := admitted.Load(); got != 30 {
		t.Errorf("%d of %d charges of 10 admitted against a ceiling of 300, want exactly 30",
			got, nodes*perNode)
	}
	if used := orgSpent(t, stores[0]); used != [3]int{nodes * perNode * 10, nodes * perNode * 10, nodes * perNode * 10} {
		t.Errorf("org day/week/month = %v, want all %d rounds", used, nodes*perNode*10)
	}
	for i := range nodes {
		seat := coord.AgentScope(fmt.Sprintf("seat-%d", i))
		if used := spentBy(t, stores[0], seat); used != [3]int{perNode * 10, perNode * 10, perNode * 10} {
			t.Errorf("%s day/week/month = %v, want its %d rounds", seat, used, perNode*10)
		}
	}
	var lost int64
	for _, c := range counted {
		lost += c.lost.Load()
	}
	t.Logf("%d charges from %d nodes lost %d races between them", nodes*perNode, nodes, lost)
}

// charge is one round of 10 on seat at testWindows, judged against orgCaps,
// failing the case on an error.
func charge(t *testing.T, store *FleetStore, seat string, orgCaps coord.Caps) coord.Spend {
	t.Helper()
	got, err := store.Charge(t.Context(), coord.ChargeRequest{
		Seat: seat, Tokens: 10, Windows: testWindows(), OrgCaps: orgCaps,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	return got
}

// waitQueued waits until n writes to scope's counter are queued on store's
// lane behind the one in flight.
func waitQueued(t *testing.T, store *FleetStore, scope string, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		store.lanesMu.Lock()
		lane := store.lanes[encodeKey(scope)]
		store.lanesMu.Unlock()
		depth := 0
		if lane != nil {
			lane.mu.Lock()
			depth = len(lane.queue)
			lane.mu.Unlock()
		}
		if depth >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d writes queued on %s's counter after 30s, want %d", depth, scope, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// loseRaces is a counter bucket on which writes to one key lose their race: each
// is refused as a write is when another node's landed first, and writes
// nothing. It loses the next `remaining`, or every one when forever is set.
type loseRaces struct {
	jetstream.KeyValue
	key       string
	forever   bool
	remaining atomic.Int64
	lost      atomic.Int64
}

func (k *loseRaces) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == k.key && (k.forever || k.remaining.Add(-1) >= 0) {
		k.lost.Add(1)
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// senderKey names the caller whose context a write was sent on: a batch is
// sent on its holder's, so the name on a write is who held the turn.
type senderKey struct{}

// stagedWrites is a counter bucket whose writes to one key are staged: the
// first waits at the door until release is closed and then lands, and every
// later one loses its race while losing is set. It records who sent each
// write that lost.
type stagedWrites struct {
	jetstream.KeyValue
	key     string
	held    chan struct{}
	release chan struct{}
	losing  atomic.Bool
	writes  atomic.Int64

	mu      sync.Mutex
	senders []string
}

func newStagedWrites(kv jetstream.KeyValue, key string) *stagedWrites {
	return &stagedWrites{KeyValue: kv, key: key, held: make(chan struct{}), release: make(chan struct{})}
}

func (k *stagedWrites) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key != k.key {
		return k.KeyValue.Update(ctx, key, value, revision)
	}
	if k.writes.Add(1) == 1 {
		close(k.held)
		<-k.release
		return k.KeyValue.Update(ctx, key, value, revision)
	}
	if k.losing.Load() {
		sender, _ := ctx.Value(senderKey{}).(string)
		k.mu.Lock()
		k.senders = append(k.senders, sender)
		k.mu.Unlock()
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// waitHeld waits until the first write is held at the door.
func (k *stagedWrites) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-k.held:
	case <-time.After(30 * time.Second):
		t.Fatal("no write reached the staged key within 30s")
	}
}

// waitLost waits until n writes have lost.
func (k *stagedWrites) waitLost(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		k.mu.Lock()
		lost := len(k.senders)
		k.mu.Unlock()
		if lost >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d writes lost within 30s, want %d", lost, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// firstLoser is who sent the first write that lost.
func (k *stagedWrites) firstLoser() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.senders) == 0 {
		return ""
	}
	return k.senders[0]
}

// lateAck is a counter bucket on which the first `losses` writes to one key
// lose their race, and the next lands but is acknowledged only `ackAfter`
// later — or fails with its context's error if that ends first, as the
// client's own request does when the broker has applied a write it has not yet
// answered.
type lateAck struct {
	jetstream.KeyValue
	key      string
	losses   int64
	ackAfter time.Duration
	attempts atomic.Int64
}

func (k *lateAck) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key != k.key {
		return k.KeyValue.Update(ctx, key, value, revision)
	}
	if k.attempts.Add(1) <= k.losses {
		return 0, jetstream.ErrKeyRevisionMismatch
	}
	landed, err := k.KeyValue.Update(context.WithoutCancel(ctx), key, value, revision)
	if err != nil {
		return 0, err
	}
	ack := time.NewTimer(k.ackAfter)
	defer ack.Stop()
	select {
	case <-ack.C:
		return landed, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// countRaces is a counter bucket that counts the writes that lost a race.
type countRaces struct {
	jetstream.KeyValue
	lost atomic.Int64
}

func (k *countRaces) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	rev, err := k.KeyValue.Update(ctx, key, value, revision)
	if errors.Is(err, jetstream.ErrKeyRevisionMismatch) || isWrongLastSequence(err) {
		k.lost.Add(1)
	}
	return rev, err
}

// holdWrites is a counter bucket whose writes to one key wait at the broker's
// door until release is closed, so a case can queue writes behind one in
// flight. It counts the writes that reach the door.
type holdWrites struct {
	jetstream.KeyValue
	key     string
	held    chan struct{}
	release chan struct{}
	writes  atomic.Int64
}

func newHoldWrites(kv jetstream.KeyValue, key string) *holdWrites {
	return &holdWrites{KeyValue: kv, key: key, held: make(chan struct{}, 1), release: make(chan struct{})}
}

func (k *holdWrites) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	if key == k.key {
		k.writes.Add(1)
		select {
		case k.held <- struct{}{}:
		default:
		}
		<-k.release
	}
	return k.KeyValue.Update(ctx, key, value, revision)
}

// waitHeld waits until a write is held at the door.
func (k *holdWrites) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-k.held:
	case <-time.After(30 * time.Second):
		t.Fatal("no write reached the held key within 30s")
	}
}
