package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- a log, its feeds and their dead letters, over a real broker ------ //

const (
	deferStream = "CREWLET_DEFERFEED_LOG"
	deferPrefix = "crewlet.deferfeed"
	deferDomain = "deferfeed"
	deferGroup  = "crewlet-deferfeed-feed"
)

// feedWithin bounds how long a case waits for what must happen: only a broken
// build reaches it.
const feedWithin = 15 * time.Second

// await is the next value on ch, failing the case once feedWithin has passed.
func await[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(feedWithin): //fixedwait:liveness
		t.Fatalf("%s never came", what)
	}
	var zero T
	return zero
}

// deferLog is one domain log on a broker of a case's own — the queue beside it
// holding the dead-letter stream every feed of the case keeps its entries in —
// and the claims its feeds share, as a fleet's nodes do.
type deferLog struct {
	q      *jetstream.Queue
	log    *jetstream.DomainLog
	claims *coordmem.Fleet
}

func newDeferLog(t *testing.T) deferLog {
	t.Helper()
	q, err := jetstream.Open(t.Context(), jetstream.Config{
		StoreDir: t.TempDir(), StoreMaxBytes: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	if err := q.EnsureDomainStream(t.Context(), jetstream.DomainStream{
		Name: deferStream, Subjects: []string{deferPrefix + ".>"},
		MaxBytes: 16 << 20, Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("create the log: %v", err)
	}
	l, err := q.DomainLog(t.Context(), deferStream)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	return deferLog{q: q, log: l, claims: coordmem.NewFleet()}
}

// sealed is an envelope of version v for op, sealed under ring.
func sealed(t *testing.T, ring recordseal.Keyring, v int, op string) []byte {
	t.Helper()
	body, err := json.Marshal(statelog.Envelope{V: v, Kind: "task.create", OpID: op, Gen: 1})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := recordseal.NewSigner(deferDomain, ring)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return signer.Seal(body)
}

// commit appends frame to the log, answering its sequence.
func (l deferLog) commit(t *testing.T, name string, frame []byte) uint64 {
	t.Helper()
	seq, _, err := l.log.Append(t.Context(), deferPrefix+"."+name, "", nil, frame)
	if err != nil {
		t.Fatalf("commit %s: %v", name, err)
	}
	return seq
}

// lettersHeld is what the feed's dead-letter subject holds.
func (l deferLog) lettersHeld(t *testing.T) []queue.Kept {
	t.Helper()
	kept, err := l.q.DeadLettersOn(t.Context(), changefeed.DeadLetterSubject(opWaker{}.Source()))
	if err != nil {
		t.Fatalf("list the dead letters: %v", err)
	}
	return kept
}

// readsUpTo is a build's envelope reader: it refuses an envelope of a version
// past newest, as a build that does not know it does.
func readsUpTo(newest int) func([]byte) (statelog.Envelope, error) {
	return func(body []byte) (statelog.Envelope, error) {
		var env statelog.Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			return statelog.Envelope{}, err
		}
		if env.V > newest {
			return statelog.Envelope{}, fmt.Errorf("envelope version %d, this build reads up to %d", env.V, newest)
		}
		return env, nil
	}
}

// opWaker wakes on every record, naming its op id: a translator over the
// envelope alone, so these cases are about the adapter and the feed.
type opWaker struct{}

func (opWaker) Source() changefeed.Source {
	return changefeed.Source{Name: "work", Group: deferGroup}
}

func (opWaker) Translate(_ context.Context, rec changefeed.Record) (changefeed.Delivery, bool, error) {
	return changefeed.Delivery{Body: map[string]any{"op": rec.ID}, ID: rec.ID, Actor: "ana"}, true, nil
}

// wakeSink hands every published wake's op id to the case.
type wakeSink chan string

func (w wakeSink) Publish(_ context.Context, _ string, ev *events.Event) error {
	raw, _ := events.DataAs[*types.RawWebhook](ev)
	op, _ := raw.Body["op"].(string)
	w <- op
	return nil
}

// settlement is how a feed settled one message: acked, or handed back after
// delay, and the deferral the opener met on it, if any.
type settlement struct {
	seq      uint64
	ack      bool
	delay    time.Duration
	deferred changefeed.DeferReason
}

// watchedOpener is the engine's adapter, with every message's settlement told
// to the case and the latest open's records kept.
type watchedOpener struct {
	inner   changefeed.Opener
	settled chan settlement

	mu   sync.Mutex
	last changefeed.Records
}

func (o *watchedOpener) Open(ctx context.Context, group string) (changefeed.Records, error) {
	r, err := o.inner.Open(ctx, group)
	if err != nil {
		return nil, err
	}
	o.mu.Lock()
	o.last = r
	o.mu.Unlock()
	return watchedRecords{Records: r, settled: o.settled}, nil
}

// consumer is the durable name the latest open read through.
func (o *watchedOpener) consumer() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.last.(*domainRecords).group.Name()
}

type watchedRecords struct {
	changefeed.Records
	settled chan settlement
}

func (r watchedRecords) Next(ctx context.Context) (*changefeed.Message, error) {
	msg, err := r.Records.Next(ctx)
	if err != nil || msg == nil {
		return msg, err
	}
	s := settlement{seq: msg.Position}
	if msg.Deferred != nil {
		s.deferred = msg.Deferred.Reason
	}
	ack, nak := msg.Ack, msg.Nak
	msg.Ack = func() error {
		err := ack()
		acked := s
		acked.ack = true
		r.settled <- acked
		return err
	}
	msg.Nak = func(d time.Duration) error {
		err := nak(d)
		naked := s
		naked.delay = d
		r.settled <- naked
		return err
	}
	return msg, nil
}

// droppedStore is the queue's dead-letter verbs, telling the case every drop.
type droppedStore struct {
	changefeed.DeadLetters
	dropped chan uint64
}

func (s droppedStore) DropDeadLetter(ctx context.Context, subject string, seq uint64) error {
	err := s.DeadLetters.DropDeadLetter(ctx, subject, seq)
	if err == nil {
		s.dropped <- seq
	}
	return err
}

// feedNode is one node's feed over the log, as the engine builds it.
type feedNode struct {
	opener  *watchedOpener
	wakes   wakeSink
	dropped chan uint64
	stop    func()
}

// node starts a feed reading the log through ring and envel, its clock now
// (nil for the wall clock).
func (l deferLog) node(t *testing.T, ring recordseal.Keyring, envel func([]byte) (statelog.Envelope, error),
	now func() time.Time) feedNode {

	t.Helper()
	verifier, err := recordseal.NewVerifier(deferDomain, ring)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	n := feedNode{
		opener: &watchedOpener{inner: worklet.FeedSource{Log: domainFeed{
			log: l.log, stream: deferStream, envel: envel, verify: verifier,
		}}, settled: make(chan settlement, 64)},
		wakes: make(wakeSink, 64), dropped: make(chan uint64, 64),
	}
	feed, err := changefeed.New(changefeed.Options{
		Opener: n.opener, Publisher: n.wakes, Claims: l.claims, Translator: opWaker{},
		DeadLetters: droppedStore{DeadLetters: l.q, dropped: n.dropped}, Now: now,
	})
	if err != nil {
		t.Fatalf("new feed: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		feed.Run(ctx)
	}()
	n.stop = sync.OnceFunc(func() {
		cancel()
		await(t, done, "the feed's stop")
	})
	t.Cleanup(n.stop)
	return n
}

var (
	keyOne = recordseal.OneKey("k1", "material-one")
	keyTwo = recordseal.OneKey("k2", "material-two")
)

// ---- the deferral, over the broker ------------------------------------ //

// A FRAME SEALED UNDER A KEY THIS NODE DOES NOT HOLD IS DEFERRED, NEVER ACKED,
// AND A NODE HOLDING THE KEY DELIVERS ITS WAKE (#838, case 1).
//
// The key rotation runbook adds a key on every node before anything is sealed
// under it, so a node that has not restarted onto it yet meets a record a peer
// can read. Every node pulls the feed from one durable consumer, so the node
// without the key hands the record back — at the first delivery's backoff —
// and the node holding it takes the redelivery and wakes the record's seat.
//
// Mutation: ack a deferral (the adapter's or the feed's), and the node holding
// the key is never handed the record.
func TestAFrameUnderAKeyThisNodeLacksIsDeferredToANodeThatHoldsIt(t *testing.T) {
	t.Parallel()
	l := newDeferLog(t)
	seq := l.commit(t, "task.t1", sealed(t, keyTwo, 1, "op-under-k2"))

	without := l.node(t, keyOne, readsUpTo(1), nil)
	got := await(t, without.opener.settled, "the settlement on the node without the key")
	if got.ack || got.seq != seq || got.deferred != changefeed.DeferUnknownKey ||
		got.delay != changefeed.DeferDelay(1) {
		t.Fatalf("the node without the key settled the record as %+v, want it handed back as %s "+
			"after %v", got, changefeed.DeferUnknownKey, changefeed.DeferDelay(1))
	}
	without.stop()
	select {
	case op := <-without.wakes:
		t.Fatalf("the node without the key published a wake for %s", op)
	default:
	}

	holder := l.node(t, keyTwo, readsUpTo(1), nil)
	if op := await(t, holder.wakes, "the wake on the node holding the key"); op != "op-under-k2" {
		t.Fatalf("the node holding the key woke for %q, want op-under-k2", op)
	}
	if s := await(t, holder.opener.settled, "the holder's settlement"); !s.ack || s.seq != seq {
		t.Errorf("the holder settled %+v, want record %d acked once its wake was published", s, seq)
	}
}

// AN ENVELOPE A NEWER BUILD WRITES IS DEFERRED, NEVER ACKED, AND A NODE ON THAT
// BUILD DELIVERS ITS WAKE (#838, case 3).
//
// During a roll the older nodes meet a record they cannot read past its frame;
// the bytes will not improve on them, and will on a peer on the newer build,
// which takes the redelivery.
//
// Mutation: ack an undecodable envelope, as the adapter did, and the newer
// node never sees the record.
func TestAnEnvelopeANewerBuildWritesIsDeferredToANodeOnThatBuild(t *testing.T) {
	t.Parallel()
	l := newDeferLog(t)
	seq := l.commit(t, "task.t1", sealed(t, keyOne, 2, "op-v2"))

	older := l.node(t, keyOne, readsUpTo(1), nil)
	got := await(t, older.opener.settled, "the settlement on the older build")
	if got.ack || got.seq != seq || got.deferred != changefeed.DeferNewerEnvelope {
		t.Fatalf("the older build settled the record as %+v, want it handed back as %s",
			got, changefeed.DeferNewerEnvelope)
	}
	older.stop()

	newer := l.node(t, keyOne, readsUpTo(2), nil)
	if op := await(t, newer.wakes, "the wake on the newer build"); op != "op-v2" {
		t.Fatalf("the newer build woke for %q, want op-v2", op)
	}
}

// A TAMPERED FRAME IS STILL ACKNOWLEDGED, AND WAKES NOBODY (#838, case 2).
//
// A MAC that fails under a key this node holds is not this fleet writing to its
// log: no node should act on it, so it is not handed back for one — the adapter
// acknowledges it and goes on to the next record, and the applier is what stops
// the fleet on it.
//
// Mutation: defer a tampered frame, and the feed is handed it and holds it
// pending; never ack it, and the consumer holds it pending.
func TestATamperedFrameIsStillAcknowledgedAndWakesNobody(t *testing.T) {
	t.Parallel()
	l := newDeferLog(t)
	forged := sealed(t, keyOne, 1, "op-forged")
	forged[len(forged)-2] ^= 0xff
	l.commit(t, "task.forged", forged)
	good := l.commit(t, "task.good", sealed(t, keyOne, 1, "op-good"))

	n := l.node(t, keyOne, readsUpTo(1), nil)
	if op := await(t, n.wakes, "the good record's wake"); op != "op-good" {
		t.Fatalf("the feed woke for %q first, want the good record's op-good", op)
	}
	if s := await(t, n.opener.settled, "the good record's settlement"); !s.ack || s.seq != good {
		t.Fatalf("the first message the feed was handed settled as %+v, want the good record %d "+
			"acked: the tampered one reaches the feed for nobody", s, good)
	}
	cons, err := l.q.JetStream().Consumer(t.Context(), deferStream, n.opener.consumer())
	if err != nil {
		t.Fatalf("read the feed's consumer: %v", err)
	}
	pending := make(chan struct{})
	go func() {
		defer close(pending)
		for t.Context().Err() == nil {
			info, err := cons.Info(t.Context())
			if err == nil && info.NumAckPending == 0 && info.AckFloor.Stream >= good {
				return
			}
			select {
			case <-t.Context().Done():
			case <-time.After(5 * time.Millisecond): //fixedwait:liveness
			}
		}
	}()
	await(t, pending, "the tampered record's acknowledgement")
}

// ---- the dead letter and its replay at start -------------------------- //

// A NODE THAT STARTS ABLE TO READ A DEAD LETTER DERIVES ITS WAKE AND DROPS IT;
// A NODE THAT STILL CANNOT LEAVES IT.
//
// A node whose clock says the record is two hours old meets it under a key it
// does not hold, and dead-letters it — frame and all — rather than hand it
// back again. A second node without the key starts: its replay reopens the
// frame, still cannot read it, and leaves it (proved finished by a record
// committed after the start, whose wake comes only after the replay). A node
// holding the key starts: its replay reopens the frame through the same
// verifier a delivery passes, publishes the wake once, and drops the entry.
// The keyring is read at boot and never changes, so this start is how a key's
// arrival reaches a dead letter.
//
// Mutation: drop a dead letter whatever the replay made of it, and the second
// node loses the wake; leave one whose wake was published, and it is still
// held at the end.
func TestANodeThatStartsAbleToReadADeadLetterDerivesItsWakeAndDropsIt(t *testing.T) {
	t.Parallel()
	l := newDeferLog(t)
	seq := l.commit(t, "task.t1", sealed(t, keyTwo, 1, "op-dead"))

	late := l.node(t, keyOne, readsUpTo(1), func() time.Time { return time.Now().UTC().Add(2 * time.Hour) })
	if s := await(t, late.opener.settled, "the dead-lettering"); !s.ack || s.seq != seq {
		t.Fatalf("the node that saw the record two hours old settled %+v, want it kept and acked", s)
	}
	late.stop()
	if held := l.lettersHeld(t); len(held) != 1 {
		t.Fatalf("the dead-letter subject holds %d entries, want the record's", len(held))
	}

	still := l.node(t, keyOne, readsUpTo(1), nil)
	l.commit(t, "task.after", sealed(t, keyOne, 1, "op-after"))
	if op := await(t, still.wakes, "the wake committed after the start"); op != "op-after" {
		t.Fatalf("the node without the key woke for %q, want op-after alone", op)
	}
	still.stop()
	if held := l.lettersHeld(t); len(held) != 1 {
		t.Fatalf("a node that cannot read the dead letter left %d entries, want it kept", len(held))
	}

	holder := l.node(t, keyTwo, readsUpTo(1), nil)
	if op := await(t, holder.wakes, "the replayed wake"); op != "op-dead" {
		t.Fatalf("the replay woke for %q, want op-dead", op)
	}
	await(t, holder.dropped, "the replayed entry's drop")
	if held := l.lettersHeld(t); len(held) != 0 {
		t.Errorf("the replayed entry is still held: %d", len(held))
	}
	select {
	case op := <-holder.wakes:
		t.Errorf("the replay published a second wake, for %q", op)
	default:
	}
}

// ---- the alarm's reading ---------------------------------------------- //

// letterBox is a dead-letter store a case writes and breaks.
type letterBox struct {
	mu   sync.Mutex
	kept []queue.Kept
	fail error
}

func (b *letterBox) KeepDeadLetter(context.Context, string, []byte) error {
	return errors.New("no case here keeps through the box")
}

func (b *letterBox) DeadLettersOn(context.Context, string) ([]queue.Kept, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.fail != nil {
		return nil, b.fail
	}
	return append([]queue.Kept(nil), b.kept...), nil
}

func (b *letterBox) DropDeadLetter(_ context.Context, _ string, seq uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, k := range b.kept {
		if k.Seq == seq {
			b.kept = append(b.kept[:i], b.kept[i+1:]...)
			break
		}
	}
	return nil
}

// set replaces what the box holds, and whether its listing fails.
func (b *letterBox) set(fail error, kept ...queue.Kept) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.kept, b.fail = kept, fail
}

// deadLetterAlarm is the feed_dead_lettered alarm of a report whose one log
// carries letters, and whether it was raised.
func deadLetterAlarm(at time.Time, letters []statelog.FeedDeadLetter) (statelog.Alarm, bool) {
	rep := statelog.NewReport(statelog.ReportInputs{NodeID: "node-1", At: at, RegisterReadable: true,
		Domains: []statelog.DomainInputs{{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG",
			FeedDeadLetters: letters}}})
	for _, a := range rep.Alarms {
		if a.Kind == statelog.KindFeedDeadLettered {
			return a, true
		}
	}
	return statelog.Alarm{}, false
}

// THE ALARM READS EVERY FEED'S DEAD LETTERS ON EVERY BEAT: raised from the
// first entry, each entry aged to now — so its count of what expires within a
// day is the reading's, not the last replay's — a failed listing read as the
// last one that answered, and cleared once the entry is gone.
//
// Mutation: read the listing once and keep it, and the hours never move; read
// a failed listing as an empty one, and a broker blip clears the alarm.
func TestTheAlarmReadsAFeedsDeadLettersOnEveryBeat(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	box := &letterBox{}
	box.set(nil,
		queue.Kept{Seq: 4, StoredAt: now.Add(-29*day - 12*time.Hour), Expires: now.Add(12 * time.Hour)},
		queue.Kept{Seq: 9, StoredAt: now.Add(-2 * time.Hour), Expires: now.Add(30*day - 2*time.Hour)})
	w := newDeadLetterWatch(newDeadLetterLedger(box, nil), nil, nil)
	src := opWaker{}.Source()

	letters := w.read(t.Context(), src, now)
	want := []statelog.FeedDeadLetter{{Age: 29*day + 12*time.Hour, ExpiresIn: 12 * time.Hour},
		{Age: 2 * time.Hour, ExpiresIn: 30*day - 2*time.Hour}}
	if fmt.Sprint(letters) != fmt.Sprint(want) {
		t.Fatalf("the reading carries %v, want %v", letters, want)
	}
	if a, raised := deadLetterAlarm(now, letters); !raised {
		t.Fatal("two dead letters raised no feed_dead_lettered")
	} else if wantDetail := "1 of them expiring within a day"; !strings.Contains(a.Detail, wantDetail) {
		t.Errorf("the alarm says %q, want %q", a.Detail, wantDetail)
	}

	// THIRTEEN HOURS ON THE BROKER DOES NOT ANSWER: the last listing that
	// answered, aged to now — and the entry that had twelve hours left is
	// past its expiry by one.
	box.set(errors.New("nats: no responders available for request"))
	later := now.Add(13 * time.Hour)
	letters = w.read(t.Context(), src, later)
	if len(letters) != 2 || letters[0].ExpiresIn != -time.Hour || letters[1].Age != 15*time.Hour {
		t.Fatalf("a failed listing carries %v, want the last that answered aged thirteen hours", letters)
	}

	// AND ONCE THE ENTRIES ARE GONE the alarm clears.
	box.set(nil)
	if letters = w.read(t.Context(), src, later.Add(time.Minute)); len(letters) != 0 {
		t.Fatalf("an empty listing carries %v", letters)
	}
	if a, raised := deadLetterAlarm(later, letters); raised {
		t.Errorf("a feed holding no dead letter still raises %q", a.Detail)
	}
}

// AN ENTRY THAT VANISHES AT ITS EXPIRY IS COUNTED ONCE AS A LOST WAKE; ONE A
// REPLAY DROPPED IS NOT.
//
// The broker lets an entry go at its retention without telling anybody, and
// its wake goes with it, so an entry the reading saw within a beat of its
// expiry and then no more is counted — once across the fleet, through the claim
// on its going, since every data node reads the subject on every beat. An entry
// a replay dropped, though as near its expiry, published its wake: the drop
// took that claim first, so neither the node that dropped it nor a peer that
// only saw it vanish counts it. One gone long before its expiry was replayed
// by somebody.
//
// Mutation: count every entry that vanished, and the replayed entries are
// counted; let a drop take no claim, and the peer counts the replayed one;
// count without the claim, and the peer counts the lost one again.
func TestAnEntryThatVanishesAtItsExpiryIsCountedOnceAsALostWake(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	stored := now.Add(-30 * 24 * time.Hour)
	expiring := queue.Kept{Seq: 1, StoredAt: stored, Expires: now.Add(5 * time.Second)}
	replayedHere := queue.Kept{Seq: 2, StoredAt: stored, Expires: now.Add(5 * time.Second)}
	replayedEarly := queue.Kept{Seq: 3, StoredAt: now.Add(-time.Hour), Expires: now.Add(30 * 24 * time.Hour)}
	box := &letterBox{}
	box.set(nil, expiring, replayedHere, replayedEarly)

	claims := coordmem.NewFleet()
	counted := func(rec *metrics.Recorder) uint64 {
		for _, s := range rec.Read() {
			if s.Name == metrics.TrackerFeedDeadLetterExpired && s.Attrs["source"] == "work" {
				return s.Total
			}
		}
		return 0
	}
	recHere, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	recPeer, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	here := newDeadLetterLedger(box, claims)
	here.now = func() time.Time { return now }
	watchHere := newDeadLetterWatch(here, claims, recHere)
	watchPeer := newDeadLetterWatch(newDeadLetterLedger(box, claims), claims, recPeer)
	src := opWaker{}.Source()
	watchHere.read(t.Context(), src, now)
	watchPeer.read(t.Context(), src, now)

	// THIS NODE'S REPLAY DROPS ONE, A PEER'S ANOTHER, AND THE RETENTION
	// TAKES THE THIRD.
	subject := changefeed.DeadLetterSubject(src)
	if err := here.DropDeadLetter(t.Context(), subject, replayedHere.Seq); err != nil {
		t.Fatal(err)
	}
	box.set(nil)
	beat := now.Add(statelog.AlarmInterval)
	watchHere.read(t.Context(), src, beat)
	watchPeer.read(t.Context(), src, beat)

	if got := counted(recHere) + counted(recPeer); got != 1 {
		t.Errorf("the fleet counted %d lost wake(s), want the one entry the retention took, once "+
			"(this node %d, its peer %d)", got, counted(recHere), counted(recPeer))
	}
}

// AN ENTRY THAT EXPIRES WHILE THIS NODE'S LISTINGS FAIL IS STILL COUNTED; ONE
// THAT WENT BEFORE ITS EXPIRY IS NOT.
//
// The reading judges an entry's going at its next listing that answers, which
// after a broker outage can be hours after the last one that did. An entry
// whose expiry passed in between was let go by the retention, however long ago
// it was last seen, and an entry that is not due yet went to a replay.
//
// Mutation: judge an entry only by how near its expiry it was when last seen,
// and the expiry during the outage is never counted; count every entry that
// vanished, and the one replayed early is counted too.
func TestAnEntryThatExpiresWhileTheListingsFailIsStillCounted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	month := 30 * 24 * time.Hour
	expiring := queue.Kept{Seq: 1, StoredAt: now.Add(2*time.Hour - month), Expires: now.Add(2 * time.Hour)}
	replayed := queue.Kept{Seq: 2, StoredAt: now.Add(-time.Hour), Expires: now.Add(month - time.Hour)}
	box := &letterBox{}
	box.set(nil, expiring, replayed)
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	w := newDeadLetterWatch(newDeadLetterLedger(box, nil), nil, rec)
	src := opWaker{}.Source()
	w.read(t.Context(), src, now)

	// THREE HOURS THE BROKER DOES NOT ANSWER, while the retention takes one
	// entry and a peer's replay the other.
	box.set(errors.New("nats: no responders available for request"))
	w.read(t.Context(), src, now.Add(time.Hour))
	box.set(nil)
	w.read(t.Context(), src, now.Add(3*time.Hour))

	var got uint64
	for _, s := range rec.Read() {
		if s.Name == metrics.TrackerFeedDeadLetterExpired && s.Attrs["source"] == src.Name {
			got += s.Total
		}
	}
	if got != 1 {
		t.Errorf("counted %d lost wake(s) across the outage, want the one entry whose expiry "+
			"passed while the listings failed", got)
	}
}
