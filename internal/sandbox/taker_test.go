package sandbox

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/seat"
)

// The processes the cases below are taken by: this node's own, and node-a's,
// which began the capture a case settles.
const (
	thisProcess   = "node-b:1"
	takingProcess = "node-a:1"
)

// takerStore is the coordination store a case's taker leases are held in: the
// in-process twin, whose clock a case moves to lapse a lease, wrapped so a case
// can make it unreachable and count what it was asked of each lease.
type takerStore struct {
	*memory.Backend
	mu sync.Mutex
	// down, when set, is every claim's, renewal's and read's answer: a store
	// that cannot be reached.
	down error
	// holding, when set, holds every renewal until its caller gives up, as a
	// store that takes a call and never answers it does; held counts them.
	holding bool
	held    int
	// stalling, when set, holds every read of a lease until its caller gives
	// up, as a store slow to answer does.
	stalling bool
	// unread, when set, is every read's answer while claims and renewals are
	// still answered: a store that cannot say of another process's lease
	// while this process's own renewals land.
	unread error
	// reads counts the reads of each lease.
	reads map[string]int
}

func newTakerStore() *takerStore {
	return &takerStore{Backend: memory.New(), reads: map[string]int{}}
}

func (s *takerStore) TryAcquire(ctx context.Context, resource string,
	opts coord.AcquireOptions) (*coord.Lease, coord.Refusal, error) {

	if err := s.unreachable(); err != nil {
		return nil, "", err
	}
	return s.Backend.TryAcquire(ctx, resource, opts)
}

func (s *takerStore) Renew(ctx context.Context, resource, owner string, epoch int64,
	ttl time.Duration) (bool, error) {

	if err := s.unreachable(); err != nil {
		return false, err
	}
	s.mu.Lock()
	holding := s.holding
	if holding {
		s.held++
	}
	s.mu.Unlock()
	if holding {
		<-ctx.Done()
		return false, errors.Join(coord.ErrUnavailable, ctx.Err())
	}
	return s.Backend.Renew(ctx, resource, owner, epoch, ttl)
}

func (s *takerStore) Get(ctx context.Context, resource string) (*coord.Lease, error) {
	s.mu.Lock()
	s.reads[resource]++
	down, stalling, unread := s.down, s.stalling, s.unread
	s.mu.Unlock()
	if down != nil {
		return nil, down
	}
	if unread != nil {
		return nil, errors.Join(coord.ErrUnavailable, unread)
	}
	if stalling {
		<-ctx.Done()
		return nil, errors.Join(coord.ErrUnavailable, ctx.Err())
	}
	return s.Backend.Get(ctx, resource)
}

func (s *takerStore) unreachable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.down
}

func (s *takerStore) setDown(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = err
}

// holdRenewals makes every renewal from here on wait for its caller to give up.
func (s *takerStore) holdRenewals() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holding = true
}

// failReads makes every read of a lease from here on fail with err, while
// claims and renewals are still answered.
func (s *takerStore) failReads(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unread = err
}

// stallReads makes every read of a lease from here on wait for its caller to
// give up.
func (s *takerStore) stallReads() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stalling = true
}

// stallingSettles is a pending-run store whose every settle of a begun stamp
// ([PendingStore.SettleBegun]) waits for its caller to give up: a store that
// answers its reads and is slow to take a write.
type stallingSettles struct{ PendingStore }

func (stallingSettles) SettleBegun(ctx context.Context, _ string, _ SessionCapture) (bool, error) {
	<-ctx.Done()
	return false, errors.Join(coord.ErrUnavailable, ctx.Err())
}

// renewalsHeld is how many renewals the store has held.
func (s *takerStore) renewalsHeld() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.held
}

// readsOf is how often incarnation's taker lease was read.
func (s *takerStore) readsOf(incarnation string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads[coord.TakerResource(incarnation)]
}

// readsAll is how often any lease was read.
func (s *takerStore) readsAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, n := range s.reads {
		total += n
	}
	return total
}

// taking is a coordinator's options holding its taker lease as incarnation in
// store: set on the options [NewCoordinator] is handed ([newCoordRigWith]), so
// a case holds the constructor's copy of each too.
func taking(incarnation string, store TakerLeases) func(*CoordinatorOptions) {
	return func(o *CoordinatorOptions) {
		o.Incarnation, o.Takers = incarnation, store
	}
}

// claimTaker claims incarnation's taker lease in store, as that process's
// coordinator claims it as it is built.
func claimTaker(t *testing.T, store *takerStore, incarnation string) *coord.Lease {
	t.Helper()
	lease, refused, err := store.Backend.TryAcquire(t.Context(), coord.TakerResource(incarnation),
		coord.AcquireOptions{Owner: incarnation, TTL: seat.SeatLeaseTTL, Ungated: true})
	if err != nil || lease == nil {
		t.Fatalf("claim %s's taker lease = (%v, %q, %v)", incarnation, lease, refused, err)
	}
	return lease
}

// heldBy is incarnation's taker lease as store holds it now, nil for none —
// read past the count a case keeps of the coordinator's reads.
func heldBy(t *testing.T, store *takerStore, incarnation string) *coord.Lease {
	t.Helper()
	lease, err := store.Backend.Get(t.Context(), coord.TakerResource(incarnation))
	if err != nil {
		t.Fatalf("read %s's taker lease: %v", incarnation, err)
	}
	return lease
}

// lapse moves store's clock past every lease it holds, as a TTL nobody renewed
// across does.
func lapse(store *takerStore) { store.Advance(2 * seat.SeatLeaseTTL) }

// takenBy stamps on t1's row a capture of its job at the place after kept,
// begun by taker under its lease's epoch at the rig's now and naming no object
// yet: a take whose process stopped before it stamped what came of it, or is
// still keeping it.
func takenBy(t *testing.T, rig *coordRig, kept SessionCapture, taker string, epoch int64) SessionCapture {
	t.Helper()
	begun := kept
	begun.CaptureSeq, begun.State, begun.At = kept.CaptureSeq+1, CaptureBegun, rig.coordinator.now()
	begun.Taker, begun.TakerEpoch = taker, epoch
	begun.Object, begun.ObjectHash, begun.ObjectSize, begun.ContentKey, begun.ContextTokens = "", "", 0, "", 0
	if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
		t.Fatalf("stamping the begun capture = %v, %v", took, err)
	}
	return begun
}

// landedRow is the turn's row on the executor log once begun's record landed:
// it names begun's place, an object of its own, the key that sealed it and what
// the session's last request carried — what only the keep that recorded it
// knew.
func landedRow(t *testing.T, begun SessionCapture) executorkeep.TurnRow {
	t.Helper()
	landed := begun
	landed.Object, landed.ObjectHash, landed.ObjectSize = objstore.KeyAt(begun.At.Add(time.Second)).String(),
		"h2", 2048
	row := keptRow(t, landed)
	row.ContentKey, row.ContextTokens = "k2", 42_000
	return row
}

// onTheLog makes row the turn's row on the executor log, as a record that
// lands makes it.
func onTheLog(restore *keptRestore, row executorkeep.TurnRow) {
	restore.mu.Lock()
	defer restore.mu.Unlock()
	restore.rows[row.Turn] = row
}

// A CAPTURE WHOSE TAKER'S LEASE LAPSED IS SETTLED FROM THE TURN'S ROW. Every
// stamp of a take names the taker lease it was taken under, and a take dies with
// its process, leaving its place begun. Once that lease has lapsed or been given
// back, the turn's row on the executor log, which has seen every record the
// taker published, decides: LANDED where the row names its place (stamp written
// from the row and kept as the turn's); REFUSED where it does not, saying its
// taker stopped, the turn's kept capture left as it was.
//
// Mutation: drop the taker's epoch from a take's stamp, and the premise fails;
// read a lapsed or released lease as held, and the capture is left begun; stamp
// it refused whatever the row names, and a landed capture is read as lost; leave
// the row's object off the landed stamp, and the turn keeps one naming none.
func TestACaptureWhoseTakersLeaseLapsedIsSettledFromTheTurnsRow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name     string
		released bool // the taker gave its lease back at a clean exit
		landed   bool // the begun capture's record is on the log
	}{
		{name: "its lease lapsed, and its record landed", landed: true},
		{name: "its lease lapsed, and its record never landed"},
		{name: "its lease was given back, and its record never landed", released: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
				if own := heldBy(t, store, thisProcess); own == nil || kept.Taker != thisProcess ||
					kept.TakerEpoch != own.Epoch {
					t.Fatalf("the premise: the collection's capture names %s at %d, want this process's own "+
						"taker lease, %+v", kept.Taker, kept.TakerEpoch, own)
				}
				lease := claimTaker(t, store, takingProcess)
				begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
				var row executorkeep.TurnRow
				if c.landed {
					row = landedRow(t, begun)
					onTheLog(restore, row)
				}
				if c.released {
					if ok, err := store.Release(t.Context(), coord.TakerResource(takingProcess), takingProcess,
						lease.Epoch); err != nil || !ok {
						t.Fatalf("give node-a's taker lease back = %v, %v", ok, err)
					}
				} else {
					lapse(store)
				}
				settled, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false)
				if !found {
					t.Fatal("the run's row was read as gone")
				}
				got := rig.get("t1")
				if got.Capture == nil || !got.Capture.sameTake(begun) || settled.Capture == nil ||
					settled.Capture.State != got.Capture.State {
					t.Fatalf("the row holds %+v (answered %+v), want the begun take settled", got.Capture,
						settled.Capture)
				}
				if !c.landed {
					if got.Capture.State != CaptureRefused ||
						!strings.Contains(got.Capture.Reason, "its taker node-a stopped before it was kept") {
						t.Errorf("the capture stands %s (%q), want it refused for its stopped taker",
							got.Capture.State, got.Capture.Reason)
					}
					if got.Kept == nil || !got.Kept.samePlace(kept) {
						t.Errorf("the row keeps %+v, want the turn's kept capture left as it was", got.Kept)
					}
					return
				}
				if got.Capture.State != CaptureLanded || got.Capture.Object != row.Object.Key.String() ||
					got.Capture.ObjectSize != row.Object.Size || got.Capture.ContentKey != "k2" ||
					got.Capture.ContextTokens != 42_000 || got.Capture.Taker != takingProcess ||
					got.Capture.TakerEpoch != lease.Epoch {
					t.Errorf("the capture stands %+v, want it landed as the log's row names it, its taker kept",
						got.Capture)
				}
				if got.Kept == nil || !got.Kept.samePlace(begun) || got.Kept.State != CaptureLanded {
					t.Errorf("the row keeps %+v, want the landed capture kept as the turn's", got.Kept)
				}
			})
		})
	}
}

// AN END DOES NOT WAIT OUT A CAPTURE WHOSE TAKER'S LEASE LAPSED. The turn's end
// waits for its captures to settle, since its tombstone refuses a later record;
// a capture whose process stopped before stamping its outcome never settles by
// itself, and was waited for over an hour with the seat's mail held behind it.
// Its taker's lease has lapsed, so the end's wait settles it from the turn's row
// at its first pass, refused because its taker stopped. IN A SYNCTEST BUBBLE,
// so the wait runs on the bubble's clock.
//
// Mutation: drop the settle from the end's wait ([Coordinator.awaitCapture]),
// or the lease store from the coordinator [NewCoordinator] builds, and the end
// is recorded a capture's life on, naming it begun and never finished.
func TestAnEndDoesNotWaitOutACaptureWhoseTakersLeaseLapsed(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
		rig.coordinator.captureTick = time.Second
		run := rig.launchKept(t, "t1")
		// THE JOB'S COLLECTION, TAKEN BY node-a's PROCESS under its taker
		// lease, which lapsed before it stamped what came of it: a take of
		// the ended job, so the loss's ending reads its box no more.
		lease := claimTaker(t, store, takingProcess)
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
			t.Fatalf("stamping the begun capture = %v, %v", took, err)
		}
		lapse(store)
		// THE TURN'S ROW ON THE LOG, its begin's: it names no capture.
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{
			"t1": {Handle: "swe", Turn: "t1"}}}
		started := time.Now()
		rig.coordinator.finish(t.Context(), rig.get("t1"), ending{fence: fenceOf(run),
			note: &failureNote{reason: string(LossBoxGone), detail: "the box is gone from its host"}})
		// THE ENDING'S OWN RETRY, off the frame that decided it, which waits
		// for the turn's captures and records the end.
		for range 3 {
			if rig.fireRetries() == 0 {
				break
			}
		}
		waited := time.Since(started)

		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the lost turn's one", len(ends))
		}
		unkept := ends[0].Ending.Unkept
		if unkept == nil || unkept.LaunchSeq != run.LaunchSeq || unkept.CaptureSeq != 1 ||
			!strings.Contains(unkept.Reason, "its taker node-a stopped before it was kept") {
			t.Errorf("the end names %+v unkept, want the capture refused for its stopped taker", unkept)
		}
		if waited >= time.Minute {
			t.Errorf("the end waited %s for a capture whose taker's lease had lapsed, want one read of the log",
				waited)
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Error("the lost run's record was left behind its end")
		}
	})
}

// A DRAINING TAKER'S CAPTURE IS NOT SETTLED UNDER IT. A drain gives presence up
// at once while its keeps upload, but holds the taker lease until the last
// ends, so a turn's end reads the lease held and waits (one lease read a pass,
// the log not read before a record's budget) until the keep lands, then ends
// with that capture kept. Settled on missing presence, the end would refuse
// the capture and tombstone the turn ahead of its record. In a synctest
// bubble, the keep landing once the wait has asked three times.
//
// Mutation: read a taker with no presence as gone, or a lease held at the
// stamp's epoch as lapsed, and the end names the capture refused.
func TestADrainingTakersCaptureIsNotSettledUnderIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ctx := t.Context()
		store := newTakerStore()
		rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
		rig.coordinator.captureTick = time.Second
		run := rig.launchKept(t, "t1")
		// node-a's PROCESS, with its presence and its taker lease, begins the
		// job's collection, and begins to drain: its presence is given up at
		// once, its taker lease held while its keep uploads.
		presence, _, err := store.Backend.TryAcquire(ctx, coord.NodeResource("node-a"),
			coord.AcquireOptions{Owner: takingProcess, TTL: seat.SeatLeaseTTL, Ungated: true})
		if err != nil || presence == nil {
			t.Fatalf("claim node-a's presence: %v", err)
		}
		lease := claimTaker(t, store, takingProcess)
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(ctx, "t1", begun); err != nil || !took {
			t.Fatalf("stamping the begun capture = %v, %v", took, err)
		}
		if ok, err := store.Release(ctx, coord.NodeResource("node-a"), takingProcess, presence.Epoch); err != nil ||
			!ok {
			t.Fatalf("give node-a's presence up = %v, %v", ok, err)
		}
		restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": {Handle: "swe", Turn: "t1"}}}
		rig.coordinator.restore = restore
		landed := begun
		landed.State = CaptureLanded
		landed.Object, landed.ObjectHash, landed.ObjectSize = objstore.KeyAt(begun.At).String(), "h1", 1024
		row := keptRow(t, landed)
		go func() {
			for store.readsOf(takingProcess) < 3 {
				time.Sleep(time.Millisecond)
			}
			onTheLog(restore, row)
			_, _ = rig.pending.StampCapture(ctx, "t1", landed)
		}()

		rig.coordinator.finish(ctx, rig.get("t1"), ending{fence: fenceOf(run),
			note: &failureNote{reason: string(LossBoxGone), detail: "the box is gone from its host"}})
		for range 3 {
			if rig.fireRetries() == 0 {
				break
			}
		}
		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the turn's one", len(ends))
		}
		if unkept := ends[0].Ending.Unkept; unkept != nil {
			t.Errorf("the end names %+v unkept, want the draining node's capture kept as it landed", unkept)
		}
		if reads := store.readsOf(takingProcess); reads < 3 {
			t.Errorf("the end's wait read node-a's taker lease %d times, want it asked at every pass", reads)
		}
		if read := restore.askedFor(); len(read) != 0 {
			t.Errorf("the turn's row was read %v before a record's budget of the wait had passed, for a capture "+
				"whose taker still held its lease", read)
		}
	})
}

// A TAKER LEASE HELD AT ANOTHER EPOCH IS GONE. A taker whose lease lapsed and
// that claimed it again holds it under a new token: every keep it had under the
// old one is fenced, so a capture stamped under that one will never be kept,
// and is settled from the turn's row as any other gone taker's is. A capture
// stamped under the token the lease holds now is not.
//
// Mutation: compare the lease's holder and never its epoch, and the lapsed
// tenure's capture is left begun under the new one.
func TestATakerLeaseHeldAtAnotherEpochIsGone(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
		first := claimTaker(t, store, takingProcess)
		begun := takenBy(t, rig, kept, takingProcess, first.Epoch)
		lapse(store)
		again := claimTaker(t, store, takingProcess)
		if again.Epoch <= first.Epoch {
			t.Fatalf("the premise: node-a's taker claimed again at %d, want above its lapsed %d", again.Epoch,
				first.Epoch)
		}
		current := begun
		current.TakerEpoch = again.Epoch
		if gone, err := rig.coordinator.takerGone(t.Context(), current); err != nil || gone {
			t.Errorf("a capture under the epoch node-a's lease holds now reads gone = %v, %v; want it kept for", gone,
				err)
		}
		if _, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false); !found {
			t.Fatal("the run's row was read as gone")
		}
		got := rig.get("t1")
		if got.Capture == nil || !got.Capture.sameTake(begun) || got.Capture.State != CaptureRefused ||
			!strings.Contains(got.Capture.Reason, "its taker node-a stopped before it was kept") {
			t.Errorf("the row holds %+v, want the lapsed tenure's capture refused for its stopped taker", got.Capture)
		}
		if read := restore.askedFor(); len(read) != 1 {
			t.Errorf("the turn's row was read %v, want once to settle the capture", read)
		}
	})
}

// A STORE THAT CANNOT ANSWER NEVER READS A TAKER AS GONE. Whether a taker's
// lease still holds is a three-valued answer: a store that cannot be reached
// says nothing of it, so the capture is left begun, waited for as before, and
// the turn's row is not read for it; once the store answers that the lease has
// lapsed, the capture is settled. Where the wait reads the row for a taker
// that is not gone (readHeld), it reads it for one the store cannot say of as
// for one that holds, and settles LANDED only: a row naming another place
// leaves the capture begun, never refused.
//
// Mutation: read a store's error as a lease gone, and the capture is refused
// while its taker may still be keeping it; admit a lease the store cannot say
// of to the read for a held taker as a taker gone, and it is refused there.
func TestAStoreThatCannotAnswerNeverReadsATakerAsGone(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
		lease := claimTaker(t, store, takingProcess)
		begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
		lapse(store)
		store.setDown(errors.New("coord: the lease store did not answer"))
		if _, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false); !found {
			t.Fatal("the run's row was read as gone")
		}
		if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
			t.Fatalf("the row holds %+v while the store could not answer, want the capture left begun", got)
		}
		if read := restore.askedFor(); len(read) != 0 {
			t.Fatalf("the turn's row was read %v while the store could not say whether the taker was gone", read)
		}
		if _, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, true); !found {
			t.Fatal("the run's row was read as gone")
		}
		if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
			t.Fatalf("the row holds %+v once the row naming another place was read for a lease the store "+
				"could not say of, want the capture left begun", got)
		}
		if read := restore.askedFor(); len(read) != 1 {
			t.Fatalf("the turn's row was read %v, want once, for a lease the store could not say of read as "+
				"one that holds", read)
		}
		store.setDown(nil)
		if _, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false); !found {
			t.Fatal("the run's row was read as gone")
		}
		if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureRefused {
			t.Errorf("the row holds %+v once the store answered the lease lapsed, want the capture refused", got)
		}
		if reads := store.readsOf(takingProcess); reads != 3 {
			t.Errorf("node-a's taker lease was read %d times, want once a pass", reads)
		}
	})
}

// A GONE TAKER'S CAPTURE IS SETTLED ONLY BY AN ANSWER FROM THE LOG. A capture
// refused is refused for good, so it is settled only where the executor log
// answered for the turn's row. A log that could not be read is no answer: the
// capture is left begun, and the wait's next pass reads the row again — one
// whose record had landed is then kept. A log that holds no row for the turn
// at all — its begin never landed, and nothing of it was recorded since — has
// seen nothing of the capture, which is refused.
//
// Mutation: read a log that could not be read as one without the turn's row,
// and a capture whose record landed is refused on one bad read; read a log
// without the turn's row as one that could not be read, and that capture is
// left begun for as long as a capture can take.
func TestAGoneTakersCaptureIsSettledOnlyByAnAnswerFromTheLog(t *testing.T) {
	t.Parallel()
	t.Run("the log could not be read", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			store := newTakerStore()
			rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
			lease := claimTaker(t, store, takingProcess)
			begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
			onTheLog(restore, landedRow(t, begun))
			lapse(store)
			restore.mu.Lock()
			restore.unreadRows = errors.New("estate: no data node answered the read")
			restore.mu.Unlock()
			settled, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false)
			if !found || settled.Capture == nil || !settled.Capture.sameTake(begun) ||
				settled.Capture.State != CaptureBegun {
				t.Fatalf("a read that failed answered %+v (found %v), want the capture left begun", settled.Capture,
					found)
			}
			if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
				t.Fatalf("the row holds %+v after a read that failed, want the capture left begun", got)
			}
			// THE NEXT PASS, once the log answers.
			restore.mu.Lock()
			restore.unreadRows = nil
			restore.mu.Unlock()
			if _, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false); !found {
				t.Fatal("the run's row was read as gone")
			}
			if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureLanded {
				t.Errorf("the row holds %+v once the log answered, want the capture its row names landed", got)
			}
			if read := restore.askedFor(); len(read) != 2 {
				t.Errorf("the log was read %v, want once at each pass", read)
			}
		})
	})
	t.Run("the log holds no row for the turn", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			store := newTakerStore()
			rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
			run := rig.launchKept(t, "t1")
			lease := claimTaker(t, store, takingProcess)
			begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
				State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
				TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
			if took, err := rig.pending.StampCapture(t.Context(), "t1", begun); err != nil || !took {
				t.Fatalf("stamping the begun capture = %v, %v", took, err)
			}
			lapse(store)
			restore := &keptRestore{rows: map[string]executorkeep.TurnRow{}, noRowFound: true}
			rig.coordinator.restore = restore
			if _, found, _ := rig.coordinator.settleGone(t.Context(), rig.get("t1"), askEvery, false); !found {
				t.Fatal("the run's row was read as gone")
			}
			got := rig.get("t1").Capture
			if got == nil || !got.sameTake(begun) || got.State != CaptureRefused ||
				!strings.Contains(got.Reason, "its taker node-a stopped before it was kept") {
				t.Errorf("the row holds %+v, want the capture refused for its stopped taker", got)
			}
			if read := restore.askedFor(); len(read) != 1 {
				t.Errorf("the log was read %v, want once", read)
			}
		})
	})
}

// relaunchAfterTaker relaunches the job of a kept turn whose newest capture
// another process took and began, its taker lease lapsed where gone and the
// capture's record on the log where landed, the relaunch waiting at most wait
// for it: the rig, the begun capture, and what the relaunch continues and why
// it continues none.
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
func relaunchAfterTaker(t *testing.T, gone, landed bool,
	wait time.Duration) (*coordRig, SessionCapture, ContinuedSession, string) {

	t.Helper()
	store := newTakerStore()
	rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
	rig.coordinator.relaunchWait = wait
	lease := claimTaker(t, store, takingProcess)
	begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
	if landed {
		onTheLog(restore, landedRow(t, begun))
	}
	if gone {
		lapse(store)
	}
	continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
	return rig, begun, continued, why
}

// A RELAUNCH SETTLES A CAPTURE WHOSE TAKER IS GONE BEFORE IT DECIDES, from the
// turn's row: landed where the row names it, and continued; refused where not,
// the relaunch continuing the job's older kept capture. A taker whose lease
// holds is waited for, and the row is read again once the wait runs out: one it
// names landed and is continued, one it does not is left begun.
//
// Mutation: settle landed only a gone taker's, or drop the read past the wait,
// and the third case continues the older capture; settle a held taker's
// refused, and the fourth's stamp is not left begun; continue only the newest,
// and the second and fourth continue nothing.
func TestARelaunchSettlesACaptureWhoseTakerIsGoneBeforeItDecides(t *testing.T) {
	t.Parallel()
	t.Run("its record landed", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, begun, continued, why := relaunchAfterTaker(t, true, true, 2*time.Second)
			if why != "" || !continued.Capture.samePlace(begun) || continued.Capture.State != CaptureLanded ||
				continued.CopyContext != 42_000 {
				t.Fatalf("the relaunch continues %+v (%q), want the capture the stopped process landed, of "+
					"42000 tokens", continued, why)
			}
			if kept := rig.get("t1").Kept; kept == nil || !kept.samePlace(begun) {
				t.Errorf("the row keeps %+v, want the capture the relaunch continued", kept)
			}
		})
	})
	t.Run("its record never landed", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, begun, continued, why := relaunchAfterTaker(t, true, false, 2*time.Second)
			if why != "" || continued.Capture.CaptureSeq != begun.CaptureSeq-1 {
				t.Fatalf("the relaunch continues %+v (%q), want the capture its job kept before the one its "+
					"stopped taker never kept", continued, why)
			}
			if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureRefused {
				t.Errorf("the row holds %+v, want the begun capture refused", c)
			}
		})
	})
	t.Run("its taker's lease still holds, and its record landed", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, begun, continued, why := relaunchAfterTaker(t, false, true, 50*time.Millisecond)
			if why != "" || !continued.Capture.samePlace(begun) || continued.Capture.State != CaptureLanded {
				t.Fatalf("the relaunch continues %+v (%q), want the capture the log took, settled landed past "+
					"the wait though its taker still holds its lease", continued, why)
			}
			if kept := rig.get("t1").Kept; kept == nil || !kept.samePlace(begun) || kept.State != CaptureLanded {
				t.Errorf("the row keeps %+v, want the capture the relaunch continued", kept)
			}
		})
	})
	t.Run("its taker's lease still holds, and its record is not on the log", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, begun, continued, why := relaunchAfterTaker(t, false, false, 50*time.Millisecond)
			if why != "" || continued.Capture.CaptureSeq != begun.CaptureSeq-1 {
				t.Fatalf("the relaunch continues %+v (%q), want the capture its job kept before the one a taker "+
					"still holding its lease is keeping", continued, why)
			}
			if c := rig.get("t1").Capture; c == nil || !c.sameTake(begun) || c.State != CaptureBegun {
				t.Errorf("the row holds %+v, want the live taker's capture left begun", c)
			}
		})
	})
}

// A CAPTURE THE LOG TOOK IS SETTLED LANDED WHATEVER ITS TAKER'S LEASE (the
// package doc says why): the relaunch reads the turn's row once past its wait,
// whatever the capture's age, and the end at most once a record's budget; a
// save beside a refused final take as much as the newest; a lease the store
// cannot say of as one that holds; the read past the wait held to a wait of its
// own and its settle to its own room. In a synctest bubble.
//
// Mutation: settle landed only a gone taker's; read the row every pass; read it
// past the wait only within the budget; settle the newest alone; read past the
// wait under the relaunch's context, or settle in no room of its own; or read
// only a lease answering held, and a case goes red.
func TestACaptureTheLogTookIsSettledLandedWhateverItsTakersLease(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// aged moves the coordinator's clock a capture's budget past the
		// capture's begin: no longer in flight, its taker's lease still held.
		aged bool
		// unread makes every read of a taker lease fail: a store that cannot
		// say of node-a's lease.
		unread bool
	}{
		{name: "a relaunch of the job lost with that node"},
		{name: "a relaunch of the job lost with that node, its capture begun longer ago than a capture can take",
			aged: true},
		{name: "a relaunch of the job lost with that node, the store unable to say of its taker's lease",
			unread: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
				rig.coordinator.relaunchWait = 2 * time.Second
				lease := claimTaker(t, store, takingProcess)
				begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
				row := landedRow(t, begun)
				onTheLog(restore, row)
				if c.aged {
					rig.now = rig.now.Add(SessionCaptureBudget)
				}
				if c.unread {
					store.failReads(errors.New("coord: the lease store could not say"))
				}
				started := time.Now()
				continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
				waited := time.Since(started)
				if held := heldBy(t, store, takingProcess); held == nil || held.Epoch != lease.Epoch {
					t.Fatalf("the premise: node-a's taker lease is %+v, want it still held at %d", held,
						lease.Epoch)
				}
				if why != "" || !continued.Capture.samePlace(begun) || continued.Capture.State != CaptureLanded ||
					continued.Capture.Object != row.Object.Key.String() || continued.Capture.ContextTokens != 42_000 ||
					continued.CopyContext != 42_000 {
					t.Fatalf("the relaunch continues %+v (%q), want the capture the log took, landed as the "+
						"turn's row names it, of 42000 tokens", continued, why)
				}
				got := rig.get("t1")
				if got.Kept == nil || !got.Kept.samePlace(begun) || got.Kept.State != CaptureLanded ||
					got.Kept.Taker != takingProcess || got.Kept.TakerEpoch != lease.Epoch {
					t.Errorf("the row keeps %+v, want the capture settled landed, its taker kept", got.Kept)
				}
				switch {
				case !c.aged && waited < rig.coordinator.relaunchWait:
					t.Errorf("the relaunch decided %s after it asked, want its whole wait of %s for a keep whose "+
						"taker still held its lease", waited, rig.coordinator.relaunchWait)
				case c.aged && waited > 0:
					t.Errorf("the relaunch decided %s after it asked, want it at once: a capture past its "+
						"budget is no keep it waits for", waited)
				}
				if read := restore.askedFor(); len(read) != 2 {
					t.Errorf("the turn's row was read %v, want once past the wait to settle the capture and "+
						"once to decide", read)
				}
			})
		})
	}
	t.Run("a relaunch of the job lost with that node, its save overtaken by a refused final take",
		func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, _, _ := keepingRigWith(t, taking(thisProcess, store))
				rig.runner.Carries = true
				rig.coordinator.relaunchWait = 2 * time.Second
				rig.launchKept(t, "t1")
				lease := claimTaker(t, store, takingProcess)
				save, _ := overtakenSave(t, rig, "t1", takingProcess, lease.Epoch, CaptureRefused)
				restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": landedRow(t, save)}}
				rig.coordinator.restore = restore
				continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
				if held := heldBy(t, store, takingProcess); held == nil || held.Epoch != lease.Epoch {
					t.Fatalf("the premise: node-a's taker lease is %+v, want it still held at %d", held,
						lease.Epoch)
				}
				if why != "" || !continued.Capture.sameTake(save) || continued.Capture.State != CaptureLanded {
					t.Fatalf("the relaunch continues %+v (%q), want the save the log took, settled landed past "+
						"the refused final take", continued, why)
				}
				if kept := rig.get("t1").Kept; kept == nil || !kept.sameTake(save) || kept.State != CaptureLanded {
					t.Errorf("the row keeps %+v, want the save settled landed", kept)
				}
				if read := restore.askedFor(); len(read) != 2 {
					t.Errorf("the turn's row was read %v, want once past the wait to settle the save and once "+
						"to decide", read)
				}
			})
		})
	for _, c := range []struct {
		name string
		// slow makes the store slow to answer what the read past the wait
		// asks of it.
		slow func(rig *coordRig, store *takerStore)
		// write is whether what the store is slow to take is the settle's
		// write, which has its own room past the read's wait
		// ([Coordinator.settleContext]).
		write bool
	}{
		{name: "a relaunch's read past its wait, beside a store slow to answer the taker's lease",
			slow: func(_ *coordRig, store *takerStore) { store.stallReads() }},
		{name: "a relaunch's read past its wait, beside a store slow to take the settle's write",
			slow: func(rig *coordRig, _ *takerStore) {
				rig.coordinator.pending = stallingSettles{PendingStore: rig.coordinator.pending}
			}, write: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
				// A RECORD'S BUDGET OF TEN MINUTES, the room a settle's write
				// is given past the read's wait, short of the relaunch's hour.
				rig.coordinator.relaunchWait, rig.coordinator.recordBudget = 2*time.Second, 10*time.Minute
				lease := claimTaker(t, store, takingProcess)
				begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
				onTheLog(restore, landedRow(t, begun))
				c.slow(rig, store)
				// AN HOUR FOR THE WHOLE RELAUNCH, which a read under its own
				// context would be held to.
				ctx, cancel := context.WithTimeout(t.Context(), time.Hour)
				defer cancel()
				started := time.Now()
				_, why := rig.coordinator.ContinuedCapture(ctx, "t1", RestorerUnder(rig.runner, ""))
				waited := time.Since(started)
				bound := 2 * rig.coordinator.relaunchWait
				if c.write {
					bound += RowBudget(rig.coordinator.pending, rig.coordinator.recordBudget)
				}
				if waited > bound {
					t.Errorf("the relaunch decided %s after it asked (%q), want it within its wait and a wait as "+
						"long again for its read past it, and the settle's own room where it writes, %s", waited,
						why, bound)
				}
				if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
					t.Errorf("the row holds %+v, want the capture left begun by a read that was not answered", got)
				}
			})
		})
	}
	for _, c := range []struct {
		name string
		// unread makes every read of a taker lease fail while node-a's
		// renewals land: a store that cannot say of its lease.
		unread bool
	}{
		{name: "the turn's end"},
		{name: "the turn's end, the store unable to say of its taker's lease", unread: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			endBesideACaptureTheLogTook(t, c.unread)
		})
	}
	t.Run("the turn's end, beside a save overtaken by a refused final take", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			ctx := t.Context()
			store := newTakerStore()
			rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
			rig.coordinator.captureTick = time.Second
			run := rig.launchKept(t, "t1")
			lease := claimTaker(t, store, takingProcess)
			save, final := overtakenSave(t, rig, "t1", takingProcess, lease.Epoch, CaptureRefused)
			restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": landedRow(t, save)}}
			rig.coordinator.restore = restore
			// node-a's PROCESS RENEWS ITS TAKER LEASE as a live process does:
			// the lease holds through every pass of the end's wait.
			stop, renewing := make(chan struct{}), make(chan struct{})
			go func() {
				defer close(renewing)
				for {
					select {
					case <-stop:
						return
					case <-time.After(seat.SeatLeaseTTL / coord.KeptRenewals):
					}
					if ok, err := store.Backend.Renew(ctx, coord.TakerResource(takingProcess), takingProcess,
						lease.Epoch, seat.SeatLeaseTTL); err != nil || !ok {
						t.Errorf("renewing node-a's taker lease = %v, %v", ok, err)
						return
					}
				}
			}()

			started := time.Now()
			rig.coordinator.finish(ctx, rig.get("t1"), ending{fence: fenceOf(run),
				note: &failureNote{reason: string(LossLaunchNodeStoreSilent),
					detail: "the node it ran on could not reach the coordination store for the host grace"}})
			for range 3 {
				if rig.fireRetries() == 0 {
					break
				}
			}
			waited := time.Since(started)
			held := heldBy(t, store, takingProcess)
			close(stop)
			<-renewing
			if held == nil || held.Epoch != lease.Epoch {
				t.Fatalf("the premise: node-a's taker lease is %+v, want it still held at %d", held, lease.Epoch)
			}

			ends := writes.endedRecords()
			if len(ends) != 1 {
				t.Fatalf("recorded %d ends, want the turn's one", len(ends))
			}
			if u := ends[0].Ending.Unkept; u == nil || u.LaunchSeq != final.LaunchSeq ||
				u.CaptureSeq != final.CaptureSeq || !ends[0].Ending.WentOn {
				t.Errorf("the end names %+v unkept, the turn gone on %v; want the refused final take unkept and "+
					"the turn gone on past the save the log took", u, ends[0].Ending.WentOn)
			}
			if waited >= SessionCaptureBudget {
				t.Errorf("the end waited %s, want it recorded once the log named the save, not by its age",
					waited)
			}
			if read := restore.askedFor(); len(read) != 1 {
				t.Errorf("the turn's row was read %v, want once, finding the save", read)
			}
		})
	})
}

// endBesideACaptureTheLogTook is the turn's end of
// [TestACaptureTheLogTookIsSettledLandedWhateverItsTakersLease]: node-a's
// capture begun under a lease its process renews, its record reaching the log
// after the end's wait has read the turn's row once; unread makes every read
// of a taker lease fail, a store that cannot say of node-a's lease.
func endBesideACaptureTheLogTook(t *testing.T, unread bool) {
	inBubble(t, func(t *testing.T) {
		ctx := t.Context()
		store := newTakerStore()
		rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
		rig.coordinator.captureTick = time.Second
		run := rig.launchKept(t, "t1")
		lease := claimTaker(t, store, takingProcess)
		begun := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 1, Launch: run.LaunchID,
			State: CaptureBegun, Final: true, At: rig.coordinator.now(), Taker: takingProcess,
			TakerEpoch: lease.Epoch, Harness: "claude-code", SessionID: "s-1"}
		if took, err := rig.pending.StampCapture(ctx, "t1", begun); err != nil || !took {
			t.Fatalf("stamping the begun capture = %v, %v", took, err)
		}
		// THE TURN'S ROW ON THE LOG, its begin's: it names no capture
		// until the record reaches it, after the wait's first read.
		restore := &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": {Handle: "swe", Turn: "t1"}}}
		rig.coordinator.restore = restore
		if unread {
			store.failReads(errors.New("coord: the lease store could not say"))
		}
		row := landedRow(t, begun)
		stop, recorded, renewing := make(chan struct{}), make(chan struct{}), make(chan struct{})
		go func() {
			defer close(recorded)
			for len(restore.askedFor()) < 1 {
				select {
				case <-stop:
					return
				case <-time.After(time.Millisecond):
				}
			}
			onTheLog(restore, row)
		}()
		// node-a's PROCESS RENEWS ITS TAKER LEASE as a live process does,
		// its leases answered while its runs are not: the lease holds
		// through every pass of the end's wait.
		go func() {
			defer close(renewing)
			for {
				select {
				case <-stop:
					return
				case <-time.After(seat.SeatLeaseTTL / coord.KeptRenewals):
				}
				if ok, err := store.Backend.Renew(ctx, coord.TakerResource(takingProcess), takingProcess,
					lease.Epoch, seat.SeatLeaseTTL); err != nil || !ok {
					t.Errorf("renewing node-a's taker lease = %v, %v", ok, err)
					return
				}
			}
		}()

		started := time.Now()
		rig.coordinator.finish(ctx, rig.get("t1"), ending{fence: fenceOf(run),
			note: &failureNote{reason: string(LossLaunchNodeStoreSilent),
				detail: "the node it ran on could not reach the coordination store for the host grace"}})
		for range 3 {
			if rig.fireRetries() == 0 {
				break
			}
		}
		waited := time.Since(started)
		held := heldBy(t, store, takingProcess)
		close(stop)
		<-recorded
		<-renewing
		if held == nil || held.Epoch != lease.Epoch {
			t.Fatalf("the premise: node-a's taker lease is %+v, want it still held at %d", held, lease.Epoch)
		}

		ends := writes.endedRecords()
		if len(ends) != 1 {
			t.Fatalf("recorded %d ends, want the turn's one", len(ends))
		}
		if unkept := ends[0].Ending.Unkept; unkept != nil {
			t.Errorf("the end names %+v unkept, want the capture the log took kept", unkept)
		}
		if waited >= SessionCaptureBudget {
			t.Errorf("the end waited %s, want it recorded once the log named the capture, not by its age",
				waited)
		}
		read := restore.askedFor()
		if len(read) != 2 {
			t.Errorf("the turn's row was read %v, want once finding nothing and once finding the capture", read)
		}
		if paced := time.Duration(len(read)) * heldRowPace; paced > waited {
			t.Errorf("the turn's row was read %d times in %s, want at most once every %s", len(read), waited,
				heldRowPace)
		}
	})
}

// AN UNRESOLVED CAPTURE IS ASKED AGAIN BEFORE A RELAUNCH DECIDES. A capture whose
// record was sent and never answered may have landed; only the turn's end asked
// it again, so a relaunch that followed read it as never answered and started a
// session of its own beside a record the log had taken. The relaunch asks it
// again under its own operation, as the end does, within its own wait: landed,
// it is continued.
//
// Mutation: drop the ask from the relaunch ([Coordinator.ContinuedCapture]),
// and it says the capture's record was never answered and continues nothing.
func TestAnUnresolvedCaptureIsAskedAgainBeforeARelaunchDecides(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		collectHeld(t, rig, "t1")
		unresolved := rig.get("t1").Capture
		if unresolved == nil || unresolved.State != CaptureUnresolved {
			t.Fatalf("the premise: the row holds %+v, want its capture unresolved", unresolved)
		}
		writes.answer(nil)
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, *unresolved)}}

		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
		if why != "" || !continued.Capture.samePlace(*unresolved) || continued.Capture.State != CaptureLanded {
			t.Fatalf("the relaunch continues %+v (%q), want the capture its ask landed", continued, why)
		}
		if kept := rig.get("t1").Kept; kept == nil || !kept.samePlace(*unresolved) || kept.State != CaptureLanded {
			t.Errorf("the row keeps %+v, want the capture the relaunch's ask landed", kept)
		}
		captures := writes.captured()
		if len(captures) < 2 {
			t.Fatalf("the log was asked %d times, want the collection's attempts and the relaunch's ask", len(captures))
		}
		last := captures[len(captures)-1]
		if last.err != nil || last.operation() != captures[0].operation() {
			t.Errorf("the log was last asked %+v, want the capture asked again under its own operation and landed",
				last)
		}
	})
}

// A CAPTURE A RELAUNCH'S ASK LANDS IS THE TURN'S OWN TO READ, CONTINUED OR NOT.
// A run that starts a session of its own is told the session the relaunch's
// read found kept, so it may read it ([ContinuedSession.KeptTurn]): the turn's
// own where its run's record keeps a capture of it. A capture whose record was
// never answered, which the relaunch asks again and the log lands, is one —
// kept once the ask is answered, not before it — so a relaunch whose runner
// then cannot take that session up still names the turn's own to its run.
//
// Mutation: name the turn's own off the record as it stood before the ask,
// and the relaunch names its run no session to read.
func TestACaptureARelaunchsAskLandsIsTheTurnsOwnToRead(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		collectHeld(t, rig, "t1")
		run := rig.get("t1")
		unresolved := run.Capture
		if unresolved == nil || unresolved.State != CaptureUnresolved || run.Kept != nil {
			t.Fatalf("the premise: the row holds %+v and keeps %+v, want its one capture unresolved and none kept",
				unresolved, run.Kept)
		}
		writes.answer(nil)
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, *unresolved)}}
		refusing := fakeRestorer{why: "no runner of this member takes a claude-code session up"}

		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", refusing)
		if why != refusing.why || continued.Capture.CaptureSeq != 0 {
			t.Fatalf("the relaunch continues %+v (%q), want none, said: its runner takes the session up no more",
				continued, why)
		}
		if kept := rig.get("t1").Kept; kept == nil || !kept.samePlace(*unresolved) {
			t.Fatalf("the premise: the row keeps %+v, want the capture the relaunch's ask landed", kept)
		}
		if continued.KeptTurn != "t1" {
			t.Errorf("the relaunch names %q to its run as a session to read, want the turn's own, whose capture "+
				"its ask landed", continued.KeptTurn)
		}
	})
}

// A RELAUNCH'S ASK OF AN UNANSWERED RECORD ENDS WITHIN THE RELAUNCH'S OWN WAIT.
// The frame that relaunches holds the seat and a slot of the node's admission,
// so it waits for what is unsettled no longer than [relaunchCaptureWait] in
// all; an ask of a record the log never answers is cut there, the relaunch
// continuing nothing of it, rather than held to a record's whole budget.
//
// IN A SYNCTEST BUBBLE, so the ask's attempts are spaced, and the relaunch's
// wait measured, on the bubble's clock, which only their own waits advance.
//
// Mutation: ask the record under the relaunch's context with no deadline of
// the relaunch's, and the relaunch waits out the record's whole budget, an
// hour here.
func TestARelaunchsAskOfAnUnansweredRecordEndsWithinItsWait(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		collectHeld(t, rig, "t1")
		if unresolved := rig.get("t1").Capture; unresolved == nil || unresolved.State != CaptureUnresolved {
			t.Fatalf("the premise: the row holds %+v, want its capture unresolved", unresolved)
		}
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{}}
		rig.coordinator.relaunchWait, rig.coordinator.recordBudget = 2*time.Second, time.Hour
		asked := len(writes.captured())

		started := time.Now()
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
		waited := time.Since(started)

		if len(writes.captured()) <= asked {
			t.Fatalf("the log was asked nothing more by the relaunch, want the capture's record asked again")
		}
		if waited > rig.coordinator.relaunchWait {
			t.Errorf("the relaunch waited %s on a record the log never answered, want it held to its own %s",
				waited, rig.coordinator.relaunchWait)
		}
		if why == "" || continued.Capture.CaptureSeq != 0 {
			t.Errorf("the relaunch continues %+v (%q), want none of a capture whose record was never answered",
				continued, why)
		}
	})
}

// A FENCED KEEP UPLOADS AND RECORDS NOTHING. A keep checks the taker lease its
// stamp names before its upload and before every attempt at its record: once
// that lease has lapsed and been claimed again at a new epoch, a peer may have
// settled the capture refused, so the keep sends nothing more. Fenced before its
// upload or before its record, it stamps the capture refused; fenced between two
// attempts at its record, unresolved, since the first may yet have landed.
//
// IN A SYNCTEST BUBBLE: the store's clock passes the lease's TTL before the
// collection, and the bubble's one renewal interval on (11.25s) while the keep
// is held, so the renewal claims the lease at a new epoch exactly then.
// Mutation: drop either fence, or read a fenced keep that sent its record as
// refused, and a case goes red.
func TestAFencedKeepUploadsAndRecordsNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// gate holds the keep while its lease moves: its first step, or its
		// upload; neither, where the record's own spacing does.
		gate       func(*sessionKeep, chan struct{})
		unanswered bool
		uploads    int
		records    int
		want       CaptureState
	}{
		{name: "before its upload", gate: func(k *sessionKeep, g chan struct{}) { k.mintGate = g },
			want: CaptureRefused},
		{name: "between its upload and its record", gate: func(k *sessionKeep, g chan struct{}) { k.gate = g },
			uploads: 1, want: CaptureRefused},
		{name: "between two attempts at its record", unanswered: true, uploads: 1, records: 2,
			want: CaptureUnresolved},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
				// THE RECORD'S ATTEMPTS AT THEIR CEILING'S SPACING, eight seconds
				// apart: the second goes out before the lease moves at the
				// renewal eleven and a quarter seconds on, and the third would
				// after it.
				rig.coordinator.sessionRetry, rig.coordinator.recordBudget = sessionRetryCeiling, time.Minute
				rig.runner.Carries = true
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				gate := make(chan struct{})
				if c.gate != nil {
					c.gate(keep, gate)
				}
				if c.unanswered {
					writes.answer(errors.New("estate: the write was sent to a data node that did not answer"))
				}
				lapse(store)
				rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
					HarnessVersion: "2.1.310", Captured: &session.Archive})
				collectLeaving(t, rig, "t1")
				begun := rig.get("t1").Capture
				if begun == nil || begun.Taker != thisProcess || begun.TakerEpoch == 0 {
					t.Fatalf("the premise: the collection's capture is %+v, want it begun under this "+
						"process's taker lease", begun)
				}
				time.Sleep(seat.SeatLeaseTTL / coord.KeptRenewals)
				synctest.Wait()
				if lease := heldBy(t, store, thisProcess); lease == nil || lease.Epoch <= begun.TakerEpoch {
					t.Fatalf("the premise: this process's taker lease is %+v a renewal on, want it claimed "+
						"again past the %d the capture names", lease, begun.TakerEpoch)
				}
				close(gate)
				rig.awaitKeeps()

				if uploads := len(keep.uploads()); uploads != c.uploads {
					t.Errorf("the keep uploaded %d sessions, want %d", uploads, c.uploads)
				}
				if records := len(writes.captured()); records != c.records {
					t.Errorf("the keep asked the log %d times, want %d", records, c.records)
				}
				got := rig.get("t1").Capture
				if got == nil || !got.sameTake(*begun) || got.State != c.want ||
					!strings.Contains(got.Reason, "holds its lease at epoch") {
					t.Errorf("the row holds %+v, want the capture %s for its fenced keep", got, c.want)
				}
			})
		})
	}
}

// A TAKER LEASE OUTLASTS THE DRAIN AND IS RELEASED AFTER THE LAST KEEP. A drain
// gives the node's presence up at its first step while its keeps still upload;
// the taker lease is not presence, and is renewed on its own cadence at the
// epoch their stamps name, so a keep outlasting the drain by three lease TTLs
// still records its capture. The stop lets the keep end before releasing the
// lease: released under it, a peer would settle the capture refused with its
// record on its way. After the last keep ends, the lease is given back.
// IN A SYNCTEST BUBBLE, so three TTLs pass on the clock the renewals keep.
//
// Mutation: renew the lease with presence, release it at the drain's first
// step, or before the stop has seen the last keep end, and a check goes red;
// leave it to lapse, and the last one does.
func TestATakerLeaseOutlastsTheDrainAndIsReleasedAfterTheLastKeep(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		ctx := t.Context()
		store := newTakerStore()
		rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
		presence, _, err := store.Backend.TryAcquire(ctx, coord.NodeResource("node-b"),
			coord.AcquireOptions{Owner: thisProcess, TTL: seat.SeatLeaseTTL, Ungated: true})
		if err != nil || presence == nil {
			t.Fatalf("claim this node's presence: %v", err)
		}
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		gate := make(chan struct{})
		keep.gate = gate
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		collectLeaving(t, rig, "t1")
		begun := rig.get("t1").Capture
		if begun == nil || begun.State != CaptureBegun || begun.Taker != thisProcess {
			t.Fatalf("the premise: the collection's capture is %+v, want it begun under this process's "+
				"taker lease", begun)
		}

		// THE DRAIN'S FIRST STEP: presence given up, the keep still uploading.
		if ok, err := store.Release(ctx, coord.NodeResource("node-b"), thisProcess, presence.Epoch); err != nil ||
			!ok {
			t.Fatalf("give this node's presence up = %v, %v", ok, err)
		}
		time.Sleep(3 * seat.SeatLeaseTTL)
		synctest.Wait()
		if lease := heldBy(t, store, thisProcess); lease == nil || lease.Epoch != begun.TakerEpoch {
			t.Fatalf("this process's taker lease is %+v three TTLs into the drain, want it held at the %d its "+
				"keep's capture names", lease, begun.TakerEpoch)
		}

		// THE NODE STOPS with the keep still under way.
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			rig.coordinator.Stop()
		}()
		synctest.Wait()
		if lease := heldBy(t, store, thisProcess); lease == nil || lease.Epoch != begun.TakerEpoch {
			t.Fatalf("this process's taker lease is %+v while its stop waits on a keep, want it still held", lease)
		}
		close(gate)
		<-stopped
		if lease := heldBy(t, store, thisProcess); lease != nil {
			t.Errorf("this process's taker lease is still held at %d once its last keep ended, want it given back",
				lease.Epoch)
		}
		if kept := rig.get("t1").Kept; kept == nil || !kept.sameTake(*begun) || kept.State != CaptureLanded {
			t.Errorf("the row keeps %+v, want the keep that outlasted the drain landed", kept)
		}
		if records := len(writes.captured()); records != 1 {
			t.Errorf("the keep asked the log %d times, want its record made once", records)
		}
	})
}

// A TAKE WITH NO TAKER LEASE BEGINS NO CAPTURE. A begun capture is waited on
// for as long as the taker lease its stamp names holds, so one this process
// could not name a lease for (claim unanswered, or renewals unanswered past the
// last one's window) would be read as its taker gone at once. The take is
// refused before it begins, saying why; nothing is uploaded or recorded.
// IN A SYNCTEST BUBBLE the window passes on the bubble's clock, three and a half
// renewal intervals after the claim: past the three it covers, inside the TTL's
// four.
//
// Mutation: begin a capture with no taker lease held, naming none, or with
// one whose window has closed, and the keep uploads and records it.
func TestATakeWithNoTakerLeaseBeginsNoCapture(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// claimed says the store answered the coordinator's claim before it
		// stopped answering.
		claimed bool
	}{
		{name: "its claim went unanswered"},
		{name: "its renewals went unanswered past their window", claimed: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				down := errors.New("coord: the lease store did not answer")
				if !c.claimed {
					store.setDown(down)
				}
				rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
				if c.claimed {
					store.setDown(down)
					every := seat.SeatLeaseTTL / coord.KeptRenewals
					time.Sleep(3*every + every/2)
					synctest.Wait()
					if lease := heldBy(t, store, thisProcess); lease == nil {
						t.Fatal("the premise: this process's taker lease lapsed at the store, want it held " +
							"inside its TTL")
					}
				}
				rig.runner.Carries = true
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
					Captured: &session.Archive})
				collectHeld(t, rig, "t1")
				got := rig.get("t1").Capture
				if got == nil || got.State != CaptureRefused || !strings.Contains(got.Reason, "holds no taker lease") {
					t.Errorf("the row holds %+v, want the capture refused untaken for want of a taker lease", got)
				}
				if uploads := len(keep.uploads()); uploads != 0 {
					t.Errorf("uploaded %d sessions with no taker lease held, want none", uploads)
				}
				if records := len(writes.captured()); records != 0 {
					t.Errorf("asked the log %d times with no taker lease held, want none", records)
				}
			})
		})
	}
}

// A STAMP WITH NO TAKER IS SETTLED BY ITS AGE ALONE: nothing says whether its
// take still runs, so a relaunch waits for it as before and reads one past a
// capture's budget as stopped; no lease is read and the log never settles it.
// The relaunch continues the job's older kept capture, reading the turn's row
// once to decide, and its line says which the stamp was.
//
// In a synctest bubble, and not parallel: it captures the process-wide logger
// ([captureLogs]).
//
// Mutation: read a lease for it, settle it from the log, or tell one past its
// budget as still being kept, and a check goes red.
func TestAStampWithNoTakerIsSettledByAge(t *testing.T) {
	for _, c := range []struct {
		name string
		age  time.Duration
		line string
	}{
		{name: "younger than a capture can take",
			line: "(launch 1, capture 2) may still be kept: it is still being kept"},
		{name: "older than a capture can take", age: SessionCaptureBudget,
			line: "(launch 1, capture 2) was not kept: it was still being kept, or was begun and never finished"},
	} {
		t.Run(c.name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				logs := captureLogs(t)
				store := newTakerStore()
				rig, restore, kept := keptTurnBy(t, taking(thisProcess, store))
				rig.coordinator.relaunchWait = 50 * time.Millisecond
				begun := takenBy(t, rig, kept, "", 0)
				rig.now = rig.now.Add(c.age)
				continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
				if why != "" || !continued.Capture.samePlace(kept) {
					t.Fatalf("the relaunch continues %+v (%q), want the capture its job kept before the stamp",
						continued, why)
				}
				if lines := passedOverLines(logs); len(lines) != 1 || !strings.Contains(lines[0], c.line) {
					t.Errorf("logged %q, want one line saying %q", lines, c.line)
				}
				if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureBegun {
					t.Errorf("the row holds %+v, want the older build's capture left begun", got)
				}
				if reads := store.readsAll(); reads != 0 {
					t.Errorf("a lease was read %d times, want none for a stamp that names no taker", reads)
				}
				if asked := restore.askedFor(); len(asked) != 1 {
					t.Errorf("the turn's row was read %v, want it read once, by the decision, and never by a "+
						"settle of a stamp that names no taker", asked)
				}
			})
		})
	}
}

// A KEEP IS FENCED ONCE ITS LAST RENEWAL NO LONGER COVERS IT, WHILE ITS LEASE
// STILL HOLDS. A taker that cannot reach the store never hears it answer no: its
// lease lapses there one TTL after its last renewal, and the window that renewal
// confirmed, an interval shorter, stops the taker first. Once renewals go
// unanswered past it, a keep uploads and records nothing and a take begun then
// finds no tenure; one renewal confirming the same epoch reopens it. IN A
// SYNCTEST BUBBLE the keep is held until three and a half intervals after the
// claim: past the window's three, inside the TTL's four and [keepReadRoom].
//
// Mutation: read a tenure as held whatever its window, confirm an unanswered
// renewal, or confirm a window as long as the TTL, and the keep records under
// an uncovered lease; leave a same-epoch renewal unconfirmed, and it stays shut.
func TestAKeepIsFencedOnceItsLastRenewalNoLongerCoversIt(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		gate    func(*sessionKeep, chan struct{})
		uploads int
		why     string
	}{
		{name: "before its upload", gate: func(k *sessionKeep, g chan struct{}) { k.mintGate = g },
			why: "so it was neither uploaded nor recorded"},
		{name: "between its upload and its record", gate: func(k *sessionKeep, g chan struct{}) { k.gate = g },
			uploads: 1, why: "so its upload was not recorded"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				store := newTakerStore()
				rig, writes, keep := keepingRigWith(t, taking(thisProcess, store))
				rig.runner.Carries = true
				rig.launchKept(t, "t1")
				session := rig.runner.Session
				gate := make(chan struct{})
				c.gate(keep, gate)
				store.setDown(errors.New("coord: the lease store did not answer"))
				every := seat.SeatLeaseTTL / coord.KeptRenewals
				time.Sleep(every)
				synctest.Wait()
				rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1",
					HarnessVersion: "2.1.310", Captured: &session.Archive})
				collectLeaving(t, rig, "t1")
				begun := rig.get("t1").Capture
				if begun == nil || begun.State != CaptureBegun || begun.TakerEpoch == 0 {
					t.Fatalf("the premise: the collection's capture is %+v, want it begun under this "+
						"process's taker lease", begun)
				}
				time.Sleep(2*every + every/2)
				synctest.Wait()
				if lease := heldBy(t, store, thisProcess); lease == nil || lease.Epoch != begun.TakerEpoch {
					t.Fatalf("the premise: this process's taker lease is %+v, want it still held at the "+
						"store at the %d the capture names", lease, begun.TakerEpoch)
				}
				if hold, held := rig.coordinator.heldTaker(); held {
					t.Errorf("a take begun past the window of the last renewal would stamp %+v, want no "+
						"tenure to stamp", hold)
				}
				close(gate)
				rig.awaitKeeps()

				if uploads := len(keep.uploads()); uploads != c.uploads {
					t.Errorf("the keep uploaded %d sessions, want %d", uploads, c.uploads)
				}
				if records := len(writes.captured()); records != 0 {
					t.Errorf("the keep asked the log %d times past its window, want none", records)
				}
				got := rig.get("t1").Capture
				if got == nil || !got.sameTake(*begun) || got.State != CaptureRefused ||
					!strings.Contains(got.Reason, "holds no lease whose last renewal still covers the keep") ||
					!strings.Contains(got.Reason, c.why) {
					t.Errorf("the row holds %+v, want the capture refused for a keep its window no longer "+
						"covers, %s", got, c.why)
				}

				// THE STORE ANSWERS AGAIN, inside the lease's TTL, and a renewal
				// confirms the same epoch.
				store.setDown(nil)
				rig.coordinator.taker.beat(t.Context())
				if hold, held := rig.coordinator.heldTaker(); !held || hold.epoch != begun.TakerEpoch {
					t.Errorf("a renewal of epoch %d confirmed left the tenure %+v (held %v), want the window "+
						"open again at it", begun.TakerEpoch, hold, held)
				}
			})
		})
	}
}

// A TAKE STAMPS THE TENURE IT RESERVES ITS PLACE UNDER. A take packs and scrubs
// the session before it reserves its place (minutes at the bounds), and the
// taker lease can lapse and be claimed again at a new epoch meanwhile. The
// stamp names the tenure read again as it reserves, so the keep lands under it;
// with the tenure read before the pack, the capture would name one every peer
// reads as gone, and its own keep would fence it.
//
// IN A SYNCTEST BUBBLE: the take is held in its scrub while the store's clock
// moves past the lease and the coordinator's next renewal claims it again.
//
// Mutation: stamp the tenure read before the pack, and the keep is fenced and
// the capture refused.
func TestATakeStampsTheTenureItReservesItsPlaceUnder(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
		first := heldBy(t, store, thisProcess)
		if first == nil {
			t.Fatal("the premise: the coordinator holds no taker lease")
		}
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		scrub := &scrubGate{PendingStore: rig.coordinator.pending, reached: make(chan struct{}),
			gate: make(chan struct{})}
		rig.coordinator.pending = scrub
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		rig.resumer.failWith(errors.New("held for the case to read the row"))
		payload, ev := rig.completion("t1")
		collected := make(chan error, 1)
		go func() { collected <- rig.coordinator.OnCompleted(t.Context(), payload, ev) }()

		<-scrub.reached
		lapse(store)
		time.Sleep(seat.SeatLeaseTTL / coord.KeptRenewals)
		synctest.Wait()
		again := heldBy(t, store, thisProcess)
		if again == nil || again.Epoch <= first.Epoch {
			t.Fatalf("the premise: this process's taker lease is %+v once claimed again, want it past the "+
				"%d the take began under", again, first.Epoch)
		}
		close(scrub.gate)
		if err := <-collected; err == nil {
			t.Fatal("the refused resume was acknowledged")
		}
		rig.awaitKeeps()

		got := rig.get("t1").Capture
		if got == nil || got.TakerEpoch != again.Epoch || got.State != CaptureLanded {
			t.Errorf("the row holds %+v, want the capture stamped under the tenure held as it reserved its "+
				"place, %d, and landed", got, again.Epoch)
		}
		if records := len(writes.captured()); records != 1 {
			t.Errorf("the keep asked the log %d times, want its record made once", records)
		}
	})
}

// scrubGate holds a take in its scrub — the read of what the box was handed,
// which a take makes after it asks for its taker lease and before it reserves
// its place — until gate closes, saying on reached that it has.
type scrubGate struct {
	PendingStore
	reached chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (s *scrubGate) Handed(ctx context.Context, run PendingRun) (string, error) {
	s.once.Do(func() { close(s.reached) })
	select {
	case <-s.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return s.PendingStore.Handed(ctx, run)
}

// AN ATTEMPT AT A KEEP'S RECORD ENDS WITH THE WINDOW IT WAS SENT IN. A keep asks
// its taker lease before every attempt at its record, and an attempt sent a
// moment before the window of its last confirmed renewal closed went on,
// bounded by the record's budget alone (thirty seconds, past the window's
// margin of one renewal interval), so it could land after a peer had settled the
// capture refused. Here the log answers no attempt and the store no renewal: the
// one attempt ends at the window's end, the next is fenced, and the capture is
// left unresolved (sent, perhaps landed). IN A SYNCTEST BUBBLE.
//
// Mutation: send an attempt under the record's budget alone, and it ends with
// the keep's own budget, minutes past the window.
func TestAnAttemptAtAKeepsRecordEndsWithTheWindowItWasSentIn(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, writes, _ := keepingRigWith(t, taking(thisProcess, store))
		rig.coordinator.recordBudget = time.Hour
		hold, held := rig.coordinator.heldTaker()
		if !held {
			t.Fatal("the premise: the coordinator holds no taker lease")
		}
		rig.runner.Carries = true
		rig.launchKept(t, "t1")
		session := rig.runner.Session
		writes.holdOn()
		store.setDown(errors.New("coord: the lease store did not answer"))
		rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-1", HarnessVersion: "2.1.310",
			Captured: &session.Archive})
		collectLeaving(t, rig, "t1")
		rig.awaitKeeps()

		attempts := writes.captured()
		if len(attempts) != 1 {
			t.Fatalf("the keep asked the log %d times, want the one attempt its window covered", len(attempts))
		}
		if !attempts[0].deadline.Equal(hold.through) {
			t.Errorf("the attempt at the record was sent to end at %v, want the end of the window it was "+
				"sent in, %v", attempts[0].deadline, hold.through)
		}
		got := rig.get("t1").Capture
		if got == nil || got.State != CaptureUnresolved ||
			!strings.Contains(got.Reason, "holds no lease whose last renewal still covers the keep") {
			t.Errorf("the row holds %+v, want the capture unresolved, its sent record fenced", got)
		}
	})
}

// A SETTLE LEAVES WHAT ITS TAKER STAMPED SINCE. A node settles a capture whose
// taker is gone from a copy of the row on which the take stood begun; the taker
// may have stamped it since — its fenced keep's unresolved record among them,
// which says a record was sent and may yet land. The settle is written only
// over the begun stamp it was decided from, so the taker's stamp stands, and
// the wait goes on with the row as it is: an unresolved capture, which the
// turn's end asks again under its own operation rather than reads as refused.
//
// Mutation: write the settle by the row's ordinary rule, which advances an
// unresolved capture to an answer, and a record that may yet land is stamped
// refused.
func TestASettleLeavesWhatItsTakerStampedSince(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		store := newTakerStore()
		rig, _, kept := keptTurnBy(t, taking(thisProcess, store))
		lease := claimTaker(t, store, takingProcess)
		begun := takenBy(t, rig, kept, takingProcess, lease.Epoch)
		asRead := rig.get("t1")
		// THE TAKER'S OWN KEEP, its record sent and not answered, stamped after the
		// waiting node read the row.
		unresolved := begun
		unresolved.State, unresolved.Reason = CaptureUnresolved, "its record was sent and not answered"
		unresolved.Object, unresolved.ObjectHash, unresolved.ObjectSize = objstore.KeyAt(begun.At).String(), "h2", 2048
		if took, err := rig.pending.StampCapture(t.Context(), "t1", unresolved); err != nil || !took {
			t.Fatalf("stamping the taker's unresolved capture = %v, %v", took, err)
		}
		lapse(store)

		settled, found, _ := rig.coordinator.settleGone(t.Context(), asRead, askEvery, false)
		if !found {
			t.Fatal("the run's row was read as gone")
		}
		if got := rig.get("t1").Capture; got == nil || !got.sameTake(begun) || got.State != CaptureUnresolved {
			t.Errorf("the row holds %+v, want the taker's unresolved stamp left as it stood", got)
		}
		if settled.Capture == nil || settled.Capture.State != CaptureUnresolved {
			t.Errorf("the settle answered %+v, want the row as it stands, unresolved", settled.Capture)
		}
	})
}

// A TAKER LEASE IS NEVER ASKED FOR LONGER THAN THE STORE KEEPS ONE. The lease
// follows the seat lease TTL the deployment configures, which nothing bounds
// from above, while the store keeps a lease outside seats and presence for
// coord.MaxDutyTTL at most and refuses a claim for longer: asked for a seat
// lease TTL past three hours, every claim was refused, and no capture was
// begun anywhere. The lease asks for MaxDutyTTL instead, and is held.
//
// Mutation: ask for the TTL as handed, and the claim is refused and nothing is
// held.
func TestATakerLeaseIsNeverAskedForLongerThanTheStoreKeepsOne(t *testing.T) {
	t.Parallel()
	store := newTakerStore()
	lease := newTakerLease(store, thisProcess, 4*time.Hour)
	t.Cleanup(lease.stop)
	if _, held := lease.held(); !held {
		t.Fatal("a taker lease asked for past the longest lease the store keeps is held nowhere")
	}
	if got := heldBy(t, store, thisProcess); got == nil {
		t.Fatal("the store holds no taker lease for this process")
	}
	if lease.ttl != coord.MaxDutyTTL {
		t.Errorf("the lease asks for %v, want the %v the store keeps a lease outside seats and presence for",
			lease.ttl, coord.MaxDutyTTL)
	}
}

// A STOP UNDER A RENEWAL SAYS NOTHING OF THE STORE. The stop ends the lease's
// renewals while one may be under way, and that renewal's call then fails on
// the stop rather than on the store: logged, it read as the coordination store
// failing to answer — the line an operator reads its health by — at every
// clean stop that came during a renewal. Here a store that holds a renewal
// until its caller gives up: the stop cuts it, says nothing of it, and gives
// the lease back.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: log a renewal the stop cut as unanswered, and the line appears.
func TestAStopUnderARenewalSaysNothingOfTheStore(t *testing.T) {
	logs := captureLogs(t)
	synctest.Test(t, func(t *testing.T) {
		store := newTakerStore()
		lease := newTakerLease(store, thisProcess, seat.SeatLeaseTTL)
		store.holdRenewals()
		time.Sleep(seat.SeatLeaseTTL / coord.KeptRenewals)
		synctest.Wait()
		if held := store.renewalsHeld(); held != 1 {
			t.Fatalf("the premise: the store holds %d renewals, want the one under way", held)
		}
		lease.stop()
		if held := heldBy(t, store, thisProcess); held != nil {
			t.Errorf("the lease is still held at %d after the stop, want it given back", held.Epoch)
		}
	})
	if line := "executor_taker_lease_unanswered"; strings.Contains(logs.String(), line) {
		t.Errorf("a renewal the stop cut was logged %s:\n%s", line, logs.String())
	}
}
