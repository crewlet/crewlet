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

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/coordtest"
)

// chatReadFleet opens a fleet store on its own buckets with one person's
// record of two cursors, so a lost or replaced record shows as cursors gone.
func chatReadFleet(t *testing.T, handle string) (*FleetStore, *nats.Conn) {
	t.Helper()
	nc := embeddedNATS(t)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	if _, err := f.AdvanceChatRead(t.Context(), handle, coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-a"): 7, coordtest.ChatID("room-b"): 9}, At: chatReadAt0,
	}); err != nil {
		t.Fatalf("AdvanceChatRead: %v", err)
	}
	return f, nc
}

// chatReadAt0 is the instant every case here writes at.
var chatReadAt0 = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

// errWriteLost is the leader's decided refusal of a conditional write whose
// revision another writer moved past (10071), as a settled bucket hands it
// back: the one refusal this package's code sees, since the in-flight answer
// is waited out first (settle.go).
var errWriteLost = refusedAs(jetstream.JSErrCodeStreamWrongLastSequence)

// A FORGET ACTS ON THE REVISION IT JUDGED, OR IT DOES NOT ACT.
//
// [FleetStore.purgeChatReadAt], the rule [FleetStore.ForgetChatRead] is built
// from, is driven directly (as [FleetStore.writeFollowAt] is) because the
// interleaving opens inside the forget, between its read and its purge. It must
// never remove a record the caller never read.
func TestAChatReadPurgeBoundToAStaleRevisionDoesNotFire(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f, _ := chatReadFleet(t, "leaver")
	key := coord.ChatReadKey("leaver")
	_, judged, err := f.chatReadAt(ctx, "leaver")
	if err != nil {
		t.Fatalf("chatReadAt: %v", err)
	}
	// A flush lands between the judgement and the purge.
	if _, err := f.AdvanceChatRead(ctx, "leaver", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-c"): 11}, At: chatReadAt0,
	}); err != nil {
		t.Fatalf("the flush in the window: %v", err)
	}

	for name, revision := range map[string]uint64{
		"a revision a flush moved past": judged,
		// No revision at all to the client: an unconditional purge.
		"revision zero": 0,
	} {
		removed, err := f.purgeChatReadAt(ctx, key, revision)
		if err != nil || removed {
			t.Fatalf("a purge at %s = (%v, %v), want (false, nil): it removed a "+
				"record the caller never read", name, removed, err)
		}
	}
	if state, err := f.ChatRead(ctx, "leaver"); err != nil || len(state.Cursors) != 3 {
		t.Fatalf("after the refused purges the record reads %v (err %v), want "+
			"its three cursors intact", state.Cursors, err)
	}

	_, current, err := f.chatReadAt(ctx, "leaver")
	if err != nil {
		t.Fatalf("chatReadAt: %v", err)
	}
	if removed, err := f.purgeChatReadAt(ctx, key, current); err != nil || !removed {
		t.Fatalf("a purge at the current revision = (%v, %v), want (true, nil)", removed, err)
	}
	// A key a peer already removed is a lost race, not a failure.
	if removed, err := f.purgeChatReadAt(ctx, key, current); err != nil || removed {
		t.Fatalf("a purge of a record already gone = (%v, %v), want (false, nil)", removed, err)
	}
}

// A SEAT FOUND BACK WHILE ITS RECORD WAS BEING FORGOTTEN KEEPS EVERY CURSOR.
//
// The whole forget, with a peer's sweep clearing the stamp (the seat is back)
// after this call judged the record absent and before its purge. The memory
// twin checks and removes under one mutex and cannot stage that, so without
// this case a purge that dropped its revision bound passes every conformance
// case and irreversibly removes a restored seat's read state.
func TestAForgetOvertakenByAReturnKeepsTheRecord(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f, _ := chatReadFleet(t, "returner")
	if _, err := f.SetChatReadAbsence(ctx, "returner", chatReadAt0); err != nil {
		t.Fatalf("SetChatReadAbsence: %v", err)
	}
	overtaken := &returnedMidForget{KeyValue: f.chatReads, back: func(ctx context.Context) error {
		_, err := f.SetChatReadAbsence(ctx, "returner", time.Time{})
		return err
	}}
	f.chatReads = overtaken

	removed, err := f.ForgetChatRead(ctx, "returner", chatReadAt0.Add(48*time.Hour))
	if err != nil {
		t.Fatalf("ForgetChatRead: %v", err)
	}
	if removed {
		t.Fatal("the forget removed a record whose stamp a peer cleared after it " +
			"was judged: a seat restored to the chart lost every cursor")
	}
	if overtaken.purges != 1 {
		t.Fatalf("the forget purged %d times, want 1 — the case proves nothing "+
			"unless the purge was reached and then refused", overtaken.purges)
	}
	state, err := f.ChatRead(ctx, "returner")
	if err != nil {
		t.Fatalf("ChatRead: %v", err)
	}
	if len(state.Cursors) != 2 || !state.AbsentSince.IsZero() {
		t.Fatalf("the record reads cursors %v absent since %v, want both cursors "+
			"and the stamp the return cleared", state.Cursors, state.AbsentSince)
	}
}

// returnedMidForget is a bucket whose first purge is preceded by back — the
// write a peer lands between a forget's judgement and its purge.
type returnedMidForget struct {
	jetstream.KeyValue
	back   func(ctx context.Context) error
	purges int
}

func (k *returnedMidForget) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	k.purges++
	if k.purges == 1 {
		if err := k.back(ctx); err != nil {
			return err
		}
	}
	return k.KeyValue.Purge(ctx, key, opts...)
}

// A CORRUPT READ RECORD RAISES RATHER THAN VANISHING FROM THE LISTING.
//
// The reconcile judges from [FleetStore.ChatReaders]; a reader dropped from it
// is never stamped or forgotten and stays forever in a bucket nothing ages. The
// contract suite writes only through the merge, so it is planted here.
func TestAnUndecodableChatReadIsRaisedNotSkipped(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f, _ := chatReadFleet(t, "reader")
	if _, err := f.chatReads.Put(ctx, coord.ChatReadKey("broken"), []byte("{not json")); err != nil {
		t.Fatalf("planting the corrupt record: %v", err)
	}

	readers, err := f.ChatReaders(ctx)
	if err == nil {
		t.Fatalf("the listing skipped the corrupt record and answered %v — the "+
			"reconcile never judges a reader it is not shown", readers)
	}
	if !strings.Contains(err.Error(), "broken") {
		t.Errorf("the listing's failure %q does not name the record it could not decode", err)
	}
	if _, err := f.ChatRead(ctx, "broken"); err == nil {
		t.Error("a corrupt record read back as somebody's read state")
	}
	// The healthy one still reads: the failure is the record's, not the bucket's.
	if state, err := f.ChatRead(ctx, "reader"); err != nil || len(state.Cursors) != 2 {
		t.Fatalf("reader: %v (err %v)", state.Cursors, err)
	}
}

// A WRITE WHOSE ANSWER WAS LOST IS MARKED AS ONE THAT MAY HAVE LANDED, AND NO
// OTHER FAILURE IS.
//
// [FleetStore.AdvanceChatRead] reads before it writes, and its two failures
// mean opposite things: a failed read wrote nothing ("not recorded"), while a
// put whose reply never came may have landed. The put here LANDS and then loses
// its answer, so the record shows the write the error must not deny; the cases
// beside it wrote nothing and must not carry the mark.
func TestAReadStateWriteWhoseAnswerWasLostMayHaveLanded(t *testing.T) {
	t.Parallel()

	t.Run("a flush whose put landed and went unanswered", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f, _ := chatReadFleet(t, "founder")
		f.chatReads = &answerLost{KeyValue: f.chatReads, land: true}

		_, err := f.AdvanceChatRead(ctx, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{coordtest.ChatID("room-c"): 11}, At: chatReadAt0,
		})
		if !errors.Is(err, coord.ErrWriteUnknown) || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("a flush whose answer was lost = %v, want ErrWriteUnknown and "+
				"ErrUnavailable", err)
		}
		f.chatReads = f.chatReads.(*answerLost).KeyValue
		if state, err := f.ChatRead(ctx, "founder"); err != nil || state.Cursors[coordtest.ChatID("room-c")] != 11 {
			t.Fatalf("the record reads %v (err %v): the case staged a write that "+
				"landed, which is the one the error must not deny", state.Cursors, err)
		}
	})

	t.Run("an absence stamp whose put went unanswered", func(t *testing.T) {
		t.Parallel()
		f, _ := chatReadFleet(t, "leaver")
		f.chatReads = &answerLost{KeyValue: f.chatReads}
		if _, err := f.SetChatReadAbsence(t.Context(), "leaver", chatReadAt0); !errors.Is(err, coord.ErrWriteUnknown) {
			t.Fatalf("an absence stamp whose answer was lost = %v, want ErrWriteUnknown", err)
		}
	})

	t.Run("a forget whose purge went unanswered", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f, _ := chatReadFleet(t, "leaver")
		if _, err := f.SetChatReadAbsence(ctx, "leaver", chatReadAt0); err != nil {
			t.Fatalf("SetChatReadAbsence: %v", err)
		}
		f.chatReads = &answerLost{KeyValue: f.chatReads}
		if _, err := f.ForgetChatRead(ctx, "leaver", chatReadAt0); !errors.Is(err, coord.ErrWriteUnknown) {
			t.Fatalf("a forget whose answer was lost = %v, want ErrWriteUnknown", err)
		}
	})

	t.Run("a flush whose read failed wrote nothing", func(t *testing.T) {
		t.Parallel()
		f, _ := chatReadFleet(t, "founder")
		lost := &answerLost{KeyValue: f.chatReads, land: true}
		f.chatReads = lost
		gone, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := f.AdvanceChatRead(gone, "founder", coord.ChatReadDelta{
			Cursors: map[string]int64{coordtest.ChatID("room-c"): 11}, At: chatReadAt0,
		})
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("a flush whose read could not be made = %v, want ErrUnavailable", err)
		}
		if errors.Is(err, coord.ErrWriteUnknown) {
			t.Fatalf("a flush that failed before any put is marked as one that may "+
				"have landed: %v", err)
		}
		if lost.puts() != 0 {
			t.Fatalf("the case staged a failure after %d put(s), want one before any", lost.puts())
		}
	})
}

// A FIRST WRITE IS A CREATE, AND A CREATE READS BETWEEN ITS WRITES — so only
// its own write going unanswered may have landed. A create refused and then
// unable to re-read the key, or refused every round, wrote nothing; marking it
// ErrWriteUnknown tells a person a flush that did not land "may have been
// recorded", against the ChatReads contract.
func TestAFirstReadStateWriteIsUnknownOnlyWhenItsOwnWriteWasUnanswered(t *testing.T) {
	t.Parallel()
	flush := coord.ChatReadDelta{Cursors: map[string]int64{coordtest.ChatID("room-c"): 11}, At: chatReadAt0}

	t.Run("a create whose write landed and went unanswered", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f, _ := chatReadFleet(t, "founder")
		lost := &answerLost{KeyValue: f.chatReads, land: true}
		f.chatReads = lost
		_, err := f.AdvanceChatRead(ctx, "newcomer", flush)
		if !errors.Is(err, coord.ErrWriteUnknown) || !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("a first flush whose create went unanswered = %v, want "+
				"ErrWriteUnknown and ErrUnavailable", err)
		}
		f.chatReads = lost.KeyValue
		if state, err := f.ChatRead(ctx, "newcomer"); err != nil || state.Cursors[coordtest.ChatID("room-c")] != 11 {
			t.Fatalf("the record reads %v (err %v): the case staged a create that "+
				"landed, which is the one the error must not deny", state.Cursors, err)
		}
	})

	t.Run("a create refused and then unable to read the key again", func(t *testing.T) {
		t.Parallel()
		f, _ := chatReadFleet(t, "founder")
		ctx, blind := context.WithCancel(t.Context())
		defer blind()
		refused := &refusedCreate{KeyValue: f.chatReads, blind: blind}
		f.chatReads = refused
		_, err := f.AdvanceChatRead(ctx, "newcomer", flush)
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("a first flush whose create could not re-read = %v, want "+
				"ErrUnavailable", err)
		}
		if errors.Is(err, coord.ErrWriteUnknown) {
			t.Fatalf("a create whose one write was refused is marked as one that "+
				"may have landed: %v", err)
		}
		if refused.count() != 1 {
			t.Fatalf("the case staged %d refused write(s), want the one the "+
				"re-read follows", refused.count())
		}
		f.chatReads = refused.KeyValue
		if state, err := f.ChatRead(t.Context(), "newcomer"); err != nil || state.Revision != 0 {
			t.Fatalf("newcomer's record reads %+v (err %v), want none", state, err)
		}
	})

	t.Run("a create refused on every round", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		f, _ := chatReadFleet(t, "founder")
		refused := &refusedCreate{KeyValue: f.chatReads}
		f.chatReads = refused
		_, err := f.AdvanceChatRead(ctx, "newcomer", flush)
		if !errors.Is(err, coord.ErrUnavailable) {
			t.Fatalf("a first flush refused on every round = %v, want ErrUnavailable", err)
		}
		if errors.Is(err, coord.ErrWriteUnknown) {
			t.Fatalf("a create whose every write was refused is marked as one that "+
				"may have landed: %v", err)
		}
		if refused.count() != createRounds {
			t.Fatalf("the create made %d write(s), want its bound of %d", refused.count(),
				createRounds)
		}
		f.chatReads = refused.KeyValue
		if state, err := f.ChatRead(ctx, "newcomer"); err != nil || state.Revision != 0 {
			t.Fatalf("newcomer's record reads %+v (err %v), want none", state, err)
		}
	})
}

// refusedCreate is a bucket whose every write is refused as one a peer beat
// ([errWriteLost]), reaching no broker. A set blind also cancels the write's
// context, so the create's re-read of the key fails after it.
type refusedCreate struct {
	jetstream.KeyValue
	blind context.CancelFunc

	mu      sync.Mutex
	refused int
}

func (k *refusedCreate) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.refused
}

func (k *refusedCreate) Update(context.Context, string, []byte, uint64) (uint64, error) {
	k.mu.Lock()
	k.refused++
	k.mu.Unlock()
	if k.blind != nil {
		k.blind()
	}
	return 0, errWriteLost
}

// errAnswerLost is what a client hands back for a request whose reply did not
// arrive in time: the write went out, and nobody knows what became of it.
var errAnswerLost = nats.ErrTimeout

// answerLost is a bucket whose every write is answered [errAnswerLost] —
// after reaching the broker when land is set, before it otherwise. Reads are
// untouched.
type answerLost struct {
	jetstream.KeyValue
	land bool

	mu      sync.Mutex
	written int
}

func (k *answerLost) puts() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.written
}

func (k *answerLost) Update(ctx context.Context, key string, value []byte,
	revision uint64) (uint64, error) {

	k.mu.Lock()
	k.written++
	k.mu.Unlock()
	if k.land {
		if _, err := k.KeyValue.Update(ctx, key, value, revision); err != nil {
			return 0, err
		}
	}
	return 0, errAnswerLost
}

func (k *answerLost) Purge(ctx context.Context, key string, opts ...jetstream.KVDeleteOpt) error {
	if k.land {
		if err := k.KeyValue.Purge(ctx, key, opts...); err != nil {
			return err
		}
	}
	return errAnswerLost
}

// A FLUSH THAT LOSES EVERY ATTEMPT IS UNAVAILABLE — AND IT SAYS NOTHING
// LANDED.
//
// Every update is refused as one another writer beat ([errWriteLost]): a LOST
// RACE ([lostUpdateRace]), so the flush retries after a window up to
// [fleetCASAttempts], then answers unavailable naming the contention — never
// [coord.ErrWriteUnknown], since every write was answered and refused
// ([coord.ChatReads] contract).
//
// THE WINDOWS ARE THE CASE'S ([roundTrip.after]), over at once and counted:
// that it waits is held here, how long by casline's own suite (casWindow).
func TestAFlushThatLosesEveryAttemptSaysNothingLanded(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f, _ := chatReadFleet(t, "founder")
	refusing := &lostForever{KeyValue: f.chatReads}
	f.chatReads = refusing
	var waits atomic.Int64
	f.chatReadTrips.after = func(time.Duration) <-chan time.Time {
		waits.Add(1)
		over := make(chan time.Time, 1)
		over <- chatReadAt0
		return over
	}

	_, err := f.AdvanceChatRead(ctx, "founder", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-c"): 11}, At: chatReadAt0,
	})
	if !errors.Is(err, coord.ErrUnavailable) || !strings.Contains(err.Error(), "lost every compare-and-swap") {
		t.Fatalf("AdvanceChatRead = %v, want an unavailable store naming the contention", err)
	}
	if errors.Is(err, coord.ErrWriteUnknown) {
		t.Fatalf("AdvanceChatRead = %v: a flush whose every write was refused is marked as "+
			"one that may have landed", err)
	}
	if n := refusing.refused.Load(); n != fleetCASAttempts {
		t.Fatalf("the flush was refused %d time(s), want every one of its %d attempts: "+
			"a write another writer beat is a lost race, not an outage", n, fleetCASAttempts)
	}
	// A window before every attempt once the round trip has its two samples
	// (roundTrip): the fixture's writes may have seeded them, or the flush's
	// first two attempts do — so every attempt but the first, or the first two.
	if n := waits.Load(); n < fleetCASAttempts-2 || n > fleetCASAttempts-1 {
		t.Fatalf("the flush waited %d time(s) between its %d attempts, want a window before each "+
			"after the first, or the first two while the round trip seeds: %d or %d", n,
			fleetCASAttempts, fleetCASAttempts-1, fleetCASAttempts-2)
	}
	f.chatReads = refusing.KeyValue
	state, err := f.ChatRead(ctx, "founder")
	if err != nil || len(state.Cursors) != 2 || state.Cursors[coordtest.ChatID("room-c")] != 0 {
		t.Fatalf("ChatRead = (%v, %v), want the two cursors it held and no room-c: "+
			"nothing landed", state.Cursors, err)
	}
}

// lostForever is a bucket on which every update is refused as one another
// writer beat, writing nothing.
type lostForever struct {
	jetstream.KeyValue
	refused atomic.Int64
}

func (k *lostForever) Update(context.Context, string, []byte, uint64) (uint64, error) {
	k.refused.Add(1)
	return 0, errWriteLost
}

// A FLUSH THE LEADER NEVER DECIDED MAY HAVE LANDED.
//
// The other side of the case above. A leader answers a write to a record with
// one already in flight with neither a landing nor a refusal, and the settled
// bucket waits that out for the write's budget (settle.go). A write still
// undecided was sent and is unanswered, so it is [coord.ErrWriteUnknown] —
// not a lost race, which would retry over a record it may have moved, nor a
// refusal, which would say it did not land. Every answer here is the in-flight
// one under a short budget, so the write is undecided by construction.
func TestAFlushTheLeaderNeverDecidedMayHaveLanded(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f, _ := chatReadFleet(t, "founder")
	underneath, ok := f.chatReads.(settledBucket)
	if !ok {
		t.Fatalf("the chat read bucket is a %T, not settled", f.chatReads)
	}
	pending := &inFlightForever{}
	f.chatReads = settledBucket{KeyValue: underneath.KeyValue,
		js: termed{term: 50 * time.Millisecond, publish: pending.publish}, holding: chatReadsHolding}

	_, err := f.AdvanceChatRead(ctx, "founder", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-c"): 11}, At: chatReadAt0,
	})
	if !errors.Is(err, coord.ErrWriteUnknown) || !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("a flush the leader never decided = %v, want ErrWriteUnknown and "+
			"ErrUnavailable", err)
	}
	if lostUpdateRace(err) {
		t.Fatalf("a flush the leader never decided = %v, which reads as a race "+
			"another writer won", err)
	}
	if n := pending.answered.Load(); n < 2 {
		t.Fatalf("the write was answered %d time(s), want the in-flight answer "+
			"waited out rather than taken", n)
	}
}

// inFlightForever is a client whose leader answers every conditional write
// with its in-flight answer, writing nothing.
type inFlightForever struct {
	answered atomic.Int64
}

func (k *inFlightForever) publish(context.Context, *nats.Msg, ...jetstream.PublishOpt) (*jetstream.PubAck, error) {
	k.answered.Add(1)
	return nil, leaderRefusal(jetstream.JSErrCodeStreamWrongLastSequenceConstant)
}

// A READ-STATE WATCH'S FIRST ANSWER IS CERTIFIED, AND AN OLDER REVISION AFTER
// IT IS NOT A CHANGE.
//
// A node's hub re-syncs every tab from the answer after watching again, so a
// record missing from it leaves a tab stale. The client ends its initial values
// on a count guess; here that ends the pass before either record, and the answer
// must still hold both as the leader does — founder with its newer cursor.
//
// The consumer then delivers founder as it stood BEFORE that flush, as a pass
// behind the leader would: not a change, and sent as one it would push an older
// state onto that seat's tabs. A flush after the answer is the next update.
func TestAChatReadWatchsFirstAnswerIsCertifiedAndNotUndoneByAnOlderRevision(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 4*passIdle)
	defer cancel()
	f, _ := chatReadFleet(t, "founder")
	if _, err := f.AdvanceChatRead(ctx, "designer", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-a"): 1}, At: chatReadAt0,
	}); err != nil {
		t.Fatalf("AdvanceChatRead: %v", err)
	}
	var stale jetstream.KeyValueEntry
	for _, kve := range recordPass(ctx, t, f.chatReads) {
		if kve.Key() == coord.ChatReadKey("founder") {
			stale = kve
		}
	}
	if stale == nil {
		t.Fatal("setup: the recorded pass holds no record for founder")
	}
	newer, err := f.AdvanceChatRead(ctx, "founder", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-a"): 8}, At: chatReadAt0,
	})
	if err != nil {
		t.Fatalf("AdvanceChatRead: %v", err)
	}

	script := make(chan jetstream.KeyValueEntry, 3)
	script <- nil // the end of the initial values, before either record
	f.chatReads = scriptedKV{KeyValue: f.chatReads, script: script}
	updates, err := f.WatchChatReads(ctx)
	if err != nil {
		t.Fatalf("WatchChatReads: %v", err)
	}
	answer := map[string]coord.ChatReadState{}
	for {
		u := nextChatReadUpdate(ctx, t, updates)
		if u.Current {
			break
		}
		if u.State == nil {
			t.Fatalf("the first answer held %s with no record", u.Handle)
		}
		answer[u.Handle] = *u.State
	}
	if len(answer) != 2 {
		t.Fatalf("the first answer held %v, want founder and designer: the client's "+
			"marker came before either record, and an answer that trusted it holds "+
			"neither", answer)
	}
	if got := answer["founder"]; got.Cursors[coordtest.ChatID("room-a")] != 8 || got.Revision != newer.Revision {
		t.Errorf("founder answered as %+v, want the newer flush at revision %d", got, newer.Revision)
	}

	flushed, err := f.AdvanceChatRead(ctx, "ops", coord.ChatReadDelta{
		Cursors: map[string]int64{coordtest.ChatID("room-b"): 2}, At: chatReadAt0,
	})
	if err != nil {
		t.Fatalf("AdvanceChatRead: %v", err)
	}
	fresh, err := getLatest(ctx, f.js, f.chatReads, coord.ChatReadKey("ops"))
	if err != nil {
		t.Fatalf("read ops's record: %v", err)
	}
	script <- stale
	script <- fresh
	u := nextChatReadUpdate(ctx, t, updates)
	if u.Handle != "ops" || u.State == nil || u.State.Revision != flushed.Revision {
		t.Fatalf("the update after an older revision of founder and ops's new record "+
			"= %+v, want ops's record: founder's older revision is not a change, and "+
			"sent as one it pushes a state founder's tabs have moved past", u)
	}
}

// nextChatReadUpdate reads one update, failing the case when the watch closes
// or ctx ends first.
func nextChatReadUpdate(ctx context.Context, t *testing.T, updates <-chan coord.ChatReadUpdate) coord.ChatReadUpdate {
	t.Helper()
	select {
	case u, ok := <-updates:
		if !ok {
			t.Fatal("the read-state watch closed while the store was healthy")
		}
		return u
	case <-ctx.Done():
		t.Fatalf("the read-state watch delivered nothing before its caller gave up: %v", ctx.Err())
	}
	return coord.ChatReadUpdate{}
}
